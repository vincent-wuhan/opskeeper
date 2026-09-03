// Package injector 实现 Harness 故障注入器。
//
// 路径 A 阶段 2 任务 2.6 — Harness fault-injector。
//
// 设计要点：
//   - 1 个 Injector 接口 + 6 个分类实现（pg / redis / mq-rabbitmq / mq-kafka / k8s / host）
//   - 注入类型按 <resource-prefix>.<action> 命名（如 pg.inject_lock_chain）
//   - Registry 按 prefix 索引，调用方按 type 自动路由
//   - Cleanup 通过 InjectID 标识（idempotent + context-aware）
//   - IsAvailable 用于 E2E 前置检查（沙箱/权限/二进制依赖）
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.2
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/harness-eval-platform/spec.md
package injector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/harness/schema"
)

// 公共错误。
var (
	ErrUnsupportedType   = errors.New("injector: unsupported inject type")
	ErrUnavailable       = errors.New("injector: not available in current env")
	ErrInjectionNotFound = errors.New("injector: injection not found (already cleaned up?)")
	ErrInjectionFailed   = errors.New("injector: injection failed")
)

// InjectSpec 是单次故障注入的输入。
//
// 直接复用 schema.InjectStep 的字段；新增 InjectID 用于幂等控制（可选）。
type InjectSpec struct {
	InjectID string                 // 幂等 ID（空则生成）
	Type     string                 // 注入类型，如 "pg.inject_lock_chain"
	Duration time.Duration          // 持续时长
	Params   map[string]interface{} // 类型特定参数
	TenantID uint64                 // 多租户隔离
	Env      string                 // staging / test / prod（prod 必须 confirm）
}

// FromSchemaStep 从 schema.InjectStep 构造 InjectSpec。
func FromSchemaStep(s schema.InjectStep, tenantID uint64, env string) (InjectSpec, error) {
	d, err := time.ParseDuration(s.Duration)
	if err != nil {
		return InjectSpec{}, fmt.Errorf("invalid duration %q: %w", s.Duration, err)
	}
	return InjectSpec{
		Type:     s.Type,
		Duration: d,
		Params:   s.Params,
		TenantID: tenantID,
		Env:      env,
	}, nil
}

// InjectResult 是单次故障注入的输出。
type InjectResult struct {
	InjectID   string            // 唯一 ID（用于 Cleanup）
	Type       string            // 注入类型
	ResourceID string            // 影响的资源（resource / table / key / pod ...）
	StartedAt  time.Time         // 注入开始时间
	Metadata   map[string]string // 注入器特定元数据（PID、容器 ID 等）
}

// Injector 是单类资源故障注入器的接口。
//
// 6 类实现：pg / redis / rabbitmq / kafka / k8s / host。
// 每个实现负责 1 个 type prefix（如 "pg." / "redis." / "k8s." / "host." / "rabbitmq." / "kafka."）。
type Injector interface {
	// Type 返回此 injector 处理的 type 前缀（含 "."，如 "pg."）。
	//
	// 路由时按 "type[:prefix_len] == prefix" 匹配。
	Type() string

	// IsAvailable 检查 injector 在当前环境是否可用。
	//
	// 完整实现：检查必要二进制（psql / redis-cli / kubectl ...）、权限、网络可达。
	// 骨架实现：返回 true。
	IsAvailable(ctx context.Context) bool

	// Inject 执行故障注入。
	//
	// 返回的 InjectResult 必须包含非空 InjectID，用于后续 Cleanup。
	// 错误：ErrUnsupportedType / ErrUnavailable / ErrInjectionFailed。
	Inject(ctx context.Context, spec InjectSpec) (*InjectResult, error)

	// Cleanup 清理指定注入（按 InjectID 幂等）。
	//
	// 错误：ErrInjectionNotFound / ErrInjectionFailed。
	Cleanup(ctx context.Context, injectID string) error
}

// Registry 是 Injector 注册中心（按 type prefix 索引）。
type Registry struct {
	mu        sync.RWMutex
	injectors []Injector // 顺序：先注册先匹配（避免 prefix 冲突）
}

// NewRegistry 创建空 Registry。
func NewRegistry() *Registry {
	return &Registry{}
}

// Register 注册一个 injector。
//
// 同一 prefix 重复注册返回错误。
func (r *Registry) Register(i Injector) error {
	prefix := i.Type()
	if !strings.HasSuffix(prefix, ".") {
		return fmt.Errorf("injector Type() must end with '.', got %q", prefix)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.injectors {
		if existing.Type() == prefix {
			return fmt.Errorf("injector for prefix %q already registered", prefix)
		}
	}
	r.injectors = append(r.injectors, i)
	return nil
}

// Route 按 inject type 路由到对应 injector。
//
// 返回 (injector, action, ok)。action 是去掉 prefix 后的部分（如 "inject_lock_chain"）。
func (r *Registry) Route(t string) (Injector, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, i := range r.injectors {
		prefix := i.Type()
		if strings.HasPrefix(t, prefix) {
			return i, strings.TrimPrefix(t, prefix), true
		}
	}
	return nil, "", false
}

// Injectors 返回所有已注册的 injector（用于诊断 / E2E 前置检查）。
func (r *Registry) Injectors() []Injector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Injector, len(r.injectors))
	copy(out, r.injectors)
	return out
}

// CleanupAll 清理所有活跃注入（graceful shutdown / E2E 收尾）。
//
// 失败累积返回（不中断），便于调用方知道哪些清理失败。
func (r *Registry) CleanupAll(ctx context.Context) []error {
	r.mu.RLock()
	injectors := make([]Injector, len(r.injectors))
	copy(injectors, r.injectors)
	r.mu.RUnlock()

	var errs []error
	for _, i := range injectors {
		// 各 injector 需自行追踪活跃注入；骨架阶段只占位
		if err := i.Cleanup(ctx, ""); err != nil && !errors.Is(err, ErrInjectionNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", i.Type(), err))
		}
	}
	return errs
}

// Package redis 是 Redis 中间件 Adapter（路径 A 阶段 2 任务 2.2）。
//
// 当前为骨架实现：接口契约 + 15 个工具方法清单 + 错误处理。
// 完整实现在 Task 2.2 后续 PR（go-redis 客户端 + 15 工具方法真实实现 + 审批门控）。
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.1.4
package redis

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// Adapter 是 Redis Adapter 实现。
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	// client *redis.Client  // 完整实现时引入
	// cluster *redis.ClusterClient  // 完整实现时引入 cluster 模式
	// tenantID uint64
}

// New 创建 Redis Adapter 实例（不连接）。
func New() *Adapter {
	return &Adapter{}
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType {
	return adapter.TypeRedis
}

// Connect 建立连接（骨架：仅记录 spec，标记 connected）。
//
// 完整实现：
//  1. 解密 ConnectionSpec.DSN（pkg/credinject）
//  2. 检测 DSN 格式：单实例 vs cluster
//  3. redis.NewClient 或 redis.NewClusterClient 建立客户端
//  4. 验证 PING 可达
//  5. 设置连接池参数
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.PoolSize == 0 {
		conn.PoolSize = 10
	}
	if conn.Timeout == 0 {
		conn.Timeout = 30 * time.Second
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = conn
	a.connected = true
	return nil
}

// Close 关闭连接池。
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	return nil
}

// Health 健康检查。
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.HealthStatus{
		Status:    "healthy",
		LatencyMs: 0,
		Message:   "skeleton: real health probe pending Task 2.2",
		CheckedAt: time.Now(),
	}, nil
}

// Diagnose 通用诊断入口。
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    []map[string]interface{}{},
		Summary:     fmt.Sprintf("skeleton: category=%s not yet implemented", q.Category),
		Suggestions: []string{},
		ElapsedMs:   0,
	}, nil
}

// Collect 采集指标 / 配置。
func (a *Adapter) Collect(ctx context.Context, q adapter.CollectQuery) (*adapter.CollectResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.CollectResult{
		Metrics:  map[string]interface{}{},
		Samples:  []map[string]interface{}{},
		Metadata: map[string]string{"skeleton": "true"},
	}, nil
}

// Execute 受限执行（写操作需审批）。
func (a *Adapter) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	if op.ApprovedBy == "" {
		return nil, adapter.ErrApprovalRequired
	}
	return &adapter.ExecResult{
		Operation: op.Operation,
		Success:   false,
		Message:   "skeleton: real execution pending Task 2.2",
		Impacted:  0,
		Metadata:  map[string]string{"skeleton": "true"},
	}, nil
}

// RegisterTools 注册 Redis Adapter 暴露的工具方法到 Registry。
//
// 15 个工具方法（按风险等级分组）。
//
// 写操作风险提示：
//   - flushdb / flushall 是破坏性操作，配置为 L4（双人审批 + 强告警）
//   - config_set 是配置修改，配置为 L3（cmdpolicy + Casbin 双层）
func RegisterTools(reg *registry.Registry) error {
	tools := []registry.Tool{
		// L0 只读
		makeTool("redis.connect", adapter.RiskL0ReadOnly, "建立 Redis 连接", nil),
		makeTool("redis.info", adapter.RiskL0ReadOnly, "server info", nil),
		makeTool("redis.dbsize", adapter.RiskL0ReadOnly, "当前 db key 数量", nil),
		makeTool("redis.cluster_info", adapter.RiskL0ReadOnly, "cluster 拓扑信息", nil),
		makeTool("redis.config_get", adapter.RiskL0ReadOnly, "读取配置项", nil),
		// L1 诊断
		makeTool("redis.big_keys", adapter.RiskL1Diagnostic, "TOP N 大 key（按字节）", nil),
		makeTool("redis.slow_log", adapter.RiskL1Diagnostic, "Redis 慢日志", nil),
		makeTool("redis.hot_keys", adapter.RiskL1Diagnostic, "热 key（基于 monitor 采样）", nil),
		makeTool("redis.key_space", adapter.RiskL1Diagnostic, "各 db 的 key 分布", nil),
		makeTool("redis.memory_usage", adapter.RiskL1Diagnostic, "key 内存占用", nil),
		makeTool("redis.fragmentation_ratio", adapter.RiskL1Diagnostic, "内存碎片率", nil),
		makeTool("redis.client_list", adapter.RiskL1Diagnostic, "客户端连接列表", nil),
		makeTool("redis.blocked_clients", adapter.RiskL1Diagnostic, "阻塞客户端", nil),
		// L3 写操作
		makeTool("redis.config_set", adapter.RiskL3HardWrite, "修改配置（需审批）", nil),
		// L4 破坏性
		makeTool("redis.flushdb", adapter.RiskL4Destructive, "清空当前 db（双人审批 + 强告警）", nil),
	}
	return reg.RegisterTools(adapter.TypeRedis, tools)
}

func makeTool(name string, risk adapter.RiskLevel, desc string, _ map[string]string) registry.Tool {
	return registry.Tool{
		Name:        name,
		Description: desc,
		RiskLevel:   risk,
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			return nil, fmt.Errorf("not_implemented: tool %q pending Task 2.2", name)
		},
		ArgsSchema: map[string]string{},
	}
}

var _ adapter.Adapter = (*Adapter)(nil)

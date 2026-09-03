// Package registry 实现 Adapter 注册中心。
//
// 路径 A 阶段 2 任务 2.1 启动 — Adapter 注册机制。
//
// 设计要点：
//   - 注册以 ResourceType 为 key（一个进程内同一 Type 仅一个 Adapter 实现）
//   - 注册时机：cmd/opskeeper 启动时；通过 init() 或显式 Register 调用
//   - 工具方法命名空间：Adapter.Register(tools []Tool) 把工具方法挂到 BaseTool
//   - 多租户：每个 TenantID 独立的 Adapter 实例（按需懒创建）
package registry

import (
	"context"
	"fmt"
	"sync"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
)

// Tool 是 Adapter 暴露给 Agent 的工具方法描述。
type Tool struct {
	// Name 是工具方法全名（<type>.<method>，如 pg.long_running_txns）
	Name string

	// Description 是 LLM 可读的描述
	Description string

	// RiskLevel 是该工具的风险等级（cmdpolicy + Casbin 双重门控）
	RiskLevel adapter.RiskLevel

	// Handler 是实际执行函数（接收 ctx + args + tenant_id）
	Handler func(ctx context.Context, args map[string]interface{}) (interface{}, error)

	// ArgsSchema 是参数 schema（map[string]string 描述每个字段类型）
	ArgsSchema map[string]string
}

// Factory 是创建 Adapter 实例的工厂函数（按租户懒创建）。
type Factory func(ctx context.Context, conn adapter.ConnectionSpec) (adapter.Adapter, error)

// Registry 是 Adapter 注册中心（进程内单例）。
type Registry struct {
	mu        sync.RWMutex
	factories map[adapter.ResourceType]Factory
	tools     map[string]Tool // 全局工具索引（key = tool name）
}

// NewRegistry 创建 Registry。
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[adapter.ResourceType]Factory),
		tools:     make(map[string]Tool),
	}
}

// RegisterFactory 注册 Adapter 工厂（按 Type 唯一）。
func (r *Registry) RegisterFactory(t adapter.ResourceType, f Factory) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.factories[t]; ok {
		return fmt.Errorf("factory already registered for type %s", t)
	}
	r.factories[t] = f
	return nil
}

// ToolNamespace 返回 ResourceType 对应的工具命名空间前缀。
//
// 例如：postgres → "pg."，k8s_cluster → "k8s."。
func ToolNamespace(t adapter.ResourceType) string {
	switch t {
	case adapter.TypePostgres:
		return "pg."
	case adapter.TypeRedis:
		return "redis."
	case adapter.TypeRabbitMQ:
		return "rabbitmq."
	case adapter.TypeKafka:
		return "kafka."
	case adapter.TypeK8sCluster:
		return "k8s."
	case adapter.TypeGitRepository:
		return "git."
	}
	return string(t) + "."
}

// RegisterTools 注册 Adapter 暴露的工具方法。
//
// 工具名必须以对应 namespace 开头（避免命名空间冲突）。
func (r *Registry) RegisterTools(t adapter.ResourceType, tools []Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := ToolNamespace(t)
	for _, tool := range tools {
		if len(tool.Name) < len(prefix) || tool.Name[:len(prefix)] != prefix {
			return fmt.Errorf("tool %q must start with %q", tool.Name, prefix)
		}
		if _, exists := r.tools[tool.Name]; exists {
			return fmt.Errorf("tool %q already registered", tool.Name)
		}
		r.tools[tool.Name] = tool
	}
	return nil
}

// GetFactory 获取工厂。
func (r *Registry) GetFactory(t adapter.ResourceType) (Factory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.factories[t]
	return f, ok
}

// GetTool 获取工具方法。
func (r *Registry) GetTool(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// ReplaceTool 替换已注册的工具（用于装饰器链）。
func (r *Registry) ReplaceTool(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name]; !exists {
		return fmt.Errorf("tool %q not registered", t.Name)
	}
	r.tools[t.Name] = t
	return nil
}

// ListTools 列出所有工具（可按 namespace 过滤，如 "pg." / "redis."）。
func (r *Registry) ListTools(namespace string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for name := range r.tools {
		if namespace == "" || (len(name) > len(namespace) && name[:len(namespace)] == namespace) {
			names = append(names, name)
		}
	}
	return names
}

// 全局默认 Registry（cmd/opskeeper 启动时初始化）。
var (
	globalMu sync.RWMutex
	global   *Registry
)

// SetGlobal 设置全局 Registry。
func SetGlobal(r *Registry) {
	globalMu.Lock()
	defer globalMu.Unlock()
	global = r
}

// Global 获取全局 Registry。
func Global() *Registry {
	globalMu.RLock()
	defer globalMu.RUnlock()
	if global == nil {
		global = NewRegistry()
	}
	return global
}

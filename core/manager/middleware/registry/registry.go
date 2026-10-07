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
	"sort"
	"strings"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/ports"
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

	// ParamsSchema is a complete JSON Schema for this tool's arguments,
	// for the tools whose arguments are not a flat name → type map.
	//
	// ArgsSchema came first and is what the loop's dispatcher reads: it
	// needs to know which arguments must be present, not how to describe
	// them to a model. A tool taking a nested object cannot be expressed in
	// a flat map at all, and until this field existed the answer was to
	// describe it in prose and let the caller guess — which is how
	// git.find_runtime_link worked, with four different input shapes
	// behind one untyped `input` argument.
	//
	// When this is set it is the authority for the *agent's* copy of the
	// tool definition; ArgsSchema remains the authority for required
	// arguments. A tool that sets it should keep the two consistent, and
	// the toolset generator's tests assert that the schema is valid JSON.
	ParamsSchema string

	// ArgsSchema is the per-argument type map: name -> type.
	//
	// A trailing "!" marks the argument REQUIRED: "int!", "string!".
	// A required argument is one the tool cannot invent a value for — a
	// pid, a role name, a table name. Marking those separately from the
	// merely typed ones is what lets a caller that is dispatching a
	// decision rather than answering a question refuse to proceed when it
	// has no value, instead of passing an empty string and letting the
	// server decide what that means.
	//
	// The suffix is a convention rather than a struct because the field
	// has always been a flat type map and changing its shape would touch
	// every adapter. RequiredArgs parses it into a list.
	ArgsSchema map[string]string
}

// RequiredMarker is the ArgsSchema value suffix that marks an argument
// required.
const RequiredMarker = "!"

// RequiredArgs returns the names of the required arguments.
//
// Kept on Tool rather than at each call site so that "which arguments must
// I have" is answered from the tool's own declaration. A caller that
// duplicated this list per tool would drift from it, and the drift would
// show up as a dispatch that reaches the server with something missing.
func (t Tool) RequiredArgs() []string {
	out := make([]string, 0, len(t.ArgsSchema))
	for name, typ := range t.ArgsSchema {
		if strings.HasSuffix(typ, RequiredMarker) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// LookupTool returns the spec of a registered tool.
//
// The contract it returns is ports.ToolSpec rather than a type of this
// package's own, so the closed loop's remediation executor can depend on the
// shape without depending on this registry. The two are in different
// modules; a structural copy of the same struct would let them drift, and
// the drift would surface as a compile error at the moment somebody wired
// them together.
func (r *Registry) LookupTool(name string) (ports.ToolSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	if !ok {
		return ports.ToolSpec{}, false
	}
	return ports.ToolSpec{
		Name:         tool.Name,
		Description:  tool.Description,
		RiskLevel:    string(tool.RiskLevel),
		RequiredArgs: tool.RequiredArgs(),
	}, true
}

// CallTool runs a registered tool by name.
//
// The args are forwarded as given and the tool's own handler validates
// them; the registry adds no interpretation of its own, because a layer
// that "helpfully" coerced argument types would be a second, disagreeing
// definition of what each tool accepts.
func (r *Registry) CallTool(ctx context.Context, name string, args map[string]interface{}) (interface{}, error) {
	r.mu.RLock()
	tool, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no tool registered as %q", name)
	}
	if tool.Handler == nil {
		return nil, fmt.Errorf("tool %q is registered without a handler", name)
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	return tool.Handler(ctx, args)
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
	case adapter.TypeMQ:
		return "mq."
	case adapter.TypeHost:
		return "host."
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

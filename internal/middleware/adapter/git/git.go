// Package git 是 Git Repository 中间件 Adapter（路径 A 阶段 2 任务 2.5）。
//
// 当前为骨架实现：接口契约 + 8 个工具方法清单 + 与 git-artifact Linker 集成点。
// 完整实现在 Task 2.5 后续 PR：
//   - 引入 go-git（go.mod 变更）替代 os/exec
//   - 真实实现 7 个 git CLI 工具（commit_history / file_at_commit / blame / diff / search_code / list_repos / connect）
//   - 凭据：access token 经 pkg/credinject 注入；HTTPS 走 token，SSH 走 key file
//   - 增量 clone（git clone --depth=1）+ watch 实现实时性
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.1.7
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/middleware-adapter/spec.md
// 关联协议：openspec/changes/unified-platform-base-selection/protocols/git-artifact-v0.md
package git

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// Adapter 是 Git Repository Adapter 实现。
//
// 与 git-artifact Linker 共享反向索引能力：find_runtime_link 工具
// 通过 LinkerRegistry 调用 4 类符号反查（pg_query / redis_cmd / k8s_image / http_route），
// 把运行时符号映射回 commit + file:line。
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	// repo  *git.Repository  // 完整实现时引入 go-git
	// tenantID uint64

	// linkerReg 是 git-artifact Linker 注册中心（可选注入）。
	//
	// 当 linkerReg == nil 时，find_runtime_link 工具返回"linker 未配置"错误。
	// 生产环境由 cmd/opskeeper 在启动时注入。
	linkerReg *gitartifact.LinkerRegistry
}

// New 创建 Git Adapter 实例（不连接）。
//
// linkerReg 可为 nil（测试场景）；生产环境应通过 cmd/opskeeper 注入真实 LinkerRegistry。
func New(linkerReg *gitartifact.LinkerRegistry) *Adapter {
	return &Adapter{linkerReg: linkerReg}
}

// SetLinkerRegistry 注入 LinkerRegistry（允许延迟绑定）。
//
// 用于 cmd/opskeeper 启动时 LinkerRegistry 尚未就绪的场景。
func (a *Adapter) SetLinkerRegistry(reg *gitartifact.LinkerRegistry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.linkerReg = reg
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType {
	return adapter.TypeGitRepository
}

// Connect 建立连接（骨架：仅记录 spec，标记 connected）。
//
// 完整实现：
//  1. 解密 ConnectionSpec.DSN（pkg/credinject）
//     - 格式：https://oauth2:<token>@host/<owner>/<repo>.git 或 git@host:owner/repo.git
//  2. 选择协议：HTTPS（token） vs SSH（key file）
//  3. git.PlainClone() 或 git.OpenRepository()
//  4. 验证 HEAD 可达
//  5. 启动 commit 监听（可选：webhook 或 poll）
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.Timeout == 0 {
		conn.Timeout = 30 * time.Second
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = conn
	a.connected = true
	return nil
}

// Close 关闭连接。
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	return nil
}

// Health 健康检查（HEAD commit 可达性）。
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.HealthStatus{
		Status:    "healthy",
		LatencyMs: 0,
		Message:   "skeleton: real health probe pending Task 2.5",
		CheckedAt: time.Now(),
	}, nil
}

// Diagnose 通用诊断入口（Git 操作无传统"诊断"语义，返回空结果）。
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    []map[string]interface{}{},
		Summary:     fmt.Sprintf("skeleton: category=%s not applicable for git", q.Category),
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

// Execute 受限执行（Git 写操作 push/commit/tag — 当前骨架不开放写，预留给后续 PR）。
func (a *Adapter) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	if op.ApprovedBy == "" {
		return nil, adapter.ErrApprovalRequired
	}
	// Git 写操作风险等级建议：
	//   - push: L3（影响远端）
	//   - tag: L3（不可逆）
	//   - reset --hard: L4（破坏性，本地历史丢失）
	return &adapter.ExecResult{
		Operation: op.Operation,
		Success:   false,
		Message:   "skeleton: git write operations pending Task 2.5 followup",
		Impacted:  0,
		Metadata:  map[string]string{"skeleton": "true"},
	}, nil
}

// RegisterTools 注册 Git Adapter 暴露的工具方法到 Registry。
//
// 8 个工具方法（7 L0 + 1 L1）：
//
//	L0 只读：connect / list_repos / commit_history / file_at_commit / blame / diff / search_code
//	L1 诊断：find_runtime_link（运行时符号 → commit + file:line 反查，集成 git-artifact Linker）
//
// 风险等级说明：
//   - Git 操作主要是只读，几乎不涉及破坏性
//   - 写操作（push / tag / reset）走 Execute，由 cmdpolicy + Casbin 双重门控
func RegisterTools(reg *registry.Registry, a *Adapter) error {
	tools := []registry.Tool{
		// L0 只读 — git 资源管理
		makeTool("git.connect", adapter.RiskL0ReadOnly, "建立 Git 仓库连接（单 repo 或 monorepo 根）", nil),
		makeTool("git.list_repos", adapter.RiskL0ReadOnly, "列出 monorepo 下子仓库（按 .gitmodules / 目录约定）", nil),
		// L0 只读 — 历史与内容
		makeTool("git.commit_history", adapter.RiskL0ReadOnly, "commit 历史（带 author / range / path 过滤）", nil),
		makeTool("git.file_at_commit", adapter.RiskL0ReadOnly, "指定 commit 的文件内容快照", nil),
		makeTool("git.blame", adapter.RiskL0ReadOnly, "逐行归属（commit + author + line）", nil),
		makeTool("git.diff", adapter.RiskL0ReadOnly, "两个 commit / 分支 / tag 之间的 diff", nil),
		makeTool("git.search_code", adapter.RiskL0ReadOnly, "跨 commit 全文搜索（git log -S / git grep）", nil),
		// L1 诊断 — 运行时符号反查（路径 A 关键集成点）
		makeLinkTool("git.find_runtime_link", adapter.RiskL1Diagnostic, a),
	}
	return reg.RegisterTools(adapter.TypeGitRepository, tools)
}

// makeTool 构造占位工具（所有非集成工具用此构造）。
func makeTool(name string, risk adapter.RiskLevel, desc string, _ map[string]string) registry.Tool {
	return registry.Tool{
		Name:        name,
		Description: desc,
		RiskLevel:   risk,
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			return nil, fmt.Errorf("not_implemented: tool %q pending Task 2.5 followup", name)
		},
		ArgsSchema: map[string]string{},
	}
}

// makeLinkTool 构造 find_runtime_link 工具（实际调用 LinkerRegistry）。
//
// 参数约定：
//   - symbol_type: string，必填，取值 pg_query / redis_cmd / k8s_image / http_route
//   - input:      map[string]interface{}，必填，符号特定字段
//
// 返回 LinkResult JSON 兼容结构（map），便于 LLM 消费。
func makeLinkTool(name string, risk adapter.RiskLevel, a *Adapter) registry.Tool {
	return registry.Tool{
		Name:        name,
		Description: "运行时符号 → commit + file:line 反查（集成 git-artifact Linker，4 类符号）",
		RiskLevel:   risk,
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			symType, _ := args["symbol_type"].(string)
			if symType == "" {
				return nil, fmt.Errorf("missing required arg: symbol_type (pg_query/redis_cmd/k8s_image/http_route)")
			}
			input, _ := args["input"].(map[string]interface{})
			if input == nil {
				return nil, fmt.Errorf("missing required arg: input")
			}
			a.mu.RLock()
			reg := a.linkerReg
			a.mu.RUnlock()
			if reg == nil {
				return nil, fmt.Errorf("linker_registry not configured: git adapter has no LinkerRegistry injected")
			}
			return dispatchLink(ctx, reg, gitartifact.SymbolType(symType), input)
		},
		ArgsSchema: map[string]string{
			"symbol_type": "string",
			"input":       "map[string]interface{}",
		},
	}
}

// dispatchLink 把 input map 转换为对应 Linker 接受的强类型并调用 Link。
//
// 强类型映射（与 gitartifact 包对齐）：
//   - pg_query:   {query: string, database?: string}
//   - redis_cmd:  {cmd: string, key?: string}
//   - k8s_image:  {image: string, tag?: string}
//   - http_route: {method: string, path: string, handler?: string}
func dispatchLink(ctx context.Context, reg *gitartifact.LinkerRegistry, t gitartifact.SymbolType, input map[string]interface{}) (interface{}, error) {
	var typedInput interface{}
	switch t {
	case gitartifact.SymbolTypePGQuery:
		q, _ := input["query"].(string)
		db, _ := input["database"].(string)
		typedInput = gitartifact.PGQuery{Query: q, Database: db}
	case gitartifact.SymbolTypeRedisCmd:
		cmd, _ := input["cmd"].(string)
		key, _ := input["key"].(string)
		typedInput = gitartifact.RedisCmd{Cmd: cmd, Key: key}
	case gitartifact.SymbolTypeK8sImage:
		image, _ := input["image"].(string)
		tag, _ := input["tag"].(string)
		typedInput = gitartifact.K8sImage{Image: image, Tag: tag}
	case gitartifact.SymbolTypeHTTPRoute:
		method, _ := input["method"].(string)
		path, _ := input["path"].(string)
		handler, _ := input["handler"].(string)
		typedInput = gitartifact.HTTPRoute{Method: method, Path: path, Handler: handler}
	default:
		return nil, fmt.Errorf("unsupported symbol_type: %s", t)
	}

	result, err := reg.LinkByType(ctx, t, typedInput)
	if err != nil {
		return nil, fmt.Errorf("link failed for %s: %w", t, err)
	}
	if result == nil {
		return map[string]interface{}{"hit": false, "symbol_type": string(t)}, nil
	}
	// 转换为 map 便于序列化（保留 confidence + flag）
	out := map[string]interface{}{
		"hit":         true,
		"symbol_type": string(t),
		"commit":      result.Commit,
		"repo":        result.Repo,
		"file_path":   result.FilePath,
		"line_start":  result.LineStart,
		"line_end":    result.LineEnd,
		"confidence":  result.Confidence,
	}
	if result.Author != "" {
		out["author"] = result.Author
	}
	if result.CommitMsg != "" {
		out["commit_msg"] = result.CommitMsg
	}
	if result.Flag != "" {
		out["flag"] = result.Flag
	}
	if result.NeedsHumanConfirm() {
		out["needs_human_confirm"] = true
	}
	return out, nil
}

var _ adapter.Adapter = (*Adapter)(nil)

// Package git 是 Git Repository 中间件 Adapter（路径 A 阶段 2 任务 2.5）。
//
// 8 个工具：7 个只读（git.connect / list_repos / commit_history /
// file_at_commit / blame / diff / search_code）+ 1 个诊断
// （git.find_runtime_link，集成 git-artifact Linker）。
//
// 实现方式：调用 git CLI 而非 go-git（理由见 runner.go 顶部注释）。
// 写操作（push / commit / tag）**不实现**——它们需要审批流，而本适配器
// 只承担只读事实核查；Execute 一律返回 ErrApprovalRequired 之外的明确拒绝，
// 不做任何写。
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.1.7
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/middleware-adapter/spec.md
// 关联协议：openspec/changes/unified-platform-base-selection/protocols/git-artifact-v0.md
package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/knowledge/gitartifact"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/secretbox"
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
	runner    *repoRunner

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
func (a *Adapter) SetLinkerRegistry(reg *gitartifact.LinkerRegistry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.linkerReg = reg
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType {
	return adapter.TypeGitRepository
}

// Connect 打开仓库。
//
// DSN 两种形态：
//
//	/path/to/checkout            本地工作树，就地读取
//	/path/to/checkout#release/2  同上，但默认 revision 是该分支
//	https://host/owner/repo.git  远端，clone 到临时目录（Close 时删除）
//	git@host:owner/repo.git      同上（ssh）
//
// DSN 先过 secretbox 解密，与其它 adapter 一致。
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.Timeout == 0 {
		conn.Timeout = defaultTimeout
	}
	dsn, err := secretbox.Decrypt(conn.DSN)
	if err != nil {
		return fmt.Errorf("git: decrypt DSN: %w", err)
	}
	conn.DSN = dsn

	r, err := openRepo(ctx, conn)
	if err != nil {
		return err
	}
	if r.timeout != conn.Timeout {
		r.timeout = conn.Timeout
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Close the previous runner before replacing it: a second Connect on
	// a remote DSN would otherwise leak the first clone's directory.
	if a.runner != nil {
		_ = a.runner.close()
	}
	a.conn = conn
	a.runner = r
	a.connected = true
	return nil
}

// Close 释放连接（删除自己 clone 出来的临时目录）。
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	if a.runner != nil {
		err = a.runner.close()
	}
	a.runner = nil
	a.connected = false
	return err
}

// handle 返回当前 runner，未连接时返回 ErrNotConnected。
func (a *Adapter) handle() (*repoRunner, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected || a.runner == nil {
		return nil, adapter.ErrNotConnected
	}
	return a.runner, nil
}

// revision 返回当前 HEAD 与分支名（诊断信息）。
func (a *Adapter) revision() (head, branch string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.runner == nil {
		return "", ""
	}
	return a.runner.head, a.runner.branch
}

// Health 探活：读一次 HEAD。
//
// 探针是 `rev-parse --verify HEAD`：它不需要网络，在只读文件系统上也成立，
// 且它的失败能把"仓库不可达"与"仓库可达但没有 commit"分开——后者是一个
// 真实且常见的新仓库状态，不该报成故障。
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	r, err := a.handle()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	out, err := r.run(ctx, runOptions{argv: []string{"rev-parse", "--verify", "--quiet", "HEAD"}, allowExitOne: true})
	latency := time.Since(start)
	if err != nil {
		return &adapter.HealthStatus{
			Status:    "down",
			Message:   "git: " + r.describe() + ": " + err.Error(),
			CheckedAt: time.Now(),
		}, nil
	}
	sha := strings.TrimSpace(string(out.stdout))
	status := "healthy"
	message := r.describe()
	switch {
	case sha == "":
		status = "degraded"
		message += ": repository has no commits on HEAD"
	case len(sha) == 40:
		message += ": HEAD " + revLabel(sha)
		if r.branch != "" {
			message += " (" + r.branch + ")"
		}
	default:
		message += ": " + sha
	}
	if latency > 2*time.Second {
		status = "degraded"
	}
	return &adapter.HealthStatus{
		Status:    status,
		LatencyMs: latency.Milliseconds(),
		Message:   message,
		CheckedAt: time.Now(),
	}, nil
}

// Diagnose categories.
const (
	catHistory = "history"
	catOwner   = "ownership"
	catChanges = "changes"
)

func diagnoseCategories() []string {
	return []string{catChanges, catHistory, catOwner}
}

// Diagnose 把 category 路由到一次只读查询。
//
// 与 k8s / host adapter 相同的形态：git 没有"诊断"这个动作，但
// "最近改了什么""这行是谁写的""仓库结构如何"是同一批事实核查问题，
// 用同一个入口暴露比让调用方逐一记住工具名更好用。
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	p := params(q.Params)
	if p == nil {
		p = params{}
	}
	start := time.Now()
	var rows []map[string]any
	var summary string
	var err error
	suggestions := []string{}

	switch q.Category {
	case catHistory, "":
		if q.Limit > 0 {
			p["limit"] = q.Limit
		}
		rows, summary, err = runCommitHistory(ctx, a, p)
		if len(rows) > 0 {
			suggestions = append(suggestions, "git.diff from=<commit>^ to=<commit> 看这次改动")
		}
	case catOwner:
		rows, summary, err = runBlame(ctx, a, p)
		if len(rows) > 0 {
			suggestions = append(suggestions, "git.commit_history path="+str(rows[0], "path")+" 看该文件的完整变更史")
		}
	case catChanges:
		rows, summary, err = runDiff(ctx, a, p)
	case catListRepos:
		rows, summary, err = runListRepos(ctx, a, p)
	default:
		return nil, fmt.Errorf("git: unknown diagnose category %q (known: %s)",
			q.Category, strings.Join(append(diagnoseCategories(), catListRepos), ", "))
	}
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		suggestions = nil
	}
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    rows,
		Summary:     summary,
		Suggestions: suggestions,
		ElapsedMs:   time.Since(start).Milliseconds(),
	}, nil
}

// catListRepos is the fourth diagnose category, named here so the error
// message above can list it without duplicating the literal.
const catListRepos = "repos"

// Collect 采集仓库概况。
//
// 返回的是一次快照，并且如实说明：git 的读是一次点查询，把它画成时间序列
// 只会得到一条平线。
func (a *Adapter) Collect(ctx context.Context, q adapter.CollectQuery) (*adapter.CollectResult, error) {
	r, err := a.handle()
	if err != nil {
		return nil, err
	}
	metrics := map[string]any{
		"head":   r.head,
		"branch": r.branch,
	}
	if n, err := r.run(ctx, runOptions{argv: []string{"rev-list", "--count", r.defaultRev()}}); err == nil {
		// rev-list --count prints one integer; a value this cannot parse
		// is dropped rather than reported as zero, because "no commits"
		// and "could not read the count" are different statements.
		if count, convErr := strconv.Atoi(strings.TrimSpace(string(n.stdout))); convErr == nil {
			metrics["commit_count"] = count
		}
	}
	if out, err := r.run(ctx, runOptions{argv: []string{"rev-parse", "--is-shallow-repository"}, allowExitOne: true}); err == nil {
		metrics["shallow"] = strings.TrimSpace(string(out.stdout)) == "true"
	}
	if out, err := r.run(ctx, runOptions{argv: []string{"ls-tree", "-r", "-z", "--name-only", r.defaultRev()}, maxBytes: 8 << 20}); err == nil {
		files := splitNUL(out.stdout)
		metrics["tracked_files"] = len(files)
	}
	if out, err := r.run(ctx, runOptions{argv: []string{"shortlog", "-sne", r.defaultRev()}, maxBytes: 256 << 10}); err == nil {
		metrics["contributors"] = len(splitLines(out.stdout))
	}
	return &adapter.CollectResult{
		Metrics: metrics,
		Samples: []map[string]interface{}{},
		Metadata: map[string]string{
			"kind": "snapshot",
			"repo": r.describe(),
		},
	}, nil
}

// Execute 拒绝一切写操作。
//
// 这不是"待实现"，而是一个决定：git 写操作（push / tag / reset）影响的是
// 远端与不可逆历史，平台目前没有任何一条经过审批的 git 写路径。与其实现一个
// 拿到了 ApprovedBy 就执行 push 的入口，不如让它明确拒绝并把原因说清——
// 一个声称做了审批而实际只是检查了字段非空的闸门，比没有闸门更危险。
func (a *Adapter) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(op.ApprovedBy) == "" {
		return nil, adapter.ErrApprovalRequired
	}
	// 风险参考（供未来开放写操作时使用）：
	//   push   → L3（影响远端）
	//   tag    → L3（不可逆）
	//   reset --hard → L4（本地历史丢失）
	return nil, fmt.Errorf("%w: git.%s (git writes have no approved path on this platform; reads only)",
		adapter.ErrInvalidOp, op.Operation)
}

// RegisterTools 注册 Git Adapter 暴露的工具方法到 Registry。
//
// 8 个工具方法（7 L0 + 1 L1）：
//
//	L0 只读：connect / list_repos / commit_history / file_at_commit / blame / diff / search_code
//	L1 诊断：find_runtime_link（运行时符号 → commit + file:line 反查）
func RegisterTools(reg *registry.Registry, a *Adapter) error {
	tools := []registry.Tool{
		// L0 只读 — git 资源管理
		makeTool("git.connect", adapter.RiskL0ReadOnly,
			"建立 Git 仓库连接（本地路径 / remote URL，DSN 经 secretbox 解密）",
			map[string]string{"dsn": "string!", "timeout_seconds": "int"}, connectOp(a)),
		makeTool("git.list_repos", adapter.RiskL0ReadOnly,
			"列出仓库内嵌的子仓库（gitlink / .gitmodules）", map[string]string{"rev": "string"}, readOp(a, runListRepos)),
		// L0 只读 — 历史与内容
		makeTool("git.commit_history", adapter.RiskL0ReadOnly,
			"commit 历史（author / message / path 过滤，rev 默认为 HEAD）",
			map[string]string{"rev": "string", "path": "string", "author": "string", "message": "string", "limit": "int"},
			readOp(a, runCommitHistory)),
		makeTool("git.file_at_commit", adapter.RiskL0ReadOnly,
			"指定 commit 的文件内容快照（二进制自动 base64）",
			map[string]string{"path": "string!", "rev": "string"}, readOp(a, runFileAtCommit)),
		makeTool("git.blame", adapter.RiskL0ReadOnly,
			"逐行归属（commit + author + 行号），可指定行范围",
			map[string]string{"path": "string!", "rev": "string", "line_start": "int", "line_end": "int"},
			readOp(a, runBlame)),
		makeTool("git.diff", adapter.RiskL0ReadOnly,
			"两个 revision 之间的文件级增删统计（numstat，含重命名）",
			map[string]string{"from": "string", "to": "string", "path": "string", "limit": "int"},
			readOp(a, runDiff)),
		makeTool("git.search_code", adapter.RiskL0ReadOnly,
			"在指定 revision 的树内做字面量全文搜索（git grep -F，固定字符串）",
			map[string]string{"pattern": "string!", "rev": "string", "path": "string", "limit": "int", "ignore_case": "bool"},
			readOp(a, runSearchCode)),
		// L1 诊断 — 运行时符号反查（路径 A 关键集成点）
		makeLinkTool("git.find_runtime_link", adapter.RiskL1Diagnostic, a),
	}
	return reg.RegisterTools(adapter.TypeGitRepository, tools)
}

// handler 是注册到 Registry 的工具实现体。
type handler func(ctx context.Context, args map[string]any) (any, error)

// connectOp 是唯一需要自己解析 DSN 再 Connect 的工具；其余工具用 readOp。
func connectOp(a *Adapter) handler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		return runConnect(ctx, a, args)
	}
}

type readRun func(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error)

// makeTool 构造一个工具。
//
// ArgsSchema 里以 "!" 结尾的条目是必填参数（见 registry.RequiredMarker）：
// 能力闸门据此判断"这个名字对应的动作是否真的能被派发"，一个必填参数
// 没有被声明成必填，闸门就会把一个派发不出去的动作报成已覆盖。
func makeTool(name string, risk adapter.RiskLevel, desc string, schema map[string]string, h handler) registry.Tool {
	if schema == nil {
		schema = map[string]string{}
	}
	return registry.Tool{
		Name:        name,
		Description: desc,
		RiskLevel:   risk,
		ArgsSchema:  schema,
		Handler:     h,
	}
}

// readOp 把一次只读查询包成统一信封。
//
// 空结果是发现，不是失败：rows 为 [] 且 count 为 0，summary 说明"没查到"。
func readOp(a *Adapter, run readRun) handler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		if _, err := a.handle(); err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]any{}
		}
		rows, summary, err := run(ctx, a, args)
		if err != nil {
			return nil, err
		}
		if rows == nil {
			rows = []map[string]any{}
		}
		return map[string]any{"rows": rows, "count": len(rows), "summary": summary}, nil
	}
}

// makeLinkTool 构造 find_runtime_link 工具（调用 LinkerRegistry）。
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
			"symbol_type": "string!",
			"input":       "map[string]interface{}!",
		},
		// The one tool in this repository whose arguments are not a flat
		// name → type map, so it is the one place ParamsSchema is set
		// instead of being derived from ArgsSchema.
		//
		// It is written out because the difference between the four input
		// shapes is the whole contract: a caller that sends {cmd, key} for
		// a k8s_image lookup gets a type error from the linker rather than
		// an answer, and a model that cannot see which fields each symbol
		// type takes will produce exactly that. The `allOf`/`if`/`then`
		// branches state the four shapes rather than describing them in
		// prose beside an untyped object.
		ParamsSchema: linkToolSchema,
	}
}

// linkToolSchema is the argument schema for git.find_runtime_link.
//
// Kept next to the dispatcher it describes: dispatchLink is what validates
// each branch by hand, and a schema in another file would be free to drift
// from the code that actually rejects a malformed input.
const linkToolSchema = `{
  "type": "object",
  "properties": {
    "symbol_type": {
      "type": "string",
      "enum": ["pg_query", "redis_cmd", "k8s_image", "http_route"],
      "description": "Which kind of runtime symbol to look up. It selects the shape of the input object."
    },
    "input": {
      "type": "object",
      "description": "The symbol itself. Its fields depend on symbol_type; see the branches below."
    }
  },
  "required": ["symbol_type", "input"],
  "allOf": [
    {
      "if": {"properties": {"symbol_type": {"const": "pg_query"}}},
      "then": {"properties": {"input": {
        "type": "object",
        "properties": {"query": {"type": "string"}, "database": {"type": "string"}},
        "required": ["query"]
      }}}
    },
    {
      "if": {"properties": {"symbol_type": {"const": "redis_cmd"}}},
      "then": {"properties": {"input": {
        "type": "object",
        "properties": {"cmd": {"type": "string"}, "key": {"type": "string"}},
        "required": ["cmd"]
      }}}
    },
    {
      "if": {"properties": {"symbol_type": {"const": "k8s_image"}}},
      "then": {"properties": {"input": {
        "type": "object",
        "properties": {"image": {"type": "string"}, "tag": {"type": "string"}},
        "required": ["image"]
      }}}
    },
    {
      "if": {"properties": {"symbol_type": {"const": "http_route"}}},
      "then": {"properties": {"input": {
        "type": "object",
        "properties": {"method": {"type": "string"}, "path": {"type": "string"}, "handler": {"type": "string"}},
        "required": ["method", "path"]
      }}}
    }
  ]
}`

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
	// 转换为 map 便于序列化（保留 confidence + needs_human_confirm）
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
	// author / commit_msg 由 indexer 从 Artifact.Meta（CI 透传）写入，
	// 缺席表示这条流水线没透传这两项，而不是这个提交没有作者。
	if result.Author != "" {
		out["author"] = result.Author
	}
	if result.CommitMsg != "" {
		out["commit_msg"] = result.CommitMsg
	}
	// needs_human_confirm 是这个端点唯一的人工确认信号，由 confidence 算出，
	// 不再由结构体上的 flag 列携带——那份拷贝的唯一写者在 HTTP 路径、唯一
	// 读者在这个工具路径，两条通道互不相交，于是同一个判断在两条通道各用
	// 一个键讲，其中一条还恒为空。
	if result.NeedsHumanConfirm() {
		out["needs_human_confirm"] = true
	}
	return out, nil
}

var _ adapter.Adapter = (*Adapter)(nil)

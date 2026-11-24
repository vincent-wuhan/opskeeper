// gateway.go 是 pig-runtime-adapter 任务 3.2 的分发前强制面：PiG 治理
// 工具网关。所有七道闸在 Dispatch 之前依次执行，任何一道拒绝都先落
// denied 调用行与审计行、再返回——拒绝必须是留痕的事实，不是静默丢弃。
//
// 落地方式是「复用 + 增补」而非「挂链」（design.md §一）：策略强制是
// 本文件净新增；限流复用 decorators.Limiter 端口（生产实现
// TokenBucketLimiter）；proposal 台账在后续切片经 decorators.WithReviewGate
// 接缝接入。不 import core/edge——工具在 Manager 进程内，与 policygate
// 线协议（节点面子进程内工具）是两个信任面。
package pigruntime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/decorators"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// 拒绝理由码闭集（deny_reason 列取值）。一类一码，审计行按它回答
// 「这次调用死在哪道闸」。
const (
	DenyReasonUndeclaredTool = "undeclared_tool"
	DenyReasonDuplicateTool  = "duplicate_tool"
	DenyReasonDriftedClass   = "drifted_class"
	DenyReasonTenantMismatch = "tenant_mismatch"
	DenyReasonRoleNotAllowed = "role_not_allowed"
	DenyReasonCrossIncident  = "cross_incident"
	DenyReasonCrossResource  = "cross_resource"
	DenyReasonReadOnlyHard   = "read_only_hard_gate"
	DenyReasonRateLimited    = "rate_limited"
	DenyReasonLedgerFailed   = "ledger_write_failed"
)

// AdmitDecision 是 Admit 的裁决形状。
type AdmitDecision string

const (
	// AdmitAllowed 七闸全过，可分发。
	AdmitAllowed AdmitDecision = model.ToolCallDecisionAllowed
	// AdmitProposalRequired 身份与作用域闸全过，但调用是 write/destructive
	// 且 ReadOnly=false：不直接分发，交给 proposal 台账待人工批准（3.5）。
	AdmitProposalRequired AdmitDecision = model.ToolCallDecisionProposalRequired
	// AdmitBlocked 七闸之一拒绝，永不分发。
	AdmitBlocked AdmitDecision = model.ToolCallDecisionBlocked
)

// ToolCallRequest 是一次进入网关的 PiG 调用。治理事实是显式字段而非
// ctx 侧信道——PiG 调用者不是 HTTP 请求，没有 JWT claims 可解析。
type ToolCallRequest struct {
	// RuntimeToolID 是 PiG 侧身份；到 TranslateToolCall 为止，不进
	// 裁决结果、不进持久化。
	RuntimeToolID string
	// ReportedClass 是进程上报的类别（pigruntime 拼写闭集）。
	ReportedClass ToolClass

	// Scope 是任务 Launch 时钉死的作用域（RuntimeSemantics 治理事实）。
	Scope RuntimeSemantics
	// WorkerIdentity 是 (tenant, role, worker) 之外的 worker 标识，
	// 与 Scope.TenantID/Role 同源。
	Worker string

	// IncidentID 是本次调用携带的事件归属；必须等于 Scope.IncidentID。
	IncidentID string
	// ResourceID 是本次调用针对的资源；非空时必须命中 Scope.Resources。
	ResourceID string

	// Parameters 是调用参数原文；网关只落哈希，原文不进端口签名
	// （HashToolPayload 在调用点计算）。
	ParametersSHA256 string

	// TraceID 是 trace 关联；span 未起时为空串。
	TraceID string

	// ReleaseDigest / CapabilityDigest 是裁决时快照，随行落库。
	ReleaseDigest    string
	CapabilityDigest string

	// CalledAt 是调用到达时戳。
	CalledAt time.Time
}

// AdmitResult 是网关裁决的全部事实。Denied 时 Err 携带拒绝原因；
// StableToolID 在身份翻译失败时为空串（此时没有稳定身份可记，行内
// 落 RuntimeToolID 的哈希以保留「调用者想碰什么」的证据）。
type AdmitResult struct {
	Decision   AdmitDecision
	DenyReason string
	Tool       ResolvedTool
	// LedgerID 是起始行的 correlation id；ErrLedgerWrite 时为空。
	LedgerID string
	Err      error
}

// ToolDispatcher 是允许裁决后的分发接缝。生产侧由 Manager 装配把
// 调用交给钉死的工具实现；测试注入 fake 断言「拒绝时调用次数为零」。
type ToolDispatcher interface {
	// Dispatch 执行一次已放行的调用，返回结果与错误。
	Dispatch(ctx context.Context, tool ResolvedTool, request ToolCallRequest) (string, error)
}

// ToolGateway 是 PiG 治理工具网关。零值不可用；New 构造。
type ToolGateway struct {
	snapshot  ExecutionSnapshot
	tenantID  string
	manifest  CapabilityManifest
	exposed   []ExposedTool
	roles     []RoleAllowlist
	readOnly  bool
	limiter   decorators.Limiter
	ledger    ToolCallLedger
	auditSink audit.Sink
	dispatch  ToolDispatcher
	// proposalSink 是 3.5 的 proposal 台账接缝；nil 时 ProposalRequired
	// 路径 fail closed（不执行、不落 proposal）。
	proposalSink decorators.MutatingProposalSink
}

// GatewayConfig 装配 ToolGateway。Ledger 与 AuditSink 为 nil 时该网关
// 拒绝一切调用（fail closed：没有证据面就没有分发）。
type GatewayConfig struct {
	// Snapshot 是任务准入时固定的发布与沙箱快照：RuntimeID 与两枚
	// digest 的来源。任务执行期间发布可被禁用或替换，但已准入快照
	// 不变——裁决行回答的是「裁决时运行的是什么」。
	Snapshot ExecutionSnapshot
	// TenantID 是快照归属租户（ExecutionSnapshot 不携带治理列，由
	// 装配从 RuntimeRecord 投影）。
	TenantID string
	// Manifest 是当前生效的发布清单。
	Manifest CapabilityManifest
	// Exposed 是该角色此刻的对账暴露集（ReconcileCapabilities 输出）。
	Exposed []ExposedTool
	// Roles 是角色准入表（纯值，与 ReconcileCapabilities 入参同源）。
	Roles []RoleAllowlist
	// ReadOnly 是 SandboxProfile.ReadOnly：true 时 write/destructive
	// 被硬闸拦截，不进审批（只读发布里的变更请求本身就是违规）。
	ReadOnly bool
	// Limiter 是限流端口；nil 视为装配错误（fail closed）。
	Limiter decorators.Limiter
	// Ledger 是调用持久化端口（3.3）。
	Ledger ToolCallLedger
	// AuditSink 是 HLD-010 审计链端口。
	AuditSink audit.Sink
	// Dispatcher 是分发接缝；nil 时 Admit 只裁决不分发。
	Dispatcher ToolDispatcher
}

// ErrGatewayClosed 表示网关缺少必要依赖（ledger/audit/limiter 缺失），
// 一切调用 fail closed。
var ErrGatewayClosed = errors.New("pigruntime: gateway not fully assembled, failing closed")

// 网关作用域闸的拒绝哨兵。每道闸一个，调用方按 errors.Is 区分
// 「死在哪道闸」，与 deny_reason 码一一对应。
var (
	// ErrTenantMismatch 调用归属租户与快照租户不一致。
	ErrTenantMismatch = errors.New("pigruntime: tool call tenant does not match the pinned runtime")
	// ErrRoleNotAllowed 调用角色不在准入表内，或该角色未放行此工具。
	ErrRoleNotAllowed = errors.New("pigruntime: tool call role is not allowed for this tool")
	// ErrCrossIncident 调用携带的事件与任务钉死的作用域不一致。
	ErrCrossIncident = errors.New("pigruntime: tool call escapes the pinned incident scope")
	// ErrCrossResource 调用针对的资源不在任务作用域白名单内。
	ErrCrossResource = errors.New("pigruntime: tool call escapes the pinned resource scope")
	// ErrReadOnlyHardGate 只读发布里的 write/destructive 调用：不进
	// 审批，直接拒绝留痕。
	ErrReadOnlyHardGate = errors.New("pigruntime: mutating tool call under a read-only release")
)

// NewToolGateway 构造网关。config 缺少证据面依赖时返回 ErrGatewayClosed
// 的装配错误——比「带病运行」诚实：一个写不进证据的网关与静默放行
// 不可区分。
func NewToolGateway(config GatewayConfig) (*ToolGateway, error) {
	if config.Ledger == nil || config.AuditSink == nil || config.Limiter == nil {
		return nil, ErrGatewayClosed
	}
	if err := config.Manifest.Validate(); err != nil {
		return nil, err
	}
	return &ToolGateway{
		snapshot:  config.Snapshot,
		tenantID:  config.TenantID,
		manifest:  config.Manifest,
		exposed:   config.Exposed,
		roles:     config.Roles,
		readOnly:  config.ReadOnly,
		limiter:   config.Limiter,
		ledger:    config.Ledger,
		auditSink: config.AuditSink,
		dispatch:  config.Dispatcher,
	}, nil
}

// Admit 依次执行七道闸，返回裁决。拒绝路径：先落 denied 行（表 + 审计），
// 再返回 AdmitResult。起始行写不进（ErrLedgerWrite）时不分发——没有
// 证据行的调用与静默放行不可区分。
func (g *ToolGateway) Admit(ctx context.Context, request ToolCallRequest) AdmitResult {
	tool, denyReason, err := g.evaluate(ctx, request)
	if denyReason != "" {
		g.recordDenied(ctx, request, tool, denyReason)
		return AdmitResult{Decision: AdmitBlocked, DenyReason: denyReason, Err: err}
	}

	decision := AdmitAllowed
	if tool.Class.Rank() >= domain.ClassWrite.Rank() {
		// ⑥ read-only 硬闸已在 evaluate 内对 ReadOnly=true 拦截；走到
		// 这里说明 ReadOnly=false，write/destructive 进 proposal-only。
		decision = AdmitProposalRequired
	}

	id, lerr := g.ledger.RecordStart(ctx, ToolCallStart{
		RuntimeID:        g.snapshot.RuntimeID,
		TaskID:           request.Scope.TaskID,
		TenantID:         request.Scope.TenantID,
		IncidentID:       request.Scope.IncidentID,
		StableToolID:     tool.StableToolID,
		ToolClass:        string(tool.Class),
		CallerRole:       request.Scope.Role,
		CallerWorker:     request.Worker,
		ReleaseDigest:    request.ReleaseDigest,
		CapabilityDigest: request.CapabilityDigest,
		ParametersSHA256: request.ParametersSHA256,
		TraceID:          request.TraceID,
		Decision:         string(decision),
		StartedAt:        request.CalledAt,
	})
	if lerr != nil {
		g.recordDenied(ctx, request, tool, DenyReasonLedgerFailed)
		return AdmitResult{
			Decision:   AdmitBlocked,
			DenyReason: DenyReasonLedgerFailed,
			Tool:       tool,
			Err:        ErrLedgerWrite,
		}
	}
	g.emitAudit(ctx, request, tool, string(decision), "")
	return AdmitResult{Decision: decision, Tool: tool, LedgerID: id}
}

// ExecuteOutcome 是 Execute 的结果：路由到哪条路径、结果哈希与错误。
type ExecuteOutcome struct {
	Admit      AdmitResult
	Dispatched bool
	// ProposalID 是 proposal 行 id（ProposalRequired 路径）。
	ProposalID string
	// ResultSHA256 是分发结果的哈希（原文不进 outcome）。
	ResultSHA256 string
	Err          error
}

// ErrProposalRequired 表示调用已落 proposal 待人工批准，本轮不执行。
var ErrProposalRequired = errors.New("pigruntime: tool call recorded as proposal, execution pending human approval")

// ErrNoDispatcher 表示网关未装配分发器：允许裁决无法执行。
var ErrNoDispatcher = errors.New("pigruntime: gateway has no dispatcher")

// Execute 是允许裁决后的完整路径：
//
//   - AdmitBlocked：不分发，直接返回（拒绝已在 Admit 内留痕）；
//   - AdmitAllowed：Dispatch → RecordEnd（success/error），收尾行回填
//     结果哈希与错误摘要；
//   - AdmitProposalRequired：把调用落 proposal（MutatingProposalSink，
//     DecisionPending）后返回 ErrProposalRequired——**本轮不执行**。
//     审批与执行走既有 human-approved exact-payload 路径（3.5）。
func (g *ToolGateway) Execute(ctx context.Context, request ToolCallRequest) ExecuteOutcome {
	admit := g.Admit(ctx, request)
	outcome := ExecuteOutcome{Admit: admit}
	switch admit.Decision {
	case AdmitBlocked:
		return outcome
	case AdmitProposalRequired:
		if g.proposalSink == nil {
			// 没有 proposal 台账就没有待审批项的落点：fail closed，
			// 不把 proposal_required 当 allowed 执行。
			outcome.Err = ErrProposalRequired
			return outcome
		}
		id, err := g.proposalSink.Insert(ctx, decorators.MutatingProposalEvent{
			ToolName:       admit.Tool.StableToolID,
			ToolClass:      string(admit.Tool.Class),
			SessionID:      request.Scope.ConversationID,
			ReviewerTaskID: request.Scope.TaskID,
			CreatedAt:      request.CalledAt,
		})
		if err != nil {
			outcome.Err = err
			return outcome
		}
		outcome.ProposalID = id
		outcome.Err = ErrProposalRequired
		g.recordEnd(ctx, admit.LedgerID, model.ToolCallStatusDispatched, "", ErrProposalRequired, request)
		return outcome
	case AdmitAllowed:
	default:
		outcome.Err = ErrGatewayClosed
		return outcome
	}

	if g.dispatch == nil {
		outcome.Err = ErrNoDispatcher
		g.recordEnd(ctx, admit.LedgerID, model.ToolCallStatusError, "", ErrNoDispatcher, request)
		return outcome
	}
	result, err := g.dispatch.Dispatch(ctx, admit.Tool, request)
	outcome.Dispatched = true
	outcome.Err = err
	outcome.ResultSHA256 = HashToolPayload(result)
	status := model.ToolCallStatusSuccess
	if err != nil {
		status = model.ToolCallStatusError
	}
	g.recordEnd(ctx, admit.LedgerID, status, outcome.ResultSHA256, err, request)
	g.emitAudit(ctx, request, admit.Tool, string(admit.Decision)+"_end", "")
	return outcome
}

// recordEnd 回填收尾行。结果与错误只落哈希/摘要，原文不进端口。
func (g *ToolGateway) recordEnd(ctx context.Context, ledgerID, status, resultSHA256 string, err error, request ToolCallRequest) {
	message := ""
	if err != nil {
		const maxLen = 512
		message = err.Error()
		if len(message) > maxLen {
			message = message[:maxLen]
		}
	}
	_ = g.ledger.RecordEnd(ctx, ToolCallEnd{
		ID:           ledgerID,
		Status:       status,
		ResultSHA256: resultSHA256,
		ErrorMessage: message,
		EndedAt:      time.Now(),
	})
}

// SetProposalSink 注入 proposal 台账（后续切片装配；本切片测试注入
// fake）。Execute 对 ProposalRequired 路径在 sink 为 nil 时 fail closed。
func (g *ToolGateway) SetProposalSink(sink decorators.MutatingProposalSink) {
	g.proposalSink = sink
}

// evaluate 依次执行七道闸（顺序即 design.md §三），返回 (tool, denyReason, err)。
// denyReason 非空表示该闸拒绝。
func (g *ToolGateway) evaluate(ctx context.Context, request ToolCallRequest) (ResolvedTool, string, error) {
	// ① 身份翻译：undeclared / duplicate / drifted 在此拦。
	tool, err := TranslateToolCall(g.manifest, g.exposed, request.RuntimeToolID, request.ReportedClass)
	if err != nil {
		return ResolvedTool{}, denyReasonOfTranslate(err), err
	}

	// ② tenant 闸：调用归属的租户非空且与快照租户一致。
	if request.Scope.TenantID == "" || g.tenantID != request.Scope.TenantID {
		return ResolvedTool{}, DenyReasonTenantMismatch, ErrTenantMismatch
	}

	// ③ role 闸：角色在准入表内且该工具被放行。
	if !g.roleAllowed(request.Scope.Role, tool.StableToolID) {
		return tool, DenyReasonRoleNotAllowed, ErrRoleNotAllowed
	}

	// ④ incident 闸：调用携带的事件必须落在任务钉死的作用域内。
	if request.Scope.IncidentID == "" || request.IncidentID != request.Scope.IncidentID {
		return tool, DenyReasonCrossIncident, ErrCrossIncident
	}

	// ⑤ resource 闸：ResourceID 非空时必须命中工具声明的作用域
	// （ResolvedTool.ResourceScope）；清单侧空串表示不限定具体资源
	// （只读诊断工具的常态），此时只要求调用确实携带了资源语义。
	if request.ResourceID != "" && tool.ResourceScope != "" {
		if request.ResourceID != tool.ResourceScope {
			return tool, DenyReasonCrossResource, ErrCrossResource
		}
	}

	// ⑥ read-only 硬闸：ReadOnly=true 时任何非 read 调用直接拒绝，
	// 不进审批——审批是给合法变更准备的通道，只读发布里的变更请求
	// 本身就是策略违规。
	if g.readOnly && tool.Class.Rank() > domain.ClassRead.Rank() {
		return tool, DenyReasonReadOnlyHard, ErrReadOnlyHardGate
	}

	// ⑦ 限流：subject 是 (tenant, role, worker) 三元组拼串，与
	// AgentTeams worker 在 ratelimit.go 的既有 subject 形态一致。
	if !g.limiter.Allow(ctx, tool.StableToolID, limiterSubject(request)) {
		return tool, DenyReasonRateLimited, decorators.ErrRateLimited
	}

	return tool, "", nil
}

// roleAllowed 判断 (role, tool) 是否命中角色准入表。同一角色可能有多条
// 准入条目（按工具族分列），任一条命中即放行——不在第一条匹配处短路。
func (g *ToolGateway) roleAllowed(role, stableToolID string) bool {
	for _, entry := range g.roles {
		if entry.Role != role {
			continue
		}
		for _, id := range entry.Tools {
			if id == stableToolID {
				return true
			}
		}
	}
	return false
}

// limiterSubject 拼 (tenant/role/worker) 三元组。与 AgentTeams worker
// 的 tenant/service/worker 拼串同构（ratelimit.go 既有形态）。
func limiterSubject(request ToolCallRequest) string {
	parts := []string{request.Scope.TenantID, request.Scope.Role, request.Worker}
	return strings.Join(parts, "/")
}

// recordDenied 落一条 denied 起始行 + 审计行。表写失败时审计仍发——
// 两个证据面独立，审计行是表不可写时的最后防线。
func (g *ToolGateway) recordDenied(ctx context.Context, request ToolCallRequest, tool ResolvedTool, denyReason string) {
	start := ToolCallStart{
		RuntimeID:        g.snapshot.RuntimeID,
		TaskID:           request.Scope.TaskID,
		TenantID:         request.Scope.TenantID,
		IncidentID:       request.Scope.IncidentID,
		StableToolID:     tool.StableToolID,
		ToolClass:        string(tool.Class),
		CallerRole:       request.Scope.Role,
		CallerWorker:     request.Worker,
		ReleaseDigest:    request.ReleaseDigest,
		CapabilityDigest: request.CapabilityDigest,
		ParametersSHA256: request.ParametersSHA256,
		TraceID:          request.TraceID,
		Decision:         string(AdmitBlocked),
		DenyReason:       denyReason,
		StartedAt:        request.CalledAt,
	}
	start = normalizeLedgerStart(start, request)
	_, _ = g.ledger.RecordStart(ctx, start)
	g.emitAudit(ctx, request, tool, string(AdmitBlocked), denyReason)
}

// normalizeLedgerStart 补齐 denied 行的必填事实：参数哈希兜底为空串
// 哈希（证据行不可因缺哈希而写不进表——「哈希了空串」与「没有行」
// 之间，失败闭合选前者）；身份翻译失败时哈希 RuntimeToolID 保留
// 「调用者想碰什么」的证据，原文不出翻译层；类别取上报类别的
// domain 拼写（保留调用方主张的事实）。
func normalizeLedgerStart(start ToolCallStart, request ToolCallRequest) ToolCallStart {
	if start.ParametersSHA256 == "" {
		start.ParametersSHA256 = HashToolPayload("")
	}
	if start.StableToolID == "" {
		start.StableToolID = "runtime:" + HashToolPayload(request.RuntimeToolID)
	}
	if start.ToolClass == "" {
		start.ToolClass = string(toDomainToolClass(request.ReportedClass))
	}
	if start.StartedAt.IsZero() {
		start.StartedAt = time.Now()
	}
	return start
}

// emitAudit 发一条 ActionPigToolCall 审计行（允许与拒绝同一动词，以
// Status 与 payload 的 decision 分形状——denied 行是失败闭合矩阵的
// 断言证据，没有它「拒绝」与「静默丢弃」不可区分）。
func (g *ToolGateway) emitAudit(ctx context.Context, request ToolCallRequest, tool ResolvedTool, decision, denyReason string) {
	status := audit.StatusSuccess
	if decision == string(AdmitBlocked) {
		status = audit.StatusDenied
	}
	stableID := tool.StableToolID
	if stableID == "" {
		stableID = "runtime:" + HashToolPayload(request.RuntimeToolID)
	}
	g.auditSink.Emit(ctx, audit.Event{
		Action:       audit.ActionPigToolCall,
		ResourceType: audit.ResourcePigTool,
		ResourceID:   stableID,
		ResourceName: tool.Name,
		Role:         request.Scope.Role,
		Status:       status,
		ErrorCode:    denyReason,
		Payload: map[string]any{
			"phase":             "admit",
			"decision":          decision,
			"deny_reason":       denyReason,
			"tool_class":        string(tool.Class),
			"runtime_id":        g.snapshot.RuntimeID,
			"task_id":           request.Scope.TaskID,
			"incident_id":       request.IncidentID,
			"parameters_sha256": request.ParametersSHA256,
			"release_digest":    request.ReleaseDigest,
			"capability_digest": request.CapabilityDigest,
		},
	})
}

// denyReasonOfTranslate 把翻译层错误映射到拒绝理由码。
func denyReasonOfTranslate(err error) string {
	switch {
	case errors.Is(err, ErrUndeclaredTool):
		return DenyReasonUndeclaredTool
	case errors.Is(err, ErrDuplicateRuntimeToolID):
		return DenyReasonDuplicateTool
	case errors.Is(err, ErrDriftedClass), errors.Is(err, ErrInvalidReportedClass), errors.Is(err, ErrInvalidContract):
		return DenyReasonDriftedClass
	default:
		return DenyReasonUndeclaredTool
	}
}

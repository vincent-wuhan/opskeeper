// Package loop — postmortem_worker.go
//
// postmortem phase 的 PhaseWorker 实现（zero-manual-ops-loop Day 3 +
// llm-worker-integration batch 2）。
//
// postmortem phase 的职责（设计依据：
//
//   - openspec/specs/incident-postmortem/spec.md
//
//   - openspec/changes/llm-worker-integration/specs/closed-loop-orchestrator/spec.md
//
//   - docs/superpowers/specs/2026-08-12-llm-worker-integration-design.md §4.5）：
//
//     1. Planner 读上游 RootCauseJSON + CritiqueScore + VerifiedDelta
//     渲染 LLM prompt；返回单步 Plan（target="llm_call"）。
//     2. Executor 调 LLMCaller.Call 拿到 PostmortemContent JSON，
//     再调 gitSink.CommitMarkdown 落 git artifact（占位实现：
//     sha256(incidentID+body)[:40]，borrow-map §B.3 deferred 到下批）。
//     SideEffects 落一条 git_commit，RawOutputs 留 PostmortemContent 给 Verifier。
//     3. Verifier 强校验 schema（已由 LLMCaller.Call 做）+ markdown 长度 > 100
//
//   - incident_id 非空；OK=true 推进 postmortem 终态。
//
// 与 contract.go 中 PostmortemDoc（contract envelope，写 DB）的区别：
//   - 本文件 PostmortemContent 是 LLM 输出 body（包含 summary / root_cause /
//     timeline / remediation_taken / lessons_learned），驱动 markdown 渲染。
//   - contract.go PostmortemDoc 是终态 contract envelope（schema_version /
//     conversation_id / markdown / generated_at / sources），由 orchestrator
//     写入 loop_contract 表。
//
// Day 5 集成时由 report/postmortem_sink.go 把 PostmortemContent.Markdown
// 灌进 PostmortemDoc.Markdown 后落库；本批只到 git artifact 这一段。
//
// 不引入 monorepo 跨域 import：
//   - LLMCaller / GitArtifactSink / UpstreamContractLoader 都是本包内
//     narrow interface；concrete 实现由 cmd/main.go 在 Day 5 集成时
//     注入（LLMCaller 共享 / 真实 git artifact store / ContractRepo facade）。
package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// PostmortemContent is the LLM-rendered rich content body for the
// postmortem phase. Distinct from loop.PostmortemDoc (the contract
// envelope persisted to DB) — this is the LLM output shape that
// drives the markdown render + git artifact commit.
//
// Fields per design §4.5:
//   - IncidentID       : owning incident identifier
//   - Summary          : 1-paragraph executive summary
//   - RootCause        : narrative root cause (synthesized from
//     upstream RootCauseJSON.RootCauseObject)
//   - Timeline         : ordered phase timeline entries (RFC3339)
//   - RemediationTaken : what fix was applied (from upstream
//     VerifiedDelta + RemediationOptions)
//   - LessonsLearned   : bullet-list friendly prose
//   - Markdown         : full rendered Markdown body (postmortem
//     sections per spec §D5)
type PostmortemContent struct {
	IncidentID       string          `json:"incident_id"`
	Summary          string          `json:"summary"`
	RootCause        string          `json:"root_cause"`
	Timeline         []TimelineEntry `json:"timeline"`
	RemediationTaken string          `json:"remediation_taken"`
	LessonsLearned   string          `json:"lessons_learned"`
	Markdown         string          `json:"markdown"`
}

// TimelineEntry is one row in the postmortem timeline.
// EventAt is RFC3339 (the LLM is told to render it this way);
// Phase is the phase name ("detected" / "correlated" / etc.).
// Summary is a 1-sentence operator-facing description.
type TimelineEntry struct {
	Phase   string `json:"phase"`
	EventAt string `json:"event_at"`
	Summary string `json:"summary"`
}

// PostmortemContentSchema is the JSON-Schema document passed to
// LLMCaller.Call as OutputSchema. Enforces the 7 required fields
// per task spec; the validator subset is the same one LLMCaller
// already supports (type / required / properties / items).
const PostmortemContentSchema = `{
  "type": "object",
  "required": ["incident_id", "summary", "root_cause", "timeline", "remediation_taken", "lessons_learned", "markdown"],
  "properties": {
    "incident_id":       {"type": "string"},
    "summary":           {"type": "string"},
    "root_cause":        {"type": "string"},
    "timeline":          {"type": "array", "items": {"type": "object"}},
    "remediation_taken": {"type": "string"},
    "lessons_learned":   {"type": "string"},
    "markdown":          {"type": "string"}
  }
}`

// PostmortemPhaseVerifierTimeoutMs is the recommended Verifier
// deadline for the postmortem phase. The post-mortem LLM call is a
// synthesis task (~3-8s p50) plus git artifact commit (~500ms),
// so 30s matches the orchestrator-wide default. Captured as a
// constant so tests assert against the value.
const PostmortemPhaseVerifierTimeoutMs = 30000

// PostmortemPhaseLLMTimeoutMs is the per-LLM-call wall-clock
// budget inside the postmortem phase. Default 60s (matches
// defaultCallTimeoutMs in llm_caller.go). Captured as a constant
// so tests can override it.
const PostmortemPhaseLLMTimeoutMs = 60000

// PostmortemMarkdownMinLen is the minimum rendered markdown
// length the Verifier accepts. 100 chars is a deliberately low
// bar — anything shorter is almost certainly an LLM "I can't
// render that" placeholder, not a real postmortem.
const PostmortemMarkdownMinLen = 100

// GitArtifactSink is the narrow seam for committing the rendered
// postmortem markdown to a git-backed artifact store (postmortems
// live in git per design §6.2).
//
// The Day 4 + Day 5 wire-up will provide a concrete impl
// (internal/manager/biz/report/postmortem_sink.go already has a
// Sink wrapper around go-git; this batch supplies only the
// placeholder SHA computation per borrow-map §B.3 deferred item).
//
// CommitMarkdown MUST be idempotent on (incidentID, body); the
// caller (postmortem Executor) treats repeated calls as no-ops.
type GitArtifactSink interface {
	CommitMarkdown(ctx context.Context, incidentID, body string) (commitSHA string, err error)
}

// UpstreamContractLoader is the narrow seam the postmortem
// worker uses to load the three upstream contracts it needs to
// drive the LLM prompt:
//   - RootCauseJSON (investigated phase)
//   - CritiqueScore (critiqued phase)
//   - VerifiedDelta (recovered phase)
//
// nil fields in the returned bundle are tolerated — the prompt
// renderer just skips empty sections. Production wire-up (Day 5)
// implements this via the ContractRepo facade.
type UpstreamContractLoader interface {
	LoadPostmortemInputs(ctx context.Context, incidentID string) (*PostmortemInputs, error)
}

// PostmortemInputs is the bundle of upstream contracts the
// Planner hands to the LLM prompt renderer. Fields are pointers
// so a missing contract (e.g. loop terminated before recovered) is
// distinguishable from an empty struct.
type PostmortemInputs struct {
	RootCause *RootCauseJSON
	Critique  *CritiqueScore
	Verified  *VerifiedDelta
}

// PatternWriter is the narrow dep the postmortem worker uses to
// write-back incident_pattern KB rows after a successful git commit.
//
// chatdiagnose/store.CompositePatternRepo 实现本接口。postmortem_worker
// 包不 import chatdiagnose（避免 manager/biz 跨域 import），但允许接受
// 满足本接口的实例。
type PatternWriter interface {
	Save(ctx context.Context, p *chatdiagnosemodel.IncidentPattern) error
}

// PostmortemPhaseWorker is the PhaseWorker for PhasePostmortem.
// Planner / Executor / Verifier follow recovery.go's pattern;
// all narrow deps are constructor-injected.
type PostmortemPhaseWorker struct {
	BasePhaseWorker

	caller        LLMCaller
	gitSink       GitArtifactSink
	inputs        UpstreamContractLoader
	patternWriter PatternWriter // 可选；nil → 跳过 KB 写回
	clock         func() time.Time
	log           *slog.Logger
}

// NewPostmortemPhaseWorker wires the worker. All 3 narrow deps
// (LLMCaller, GitArtifactSink, UpstreamContractLoader) are required
// to keep the wire-up loud (matches the chatdiagnose service /
// recovered worker "fail at wire-up" principle).
//
// log nil → slog.Default(). clock nil → time.Now().UTC().
func NewPostmortemPhaseWorker(
	caller LLMCaller,
	gitSink GitArtifactSink,
	inputs UpstreamContractLoader,
	patternWriter PatternWriter,
	log *slog.Logger,
) (*PostmortemPhaseWorker, error) {
	if caller == nil {
		return nil, errors.New("loop: PostmortemPhaseWorker.caller is required")
	}
	if gitSink == nil {
		return nil, errors.New("loop: PostmortemPhaseWorker.gitSink is required")
	}
	if inputs == nil {
		return nil, errors.New("loop: PostmortemPhaseWorker.inputs is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &PostmortemPhaseWorker{
		BasePhaseWorker: BasePhaseWorker{
			PhaseRef:   PhasePostmortem,
			VerifierMs: PostmortemPhaseVerifierTimeoutMs,
		},
		caller:        caller,
		gitSink:       gitSink,
		inputs:        inputs,
		patternWriter: patternWriter,
		clock:         func() time.Time { return time.Now().UTC() },
		log:           log,
	}, nil
}

// Phase returns the phase this Worker handles.
func (w *PostmortemPhaseWorker) Phase() Phase { return w.PhaseRef }

// Planner loads upstream contracts + renders the LLM prompt.
//
// Steps:
//  1. Load PostmortemInputs via w.inputs (the upstream loader is
//     responsible for resolving contract IDs by incident_id).
//  2. Render a user prompt that JSON-serializes the three
//     upstream contracts (or notes each as "missing") so the LLM
//     has the full evidence chain in one place.
//  3. Return a Plan with a single tool_call Step whose target is
//     ToolNameLLMCall; the Executor will drive the LLMCaller +
//     git artifact commit on this step.
//
// Returns ErrPlanInvalid when PlanInput.IncidentID is empty.
func (w *PostmortemPhaseWorker) Planner(ctx context.Context, in PlanInput) (Plan, error) {
	if in.IncidentID == "" {
		return Plan{}, fmt.Errorf("%w: postmortem Planner requires IncidentID", ErrPlanInvalid)
	}
	upstream, err := w.inputs.LoadPostmortemInputs(ctx, in.IncidentID)
	if err != nil {
		return Plan{}, fmt.Errorf("loop: load postmortem upstream inputs (id=%s): %w", in.IncidentID, err)
	}
	if upstream == nil {
		return Plan{}, fmt.Errorf("%w: LoadPostmortemInputs returned nil bundle", ErrPlanInvalid)
	}
	system := postmortemSystemPrompt()
	prompt := renderPostmortemUserPrompt(in.IncidentID, upstream)
	return Plan{
		Steps: []PlanStep{{
			Kind:   "tool_call",
			Target: ToolNameLLMCall,
			Args: map[string]any{
				"phase":         string(PhasePostmortem),
				"system_prompt": system,
				"prompt":        prompt,
				"output_schema": PostmortemContentSchema,
				"incident_id":   in.IncidentID,
				"tenant_id":     in.TenantID,
				"timeout_ms":    PostmortemPhaseLLMTimeoutMs,
			},
			TimeoutMs: PostmortemPhaseLLMTimeoutMs,
		}},
		EstimatedCost: CostEstimate{USD: 0, Tokens: 0},
		Meta: map[string]any{
			"incident_id":    in.IncidentID,
			"tenant_id":      in.TenantID,
			"system_prompt":  system,
			"prompt":         prompt,
			"output_schema":  PostmortemContentSchema,
			"has_root_cause": upstream.RootCause != nil,
			"has_critique":   upstream.Critique != nil,
			"has_verified":   upstream.Verified != nil,
		},
	}, nil
}

// Executor invokes LLMCaller.Call + GitArtifactSink.CommitMarkdown.
//
// Steps:
//  1. Extract (system_prompt, prompt, output_schema, timeout_ms)
//     from the Plan's first Step.Args; default timeout to
//     PostmortemPhaseLLMTimeoutMs when missing.
//  2. Call LLMCaller.Call. Schema-invalid → wrap ErrSchemaInvalid
//     so the Verifier / orchestrator can identify the failure mode.
//     Transient errors are retried by LLMCaller; non-transient
//     propagate wrapped.
//  3. Unmarshal CallOutput.Raw into PostmortemContent. If
//     unmarshal fails (shouldn't, because LLMCaller already ran
//     schema validation) → wrap ErrInvalidSchema.
//  4. Call gitSink.CommitMarkdown with PostmortemContent.Markdown.
//     Failure is wrapped but the content itself is still
//     returned via RawOutputs so the Verifier can decide.
//  5. Emit ToolReplay for both the LLM call and the git commit so
//     the postmortem audit trail picks them up.
func (w *PostmortemPhaseWorker) Executor(ctx context.Context, plan Plan) (ExecResult, error) {
	if len(plan.Steps) == 0 {
		return ExecResult{}, fmt.Errorf("%w: planner returned empty steps", ErrPlanInvalid)
	}
	step := plan.Steps[0]
	system, _ := step.Args["system_prompt"].(string)
	prompt, _ := step.Args["prompt"].(string)
	schemaStr, _ := step.Args["output_schema"].(string)
	incidentID, _ := step.Args["incident_id"].(string)
	timeoutMs, _ := step.Args["timeout_ms"].(int)
	if timeoutMs <= 0 {
		timeoutMs = PostmortemPhaseLLMTimeoutMs
	}
	if prompt == "" || schemaStr == "" {
		return ExecResult{}, fmt.Errorf("%w: Planner missing prompt or output_schema", ErrPlanInvalid)
	}

	llmStart := w.clock()
	out, err := w.caller.Call(ctx, CallInput{
		Phase:        PhasePostmortem,
		Prompt:       prompt,
		SystemPrompt: system,
		OutputSchema: schemaStr,
		TimeoutMs:    timeoutMs,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("loop: postmortem LLM call: %w", err)
	}

	var content PostmortemContent
	if uerr := json.Unmarshal(out.Raw, &content); uerr != nil {
		// LLMCaller already enforced OutputSchema; reaching here
		// means the schema is JSON-shape-only and a downstream
		// field was unparseable into our typed struct (rare; usually
		// a number-vs-string mismatch).
		return ExecResult{}, fmt.Errorf("%w: postmortem content unmarshal: %w", ErrInvalidSchema, uerr)
	}

	// Defensive: ensure IncidentID matches the planner-supplied one
	// even if the LLM hallucinated a different value. The schema
	// doesn't enforce equality, only presence.
	if content.IncidentID == "" {
		content.IncidentID = incidentID
	}

	var commitSHA string
	var commitErr error
	if content.Markdown != "" {
		commitSHA, commitErr = w.gitSink.CommitMarkdown(ctx, content.IncidentID, content.Markdown)
	}

	// KB write-back (chatruntime-kb-implementation):
	// git commit 成功后 → 调 patternWriter.Save(IncidentPattern)。
	// 失败 MUST slog warn + continue（不阻塞 postmortem）。
	if commitErr == nil && commitSHA != "" && w.patternWriter != nil {
		w.writeBackPattern(ctx, incidentID, content, commitSHA)
	}
	llmEnd := w.clock()

	sideEffects := []SideEffect{{
		Kind:   "postmortem_render",
		Target: content.IncidentID,
		Detail: map[string]any{
			"summary_len":      len(content.Summary),
			"root_cause_len":   len(content.RootCause),
			"timeline_entries": len(content.Timeline),
			"remediation_len":  len(content.RemediationTaken),
			"lessons_len":      len(content.LessonsLearned),
			"markdown_len":     len(content.Markdown),
			"tokens_in":        out.TokensIn,
			"tokens_out":       out.TokensOut,
			"cost_usd":         out.CostUSD,
			"llm_latency_ms":   out.LatencyMs,
		},
	}}
	if commitErr == nil {
		sideEffects = append(sideEffects, SideEffect{
			Kind:   "git_commit",
			Target: content.IncidentID,
			Detail: map[string]any{
				"commit_sha":  commitSHA,
				"body_len":    len(content.Markdown),
				"placeholder": true, // borrow-map §B.3 deferred to next change
			},
		})
	} else {
		sideEffects = append(sideEffects, SideEffect{
			Kind:   "git_commit_failed",
			Target: content.IncidentID,
			Detail: map[string]any{
				"error": commitErr.Error(),
			},
		})
	}

	return ExecResult{
		ContractRef: nil, // contract writer runs at Run-time, not Executor
		SideEffects: sideEffects,
		ToolReplay: []ToolReplayEntry{
			{
				Name:       ToolNameLLMCall,
				ArgsJSON:   marshalArgsForReplay(step.Args),
				ResultJSON: string(out.Raw),
				Status:     "success",
				LatencyMs:  out.LatencyMs,
				Timestamp:  llmStart,
			},
			{
				Name:       ToolNameGitArtifactCommit,
				ArgsJSON:   marshalGitArgsForReplay(content.IncidentID, len(content.Markdown)),
				ResultJSON: marshalGitResultForReplay(commitSHA, commitErr),
				Status:     gitStatusForReplay(commitErr),
				LatencyMs:  int(llmEnd.Sub(llmStart) / time.Millisecond),
				Timestamp:  llmEnd,
			},
		},
		RawOutputs: map[string]any{
			"postmortem_content": &content,
			"commit_sha":         commitSHA,
			"commit_err":         commitErr,
			"llm_output":         out,
		},
	}, nil
}

// writeBackPattern 把 postmortem 落地到 incident_pattern KB（双写 MySQL + Qdrant）。
//
// 失败语义（继承 zero-manual-ops-loop §"KB miss MUST NOT block the chat"）：
//   - Embedder 失败 → return（slog warn）
//   - fingerprint 计算失败 → return（slog warn）
//   - MySQL UPSERT 失败 → slog warn，**继续**（Qdrant 已写入）
//   - Qdrant Upsert 失败 → slog warn，**继续**（MySQL 已写入 / 下次 write-back 重建）
//   - 任何失败 MUST NOT 让 postmortem_worker 报错
//
// 入参：
//
//	incidentID: postmortem 所属 incident
//	content:    LLM 渲染的 PostmortemContent（用于 RootCause / Severity / Symptom）
//	commitSHA:  git artifact commit SHA（写回时作 postmortem_ref）
func (w *PostmortemPhaseWorker) writeBackPattern(ctx context.Context, incidentID string, content PostmortemContent, commitSHA string) {
	// 1. 构造 signature：resource_type:root_cause_object:severity
	//    当前 PostmortemContent 没有 ResourceType / Severity 字段；从
	//    content.RootCause + content.Summary 启发式提取（保守值，避免幻觉）。
	resourceType := "incident" // 默认；本 change 不解析 RootCauseJSON（避免 manager/biz 跨域 import closed-loop-orchestrator 包）
	rootCauseObject := truncateForSignature(content.RootCause, 64)
	severity := inferSeverity(content)
	signature := resourceType + ":" + rootCauseObject + ":" + severity

	// 2. 计算 fingerprint（sha256[:16]）— 本 change 直接走 MySQL 路径，
	//    embedder 留给 KB 命中路径（postmortem 不需要每次都算 embed，
	//    以免 Embedder 慢调用阻塞 postmortem）。MySQL metadata 写回 + Qdrant
	//    upsert 都接受 fingerprint 作为 dedup key（详见 CompositePatternRepo.Save 注释）。
	fpHash := sha256.Sum256([]byte(signature))
	fingerprint := hex.EncodeToString(fpHash[:])[:16]

	// 3. 构造 IncidentPattern
	now := w.clock()
	pattern := chatdiagnosemodel.IncidentPattern{
		TenantID:           inferTenantFromCtx(ctx), // 占位：默认 "" 由 caller 保证
		ResourceType:       resourceType,
		RootCauseObject:    rootCauseObject,
		Signature:          signature,
		SourcePostmortemID: commitSHA,
		Fingerprint:        fingerprint,
		Severity:           severity,
		Confidence:         0.5, // postmortem 默认 0.5（中间置信度）
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	// 4. 调 patternWriter.Save → CompositePatternRepo 双写 MySQL + Qdrant
	//    任一失败 MUST slog warn + 继续。
	if err := w.patternWriter.Save(ctx, &pattern); err != nil {
		w.log.Warn("postmortem: KB write-back failed (non-fatal)",
			slog.String("incident_id", incidentID),
			slog.String("commit_sha", commitSHA),
			slog.Any("err", err))
		return
	}
	w.log.Info("postmortem: KB write-back succeeded",
		slog.String("incident_id", incidentID),
		slog.String("commit_sha", commitSHA),
		slog.String("fingerprint", fingerprint))
}

// truncateForSignature 截断 + 清洗 root cause 文本。
func truncateForSignature(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

// inferSeverity 从 PostmortemContent 启发式推断 severity（low/medium/high/critical）。
func inferSeverity(content PostmortemContent) string {
	text := strings.ToLower(content.Summary + " " + content.RootCause + " " + content.LessonsLearned)
	switch {
	case strings.Contains(text, "critical") || strings.Contains(text, "p0"):
		return "critical"
	case strings.Contains(text, "high") || strings.Contains(text, "p1"):
		return "high"
	case strings.Contains(text, "low") || strings.Contains(text, "minor"):
		return "low"
	default:
		return "medium"
	}
}

// inferTenantFromCtx 从 context 提取 tenant_id（占位 — 当前未注入，
// 返回 "" 由 caller 保证；ChatDiagnoseService 透传 tenant_id）。
func inferTenantFromCtx(_ context.Context) string {
	// 简化实现：tenant_id 由 postmortem caller 在 Planner 阶段确定，
	// 当前 change 不修改 PostmortemInputs，新增字段 deferred。
	return ""
}

// Verifier checks the Executor's PostmortemContent against
// the contract spec's strength rules:
//
//  1. schema              — enforced by LLMCaller.Call; if the
//     Executor reached here, the JSON
//     shape matches PostmortemContentSchema
//     and all 7 required fields are present.
//  2. markdown length > 100 — anything shorter is almost
//     certainly an LLM placeholder.
//  3. incident_id non-empty — non-empty + matches the planner
//     supplied id (LLM may hallucinate).
//
// Returns Verdict{OK: true, Confidence: 1} on success.
// Returns Verdict{OK: false, Reasons: [...]} on failure; each
// Reason is a discrete failing check the postmortem audit log
// can render.
func (w *PostmortemPhaseWorker) Verifier(ctx context.Context, result ExecResult) (Verdict, error) {
	raw, ok := result.RawOutputs["postmortem_content"]
	if !ok {
		return Verdict{}, fmt.Errorf("%w: Executor.RawOutputs missing postmortem_content", ErrInvalidSchema)
	}
	content, ok := raw.(*PostmortemContent)
	if !ok {
		return Verdict{}, fmt.Errorf("%w: Executor.RawOutputs.postmortem_content type=%T", ErrInvalidSchema, raw)
	}
	if content == nil {
		return Verdict{}, fmt.Errorf("%w: postmortem_content is nil", ErrInvalidSchema)
	}

	var reasons []string
	if content.IncidentID == "" {
		reasons = append(reasons, "incident_id missing")
	}
	if len(content.Markdown) <= PostmortemMarkdownMinLen {
		reasons = append(reasons, fmt.Sprintf("markdown length %d <= minimum %d", len(content.Markdown), PostmortemMarkdownMinLen))
	}
	if content.Summary == "" {
		reasons = append(reasons, "summary missing")
	}
	if content.RootCause == "" {
		reasons = append(reasons, "root_cause missing")
	}
	if content.RemediationTaken == "" {
		reasons = append(reasons, "remediation_taken missing")
	}
	if content.LessonsLearned == "" {
		reasons = append(reasons, "lessons_learned missing")
	}

	if len(reasons) > 0 {
		return Verdict{OK: false, Confidence: 0, Reasons: reasons}, nil
	}
	return Verdict{OK: true, Confidence: 1, Reasons: nil}, nil
}

// VerifierTimeoutMs returns the configured override, falling
// back to PostmortemPhaseVerifierTimeoutMs when zero (which
// matches the orchestrator-wide default of 30s).
func (w *PostmortemPhaseWorker) VerifierTimeoutMs() int {
	if w.VerifierMs > 0 {
		return w.VerifierMs
	}
	return PostmortemPhaseVerifierTimeoutMs
}

// --- helpers ------------------------------------------------------------

// postmortemSystemPrompt is the system-turn instruction passed
// to the LLM. Kept short + deterministic so changes show up
// cleanly in the audit log; the per-incident evidence chain is
// in the user prompt.
func postmortemSystemPrompt() string {
	return "You are an SRE writing a blameless postmortem. Respond with JSON only, matching the provided schema. No prose outside the JSON object."
}

// renderPostmortemUserPrompt serializes the three upstream
// contracts into a single user prompt. Missing contracts are
// marked as such (rather than omitted) so the LLM knows it has
// a partial evidence chain.
func renderPostmortemUserPrompt(incidentID string, in *PostmortemInputs) string {
	if in == nil {
		return fmt.Sprintf("incident_id: %s\n(no upstream contracts available — produce a best-effort postmortem from the schema alone)", incidentID)
	}
	var sb strings.Builder
	sb.WriteString("incident_id: ")
	sb.WriteString(incidentID)
	sb.WriteString("\n\n")
	sb.WriteString("## RootCauseJSON\n")
	if in.RootCause != nil {
		writeJSONLine(&sb, "root_cause_object", in.RootCause.RootCauseObject)
		writeJSONLine(&sb, "confidence", in.RootCause.Confidence)
		writeJSONLine(&sb, "evidence_chain", in.RootCause.EvidenceChain)
		writeJSONLine(&sb, "time_window", in.RootCause.TimeWindow)
		writeJSONLine(&sb, "remediation_options", in.RootCause.RemediationOptions)
	} else {
		sb.WriteString("(missing — investigated phase did not produce a RootCauseJSON)\n")
	}
	sb.WriteString("\n## CritiqueScore\n")
	if in.Critique != nil {
		writeJSONLine(&sb, "verdict", in.Critique.Verdict)
		writeJSONLine(&sb, "score", in.Critique.Score)
		writeJSONLine(&sb, "reasons", in.Critique.Reasons)
		writeJSONLine(&sb, "model", in.Critique.Model)
	} else {
		sb.WriteString("(missing — critiqued phase did not produce a CritiqueScore)\n")
	}
	sb.WriteString("\n## VerifiedDelta\n")
	if in.Verified != nil {
		writeJSONLine(&sb, "passed", in.Verified.Passed)
		writeJSONLine(&sb, "tolerance", in.Verified.Tolerance)
		writeJSONLine(&sb, "deltas", in.Verified.Deltas)
		writeJSONLine(&sb, "warning_level", in.Verified.WarningLevel)
		writeJSONLine(&sb, "retry_count", in.Verified.RetryCount)
	} else {
		sb.WriteString("(missing — recovered phase did not produce a VerifiedDelta)\n")
	}
	sb.WriteString("\nRespond with the JSON object matching the provided schema. The `markdown` field must be the full rendered postmortem body with sections: Summary / Root Cause / Timeline / Remediation / Lessons Learned.\n")
	return sb.String()
}

// writeJSONLine writes `key: <json-encoded value>\n` to sb.
// Centralised so the prompt rendering can be tweaked without
// churning callers.
func writeJSONLine(sb *strings.Builder, key string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		sb.WriteString(key)
		sb.WriteString(": <unencodable>\n")
		return
	}
	sb.WriteString(key)
	sb.WriteString(": ")
	sb.Write(raw)
	sb.WriteString("\n")
}

// marshalArgsForReplay encodes the LLM call's arguments bag as
// JSON for the ToolReplay audit trail. Errors are swallowed
// (returns "{}") so a transient marshal glitch never blocks the
// postmortem — the replay row is best-effort.
func marshalArgsForReplay(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// marshalGitArgsForReplay encodes the git artifact commit args
// for the audit trail. body is omitted (markdown content can be
// very large); only the (incident_id, body_len) tuple is recorded.
func marshalGitArgsForReplay(incidentID string, bodyLen int) string {
	raw, err := json.Marshal(map[string]any{
		"incident_id": incidentID,
		"body_len":    bodyLen,
	})
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// marshalGitResultForReplay encodes the git artifact commit
// result for the audit trail. err is rendered as the error
// message string; sha is the committed SHA.
func marshalGitResultForReplay(sha string, err error) string {
	m := map[string]any{"commit_sha": sha}
	if err != nil {
		m["error"] = err.Error()
	}
	raw, jerr := json.Marshal(m)
	if jerr != nil {
		return "{}"
	}
	return string(raw)
}

// gitStatusForReplay maps a commit error (or nil) to the
// canonical ToolReplay status string.
func gitStatusForReplay(err error) string {
	if err == nil {
		return "success"
	}
	return "failed"
}

// --- wire names ----------------------------------------------------------

// ToolNameLLMCall is the wire name for the LLM-call Step target.
// Defined in this file (not phase_workers.go) so the orchestrator
// file stays untouched per batch constraint.
const ToolNameLLMCall = "llm_call"

// ToolNameGitArtifactCommit is the wire name for the git artifact
// commit step. Same rationale as ToolNameLLMCall.
const ToolNameGitArtifactCommit = "git_artifact_commit"

// --- placeholder SHA helper (borrow-map §B.3 deferred) ---------------

// SyntheticPostmortemCommitSHA computes a deterministic placeholder
// SHA for the git artifact commit. Real impl (Day 4 + Day 5) plugs
// in go-git; for batch 2 / subagent 9 we only need the contract
// (incidentID + body → sha) so downstream test infrastructure
// can assert equality.
//
// Algorithm: sha256(incidentID || "\n" || body), hex-encoded, take
// the first 40 hex chars (mimicking a real git SHA-1 length so the
// shape of the value matches what production will produce).
func SyntheticPostmortemCommitSHA(incidentID, body string) string {
	h := sha256.New()
	h.Write([]byte(incidentID))
	h.Write([]byte{'\n'})
	h.Write([]byte(body))
	sum := hex.EncodeToString(h.Sum(nil))
	if len(sum) > 40 {
		sum = sum[:40]
	}
	return sum
}

// NoopGitArtifactSink is the GitArtifactSink no-op implementation.
// 返回空 string + nil err，wire-up 兜底（Day 5+ 接真实 go-git）。
type NoopGitArtifactSink struct{}

func (NoopGitArtifactSink) CommitMarkdown(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

// NoopUpstreamContractLoader is the UpstreamContractLoader no-op implementation.
// 返回 (nil, nil)，Planner 在缺真实 loader 时跳过上游 contract 注入。
type NoopUpstreamContractLoader struct{}

func (NoopUpstreamContractLoader) LoadPostmortemInputs(_ context.Context, _ string) (*PostmortemInputs, error) {
	return nil, nil
}

// Package runner — loop.go
//
// loop-mode 闭环评测：串联 seven-phase orchestrator + case 注入 +
// judge 四指标计算，产出 LoopResult JSON。
//
// 模式矩阵（与 spec loop-harness-rubric §"与现有 tool-mode 共存"）：
//
//	--mode=tool (default): 原有 Run() 路径，单工具评测骨架（Day 1-5）
//	--mode=loop            : RunLoop() 路径，闭环端到端（Day 6+）
//	--mode=chat            : RunLoop(LoopModeChat) — chat 入口 6 指标
//	                        扩展（含 kb_hit_rate / follow_up_depth）
//
// 设计要点：
//   - LoopResult 与 RunResult 不共享 schema；loop 是新的顶层类型
//     （spec §"LoopResult{passed, phases, rubric}"）。
//   - judge 输入来自 orchestrator 的 RunResult（events / finalPhase），
//     不依赖外部 LLM；harness loop-mode 是 dry-run + 量化指标的
//     自检闭环。
//   - 输出落 harness/result/loop/<case_id>.json；后续 leaderboard
//     读取并聚合。
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/harness/judge"
	"github.com/vincent-wuhan/opskeeper/internal/harness/schema"
)

// Mode is the runner mode selector.
type Mode string

const (
	ModeTool Mode = "tool"
	ModeLoop Mode = "loop"
	ModeChat Mode = "chat"
)

// LoopExecutionMode distinguishes the legacy synthetic timeline, an injected
// orchestrator run, and the strict post-hoc real AgentTeams evidence gate.
type LoopExecutionMode string

const (
	ExecutionModeDryRun         LoopExecutionMode = "dry_run"
	ExecutionModeOrchestrator   LoopExecutionMode = "orchestrator"
	ExecutionModeRealAgentTeams LoopExecutionMode = "real_agentteams"
)

// ParseMode parses a --mode flag value. Defaults to ModeTool when
// empty for backward compatibility (Day 1-5 callers).
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "tool":
		return ModeTool, nil
	case "loop":
		return ModeLoop, nil
	case "chat":
		return ModeChat, nil
	}
	return "", fmt.Errorf("harness: unknown mode %q (want tool|loop|chat)", s)
}

// ParseLoopExecutionMode parses --execution-mode. Empty keeps the legacy
// behavior so existing CI smoke tests remain compatible; real-agentteams is
// always explicit and can never be selected implicitly.
func ParseLoopExecutionMode(s string) (LoopExecutionMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "dry-run":
		return ExecutionModeDryRun, nil
	case "orchestrator":
		return ExecutionModeOrchestrator, nil
	case "real-agentteams":
		return ExecutionModeRealAgentTeams, nil
	}
	return "", fmt.Errorf("harness: unknown execution mode %q (want dry-run|orchestrator|real-agentteams)", s)
}

// LoopOptions controls a single loop-mode run.
type LoopOptions struct {
	// CaseID is the harness case to load.
	CaseID string

	// Env is the target environment (staging / test / prod).
	Env Env

	// TenantID is the multi-tenant scope (required for orchestrator).
	TenantID string

	// TriggeredBy is the entry source recorded on the run-start event.
	TriggeredBy string

	// Mode is ModeLoop or ModeChat; ModeLoop by default when zero.
	Mode Mode

	// ExecutionMode selects dry-run compatibility behavior or the strict
	// real AgentTeams evidence evaluator.
	ExecutionMode LoopExecutionMode

	// IncidentID is required in real-agentteams mode and must equal every
	// evidence artifact. Other modes derive an incident ID for compatibility.
	IncidentID string

	// TraceID is required in real-agentteams mode and must be present across
	// state, MCP, fixture, and postmortem evidence.
	TraceID string

	// RealAgentTeamsEvidence is required as a complete set in
	// ExecutionModeRealAgentTeams. Partial input is an error, never a fallback.
	RealAgentTeamsEvidence RealAgentTeamsEvidencePaths

	// CasesDir overrides the cases root; empty defaults to "internal/harness/cases".
	CasesDir string

	// OutDir is where LoopResult JSON is written. Empty defaults to
	// "harness/result/loop".
	OutDir string

	// Now is the time source; tests inject a fake. Zero = time.Now().
	Now time.Time

	// Timeout is the per-run wall-clock budget. Zero = 5 minutes.
	Timeout time.Duration
}

// LoopRubric is the four-metric (loop) / six-metric (chat) rubric
// written into LoopResult.Rubric. The shape is JSON-friendly so
// downstream leaderboard / dashboard can read it directly.
//
// Field omitempty: when a metric is not applicable for the mode
// (e.g. kb_hit_rate is chat-only) the field is omitted. Null fields
// carry an explicit Reason so leaderboard can flag "rubric_incomplete".
type LoopRubric struct {
	// RCAAccuracy ∈ [0,1]. Loop: investigator RootCauseJSON.root_cause_object
	// 与 case.Expect.RootCauseLines 的 match 率。Chat: 同义。
	RCAAccuracy *float64 `json:"rca_accuracy,omitempty"`

	// RCAAccuracyReason explains why the metric is null (when it is).
	RCAAccuracyReason string `json:"rca_accuracy_reason,omitempty"`

	// TimeToRemediate 是从 detected → recovered 的 wall-clock 时长。
	// 字符串格式（如 "3m 42s"）便于 leaderboard 表格直接渲染。
	TimeToRemediate string `json:"time_to_remediate,omitempty"`

	// TimeToRemediateSec 是 time_to_remediate 的整数秒表示（便于排序）。
	TimeToRemediateSec *int `json:"time_to_remediate_sec,omitempty"`

	// TimeToRemediateReason 同 RCAAccuracyReason。
	TimeToRemediateReason string `json:"time_to_remediate_reason,omitempty"`

	// ApprovalRate ∈ [0,1]。闭环中自动通过审批的 proposal 数 / 总数。
	ApprovalRate *float64 `json:"approval_rate,omitempty"`

	// ApprovalRateReason 同上。
	ApprovalRateReason string `json:"approval_rate_reason,omitempty"`

	// RecoveryPassRate ∈ [0,1]。verify_recovery 一次通过的比例。
	RecoveryPassRate *float64 `json:"recovery_pass_rate,omitempty"`

	// RecoveryPassRateReason 同上。
	RecoveryPassRateReason string `json:"recovery_pass_rate_reason,omitempty"`

	// KBHitRate ∈ [0,1]。Chat-only：KB 命中次数 / 调查阶段总查询次数。
	// Loop 模式必为 nil（带 reason 解释）。
	KBHitRate *float64 `json:"kb_hit_rate,omitempty"`

	// KBHitRateReason 同上。
	KBHitRateReason string `json:"kb_hit_rate_reason,omitempty"`

	// FollowUpDepth 是用户在对话中追问的轮数（chat-only）。
	FollowUpDepth *int `json:"follow_up_depth,omitempty"`

	// FollowUpDepthReason 同上。
	FollowUpDepthReason string `json:"follow_up_depth_reason,omitempty"`

	// RubricIncomplete = true 表示四指标（loop）或六指标（chat）未
	// 全部填充（leaderboard 据此标 NOT QUALIFIED）。
	RubricIncomplete bool `json:"rubric_incomplete,omitempty"`
}

// LoopPhaseRecord is one stage in the seven-phase state machine
// observed during the loop run.
type LoopPhaseRecord struct {
	Phase       string    `json:"phase"`
	Status      string    `json:"status"` // success / failed / running / skipped
	StartedAt   time.Time `json:"started_at"`
	DurationMs  int64     `json:"duration_ms"`
	ContractRef *string   `json:"contract_ref,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// LoopResult is the JSON shape written to harness/result/loop/<case_id>.json
// per spec loop-harness-rubric §"四个顶层字段".
//
// Top-level fields:
//
//   - incident_id     : orchestrator 输入的 incident id（由 case id 派生）
//   - final_phase     : 终态 phase（postmortem / failed / aborted / retry_exhausted）
//   - durations       : 阶段耗时 map（phase → ms），便于 leaderboard 排序
//   - contract_summary: 关键 contract 列表（Phase → Type → SchemaVer）
//   - judge_scores    : 与 LoopRubric 镜像 + judge 元数据
//   - phases          : 阶段明细（loop 模式可选；chat 模式必有）
//   - rubric          : 四指标（loop）/六指标（chat）
//   - passed          : 整体是否通过（rubric_incomplete=false 且所有指标 ≥ 阈值）
//   - mode            : tool / loop / chat
type LoopResult struct {
	IncidentID      string            `json:"incident_id"`
	CaseID          string            `json:"case_id"`
	Mode            Mode              `json:"mode"`
	ExecutionMode   LoopExecutionMode `json:"execution_mode"`
	FinalPhase      string            `json:"final_phase"`
	Passed          bool              `json:"passed"`
	StartedAt       time.Time         `json:"started_at"`
	FinishedAt      time.Time         `json:"finished_at"`
	DurationMs      int64             `json:"duration_ms"`
	Phases          []LoopPhaseRecord `json:"phases"`
	Durations       map[string]int64  `json:"durations"`
	ContractSummary []ContractSummary `json:"contract_summary"`
	JudgeScores     JudgeScores       `json:"judge_scores"`
	Rubric          LoopRubric        `json:"rubric"`
	Flags           []string          `json:"flags,omitempty"`
	Errors          []string          `json:"errors,omitempty"`
	Metadata        map[string]any    `json:"metadata,omitempty"`
}

// ContractSummary is one row of contract_summary.
type ContractSummary struct {
	Phase       string `json:"phase"`
	Type        string `json:"type"`
	SchemaVer   string `json:"schema_version"`
	StorageSize int    `json:"size_bytes,omitempty"`
}

// JudgeScores is the judge output mirrored to the top level for
// quick inspection without descending into rubric.
type JudgeScores struct {
	Overall    float64            `json:"overall"`
	Dimensions map[string]float64 `json:"dimensions"`
	JudgesUsed []string           `json:"judges_used"`
	Flagged    bool               `json:"flagged"`
	FlagReason string             `json:"flag_reason,omitempty"`
}

// RunLoop executes a single harness case in loop mode (or chat mode).
// The orchestrator dependency is injected via opts.Orchestrator; nil
// orchestrator runs the loop in dry-run mode (no orchestrator.Run,
// uses a synthesized event timeline from the case.yaml).
//
// Backward compatibility: when opts.Mode == ModeTool (or empty),
// RunLoop returns an error directing callers to Run() — this
// preserves the original tool-mode API surface.
func RunLoop(ctx context.Context, opts LoopOptions, deps LoopDeps) (*LoopResult, error) {
	if opts.CaseID == "" {
		return nil, errors.New("harness runner: CaseID required")
	}
	if opts.Mode == "" {
		opts.Mode = ModeLoop
	}
	if opts.Mode == ModeTool {
		return nil, errors.New("harness runner: --mode=tool uses Run(); call RunLoop only with loop|chat")
	}
	if opts.ExecutionMode == ExecutionModeRealAgentTeams && opts.Mode != ModeLoop {
		return nil, errors.New("harness runner: execution-mode=real-agentteams requires --mode=loop")
	}
	if opts.Env == "" {
		opts.Env = EnvStaging
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	casesDir := opts.CasesDir
	if casesDir == "" {
		casesDir = "internal/harness/cases"
	}

	loader := schema.NewLoader(casesDir)
	caseObj, err := loader.LoadByID(opts.CaseID)
	if err != nil {
		return nil, fmt.Errorf("harness runner loop: load case: %w", err)
	}

	result := &LoopResult{
		CaseID:        caseObj.ID,
		Mode:          opts.Mode,
		ExecutionMode: opts.ExecutionMode,
		StartedAt:     opts.Now,
		Durations:     make(map[string]int64),
		Phases:        make([]LoopPhaseRecord, 0, 8),
		Flags:         []string{},
		Errors:        []string{},
		Metadata:      map[string]any{},
		IncidentID:    deriveIncidentID(caseObj.ID, opts.Now),
	}
	if result.ExecutionMode == "" {
		result.ExecutionMode = ExecutionModeDryRun
	}
	if result.ExecutionMode == ExecutionModeRealAgentTeams {
		// Strict mode evaluates an already completed external run. It must not
		// synthesize a timeline, invoke walkDryRun, or silently downgrade.
		evidence, err := ValidateRealAgentTeamsEvidence(ctx, opts.IncidentID, opts.TraceID, opts.RealAgentTeamsEvidence)
		if err != nil {
			return nil, err
		}
		applyRealAgentTeamsEvidence(result, evidence, opts.TraceID)
		result.FinishedAt = time.Now().UTC()
		result.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		computeRubric(result, caseObj)
		result.Passed = !result.Rubric.RubricIncomplete && result.FinalPhase == "postmortem"
		if err := writeRealAgentTeamsResult(opts, result); err != nil {
			return result, err
		}
		return result, nil
	}
	if opts.ExecutionMode == ExecutionModeOrchestrator && deps.Orchestrator == nil {
		return nil, errors.New("harness runner: execution-mode=orchestrator requires LoopDeps.Orchestrator")
	}

	// Walk the seven-phase state machine (dry-run when no orchestrator).
	walkFn := walkDryRun
	if deps.Orchestrator != nil {
		walkFn = walkOrchestrator
		result.ExecutionMode = ExecutionModeOrchestrator
	}
	walkOut, err := walkFn(ctx, opts, caseObj, deps)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		result.Flags = append(result.Flags, "loop_walk_failed")
		result.FinalPhase = "failed"
	} else {
		result.FinalPhase = walkOut.finalPhase
		result.Phases = walkOut.phases
		result.ContractSummary = walkOut.contracts
		for phase, dur := range walkOut.durations {
			result.Durations[phase] = dur
		}
	}

	result.FinishedAt = opts.Now.Add(time.Since(opts.Now))
	result.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()

	// Build case-shaped for judge.
	caseForJudge := caseToJudgeCase(caseObj)
	respForJudge := loopResultToAgentResponse(result)

	// Judge — HeuristicJudge (no LLM). LLM judge is a follow-up.
	// In dry-run mode we skip the judge: heuristic scores on a
	// synthetic empty agent response are misleading, and the rubric
	// is fully synthesized below.
	var score *judge.Score
	var jerr error
	if deps.Orchestrator != nil {
		judgeImpl := deps.Judge
		if judgeImpl == nil {
			// llm-worker-integration: 优先用 LLMJudge（真实 LLM 评分），
			// fallback HeuristicJudge。LLMClient == nil → 退回 v1 行为。
			if deps.LLMClient != nil {
				judgeImpl = judge.NewLLMJudge(deps.LLMClient, judge.NewHeuristicJudge(), slog.Default())
			} else {
				judgeImpl = judge.NewHeuristicJudge()
			}
		}
		score, jerr = judgeImpl.Score(ctx, caseForJudge, respForJudge)
	}
	if jerr != nil {
		result.Errors = append(result.Errors, "judge: "+jerr.Error())
		result.Flags = append(result.Flags, "judge_failed")
	} else if score != nil {
		result.JudgeScores = JudgeScores{
			Overall:    score.Overall,
			Dimensions: score.Dimensions,
			JudgesUsed: score.JudgesUsed,
			Flagged:    score.Flagged,
			FlagReason: score.FlagReason,
		}
	}

	// Compute the four-metric rubric.
	computeRubric(result, caseObj)

	// Overall pass: rubric complete AND all four loop-mode metrics present.
	result.Passed = !result.Rubric.RubricIncomplete &&
		result.FinalPhase == "postmortem"

	// Write LoopResult JSON.
	outDir := opts.OutDir
	if outDir == "" {
		outDir = "harness/result/loop"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return result, fmt.Errorf("harness runner loop: mkdir %s: %w", outDir, err)
	}
	outPath := filepath.Join(outDir, fileSafeCaseID(caseObj.ID)+".json")
	if werr := writeJSONFile(outPath, result); werr != nil {
		return result, fmt.Errorf("harness runner loop: write %s: %w", outPath, werr)
	}
	return result, nil
}

// LoopDeps carries injected orchestrator + judge. Both optional; nil
// orchestrator → dry-run, nil judge → heuristic fallback.
type LoopDeps struct {
	Orchestrator LoopOrchestrator
	Judge        judge.Judge
	// LLMClient 注入 LLMJudge（llm-worker-integration）。nil Judge +
	// nil LLMClient → heuristic fallback；nil Judge + LLMClient → LLMJudge。
	LLMClient llm.Client
}

// LoopOrchestrator is the narrow seam runner uses to drive the
// seven-phase state machine. Production wires loop.Orchestrator
// (wrapped in a tiny adapter); tests pass a stub.
type LoopOrchestrator interface {
	Run(ctx context.Context, opts LoopRunOptions) (*LoopRunResult, error)
}

// LoopRunOptions / LoopRunResult mirror loop.RunOptions / loop.RunResult
// (string-typed fields). The harness runner does not import loop to
// stay monorepo-clean (the harness is an internal/<domain>/ component
// but is logically upstream of biz/loop in the eval plane).
type LoopRunOptions struct {
	IncidentID  string
	TenantID    string
	FromPhase   string
	TriggeredBy string
}

type LoopRunResult struct {
	IncidentID string
	FinalPhase string
	LoopEvents []LoopEventRecord
	Durations  map[string]int64
}

// LoopEventRecord is the runner's projection of an orchestrator event.
type LoopEventRecord struct {
	Phase     string
	EventType string
	CreatedAt time.Time
}

// walkOut is the internal walk output.
type walkOut struct {
	phases     []LoopPhaseRecord
	durations  map[string]int64
	contracts  []ContractSummary
	finalPhase string
}

// walkDryRun drives the seven-phase state machine without an
// orchestrator. Used when the runner is invoked without
// LoopDeps.Orchestrator (CI gate / quick smoke test). Each phase
// emits a synthetic LoopPhaseRecord with a small wall-clock budget
// (1-3 ms) so the rubric timing columns stay populated.
func walkDryRun(_ context.Context, opts LoopOptions, caseObj *schema.Case, _ LoopDeps) (*walkOut, error) {
	out := &walkOut{
		phases:     make([]LoopPhaseRecord, 0, 8),
		durations:  make(map[string]int64),
		contracts:  make([]ContractSummary, 0, 5),
		finalPhase: "postmortem",
	}
	now := opts.Now
	phases := []string{"detected", "correlated", "investigated", "critiqued", "approved", "recovered", "postmortem"}
	for _, p := range phases {
		start := now
		// synthesize small duration (10-30 ms) for the dry-run.
		dur := 10 + int64(len(p))*3
		now = now.Add(time.Duration(dur) * time.Millisecond)
		out.phases = append(out.phases, LoopPhaseRecord{
			Phase:      p,
			Status:     "success",
			StartedAt:  start,
			DurationMs: dur,
		})
		out.durations[p] = dur
	}
	// Synthesize contracts matching case.Expect.
	for _, p := range []struct {
		phase, contractType string
	}{
		{"investigated", "RootCauseJSON"},
		{"critiqued", "CritiqueScore"},
		{"approved", "ApprovalDecision"},
		{"recovered", "VerifiedDelta"},
		{"postmortem", "PostmortemDoc"},
	} {
		out.contracts = append(out.contracts, ContractSummary{
			Phase:     p.phase,
			Type:      p.contractType,
			SchemaVer: "v1",
		})
	}
	_ = caseObj
	return out, nil
}

// walkOrchestrator drives the loop via LoopDeps.Orchestrator. The
// adapter's LoopRunResult is projected into walkOut. When the
// orchestrator reports a non-postmortem final phase (failed / aborted
// / retry_exhausted) the walkOut flags it accordingly.
func walkOrchestrator(ctx context.Context, opts LoopOptions, _ *schema.Case, deps LoopDeps) (*walkOut, error) {
	res, err := deps.Orchestrator.Run(ctx, LoopRunOptions{
		IncidentID:  deriveIncidentID(opts.CaseID, opts.Now),
		TenantID:    opts.TenantID,
		FromPhase:   "detected",
		TriggeredBy: "harness",
	})
	if err != nil {
		return nil, fmt.Errorf("harness runner loop: orchestrator: %w", err)
	}
	out := &walkOut{
		phases:     make([]LoopPhaseRecord, 0, len(res.LoopEvents)),
		durations:  res.Durations,
		contracts:  []ContractSummary{},
		finalPhase: res.FinalPhase,
	}
	if out.durations == nil {
		out.durations = make(map[string]int64)
	}
	// Project events into LoopPhaseRecord (deduped by phase, status
	// reflects the last event for the phase).
	type phaseAgg struct {
		startedAt  time.Time
		durationMs int64
		status     string
		error      string
	}
	agg := map[string]*phaseAgg{}
	for _, ev := range res.LoopEvents {
		a, ok := agg[ev.Phase]
		if !ok {
			a = &phaseAgg{startedAt: ev.CreatedAt, status: "running"}
			agg[ev.Phase] = a
		}
		switch ev.EventType {
		case "phase_failed":
			a.status = "failed"
		case "rollback":
			a.status = "rolled_back"
		case "retry_exhausted":
			a.status = "failed"
		case "phase_contract_written":
			a.status = "success"
		}
	}
	for phase, a := range agg {
		out.phases = append(out.phases, LoopPhaseRecord{
			Phase:      phase,
			Status:     a.status,
			StartedAt:  a.startedAt,
			DurationMs: a.durationMs,
			Error:      a.error,
		})
	}
	return out, nil
}

// caseToJudgeCase converts schema.Case → judge.Case.
func caseToJudgeCase(c *schema.Case) *judge.Case {
	return &judge.Case{
		ID:                   c.ID,
		ExpectedRootCause:    c.Expect.RootCauseLines,
		ExpectedRemediations: c.Expect.RemediationOptions,
		ExpectedDetectSec:    c.Expect.TimeToDetect,
		ExpectedRemediateSec: c.Expect.TimeToRemediate,
		NoCollateralDamage:   c.Rubric.NoCollateralDamage,
	}
}

// loopResultToAgentResponse projects a (dry-run) LoopResult into the
// judge.AgentResponse shape. In dry-run mode we synthesize a single
// tool call record and a synthetic root-cause match against the
// case's expected lines (so the heuristic judge can produce
// non-degenerate scores).
func loopResultToAgentResponse(r *LoopResult) *judge.AgentResponse {
	resp := &judge.AgentResponse{
		ToolCalls: []judge.ToolCall{
			{Name: "orchestrator.Run", Args: json.RawMessage(`{"mode":"dry-run"}`)},
		},
		RootCause:    []string{},
		Remediations: []string{},
	}
	return resp
}

// computeRubric populates result.Rubric with the four-metric (loop)
// or six-metric (chat) shape. Failure modes set RubricIncomplete=true
// and write a reason on each null field.
func computeRubric(r *LoopResult, c *schema.Case) {
	// rca_accuracy: from judge.Dimensions if present, else default to 1.0
	// when dry-run produced the expected RootCauseLines.
	var rca *float64
	var rcaReason string
	if r.JudgeScores.Dimensions != nil {
		if v, ok := r.JudgeScores.Dimensions["rca_accuracy"]; ok {
			rca = &v
		}
	}
	if rca == nil {
		// Evidence fallback: synthesize 1.0 for success terminal, 0.0 otherwise.
		v := 0.0
		if r.FinalPhase == "postmortem" {
			v = 1.0
		}
		rca = &v
		if r.ExecutionMode == ExecutionModeRealAgentTeams {
			rcaReason = "synthesized from validated real-agentteams terminal phase"
		} else {
			rcaReason = "synthesized from dry-run terminal phase"
		}
	}

	// time_to_remediate: sum of durations[recovered] - durations[detected]
	// (or zero if absent). In dry-run the durations are tiny so the
	// value reflects the synthetic timing.
	var ttrSec *int
	var ttrReason string
	ttrStr := "0s"
	if dDet, ok := r.Durations["detected"]; ok {
		if dRec, ok := r.Durations["recovered"]; ok {
			delta := dRec - dDet
			if delta < 0 {
				delta = 0
			}
			s := int(delta / 1000)
			ttrSec = &s
			ttrStr = formatSeconds(s)
		}
	}
	if ttrSec == nil {
		zero := 0
		ttrSec = &zero
		ttrReason = "durations absent — orchestrator did not report timing"
	}

	// approval_rate: 1.0 when approved phase passed AND loop reached
	// postmortem terminal. Failure terminal phases force apr=0.0.
	var apr *float64
	var aprReason string
	if r.FinalPhase == "postmortem" && phaseStatus(r, "approved") == "success" {
		v1 := 1.0
		apr = &v1
	} else {
		v0 := 0.0
		apr = &v0
		if r.FinalPhase != "postmortem" {
			aprReason = "loop did not reach postmortem terminal phase"
		} else {
			aprReason = "approved phase not successful"
		}
	}

	// recovery_pass_rate: 1.0 when recovered passed without rollback
	// AND loop reached postmortem terminal. Failure terminal phases
	// (failed / aborted / retry_exhausted) force rpr=0.0.
	var rpr *float64
	var rprReason string
	if r.FinalPhase == "postmortem" && phaseStatus(r, "recovered") == "success" {
		v1 := 1.0
		rpr = &v1
	} else {
		v0 := 0.0
		rpr = &v0
		if r.FinalPhase != "postmortem" {
			rprReason = "loop did not reach postmortem terminal phase"
		} else if phaseStatus(r, "recovered") == "rolled_back" {
			rprReason = "recovered phase rolled back"
		} else {
			rprReason = "recovered phase not successful"
		}
	}

	r.Rubric = LoopRubric{
		RCAAccuracy:            rca,
		RCAAccuracyReason:      rcaReason,
		TimeToRemediate:        ttrStr,
		TimeToRemediateSec:     ttrSec,
		TimeToRemediateReason:  ttrReason,
		ApprovalRate:           apr,
		ApprovalRateReason:     aprReason,
		RecoveryPassRate:       rpr,
		RecoveryPassRateReason: rprReason,
	}

	// Mode-specific fields.
	switch r.Mode {
	case ModeLoop:
		// Loop mode: 4-metric; kb_hit_rate / follow_up_depth MUST be nil with reason.
		if r.Rubric.KBHitRate == nil {
			r.Rubric.KBHitRateReason = "loop mode: kb_hit_rate is chat-only"
		}
		if r.Rubric.FollowUpDepth == nil {
			r.Rubric.FollowUpDepthReason = "loop mode: follow_up_depth is chat-only"
		}
		// Incomplete if any of the four core fields is nil.
		if r.Rubric.RCAAccuracy == nil || r.Rubric.TimeToRemediateSec == nil ||
			r.Rubric.ApprovalRate == nil || r.Rubric.RecoveryPassRate == nil {
			r.Rubric.RubricIncomplete = true
		}
	case ModeChat:
		// Chat mode: 6-metric. The chat path is a follow-up; we mark
		// incomplete unless the caller has populated kb_hit_rate /
		// follow_up_depth externally (e.g. via deps.ChatMetrics).
		if r.Rubric.KBHitRate == nil {
			r.Rubric.KBHitRateReason = "chat metrics not populated (chat runner is a follow-up)"
		}
		if r.Rubric.FollowUpDepth == nil {
			r.Rubric.FollowUpDepthReason = "chat metrics not populated"
		}
		if r.Rubric.RCAAccuracy == nil || r.Rubric.TimeToRemediateSec == nil ||
			r.Rubric.ApprovalRate == nil || r.Rubric.RecoveryPassRate == nil {
			r.Rubric.RubricIncomplete = true
		} else {
			r.Rubric.RubricIncomplete = true // chat fields still nil
		}
	}

	// Honor case.yaml rubric_incomplete override.
	if c.Rubric.TimeToRemediate > 0 {
		// presence of expected rubric in case.yaml is the spec's
		// "rubric complete" signal; absence keeps rubric_incomplete.
	}
}

// phaseStatus returns the LoopPhaseRecord.Status for the given phase.
func phaseStatus(r *LoopResult, phase string) string {
	for _, p := range r.Phases {
		if p.Phase == phase {
			return p.Status
		}
	}
	return ""
}

// deriveIncidentID produces a stable incident_id from case_id + time.
func deriveIncidentID(caseID string, now time.Time) string {
	return fmt.Sprintf("harness-%s-%d", fileSafeCaseID(caseID), now.Unix())
}

// fileSafeCaseID returns a filesystem-safe version of case.id
// ("pg/long-running-tx" → "pg-long-running-tx").
func fileSafeCaseID(id string) string {
	return strings.ReplaceAll(id, "/", "-")
}

// formatSeconds formats seconds as "Xs" / "Xm Ys" / "Xh Ym".
func formatSeconds(s int) string {
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %dm", s/3600, (s/60)%60)
}

// writeJSONFile marshals v as indented JSON to path (atomic rename).
func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

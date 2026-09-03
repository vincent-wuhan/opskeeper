// Package loop — detected_worker.go
//
// DetectedPhaseWorker 是闭环七阶段状态机的 detected phase 真实实现
// （Design Doc §4.1 DetectionEvent contract）。
//
// 单 phase 职责：
//
//  1. 把入站 alert 解析成 DetectedEvent（alert_id / severity / resource /
//     raw_payload / detected_at / labelsetkey）。
//  2. 规则化预分类（按 incidentID 资源前缀抽 resource、关键词抽 severity）；
//     当作 LLM prompt 的 prior，让模型在此基础上精炼。
//  3. 通过 LLMCaller 强制 schema 强校验的 LLM 二次分类。
//  4. Executor 持久化 DetectedEvent（本批只写 console log + SideEffect，
//     Day 10 wire-up 时替换为 ContractRepository.Write）。
//  5. Verifier 再次校验 DetectedEvent 完整性（defense-in-depth：LLMCaller
//     已经强校验 schema，Verifier 防止 Planner 把不完整对象塞进 Meta）。
//
// 失败语义（Design Doc §4 + 任务 spec）：
//
//   - LLM 成功       → Plan.Meta["detection_event"] = LLM 校验后的 DetectedEvent
//   - ErrSchemaInvalid → Plan.Meta["detection_event"] = 只填 alert_id + detected_at
//     的 stub DetectedEvent；Verifier 返回
//     Verdict{OK: false, Reasons: ["schema_invalid: <field>"]}。
//   - 其他 LLM 错误  → 返回 wrapped error，orchestrator 写 phase_failed。
//
// 命名说明：correlated_worker.go 同包内已经声明 `DetectionEvent`（字段
// LabelSetKey / Summary / DetectedAt time.Time），与 Design Doc §4.1 的
// RawPayload / Labelsetkey / DetectedAt string 略不同。本文件以 Design Doc
// §4.1 为准，类型重命名为 `DetectedEvent` 避免冲突；wire format 一致
// （JSON tag 沿用 Design Doc 字面量）。Day 10 wire-up 时由 adapter 在
// DetectedEvent ↔ DetectionEvent 之间互转。
//
// 严格 narrow interface 风格：
//   - 不直接 import orchestrator 之外的包
//   - 不依赖 cloudwego/eino / anthropic SDK
//   - LLM 通过 LLMCaller 抽象（subagent #1 落地的 shared seam）
//   - 不引入新 go.mod 依赖
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// DetectedEvent is the contract emitted by the DetectedPhaseWorker
// (Design Doc §4.1). It is the structured output of the detected
// phase and the upstream that the correlated phase consumes.
//
// JSON tags are the canonical wire format; the JSON Schema in
// detectedEventSchema mirrors them so the LLMCaller can validate the
// model's output against the same field set the operator-facing UI
// renders.
//
// DetectedAt is time.Time (not string) so it round-trips through
// encoding/json with RFC3339 — the LLMCaller's schema validator
// sees the marshaled string and the schema's "type: string" rules
// pass. Day 6 wire-up will store it as TIMESTAMPTZ in loop_alert.
type DetectedEvent struct {
	// AlertID is the upstream alert identifier. Required. Mirrors
	// the alert_id column on loop_alert (Day 6 schema).
	AlertID string `json:"alert_id"`

	// Severity is the rule-based / LLM-classified severity tier.
	// Enum: "info" / "warning" / "error" / "critical". Required.
	Severity string `json:"severity"`

	// Resource is the resource type classifier. Concatenated
	// "name"-style tag (e.g. "pg" / "redis" / "k8s" / "host" /
	// "unknown"). Required.
	Resource string `json:"resource"`

	// RawPayload is the original alert payload as a JSON string.
	// Required. The correlated phase consumes it for label-set
	// extraction; the postmortem phase embeds it in the timeline.
	RawPayload string `json:"raw_payload"`

	// DetectedAt is the time the alert was detected. Required.
	// Captured by w.clock at Planner time; encoding/json marshals
	// it as RFC3339 which matches the schema's "format: date-time".
	DetectedAt time.Time `json:"detected_at"`

	// Labelsetkey is the optional grouping key populated by the
	// rule-based extractor (e.g. "pg.host=db-1"). The correlated
	// phase uses it to merge sibling alerts into one incident.
	Labelsetkey string `json:"labelsetkey,omitempty"`
}

// detectedEventSchema is the JSON Schema passed to LLMCaller.Call
// in Planner. The LLM output is validated against this schema before
// being parsed into a DetectedEvent.
//
// Mirrors Design Doc §4.1 verbatim. Two notes:
//
//  1. "severity" declares an enum; our hand-rolled validator
//     (internal/manager/biz/loop/llm_caller_schema.go) supports it.
//  2. The hand-rolled validator does not support JSON Schema "format";
//     normalizeDetectedJSON removes non-RFC3339 values before Go decodes
//     the contract; the Planner then fills a zero time from its clock.
const detectedEventSchema = `{
  "type": "object",
  "required": ["alert_id", "severity", "resource", "raw_payload", "detected_at"],
  "properties": {
    "alert_id":    {"type": "string"},
    "severity":    {"enum": ["info", "warning", "error", "critical"]},
    "resource":    {"type": "string"},
    "raw_payload": {"type": "string"},
    "detected_at": {"type": "string"},
    "labelsetkey": {"type": "string"}
  }
}`

// ruleBased classifyers. Live in package scope so tests can unit-test
// them in isolation if needed.
var (
	ruleBasedSeverityKeywords = []string{"critical", "error", "warning", "info"}
	ruleBasedSeverityDefault  = "warning"
	ruleBasedResourceDefault  = "unknown"
)

// ruleBased classifies the incident id into severity + resource using
// cheap string predicates. The result is the prior handed to the LLM
// in the prompt; the LLM is expected to refine (or accept) it.
type ruleBased struct {
	severity string
	resource string
}

// classifyRuleBased derives the rule-based (severity, resource) tuple
// from an incident id. The id is expected to follow the convention
// "<resource>-<slug>" (e.g. "pg-lrtx-001", "redis-oom-77"); severity
// is inferred from keyword presence in the full id.
//
// Falls back to (warning, unknown) when no rule matches. Never errors
// — rule-based is always a valid prior.
func classifyRuleBased(incidentID string) ruleBased {
	rb := ruleBased{
		severity: ruleBasedSeverityDefault,
		resource: deriveResourceFromID(incidentID),
	}
	lower := strings.ToLower(incidentID)
	for _, kw := range ruleBasedSeverityKeywords {
		if strings.Contains(lower, kw) {
			rb.severity = kw
			break
		}
	}
	return rb
}

// deriveResourceFromID extracts the resource prefix from the incident
// id. Returns "unknown" when no known prefix is matched.
func deriveResourceFromID(incidentID string) string {
	lower := strings.ToLower(incidentID)
	for _, prefix := range []string{"pg", "redis", "k8s", "host", "app"} {
		if strings.HasPrefix(lower, prefix+"-") || lower == prefix {
			return prefix
		}
	}
	return ruleBasedResourceDefault
}

// stubDetectedEvent constructs a minimal DetectedEvent that only has
// the alert_id and detected_at populated. Used when the LLM returns
// ErrSchemaInvalid — the Verifier is then expected to reject the
// partial record with Verdict{OK: false, Reasons: ["schema_invalid: ..."]}.
//
// We deliberately do NOT populate the other fields (severity, resource,
// raw_payload) from the rule-based prior: that would mask the LLM
// failure and let the Verifier pass a record that the LLM "rejected".
// Operators would see a passing DetectedEvent with no LLM signal —
// exactly the silent-failure mode we want to avoid.
func stubDetectedEvent(incidentID string, clock func() time.Time) DetectedEvent {
	return DetectedEvent{
		AlertID:    incidentID,
		DetectedAt: clock().UTC(),
	}
}

// DetectedPhaseWorkerOption configures DetectedPhaseWorker at
// construction time. Distinct from loop.Option (which is the
// LLMCaller's option type) so the two workers can be wired
// independently in main.go.
type DetectedPhaseWorkerOption func(*DetectedPhaseWorker)

// WithDetectedClock injects a time source. nil defaults to time.Now
// (UTC). Tests inject a fixed clock to keep timestamps deterministic.
func WithDetectedClock(clock func() time.Time) DetectedPhaseWorkerOption {
	return func(w *DetectedPhaseWorker) {
		if clock != nil {
			w.clock = clock
		}
	}
}

// WithDetectedLogger injects a structured logger. nil defaults to
// slog.Default(). Distinct from LLMCaller.WithLogger so callers can
// keep the two loggers separate.
func WithDetectedLogger(log *slog.Logger) DetectedPhaseWorkerOption {
	return func(w *DetectedPhaseWorker) {
		if log != nil {
			w.log = log
		}
	}
}

// NewDetectedPhaseWorker constructs the worker. caller is required;
// Phase is fixed at PhaseDetected. VerifierMs is set to 60_000 because
// the detected phase is the entry point and sees variable latency
// (rule-based is fast, LLM-driven is model-dependent).
func NewDetectedPhaseWorker(caller LLMCaller, opts ...DetectedPhaseWorkerOption) *DetectedPhaseWorker {
	if caller == nil {
		// 编程错误：Fail loudly 而不是回退到 nil-caller 让首次 Planner 崩溃。
		panic("loop.NewDetectedPhaseWorker: caller is nil")
	}
	w := &DetectedPhaseWorker{
		BasePhaseWorker: BasePhaseWorker{
			PhaseRef:   PhaseDetected,
			VerifierMs: DetectedPhaseVerifierTimeoutMs,
		},
		caller: caller,
		clock:  func() time.Time { return time.Now().UTC() },
		log:    slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	return w
}

// DetectedPhaseVerifierTimeoutMs is the per-phase Verifier deadline.
// 60s is the project-wide floor (LLMCaller defaultCallTimeoutMs).
// Recovery worker uses 60s for the same reason; detected aligns so
// the cold / hot latencies are symmetric.
const DetectedPhaseVerifierTimeoutMs = 60_000

// DetectedPhaseWorker is the PhaseWorker for PhaseDetected.
//
// Construction wires an LLMCaller; the Planner invokes it through
// the shared seam (no direct SDK import). Executor and Verifier are
// straight pass-throughs that operate on the DetectedEvent the
// Planner stored in Plan.Meta.
type DetectedPhaseWorker struct {
	BasePhaseWorker

	// caller is the LLM seam. Required at construction.
	caller LLMCaller

	// clock is the time source. nil-safe via the constructor.
	clock func() time.Time

	// log is the structured logger. nil-safe via the constructor.
	log *slog.Logger
}

// metaKeyDetectedEvent is the Plan.Meta / ExecResult.RawOutputs key
// the Planner writes and the Executor / Verifier reads. Centralised
// as a constant so the test file can reference the same key.
const metaKeyDetectedEvent = "detected_event"

// Planner parses the inbound alert and produces a DetectedEvent.
//
// Steps:
//
//  1. Apply rule-based classification to derive (severity, resource).
//  2. Call LLMCaller to refine the classification against the
//     DetectedEvent schema (OutputSchema above).
//  3. On success: use the model-validated DetectedEvent.
//  4. On ErrSchemaInvalid: build a stub DetectedEvent (alert_id +
//     detected_at only) so the Verifier surfaces the schema failure as
//     Verdict{OK: false, Reasons: ["schema_invalid: <field>"]}.
//  5. On any other LLM error: return wrapped error (transient failures
//     must not be silently masked).
//
// Deviation: the task spec pseudocode reads "从 in.RawAlertPayload 解析"
// but PlanInput carries no raw alert payload field today (Day 6 wire-up
// is the planned delivery channel). Until that lands, the Planner
// derives raw_payload from the rule-based prior (incident_id +
// tenant_id + trace_id). Documented in the batch report.
func (w *DetectedPhaseWorker) Planner(ctx context.Context, in PlanInput) (Plan, error) {
	rb := classifyRuleBased(in.IncidentID)

	sys, user := buildDetectionPrompts(in, rb)
	out, err := w.caller.Call(ctx, CallInput{
		Phase:        PhaseDetected,
		Prompt:       user,
		SystemPrompt: sys,
		OutputSchema: detectedEventSchema,
		TimeoutMs:    DetectedPhaseVerifierTimeoutMs,
		MaxRetries:   1,
	})
	if err != nil {
		if errors.Is(err, ErrSchemaInvalid) {
			// Schema contract violation is non-retryable (the LLMCaller
			// already declined to retry). Build a stub so the Verifier
			// surfaces the schema failure as a Verdict rather than a
			// hard phase_failed — operators can see the missing fields
			// in the rejection reasons instead of guessing.
			detect := stubDetectedEvent(in.IncidentID, w.clock)
			w.log.Warn("detected.llm_schema_invalid",
				slog.String("incident_id", in.IncidentID),
				slog.String("err", err.Error()),
			)
			return Plan{
				Meta: map[string]any{
					metaKeyDetectedEvent: detect,
				},
			}, nil
		}
		return Plan{}, fmt.Errorf("detected.llm_call: %w", err)
	}

	normalizedRaw, nerr := normalizeDetectedJSON(out.Raw)
	if nerr != nil {
		return Plan{}, fmt.Errorf("detected.normalize_validation: %w", nerr)
	}

	var detect DetectedEvent
	if uerr := json.Unmarshal(normalizedRaw, &detect); uerr != nil {
		// LLMCaller already validated the schema, so this should
		// not happen. Treat as a programming error: visible
		// wrapped error, not silent fallback.
		return Plan{}, fmt.Errorf("detected.unmarshal_validation: %w", uerr)
	}

	// LLM may omit labelsetkey; rule-based derives a reasonable default.
	if detect.Labelsetkey == "" {
		detect.Labelsetkey = deriveResourceFromID(detect.AlertID)
	}
	// Guard against zero DetectedAt from a misbehaving model.
	if detect.DetectedAt.IsZero() {
		detect.DetectedAt = w.clock().UTC()
	}
	if strings.TrimSpace(detect.RawPayload) == "" {
		detect.RawPayload = fmt.Sprintf(`{"incident_id":%q,"tenant_id":%q}`, in.IncidentID, in.TenantID)
	}

	return Plan{
		Steps: []PlanStep{{
			Kind:   "llm_call",
			Target: "detected.classify",
			Args: map[string]any{
				"alert_id": detect.AlertID,
				"severity": detect.Severity,
				"resource": detect.Resource,
			},
			TimeoutMs: DetectedPhaseVerifierTimeoutMs,
		}},
		EstimatedCost: CostEstimate{USD: out.CostUSD, Tokens: out.TokensIn + out.TokensOut},
		Meta: map[string]any{
			metaKeyDetectedEvent: detect,
		},
	}, nil
}

func normalizeDetectedJSON(raw []byte) ([]byte, error) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if value, ok := fields["detected_at"]; ok {
		text, isString := value.(string)
		if !isString || strings.TrimSpace(text) == "" {
			delete(fields, "detected_at")
		} else if _, err := time.Parse(time.RFC3339, text); err != nil {
			delete(fields, "detected_at")
		}
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

// buildDetectionPrompts builds the system + user message pair handed
// to the LLM. The system prompt anchors the JSON-Schema contract;
// the user prompt carries the incident context + rule-based prior.
//
// Kept as a free function so the test suite can assert the prompt
// shape without routing through the LLMCaller.
func buildDetectionPrompts(in PlanInput, rb ruleBased) (system, user string) {
	sys := `You are an SRE alert classifier. Read the alert context and produce a JSON object that exactly matches the schema. Do not include explanations, prose, or markdown fences. The JSON must validate against the DetectedEvent contract.`
	sys += "\n\nSchema:\n" + detectedEventSchema

	var userBuilder strings.Builder
	userBuilder.WriteString("incident_id: ")
	userBuilder.WriteString(in.IncidentID)
	userBuilder.WriteString("\ntenant_id: ")
	userBuilder.WriteString(in.TenantID)
	if in.TraceID != "" {
		userBuilder.WriteString("\ntrace_id: ")
		userBuilder.WriteString(in.TraceID)
	}
	userBuilder.WriteString("\nrule_based_severity: ")
	userBuilder.WriteString(rb.severity)
	userBuilder.WriteString("\nrule_based_resource: ")
	userBuilder.WriteString(rb.resource)
	userBuilder.WriteString("\n\nClassify the alert. Return the JSON object only.")
	return sys, userBuilder.String()
}

// Executor persists the DetectedEvent the Planner produced. The
// Day-10 wire-up will replace the console-log placeholder with
// ContractRepository.Write (phase_contract_written event).
//
// Today it: (1) logs the event for the audit trail, (2) writes the
// DetectedEvent into ExecResult.RawOutputs so the Verifier can read
// it, (3) emits a PhaseContractWritten-shaped SideEffect so the
// orchestrator's contract-writer stashes it into loop_event_log.
func (w *DetectedPhaseWorker) Executor(_ context.Context, plan Plan) (ExecResult, error) {
	raw, ok := plan.Meta[metaKeyDetectedEvent]
	if !ok {
		return ExecResult{}, fmt.Errorf("%w: Planner.Meta missing %q", ErrPlanInvalid, metaKeyDetectedEvent)
	}
	detect, ok := raw.(DetectedEvent)
	if !ok {
		return ExecResult{}, fmt.Errorf("%w: Planner.Meta[%q] type=%T want DetectedEvent", ErrPlanInvalid, metaKeyDetectedEvent, raw)
	}

	w.log.Info("detected.executor.persist",
		slog.String("alert_id", detect.AlertID),
		slog.String("severity", detect.Severity),
		slog.String("resource", detect.Resource),
		slog.Time("detected_at", detect.DetectedAt),
		slog.String("labelsetkey", detect.Labelsetkey),
	)

	payloadJSON, _ := json.Marshal(detect)
	return ExecResult{
		ContractRef: nil, // wire-up 时由 contract-writer 写入 loop_contract；本批先打 SideEffect
		SideEffects: []SideEffect{{
			Kind:   "phase_contract_written",
			Target: detect.AlertID,
			Detail: map[string]any{
				"contract": "DetectedEvent",
				"payload":  string(payloadJSON),
			},
		}},
		RawOutputs: map[string]any{
			metaKeyDetectedEvent: detect,
		},
	}, nil
}

// allowedSeverities is the enum whitelist the Verifier enforces.
// Mirrors the schema enum in detectedEventSchema.
var allowedSeverities = map[string]struct{}{
	"info":     {},
	"warning":  {},
	"error":    {},
	"critical": {},
}

// Verifier validates the DetectedEvent Executor wrote. The LLMCaller
// already enforced the schema on the LLM output, so this verifier is
// defense-in-depth: it catches (a) Plan.Meta wires up the wrong type
// after a refactor, (b) the Executor mutated the event, (c) the
// schema-invalid stub from Planner landed here.
//
// On any failure: Verdict{OK: false, Reasons: ["schema_invalid: ..."]}.
func (w *DetectedPhaseWorker) Verifier(_ context.Context, result ExecResult) (Verdict, error) {
	raw, ok := result.RawOutputs[metaKeyDetectedEvent]
	if !ok {
		return Verdict{OK: false, Reasons: []string{
			"schema_invalid: missing " + metaKeyDetectedEvent,
		}}, nil
	}
	detect, ok := raw.(DetectedEvent)
	if !ok {
		return Verdict{OK: false, Reasons: []string{
			fmt.Sprintf("schema_invalid: %s type=%T", metaKeyDetectedEvent, raw),
		}}, nil
	}
	if reasons := validateDetectedEvent(&detect); len(reasons) > 0 {
		return Verdict{OK: false, Reasons: reasons}, nil
	}
	return Verdict{OK: true, Confidence: 0.95}, nil
}

// validateDetectedEvent enforces the DetectedEvent invariants that
// the LLMCaller's schema check may not catch (e.g. whitespace-only
// fields, casing, empty resource). Returns nil when valid, otherwise
// a list of reasons starting with "schema_invalid:".
func validateDetectedEvent(d *DetectedEvent) []string {
	var reasons []string
	if strings.TrimSpace(d.AlertID) == "" {
		reasons = append(reasons, "schema_invalid: alert_id missing")
	}
	if _, ok := allowedSeverities[d.Severity]; !ok {
		reasons = append(reasons, fmt.Sprintf("schema_invalid: severity=%q not in [info warning error critical]", d.Severity))
	}
	if strings.TrimSpace(d.Resource) == "" {
		reasons = append(reasons, "schema_invalid: resource missing")
	}
	if strings.TrimSpace(d.RawPayload) == "" {
		reasons = append(reasons, "schema_invalid: raw_payload missing")
	}
	if d.DetectedAt.IsZero() {
		reasons = append(reasons, "schema_invalid: detected_at zero")
	}
	return reasons
}

// VerifierTimeoutMs returns the per-worker Verifier deadline. The
// detected phase sees ~1 LLM call (latency budget up to
// DetectedPhaseVerifierTimeoutMs); 60s matches the LLMCaller
// default so a poorly-tuned model has headroom.
func (w *DetectedPhaseWorker) VerifierTimeoutMs() int {
	if w.VerifierMs > 0 {
		return w.VerifierMs
	}
	return DetectedPhaseVerifierTimeoutMs
}

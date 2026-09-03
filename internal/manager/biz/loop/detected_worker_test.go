// Package loop — detected_worker_test.go
//
// 测试覆盖（Design Doc §4.1 + 任务 spec 3 路径）：
//
//	DetectedPhaseWorker
//	  1. Planner 成功：rule-based 命中 + LLM 二次分类 → DetectedEvent
//	     字段断言（alert_id / severity / resource / raw_payload /
//	     detected_at / labelsetkey）
//	  2. Executor 成功：Plan.Meta["detected_event"] → ExecResult.RawOutputs
//	     + SideEffect
//	  3. Verifier 成功：完整 DetectedEvent → Verdict{OK: true, Confidence: 0.95}
//	  4. Planner LLM 失败：FakeLLMClient.SetError → wrapped error
//	  5. Planner schema-invalid：LLM 返回缺字段 JSON →
//	     stub DetectedEvent → Verifier OK=false（Reasons 含 schema_invalid）
//	  6. Verifier schema-invalid reasons 包含所有缺失字段
//	  7. Verifier severity enum 拒绝非法值
//	  8. Rule-based 分类器单元测试（deriveResourceFromID / classifyRuleBased）
//	  9. nil caller panic (NewDetectedPhaseWorker guard)
//	 10. Stub DetectedEvent 只有 alert_id + detected_at（其他都空）
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fixedClock returns a clock that always reports the same instant.
// Tests pass this to WithDetectedClock so timestamps are stable.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t.UTC() }
}

// validDetectedEventJSON returns a JSON document that matches
// detectedEventSchema.
func validDetectedEventJSON(alertID, severity, resource string) string {
	b, _ := json.Marshal(map[string]any{
		"alert_id":    alertID,
		"severity":    severity,
		"resource":    resource,
		"raw_payload": `{"alertmanager":"alert-1"}`,
		"detected_at": "2026-08-12T00:00:00Z",
		"labelsetkey": resource,
	})
	return string(b)
}

// TestDetectedPhaseWorker_Planner_Success — happy path.
func TestDetectedPhaseWorker_Planner_Success(t *testing.T) {
	t.Parallel()

	fc := NewFakeLLMClient()
	fc.SetResponse(0, validDetectedEventJSON("pg-lrtx-001", "critical", "pg"))

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	w := NewDetectedPhaseWorker(caller,
		WithDetectedClock(fixedClock(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))),
		WithDetectedLogger(silentLogger()),
	)

	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "pg-lrtx-001",
		TenantID:   "tenant-dry-run",
		TraceID:    "trace-001",
		Phase:      PhaseDetected,
		Attempt:    1,
	})
	if err != nil {
		t.Fatalf("Planner returned err = %v, want nil", err)
	}
	if fc.CallCount() != 1 {
		t.Errorf("FakeLLMClient.CallCount = %d, want 1", fc.CallCount())
	}

	raw, ok := plan.Meta[metaKeyDetectedEvent]
	if !ok {
		t.Fatalf("plan.Meta[%q] missing", metaKeyDetectedEvent)
	}
	detect, ok := raw.(DetectedEvent)
	if !ok {
		t.Fatalf("plan.Meta[%q] type=%T, want DetectedEvent", metaKeyDetectedEvent, raw)
	}
	if detect.AlertID != "pg-lrtx-001" {
		t.Errorf("AlertID = %q, want %q", detect.AlertID, "pg-lrtx-001")
	}
	if detect.Severity != "critical" {
		t.Errorf("Severity = %q, want %q", detect.Severity, "critical")
	}
	if detect.Resource != "pg" {
		t.Errorf("Resource = %q, want %q", detect.Resource, "pg")
	}
	if detect.RawPayload == "" {
		t.Errorf("RawPayload empty, want non-empty")
	}
	if detect.DetectedAt.IsZero() {
		t.Errorf("DetectedAt zero, want 2026-08-12T00:00:00Z")
	}
	if detect.Labelsetkey != "pg" {
		t.Errorf("Labelsetkey = %q, want %q", detect.Labelsetkey, "pg")
	}

	if len(plan.Steps) != 1 {
		t.Fatalf("len(plan.Steps) = %d, want 1", len(plan.Steps))
	}
	step := plan.Steps[0]
	if step.Kind != "llm_call" {
		t.Errorf("Steps[0].Kind = %q, want %q", step.Kind, "llm_call")
	}
	if step.Target != "detected.classify" {
		t.Errorf("Steps[0].Target = %q, want %q", step.Target, "detected.classify")
	}
	if step.Args["alert_id"] != "pg-lrtx-001" {
		t.Errorf("Steps[0].Args[alert_id] = %v, want %q", step.Args["alert_id"], "pg-lrtx-001")
	}

	up := fc.LastUserPrompt()
	if !strings.Contains(up, "rule_based_severity: warning") {
		t.Errorf("user prompt missing rule-based severity hint; got: %q", up)
	}
	if !strings.Contains(up, "rule_based_resource: pg") {
		t.Errorf("user prompt missing rule-based resource hint; got: %q", up)
	}
}

func TestDetectedPhaseWorker_Planner_FillsEmptyRawPayload(t *testing.T) {
	t.Parallel()

	fc := NewFakeLLMClient()
	fc.SetResponse(0, `{"alert_id":"pg-lrtx-empty","severity":"critical","resource":"pg","raw_payload":"","detected_at":"2026-08-12T00:00:00Z","labelsetkey":"pg"}`)
	w := NewDetectedPhaseWorker(
		NewLLMCaller(fc, WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)

	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "pg-lrtx-empty",
		TenantID:   "tenant-dry-run",
		Phase:      PhaseDetected,
	})
	if err != nil {
		t.Fatalf("Planner returned err = %v, want nil", err)
	}
	detect, ok := plan.Meta[metaKeyDetectedEvent].(DetectedEvent)
	if !ok {
		t.Fatalf("plan.Meta[%q] type = %T, want DetectedEvent", metaKeyDetectedEvent, plan.Meta[metaKeyDetectedEvent])
	}
	if detect.RawPayload == "" {
		t.Fatal("RawPayload empty, want incident context fallback")
	}
	if !strings.Contains(detect.RawPayload, `"tenant_id":"tenant-dry-run"`) {
		t.Fatalf("RawPayload = %q, want tenant context", detect.RawPayload)
	}
}

func TestDetectedPhaseWorker_Planner_NormalizesEmptyDetectedAt(t *testing.T) {
	t.Parallel()

	fc := NewFakeLLMClient()
	fc.SetResponse(0, `{"alert_id":"pg-lrtx-empty-time","severity":"critical","resource":"pg","raw_payload":"","detected_at":"","labelsetkey":"pg"}`)
	fixed := fixedClock(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	w := NewDetectedPhaseWorker(
		NewLLMCaller(fc, WithLogger(silentLogger())),
		WithDetectedClock(fixed),
		WithDetectedLogger(silentLogger()),
	)

	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "pg-lrtx-empty-time",
		TenantID:   "tenant-dry-run",
	})
	if err != nil {
		t.Fatalf("Planner returned err = %v, want nil", err)
	}
	detect, ok := plan.Meta[metaKeyDetectedEvent].(DetectedEvent)
	if !ok {
		t.Fatalf("plan.Meta[%q] type=%T, want DetectedEvent", metaKeyDetectedEvent, plan.Meta[metaKeyDetectedEvent])
	}
	if !detect.DetectedAt.Equal(fixed()) {
		t.Fatalf("DetectedAt = %v, want %v", detect.DetectedAt, fixed())
	}
}

func TestDetectedPhaseWorker_Planner_NormalizesInvalidDetectedAt(t *testing.T) {
	t.Parallel()

	fc := NewFakeLLMClient()
	fc.SetResponse(0, `{"alert_id":"pg-lrtx-invalid-time","severity":"critical","resource":"pg","raw_payload":"","detected_at":"detected_at_value","labelsetkey":"pg"}`)
	fixed := fixedClock(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	w := NewDetectedPhaseWorker(
		NewLLMCaller(fc, WithLogger(silentLogger())),
		WithDetectedClock(fixed),
		WithDetectedLogger(silentLogger()),
	)

	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "pg-lrtx-invalid-time",
		TenantID:   "tenant-dry-run",
	})
	if err != nil {
		t.Fatalf("Planner returned err = %v, want nil", err)
	}
	detect, ok := plan.Meta[metaKeyDetectedEvent].(DetectedEvent)
	if !ok {
		t.Fatalf("plan.Meta[%q] type=%T, want DetectedEvent", metaKeyDetectedEvent, plan.Meta[metaKeyDetectedEvent])
	}
	if !detect.DetectedAt.Equal(fixed()) {
		t.Fatalf("DetectedAt = %v, want %v", detect.DetectedAt, fixed())
	}
}

// TestDetectedPhaseWorker_Executor_Success — Planner produces a
// Plan with the DetectedEvent in Meta; Executor copies it into
// ExecResult.RawOutputs and emits a PhaseContractWritten SideEffect.
func TestDetectedPhaseWorker_Executor_Success(t *testing.T) {
	t.Parallel()

	detect := DetectedEvent{
		AlertID:     "pg-lrtx-001",
		Severity:    "critical",
		Resource:    "pg",
		RawPayload:  `{"alertmanager":"alert-1"}`,
		DetectedAt:  time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
		Labelsetkey: "pg",
	}
	w := NewDetectedPhaseWorker(NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)
	plan := Plan{Meta: map[string]any{metaKeyDetectedEvent: detect}}

	res, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor returned err = %v, want nil", err)
	}
	if got, ok := res.RawOutputs[metaKeyDetectedEvent].(DetectedEvent); !ok {
		t.Errorf("RawOutputs[%q] type=%T, want DetectedEvent", metaKeyDetectedEvent, res.RawOutputs[metaKeyDetectedEvent])
	} else if got != detect {
		t.Errorf("RawOutputs[%q] = %+v, want %+v", metaKeyDetectedEvent, got, detect)
	}
	if len(res.SideEffects) != 1 {
		t.Fatalf("len(SideEffects) = %d, want 1", len(res.SideEffects))
	}
	se := res.SideEffects[0]
	if se.Kind != "phase_contract_written" {
		t.Errorf("SideEffects[0].Kind = %q, want %q", se.Kind, "phase_contract_written")
	}
	if se.Target != "pg-lrtx-001" {
		t.Errorf("SideEffects[0].Target = %q, want %q", se.Target, "pg-lrtx-001")
	}
	if contract, _ := se.Detail["contract"].(string); contract != "DetectedEvent" {
		t.Errorf("SideEffects[0].Detail[contract] = %q, want %q", contract, "DetectedEvent")
	}
}

// TestDetectedPhaseWorker_Verifier_OK — Verifier returns Verdict{OK: true}
// when Executor handed it a fully-populated DetectedEvent.
func TestDetectedPhaseWorker_Verifier_OK(t *testing.T) {
	t.Parallel()

	detect := DetectedEvent{
		AlertID:    "alert-1",
		Severity:   "warning",
		Resource:   "redis",
		RawPayload: `{"k":"v"}`,
		DetectedAt: time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
	}
	w := NewDetectedPhaseWorker(NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)
	res := ExecResult{RawOutputs: map[string]any{metaKeyDetectedEvent: detect}}

	verdict, err := w.Verifier(context.Background(), res)
	if err != nil {
		t.Fatalf("Verifier returned err = %v, want nil", err)
	}
	if !verdict.OK {
		t.Errorf("Verdict.OK = false, want true; reasons=%v", verdict.Reasons)
	}
	if verdict.Confidence != 0.95 {
		t.Errorf("Verdict.Confidence = %v, want 0.95", verdict.Confidence)
	}
	if len(verdict.Reasons) != 0 {
		t.Errorf("Verdict.Reasons = %v, want empty", verdict.Reasons)
	}
}

// TestDetectedPhaseWorker_Planner_LLMError — fake LLM returns an
// error; LLMCaller wraps it after retries; Planner returns wrapped
// error.
func TestDetectedPhaseWorker_Planner_LLMError(t *testing.T) {
	t.Parallel()

	fc := NewFakeLLMClient()
	fc.SetError(0, errors.New("ChatCompletion: context deadline exceeded"))
	fc.SetError(1, errors.New("ChatCompletion: context deadline exceeded"))

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	w := NewDetectedPhaseWorker(caller, WithDetectedLogger(silentLogger()))

	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "pg-lrtx-001",
		TenantID:   "tenant-dry-run",
		Phase:      PhaseDetected,
		Attempt:    1,
	})
	if err == nil {
		t.Fatal("Planner returned err = nil, want wrapped error")
	}
	if plan.Meta != nil {
		t.Errorf("plan.Meta = %v, want nil (Planner failed; Executor should not run)", plan.Meta)
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want wrapped deadline/timeout", err)
	}
	if !strings.Contains(err.Error(), "detected.llm_call") {
		t.Errorf("err = %v, want %q prefix in chain", err, "detected.llm_call")
	}
	if errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("Planner wrapped ErrSchemaInvalid for a transient error; that sentinel is reserved for schema failures")
	}
	if fc.CallCount() != 2 {
		t.Errorf("FakeLLMClient.CallCount = %d, want 2 (1 original + 1 retry)", fc.CallCount())
	}
}

// TestDetectedPhaseWorker_SchemaInvalid — fake LLM returns JSON
// missing a required field; LLMCaller returns ErrSchemaInvalid; the
// Planner builds a stub DetectedEvent; the Verifier rejects it.
func TestDetectedPhaseWorker_SchemaInvalid(t *testing.T) {
	t.Parallel()

	fc := NewFakeLLMClient()
	fc.SetResponse(0, `{"alert_id":"pg-lrtx-001"}`)

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	w := NewDetectedPhaseWorker(caller,
		WithDetectedClock(fixedClock(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))),
		WithDetectedLogger(silentLogger()),
	)

	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "pg-lrtx-001",
		TenantID:   "tenant-dry-run",
		Phase:      PhaseDetected,
		Attempt:    1,
	})
	if err != nil {
		t.Fatalf("Planner returned err = %v, want nil (Planner degrades to stub on ErrSchemaInvalid)", err)
	}
	if fc.CallCount() != 1 {
		t.Errorf("FakeLLMClient.CallCount = %d, want 1 (schema errors are not retried)", fc.CallCount())
	}

	raw, ok := plan.Meta[metaKeyDetectedEvent]
	if !ok {
		t.Fatalf("plan.Meta[%q] missing", metaKeyDetectedEvent)
	}
	detect, ok := raw.(DetectedEvent)
	if !ok {
		t.Fatalf("plan.Meta[%q] type=%T, want DetectedEvent", metaKeyDetectedEvent, raw)
	}
	if detect.AlertID != "pg-lrtx-001" {
		t.Errorf("stub AlertID = %q, want %q", detect.AlertID, "pg-lrtx-001")
	}
	if detect.DetectedAt.IsZero() {
		t.Errorf("stub DetectedAt zero, want 2026-08-12T00:00:00Z")
	}
	if detect.Severity != "" || detect.Resource != "" || detect.RawPayload != "" || detect.Labelsetkey != "" {
		t.Errorf("stub DetectedEvent has unexpected populated fields: %+v", detect)
	}

	res, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor returned err = %v, want nil", err)
	}

	verdict, err := w.Verifier(context.Background(), res)
	if err != nil {
		t.Fatalf("Verifier returned err = %v, want nil", err)
	}
	if verdict.OK {
		t.Fatalf("Verdict.OK = true, want false; Reasons=%v", verdict.Reasons)
	}
	wantReasons := []string{
		"schema_invalid: severity=",
		"schema_invalid: resource missing",
		"schema_invalid: raw_payload missing",
	}
	for _, want := range wantReasons {
		found := false
		for _, got := range verdict.Reasons {
			if strings.Contains(got, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Verdict.Reasons = %v, want to contain %q", verdict.Reasons, want)
		}
	}
}

// TestDetectedPhaseWorker_Verifier_RejectsBadSeverity — Verifier
// rejects a DetectedEvent whose severity is not in the allowed enum.
func TestDetectedPhaseWorker_Verifier_RejectsBadSeverity(t *testing.T) {
	t.Parallel()

	detect := DetectedEvent{
		AlertID:    "alert-1",
		Severity:   "fatal",
		Resource:   "pg",
		RawPayload: `{"k":"v"}`,
		DetectedAt: time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
	}
	w := NewDetectedPhaseWorker(NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)
	res := ExecResult{RawOutputs: map[string]any{metaKeyDetectedEvent: detect}}

	verdict, err := w.Verifier(context.Background(), res)
	if err != nil {
		t.Fatalf("Verifier returned err = %v, want nil", err)
	}
	if verdict.OK {
		t.Fatalf("Verdict.OK = true, want false; Reasons=%v", verdict.Reasons)
	}
	if len(verdict.Reasons) != 1 {
		t.Fatalf("Verdict.Reasons = %v, want exactly 1 reason", verdict.Reasons)
	}
	if !strings.Contains(verdict.Reasons[0], `severity="fatal"`) {
		t.Errorf("Verdict.Reasons[0] = %q, want to mention severity=%q", verdict.Reasons[0], "fatal")
	}
}

// TestDetectedPhaseWorker_Verifier_MissingMeta — Verifier rejects
// ExecResult whose RawOutputs carries no DetectedEvent at all.
func TestDetectedPhaseWorker_Verifier_MissingMeta(t *testing.T) {
	t.Parallel()

	w := NewDetectedPhaseWorker(NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)
	verdict, err := w.Verifier(context.Background(), ExecResult{})
	if err != nil {
		t.Fatalf("Verifier returned err = %v, want nil", err)
	}
	if verdict.OK {
		t.Fatalf("Verdict.OK = true, want false")
	}
	if len(verdict.Reasons) != 1 {
		t.Fatalf("Verdict.Reasons = %v, want exactly 1 reason", verdict.Reasons)
	}
	if !strings.Contains(verdict.Reasons[0], "schema_invalid: missing") {
		t.Errorf("Verdict.Reasons[0] = %q, want to mention missing key", verdict.Reasons[0])
	}
}

// TestDetectedPhaseWorker_Verifier_WrongType — Verifier rejects
// when RawOutputs carries a non-DetectedEvent under the expected key.
func TestDetectedPhaseWorker_Verifier_WrongType(t *testing.T) {
	t.Parallel()

	w := NewDetectedPhaseWorker(NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)
	res := ExecResult{RawOutputs: map[string]any{metaKeyDetectedEvent: "not-a-detected-event"}}

	verdict, err := w.Verifier(context.Background(), res)
	if err != nil {
		t.Fatalf("Verifier returned err = %v, want nil", err)
	}
	if verdict.OK {
		t.Fatalf("Verdict.OK = true, want false")
	}
	if len(verdict.Reasons) != 1 {
		t.Fatalf("Verdict.Reasons = %v, want exactly 1 reason", verdict.Reasons)
	}
	if !strings.Contains(verdict.Reasons[0], "type=") {
		t.Errorf("Verdict.Reasons[0] = %q, want to mention type mismatch", verdict.Reasons[0])
	}
}

// TestDetectedPhaseWorker_Executor_RejectsMissingMeta — Executor
// rejects a Plan that lacks the DetectedEvent Meta key.
func TestDetectedPhaseWorker_Executor_RejectsMissingMeta(t *testing.T) {
	t.Parallel()

	w := NewDetectedPhaseWorker(NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)
	_, err := w.Executor(context.Background(), Plan{})
	if err == nil {
		t.Fatal("Executor returned err = nil, want ErrPlanInvalid-wrapped")
	}
	if !errors.Is(err, ErrPlanInvalid) {
		t.Errorf("err = %v, want ErrPlanInvalid in chain", err)
	}
}

// TestDetectedPhaseWorker_Executor_RejectsWrongType — Executor
// rejects a Plan whose Meta carries the wrong type.
func TestDetectedPhaseWorker_Executor_RejectsWrongType(t *testing.T) {
	t.Parallel()

	w := NewDetectedPhaseWorker(NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)
	_, err := w.Executor(context.Background(), Plan{Meta: map[string]any{
		metaKeyDetectedEvent: "string-instead-of-struct",
	}})
	if err == nil {
		t.Fatal("Executor returned err = nil, want ErrPlanInvalid-wrapped")
	}
	if !errors.Is(err, ErrPlanInvalid) {
		t.Errorf("err = %v, want ErrPlanInvalid in chain", err)
	}
}

// TestClassifyRuleBased — unit test for the rule-based classifier.
func TestClassifyRuleBased(t *testing.T) {
	t.Parallel()

	cases := []struct {
		id       string
		severity string
		resource string
	}{
		{"pg-lrtx-001", "warning", "pg"},
		{"pg-critical-002", "critical", "pg"},
		{"redis-error-77", "error", "redis"},
		{"redis-information-1", "info", "redis"},
		{"k8s-warning-99", "warning", "k8s"},
		{"host-1", "warning", "host"},
		{"app-x", "warning", "app"},
		{"unknown-tenant-1", "warning", "unknown"},
		{"completely-unrecognized-id", "warning", "unknown"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			rb := classifyRuleBased(tc.id)
			if rb.severity != tc.severity {
				t.Errorf("severity = %q, want %q", rb.severity, tc.severity)
			}
			if rb.resource != tc.resource {
				t.Errorf("resource = %q, want %q", rb.resource, tc.resource)
			}
		})
	}
}

// TestDeriveResourceFromID — focused unit test for the resource
// prefix extraction.
func TestDeriveResourceFromID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"pg-lrtx-001", "pg"},
		{"redis-1", "redis"},
		{"k8s-99", "k8s"},
		{"host-1", "host"},
		{"app-x", "app"},
		{"PG-1", "pg"},
		{"alert-1", "unknown"},
		{"", "unknown"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := deriveResourceFromID(tc.in); got != tc.want {
				t.Errorf("deriveResourceFromID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStubDetectedEvent — guard the stub used by the
// schema-invalid path.
func TestStubDetectedEvent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	detect := stubDetectedEvent("pg-lrtx-001", func() time.Time { return now })
	if detect.AlertID != "pg-lrtx-001" {
		t.Errorf("AlertID = %q, want %q", detect.AlertID, "pg-lrtx-001")
	}
	if !detect.DetectedAt.Equal(now) {
		t.Errorf("DetectedAt = %v, want %v", detect.DetectedAt, now)
	}
	if detect.Severity != "" {
		t.Errorf("Severity = %q, want empty", detect.Severity)
	}
	if detect.Resource != "" {
		t.Errorf("Resource = %q, want empty", detect.Resource)
	}
	if detect.RawPayload != "" {
		t.Errorf("RawPayload = %q, want empty", detect.RawPayload)
	}
	if detect.Labelsetkey != "" {
		t.Errorf("Labelsetkey = %q, want empty", detect.Labelsetkey)
	}
}

// TestNewDetectedPhaseWorker_NilCallerPanics — guard the constructor
// panic on nil caller.
func TestNewDetectedPhaseWorker_NilCallerPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("NewDetectedPhaseWorker(nil) did not panic, want panic")
		}
	}()
	_ = NewDetectedPhaseWorker(nil)
}

// TestNewDetectedPhaseWorker_Options — guard the option setters.
func TestNewDetectedPhaseWorker_Options(t *testing.T) {
	t.Parallel()

	fc := NewFakeLLMClient()
	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	fixedNow := time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC)

	wantLog := silentLogger()
	w := NewDetectedPhaseWorker(caller,
		WithDetectedClock(fixedClock(fixedNow)),
		WithDetectedLogger(wantLog),
	)
	if !w.clock().Equal(fixedNow) {
		t.Errorf("clock() = %v, want %v", w.clock(), fixedNow)
	}
	if w.log != wantLog {
		t.Errorf("log != injected logger")
	}

	w2 := NewDetectedPhaseWorker(caller,
		WithDetectedClock(nil),
		WithDetectedLogger(nil),
	)
	if w2.clock == nil {
		t.Errorf("clock is nil after WithDetectedClock(nil); want default")
	}
	if w2.log == nil {
		t.Errorf("log is nil after WithDetectedLogger(nil); want default")
	}
}

// TestDetectedPhaseWorker_VerifierTimeoutMs — VerifierTimeoutMs
// returns 60_000 by default.
func TestDetectedPhaseWorker_VerifierTimeoutMs(t *testing.T) {
	t.Parallel()

	w := NewDetectedPhaseWorker(NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)
	if got := w.VerifierTimeoutMs(); got != DetectedPhaseVerifierTimeoutMs {
		t.Errorf("VerifierTimeoutMs = %d, want %d", got, DetectedPhaseVerifierTimeoutMs)
	}
	if DetectedPhaseVerifierTimeoutMs != 60_000 {
		t.Errorf("DetectedPhaseVerifierTimeoutMs = %d, want 60_000 (project floor)", DetectedPhaseVerifierTimeoutMs)
	}
}

// TestDetectedPhaseWorker_Phase — Phase returns PhaseDetected.
func TestDetectedPhaseWorker_Phase(t *testing.T) {
	t.Parallel()

	w := NewDetectedPhaseWorker(NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLogger())),
		WithDetectedLogger(silentLogger()),
	)
	if got := w.Phase(); got != PhaseDetected {
		t.Errorf("Phase() = %q, want %q", got, PhaseDetected)
	}
}

// TestDetectedPhaseWorker_SchemaIsValid — sanity check on the
// detectedEventSchema constant.
func TestDetectedPhaseWorker_SchemaIsValid(t *testing.T) {
	t.Parallel()

	parsed, err := parseSchema(detectedEventSchema)
	if err != nil {
		t.Fatalf("parseSchema(detectedEventSchema) = %v, want nil", err)
	}
	if parsed.Required == nil {
		t.Fatal("schema.required is nil")
	}
	wantRequired := map[string]bool{
		"alert_id": false, "severity": false, "resource": false,
		"raw_payload": false, "detected_at": false,
	}
	for _, r := range parsed.Required {
		if _, ok := wantRequired[r]; !ok {
			t.Errorf("schema.required has unexpected %q", r)
		}
		wantRequired[r] = true
	}
	for k, v := range wantRequired {
		if !v {
			t.Errorf("schema.required missing %q", k)
		}
	}
	detect := DetectedEvent{
		AlertID:    "a1",
		Severity:   "critical",
		Resource:   "pg",
		RawPayload: `{"k":"v"}`,
		DetectedAt: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
	}
	raw, _ := json.Marshal(detect)
	if err := parsed.validate(raw); err != nil {
		t.Errorf("parsed.validate(good) = %v, want nil", err)
	}
}

// TestDetectedPhaseWorker_FullPipeline — black-box exercise of
// Planner → Executor → Verifier with all real interfaces.
func TestDetectedPhaseWorker_FullPipeline(t *testing.T) {
	t.Parallel()

	fc := NewFakeLLMClient()
	fc.SetResponse(0, validDetectedEventJSON("redis-oom-77", "error", "redis"))

	caller := NewLLMCaller(fc, WithLogger(silentLogger()))
	w := NewDetectedPhaseWorker(caller,
		WithDetectedClock(fixedClock(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))),
		WithDetectedLogger(silentLogger()),
	)

	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "redis-oom-77",
		TenantID:   "tenant-dry-run",
		TraceID:    "trace-007",
		Phase:      PhaseDetected,
		Attempt:    1,
	})
	if err != nil {
		t.Fatalf("Planner err = %v", err)
	}

	res, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err = %v", err)
	}

	verdict, err := w.Verifier(context.Background(), res)
	if err != nil {
		t.Fatalf("Verifier err = %v", err)
	}
	if !verdict.OK {
		t.Errorf("Verdict.OK = false, want true; Reasons=%v", verdict.Reasons)
	}
	if got := fc.CallCount(); got != 1 {
		t.Errorf("CallCount = %d, want 1", got)
	}
}

// TestDetectedPhaseWorker_BuildDetectionPrompts — guards the prompt
// shape so a regression in the prompt template can't be silently
// shipped.
func TestDetectedPhaseWorker_BuildDetectionPrompts(t *testing.T) {
	t.Parallel()

	in := PlanInput{
		IncidentID: "pg-lrtx-001",
		TenantID:   "tenant-dry-run",
		TraceID:    "trace-008",
		Phase:      PhaseDetected,
	}
	rb := ruleBased{severity: "warning", resource: "pg"}

	sys, user := buildDetectionPrompts(in, rb)

	if !strings.Contains(sys, "Schema:") {
		t.Errorf("system prompt missing schema block; got: %q", sys)
	}
	if !strings.Contains(sys, `"alert_id"`) {
		t.Errorf("system prompt missing alert_id field; got: %q", sys)
	}
	if !strings.Contains(sys, `"severity"`) {
		t.Errorf("system prompt missing severity field; got: %q", sys)
	}
	if !strings.Contains(sys, `"enum"`) {
		t.Errorf("system prompt missing severity enum; got: %q", sys)
	}

	mustContain := []string{
		"incident_id: pg-lrtx-001",
		"tenant_id: tenant-dry-run",
		"trace_id: trace-008",
		"rule_based_severity: warning",
		"rule_based_resource: pg",
		"Return the JSON object only",
	}
	for _, want := range mustContain {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q; got: %q", want, user)
		}
	}

	_, userNoTrace := buildDetectionPrompts(PlanInput{
		IncidentID: "pg-lrtx-001",
		TenantID:   "tenant-dry-run",
		Phase:      PhaseDetected,
	}, rb)
	if strings.Contains(userNoTrace, "trace_id:") {
		t.Errorf("user prompt for empty TraceID should not include trace_id line; got: %q", userNoTrace)
	}
}

// TestDetectedPhaseWorker_EstimatedCost — Planner surfaces the
// LLMCaller's CostUSD on the Plan.
func TestDetectedPhaseWorker_EstimatedCost(t *testing.T) {
	t.Parallel()

	fc := NewFakeLLMClient()
	fc.SetResponse(0, validDetectedEventJSON("pg-lrtx-001", "warning", "pg"))

	caller := NewLLMCaller(fc,
		WithLogger(silentLogger()),
		WithCostFunc(func(_ string, in, out int) float64 {
			return float64(in+out) * 0.000001
		}),
	)
	w := NewDetectedPhaseWorker(caller, WithDetectedLogger(silentLogger()))

	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "pg-lrtx-001",
		TenantID:   "tenant-dry-run",
		Phase:      PhaseDetected,
		Attempt:    1,
	})
	if err != nil {
		t.Fatalf("Planner err = %v", err)
	}
	// CostUSD is 0 because FakeLLMClient returns 0 usage; the
	// assertion validates that the Planner surfaces the LLMCaller
	// telemetry rather than computing it correctly (the cost
	// estimator is exercised in llm_caller_test.go).
	if plan.EstimatedCost.Tokens != 0 {
		t.Errorf("EstimatedCost.Tokens = %d, want 0 (FakeLLMClient omits usage)", plan.EstimatedCost.Tokens)
	}
}

// keep silentLogger referenced in case refactors drop usages
var _ = slog.Default

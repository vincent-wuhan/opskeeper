// Package loop — approved_worker_test.go
//
// approved phase LLM-driven Worker 路径 A 集成批次 3 的覆盖：
//
//  1. actionability < 0.5（dangerous tier）→ Plan.Meta
//     severity=dangerous + PauseHook.Evaluate 收到 dangerous
//     + pauseRequired=true → Verifier OK=false / pause_required
//  2. actionability ∈ [0.5, 0.8)（mutating tier）→ severity=mutating
//     + PauseHook 收到 mutating + pauseRequired=false → advance
//  3. actionability ≥ 0.8（safe tier）→ severity=safe + PauseHook
//     收到 safe + pauseRequired=false → advance
//  4. critique 加载失败（fake loader 返回 error）→ Planner log warn
//     + 退化 v1（severity="safe"，PauseHook 仍调但不带 LLM 语义）
//  5. critiqueLoader == nil（构造时不注入）→ Planner 退化 v1
//  6. severityFromActionability 边界值（-0.1, 0.5, 0.8, 1.0）
//  7. Verifier 对未知 decision 返回 OK=false / unknown_decision
//  8. NewApprovedPhaseWorker 默认值（pauseHook nil → NoopPauseHook
//     / clock nil → time.Now / log nil → slog.Default）
package loop

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// silentApprovedLogger 把 slog 输出丢到 io.Discard，让单测输出干净。
func silentApprovedLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// capturingApprovedLogger 把所有 WARN/ERROR 日志收集到 buffer，测试可断言内容。
// INFO 级默认丢到 io.Discard，避免污染 -v 跑出来的输出。
func capturingApprovedLogger(buf *bytes.Buffer) *slog.Logger {
	level := slog.LevelInfo
	return slog.New(slog.NewTextHandler(&writeTee{writers: []io.Writer{io.Discard, buf}, min: &level}, nil))
}

type writeTee struct {
	mu      sync.Mutex
	writers []io.Writer
	min     *slog.Level
}

func (t *writeTee) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, w := range t.writers {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// fakeApprovedCritiqueLoader 是 ApprovedCritiqueLoader 的内存 fake。
//   - err != nil → 短路返回 err（模拟 load 失败）
//   - dims != nil → 返回 dims
//   - 都不设 → 返回 (nil, nil)（模拟 upstream critique 缺失）
type fakeApprovedCritiqueLoader struct {
	mu    sync.Mutex
	dims  *CritiqueDimensions
	err   error
	calls int
}

func (f *fakeApprovedCritiqueLoader) LoadCritiqueDimensions(_ context.Context, _ string) (*CritiqueDimensions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.dims, nil
}

func (f *fakeApprovedCritiqueLoader) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// recordingPauseHook 记录每次 Evaluate 收到的 PauseInput，按构造时的
// pauseRequired / token / err 返回。
type recordingPauseHook struct {
	mu            sync.Mutex
	inputs        []PauseInput
	pauseRequired bool
	token         string
	err           error
}

func (h *recordingPauseHook) Evaluate(_ context.Context, in PauseInput) (bool, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inputs = append(h.inputs, in)
	return h.pauseRequired, h.token, h.err
}

func (h *recordingPauseHook) LastInput(t *testing.T) PauseInput {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.inputs) == 0 {
		t.Fatalf("recordingPauseHook: no calls recorded")
	}
	return h.inputs[len(h.inputs)-1]
}

func (h *recordingPauseHook) CallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.inputs)
}

func newApprovedPlanInput() PlanInput {
	return PlanInput{
		IncidentID: "inc-test",
		TenantID:   "tenant-test",
		Phase:      PhaseApproved,
		Attempt:    1,
	}
}

func mustApprovedWorker(t *testing.T, hook PauseHook, loader ApprovedCritiqueLoader, log *slog.Logger) *ApprovedPhaseWorker {
	t.Helper()
	w, err := NewApprovedPhaseWorker(
		hook,
		nil, // clock: use default
		log,
		WithApprovedCritiqueLoader(loader),
	)
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	return w
}

// ============================================================================
// 路径 1：actionability < 0.5 → dangerous → PauseHook 收到 dangerous
//                                     → pauseRequired=true → pause
// ============================================================================

func TestApprovedWorker_DangerousPauses(t *testing.T) {
	hook := &recordingPauseHook{pauseRequired: true, token: "deadbeefcafebabe1234567890abcdef"}
	loader := &fakeApprovedCritiqueLoader{
		dims: &CritiqueDimensions{
			Accuracy:      0.9,
			Completeness:  0.8,
			Actionability: 0.3,
		},
	}
	w := mustApprovedWorker(t, hook, loader, silentApprovedLogger())

	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}

	if got := plan.Meta[approvedMetaSeverity]; got != "dangerous" {
		t.Fatalf("Plan.Meta[%q] = %v, want %q", approvedMetaSeverity, got, "dangerous")
	}
	if got := plan.Meta[approvedMetaActionability]; got != 0.3 {
		t.Fatalf("Plan.Meta[%q] = %v, want 0.3", approvedMetaActionability, got)
	}
	if got := plan.Steps[0].Args["severity"]; got != "dangerous" {
		t.Fatalf("plan Steps[0].Args[severity] = %v, want dangerous", got)
	}
	if loader.Calls() != 1 {
		t.Fatalf("critiqueLoader calls = %d, want 1", loader.Calls())
	}

	execRes, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v", err)
	}

	// PauseHook 收到 severity=dangerous。
	lastIn := hook.LastInput(t)
	if lastIn.Severity != "dangerous" {
		t.Errorf("PauseInput.Severity = %q, want %q", lastIn.Severity, "dangerous")
	}
	if lastIn.Phase != PhaseApproved {
		t.Errorf("PauseInput.Phase = %v, want %v", lastIn.Phase, PhaseApproved)
	}
	if lastIn.Actor == "" {
		t.Errorf("PauseInput.Actor should be non-empty")
	}
	if hook.CallCount() != 1 {
		t.Fatalf("PauseHook calls = %d, want 1", hook.CallCount())
	}

	// Executor 输出 pause_required=true + pause_token。
	if pr, _ := execRes.RawOutputs[approvedMetaPauseRequired].(bool); !pr {
		t.Errorf("RawOutputs[%q] = %v, want true", approvedMetaPauseRequired, execRes.RawOutputs[approvedMetaPauseRequired])
	}
	if tok, _ := execRes.RawOutputs["pause_token"].(string); tok != "deadbeefcafebabe1234567890abcdef" {
		t.Errorf("RawOutputs[\"pause_token\"] = %q, want deadbeef...", tok)
	}
	if dec, _ := execRes.RawOutputs[ApprovedDecisionRawKey].(string); dec != string(ApprovedExecPause) {
		t.Errorf("RawOutputs[%q] = %q, want %q", ApprovedDecisionRawKey, dec, ApprovedExecPause)
	}
	if len(execRes.SideEffects) != 1 || execRes.SideEffects[0].Target != ApprovedPauseTokenField {
		t.Errorf("SideEffects[0].Target = %q, want %q", sideEffectOrEmpty(execRes), ApprovedPauseTokenField)
	}

	// Verifier 给出 pause_required 拒绝。
	verdict, err := w.Verifier(context.Background(), execRes)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if verdict.OK {
		t.Errorf("Verifier.OK = true, want false (pause_required)")
	}
	if len(verdict.Reasons) != 1 || verdict.Reasons[0] != "pause_required" {
		t.Errorf("Verifier.Reasons = %v, want [pause_required]", verdict.Reasons)
	}
}

// ============================================================================
// 路径 2：actionability ∈ [0.5, 0.8) → mutating → pauseRequired=false
//                                    → advance
// ============================================================================

func TestApprovedWorker_MutatingAdvances(t *testing.T) {
	hook := &recordingPauseHook{pauseRequired: false}
	loader := &fakeApprovedCritiqueLoader{
		dims: &CritiqueDimensions{
			Accuracy:      0.85,
			Completeness:  0.9,
			Actionability: 0.65,
		},
	}
	w := mustApprovedWorker(t, hook, loader, silentApprovedLogger())

	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	if got := plan.Meta[approvedMetaSeverity]; got != "mutating" {
		t.Fatalf("Plan.Meta[%q] = %v, want %q", approvedMetaSeverity, got, "mutating")
	}

	execRes, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v", err)
	}
	if lastIn := hook.LastInput(t); lastIn.Severity != "mutating" {
		t.Errorf("PauseInput.Severity = %q, want %q", lastIn.Severity, "mutating")
	}
	if pr, _ := execRes.RawOutputs[approvedMetaPauseRequired].(bool); pr {
		t.Errorf("RawOutputs[%q] = true, want false", approvedMetaPauseRequired)
	}
	if dec, _ := execRes.RawOutputs[ApprovedDecisionRawKey].(string); dec != string(ApprovedExecAdvance) {
		t.Errorf("RawOutputs[%q] = %q, want %q", ApprovedDecisionRawKey, dec, ApprovedExecAdvance)
	}
	if len(execRes.SideEffects) != 0 {
		t.Errorf("SideEffects should be empty for advance, got %d", len(execRes.SideEffects))
	}

	verdict, err := w.Verifier(context.Background(), execRes)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if !verdict.OK {
		t.Errorf("Verifier.OK = false, want true (advance)")
	}
}

// ============================================================================
// 路径 3：actionability ≥ 0.8 → safe → pauseRequired=false → advance
// ============================================================================

func TestApprovedWorker_SafeAdvances(t *testing.T) {
	hook := &recordingPauseHook{pauseRequired: false}
	loader := &fakeApprovedCritiqueLoader{
		dims: &CritiqueDimensions{
			Accuracy:      0.95,
			Completeness:  0.92,
			Actionability: 0.95,
		},
	}
	w := mustApprovedWorker(t, hook, loader, silentApprovedLogger())

	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	if got := plan.Meta[approvedMetaSeverity]; got != "safe" {
		t.Fatalf("Plan.Meta[%q] = %v, want %q", approvedMetaSeverity, got, "safe")
	}

	execRes, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v", err)
	}
	if lastIn := hook.LastInput(t); lastIn.Severity != "safe" {
		t.Errorf("PauseInput.Severity = %q, want %q", lastIn.Severity, "safe")
	}
	if dec, _ := execRes.RawOutputs[ApprovedDecisionRawKey].(string); dec != string(ApprovedExecAdvance) {
		t.Errorf("RawOutputs[%q] = %q, want %q", ApprovedDecisionRawKey, dec, ApprovedExecAdvance)
	}
	verdict, err := w.Verifier(context.Background(), execRes)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if !verdict.OK {
		t.Errorf("Verifier.OK = false, want true (advance)")
	}
}

// ============================================================================
// 路径 4：critique 加载失败 → Planner log warn + 退化 v1
// ============================================================================

func TestApprovedWorker_CritiqueLoadErrorFallsBack(t *testing.T) {
	hook := &recordingPauseHook{pauseRequired: false}
	loader := &fakeApprovedCritiqueLoader{err: errors.New("upstream timeout")}
	var logBuf bytes.Buffer
	log := capturingApprovedLogger(&logBuf)

	w := mustApprovedWorker(t, hook, loader, log)

	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}

	// 退化 v1：severity="safe"。
	if got := plan.Meta[approvedMetaSeverity]; got != "safe" {
		t.Fatalf("Plan.Meta[%q] = %v, want %q (v1 fallback)", approvedMetaSeverity, got, "safe")
	}
	if _, has := plan.Meta[approvedMetaActionability]; has {
		t.Errorf("Plan.Meta should NOT include %q on fallback", approvedMetaActionability)
	}

	// Planner 写了一条 WARN 日志。
	if !strings.Contains(logBuf.String(), "approved planner") || !strings.Contains(logBuf.String(), "fallback") {
		t.Errorf("expected WARN log mentioning 'approved planner' + 'fallback', got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "upstream timeout") {
		t.Errorf("expected WARN log to include err 'upstream timeout', got: %q", logBuf.String())
	}

	// Executor 仍调 PauseHook，但只拿默认 severity=safe。
	execRes, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v", err)
	}
	if lastIn := hook.LastInput(t); lastIn.Severity != "safe" {
		t.Errorf("PauseInput.Severity = %q, want %q (v1 default)", lastIn.Severity, "safe")
	}
	if hook.CallCount() != 1 {
		t.Errorf("PauseHook calls = %d, want 1", hook.CallCount())
	}
	if dec, _ := execRes.RawOutputs[ApprovedDecisionRawKey].(string); dec != string(ApprovedExecAdvance) {
		t.Errorf("RawOutputs[%q] = %q, want advance", ApprovedDecisionRawKey, dec)
	}
}

// ============================================================================
// Bonus 1：critiqueLoader == nil（构造时未注入）→ 退化 v1，零 IO
// ============================================================================

func TestApprovedWorker_NilCritiqueLoaderFallsBack(t *testing.T) {
	hook := &recordingPauseHook{pauseRequired: false}
	// 不传 WithApprovedCritiqueLoader：内部默认 NoopApprovedCritiqueLoader，
	// Noop 立即返回 (nil, nil)，Planner 退化 v1。
	w, err := NewApprovedPhaseWorker(hook, nil, silentApprovedLogger())
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	if got := plan.Meta[approvedMetaSeverity]; got != "safe" {
		t.Errorf("severity = %v, want safe (nil loader → fallback)", got)
	}
}

// ============================================================================
// Bonus 2：severityFromActionability 边界 + 越界值
// ============================================================================

func TestSeverityFromActionability(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want string
	}{
		{"negative_clamped_to_dangerous", -0.1, "dangerous"},
		{"zero_dangerous", 0.0, "dangerous"},
		{"just_below_dangerous_threshold", 0.4999, "dangerous"},
		{"dangerous_threshold_mutating", 0.5, "mutating"},
		{"mid_mutating", 0.65, "mutating"},
		{"just_below_safe_threshold", 0.7999, "mutating"},
		{"safe_threshold_safe", 0.8, "safe"},
		{"high_safe", 0.95, "safe"},
		{"over_one_clamped_to_safe", 1.5, "safe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := severityFromActionability(c.in); got != c.want {
				t.Errorf("severityFromActionability(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// ============================================================================
// Bonus 3：PauseHook.Evaluate 错误 → Executor wrap 错误
// ============================================================================

func TestApprovedWorker_ExecutorPauseHookError(t *testing.T) {
	hook := &recordingPauseHook{err: errors.New("hitl coordinator down")}
	loader := &fakeApprovedCritiqueLoader{
		dims: &CritiqueDimensions{Actionability: 0.3},
	}
	w := mustApprovedWorker(t, hook, loader, silentApprovedLogger())
	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	_, err = w.Executor(context.Background(), plan)
	if err == nil {
		t.Fatalf("Executor err = nil, want wrap")
	}
	if !strings.Contains(err.Error(), "approved pause hook") {
		t.Errorf("Executor err = %v, want wrap containing 'approved pause hook'", err)
	}
	if !errors.Is(err, err) { // sanity: errors.Is 不因自己而出错
		_ = err
	}
}

// ============================================================================
// Bonus 4：PauseHook 返回 pauseRequired=true 但空 token → Executor mint
// ============================================================================

func TestApprovedWorker_ExecutorMintsPauseTokenWhenMissing(t *testing.T) {
	hook := &recordingPauseHook{pauseRequired: true, token: ""}
	loader := &fakeApprovedCritiqueLoader{
		dims: &CritiqueDimensions{Actionability: 0.1},
	}
	w := mustApprovedWorker(t, hook, loader, silentApprovedLogger())
	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	execRes, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v", err)
	}
	tok, _ := execRes.RawOutputs["pause_token"].(string)
	if tok == "" {
		t.Errorf("pause_token should be minted, got empty")
	}
	if len(tok) != 64 { // 32-byte hex
		t.Errorf("pause_token len = %d, want 64 (32-byte hex)", len(tok))
	}
}

// ============================================================================
// Bonus 5：Verifier 收到未知 decision → OK=false / unknown_decision
// ============================================================================

func TestApprovedWorker_VerifierUnknownDecision(t *testing.T) {
	w, err := NewApprovedPhaseWorker(&recordingPauseHook{}, nil, silentApprovedLogger())
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	res := ExecResult{RawOutputs: map[string]any{ApprovedDecisionRawKey: "wat"}}
	verdict, err := w.Verifier(context.Background(), res)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if verdict.OK {
		t.Errorf("Verifier.OK = true, want false (unknown decision)")
	}
	if len(verdict.Reasons) != 1 || verdict.Reasons[0] != "unknown_decision" {
		t.Errorf("Reasons = %v, want [unknown_decision]", verdict.Reasons)
	}
}

// ============================================================================
// Bonus 6：构造器默认值（pauseHook / clock / log 全 nil）
// ============================================================================

func TestNewApprovedPhaseWorker_Defaults(t *testing.T) {
	w, err := NewApprovedPhaseWorker(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	if w.pauseHook == nil {
		t.Errorf("pauseHook default = nil, want NoopPauseHook")
	}
	if w.critiqueLoader == nil {
		t.Errorf("critiqueLoader default = nil, want NoopApprovedCritiqueLoader")
	}
	if w.clock == nil {
		t.Errorf("clock default = nil")
	}
	if w.log == nil {
		t.Errorf("log default = nil")
	}
}

// sideEffectOrEmpty 用来在断言信息里打印 SideEffects[0] Target 的内容。
func sideEffectOrEmpty(r ExecResult) string {
	if len(r.SideEffects) == 0 {
		return ""
	}
	return r.SideEffects[0].Target
}

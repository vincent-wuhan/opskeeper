// Package loop — recovery_test.go
//
// 测试覆盖（zero-manual-ops-loop Day 3 任务 3.3 + 3.4）：
//
//	PhaseRecovered Worker
//	  1. Planner：上游 ApprovalDecision → PlanStep verify_recovery 调用
//	  2. Executor：调用 verifyCaller 并解码 VerifiedDelta
//	  3. Verifier：passed=true → Verdict.OK=true
//	  4. Verifier：passed=false → Verdict.OK=false + Reasons 包含 failed_metrics
//	  5. Verifier：retry_count > MaxRetryCount → Reasons 含 severity escalation
//	  6. HandleRollback：increment retry_count + 超上限升级
//	  7. retry_count 上限（boundary 3 → 4）
//	  8. 并发安全：HandleRollback 并发调 32 次后计数正确
//	  9. ApprovedDecisionLoader nil → Planner 拒绝
//	 10. VerifyRecoveryCaller 返回 malformed → Executor 拒绝
//
//	DefaultMetrics 子集
//	 11. defaultMetricsForResource 5 类资源各返回正确子集
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVerifyCaller 是 VerifyRecoveryCaller 的内存 fake；返回装好的 JSON。
type fakeVerifyCaller struct {
	mu       sync.Mutex
	response string
	err      error
	calls    atomic.Int32
	lastArgs string
}

func (f *fakeVerifyCaller) InvokeVerifyRecovery(_ context.Context, argsJSON string) (string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastArgs = argsJSON
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

func (f *fakeVerifyCaller) callCount() int { return int(f.calls.Load()) }

// fakeApprovalLoader 是 ApprovedDecisionLoader 的内存 fake。
type fakeApprovalLoader struct {
	mu       sync.Mutex
	decision *ApprovalDecision
	err      error
}

func (f *fakeApprovalLoader) LoadApprovedDecision(_ context.Context, _ string, _ int64) (*ApprovalDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.decision, nil
}

// inMemoryStateStore 是 RecoveryStateStore 的最小内存实现（loop 包内）。
type inMemoryStateStore struct {
	mu     sync.Mutex
	counts map[string]int
}

func newInMemoryStateStore() *inMemoryStateStore {
	return &inMemoryStateStore{counts: make(map[string]int)}
}

func (s *inMemoryStateStore) Get(_ context.Context, id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[id], nil
}

func (s *inMemoryStateStore) Increment(_ context.Context, id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[id]++
	return s.counts[id], nil
}

func (s *inMemoryStateStore) Reset(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counts, id)
	return nil
}

// newRecoveredWorkerFor 测试 helper：返回装好依赖的 RecoveredPhaseWorker。
func newRecoveredWorkerFor(t *testing.T, caller VerifyRecoveryCaller, store RecoveryStateStore, loader ApprovedDecisionLoader) *RecoveredPhaseWorker {
	t.Helper()
	w, err := NewRecoveredPhaseWorker(caller, store, loader, slog.Default())
	if err != nil {
		t.Fatalf("NewRecoveredPhaseWorker err: %v", err)
	}
	return w
}

// makeVerifiedJSON 把 VerifiedDelta 编码成 JSON 字符串（fakeCaller 的 response）。
func makeVerifiedJSON(t *testing.T, vd VerifiedDelta) string {
	t.Helper()
	raw, err := json.Marshal(vd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// --- Test 1: NewRecoveredPhaseWorker requires all deps -----------------

// TestNewRecoveredPhaseWorker_RequiresDeps 验证 3 个必填依赖为 nil 时
// 构造器拒绝（与 chatdiagnose service 同样的"fail at wire-up"原则）。
func TestNewRecoveredPhaseWorker_RequiresDeps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		caller           VerifyRecoveryCaller
		store            RecoveryStateStore
		loader           ApprovedDecisionLoader
		wantErrSubstring string
	}{
		{"nil caller", nil, newInMemoryStateStore(), &fakeApprovalLoader{}, "verifyCaller"},
		{"nil store", &fakeVerifyCaller{}, nil, &fakeApprovalLoader{}, "stateStore"},
		{"nil loader", &fakeVerifyCaller{}, newInMemoryStateStore(), nil, "approvedRefLoader"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRecoveredPhaseWorker(tc.caller, tc.store, tc.loader, slog.Default())
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Errorf("error should mention %q; got %v", tc.wantErrSubstring, err)
			}
		})
	}
}

// --- Test 2: Planner builds correct Plan --------------------------------

// TestRecoveredWorker_Planner_BuildsPlan：上游 ApprovalDecision
// {target=host-1, resource_type=host, metrics=[cpu_usage,mem_usage]}
// 应当被翻译成 PlanStep{Target: verify_recovery, Args: ...}。
func TestRecoveredWorker_Planner_BuildsPlan(t *testing.T) {
	t.Parallel()
	loader := &fakeApprovalLoader{
		decision: &ApprovalDecision{
			SchemaVersion: "v1",
			SkillID:       "skill-1",
			Target:        "host-1",
			ResourceType:  "host",
			Tolerance:     0.20,
			VerifyMetrics: []string{"cpu_usage", "mem_usage"},
			ApprovedAt:    time.Now().UTC(),
			ApprovedBy:    "auto",
		},
	}
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), loader)
	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID:       "inc-p",
		TenantID:         "t1",
		Phase:            PhaseRecovered,
		UpstreamContract: &ContractRef{ID: 42},
	})
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("Steps len = %d, want 1", len(plan.Steps))
	}
	step := plan.Steps[0]
	if step.Target != "verify_recovery" {
		t.Errorf("step.Target = %q, want verify_recovery", step.Target)
	}
	if step.Kind != "tool_call" {
		t.Errorf("step.Kind = %q, want tool_call", step.Kind)
	}
	if got := step.Args["target"]; got != "host-1" {
		t.Errorf("args.target = %v, want host-1", got)
	}
	if got := step.Args["resource_type"]; got != "host" {
		t.Errorf("args.resource_type = %v, want host", got)
	}
	if got := step.Args["tolerance"]; got != 0.20 {
		t.Errorf("args.tolerance = %v, want 0.20", got)
	}
	metrics, _ := step.Args["metrics"].([]string)
	if len(metrics) != 2 || metrics[0] != "cpu_usage" || metrics[1] != "mem_usage" {
		t.Errorf("args.metrics = %v, want [cpu_usage mem_usage]", metrics)
	}
	if step.TimeoutMs != RecoveredPhaseVerifierTimeoutMs {
		t.Errorf("step.TimeoutMs = %d, want %d", step.TimeoutMs, RecoveredPhaseVerifierTimeoutMs)
	}
}

// TestRecoveredWorker_Planner_DefaultMetrics：当 ApprovalDecision.VerifyMetrics 为空，
// Planner 退回到 defaultMetricsForResource(ResourceType) 的 default 子集。
func TestRecoveredWorker_Planner_DefaultMetrics(t *testing.T) {
	t.Parallel()
	loader := &fakeApprovalLoader{
		decision: &ApprovalDecision{
			SchemaVersion: "v1",
			SkillID:       "skill-1",
			Target:        "pg-cluster-x",
			ResourceType:  "pg",
			VerifyMetrics: nil, // 空 → 用 default
		},
	}
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), loader)
	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID:       "inc-pg",
		TenantID:         "t1",
		Phase:            PhaseRecovered,
		UpstreamContract: &ContractRef{ID: 1},
	})
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	metrics, _ := plan.Steps[0].Args["metrics"].([]string)
	want := []string{"cpu_usage", "mem_usage", "qps", "latency_p99"}
	if len(metrics) != len(want) {
		t.Fatalf("default metrics len = %d, want %d", len(metrics), len(want))
	}
	for i, m := range want {
		if metrics[i] != m {
			t.Errorf("metrics[%d] = %q, want %q", i, metrics[i], m)
		}
	}
}

// TestRecoveredWorker_Planner_RejectsMissingUpstream：没有 UpstreamContract
// 的 PlanInput 直接拒绝（recovered phase 必须有 ApprovalDecision upstream）。
func TestRecoveredWorker_Planner_RejectsMissingUpstream(t *testing.T) {
	t.Parallel()
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), &fakeApprovalLoader{})
	_, err := w.Planner(context.Background(), PlanInput{
		IncidentID:       "inc-p",
		TenantID:         "t1",
		Phase:            PhaseRecovered,
		UpstreamContract: nil,
	})
	if err == nil {
		t.Fatalf("expected error for missing upstream contract")
	}
	if !errors.Is(err, ErrWorkerNotRegistered) {
		t.Errorf("error chain should contain ErrWorkerNotRegistered, got %v", err)
	}
}

// --- Test 3: Executor calls verifyCaller and decodes -------------------

// TestRecoveredWorker_Executor_Pass：Executor 调 verifyCaller + 解码
// + 填 RawOutputs["verified_delta"]。
func TestRecoveredWorker_Executor_Pass(t *testing.T) {
	t.Parallel()
	caller := &fakeVerifyCaller{
		response: makeVerifiedJSON(t, VerifiedDelta{
			SchemaVersion: ContractSchemaV1,
			Passed:        true,
			Deltas: map[string]float64{
				"cpu_usage": 0.05,
				"mem_usage": 0.10,
			},
			SampleSize:   100,
			Tolerance:    0.15,
			RetryCount:   0,
			WarningLevel: "pass",
		}),
	}
	w := newRecoveredWorkerFor(t, caller, newInMemoryStateStore(), &fakeApprovalLoader{})

	plan := Plan{
		Steps: []PlanStep{{
			Kind:   "tool_call",
			Target: ToolNameVerifyRecovery,
			Args: map[string]any{
				"target":        "host-1",
				"resource_type": "host",
				"metrics":       []string{"cpu_usage", "mem_usage"},
			},
		}},
	}
	res, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v", err)
	}
	if caller.callCount() != 1 {
		t.Errorf("verifyCaller calls = %d, want 1", caller.callCount())
	}
	if len(res.ToolReplay) != 1 || res.ToolReplay[0].Name != ToolNameVerifyRecovery {
		t.Errorf("ToolReplay = %+v, want 1 entry with name verify_recovery", res.ToolReplay)
	}
	if res.RawOutputs["verified_delta"] == nil {
		t.Errorf("RawOutputs missing verified_delta")
	}
	vd, ok := res.RawOutputs["verified_delta"].(*VerifiedDelta)
	if !ok {
		t.Fatalf("verified_delta type=%T", res.RawOutputs["verified_delta"])
	}
	if !vd.Passed {
		t.Errorf("expected Passed=true")
	}
}

// TestRecoveredWorker_Executor_RejectsMalformed：verifyCaller 返回
// 不合法的 JSON 时 Executor 拒绝（error 链含 ErrInvalidSchema）。
func TestRecoveredWorker_Executor_RejectsMalformed(t *testing.T) {
	t.Parallel()
	caller := &fakeVerifyCaller{response: "{not json"}
	w := newRecoveredWorkerFor(t, caller, newInMemoryStateStore(), &fakeApprovalLoader{})
	plan := Plan{Steps: []PlanStep{{Kind: "tool_call", Target: ToolNameVerifyRecovery}}}
	_, err := w.Executor(context.Background(), plan)
	if err == nil {
		t.Fatalf("expected error for malformed JSON")
	}
	if !errors.Is(err, ErrInvalidSchema) {
		t.Errorf("error chain should contain ErrInvalidSchema, got %v", err)
	}
}

// TestRecoveredWorker_Executor_RejectsInvalidSchema：verifyCaller 返回
// 通过 JSON 解码但 contract 校验失败的 payload（tolerance=0 → out of range）。
func TestRecoveredWorker_Executor_RejectsInvalidSchema(t *testing.T) {
	t.Parallel()
	caller := &fakeVerifyCaller{
		response: makeVerifiedJSON(t, VerifiedDelta{
			SchemaVersion: ContractSchemaV1,
			Passed:        false,
			Deltas:        map[string]float64{"cpu_usage": 1.0},
			Tolerance:     0, // 触发 ValidateVerifiedDelta 拒绝
			WarningLevel:  "fail",
		}),
	}
	w := newRecoveredWorkerFor(t, caller, newInMemoryStateStore(), &fakeApprovalLoader{})
	plan := Plan{Steps: []PlanStep{{Kind: "tool_call", Target: ToolNameVerifyRecovery}}}
	_, err := w.Executor(context.Background(), plan)
	if err == nil {
		t.Fatalf("expected error for invalid schema")
	}
	if !errors.Is(err, ErrInvalidSchema) {
		t.Errorf("error chain should contain ErrInvalidSchema, got %v", err)
	}
}

// --- Test 4: Verifier maps Passed=true to OK=true ----------------------

// TestRecoveredWorker_Verifier_Pass：VerifiedDelta.Passed=true →
// Verdict{OK: true, Confidence: vd.Tolerance}。
func TestRecoveredWorker_Verifier_Pass(t *testing.T) {
	t.Parallel()
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), &fakeApprovalLoader{})
	vd := &VerifiedDelta{
		SchemaVersion: ContractSchemaV1,
		Passed:        true,
		Deltas:        map[string]float64{"cpu_usage": 0.05},
		Tolerance:     0.15,
		WarningLevel:  "pass",
	}
	result := ExecResult{RawOutputs: map[string]any{"verified_delta": vd}}
	verdict, err := w.Verifier(context.Background(), result)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if !verdict.OK {
		t.Errorf("expected OK=true, got %+v", verdict)
	}
	if verdict.Confidence != 0.15 {
		t.Errorf("Confidence = %v, want 0.15", verdict.Confidence)
	}
	if len(verdict.Reasons) != 0 {
		t.Errorf("Reasons should be empty on pass; got %v", verdict.Reasons)
	}
}

// --- Test 5: Verifier maps Passed=false to OK=false --------------------

// TestRecoveredWorker_Verifier_Fail：VerifiedDelta.Passed=false +
// failed_metrics=["cpu_usage"] → Verdict{OK: false, Reasons 含 cpu_usage}。
func TestRecoveredWorker_Verifier_Fail(t *testing.T) {
	t.Parallel()
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), &fakeApprovalLoader{})
	vd := &VerifiedDelta{
		SchemaVersion: ContractSchemaV1,
		Passed:        false,
		FailedMetrics: []string{"cpu_usage"},
		Deltas:        map[string]float64{"cpu_usage": 0.50},
		Tolerance:     0.15,
		RetryCount:    1,
		WarningLevel:  "fail",
	}
	result := ExecResult{
		SideEffects: []SideEffect{{Kind: "recovery_verification", Target: "host-1"}},
		RawOutputs:  map[string]any{"verified_delta": vd},
	}
	verdict, err := w.Verifier(context.Background(), result)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if verdict.OK {
		t.Errorf("expected OK=false, got %+v", verdict)
	}
	if len(verdict.Reasons) == 0 {
		t.Fatalf("Reasons should include at least one entry")
	}
	found := false
	for _, r := range verdict.Reasons {
		if strings.Contains(r, "cpu_usage") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Reasons should mention cpu_usage; got %v", verdict.Reasons)
	}
}

// --- Test 6: Verifier triggers severity escalation ---------------------

// TestRecoveredWorker_Verifier_SeverityEscalated：retry_count > MaxRetryCount
// 时 Reasons 包含 severity escalation 字符串。
func TestRecoveredWorker_Verifier_SeverityEscalated(t *testing.T) {
	t.Parallel()
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), &fakeApprovalLoader{})
	vd := &VerifiedDelta{
		SchemaVersion: ContractSchemaV1,
		Passed:        false,
		FailedMetrics: []string{"cpu_usage"},
		Deltas:        map[string]float64{"cpu_usage": 0.50},
		Tolerance:     0.15,
		RetryCount:    MaxRetryCount + 1, // 4 > 3
		WarningLevel:  "fail",
	}
	result := ExecResult{
		SideEffects: []SideEffect{{Kind: "recovery_verification", Target: "host-1"}},
		RawOutputs:  map[string]any{"verified_delta": vd},
	}
	verdict, err := w.Verifier(context.Background(), result)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if verdict.OK {
		t.Errorf("expected OK=false on escalated failure")
	}
	hasEscalation := false
	for _, r := range verdict.Reasons {
		if strings.Contains(r, "severity escalated to dangerous") {
			hasEscalation = true
			break
		}
	}
	if !hasEscalation {
		t.Errorf("Reasons should include severity escalation; got %v", verdict.Reasons)
	}
}

// --- Test 7: HandleRollback increments and escalates -------------------

// TestRecoveredWorker_HandleRollback_IncrementsAndEscalates：第一次
// increment → retry_count=1 → 不升级；第 4 次 → retry_count=4 → 升级。
func TestRecoveredWorker_HandleRollback_IncrementsAndEscalates(t *testing.T) {
	t.Parallel()
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), &fakeApprovalLoader{})
	ctx := context.Background()
	for i := 1; i <= MaxRetryCount; i++ {
		n, esc, err := w.HandleRollback(ctx, "inc-hr")
		if err != nil {
			t.Fatalf("HandleRollback #%d err: %v", i, err)
		}
		if n != i {
			t.Errorf("retry_count #%d = %d, want %d", i, n, i)
		}
		if esc {
			t.Errorf("retry_count=%d should not escalate (boundary MaxRetryCount=%d)", i, MaxRetryCount)
		}
	}
	n, esc, err := w.HandleRollback(ctx, "inc-hr")
	if err != nil {
		t.Fatalf("HandleRollback final err: %v", err)
	}
	if n != MaxRetryCount+1 {
		t.Errorf("retry_count = %d, want %d", n, MaxRetryCount+1)
	}
	if !esc {
		t.Errorf("retry_count > MaxRetryCount should escalate")
	}
	// EscalationSnapshot 应包含该 incident。
	snaps := w.EscalationSnapshots()
	found := false
	for _, s := range snaps {
		if s.IncidentID == "inc-hr" && s.RetryCount == MaxRetryCount+1 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("EscalationSnapshots missing inc-hr entry: %+v", snaps)
	}
}

// --- Test 8: HandleRollback concurrent safety --------------------------

// TestRecoveredWorker_HandleRollback_Concurrent：32 个并发
// HandleRollback 后 retry_count 必须正好是 32（StateStore 锁有效）。
func TestRecoveredWorker_HandleRollback_Concurrent(t *testing.T) {
	t.Parallel()
	store := newInMemoryStateStore()
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, store, &fakeApprovalLoader{})
	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _, err := w.HandleRollback(context.Background(), "inc-conc")
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent err: %v", e)
	}
	got, _ := store.Get(context.Background(), "inc-conc")
	if got != N {
		t.Errorf("retry_count = %d, want %d", got, N)
	}
}

// --- Test 9: PhaseWorker registration --------------------------------

// TestRecoveredPhaseWorker_PhaseAndTimeout：PhaseWorker 接口的
// Phase() / VerifierTimeoutMs() 正确。
func TestRecoveredPhaseWorker_PhaseAndTimeout(t *testing.T) {
	t.Parallel()
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), &fakeApprovalLoader{})
	if w.Phase() != PhaseRecovered {
		t.Errorf("Phase = %s, want recovered", w.Phase())
	}
	if w.VerifierTimeoutMs() != RecoveredPhaseVerifierTimeoutMs {
		t.Errorf("VerifierTimeoutMs = %d, want %d", w.VerifierTimeoutMs(), RecoveredPhaseVerifierTimeoutMs)
	}
}

// TestRecoveredPhaseWorker_RegistryRoundTrip：注册 → 查表 → 注销。
// 与 orchestrator_test.go::TestPhaseWorkerRegistry 同样的语义。
func TestRecoveredPhaseWorker_RegistryRoundTrip(t *testing.T) {
	t.Parallel()
	registryMu.Lock()
	prev, had := DefaultPhaseWorkerRegistry[PhaseRecovered]
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		if had {
			DefaultPhaseWorkerRegistry[PhaseRecovered] = prev
		} else {
			delete(DefaultPhaseWorkerRegistry, PhaseRecovered)
		}
	})

	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), &fakeApprovalLoader{})
	RegisterPhaseWorker(w)
	got, err := LookupPhaseWorker(PhaseRecovered)
	if err != nil {
		t.Fatalf("Lookup err: %v", err)
	}
	if got.Phase() != PhaseRecovered {
		t.Errorf("Lookup returned wrong phase: %s", got.Phase())
	}
	UnregisterPhaseWorker(PhaseRecovered)
	if _, err := LookupPhaseWorker(PhaseRecovered); !errors.Is(err, ErrWorkerNotRegistered) {
		t.Errorf("after Unregister should return ErrWorkerNotRegistered, got %v", err)
	}
}

// --- Test 10: DefaultMetricsForResource --------------------------------

// TestDefaultMetricsForResource：5 类资源各返回正确子集，未知类型返回 nil。
func TestDefaultMetricsForResource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		resource string
		want     []string
	}{
		{"host", []string{"cpu_usage", "mem_usage"}},
		{"pg", []string{"cpu_usage", "mem_usage", "qps", "latency_p99"}},
		{"redis", []string{"mem_usage", "qps", "latency_p99"}},
		{"k8s", []string{"cpu_usage", "mem_usage"}},
		{"app", []string{"qps", "latency_p99"}},
		{"unknown", nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("resource=%s", tc.resource), func(t *testing.T) {
			got := defaultMetricsForResource(tc.resource)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i, m := range tc.want {
				if got[i] != m {
					t.Errorf("metrics[%d] = %q, want %q", i, got[i], m)
				}
			}
		})
	}
}

// --- Test 11: Verifier rejects missing raw_outputs ---------------------

// TestRecoveredWorker_Verifier_MissingRawOutputs：result.RawOutputs
// 没有 verified_delta 时 Verifier 拒绝。
func TestRecoveredWorker_Verifier_MissingRawOutputs(t *testing.T) {
	t.Parallel()
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, newInMemoryStateStore(), &fakeApprovalLoader{})
	_, err := w.Verifier(context.Background(), ExecResult{})
	if err == nil {
		t.Fatalf("expected error for missing raw_outputs")
	}
	if !errors.Is(err, ErrInvalidSchema) {
		t.Errorf("error chain should contain ErrInvalidSchema, got %v", err)
	}
}

// --- Test 12: ToolNameVerifyRecovery constant --------------------------

// TestToolNameVerifyRecovery_Stable：常量的 wire name 与 aiops/tools
// 包内同名字面量一致（避免 silent rename 引入回归）。
func TestToolNameVerifyRecovery_Stable(t *testing.T) {
	t.Parallel()
	if ToolNameVerifyRecovery != "verify_recovery" {
		t.Errorf("ToolNameVerifyRecovery = %q, want verify_recovery", ToolNameVerifyRecovery)
	}
}

// --- Test 13: HandleRollback snapshot dedup ----------------------------

// TestRecoveredWorker_EscalationCache_Dedup：30s 内的重复升级只
// 记录一次（防 log flood）。
func TestRecoveredWorker_EscalationCache_Dedup(t *testing.T) {
	t.Parallel()
	store := newInMemoryStateStore()
	w := newRecoveredWorkerFor(t, &fakeVerifyCaller{}, store, &fakeApprovalLoader{})
	ctx := context.Background()
	// 预先 increment 到 MaxRetryCount=3，让下一步 HandleRollback → retry_count=4。
	for i := 0; i < MaxRetryCount; i++ {
		if _, err := store.Increment(ctx, "inc-dup"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// 第 4 次 HandleRollback → retry_count=4 → 触发 escalation
	if _, esc, err := w.HandleRollback(ctx, "inc-dup"); err != nil || !esc {
		t.Fatalf("first rollback: esc=%v err=%v", esc, err)
	}
	snapsBefore := len(w.EscalationSnapshots())
	// 第 5 次立刻升级（仍 retry_count > MaxRetryCount），但 30s 内 dedup。
	if _, esc, err := w.HandleRollback(ctx, "inc-dup"); err != nil || !esc {
		t.Fatalf("second rollback: esc=%v err=%v", esc, err)
	}
	snapsAfter := len(w.EscalationSnapshots())
	if snapsAfter != snapsBefore {
		t.Errorf("escalation cache should dedup; before=%d after=%d", snapsBefore, snapsAfter)
	}
	w.ResetEscalationCache()
	if got := len(w.EscalationSnapshots()); got != 0 {
		t.Errorf("after ResetEscalationCache len = %d, want 0", got)
	}
}

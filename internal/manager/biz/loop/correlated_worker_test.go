// Package loop — correlated_worker_test.go
//
// 测试覆盖（path A · llm-worker-integration 批次 2 / subagent 6）：
//
//	CorrelatedPhaseWorker
//	  1. 成功：alertRepo 找到 3 个同 labelsetkey 历史 alert +
//	     LLM 给 valid JSON → CorrelatedGroup 字段断言
//	  2. LLM 失败：FakeLLMClient.SetError → wrapped error
//	  3. schema-invalid：缺 alert_ids 字段 → ErrSchemaInvalid 路径
//	辅助
//	  4. NewCorrelatedPhaseWorker nil 依赖拒绝
//	  5. ValidateCorrelatedGroup 各 invalid 路径
//	  6. VerifierTimeoutMs = 60_000
//	  7. Planner requires currentEventLoader
//	  8. alertRepo error 透传
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAlertRepo 是 AlertRepository 的最小内存 fake。
type fakeAlertRepo struct {
	mu       sync.Mutex
	byKey    map[string][]DetectionEvent
	err      error
	gotKey   string
	gotSince time.Time
}

func newFakeAlertRepo() *fakeAlertRepo {
	return &fakeAlertRepo{byKey: make(map[string][]DetectionEvent)}
}

func (f *fakeAlertRepo) put(key string, evs []DetectionEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byKey[key] = evs
}

func (f *fakeAlertRepo) FindByLabelsetkey(_ context.Context, key string, since time.Time) ([]DetectionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotKey = key
	f.gotSince = since
	if f.err != nil {
		return nil, f.err
	}
	src := f.byKey[key]
	out := make([]DetectionEvent, 0, len(src))
	for _, e := range src {
		if !e.DetectedAt.Before(since) {
			out = append(out, e)
		}
	}
	return out, nil
}

// fakeEventLoader 是 CurrentDetectionEventLoader 的最小内存 fake。
type fakeEventLoader struct {
	mu  sync.Mutex
	ev  DetectionEvent
	err error
}

func (f *fakeEventLoader) Load(_ context.Context, _ PlanInput) (DetectionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return DetectionEvent{}, f.err
	}
	return f.ev, nil
}

// silentLog 把所有 log 输出扔掉。
func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newCorrelatedWorkerFor 测试 helper：返回装好依赖的 CorrelatedPhaseWorker。
func newCorrelatedWorkerFor(t *testing.T, caller LLMCaller, repo AlertRepository, opts ...CorrelatedPhaseWorkerOption) *CorrelatedPhaseWorker {
	t.Helper()
	w, err := NewCorrelatedPhaseWorker(caller, repo, opts...)
	if err != nil {
		t.Fatalf("NewCorrelatedPhaseWorker err: %v", err)
	}
	return w
}

// validCorrelatedJSON 返回满足 CorrelatedGroup schema 的 JSON 字符串。
func validCorrelatedJSON(incidentID string, alertIDs []string, rootHypothesis string, confidence float64) string {
	b, _ := json.Marshal(map[string]any{
		"incident_id":     incidentID,
		"alert_ids":       alertIDs,
		"root_hypothesis": rootHypothesis,
		"confidence":      confidence,
	})
	return string(b)
}

// validDetectionEvent 构造一个测试用 DetectionEvent。
func validDetectionEvent(alertID, labelsetkey string, detectedAt time.Time) DetectionEvent {
	return DetectionEvent{
		AlertID:     alertID,
		Severity:    "error",
		Resource:    "pg",
		LabelSetKey: labelsetkey,
		DetectedAt:  detectedAt,
		Summary:     "long-running tx blocked > 30s",
	}
}

// --- Test 1: NewCorrelatedPhaseWorker requires deps --------------------

// TestNewCorrelatedPhaseWorker_RequiresDeps 验证 2 个必填依赖为 nil 时
// 构造器拒绝（与 RecoveredPhaseWorker 同原则："fail at wire-up"）。
func TestNewCorrelatedPhaseWorker_RequiresDeps(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	caller := NewLLMCaller(fc, WithLogger(silentLog()))
	cases := []struct {
		name             string
		caller           LLMCaller
		repo             AlertRepository
		wantErrSubstring string
	}{
		{"nil caller", nil, newFakeAlertRepo(), "caller"},
		{"nil repo", caller, nil, "alertRepo"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCorrelatedPhaseWorker(tc.caller, tc.repo)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Errorf("error should mention %q; got %v", tc.wantErrSubstring, err)
			}
		})
	}
}

// --- Test 2: success path ----------------------------------------------

// TestCorrelatedWorker_HappyPath 覆盖"成功"路径：
//   - alertRepo 配 3 个同 labelsetkey 历史 alert
//   - LLM 给 valid JSON（含 alert_ids + root_hypothesis + confidence）
//   - Planner 跑通：Plan 1 step，args 含 system_prompt + user_prompt
//   - Executor 跑通：RawOutputs["correlated_group"] 是 *CorrelatedGroup
//     且字段匹配 LLM 输出
//   - Verifier 跑通：OK=true，Confidence 等于 LLM 自报
func TestCorrelatedWorker_HappyPath(t *testing.T) {
	t.Parallel()

	const (
		incidentID   = "inc-corr-1"
		labelsetkey  = "device_id=42"
		currentAlert = "a-current"
		historical1  = "a-hist-1"
		historical2  = "a-hist-2"
		historical3  = "a-hist-3"
	)

	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	repo := newFakeAlertRepo()
	repo.put(labelsetkey, []DetectionEvent{
		validDetectionEvent(historical1, labelsetkey, now.Add(-30*time.Minute)),
		validDetectionEvent(historical2, labelsetkey, now.Add(-15*time.Minute)),
		validDetectionEvent(historical3, labelsetkey, now.Add(-5*time.Minute)),
	})

	fc := NewFakeLLMClient()
	fc.SetResponse(0, validCorrelatedJSON(incidentID,
		[]string{currentAlert, historical1, historical2, historical3},
		"shared root cause: long-running transaction",
		0.92,
	))
	caller := NewLLMCaller(fc, WithLogger(silentLog()))

	current := validDetectionEvent(currentAlert, labelsetkey, now)
	current.Severity = "critical"
	loader := &fakeEventLoader{ev: current}

	w := newCorrelatedWorkerFor(t, caller, repo,
		WithCorrelatedClock(func() time.Time { return now }),
		WithCorrelatedLogger(silentLog()),
		WithCurrentDetectionEventLoader(loader),
	)

	planIn := PlanInput{
		IncidentID: incidentID,
		TenantID:   "t1",
		Phase:      PhaseCorrelated,
		Attempt:    1,
		UpstreamContract: &ContractRef{
			ID:            101,
			Type:          "DetectionEvent",
			SchemaVersion: "v1",
		},
	}

	// --- Planner ---
	plan, err := w.Planner(context.Background(), planIn)
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("Steps len = %d, want 1", len(plan.Steps))
	}
	step := plan.Steps[0]
	if step.Kind != "llm_call" {
		t.Errorf("step.Kind = %q, want llm_call", step.Kind)
	}
	if step.Target != "correlated" {
		t.Errorf("step.Target = %q, want correlated", step.Target)
	}
	if got := step.Args["labelsetkey"]; got != labelsetkey {
		t.Errorf("args.labelsetkey = %v, want %s", got, labelsetkey)
	}
	if got := step.Args["current_alert_id"]; got != currentAlert {
		t.Errorf("args.current_alert_id = %v, want %s", got, currentAlert)
	}
	if got := step.Args["historical_count"]; got != 3 {
		t.Errorf("args.historical_count = %v, want 3", got)
	}
	if sp, _ := step.Args["system_prompt"].(string); !strings.Contains(sp, "alert-correlation engine") {
		t.Errorf("system_prompt missing engine intro: %q", sp)
	}
	if up, _ := step.Args["user_prompt"].(string); !strings.Contains(up, currentAlert) {
		t.Errorf("user_prompt missing current alert id: %q", up)
	}

	// alertRepo 收到正确 key + since
	if repo.gotKey != labelsetkey {
		t.Errorf("alertRepo.gotKey = %q, want %q", repo.gotKey, labelsetkey)
	}
	expectedSince := now.Add(-24 * time.Hour)
	if !repo.gotSince.Equal(expectedSince) {
		t.Errorf("alertRepo.gotSince = %v, want %v", repo.gotSince, expectedSince)
	}

	// --- Executor ---
	result, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v", err)
	}
	if fc.CallCount() != 1 {
		t.Errorf("LLM CallCount = %d, want 1", fc.CallCount())
	}
	rawGroup, ok := result.RawOutputs["correlated_group"]
	if !ok {
		t.Fatalf("RawOutputs missing correlated_group")
	}
	group, ok := rawGroup.(*CorrelatedGroup)
	if !ok {
		t.Fatalf("RawOutputs.correlated_group type=%T, want *CorrelatedGroup", rawGroup)
	}
	if group.IncidentID != incidentID {
		t.Errorf("group.IncidentID = %q, want %q", group.IncidentID, incidentID)
	}
	if len(group.AlertIDs) != 4 {
		t.Fatalf("group.AlertIDs len = %d, want 4", len(group.AlertIDs))
	}
	wantAlerts := []string{currentAlert, historical1, historical2, historical3}
	for i, want := range wantAlerts {
		if group.AlertIDs[i] != want {
			t.Errorf("group.AlertIDs[%d] = %q, want %q", i, group.AlertIDs[i], want)
		}
	}
	if group.RootHypothesis != "shared root cause: long-running transaction" {
		t.Errorf("group.RootHypothesis = %q, want %q",
			group.RootHypothesis, "shared root cause: long-running transaction")
	}
	if group.Confidence != 0.92 {
		t.Errorf("group.Confidence = %v, want 0.92", group.Confidence)
	}

	// SideEffect 应当包含 phase_contract_written + labelsetkey / confidence
	if len(result.SideEffects) != 1 {
		t.Fatalf("SideEffects len = %d, want 1", len(result.SideEffects))
	}
	se := result.SideEffects[0]
	if se.Kind != "phase_contract_written" {
		t.Errorf("SideEffect.Kind = %q, want phase_contract_written", se.Kind)
	}
	if se.Target != incidentID {
		t.Errorf("SideEffect.Target = %q, want %s", se.Target, incidentID)
	}
	if got, _ := se.Detail["contract_type"].(string); got != "CorrelatedGroup" {
		t.Errorf("SideEffect.Detail.contract_type = %v, want CorrelatedGroup", got)
	}
	if got, _ := se.Detail["labelsetkey"].(string); got != labelsetkey {
		t.Errorf("SideEffect.Detail.labelsetkey = %v, want %s", got, labelsetkey)
	}
	if got, _ := se.Detail["confidence"].(float64); got != 0.92 {
		t.Errorf("SideEffect.Detail.confidence = %v, want 0.92", got)
	}
	payloadJSON, ok := se.Detail["payload"].(string)
	if !ok || payloadJSON == "" {
		t.Fatalf("SideEffect.Detail.payload type=%T, want non-empty string", se.Detail["payload"])
	}
	var persistedGroup CorrelatedGroup
	if err := json.Unmarshal([]byte(payloadJSON), &persistedGroup); err != nil {
		t.Fatalf("unmarshal CorrelatedGroup payload: %v", err)
	}
	if !reflect.DeepEqual(persistedGroup, *group) {
		t.Fatalf("SideEffect.Detail.payload = %+v, want %+v", persistedGroup, *group)
	}
	contracts := NewInMemoryContractRepo()
	contractOrchestrator, err := NewOrchestrator(OrchestratorDeps{
		Locker:       stubLocker{},
		ContractRepo: contracts,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator err: %v", err)
	}
	if err := contractOrchestrator.(*orchestrator).writeSideEffectContracts(
		context.Background(),
		RunOptions{TenantID: "tenant-1", IncidentID: incidentID},
		PhaseCorrelated,
		&result,
	); err != nil {
		t.Fatalf("writeSideEffectContracts err: %v", err)
	}
	stored, err := contracts.ReadContract(context.Background(), "tenant-1", incidentID, PhaseCorrelated, "CorrelatedGroup")
	if err != nil {
		t.Fatalf("ReadContract err: %v", err)
	}
	if stored == nil {
		t.Fatal("CorrelatedGroup contract not written")
	}
	if stored.Payload != payloadJSON {
		t.Fatalf("stored payload = %q, want %q", stored.Payload, payloadJSON)
	}
	if result.ContractRef == nil || result.ContractRef.Type != "CorrelatedGroup" {
		t.Fatalf("result.ContractRef = %+v, want CorrelatedGroup", result.ContractRef)
	}

	// --- Verifier ---
	verdict, err := w.Verifier(context.Background(), result)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if !verdict.OK {
		t.Errorf("Verifier OK = false, want true; reasons=%v", verdict.Reasons)
	}
	if verdict.Confidence != 0.92 {
		t.Errorf("Verifier Confidence = %v, want 0.92", verdict.Confidence)
	}
}

// --- Test 3: LLM failure path ------------------------------------------

// TestCorrelatedWorker_LLMFailure 覆盖"LLM 失败"路径：
//   - alertRepo 配 1 个同 labelsetkey 历史 alert
//   - FakeLLMClient 在第 0 次调用就 SetError → 连续 transient 失败耗尽
//   - Executor 返回 wrapped error（包含 "correlated llm call" 上下文）
func TestCorrelatedWorker_LLMFailure(t *testing.T) {
	t.Parallel()

	const (
		incidentID  = "inc-corr-2"
		labelsetkey = "device_id=99"
	)

	now := time.Date(2026, 8, 12, 11, 0, 0, 0, time.UTC)
	repo := newFakeAlertRepo()
	repo.put(labelsetkey, []DetectionEvent{
		validDetectionEvent("a-hist-1", labelsetkey, now.Add(-1*time.Hour)),
	})

	fc := NewFakeLLMClient()
	fc.SetError(0, errors.New("context deadline exceeded"))
	fc.SetError(1, errors.New("context deadline exceeded"))
	caller := NewLLMCaller(fc, WithLogger(silentLog()))

	current := validDetectionEvent("a-current", labelsetkey, now)
	loader := &fakeEventLoader{ev: current}
	w := newCorrelatedWorkerFor(t, caller, repo,
		WithCorrelatedClock(func() time.Time { return now }),
		WithCorrelatedLogger(silentLog()),
		WithCurrentDetectionEventLoader(loader),
	)

	planIn := PlanInput{
		IncidentID: incidentID,
		TenantID:   "t1",
		Phase:      PhaseCorrelated,
		UpstreamContract: &ContractRef{
			ID:            102,
			Type:          "DetectionEvent",
			SchemaVersion: "v1",
		},
	}

	plan, err := w.Planner(context.Background(), planIn)
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	_, err = w.Executor(context.Background(), plan)
	if err == nil {
		t.Fatal("Executor err = nil, want wrapped LLM failure")
	}
	if !strings.Contains(err.Error(), "correlated llm call") {
		t.Errorf("Executor error should mention correlated llm call; got %v", err)
	}
	if !strings.Contains(err.Error(), "after 1 retries") {
		t.Errorf("Executor error should mention retry exhaustion; got %v", err)
	}
	if fc.CallCount() != 2 {
		t.Errorf("FakeLLM CallCount = %d, want 2 (1 retry)", fc.CallCount())
	}
}

// --- Test 4: schema-invalid path ---------------------------------------

// TestCorrelatedWorker_SchemaInvalid_MissingAlertIDs 覆盖"schema-invalid"：
//   - LLM 给的 JSON 缺 alert_ids 字段（违反 schema.required）
//   - LLMCaller 的 schema validator 应在 ExtractJSON 之后 detect 缺失，
//     返回 ErrSchemaInvalid
//   - Executor 把 ErrSchemaInvalid 透传（wrapped "correlated llm call"）
func TestCorrelatedWorker_SchemaInvalid_MissingAlertIDs(t *testing.T) {
	t.Parallel()

	const (
		incidentID  = "inc-corr-3"
		labelsetkey = "device_id=7"
	)

	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	repo := newFakeAlertRepo()
	repo.put(labelsetkey, []DetectionEvent{
		validDetectionEvent("a-hist-1", labelsetkey, now.Add(-1*time.Hour)),
	})

	missingFieldJSON := `{"incident_id":"inc-corr-3","root_hypothesis":"some hypothesis"}`

	fc := NewFakeLLMClient()
	fc.SetResponse(0, missingFieldJSON)
	caller := NewLLMCaller(fc, WithLogger(silentLog()))

	current := validDetectionEvent("a-current", labelsetkey, now)
	loader := &fakeEventLoader{ev: current}
	w := newCorrelatedWorkerFor(t, caller, repo,
		WithCorrelatedClock(func() time.Time { return now }),
		WithCorrelatedLogger(silentLog()),
		WithCurrentDetectionEventLoader(loader),
	)

	planIn := PlanInput{
		IncidentID: incidentID,
		TenantID:   "t1",
		Phase:      PhaseCorrelated,
		UpstreamContract: &ContractRef{
			ID:            103,
			Type:          "DetectionEvent",
			SchemaVersion: "v1",
		},
	}

	plan, err := w.Planner(context.Background(), planIn)
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	_, err = w.Executor(context.Background(), plan)
	if err == nil {
		t.Fatal("Executor err = nil, want ErrSchemaInvalid-wrapped error")
	}
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("Executor err should wrap ErrSchemaInvalid; got %v", err)
	}
	if !strings.Contains(err.Error(), "correlated llm call") {
		t.Errorf("Executor error should mention correlated llm call; got %v", err)
	}
	if fc.CallCount() != 1 {
		t.Errorf("FakeLLM CallCount = %d, want 1 (no retry on schema-invalid)", fc.CallCount())
	}
}

// --- Test 5: Verifier rejects empty alert_ids ---------------------------

// TestCorrelatedWorker_Verifier_RejectsEmptyAlertIDs 覆盖"Executor
// 兜底失败 + Verifier 拒收"路径：LLM 故意把 alert_ids 输出成空数组
// 加上 Executor 兜底也没填上（currentAlertID 也故意空着）→ Verifier
// 报 ErrCorrelatedSchemaInvalid + Verdict.OK=false。
func TestCorrelatedWorker_Verifier_RejectsEmptyAlertIDs(t *testing.T) {
	t.Parallel()

	const (
		incidentID  = "inc-corr-4"
		labelsetkey = "device_id=8"
	)

	now := time.Date(2026, 8, 12, 13, 0, 0, 0, time.UTC)
	repo := newFakeAlertRepo()
	repo.put(labelsetkey, []DetectionEvent{
		validDetectionEvent("a-hist-1", labelsetkey, now.Add(-1*time.Hour)),
	})

	emptyAlertIDsJSON := validCorrelatedJSON(incidentID, []string{}, "ambiguous", 0.4)

	fc := NewFakeLLMClient()
	fc.SetResponse(0, emptyAlertIDsJSON)
	caller := NewLLMCaller(fc, WithLogger(silentLog()))

	current := validDetectionEvent("", labelsetkey, now)
	loader := &fakeEventLoader{ev: current}
	w := newCorrelatedWorkerFor(t, caller, repo,
		WithCorrelatedClock(func() time.Time { return now }),
		WithCorrelatedLogger(silentLog()),
		WithCurrentDetectionEventLoader(loader),
	)

	planIn := PlanInput{
		IncidentID: incidentID,
		TenantID:   "t1",
		Phase:      PhaseCorrelated,
		UpstreamContract: &ContractRef{
			ID:            104,
			Type:          "DetectionEvent",
			SchemaVersion: "v1",
		},
	}

	plan, err := w.Planner(context.Background(), planIn)
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	result, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v", err)
	}
	verdict, err := w.Verifier(context.Background(), result)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if verdict.OK {
		t.Errorf("Verifier OK = true, want false; confidence=%v", verdict.Confidence)
	}
	if len(verdict.Reasons) == 0 {
		t.Errorf("Verifier Reasons empty, want at least one reason")
	}
	found := false
	for _, r := range verdict.Reasons {
		if strings.Contains(r, "alert_ids") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Verifier Reasons should mention alert_ids; got %v", verdict.Reasons)
	}
}

// --- Test 6: VerifierTimeoutMs = 60_000 --------------------------------

// TestCorrelatedWorker_VerifierTimeoutMs 验证 VerifierTimeoutMs 固定 60s。
func TestCorrelatedWorker_VerifierTimeoutMs(t *testing.T) {
	t.Parallel()
	w := newCorrelatedWorkerFor(t,
		NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLog())),
		newFakeAlertRepo(),
	)
	if got := w.VerifierTimeoutMs(); got != 60_000 {
		t.Errorf("VerifierTimeoutMs = %d, want 60_000", got)
	}
}

// --- Test 7: ValidateCorrelatedGroup unit tests ------------------------

// TestValidateCorrelatedGroup 覆盖 ValidateCorrelatedGroup 各 invalid 路径。
// ResourceType/Target/TimeWindow 是 optional 字段（omitempty），LLM 不
// 强制要求输出；Verifier 只校验 LLM 必填字段。
func TestValidateCorrelatedGroup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		g       *CorrelatedGroup
		wantErr bool
		wantSub string
	}{
		{
			name:    "nil",
			g:       nil,
			wantErr: true,
			wantSub: "nil CorrelatedGroup",
		},
		{
			name:    "missing incident_id",
			g:       &CorrelatedGroup{AlertIDs: []string{"a1"}, RootHypothesis: "x"},
			wantErr: true,
			wantSub: "incident_id missing",
		},
		{
			name:    "empty alert_ids",
			g:       &CorrelatedGroup{IncidentID: "i1", AlertIDs: []string{}, RootHypothesis: "x"},
			wantErr: true,
			wantSub: "alert_ids empty",
		},
		{
			name:    "alert_ids contains empty string",
			g:       &CorrelatedGroup{IncidentID: "i1", AlertIDs: []string{"a1", ""}, RootHypothesis: "x"},
			wantErr: true,
			wantSub: "alert_ids[1] is empty",
		},
		{
			name:    "missing root_hypothesis",
			g:       &CorrelatedGroup{IncidentID: "i1", AlertIDs: []string{"a1"}},
			wantErr: true,
			wantSub: "root_hypothesis missing",
		},
		{
			name:    "confidence out of range high",
			g:       &CorrelatedGroup{IncidentID: "i1", AlertIDs: []string{"a1"}, RootHypothesis: "x", Confidence: 1.5},
			wantErr: true,
			wantSub: "confidence=1.5 out of [0,1]",
		},
		{
			name:    "confidence out of range low",
			g:       &CorrelatedGroup{IncidentID: "i1", AlertIDs: []string{"a1"}, RootHypothesis: "x", Confidence: -0.1},
			wantErr: true,
			wantSub: "confidence=-0.1 out of [0,1]",
		},
		{
			name:    "valid with confidence 0",
			g:       &CorrelatedGroup{IncidentID: "i1", AlertIDs: []string{"a1"}, RootHypothesis: "x", Confidence: 0},
			wantErr: false,
		},
		{
			name:    "valid with confidence 1",
			g:       &CorrelatedGroup{IncidentID: "i1", AlertIDs: []string{"a1"}, RootHypothesis: "x", Confidence: 1},
			wantErr: false,
		},
		{
			name:    "valid minimal",
			g:       &CorrelatedGroup{IncidentID: "i1", AlertIDs: []string{"a1"}, RootHypothesis: "x"},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateCorrelatedGroup(tc.g)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateCorrelatedGroup err = nil, want error containing %q", tc.wantSub)
				}
				if !errors.Is(err, ErrCorrelatedSchemaInvalid) {
					t.Errorf("err should wrap ErrCorrelatedSchemaInvalid; got %v", err)
				}
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("err should contain %q; got %q", tc.wantSub, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("ValidateCorrelatedGroup err = %v, want nil", err)
				}
			}
		})
	}
}

// --- Test 8: Planner requires currentEventLoader ------------------------

// TestCorrelatedWorker_Planner_RequiresLoader 验证 Planner 在未注入
// currentEventLoader 时直接 fail-fast（fail at wire-up 原则）。
func TestCorrelatedWorker_Planner_RequiresLoader(t *testing.T) {
	t.Parallel()
	w := newCorrelatedWorkerFor(t,
		NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLog())),
		newFakeAlertRepo(),
	)
	_, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "inc-noop",
		Phase:      PhaseCorrelated,
	})
	if err == nil {
		t.Fatal("Planner err = nil, want loader-not-configured error")
	}
	if !errors.Is(err, ErrPlanInvalid) {
		t.Errorf("err should wrap ErrPlanInvalid; got %v", err)
	}
	if !strings.Contains(err.Error(), "current DetectionEvent loader") {
		t.Errorf("err should mention current DetectionEvent loader; got %v", err)
	}
}

// --- Test 9: alertRepo error propagation -------------------------------

// TestCorrelatedWorker_Planner_AlertRepoError 验证 alertRepo 返回 error
// 时 Planner 透传 wrapped。
func TestCorrelatedWorker_Planner_AlertRepoError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	repo := newFakeAlertRepo()
	repo.err = errors.New("db connection refused")
	caller := NewLLMCaller(NewFakeLLMClient(), WithLogger(silentLog()))
	current := validDetectionEvent("a-current", "device_id=1", now)
	loader := &fakeEventLoader{ev: current}
	w := newCorrelatedWorkerFor(t, caller, repo,
		WithCorrelatedClock(func() time.Time { return now }),
		WithCorrelatedLogger(silentLog()),
		WithCurrentDetectionEventLoader(loader),
	)
	_, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "inc-1",
		Phase:      PhaseCorrelated,
	})
	if err == nil {
		t.Fatal("Planner err = nil, want alertRepo error wrapped")
	}
	if !strings.Contains(err.Error(), "FindByLabelsetkey") {
		t.Errorf("err should mention FindByLabelsetkey; got %v", err)
	}
	if !strings.Contains(err.Error(), "db connection refused") {
		t.Errorf("err should wrap alertRepo error; got %v", err)
	}
}

func TestCorrelatedWorker_SchemaAllowsTrustedIncidentFallback(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	fake := NewFakeLLMClient()
	fake.SetResponse(0, `{"alert_ids":["a-current"],"root_hypothesis":"single-alert group","confidence":0.8}`)
	caller := NewLLMCaller(fake, WithLogger(silentLog()))
	current := validDetectionEvent("a-current", "device_id=1", now)
	w := newCorrelatedWorkerFor(t, caller, newFakeAlertRepo(),
		WithCorrelatedClock(func() time.Time { return now }),
		WithCorrelatedLogger(silentLog()),
		WithCurrentDetectionEventLoader(&fakeEventLoader{ev: current}),
	)
	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: "inc-trusted",
		TenantID:   "tenant-a",
		Phase:      PhaseCorrelated,
	})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	result, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor: %v", err)
	}
	group, ok := result.RawOutputs["correlated_group"].(*CorrelatedGroup)
	if !ok {
		t.Fatalf("correlated_group type = %T", result.RawOutputs["correlated_group"])
	}
	if group.IncidentID != "inc-trusted" {
		t.Fatalf("IncidentID = %q, want trusted inc-trusted", group.IncidentID)
	}
}

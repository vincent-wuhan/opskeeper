// Package loop — critiqued_worker_test.go
//
// 测试覆盖（LLM-driven critiqued phase，路径 A 集成批次 2）：
//
//  1. 成功路径：FakeLLM 返回合法 3 维 JSON → Planner/Executor/Verifier
//     全跑通，CritiqueDimensions 字段断言 + Verdict.OK=true
//  2. LLM 调用失败：FakeLLM 返回 error → Planner 错误链 wrap（不含
//     ErrSchemaInvalid，验证 retry 路径）
//  3. schema-invalid（缺字段）：missing actionability → LLMCaller 内部
//     schema 校验失败 → wrapped ErrSchemaInvalid
//  4. 越界（actionability=1.5）：schema validator 的 minimum/maximum
//     拦截 → wrapped ErrSchemaInvalid
//  5. 防御：上游 RootCauseJSON 加载失败 → Planner 拒绝（ErrPlanInvalid wrap）
//  6. 防御：caller == nil → Planner 拒绝（programmer error）
package loop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

// silentCritiqueLogger 把所有日志输出丢到 io.Discard，让单测输出干净。
func silentCritiqueLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRootCauseLoader 是 RootCauseRefLoader 的内存 fake。
// err != nil 时短路返回错误；rc == nil 时返回 (nil, nil)。
type fakeRootCauseLoader struct {
	mu  sync.Mutex
	rc  *RootCauseJSON
	err error
}

func (f *fakeRootCauseLoader) LoadRootCause(_ context.Context, _ int64) (*RootCauseJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.rc, nil
}

// newSampleRootCauseJSON 返回一份通过 ValidateRootCauseJSON 的最小 RootCauseJSON。
func newSampleRootCauseJSON() *RootCauseJSON {
	return &RootCauseJSON{
		SchemaVersion: ContractSchemaV1,
		RootCauseObject: &RootCauseObject{
			Kind:    "pg.long_running_tx",
			Summary: "session 12345 held a repeatable-read tx on orders for 320s",
			Detail: map[string]any{
				"pid":        12345,
				"duration_s": 320,
			},
		},
		Confidence: 0.92,
		EvidenceChain: []EvidenceItem{{
			Tool:  "query_promql",
			Query: "pg_long_running_transactions{database=\"prod\"}",
			Value: 1,
			Count: 1,
		}},
		TimeWindow: TimeWindow{
			Start: time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		},
		RemediationOptions: []RemediationOption{{
			Action:      "pg.terminate_session",
			Target:      "postgres://prod-cluster-1/pid=12345",
			Risk:        "safe",
			AutoApprove: false,
		}},
	}
}

// newTestCritiquedWorker 构造一个挂上 fake LLM + fake 上游 loader 的 worker。
// fakeLLM 由 caller 包入 NewLLMCaller；上游 RootCauseJSON 由 loader 提供。
func newTestCritiquedWorker(t *testing.T, fakeLLM *FakeLLMClient, rc *RootCauseJSON, rcErr error) (*CritiquedPhaseWorker, *FakeLLMClient) {
	t.Helper()
	caller := NewLLMCaller(fakeLLM, WithLogger(silentCritiqueLogger()))
	loader := &fakeRootCauseLoader{rc: rc, err: rcErr}
	clock := func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) }
	w := NewCritiquedPhaseWorker(
		caller,
		WithCritiqueClock(clock),
		WithCritiqueLogger(silentCritiqueLogger()),
		WithCritiqueUpstreamLoader(loader),
	)
	if w == nil {
		t.Fatalf("NewCritiquedPhaseWorker returned nil")
	}
	return w, fakeLLM
}

// TestCritiquedPlanner_Success — happy path：合法 3 维 JSON → Planner 返回
// Plan，Meta 里 critique_dimensions 字段断言 + Executor + Verifier 全部 OK。
func TestCritiquedPlanner_Success(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetResponse(0, `{"accuracy":0.85,"completeness":0.9,"actionability":0.7}`)

	w, fcPtr := newTestCritiquedWorker(t, fc, newSampleRootCauseJSON(), nil)

	in := PlanInput{
		IncidentID: "inc-crit-001",
		TenantID:   "tenant-crit",
		Phase:      PhaseCritiqued,
		Attempt:    1,
		UpstreamContract: &ContractRef{
			ID:            42,
			Type:          "RootCauseJSON",
			SchemaVersion: ContractSchemaV1,
		},
		TraceID: "trace-crit-001",
	}

	plan, err := w.Planner(context.Background(), in)
	if err != nil {
		t.Fatalf("Planner returned err = %v, want nil", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("plan.Steps len = %d, want 1", len(plan.Steps))
	}
	if plan.Steps[0].Target != "critique_llm" {
		t.Errorf("plan.Steps[0].Target = %q, want %q", plan.Steps[0].Target, "critique_llm")
	}

	dimsVal, ok := plan.Meta[critiqueDimensionsKey]
	if !ok {
		t.Fatalf("plan.Meta missing %q", critiqueDimensionsKey)
	}
	dims, ok := dimsVal.(*CritiqueDimensions)
	if !ok {
		t.Fatalf("plan.Meta[%q] type = %T, want *CritiqueDimensions", critiqueDimensionsKey, dimsVal)
	}
	if dims.Accuracy != 0.85 {
		t.Errorf("dims.Accuracy = %v, want 0.85", dims.Accuracy)
	}
	if dims.Completeness != 0.9 {
		t.Errorf("dims.Completeness = %v, want 0.9", dims.Completeness)
	}
	if dims.Actionability != 0.7 {
		t.Errorf("dims.Actionability = %v, want 0.7", dims.Actionability)
	}

	if rawVal, ok := plan.Meta[critiqueRawKey]; !ok {
		t.Errorf("plan.Meta missing %q", critiqueRawKey)
	} else if rawStr, ok := rawVal.(string); !ok {
		t.Errorf("plan.Meta[%q] type = %T, want string", critiqueRawKey, rawVal)
	} else if !strings.Contains(rawStr, `"accuracy":0.85`) {
		t.Errorf("plan.Meta[%q] = %q, missing accuracy", critiqueRawKey, rawStr)
	}

	// Executor 占位持久化：把 dims 装进 ExecResult.RawOutputs。
	execResult, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor returned err = %v, want nil", err)
	}
	if len(execResult.SideEffects) != 1 {
		t.Fatalf("execResult.SideEffects len = %d, want 1", len(execResult.SideEffects))
	}
	if execResult.SideEffects[0].Target != "CritiqueScore" {
		t.Errorf("SideEffect[0].Target = %q, want %q", execResult.SideEffects[0].Target, "CritiqueScore")
	}
	if d, ok := execResult.SideEffects[0].Detail["accuracy"].(float64); !ok || d != 0.85 {
		t.Errorf("SideEffect.Detail[accuracy] = %v (%T), want 0.85", execResult.SideEffects[0].Detail["accuracy"], execResult.SideEffects[0].Detail["accuracy"])
	}

	// Verifier 应该返回 OK=true，Confidence 是三个维度的均值。
	verdict, err := w.Verifier(context.Background(), execResult)
	if err != nil {
		t.Fatalf("Verifier returned err = %v, want nil", err)
	}
	if !verdict.OK {
		t.Errorf("verdict.OK = false, want true; reasons=%v", verdict.Reasons)
	}
	wantMean := (0.85 + 0.9 + 0.7) / 3.0
	if delta := verdict.Confidence - wantMean; delta > 1e-9 || delta < -1e-9 {
		t.Errorf("verdict.Confidence = %v, want %v", verdict.Confidence, wantMean)
	}

	// LLM 必须只被调用一次。
	if got := fcPtr.CallCount(); got != 1 {
		t.Errorf("FakeLLMClient.CallCount = %d, want 1", got)
	}
}

// TestCritiquedPlanner_LLMError — LLM 调用失败（timeout） → Planner 错误
// wrap；不应把 ErrSchemaInvalid 串进去（避免混 retry 路径和 schema 路径）。
func TestCritiquedPlanner_LLMError(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	// 给两次失败迫使 MaxRetries=1 全部耗尽；error 是 timeout 类。
	fc.SetError(0, errors.New("ChatCompletion: context deadline exceeded"))
	fc.SetError(1, errors.New("ChatCompletion: context deadline exceeded"))

	w, _ := newTestCritiquedWorker(t, fc, newSampleRootCauseJSON(), nil)

	in := PlanInput{
		IncidentID:       "inc-crit-err",
		TenantID:         "tenant-crit",
		Phase:            PhaseCritiqued,
		UpstreamContract: &ContractRef{ID: 1, Type: "RootCauseJSON", SchemaVersion: ContractSchemaV1},
	}
	_, err := w.Planner(context.Background(), in)
	if err == nil {
		t.Fatalf("Planner returned nil err; want timeout wrap")
	}
	if errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err wrapped ErrSchemaInvalid; LLM timeout is NOT a schema error: %v", err)
	}
	if !strings.Contains(err.Error(), "critiqued planner LLM call") {
		t.Errorf("err = %q, want chain to include 'critiqued planner LLM call'", err.Error())
	}
}

// TestCritiquedPlanner_SchemaInvalid_MissingField — LLM 返回 JSON 缺
// actionability 字段 → LLMCaller 内部 schema 校验失败 → wrapped ErrSchemaInvalid。
func TestCritiquedPlanner_SchemaInvalid_MissingField(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetResponse(0, `{"accuracy":0.85,"completeness":0.9}`) // missing actionability

	w, _ := newTestCritiquedWorker(t, fc, newSampleRootCauseJSON(), nil)

	in := PlanInput{
		IncidentID:       "inc-crit-miss",
		TenantID:         "tenant-crit",
		Phase:            PhaseCritiqued,
		UpstreamContract: &ContractRef{ID: 1, Type: "RootCauseJSON", SchemaVersion: ContractSchemaV1},
	}
	_, err := w.Planner(context.Background(), in)
	if err == nil {
		t.Fatalf("Planner returned nil err; want schema-invalid wrap")
	}
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err = %v, want errors.Is(ErrSchemaInvalid) true", err)
	}
}

// TestCritiquedPlanner_SchemaInvalid_OutOfRange — actionability=1.5 越过
// schema maximum=1 → schema validator 拦截 → wrapped ErrSchemaInvalid。
// 该用例隐式验证 llm_caller_schema.go 已支持 minimum/maximum bound。
func TestCritiquedPlanner_SchemaInvalid_OutOfRange(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetResponse(0, `{"accuracy":0.85,"completeness":0.9,"actionability":1.5}`)

	w, _ := newTestCritiquedWorker(t, fc, newSampleRootCauseJSON(), nil)

	in := PlanInput{
		IncidentID:       "inc-crit-oor",
		TenantID:         "tenant-crit",
		Phase:            PhaseCritiqued,
		UpstreamContract: &ContractRef{ID: 1, Type: "RootCauseJSON", SchemaVersion: ContractSchemaV1},
	}
	_, err := w.Planner(context.Background(), in)
	if err == nil {
		t.Fatalf("Planner returned nil err; want out-of-range wrap")
	}
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err = %v, want errors.Is(ErrSchemaInvalid) true", err)
	}
}

// TestCritiquedPlanner_UpstreamLoadError — 上游 RootCauseJSON 加载失败
// → Planner 拒绝（ErrPlanInvalid wrap）。
func TestCritiquedPlanner_UpstreamLoadError(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetResponse(0, `{"accuracy":0.85,"completeness":0.9,"actionability":0.7}`)

	dbErr := errors.New("db connection refused")
	w, _ := newTestCritiquedWorker(t, fc, nil, dbErr)

	in := PlanInput{
		IncidentID:       "inc-crit-up",
		TenantID:         "tenant-crit",
		Phase:            PhaseCritiqued,
		UpstreamContract: &ContractRef{ID: 1, Type: "RootCauseJSON", SchemaVersion: ContractSchemaV1},
	}
	_, err := w.Planner(context.Background(), in)
	if err == nil {
		t.Fatalf("Planner returned nil err; want load error wrap")
	}
	// 上游 DB 错误是 IO 类（不是 plan 校验类），不应 wrap ErrPlanInvalid；
	// 直接断言原始错误被透传。
	if !strings.Contains(err.Error(), "db connection refused") {
		t.Errorf("err = %v, want chain to include 'db connection refused'", err)
	}
	if errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err = %v, want NOT ErrSchemaInvalid (upstream IO is not schema)", err)
	}
	if fc.CallCount() != 0 {
		t.Errorf("FakeLLMClient.CallCount = %d, want 0 (LLM must not be called when upstream load fails)", fc.CallCount())
	}
}

// TestCritiquedPlanner_NilCaller — programmer error：caller 未注入 → 拒绝。
func TestCritiquedPlanner_NilCaller(t *testing.T) {
	t.Parallel()
	loader := &fakeRootCauseLoader{rc: newSampleRootCauseJSON()}
	w := NewCritiquedPhaseWorker(
		nil, // caller == nil
		WithCritiqueLogger(silentCritiqueLogger()),
		WithCritiqueUpstreamLoader(loader),
	)
	in := PlanInput{
		IncidentID:       "inc-crit-nil",
		Phase:            PhaseCritiqued,
		UpstreamContract: &ContractRef{ID: 1, Type: "RootCauseJSON", SchemaVersion: ContractSchemaV1},
	}
	_, err := w.Planner(context.Background(), in)
	if err == nil {
		t.Fatalf("Planner returned nil err; want programmer-error rejection")
	}
	if !errors.Is(err, ErrPlanInvalid) {
		t.Errorf("err = %v, want errors.Is(ErrPlanInvalid) true", err)
	}
}

// TestCritiquedVerifier_BoundsFailDirect — 绕过 Planner，直接构造
// ExecResult.RawOutputs 注入越界维度，验证 Verifier 自己的兜底逻辑。
// Planner 路径已被上面 4 个 case 覆盖 schema + retry；这里专注 worker 内
// 部 sanity check（防 Plan.Meta 被外部篡改的场景）。
func TestCritiquedVerifier_BoundsFailDirect(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	w, _ := newTestCritiquedWorker(t, fc, newSampleRootCauseJSON(), nil)

	badDims := &CritiqueDimensions{
		Accuracy:      0.5,
		Completeness:  0.5,
		Actionability: 1.5, // out of [0,1]
	}
	res := ExecResult{
		RawOutputs: map[string]any{
			critiqueDimensionsKey: badDims,
		},
	}
	verdict, err := w.Verifier(context.Background(), res)
	if err != nil {
		t.Fatalf("Verifier returned err = %v, want nil (verifier returns via verdict)", err)
	}
	if verdict.OK {
		t.Errorf("verdict.OK = true, want false")
	}
	if len(verdict.Reasons) == 0 {
		t.Errorf("verdict.Reasons is empty; want at least one out-of-range reason")
	}
	foundActionability := false
	for _, r := range verdict.Reasons {
		if strings.Contains(r, "actionability") && strings.Contains(r, "1.5") {
			foundActionability = true
		}
	}
	if !foundActionability {
		t.Errorf("verdict.Reasons %v missing actionability=1.5 mention", verdict.Reasons)
	}
}

// TestCritiquedVerifier_MissingDims — ExecResult.RawOutputs 缺 critique_dimensions
// → Verifier 直接返回 OK=false + "missing_critique_dimensions"。
func TestCritiquedVerifier_MissingDims(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	w, _ := newTestCritiquedWorker(t, fc, newSampleRootCauseJSON(), nil)

	res := ExecResult{
		RawOutputs: map[string]any{
			"unrelated_key": "value",
		},
	}
	verdict, err := w.Verifier(context.Background(), res)
	if err != nil {
		t.Fatalf("Verifier returned err = %v, want nil", err)
	}
	if verdict.OK {
		t.Errorf("verdict.OK = true, want false (missing dims)")
	}
	if len(verdict.Reasons) != 1 || verdict.Reasons[0] != "missing_critique_dimensions" {
		t.Errorf("verdict.Reasons = %v, want [missing_critique_dimensions]", verdict.Reasons)
	}
}

// TestCritiquedVerifierTimeoutMs_Override — 60_000 override 走 BasePhaseWorker
// 字段；自定义 VerifierMs 时优先。
func TestCritiquedVerifierTimeoutMs_Override(t *testing.T) {
	t.Parallel()
	caller := NewLLMCaller(NewFakeLLMClient(), WithLogger(silentCritiqueLogger()))
	w := NewCritiquedPhaseWorker(caller, WithCritiqueLogger(silentCritiqueLogger()))
	if got := w.VerifierTimeoutMs(); got != critiquedPhaseVerifierTimeoutMs {
		t.Errorf("VerifierTimeoutMs = %d, want %d", got, critiquedPhaseVerifierTimeoutMs)
	}
}

// --- sanity: package-internal fake LLM call to validate wiring ----------

// TestCritiqued_EndToEnd_NoRetryOnSuccess — sanity test：Planner 一次成功
// LLM 调用就出 Plan，验证 fake LLM 的 "happy path no retry" 契约。
func TestCritiqued_EndToEnd_NoRetryOnSuccess(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetResponse(0, `{"accuracy":0.8,"completeness":0.8,"actionability":0.8}`)

	w, fcPtr := newTestCritiquedWorker(t, fc, newSampleRootCauseJSON(), nil)
	in := PlanInput{
		IncidentID:       "inc-crit-e2e",
		Phase:            PhaseCritiqued,
		UpstreamContract: &ContractRef{ID: 1, Type: "RootCauseJSON", SchemaVersion: ContractSchemaV1},
	}
	if _, err := w.Planner(context.Background(), in); err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	if got := fcPtr.CallCount(); got != 1 {
		t.Errorf("CallCount = %d, want 1 (happy path no retry)", got)
	}
}

// 用到的 import 防止 unused warning（errors / time / llm 等）。
var (
	_ = errors.New
	_ = llm.ChatReq{}
)

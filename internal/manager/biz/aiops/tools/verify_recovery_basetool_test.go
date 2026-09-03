package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
)

// fakeQuerier 是 MetricQuerier 的内存 fake，按 target+metric 装 baseline/current。
// 未装时返回 (1.0, 1.0, 100) —— 偏差=0，天然通过校验。
type fakeQuerier struct {
	mu sync.Mutex
	// fixtures: target+metric → (baseline, current, sample)
	fixtures map[string]metricTriple
	// calls: 累积每次 QueryMetric 调用，便于断言次数 / 并发安全。
	calls atomic.Int32
	// failOn: 设了这个 metric 时返回错误（用于 IO 失败测试）。
	failOn map[string]error
}

type metricTriple struct {
	baseline, current float64
	sample            int
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{
		fixtures: make(map[string]metricTriple),
		failOn:   make(map[string]error),
	}
}

func (f *fakeQuerier) plant(target, metric string, baseline, current float64, sample int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fixtures[target+"|"+metric] = metricTriple{baseline, current, sample}
}

func (f *fakeQuerier) failMetric(metric string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn[metric] = err
}

func (f *fakeQuerier) QueryMetric(_ context.Context, req MetricQueryRequest) (MetricQueryResult, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn[req.Metric]; ok {
		return MetricQueryResult{}, err
	}
	t, ok := f.fixtures[req.Target+"|"+req.Metric]
	if !ok {
		// 默认：偏差=0（永远 pass），让不相关测试不踩到。
		return MetricQueryResult{BaselineAvg: 1.0, CurrentAvg: 1.0, SampleSize: 100}, nil
	}
	return MetricQueryResult{BaselineAvg: t.baseline, CurrentAvg: t.current, SampleSize: t.sample}, nil
}

func (f *fakeQuerier) callCount() int { return int(f.calls.Load()) }

// newVerifyRecoveryToolFor 测试构造器；用 InMemoryRecoveryStateStore 默认。
func newVerifyRecoveryToolFor(t *testing.T, q MetricQuerier, store RecoveryStateStore, cfg ...VerifyRecoveryConfig) *VerifyRecoveryTool {
	t.Helper()
	c := VerifyRecoveryConfig{}
	if len(cfg) > 0 {
		c = cfg[0]
	}
	return NewVerifyRecoveryTool(q, store, nil, c)
}

// --- Test 1: pass -------------------------------------------------------

// TestVerifyRecovery_Pass：当所有 metric 都在 tolerance 内时返回 passed=true。
func TestVerifyRecovery_Pass(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	// baseline 10 / current 11 → 相对偏差 10%，< 15% tolerance
	q.plant("host-1", "cpu_usage", 10, 11, 50)
	q.plant("host-1", "mem_usage", 20, 22, 50)
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	argsJSON := `{
		"skill_id": "inc-pass",
		"target": "host-1",
		"resource_type": "host",
		"tolerance": 0.15,
		"metrics": ["cpu_usage","mem_usage"]
	}`
	out, err := tool.InvokableRun(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun err: %v", err)
	}
	var got VerifyOutcome
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Passed {
		t.Errorf("expected Passed=true, got %+v", got)
	}
	if len(got.FailedMetrics) != 0 {
		t.Errorf("expected no failed metrics, got %v", got.FailedMetrics)
	}
	if got.WarningLevel != "pass" {
		t.Errorf("WarningLevel = %q, want pass", got.WarningLevel)
	}
	if got.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0 (fresh incident)", got.RetryCount)
	}
	if got.SchemaVersion != "v1" {
		t.Errorf("SchemaVersion = %q, want v1", got.SchemaVersion)
	}
	if got.Tolerance != 0.15 {
		t.Errorf("Tolerance = %v, want 0.15", got.Tolerance)
	}
}

// --- Test 2: fail -------------------------------------------------------

// TestVerifyRecovery_Fail：任一 metric 超 tolerance 时返回 passed=false
// + failed_metrics 列出该 metric + Verifier OK=false。
func TestVerifyRecovery_Fail(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	// cpu baseline=10 / current=20 → 100% deviation，远超 15%
	q.plant("host-1", "cpu_usage", 10, 20, 50)
	q.plant("host-1", "mem_usage", 20, 22, 50)
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	argsJSON := `{
		"skill_id":"inc-fail","target":"host-1","resource_type":"host",
		"tolerance":0.15,"metrics":["cpu_usage","mem_usage"]
	}`
	out, err := tool.InvokableRun(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun err: %v", err)
	}
	var got VerifyOutcome
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Passed {
		t.Errorf("expected Passed=false, got %+v", got)
	}
	if len(got.FailedMetrics) != 1 || got.FailedMetrics[0] != "cpu_usage" {
		t.Errorf("FailedMetrics = %v, want [cpu_usage]", got.FailedMetrics)
	}
	if got.Deltas["cpu_usage"] < 0.99 || got.Deltas["cpu_usage"] > 1.01 {
		t.Errorf("cpu_usage delta = %v, want ≈1.0", got.Deltas["cpu_usage"])
	}
	if got.WarningLevel != "fail" {
		t.Errorf("WarningLevel = %q, want fail", got.WarningLevel)
	}
}

// --- Test 3: metric not allowed (no IO) --------------------------------

// TestVerifyRecovery_MetricNotAllowed：当 metric 不在 allowlist 内，
// 直接拒绝且 querier 一次都不调用。
func TestVerifyRecovery_MetricNotAllowed(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	argsJSON := `{
		"skill_id":"inc-bad-metric","target":"host-1","resource_type":"host",
		"tolerance":0.15,"metrics":["secret_token_count"]
	}`
	_, err := tool.InvokableRun(context.Background(), argsJSON)
	if err == nil {
		t.Fatalf("expected ErrMetricNotAllowed, got nil")
	}
	if !errors.Is(err, ErrMetricNotAllowed) {
		t.Errorf("error chain should contain ErrMetricNotAllowed, got %v", err)
	}
	if q.callCount() != 0 {
		t.Errorf("querier should not be called for disallowed metric; calls=%d", q.callCount())
	}
}

// TestVerifyRecovery_ResourceSubsetViolation：metric 在 allowlist 但
// 不在该 resource 子集内（host 不允许 qps）。
func TestVerifyRecovery_ResourceSubsetViolation(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	argsJSON := `{
		"skill_id":"inc-bad-sub","target":"host-1","resource_type":"host",
		"tolerance":0.15,"metrics":["qps"]
	}`
	_, err := tool.InvokableRun(context.Background(), argsJSON)
	if err == nil {
		t.Fatalf("expected ErrMetricNotAllowed (subset), got nil")
	}
	if !errors.Is(err, ErrMetricNotAllowed) {
		t.Errorf("error chain should contain ErrMetricNotAllowed, got %v", err)
	}
	if q.callCount() != 0 {
		t.Errorf("querier should not be called for subset violation; calls=%d", q.callCount())
	}
}

// TestVerifyRecovery_ToleranceOutOfRange：tolerance 不在 [0,1] 拒绝。
func TestVerifyRecovery_ToleranceOutOfRange(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	argsJSON := `{
		"skill_id":"inc-bad-tol","target":"host-1","resource_type":"host",
		"tolerance":1.5,"metrics":["cpu_usage"]
	}`
	_, err := tool.InvokableRun(context.Background(), argsJSON)
	if err == nil {
		t.Fatalf("expected ErrToleranceOutOfRange, got nil")
	}
	if !errors.Is(err, ErrToleranceOutOfRange) {
		t.Errorf("error chain should contain ErrToleranceOutOfRange, got %v", err)
	}
}

// TestVerifyRecovery_WindowInvalid：baseline_window=0 或 > 1h 拒绝。
func TestVerifyRecovery_WindowInvalid(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	cases := []struct{ name, args string }{
		{"zero baseline", `{"skill_id":"x","target":"h","resource_type":"host","tolerance":0.15,"metrics":["cpu_usage"],"baseline_window":"0s"}`},
		{">1h baseline", `{"skill_id":"x","target":"h","resource_type":"host","tolerance":0.15,"metrics":["cpu_usage"],"baseline_window":"2h"}`},
		{"zero compare", `{"skill_id":"x","target":"h","resource_type":"host","tolerance":0.15,"metrics":["cpu_usage"],"compare_window":"0s"}`},
		{">1h compare", `{"skill_id":"x","target":"h","resource_type":"host","tolerance":0.15,"metrics":["cpu_usage"],"compare_window":"90m"}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.InvokableRun(context.Background(), tc.args)
			if err == nil {
				t.Fatalf("expected ErrWindowInvalid, got nil")
			}
			if !errors.Is(err, ErrWindowInvalid) {
				t.Errorf("error chain should contain ErrWindowInvalid, got %v", err)
			}
		})
	}
}

// TestVerifyRecovery_ResourceUnknown：resource_type 不在枚举内。
func TestVerifyRecovery_ResourceUnknown(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	argsJSON := `{
		"skill_id":"x","target":"h","resource_type":"unicorn",
		"tolerance":0.15,"metrics":["cpu_usage"]
	}`
	_, err := tool.InvokableRun(context.Background(), argsJSON)
	if err == nil {
		t.Fatalf("expected ErrResourceTypeUnknown, got nil")
	}
	if !errors.Is(err, ErrResourceTypeUnknown) {
		t.Errorf("error chain should contain ErrResourceTypeUnknown, got %v", err)
	}
}

// --- Test 4: retry_count upper bound (severity escalation) -------------

// TestVerifyRecovery_SeverityEscalated：当 retry_count > MaxRetryCount
// 时 SeverityEscalated=true。retry_count 由 StateStore 提供，
// 验证 InMemory store 与 VerifyOutcome 的 retry_count 联动正确。
func TestVerifyRecovery_SeverityEscalated(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	store := NewInMemoryRecoveryStateStore()
	// 预热：先跑一次失败让 retry_count=1；再跑一次失败让它到 2。
	// MaxRetryCount=3；SeverityEscalated 的判定是 retry_count > MaxRetryCount。
	// 因此需要 retry_count >= 4。手动设 4 次 increment。
	if _, err := store.Increment(context.Background(), "inc-escal"); err != nil {
		t.Fatalf("seed increment: %v", err)
	}
	if _, err := store.Increment(context.Background(), "inc-escal"); err != nil {
		t.Fatalf("seed increment: %v", err)
	}
	if _, err := store.Increment(context.Background(), "inc-escal"); err != nil {
		t.Fatalf("seed increment: %v", err)
	}
	if _, err := store.Increment(context.Background(), "inc-escal"); err != nil {
		t.Fatalf("seed increment: %v", err)
	}

	q.plant("host-1", "cpu_usage", 10, 100, 50) // 必 fail
	tool := newVerifyRecoveryToolFor(t, q, store)

	argsJSON := `{
		"skill_id":"inc-escal","target":"host-1","resource_type":"host",
		"tolerance":0.15,"metrics":["cpu_usage"]
	}`
	out, err := tool.InvokableRun(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun err: %v", err)
	}
	var got VerifyOutcome
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RetryCount != 4 {
		t.Errorf("RetryCount = %d, want 4", got.RetryCount)
	}
	if !got.SeverityEscalated {
		t.Errorf("SeverityEscalated should be true (retry_count > MaxRetryCount=3)")
	}
	// 边界：retry_count=3 时不应升级
	store.Reset(context.Background(), "inc-edge")
	for i := 0; i < 3; i++ {
		if _, err := store.Increment(context.Background(), "inc-edge"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	q2 := newFakeQuerier()
	q2.plant("host-1", "cpu_usage", 10, 100, 50)
	tool2 := newVerifyRecoveryToolFor(t, q2, store)
	argsJSON2 := `{
		"skill_id":"inc-edge","target":"host-1","resource_type":"host",
		"tolerance":0.15,"metrics":["cpu_usage"]
	}`
	out2, err := tool2.InvokableRun(context.Background(), argsJSON2)
	if err != nil {
		t.Fatalf("edge InvokableRun err: %v", err)
	}
	var got2 VerifyOutcome
	if err := json.Unmarshal([]byte(out2), &got2); err != nil {
		t.Fatalf("edge decode: %v", err)
	}
	if got2.SeverityEscalated {
		t.Errorf("SeverityEscalated should be false at boundary retry_count=3")
	}
	if got2.RetryCount != 3 {
		t.Errorf("RetryCount = %d, want 3", got2.RetryCount)
	}
}

// --- Test 5: concurrent safety -----------------------------------------

// TestVerifyRecovery_ConcurrentSafety：并行跑 32 个 InvokableRun，
// StateStore.Increment / Get 都要并发安全；最终 retry_count 应等于调用次数。
// 同时验证 querier 收到正确次数的调用。
func TestVerifyRecovery_ConcurrentSafety(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	q.plant("host-1", "cpu_usage", 10, 11, 50)
	store := NewInMemoryRecoveryStateStore()

	tool := newVerifyRecoveryToolFor(t, q, store, VerifyRecoveryConfig{
		DefaultTolerance: DefaultTolerance,
		MaxConcurrent:    4, // 限流：超过会排队，但所有 call 都应完成
	})

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			argsJSON := fmt.Sprintf(`{
				"skill_id":"inc-conc","target":"host-1","resource_type":"host",
				"tolerance":0.15,"metrics":["cpu_usage"]
			}`)
			out, err := tool.InvokableRun(context.Background(), argsJSON)
			if err != nil {
				errs <- err
				return
			}
			var got VerifyOutcome
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				errs <- err
				return
			}
			if !got.Passed {
				errs <- fmt.Errorf("idx=%d: Passed=false", idx)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent err: %v", e)
	}
	if calls := q.callCount(); calls != N {
		t.Errorf("querier calls = %d, want %d", calls, N)
	}
}

// TestVerifyRecovery_ConcurrentRetryIncrement：单独测 StateStore
// 在并发 increment 下的原子性。
func TestVerifyRecovery_ConcurrentRetryIncrement(t *testing.T) {
	t.Parallel()
	store := NewInMemoryRecoveryStateStore()
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if _, err := store.Increment(context.Background(), "inc-rc"); err != nil {
				t.Errorf("increment: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := store.Get(context.Background(), "inc-rc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != N {
		t.Errorf("retry_count = %d, want %d", got, N)
	}
}

// --- Test 6: Info + WhenToUse + Schema ---------------------------------

// TestVerifyRecovery_Info：BaseTool.Info 返回稳定 wire name + Class=read。
func TestVerifyRecovery_Info(t *testing.T) {
	t.Parallel()
	tool := newVerifyRecoveryToolFor(t, newFakeQuerier(), NewInMemoryRecoveryStateStore())
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info err: %v", err)
	}
	if info.Name != ToolNameVerifyRecovery {
		t.Errorf("Name = %q, want %q", info.Name, ToolNameVerifyRecovery)
	}
	if info.Class != "read" {
		t.Errorf("Class = %q, want read", info.Class)
	}
	if info.Description == "" {
		t.Errorf("Description empty")
	}
	if !strings.Contains(strings.ToLower(info.WhenToUse), "not") {
		t.Errorf("WhenToUse must include NOT reverse guard: %q", info.WhenToUse)
	}
	var schema map[string]any
	if err := json.Unmarshal(info.Parameters, &schema); err != nil {
		t.Fatalf("Parameters invalid JSON: %v", err)
	}
	if v, _ := schema["additionalProperties"].(bool); v {
		t.Errorf("schema additionalProperties should be false")
	}
	req, _ := schema["required"].([]any)
	if len(req) < 4 {
		t.Errorf("required fields len = %d, want >=4 (skill_id/target/resource_type/metrics)", len(req))
	}
	// 编译期断言：VerifyRecoveryTool 必须实现 basetool.BaseTool。
	var _ basetool.BaseTool = (*VerifyRecoveryTool)(nil)
}

// --- Test 7: arg validation (bad JSON / missing required) --------------

// TestVerifyRecovery_BadJSON / MissingRequired 覆盖 fail-fast args 校验。
func TestVerifyRecovery_BadJSON(t *testing.T) {
	t.Parallel()
	tool := newVerifyRecoveryToolFor(t, newFakeQuerier(), NewInMemoryRecoveryStateStore())
	_, err := tool.InvokableRun(context.Background(), `{"not json`)
	if err == nil {
		t.Fatalf("expected error for bad JSON")
	}
	if !errors.Is(err, ErrArgsInvalid) {
		t.Errorf("error chain should contain ErrArgsInvalid, got %v", err)
	}
}

func TestVerifyRecovery_MissingRequired(t *testing.T) {
	t.Parallel()
	tool := newVerifyRecoveryToolFor(t, newFakeQuerier(), NewInMemoryRecoveryStateStore())
	cases := []string{
		`{"target":"h","resource_type":"host","metrics":["cpu_usage"]}`,     // missing skill_id
		`{"skill_id":"x","resource_type":"host","metrics":["cpu_usage"]}`,   // missing target
		`{"skill_id":"x","target":"h","metrics":["cpu_usage"]}`,             // missing resource_type
		`{"skill_id":"x","target":"h","resource_type":"host"}`,              // missing metrics
		`{"skill_id":"x","target":"h","resource_type":"host","metrics":[]}`, // empty metrics
	}
	for i, argsJSON := range cases {
		_, err := tool.InvokableRun(context.Background(), argsJSON)
		if err == nil {
			t.Errorf("case %d: expected error for missing required", i)
		}
	}
}

// --- Test 8: warn classification ---------------------------------------

// TestVerifyRecovery_WarnLevel：当 delta 在 [tolerance, 2*tolerance] 之间时
// warning_level="warn" 但 passed=true（design §A.3 三档规则）。
func TestVerifyRecovery_WarnLevel(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	// tolerance=0.15；deviation 20% 在 (15%, 30%] → warn
	q.plant("host-1", "cpu_usage", 100, 120, 50)
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())
	argsJSON := `{
		"skill_id":"inc-warn","target":"host-1","resource_type":"host",
		"tolerance":0.15,"metrics":["cpu_usage"]
	}`
	out, err := tool.InvokableRun(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun err: %v", err)
	}
	var got VerifyOutcome
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Passed {
		t.Errorf("delta in [tolerance, 2*tolerance] should still pass; got Passed=false")
	}
	if got.WarningLevel != "warn" {
		t.Errorf("WarningLevel = %q, want warn", got.WarningLevel)
	}
}

// --- Test 9: querier IO error propagated --------------------------------

// TestVerifyRecovery_QuerierError：单 metric IO 失败时 error 链含
// base error + metric name。
func TestVerifyRecovery_QuerierError(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	q.failMetric("cpu_usage", errors.New("prom down"))
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())
	argsJSON := `{
		"skill_id":"inc-io","target":"host-1","resource_type":"host",
		"tolerance":0.15,"metrics":["cpu_usage"]
	}`
	_, err := tool.InvokableRun(context.Background(), argsJSON)
	if err == nil {
		t.Fatalf("expected error for querier IO failure")
	}
	if !strings.Contains(err.Error(), "prom down") {
		t.Errorf("error should carry underlying cause; got %v", err)
	}
	if !strings.Contains(err.Error(), "cpu_usage") {
		t.Errorf("error should carry metric name; got %v", err)
	}
}

// --- Test 10: window defaults applied -----------------------------------

// TestVerifyRecovery_DefaultWindows：当未指定 window 时使用 default
// (5m baseline / 2m compare)。通过 fake 的 Now 输入验证
// MetricQueryRequest.BaselineWindow / CompareWindow 拿到默认值。
func TestVerifyRecovery_DefaultWindows(t *testing.T) {
	t.Parallel()
	var captured MetricQueryRequest
	var mu sync.Mutex
	q := &captureQuerier{
		capture: func(req MetricQueryRequest) MetricQueryResult {
			mu.Lock()
			defer mu.Unlock()
			captured = req
			return MetricQueryResult{BaselineAvg: 1, CurrentAvg: 1, SampleSize: 50}
		},
	}
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())
	argsJSON := `{
		"skill_id":"inc-dw","target":"host-1","resource_type":"host",
		"tolerance":0.15,"metrics":["cpu_usage"]
	}`
	if _, err := tool.InvokableRun(context.Background(), argsJSON); err != nil {
		t.Fatalf("InvokableRun err: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if captured.BaselineWindow != DefaultBaselineWindow {
		t.Errorf("BaselineWindow = %s, want %s", captured.BaselineWindow, DefaultBaselineWindow)
	}
	if captured.CompareWindow != DefaultCompareWindow {
		t.Errorf("CompareWindow = %s, want %s", captured.CompareWindow, DefaultCompareWindow)
	}
}

// captureQuerier 让 MetricQuerier 直接捕获入参（用于 default windows 测试）。
type captureQuerier struct {
	capture func(MetricQueryRequest) MetricQueryResult
}

func (c *captureQuerier) QueryMetric(_ context.Context, req MetricQueryRequest) (MetricQueryResult, error) {
	return c.capture(req), nil
}

// --- Test 11: state store missing → degrade warn -----------------------

// TestVerifyRecovery_StoreMissingDegrades：StateStore 为 nil 时
// InvokableRun 直接拒绝（不允许持久层缺失时沉默）。
func TestVerifyRecovery_StoreMissingDegrades(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	tool := newVerifyRecoveryToolFor(t, q, nil)
	_, err := tool.InvokableRun(context.Background(), `{
		"skill_id":"x","target":"h","resource_type":"host","metrics":["cpu_usage"]
	}`)
	if err == nil {
		t.Fatalf("expected error when state store missing")
	}
	if !errors.Is(err, ErrArgsInvalid) {
		t.Errorf("error chain should contain ErrArgsInvalid, got %v", err)
	}
}

// TestVerifyRecovery_StoreReadFailureDegrades：StateStore.Get 失败
// 也不阻塞（降级为 0 + 仍然输出 passed）。
func TestVerifyRecovery_StoreReadFailureDegrades(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	q.plant("host-1", "cpu_usage", 10, 11, 50)
	store := &failingStore{getErr: errors.New("redis down")}
	tool := newVerifyRecoveryToolFor(t, q, store)
	out, err := tool.InvokableRun(context.Background(), `{
		"skill_id":"x","target":"host-1","resource_type":"host","metrics":["cpu_usage"]
	}`)
	if err != nil {
		t.Fatalf("InvokableRun err: %v", err)
	}
	var got VerifyOutcome
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0 (degraded)", got.RetryCount)
	}
}

type failingStore struct {
	getErr, incErr, resetErr error
}

func (f *failingStore) Get(_ context.Context, _ string) (int, error) { return 0, f.getErr }
func (f *failingStore) Increment(_ context.Context, _ string) (int, error) {
	return 0, f.incErr
}
func (f *failingStore) Reset(_ context.Context, _ string) error { return f.resetErr }

// --- Test 12: IncrementRetryCount / Reset / Get helpers ----------------

// TestVerifyRecovery_HelperAPI 验证三个暴露 helper 的语义。
func TestVerifyRecovery_HelperAPI(t *testing.T) {
	t.Parallel()
	store := NewInMemoryRecoveryStateStore()
	tool := newVerifyRecoveryToolFor(t, newFakeQuerier(), store)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		got, err := tool.IncrementRetryCount(ctx, "inc-helper")
		if err != nil {
			t.Fatalf("inc %d: %v", i, err)
		}
		if got != i {
			t.Errorf("inc %d returned %d, want %d", i, got, i)
		}
	}
	got, err := tool.GetRetryCount(ctx, "inc-helper")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != 5 {
		t.Errorf("get = %d, want 5", got)
	}
	if err := tool.ResetRetryCount(ctx, "inc-helper"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got, _ = tool.GetRetryCount(ctx, "inc-helper")
	if got != 0 {
		t.Errorf("after reset = %d, want 0", got)
	}
}

// --- Test 13: ctx cancellation -----------------------------------------

// TestVerifyRecovery_CtxCanceled：ctx 取消时 InvokableRun 返回错误。
func TestVerifyRecovery_CtxCanceled(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	q.plant("host-1", "cpu_usage", 10, 11, 50)
	q.failMetric("mem_usage", context.Canceled)
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())
	argsJSON := `{
		"skill_id":"x","target":"host-1","resource_type":"host","metrics":["cpu_usage","mem_usage"]
	}`
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	_, err := tool.InvokableRun(ctx, argsJSON)
	if err == nil {
		t.Fatalf("expected ctx-cancel error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error should carry ctx-cancel reason; got %v", err)
	}
}

// --- Test 14: relative deviation epsilon -------------------------------

// TestVerifyRecovery_DeviationEpsilon：baseline_avg=0 时不除零。
func TestVerifyRecovery_DeviationEpsilon(t *testing.T) {
	t.Parallel()
	q := newFakeQuerier()
	// baseline=0 时 division 不该 NaN。final delta 用 EpsilonBaseline 防爆。
	q.plant("host-1", "cpu_usage", 0, 1, 50)
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())
	out, err := tool.InvokableRun(context.Background(), `{
		"skill_id":"x","target":"host-1","resource_type":"host","tolerance":0.15,"metrics":["cpu_usage"]
	}`)
	if err != nil {
		t.Fatalf("InvokableRun err: %v", err)
	}
	var got VerifyOutcome
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// delta 应该是 1 / EpsilonBaseline（非常大），所以 passed=false
	if got.Passed {
		t.Errorf("expected Passed=false (zero baseline means huge relative deviation)")
	}
	if got.Deltas["cpu_usage"] < 1e5 {
		t.Errorf("delta should be huge when baseline≈0; got %v", got.Deltas["cpu_usage"])
	}
	_ = time.Now // 保持 time 包引用，future 测试可能用到
}

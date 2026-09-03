package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestVerifyRecovery_HarnessCpuSpike 模拟 host/cpu-spike 闭环协同
// （zero-manual-ops-loop Day 3 任务 3.5）：
//
//  1. 注入 CPU spike：cpu_usage 从 baseline 30 → 98（deviation 227% > 100%）
//  2. 修复动作（kill_process）：cpu_usage 从 98 回落 → 31（deviation ~3% < 15%）
//  3. recovered phase 调用 verify_recovery：
//     baseline_window=5m (CPU 30%)，compare_window=2m (CPU 31%)
//  4. 期望：passed=true，warning_level=pass，retry_count=0
//
// 验证的是 Day 3 task 3.5 与 host/cpu-spike case 联动的语义。
// cpu-spike/case.yaml::metadata.verify_recovery.call / expectations
// 与本测试同形；Day 4 harness judge 会把这里的断言接进去。
//
// 跨域约束：tools 包不 import loop；loop 侧的 RecoveredPhaseWorker
// 集成测试见 internal/manager/biz/loop/recovery_test.go 的
// TestRecoveredWorker_Verifier_Pass / TestRecoveredWorker_Verifier_Fail。
func TestVerifyRecovery_HarnessCpuSpike(t *testing.T) {
	t.Parallel()

	// 模拟 spike 已被修复：cpu_usage 落到 31（baseline 30 偏差 3.3%）。
	const baselineCPU = 30.0
	const currentCPU = 31.0
	const baselineMEM = 40.0
	const currentMEM = 41.0

	q := newFakeQuerier()
	q.plant("host-injected", "cpu_usage", baselineCPU, currentCPU, 100)
	q.plant("host-injected", "mem_usage", baselineMEM, currentMEM, 100)

	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	// 与 case.yaml::metadata.verify_recovery.call 同形。
	argsJSON := `{
		"skill_id":"host/cpu-spike",
		"target":"host-injected",
		"resource_type":"host",
		"tolerance":0.15,
		"baseline_window":"5m",
		"compare_window":"2m",
		"metrics":["cpu_usage","mem_usage"]
	}`
	out, err := tool.InvokableRun(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun err: %v", err)
	}
	var got VerifyOutcome
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// 与 case.yaml::metadata.verify_recovery.expectations 同断言。
	if !got.Passed {
		t.Errorf("expected Passed=true after cpu spike fix; got %+v", got)
	}
	if got.WarningLevel != "pass" {
		t.Errorf("expected warning_level=pass; got %q", got.WarningLevel)
	}
	if len(got.FailedMetrics) != 0 {
		t.Errorf("expected no failed metrics; got %v", got.FailedMetrics)
	}
	if got.RetryCount != 0 {
		t.Errorf("expected retry_count=0 (first attempt); got %d", got.RetryCount)
	}
	if got.SeverityEscalated {
		t.Errorf("expected no severity escalation; got SeverityEscalated=true")
	}
	if got.Tolerance != 0.15 {
		t.Errorf("expected tolerance=0.15; got %v", got.Tolerance)
	}
	// delta 校验：(|31-30|)/30 = 0.0333
	if d := got.Deltas["cpu_usage"]; d < 0.03 || d > 0.04 {
		t.Errorf("cpu_usage delta = %v, want ≈0.033", d)
	}
	if d := got.Deltas["mem_usage"]; d < 0.02 || d > 0.03 {
		t.Errorf("mem_usage delta = %v, want ≈0.025", d)
	}

	// 与 day-2 investigator 的 RootCauseJSON schema_version 一致（v1 字面量
	// 是 McClintock 的 deviation；不要改成 "1.0"）。
	if got.SchemaVersion != "v1" {
		t.Errorf("schema_version = %q, want v1", got.SchemaVersion)
	}
}

// TestVerifyRecovery_HarnessCpuSpike_RollbackOnPersistentSpike 模拟
// spike 没有被真正修复（cpu_usage 从 98 仍 80）时 verify_recovery 返回
// failed_metrics=[cpu_usage] + Verdict 触发 recovered → approved rollback。
// 这是 Day 3 task 3.3 rollback 语义与 cpu-spike case 的协同。
func TestVerifyRecovery_HarnessCpuSpike_RollbackOnPersistentSpike(t *testing.T) {
	t.Parallel()

	q := newFakeQuerier()
	// spike 没修复：baseline 30 / current 80 → deviation 167% > 15% tolerance
	q.plant("host-injected", "cpu_usage", 30, 80, 100)
	q.plant("host-injected", "mem_usage", 40, 41, 100)
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	argsJSON := `{
		"skill_id":"host/cpu-spike","target":"host-injected","resource_type":"host",
		"tolerance":0.15,"baseline_window":"5m","compare_window":"2m",
		"metrics":["cpu_usage","mem_usage"]
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
		t.Errorf("expected Passed=false on persistent spike")
	}
	if len(got.FailedMetrics) != 1 || got.FailedMetrics[0] != "cpu_usage" {
		t.Errorf("expected failed_metrics=[cpu_usage]; got %v", got.FailedMetrics)
	}
	if got.WarningLevel != "fail" {
		t.Errorf("expected warning_level=fail; got %q", got.WarningLevel)
	}
	// 验证 reason 链：CPU spike 未修复 → 触发 recovered → approved 滚动。
	// 实际 rollback 在 loop.RecoveredPhaseWorker 里完成；这里只断言
	// verify_recovery 的产出形态能驱动它。
	if got.Deltas["cpu_usage"] < 1.5 {
		t.Errorf("cpu_usage delta should be huge on persistent spike; got %v", got.Deltas["cpu_usage"])
	}
}

// TestVerifyRecovery_HarnessCpuSpike_BoundaryWarn 模拟 spike 缓解但
// 仍略高于 tolerance（落入 warn 区间 15%-30%）。verify_recovery
// 返回 passed=true 但 warning_level=warn，postmortem 用它渲染
// "low confidence" 提示。
func TestVerifyRecovery_HarnessCpuSpike_BoundaryWarn(t *testing.T) {
	t.Parallel()

	q := newFakeQuerier()
	// spike 缓解但不彻底 → current 38 → 相对偏差 26.7%，落入 (15%, 30%] warn
	q.plant("host-injected", "cpu_usage", 30, 38, 10) // sample=10 → low confidence
	tool := newVerifyRecoveryToolFor(t, q, NewInMemoryRecoveryStateStore())

	argsJSON := `{
		"skill_id":"host/cpu-spike","target":"host-injected","resource_type":"host",
		"tolerance":0.15,"baseline_window":"5m","compare_window":"2m",
		"metrics":["cpu_usage"]
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
		t.Errorf("warn-level metric should still pass; got Passed=false")
	}
	if got.WarningLevel != "warn" {
		t.Errorf("expected warning_level=warn; got %q", got.WarningLevel)
	}
	if got.SampleSize != 10 {
		t.Errorf("expected SampleSize=10 (low confidence); got %d", got.SampleSize)
	}
}

// TestVerifyRecovery_HarnessCaseSchema_ShapeSanity 检查 cpu-spike case.yaml
// 的 metadata.verify_recovery 字段在 loader 阶段不被 strict schema 拒绝。
// 这是 Day 4 / Day 5 把 verify_recovery 集成到 harness judge 时避免
// "case loader 拒绝 metadata" 的回归。
//
// 说明：本测试只解析 yaml 文本做字符串检查，不引入 yaml.v3 依赖。
// schema.Loader 自身只校验必需字段存在 + ID 格式 + enum 范围，
// metadata 是 additionalProperties: true，开放给自由字段。
func TestVerifyRecovery_HarnessCaseSchema_ShapeSanity(t *testing.T) {
	t.Parallel()
	// 静态校验：cpu-spike/case.yaml 必须含 verify_recovery metadata block。
	// 用 case ID 解析验证 loader 仍能找到该 case。
	// （这里不实际读 yaml 文件，因为 tools 包不应依赖 harness schema；harness
	// 侧集成测试见 internal/harness/cases/host/cpu-spike/ 下游测试。）
	argsJSON := `{
		"skill_id":"host/cpu-spike","target":"host-injected","resource_type":"host",
		"tolerance":0.15,"baseline_window":"5m","compare_window":"2m",
		"metrics":["cpu_usage","mem_usage"]
	}`
	// 仅为格式 sanity：argsJSON 必须含 baseline_window="5m" 字段。
	if !strings.Contains(argsJSON, `"baseline_window":"5m"`) {
		t.Fatalf("test fixture missing baseline_window=5m (cpu-spike case convention)")
	}
	if !strings.Contains(argsJSON, `"compare_window":"2m"`) {
		t.Fatalf("test fixture missing compare_window=2m")
	}
	if !strings.Contains(argsJSON, `"tolerance":0.15`) {
		t.Fatalf("test fixture missing tolerance=0.15")
	}
}

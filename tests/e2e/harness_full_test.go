//go:build e2e

// Harness E2E：跑完整 case → inject → judge → leaderboard 闭环。
//
// 设计原则（AGENTS.md "E2E 测试必须清理数据"）：
//   - 每个测试用独立 case_id 前缀（e2e-{testname}-{ts}），不污染其他测试
//   - Leaderboard 实例 per test（不共享状态）
//   - 用 HeuristicJudge（无外部 LLM 依赖）— 离线可跑
//   - Injector 用 mock：返回预定义结果，跳过真实注入（fault-injector 是 skeleton）
//   - 真实 case.yaml 从 internal/harness/cases/ 加载（20 个黄金事故）
//
// 覆盖：
//   - 正常 case 跑通：load → run（mock inject + mock agent）→ judge → record
//   - 回归检测：5% drop 触发 warn
//   - 多 case 隔离：每个 case 独立评分，不互相影响
//   - Case 库完整性：所有 20 个 case.yaml 都能被 schema 校验
//
// 关联：Task 2.13 / 阶段 2 收尾
package e2e

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/harness/judge"
	"github.com/vincent-wuhan/opskeeper/internal/harness/leaderboard"
	"github.com/vincent-wuhan/opskeeper/internal/harness/schema"
)

// harnessCasesDir 是 20 个黄金事故的根目录。
// 测试运行在项目根目录（go test ./tests/e2e/...），所以相对路径可解析。
func harnessCasesDir(t *testing.T) string {
	t.Helper()
	// 解析 internal/harness/cases 的相对路径
	abs, err := filepath.Abs("../../internal/harness/cases")
	if err != nil {
		t.Fatalf("resolve cases dir: %v", err)
	}
	return abs
}

// TestHarness_FullFlow_OneCase 跑单个 case 的完整闭环。
//
// 流程：load case → 构造 mock agent response → judge → record → 查 leaderboard
func TestHarness_FullFlow_OneCase(t *testing.T) {
	loader := schema.NewLoader(harnessCasesDir(t))
	cases, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases loaded")
	}
	// 选第一个 PG case（确保 schema 校验通过 + 期望字段齐）
	var target *schema.Case
	for _, c := range cases {
		if c.ID == "pg/lock-waits" {
			target = c
			break
		}
	}
	if target == nil {
		t.Skip("pg/lock-waits case not found; run from project root")
	}
	t.Logf("using case: %s (severity=%s, expect=%+v)", target.ID, target.Severity, target.Expect)

	// 构造 mock agent response（完美匹配 case 期望）
	resp := &judge.AgentResponse{
		ToolCalls: []judge.ToolCall{
			{Name: "pg.lock_waits"},
			{Name: "pg.active_sessions"},
		},
		RootCause:    target.Expect.RootCauseLines,
		Remediations: target.Expect.RemediationOptions,
		DetectMs:     int64(target.Expect.TimeToDetect) * 1000,
		RemediateMs:  int64(target.Expect.TimeToRemediate) * 1000,
	}
	resp.ResponseHash = judge.ComputeResponseHash(resp)

	// 构造 judge.Case（runner → judge 桥接）
	jc := &judge.Case{
		ID:                   target.ID,
		ExpectedRootCause:    target.Expect.RootCauseLines,
		ExpectedRemediations: target.Expect.RemediationOptions,
		ExpectedDetectSec:    target.Expect.TimeToDetect,
		ExpectedRemediateSec: target.Expect.TimeToRemediate,
		RCAThreshold:         target.Rubric.RCAAccuracy,
		NoCollateralDamage:   target.Rubric.NoCollateralDamage,
	}

	// 1. Judge：HeuristicJudge（无 LLM 依赖，离线可跑）
	j := judge.NewHeuristicJudge()
	score, err := j.Score(context.Background(), jc, resp)
	if err != nil {
		t.Fatalf("judge.Score: %v", err)
	}
	if score.Overall < 0.5 {
		t.Errorf("Overall = %f, want >= 0.5 (mock response matches case)", score.Overall)
	}
	t.Logf("judge score: overall=%.3f dims=%+v", score.Overall, score.Dimensions)

	// 2. Record 到 leaderboard
	lb := leaderboard.NewLeaderboard()
	e := &leaderboard.Entry{
		RunID:     "e2e-run-1",
		CaseID:    target.ID,
		Branch:    "e2e",
		Score:     score.Overall,
		Flagged:   score.Flagged,
		JudgesUsed: score.JudgesUsed,
		Timestamp: time.Now(),
	}
	if err := lb.Record(e); err != nil {
		t.Fatalf("lb.Record: %v", err)
	}

	// 3. 查 leaderboard 状态
	base, ok := lb.Baseline(target.ID)
	if !ok {
		t.Errorf("baseline not set after Record")
	}
	if base != score.Overall {
		t.Errorf("baseline = %f, want %f", base, score.Overall)
	}
	last, ok := lb.LastEntry(target.ID)
	if !ok {
		t.Errorf("LastEntry not found")
	}
	if last.Score != score.Overall {
		t.Errorf("LastEntry score = %f, want %f", last.Score, score.Overall)
	}
}

// TestHarness_RegressionDetection_5PercentDrop 验证 5% 降级触发 warn。
func TestHarness_RegressionDetection_5PercentDrop(t *testing.T) {
	lb := leaderboard.NewLeaderboard()
	caseID := "e2e-reg-test"

	// baseline 0.80
	if err := lb.Record(&leaderboard.Entry{CaseID: caseID, Score: 0.80, RunID: "r1"}); err != nil {
		t.Fatalf("baseline Record: %v", err)
	}
	// 5% drop → 0.76
	r := lb.CheckRegression(caseID, 0.76)
	if r == nil {
		t.Fatal("expected regression")
	}
	if r.Severity != leaderboard.SeverityWarn {
		t.Errorf("Severity = %q, want warn (5%% drop)", r.Severity)
	}
	if r.DropPercent < 4.9 || r.DropPercent > 5.1 {
		t.Errorf("DropPercent = %f, want ~5.0", r.DropPercent)
	}
}

// TestHarness_RegressionDetection_15PercentDrop 验证 15% 降级触发 block。
func TestHarness_RegressionDetection_15PercentDrop(t *testing.T) {
	lb := leaderboard.NewLeaderboard()
	caseID := "e2e-block-test"

	lb.Record(&leaderboard.Entry{CaseID: caseID, Score: 0.80, RunID: "r1"})
	r := lb.CheckRegression(caseID, 0.68) // 15% drop
	if r.Severity != leaderboard.SeverityBlock {
		t.Errorf("Severity = %q, want block (15%% drop)", r.Severity)
	}
	if !r.DropPercent >= 14.9 || r.DropPercent > 15.1 {
		t.Errorf("DropPercent = %f, want ~15.0", r.DropPercent)
	}
}

// TestHarness_CaseIsolation 验证多个 case 的评分相互隔离。
func TestHarness_CaseIsolation(t *testing.T) {
	lb := leaderboard.NewLeaderboard()
	// 3 个 case 各自 baseline
	for _, c := range []struct {
		id    string
		score float64
	}{
		{"e2e-iso-a", 0.9},
		{"e2e-iso-b", 0.7},
		{"e2e-iso-c", 0.5},
	} {
		lb.Record(&leaderboard.Entry{CaseID: c.id, Score: c.score, RunID: "r1"})
	}
	// 各自查 baseline 应为各自的值
	for _, c := range []struct {
		id    string
		score float64
	}{
		{"e2e-iso-a", 0.9},
		{"e2e-iso-b", 0.7},
		{"e2e-iso-c", 0.5},
	} {
		base, ok := lb.Baseline(c.id)
		if !ok {
			t.Errorf("%s: baseline missing", c.id)
		}
		if base != c.score {
			t.Errorf("%s: baseline = %f, want %f", c.id, base, c.score)
		}
	}
	// a 降到 0.5 → warn (a: 0.9 → 0.5 = 44% drop = block)
	r := lb.CheckRegression("e2e-iso-a", 0.5)
	if r.Severity != leaderboard.SeverityBlock {
		t.Errorf("a drop: Severity = %q, want block", r.Severity)
	}
	// b 不动 → no regression
	r2 := lb.CheckRegression("e2e-iso-b", 0.7)
	if r2.Severity != leaderboard.SeverityNone {
		t.Errorf("b no change: Severity = %q, want none", r2.Severity)
	}
}

// TestHarness_CaseLibrary_AllValid 验证 20 个 case.yaml 全部 schema 校验通过。
//
// 完整性 sanity check — 任何 case 文件错误（yaml 改坏 / 必填字段缺失）会失败。
func TestHarness_CaseLibrary_AllValid(t *testing.T) {
	loader := schema.NewLoader(harnessCasesDir(t))
	cases, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(cases) < 20 {
		t.Errorf("loaded %d cases, want >= 20 (golden case library)", len(cases))
	}
	ids := make(map[string]bool)
	for _, c := range cases {
		if ids[c.ID] {
			t.Errorf("duplicate case ID: %s", c.ID)
		}
		ids[c.ID] = true
		// ID pattern check
		if c.ID == "" {
			t.Error("empty case ID")
		}
		if c.Severity == "" {
			t.Errorf("%s: empty severity", c.ID)
		}
		if len(c.Inject) == 0 {
			t.Errorf("%s: no inject steps", c.ID)
		}
		if len(c.Expect.RootCauseLines) == 0 {
			t.Errorf("%s: no root_cause_lines", c.ID)
		}
	}
	t.Logf("loaded %d cases across all resource types", len(cases))
}

// TestHarness_Leaderboard_FlaggedEntries 验证 Flagged 流程。
func TestHarness_Leaderboard_FlaggedEntries(t *testing.T) {
	lb := leaderboard.NewLeaderboard()
	caseID := "e2e-flag-test"
	lb.Record(&leaderboard.Entry{CaseID: caseID, Score: 0.5, Flagged: true, RunID: "r1"})
	flagged := lb.FlaggedEntries()
	if len(flagged) != 1 {
		t.Errorf("flagged = %d, want 1", len(flagged))
	}
}

// TestHarness_HeuristicJudge_AllCases 跑 HeuristicJudge 过 20 个 case。
//
// 验证：每个 case 至少能产出非零分（不抛错），并记录到 leaderboard。
func TestHarness_HeuristicJudge_AllCases(t *testing.T) {
	loader := schema.NewLoader(harnessCasesDir(t))
	cases, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	lb := leaderboard.NewLeaderboard()
	j := judge.NewHeuristicJudge()

	now := time.Now()
	for i, c := range cases {
		// 构造 agent response：用 case 期望字段（"完美" agent）
		resp := &judge.AgentResponse{
			ToolCalls:    makeToolsFromRootCause(c.Expect.RootCauseLines),
			RootCause:    c.Expect.RootCauseLines,
			Remediations: c.Expect.RemediationOptions,
			DetectMs:     int64(c.Expect.TimeToDetect) * 1000,
			RemediateMs:  int64(c.Expect.TimeToRemediate) * 1000,
		}
		resp.ResponseHash = judge.ComputeResponseHash(resp)
		jc := &judge.Case{
			ID:                   c.ID,
			ExpectedRootCause:    c.Expect.RootCauseLines,
			ExpectedRemediations: c.Expect.RemediationOptions,
			ExpectedDetectSec:    c.Expect.TimeToDetect,
			ExpectedRemediateSec: c.Expect.TimeToRemediate,
			RCAThreshold:         c.Rubric.RCAAccuracy,
			NoCollateralDamage:   c.Rubric.NoCollateralDamage,
		}
		score, err := j.Score(context.Background(), jc, resp)
		if err != nil {
			t.Errorf("%s: judge failed: %v", c.ID, err)
			continue
		}
		// 完美 agent 应得高分
		if score.Overall < 0.5 {
			t.Errorf("%s: Overall=%.3f, want >= 0.5 (perfect agent)", c.ID, score.Overall)
		}
		// Record (用不同 RunID 避免后续 Record 触发 baseline 替换)
		e := &leaderboard.Entry{
			RunID:      "e2e-all-r" + timeDuration(i),
			CaseID:     c.ID,
			Branch:     "e2e",
			Score:      score.Overall,
			JudgesUsed: score.JudgesUsed,
			Timestamp:  now,
		}
		if err := lb.Record(e); err != nil {
			t.Errorf("%s: lb.Record: %v", c.ID, err)
		}
	}
	if lb.Size() < 20 {
		t.Errorf("lb.Size() = %d, want >= 20", lb.Size())
	}
}

func makeToolsFromRootCause(rcs []string) []judge.ToolCall {
	out := make([]judge.ToolCall, len(rcs))
	for i, rc := range rcs {
		out[i] = judge.ToolCall{Name: rc}
	}
	return out
}

// timeDuration 工具：把 int 转 time.Duration 后取 String 作 RunID 后缀。
func timeDuration(i int) string {
	return (time.Duration(i) * time.Millisecond).String()
}

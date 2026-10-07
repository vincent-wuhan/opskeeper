//go:build e2e

// Harness E2E：跑完整 case → inject → judge → leaderboard 闭环。
//
// 设计原则（AGENTS.md "E2E 测试必须清理数据"）：
//   - 每个测试用独立 case_id 前缀（e2e-{testname}-{ts}），不污染其他测试
//   - Leaderboard 实例 per test（不共享状态）
//   - 用 HeuristicJudge（无外部 LLM 依赖）— 离线可跑
//   - Injector 用 mock：返回预定义结果，跳过真实注入（fault-injector 是 skeleton）
//   - 真实 case.yaml 从 core/harness/cases/ 加载（20 个黄金事故）
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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/harness/judge"
	"github.com/vincent-wuhan/opskeeper/core/harness/leaderboard"
	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

// harnessCasesDir 是 20 个黄金事故的根目录。
// 测试运行在项目根目录（go test ./tests/e2e/...），所以相对路径可解析。
func harnessCasesDir(t *testing.T) string {
	t.Helper()
	// 解析 core/harness/cases 的相对路径
	abs, err := filepath.Abs("../../core/harness/cases")
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
	// 断言对的是 case 自己声明的阈值，而不是写死的 0.5。此前这个 case 的
	// rubric.rca_accuracy 从未参与判定，e2e 只能拿一个魔数兜着，于是
	// 「阈值被忽略」这件事在整个测试套件里没有任何一处会红。
	if bar := target.Rubric.RCAAccuracy; bar > 0 {
		if rca := score.Dimensions["rca_accuracy"]; rca < bar {
			t.Errorf("rca_accuracy = %f, want >= declared %f", rca, bar)
		}
		if score.Flagged {
			t.Errorf("Flagged = true although rca_accuracy met the declared bar: %s", score.FlagReason)
		}
	}
	t.Logf("judge score: overall=%.3f rca_threshold=%.3f dims=%+v", score.Overall, target.Rubric.RCAAccuracy, score.Dimensions)

	// 2. 落一份 LoopResult，进看板。
	//
	// 这一段此前调的是 `leaderboard.NewLeaderboard()`——决策 295 删掉了那个
	// 269 行的纯内存类型（零生产调用方），而**这个文件仍在调它**，于是整个
	// e2e 包从那一刻起编译不过。没人发现是因为它带 `//go:build e2e`，
	// 十二道本地闸门与 `go test ./...` 都碰不到它，而 CI 上的 e2e job
	// 连红四个决策没人去看。**一个只被 e2e 调用的实现被判定为零调用方，
	// 这个判断本身是对的**——它确实没有生产调用方；错的是删掉之后没人
	// 跟着改唯一还在调它的地方。
	dir := t.TempDir()
	writeLoopResult(t, dir, loopResult(target.ID, true, uniformMetrics(score.Overall)))
	lb, err := leaderboard.NewLoopBoard(dir)
	if err != nil {
		t.Fatalf("NewLoopBoard: %v", err)
	}
	if len(lb.Entries) != 1 {
		t.Fatalf("board entries = %d, want 1", len(lb.Entries))
	}
	if got := lb.Entries[0].CaseID; got != target.ID {
		t.Errorf("entry case id = %q, want %q", got, target.ID)
	}
	if !lb.Entries[0].Passed {
		t.Error("a passing case did not come out as passed")
	}

	// 3. 锁基线并落盘，再读回来。
	//
	// 四个指标都设成 judge 的总分，所以聚合值必须正好等于它——聚合口径是
	// 已测指标的均值，未测量的跳过而不是当 0，而一个只设一个指标的 case
	// 也该只按那一个算。
	base, unmeasured := leaderboard.LockBaseline(lb, "e2e")
	if len(unmeasured) != 0 {
		t.Fatalf("every entry has metrics, yet %v were reported unmeasured", unmeasured)
	}
	got, ok := base.Scores[target.ID]
	if !ok {
		t.Fatalf("baseline has no score for %q", target.ID)
	}
	if got != score.Overall {
		t.Errorf("baseline = %f, want %f", got, score.Overall)
	}
	path := filepath.Join(dir, "baseline.json")
	if err := leaderboard.SaveBaseline(path, base); err != nil {
		t.Fatalf("SaveBaseline: %v", err)
	}
	loaded, err := leaderboard.LoadBaseline(path)
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if loaded.Scores[target.ID] != score.Overall {
		t.Errorf("round-tripped baseline = %f, want %f", loaded.Scores[target.ID], score.Overall)
	}

	// 4. 拿同一份看板对同一份基线查回归：一个 case 都没有动过。
	rep := leaderboard.CheckBoard(lb, base)
	if len(rep.Regressions) != 0 {
		t.Errorf("identical board reports regressions: %+v", rep.Regressions)
	}
	if rep.Worst() != leaderboard.SeverityNone {
		t.Errorf("Worst = %q, want none", rep.Worst())
	}
	if rep.Unaccounted() != 0 {
		t.Errorf("Unaccounted = %d, want 0", rep.Unaccounted())
	}
}

// TestHarness_RegressionDetection_5PercentDrop 验证 5% 降级触发 warn。
func TestHarness_RegressionDetection_5PercentDrop(t *testing.T) {
	caseID := "e2e-reg-test"
	base := baselineOf(caseID, 0.80)

	// 5% drop → 0.76
	r := leaderboard.CheckRegressionFor(base, caseID, 0.80, 0.76)
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
	caseID := "e2e-block-test"
	base := baselineOf(caseID, 0.80)

	r := leaderboard.CheckRegressionFor(base, caseID, 0.80, 0.68) // 15% drop
	if r.Severity != leaderboard.SeverityBlock {
		t.Errorf("Severity = %q, want block (15%% drop)", r.Severity)
	}
	if r.DropPercent < 14.9 || r.DropPercent > 15.1 {
		t.Errorf("DropPercent = %f, want ~15.0", r.DropPercent)
	}
}

// TestHarness_CaseIsolation 验证多个 case 的评分相互隔离。
func TestHarness_CaseIsolation(t *testing.T) {
	scored := []struct {
		id    string
		score float64
	}{
		{"e2e-iso-a", 0.9},
		{"e2e-iso-b", 0.7},
		{"e2e-iso-c", 0.5},
	}
	dir := t.TempDir()
	for _, c := range scored {
		writeLoopResult(t, dir, loopResult(c.id, true, uniformMetrics(c.score)))
	}
	lb, err := leaderboard.NewLoopBoard(dir)
	if err != nil {
		t.Fatalf("NewLoopBoard: %v", err)
	}
	base, _ := leaderboard.LockBaseline(lb, "e2e")

	// 各自查 baseline 应为各自的值
	for _, c := range scored {
		got, ok := base.Scores[c.id]
		if !ok {
			t.Errorf("%s: baseline missing", c.id)
		}
		if got != c.score {
			t.Errorf("%s: baseline = %f, want %f", c.id, got, c.score)
		}
	}
	// a 降到 0.5 → warn (a: 0.9 → 0.5 = 44% drop = block)
	r := leaderboard.CheckRegressionFor(base, "e2e-iso-a", 0.9, 0.5)
	if r.Severity != leaderboard.SeverityBlock {
		t.Errorf("a drop: Severity = %q, want block", r.Severity)
	}
	// b 不动 → no regression
	r2 := leaderboard.CheckRegressionFor(base, "e2e-iso-b", 0.7, 0.7)
	if r2.Severity != leaderboard.SeverityNone {
		t.Errorf("b no change: Severity = %q, want none", r2.Severity)
	}
	// c 没动过 → 整份看板不该报出任何回归
	if rep := leaderboard.CheckBoard(lb, base); len(rep.Regressions) != 0 {
		t.Errorf("an unchanged board reports regressions: %+v", rep.Regressions)
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
	// 这条用例的名字来自已删的那个内存类型（FlaggedEntries）。它断言的那个
	// 行为——**一条跑挂的 case 不许混进排行榜**——还在，只是现在由准入规则
	// 承担：recovery_pass_rate 低于门槛的 entry 不合格，且必须写明原因。
	// 一个不合格的 entry 带着空原因进榜，与一个合格的 entry 无法区分。
	dir := t.TempDir()
	writeLoopResult(t, dir, loopResult("e2e-ok-case", true, uniformMetrics(0.8)))
	// recovery_pass_rate 0.2 低于 0.5 门槛，所以这条不合格。
	writeLoopResult(t, dir, loopResult("e2e-failed-case", false,
		metrics{rca: 0.9, approval: 0.9, recovery: 0.2, kb: 0.9}))
	lb, err := leaderboard.NewLoopBoard(dir)
	if err != nil {
		t.Fatalf("NewLoopBoard: %v", err)
	}
	if lb.RecoveryPassRateThreshold != 0.5 {
		t.Errorf("threshold = %v, want 0.5", lb.RecoveryPassRateThreshold)
	}
	qualified, notQualified := 0, 0
	for _, e := range lb.Entries {
		if e.Qualified {
			qualified++
			continue
		}
		notQualified++
		if e.NotQualifiedReason == "" {
			t.Errorf("%s: not qualified but says nothing about why", e.CaseID)
		}
	}
	if qualified != 1 || notQualified != 1 {
		t.Errorf("qualified = %d, notQualified = %d, want 1 and 1", qualified, notQualified)
	}
	// 报告里必须有它，但必须**带标记**：藏起来比标出来更糟——一个 case
	// 从排行榜上消失，读者无从知道它是没跑还是没通过。
	report := lb.Render()
	if !strings.Contains(report, "e2e-failed-case") {
		t.Errorf("a case below the recovery threshold vanished from the report:\n%s", report)
	}
	if !strings.Contains(report, "NOT QUALIFIED") ||
		!strings.Contains(report, "NOT QUALIFIED details") {
		t.Errorf("the report does not mark the unqualified case:\n%s", report)
	}
	// 合格计数必须把它排除掉——"入榜"说的是这个，不是"出现在报告里"。
	if !strings.Contains(report, "**Qualified**: 1") {
		t.Errorf("the qualified count does not exclude the unqualified case:\n%s", report)
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
	dir := t.TempDir()
	j := judge.NewHeuristicJudge()
	for _, c := range cases {
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
		// 完美 agent 应得满分档：rca 压住 case 自报的阈值，且不被 flag。
		if bar := c.Rubric.RCAAccuracy; bar > 0 {
			if rca := score.Dimensions["rca_accuracy"]; rca < bar {
				t.Errorf("%s: rca_accuracy=%.3f, want >= declared %.3f", c.ID, rca, bar)
			}
			if score.Flagged {
				t.Errorf("%s: Flagged=true on a perfect response: %s", c.ID, score.FlagReason)
			}
		}
		// 落一份 LoopResult。每个 case 一个文件，文件名不同。
		writeLoopResult(t, dir, loopResult(c.ID, true, uniformMetrics(score.Overall)))
	}
	lb, err := leaderboard.NewLoopBoard(dir)
	if err != nil {
		t.Fatalf("NewLoopBoard: %v", err)
	}
	if len(lb.Entries) < 20 {
		t.Errorf("board entries = %d, want >= 20", len(lb.Entries))
	}
	// 每个 case 都测出了分数，于是没有一个该被算成"未测量"。
	base, unmeasured := leaderboard.LockBaseline(lb, "e2e-all")
	if len(unmeasured) != 0 {
		t.Errorf("%d case(s) reported unmeasured although all four metrics were set: %v",
			len(unmeasured), unmeasured)
	}
	if len(base.Scores) != len(lb.Entries) {
		t.Errorf("baseline covers %d case(s), board has %d", len(base.Scores), len(lb.Entries))
	}
}

func makeToolsFromRootCause(rcs []string) []judge.ToolCall {
	out := make([]judge.ToolCall, len(rcs))
	for i, rc := range rcs {
		out[i] = judge.ToolCall{Name: rc}
	}
	return out
}

// metrics 是写一份 LoopResult 时用的四个指标。与 leaderboard.BoardMetrics
// 同名同序，少写一个就会被聚合口径跳过——所以这里**只**用这个构造器，
// 不在测试里手写 JSON。
type metrics struct {
	rca      float64
	approval float64
	recovery float64
	kb       float64
}

// uniformMetrics 让四个指标都等于同一个值，于是聚合分数正好等于那个值。
// 用它来断言"基线 == judge 的总分"才成立；四个值不同时聚合是均值，
// 那条断言也就没有意义了。
func uniformMetrics(v float64) metrics {
	return metrics{rca: v, approval: v, recovery: v, kb: v}
}

// loopResult 造一份与 harness/result/loop 下形状一致的 LoopResult。
func loopResult(caseID string, passed bool, m metrics) *leaderboard.LoopResultJSON {
	rca, approval, recovery, kb := m.rca, m.approval, m.recovery, m.kb
	lr := &leaderboard.LoopResultJSON{
		IncidentID: "e2e-" + caseID,
		CaseID:     caseID,
		Mode:       "loop",
		FinalPhase: "done",
		Passed:     passed,
	}
	lr.Rubric.RCAAccuracy = &rca
	lr.Rubric.ApprovalRate = &approval
	lr.Rubric.RecoveryPassRate = &recovery
	lr.Rubric.KBHitRate = &kb
	return lr
}

func writeLoopResult(t *testing.T, dir string, lr *leaderboard.LoopResultJSON) {
	t.Helper()
	raw, err := json.MarshalIndent(lr, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", lr.CaseID, err)
	}
	name := filepath.Join(dir, fmt.Sprintf("%s.json", strings.ReplaceAll(lr.CaseID, "/", "_")))
	if err := os.WriteFile(name, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// baselineOf 造一份只含一个 case 的基线，供不依赖看板的阈值用例使用。
func baselineOf(caseID string, score float64) *leaderboard.Baseline {
	return &leaderboard.Baseline{
		LockedAt: time.Now().UTC(),
		LockedBy: "e2e",
		Scores:   map[string]float64{caseID: score},
	}
}

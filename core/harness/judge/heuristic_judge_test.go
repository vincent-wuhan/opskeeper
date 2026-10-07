package judge

import (
	"context"
	"strings"
	"testing"
)

func TestHeuristicJudge_Name(t *testing.T) {
	j := NewHeuristicJudge()
	if j.Name() != "heuristic-v1" {
		t.Errorf("Name = %q, want heuristic-v1", j.Name())
	}
}

func TestHeuristicJudge_PerfectScore(t *testing.T) {
	j := NewHeuristicJudge()
	c := &Case{
		ID:                   "pg/lock-waits",
		ExpectedRootCause:    []string{"pg.lock_waits", "pg.active_sessions"},
		ExpectedRemediations: []string{"pg.kill_session"},
		ExpectedDetectSec:    30,
		ExpectedRemediateSec: 60,
		NoCollateralDamage:   true,
	}
	r := &AgentResponse{
		RootCause:    []string{"pg.lock_waits", "pg.active_sessions"},
		Remediations: []string{"pg.kill_session"},
		DetectMs:     30_000, // equal to expected
		RemediateMs:  60_000,
	}
	s, err := j.Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if s.Overall < 0.95 {
		t.Errorf("Overall = %f, want ~1.0 (perfect)", s.Overall)
	}
}

func TestHeuristicJudge_PartialRCA(t *testing.T) {
	j := NewHeuristicJudge()
	c := &Case{
		ID:                "pg/lock-waits",
		ExpectedRootCause: []string{"pg.lock_waits", "pg.active_sessions"},
	}
	r := &AgentResponse{
		RootCause: []string{"pg.lock_waits"}, // only 1 of 2
	}
	s, _ := j.Score(context.Background(), c, r)
	if s.Dimensions["rca_accuracy"] != 0.5 {
		t.Errorf("rca_accuracy = %f, want 0.5 (1/2 hit)", s.Dimensions["rca_accuracy"])
	}
}

func TestHeuristicJudge_CollateralDamagePenalty(t *testing.T) {
	j := NewHeuristicJudge()
	c := &Case{
		ID:                 "pg/lock-waits",
		NoCollateralDamage: true,
	}
	r1 := &AgentResponse{} // no errors
	r2 := &AgentResponse{Errors: []string{"connection lost"}}
	s1, _ := j.Score(context.Background(), c, r1)
	s2, _ := j.Score(context.Background(), c, r2)
	if s1.Dimensions["collateral_safety"] != 1.0 {
		t.Errorf("no errors: safety = %f, want 1.0", s1.Dimensions["collateral_safety"])
	}
	if s2.Dimensions["collateral_safety"] != 0.0 {
		t.Errorf("with errors: safety = %f, want 0.0", s2.Dimensions["collateral_safety"])
	}
}

func TestHeuristicJudge_TimeEfficiency(t *testing.T) {
	cases := []struct {
		actualMs    int64
		expectedSec int
		want        float64
	}{
		{0, 60, 1.0},       // instant
		{60_000, 60, 1.0},  // equal
		{120_000, 60, 0.5}, // 2x → 0.5
		{180_000, 60, 0.0}, // 3x → 0.0
		{300_000, 60, 0.0}, // 5x → clamped to 0
	}
	for _, tc := range cases {
		got := timeEfficiency(tc.actualMs, tc.expectedSec)
		if got != tc.want {
			t.Errorf("timeEfficiency(%d, %d) = %f, want %f", tc.actualMs, tc.expectedSec, got, tc.want)
		}
	}
}

func TestHeuristicJudge_NilCase(t *testing.T) {
	j := NewHeuristicJudge()
	_, err := j.Score(context.Background(), nil, &AgentResponse{})
	if err == nil {
		t.Errorf("expected error on nil case")
	}
}

func TestHeuristicJudge_EmptyExpectedAllowsPartialCredit(t *testing.T) {
	j := NewHeuristicJudge()
	c := &Case{ID: "test"} // no expected root cause / remediation
	r := &AgentResponse{
		RootCause:    []string{"pg.lock_waits"},
		Remediations: []string{"pg.kill_session"},
	}
	s, _ := j.Score(context.Background(), c, r)
	// Empty expected + actual given → 0.5 partial credit
	if s.Dimensions["rca_accuracy"] != 0.5 {
		t.Errorf("rca_accuracy = %f, want 0.5 for empty expected", s.Dimensions["rca_accuracy"])
	}
}

// case 自报的 rca_accuracy 阈值必须真的拦住一次运行。
//
// 这个字段此前是一条纯声明：schema 校验它、两条桥接（runner / opskeeper-eval）
// 曾经不传它、两个 judge 都不读它。case.yaml 里写 rubric.rca_accuracy: 0.8 与
// 写0.5 对评分完全一样，而 e2e 只能断言一个硬编码的 0.5。
func TestHeuristicJudge_DeclaredThresholdFlagsBelowBar(t *testing.T) {
	j := NewHeuristicJudge()
	c := &Case{
		ID:                "pg/lock-waits",
		ExpectedRootCause: []string{"pg.lock_waits", "pg.active_sessions"},
		RCAThreshold:      0.8,
	}
	// 轨迹里两条观测都在，所以 Reason 轴不flag：本用例要验的只有阈值这条路。
	r := &AgentResponse{
		RootCause: []string{"pg.lock_waits"}, // rca = 0.5
		ToolCalls: []ToolCall{{Name: "pg.lock_waits"}, {Name: "pg.active_sessions"}},
	}
	s, err := j.Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if !s.Flagged {
		t.Errorf("Flagged = false, want true: rca_accuracy 0.5 is under the declared 0.8")
	}
	if !strings.Contains(s.FlagReason, "0.800") {
		t.Errorf("FlagReason = %q, want it to name the declared threshold", s.FlagReason)
	}
}

// 恰好压线不算违规；阈值是下界不是目标值。
func TestHeuristicJudge_DeclaredThresholdNotFlaggedAtBar(t *testing.T) {
	j := NewHeuristicJudge()
	c := &Case{
		ID:                "pg/lock-waits",
		ExpectedRootCause: []string{"pg.lock_waits"},
		RCAThreshold:      1.0,
	}
	// 带一条真实观测的 tool call：否则 Reason 轴会因为「结论正确但轨迹没有
	// 证据」而独立flag，那条路径与本用例要验的阈值无关。
	r := &AgentResponse{
		RootCause: []string{"pg.lock_waits"},
		ToolCalls: []ToolCall{{Name: "pg.lock_waits"}},
	}
	s, err := j.Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if s.Flagged {
		t.Errorf("Flagged = true at rca_accuracy == threshold: %s", s.FlagReason)
	}
}

// 0 表示「case 没有声明阈值」，不是「阈值是 0」。否则每个没写 rubric 的 case
// 的每一次运行都会被 flag，flag 就失去了筛选力。
func TestHeuristicJudge_ZeroThresholdMeansUndeclared(t *testing.T) {
	j := NewHeuristicJudge()
	c := &Case{
		ID:                "pg/lock-waits",
		ExpectedRootCause: []string{"pg.lock_waits", "pg.active_sessions"},
		RCAThreshold:      0,
	}
	r := &AgentResponse{
		RootCause: []string{"pg.lock_waits"},
		ToolCalls: []ToolCall{{Name: "pg.lock_waits"}, {Name: "pg.active_sessions"}},
	}
	s, err := j.Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if s.Flagged {
		t.Errorf("Flagged = true with no declared threshold: %s", s.FlagReason)
	}
}

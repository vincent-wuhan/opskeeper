package judge

import (
	"context"
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

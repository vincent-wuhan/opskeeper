package judge

import (
	"context"
	"strings"
	"testing"
)

// diagnosticCase is one pg/lock-waits-shaped case: the fault is a lock wait
// chain on table orders, and the observations the case requires are the two
// vocabulary symbols below.
func diagnosticCase() *Case {
	return &Case{
		ID:                   "pg/lock-waits",
		ExpectedRootCause:    []string{"pg.lock_waits", "pg.active_sessions"},
		ExpectedRemediations: []string{"pg.kill_session"},
		ExpectedDetectSec:    60,
		ExpectedRemediateSec: 120,
		ExpectedLocus:        []string{"pg", "orders"},
		ExpectedFaultType:    []string{"lock", "waits"},
	}
}

func TestTheThreeAxesAnswerThreeDifferentQuestions(t *testing.T) {
	// The conclusion localizes and identifies the fault, but nothing in the
	// trace shows the agent ever looked at the database. This is exactly the
	// run an outcome-only score cannot distinguish from a real diagnosis.
	c := diagnosticCase()
	r := &AgentResponse{
		// A conclusion that localizes properly names the table, not just the
		// engine: "pg is locked" is true of every pg incident in the corpus.
		RootCause:    []string{"pg.lock_waits on orders", "pg.active_sessions"},
		Remediations: []string{"pg.kill_session"},
	}
	axes := DiagnosticAxes(c, r)
	if axes[DimensionLocalization] != 1.0 {
		t.Errorf("localization = %v, want 1.0 (pg and orders are both named)", axes[DimensionLocalization])
	}
	if axes[DimensionIdentification] != 1.0 {
		t.Errorf("identification = %v, want 1.0 (lock waits is the conclusion)", axes[DimensionIdentification])
	}
	if axes[DimensionReason] != 0.0 {
		t.Errorf("reason = %v, want 0.0 (the trace is empty)", axes[DimensionReason])
	}
}

func TestTheReasonAxisReadsTheTraceNotTheConclusion(t *testing.T) {
	c := diagnosticCase()
	withoutTrace := &AgentResponse{
		RootCause:    []string{"pg.lock_waits", "pg.active_sessions"},
		Remediations: []string{"pg.kill_session"},
	}
	withTrace := &AgentResponse{
		RootCause:    []string{"pg.lock_waits", "pg.active_sessions"},
		Remediations: []string{"pg.kill_session"},
		ToolCalls: []ToolCall{{
			Name:   "query_pg_stat_activity",
			Args:   []byte(`{"table":"orders"}`),
			Result: []byte(`{"wait_event":"Lock","active_sessions":5}`),
		}},
	}
	// Same conclusion text twice. Only the trace differs, and only the
	// reason axis may move — if the other two move as well, the axes are not
	// separable and the three numbers are one number printed three times.
	before := DiagnosticAxes(c, withoutTrace)
	after := DiagnosticAxes(c, withTrace)
	if before[DimensionReason] >= after[DimensionReason] {
		t.Fatalf("reason did not move with the trace: before=%v after=%v",
			before[DimensionReason], after[DimensionReason])
	}
	for _, axis := range []string{DimensionLocalization, DimensionIdentification} {
		if before[axis] != after[axis] {
			t.Errorf("%s moved with the trace (%v → %v) but it asks about the conclusion",
				axis, before[axis], after[axis])
		}
	}
}

func TestALocalizationOnlyInTheTraceIsAMiss(t *testing.T) {
	c := diagnosticCase()
	r := &AgentResponse{
		// The agent queried the right table, then reported a conclusion that
		// names neither the family nor the table. The operator reading the
		// conclusion cannot act on a locus only a tool result knows about,
		// so this is a miss rather than a partial credit.
		ToolCalls: []ToolCall{{
			Name:   "query_pg_stat_activity",
			Args:   []byte(`{"table":"orders"}`),
			Result: []byte(`{"wait_event":"Lock"}`),
		}},
		RootCause: []string{"long_running_txns"},
	}
	if got := DiagnosticAxes(c, r)[DimensionLocalization]; got != 0.0 {
		t.Errorf("localization = %v, want 0.0 (the conclusion names no locus)", got)
	}
}

func TestAPartialAnswerScoresPartialAxes(t *testing.T) {
	c := diagnosticCase()
	r := &AgentResponse{
		RootCause:    []string{"pg lock waits on the primary"},
		Remediations: []string{"kill session"},
		ToolCalls: []ToolCall{{
			Name:   "query_pg_stat_activity",
			Result: []byte(`{"active_sessions":5}`),
		}},
	}
	axes := DiagnosticAxes(c, r)
	if axes[DimensionLocalization] != 0.5 {
		t.Errorf("localization = %v, want 0.5 (pg named, orders not)", axes[DimensionLocalization])
	}
	if axes[DimensionIdentification] != 1.0 {
		t.Errorf("identification = %v, want 1.0 (lock waits is prose in the conclusion)", axes[DimensionIdentification])
	}
	// active_sessions is grounded; lock_waits is only in the conclusion.
	if axes[DimensionReason] != 0.5 {
		t.Errorf("reason = %v, want 0.5 (one of two required observations)", axes[DimensionReason])
	}
}

func TestASymbolIsMatchedByItsTailWords(t *testing.T) {
	c := diagnosticCase()
	r := &AgentResponse{
		ToolCalls: []ToolCall{{
			Name:   "query_logql",
			Result: []byte(`{"msg":"long lock waits observed","active sessions 5"}`),
		}},
	}
	if got := DiagnosticAxes(c, r)[DimensionReason]; got != 1.0 {
		t.Errorf("reason = %v, want 1.0 (both symbols present as prose)", got)
	}
}

func TestHalfASymbolIsNotAnObservation(t *testing.T) {
	c := diagnosticCase()
	r := &AgentResponse{
		ToolCalls: []ToolCall{{
			Name:   "query_logql",
			Result: []byte(`{"msg":"sessions 5"}`),
		}},
	}
	// "active_sessions" is two words; a trace that carries only "sessions"
	// did not make the observation the case requires.
	if got := DiagnosticAxes(c, r)[DimensionReason]; got != 0.0 {
		t.Errorf("reason = %v, want 0.0 (half a symbol is not the observation)", got)
	}
}

func TestAnUndeclaredAxisIsAbsentRatherThanZero(t *testing.T) {
	c := &Case{
		ID:                "pg/lock-waits",
		ExpectedRootCause: []string{"pg.lock_waits"},
	}
	axes := DiagnosticAxes(c, &AgentResponse{})
	if _, ok := axes[DimensionLocalization]; ok {
		t.Error("localization present for a case that declared no locus")
	}
	if _, ok := axes[DimensionIdentification]; ok {
		t.Error("identification present for a case that declared no fault type")
	}
	if _, ok := axes[DimensionReason]; !ok {
		t.Error("reason absent although the case declared its observations")
	}
	if len(DiagnosticAxes(nil, nil)) != 0 {
		t.Error("axes computed for a nil case or response")
	}
}

func TestTheHeuristicJudgeCarriesTheAxes(t *testing.T) {
	score, err := NewHeuristicJudge().Score(context.Background(), diagnosticCase(), &AgentResponse{
		RootCause:    []string{"pg.lock_waits", "pg.active_sessions"},
		Remediations: []string{"pg.kill_session"},
		ToolCalls:    []ToolCall{{Name: "query_pg_stat_activity", Result: []byte(`lock waits, active sessions`)}},
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	for _, axis := range []string{DimensionLocalization, DimensionIdentification, DimensionReason} {
		if _, ok := score.Dimensions[axis]; !ok {
			t.Errorf("%s missing from a heuristic score: %v", axis, score.Dimensions)
		}
	}
	// The axes are added to the process dims, not swapped for them: a
	// leaderboard that lost rca_accuracy would compare rows across two
	// different formulas.
	for _, dim := range []string{"rca_accuracy", "time_efficiency", "remediation_quality", "collateral_safety"} {
		if _, ok := score.Dimensions[dim]; !ok {
			t.Errorf("process dimension %s disappeared: %v", dim, score.Dimensions)
		}
	}
	if err := score.IsValid(); err != nil {
		t.Errorf("score with axes is not valid: %v", err)
	}
}

func TestGoodOutcomeWithAnUngroundedTraceIsFlaggedForReview(t *testing.T) {
	c := diagnosticCase()
	r := &AgentResponse{
		RootCause:    []string{"pg.lock_waits", "pg.active_sessions"},
		Remediations: []string{"pg.kill_session"},
	}
	score, err := NewHeuristicJudge().Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.Overall < DiagnosticOutcomeFloor {
		t.Fatalf("fixture no longer has a good outcome (overall=%v)", score.Overall)
	}
	if !score.Flagged {
		t.Fatalf("a %.2f answer with an empty trace was not flagged", score.Overall)
	}
	if !strings.Contains(score.FlagReason, DimensionReason) {
		t.Errorf("flag reason does not name the axis: %q", score.FlagReason)
	}
}

func TestGoodOutcomeWithAGroundedTraceIsNotFlagged(t *testing.T) {
	score, err := NewHeuristicJudge().Score(context.Background(), diagnosticCase(), &AgentResponse{
		RootCause:    []string{"pg.lock_waits", "pg.active_sessions"},
		Remediations: []string{"pg.kill_session"},
		ToolCalls:    []ToolCall{{Name: "query_pg_stat_activity", Result: []byte(`lock waits; active sessions 5`)}},
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.Flagged {
		t.Errorf("grounded trace was flagged: %s", score.FlagReason)
	}
}

func TestALowOutcomeIsNotFlaggedForItsTrace(t *testing.T) {
	c := diagnosticCase()
	// Nothing right, nothing shown: the score is already low, and flagging
	// these would fill the reviewer queue with runs whose number already
	// says what is wrong.
	score, err := NewHeuristicJudge().Score(context.Background(), c, &AgentResponse{})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.Overall >= DiagnosticOutcomeFloor {
		t.Fatalf("fixture is not a low outcome (overall=%v)", score.Overall)
	}
	if score.Flagged {
		t.Errorf("low outcome flagged for its trace: %s", score.FlagReason)
	}
}

func TestAnUnmeasuredReasonAxisCannotFlagARun(t *testing.T) {
	// A case that declares no observations leaves the reason axis absent.
	// Absence is not failure, so the flag rule must not fire on it.
	c := &Case{ID: "host/cpu-spike", ExpectedLocus: []string{"host"}, ExpectedFaultType: []string{"cpu"}}
	score, err := NewHeuristicJudge().Score(context.Background(), c, &AgentResponse{
		RootCause: []string{"host"},
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.Flagged {
		t.Errorf("a run was flagged without a measured reason axis: %s", score.FlagReason)
	}
}

func TestMeanScoreKeepsTheAxes(t *testing.T) {
	c := diagnosticCase()
	r := &AgentResponse{
		RootCause:    []string{"pg.lock_waits", "pg.active_sessions"},
		Remediations: []string{"pg.kill_session"},
	}
	a, err := NewHeuristicJudge().Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score a: %v", err)
	}
	b := &Score{Overall: 0.8, Dimensions: map[string]float64{"rca_accuracy": 0.8}}
	merged := MeanScore(a, b)
	for _, axis := range []string{DimensionLocalization, DimensionIdentification, DimensionReason} {
		if _, ok := merged.Dimensions[axis]; !ok {
			t.Errorf("%s lost in the duo mean: %v", axis, merged.Dimensions)
		}
	}
}

func TestTheAxesAreNumbersInRange(t *testing.T) {
	c := diagnosticCase()
	score, err := NewHeuristicJudge().Score(context.Background(), c, &AgentResponse{
		RootCause: []string{"pg.lock_waits"},
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	for axis, value := range score.Dimensions {
		if value < 0 || value > 1 {
			t.Errorf("dimension %s = %v, outside [0,1]", axis, value)
		}
	}
}

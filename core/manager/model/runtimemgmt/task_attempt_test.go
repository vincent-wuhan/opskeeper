package runtimemgmt

import (
	"testing"
	"time"
)

// TestAttemptRecord_HasRetryBudget covers the retry-budget gate.
// MaxAttempts <= 0 means "no budget" (orchestrator safety default).
// Otherwise budget is exhausted when AttemptNumber >= MaxAttempts.
func TestAttemptRecord_HasRetryBudget(t *testing.T) {
	cases := []struct {
		name       string
		attempt    int
		max        int
		wantBudget bool
	}{
		{"first attempt, one allowed", 1, 1, false},
		{"first of three", 1, 3, true},
		{"second of three", 2, 3, true},
		{"third of three", 3, 3, false},
		{"no budget configured", 1, 0, false},
		{"negative budget", 1, -1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := AttemptRecord{AttemptNumber: c.attempt, MaxAttempts: c.max}
			if got := a.HasRetryBudget(); got != c.wantBudget {
				t.Errorf("HasRetryBudget = %v, want %v", got, c.wantBudget)
			}
		})
	}
}

// TestAttemptRecord_FailureClassOf pins the helper: only failed
// attempts surface a FailureClass; everything else returns empty.
func TestAttemptRecord_FailureClassOf(t *testing.T) {
	a := AttemptRecord{Outcome: AttemptOutcomeFailed, FailureClass: FailureClassLeaseExpired}
	if got := a.FailureClassOf(); got != FailureClassLeaseExpired {
		t.Errorf("failed: got %q, want %q", got, FailureClassLeaseExpired)
	}
	a.Outcome = AttemptOutcomeSucceeded
	if got := a.FailureClassOf(); got != "" {
		t.Errorf("succeeded: got %q, want empty", got)
	}
	a.Outcome = AttemptOutcomeSuperseded
	a.FailureClass = FailureClassLeaseExpired
	if got := a.FailureClassOf(); got != "" {
		t.Errorf("superseded: got %q, want empty (supersession masks class)", got)
	}
	a.Outcome = AttemptOutcomeCancelled
	if got := a.FailureClassOf(); got != "" {
		t.Errorf("cancelled: got %q, want empty", got)
	}
}

// TestAttemptRecord_Elapsed covers the duration helper. Returns zero
// for in-flight attempts and the actual span for ended ones.
func TestAttemptRecord_Elapsed(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	start := now
	end := now.Add(5 * time.Second)

	in := AttemptRecord{StartedAt: start}
	if got := in.Elapsed(); got != 0 {
		t.Errorf("in-flight Elapsed = %v, want 0", got)
	}

	ended := AttemptRecord{StartedAt: start, EndedAt: &end}
	if got := ended.Elapsed(); got != 5*time.Second {
		t.Errorf("ended Elapsed = %v, want 5s", got)
	}
}

func TestIsTerminalOutcome(t *testing.T) {
	terminal := []string{AttemptOutcomeSucceeded, AttemptOutcomeFailed, AttemptOutcomeCancelled, AttemptOutcomeSuperseded}
	nonterminal := []string{"", "running", "queued"}
	for _, s := range terminal {
		if !IsTerminalOutcome(s) {
			t.Errorf("IsTerminalOutcome(%s) = false, want true", s)
		}
	}
	for _, s := range nonterminal {
		if IsTerminalOutcome(s) {
			t.Errorf("IsTerminalOutcome(%s) = true, want false", s)
		}
	}
}

package runtimemgmt

import (
	"errors"
	"testing"
)

// TestValidTransitions_Table pins the allowed-state-machine edges.
// Mirrors task_state.go ValidTransitions; if this fails it means the
// state machine lost a path. Adding a new transition MUST update both
// the table and ValidTransitions.
func TestValidTransitions_Table(t *testing.T) {
	must := []struct {
		from, to TaskState
	}{
		{TaskStateQueued, TaskStateAdmitted},
		{TaskStateQueued, TaskStateFailed},
		{TaskStateQueued, TaskStateCancelled},
		{TaskStateAdmitted, TaskStateClaimed},
		{TaskStateAdmitted, TaskStateFailed},
		{TaskStateAdmitted, TaskStateCancelled},
		{TaskStateClaimed, TaskStatePreparing},
		{TaskStateClaimed, TaskStateFailed},
		{TaskStateClaimed, TaskStateCancelled},
		{TaskStatePreparing, TaskStateRunning},
		{TaskStatePreparing, TaskStateFailed},
		{TaskStatePreparing, TaskStateCancelled},
		{TaskStateRunning, TaskStateSucceeded},
		{TaskStateRunning, TaskStateFailed},
		{TaskStateRunning, TaskStateCancelled},
	}
	for _, c := range must {
		if !IsValidTransition(c.from, c.to) {
			t.Errorf("missing valid transition: %s -> %s", c.from, c.to)
		}
	}
}

// TestValidTransitions_Forbidden pins the explicitly-banned paths.
// Spec design.md §Durable task model says no back-edges from terminal
// states, and no skipping intermediate phases.
func TestValidTransitions_Forbidden(t *testing.T) {
	banned := []struct {
		from, to TaskState
	}{
		// Terminal states have no outgoing edges.
		{TaskStateSucceeded, TaskStateRunning},
		{TaskStateSucceeded, TaskStateFailed},
		{TaskStateSucceeded, TaskStateCancelled},
		{TaskStateFailed, TaskStateRunning},
		{TaskStateFailed, TaskStateAdmitted},
		{TaskStateFailed, TaskStateClaimed},
		{TaskStateCancelled, TaskStateRunning},
		{TaskStateCancelled, TaskStateAdmitted},
		// Skipping intermediate phases.
		{TaskStateQueued, TaskStateClaimed},
		{TaskStateQueued, TaskStateRunning},
		{TaskStateQueued, TaskStateSucceeded},
		{TaskStateAdmitted, TaskStatePreparing},
		{TaskStateAdmitted, TaskStateRunning},
		{TaskStateClaimed, TaskStateRunning},
		{TaskStatePreparing, TaskStateSucceeded},
		// Reverse direction (no back-edges).
		{TaskStateAdmitted, TaskStateQueued},
		{TaskStateClaimed, TaskStateAdmitted},
		{TaskStatePreparing, TaskStateClaimed},
		{TaskStateRunning, TaskStatePreparing},
		// Unknown state.
		{TaskState("unknown"), TaskStateRunning},
		{TaskState(""), TaskStateRunning},
	}
	for _, c := range banned {
		if IsValidTransition(c.from, c.to) {
			t.Errorf("forbidden transition allowed: %s -> %s", c.from, c.to)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []TaskState{TaskStateSucceeded, TaskStateFailed, TaskStateCancelled}
	nonTerminal := []TaskState{TaskStateQueued, TaskStateAdmitted, TaskStateClaimed, TaskStatePreparing, TaskStateRunning, TaskState("")}
	for _, s := range terminal {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = false, want true", s)
		}
	}
	for _, s := range nonTerminal {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = true, want false", s)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in        string
		want      TaskState
		wantError bool
	}{
		{"queued", TaskStateQueued, false},
		{"admitted", TaskStateAdmitted, false},
		{"claimed", TaskStateClaimed, false},
		{"preparing", TaskStatePreparing, false},
		{"running", TaskStateRunning, false},
		{"succeeded", TaskStateSucceeded, false},
		{"failed", TaskStateFailed, false},
		{"cancelled", TaskStateCancelled, false},
		{"", "", true},
		{"UNKNOWN", "", true},
		{"Queued", "", true}, // case-sensitive: typed strings.
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if c.wantError {
			if !errors.Is(err, ErrUnknownState) {
				t.Errorf("Normalize(%q) err=%v, want ErrUnknownState", c.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Normalize(%q) unexpected err=%v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Normalize(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestValidTransitions_Coverage guards against accidentally leaving a
// non-terminal state with no outbound edges; such a state would trap
// tasks with no escape.
func TestValidTransitions_Coverage(t *testing.T) {
	for _, s := range AllTaskStates {
		edges := ValidTransitions[s]
		if IsTerminal(s) {
			if len(edges) != 0 {
				t.Errorf("terminal state %s has edges: %v", s, edges)
			}
			continue
		}
		if len(edges) == 0 {
			t.Errorf("non-terminal state %s has empty transition set", s)
		}
	}
}

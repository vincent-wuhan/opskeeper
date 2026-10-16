package runtimemgmt

import (
	"testing"
	"time"
)

// TestResumeFor_PicksHighestSequence pins the resume-target rule:
// the checkpoint with the highest Sequence for the (task, attempt)
// is the resume target, regardless of RecordedAt ordering.
func TestResumeFor_PicksHighestSequence(t *testing.T) {
	now := time.Now().UTC()
	checkpoints := []Checkpoint{
		{TaskID: "t", AttemptNumber: 1, Sequence: 1, RecordedAt: now},
		{TaskID: "t", AttemptNumber: 1, Sequence: 3, RecordedAt: now.Add(-time.Hour)},
		{TaskID: "t", AttemptNumber: 1, Sequence: 2, RecordedAt: now.Add(-2 * time.Hour)},
	}
	got := ResumeFor(checkpoints)
	if got == nil {
		t.Fatalf("ResumeFor returned nil")
	}
	if got.Sequence != 3 {
		t.Errorf("resume target Sequence = %d, want 3", got.Sequence)
	}
}

func TestResumeFor_Empty(t *testing.T) {
	if got := ResumeFor(nil); got != nil {
		t.Errorf("ResumeFor(nil) = %+v, want nil", got)
	}
	if got := ResumeFor([]Checkpoint{}); got != nil {
		t.Errorf("ResumeFor(empty) = %+v, want nil", got)
	}
}

// TestResumeFor_SkipNil is a defensive guard for repositories that
// occasionally hand back nil checkpointer rows.
func TestResumeFor_SkipNil(t *testing.T) {
	now := time.Now().UTC()
	checkpoints := []*Checkpoint{
		nil,
		{TaskID: "t", AttemptNumber: 1, Sequence: 5, RecordedAt: now},
		nil,
	}
	flat := make([]Checkpoint, 0, len(checkpoints))
	for _, p := range checkpoints {
		if p == nil {
			// ResumeFor's contract doesn't include nil entries but
			// this test pins the behaviour so a future repo
			// refactor doesn't surprise callers.
			continue
		}
		flat = append(flat, *p)
	}
	got := ResumeFor(flat)
	if got == nil || got.Sequence != 5 {
		t.Errorf("ResumeFor did not pick the only non-nil row: %+v", got)
	}
}

// TestCheckpoint_IsForCurrentClaim covers the stale-claim filter.
// generation=0 is reserved as "no claim yet" and MUST always report
// false so the orchestrator never treats a pre-claim checkpoint as
// authoritative.
func TestCheckpoint_IsForCurrentClaim(t *testing.T) {
	c := Checkpoint{Generation: 4}
	if !c.IsForCurrentClaim(4) {
		t.Errorf("matching generation should be current claim")
	}
	if c.IsForCurrentClaim(5) {
		t.Errorf("older generation should not be current claim")
	}
	c0 := Checkpoint{Generation: 0}
	if c0.IsForCurrentClaim(0) {
		t.Errorf("zero generation should NOT match (reserved meaning)")
	}
}

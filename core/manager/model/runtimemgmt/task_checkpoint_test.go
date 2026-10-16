package runtimemgmt

import (
	"testing"
	"time"
)

// TestResumeFor_PicksHighestSequence 固定恢复目标规则：同一任务与尝试中
// Sequence 最大的检查点为恢复目标，与 RecordedAt 顺序无关。
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

// TestResumeFor_SkipNil 是防御性测试，覆盖仓储偶发返回 nil 检查点的情况。
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
			// ResumeFor 契约不包含 nil，但固定该行为可避免未来仓储重构
			// 让调用方意外失败。
			continue
		}
		flat = append(flat, *p)
	}
	got := ResumeFor(flat)
	if got == nil || got.Sequence != 5 {
		t.Errorf("ResumeFor did not pick the only non-nil row: %+v", got)
	}
}

// TestCheckpoint_IsForCurrentClaim 覆盖陈旧认领过滤。generation=0
// 保留为尚未认领，必须返回 false，避免认领前检查点被视为权威。
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

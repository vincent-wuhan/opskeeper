package runtimemgmt

import (
	"errors"
	"testing"
)

// TestValidTransitions_Table 固定允许的状态机边，镜像 task_state.go 的
// ValidTransitions；新增迁移必须同时更新两处。
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

// TestValidTransitions_Forbidden 固定显式禁止路径：终态不能回退，
// 也不能跳过中间阶段。
func TestValidTransitions_Forbidden(t *testing.T) {
	banned := []struct {
		from, to TaskState
	}{
		// 终态没有出边。
		{TaskStateSucceeded, TaskStateRunning},
		{TaskStateSucceeded, TaskStateFailed},
		{TaskStateSucceeded, TaskStateCancelled},
		{TaskStateFailed, TaskStateRunning},
		{TaskStateFailed, TaskStateAdmitted},
		{TaskStateFailed, TaskStateClaimed},
		{TaskStateCancelled, TaskStateRunning},
		{TaskStateCancelled, TaskStateAdmitted},
		// 跳过中间阶段必须禁止。
		{TaskStateQueued, TaskStateClaimed},
		{TaskStateQueued, TaskStateRunning},
		{TaskStateQueued, TaskStateSucceeded},
		{TaskStateAdmitted, TaskStatePreparing},
		{TaskStateAdmitted, TaskStateRunning},
		{TaskStateClaimed, TaskStateRunning},
		{TaskStatePreparing, TaskStateSucceeded},
		// 反向迁移必须禁止，不允许回退边。
		{TaskStateAdmitted, TaskStateQueued},
		{TaskStateClaimed, TaskStateAdmitted},
		{TaskStatePreparing, TaskStateClaimed},
		{TaskStateRunning, TaskStatePreparing},
		// 未知状态必须拒绝。
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

// TestValidTransitions_Coverage 防止非终态意外没有出边；否则任务会被困住。
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

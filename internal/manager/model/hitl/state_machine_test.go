package hitl

import "testing"

// TestValidTransitions_Table 测试合法迁移表覆盖完整状态机路径。
//
// 设计：每个 state 都至少有 1 条合法迁移；终态不允许任何迁移。
func TestValidTransitions_Table(t *testing.T) {
	must := []struct {
		from, to string
	}{
		{StatePending, StateApproved},
		{StatePending, StateRejected},
		{StatePending, StateExpired},
		{StatePending, StatePaused},
		{StateApproved, StateExecuted},
		{StateApproved, StateFailed},
		{StateApproved, StatePaused},
		{StateApproved, StateRolledBack},
		{StatePaused, StateResumed},
		{StatePaused, StateRejected},
		{StatePaused, StateExpired},
		{StateResumed, StateExecuted},
		{StateResumed, StateFailed},
		{StateResumed, StatePaused},
		{StateResumed, StateRolledBack},
		{StateExecuted, StateRolledBack},
	}
	for _, c := range must {
		if !IsValidTransition(c.from, c.to) {
			t.Errorf("missing valid transition: %s -> %s", c.from, c.to)
		}
	}
}

// TestValidTransitions_Forbidden 测试不允许的迁移全部报错。
func TestValidTransitions_Forbidden(t *testing.T) {
	banned := []struct {
		from, to string
	}{
		// 终态不能再迁移（除了 executed -> rolled_back 允许一次）
		{StateRejected, StatePending},
		{StateRejected, StateApproved},
		{StateExpired, StatePending},
		{StateExpired, StateApproved},
		{StateFailed, StatePending},
		{StateFailed, StateApproved},
		{StateRolledBack, StateApproved},
		// pending 不能直接 executed（必须先 approved）
		{StatePending, StateExecuted},
		{StatePending, StateFailed},
		{StatePending, StateResumed},
		// rejected/expired 不能再 rolled_back
		{StateRejected, StateRolledBack},
		{StateExpired, StateRolledBack},
		// 凭空跳
		{StateApproved, StateResumed},
	}
	for _, c := range banned {
		if IsValidTransition(c.from, c.to) {
			t.Errorf("forbidden transition allowed: %s -> %s", c.from, c.to)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []string{StateExecuted, StateRejected, StateExpired, StateFailed, StateRolledBack}
	nonterminal := []string{StatePending, StateApproved, StatePaused, StateResumed, "unknown"}
	for _, s := range terminal {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = false, want true", s)
		}
	}
	for _, s := range nonterminal {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = true, want false", s)
		}
	}
}

// TestValidTransitions_Coverage 至少所有非终态都有出度。
//
// 终态除外。
func TestValidTransitions_Coverage(t *testing.T) {
	nonTerminal := []string{StatePending, StateApproved, StatePaused, StateResumed}
	for _, s := range nonTerminal {
		if _, ok := ValidTransitions[s]; !ok {
			t.Errorf("non-terminal state %s missing from ValidTransitions map", s)
		}
		if len(ValidTransitions[s]) == 0 {
			t.Errorf("non-terminal state %s has empty transition set", s)
		}
	}
}

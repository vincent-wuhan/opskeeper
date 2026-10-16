package runtimemgmt

import (
	"testing"
	"time"
)

// TestClaim_IsStale 覆盖租约陈旧判断：LeasedUntil 在过去必须视为陈旧，
// 在未来不得视为陈旧；零值也必须视为陈旧，避免未分配期限被误认为新鲜。
func TestClaim_IsStale(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		claim Claim
		now   time.Time
		stale bool
	}{
		{
			name:  "deadline in the past",
			claim: Claim{LeasedUntil: now.Add(-time.Minute)},
			now:   now,
			stale: true,
		},
		{
			name:  "deadline in the future",
			claim: Claim{LeasedUntil: now.Add(time.Minute)},
			now:   now,
			stale: false,
		},
		{
			name:  "deadline equal to now (not strictly before)",
			claim: Claim{LeasedUntil: now},
			now:   now,
			stale: true,
		},
		{
			name:  "zero deadline always stale",
			claim: Claim{LeasedUntil: time.Time{}},
			now:   now,
			stale: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.claim.IsStale(c.now); got != c.stale {
				t.Errorf("IsStale() = %v, want %v", got, c.stale)
			}
		})
	}
}

// TestClaim_BelongsToRuntime 覆盖所有权守卫：恢复候选回传错误运行时
// ID 时必须拒绝。
func TestClaim_BelongsToRuntime(t *testing.T) {
	c := Claim{ClaimedBy: "runtime:edge:abc"}
	if !c.BelongsToRuntime("runtime:edge:abc") {
		t.Errorf("matching runtime should belong")
	}
	if c.BelongsToRuntime("runtime:edge:def") {
		t.Errorf("non-matching runtime must not belong")
	}
	if c.BelongsToRuntime("") {
		t.Errorf("empty candidate must not belong (avoid silent accept)")
	}
	if (Claim{ClaimedBy: ""}).BelongsToRuntime("runtime:edge:abc") {
		t.Errorf("empty ClaimedBy must not belong (avoid silent accept)")
	}
}

// TestClaim_AdvanceTo 覆盖恢复辅助函数：新认领必须代际加一，记录新所有者、
// 新 ClaimedAt 与新 LeasedUntil，并清空 OwnerLeaseID。
func TestClaim_AdvanceTo(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	newLease := now.Add(5 * time.Minute)
	c := Claim{
		TaskID: "t", TenantID: "tn",
		Generation: 3, ClaimedBy: "old",
		ClaimedAt:    now.Add(-time.Hour),
		LeasedUntil:  now.Add(-time.Hour + 5*time.Minute),
		OwnerLeaseID: "lease-2",
	}
	adv := c.AdvanceTo("runtime:edge:xyz", now, newLease)
	if adv.Generation != c.Generation+1 {
		t.Errorf("Generation = %d, want %d", adv.Generation, c.Generation+1)
	}
	if adv.ClaimedBy != "runtime:edge:xyz" {
		t.Errorf("ClaimedBy = %q", adv.ClaimedBy)
	}
	if !adv.ClaimedAt.Equal(now.UTC()) {
		t.Errorf("ClaimedAt = %v, want %v", adv.ClaimedAt, now.UTC())
	}
	if !adv.LeasedUntil.Equal(newLease.UTC()) {
		t.Errorf("LeasedUntil = %v, want %v", adv.LeasedUntil, newLease.UTC())
	}
	if adv.OwnerLeaseID != "" {
		t.Errorf("OwnerLeaseID = %q, want blank", adv.OwnerLeaseID)
	}
	if adv.TaskID != c.TaskID || adv.TenantID != c.TenantID {
		t.Errorf("ids not preserved: %+v", adv)
	}
}

// TestClaim_AdvanceTo_OriginalUntouched 防止源认领被原地修改；
// 辅助函数必须返回新值。
func TestClaim_AdvanceTo_OriginalUntouched(t *testing.T) {
	now := time.Now().UTC()
	c := Claim{TaskID: "t", Generation: 0, ClaimedBy: "old"}
	_ = c.AdvanceTo("new", now, now.Add(time.Minute))
	if c.Generation != 0 || c.ClaimedBy != "old" {
		t.Errorf("AdvanceTo mutated source: %+v", c)
	}
}

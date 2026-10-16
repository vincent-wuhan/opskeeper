package runtimemgmt

import (
	"testing"
	"time"
)

// TestClaim_IsStale covers the lease-staleness decision. A claim
// whose LeasedUntil is in the past MUST report stale; one in the
// future MUST NOT. A zero LeasedUntil MUST report stale (so a
// claim row that has not been assigned a deadline cannot be
// mistaken for fresh).
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

// TestClaim_BelongsToRuntime covers the ownership guard. A recovery
// candidate echoing back the wrong runtime id MUST be rejected.
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

// TestClaim_AdvanceTo covers the recovery-time helper. The new claim
// MUST advance generation by one, record the new owner, set new
// ClaimedAt, set new LeasedUntil, and clear OwnerLeaseID.
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

// TestClaim_AdvanceTo_OriginalUntouched guards against accidental
// in-place mutation of the source claim; the helper MUST return a
// new value.
func TestClaim_AdvanceTo_OriginalUntouched(t *testing.T) {
	now := time.Now().UTC()
	c := Claim{TaskID: "t", Generation: 0, ClaimedBy: "old"}
	_ = c.AdvanceTo("new", now, now.Add(time.Minute))
	if c.Generation != 0 || c.ClaimedBy != "old" {
		t.Errorf("AdvanceTo mutated source: %+v", c)
	}
}

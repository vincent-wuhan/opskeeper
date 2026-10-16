package runtimemgmt

import (
	"errors"
	"testing"
	"time"
)

// TestNewPrepareDeadline covers the prepare-window construction. A
// zero or negative TTL MUST produce a zero deadline so callers can
// opt out of a prepare window for read-only tasks.
func TestNewPrepareDeadline(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	if got := NewPrepareDeadline(now, 0); !got.IsZero() {
		t.Errorf("ttl=0 -> %v, want zero", got)
	}
	if got := NewPrepareDeadline(now, -time.Second); !got.IsZero() {
		t.Errorf("ttl<0 -> %v, want zero", got)
	}
	if got := NewPrepareDeadline(time.Time{}, time.Minute); !got.IsZero() {
		t.Errorf("zero claimTime -> %v, want zero", got)
	}
	got := NewPrepareDeadline(now, 30*time.Second)
	want := now.Add(30 * time.Second)
	if !got.Equal(want) {
		t.Errorf("ttl=30s -> %v, want %v", got, want)
	}
}

func TestNewLivenessDeadline(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	if got := NewLivenessDeadline(now, 0); !got.IsZero() {
		t.Errorf("ttl=0 -> %v, want zero", got)
	}
	if got := NewLivenessDeadline(now, -time.Second); !got.IsZero() {
		t.Errorf("ttl<0 -> %v, want zero", got)
	}
	got := NewLivenessDeadline(now, time.Minute)
	if !got.Equal(now.Add(time.Minute)) {
		t.Errorf("ttl=1m -> %v, want %v", got, now.Add(time.Minute))
	}
}

// TestLease_HasPrepare_Liveness covers the "deadline not assigned
// yet" semantics. A lease row exists from claim time, but the
// deadlines only become meaningful once the runtime has started the
// phase. Treat zero deadline as "not actionable" so callers do not
// accidentally expire a not-yet-started lease.
func TestLease_HasPrepare_Liveness(t *testing.T) {
	l := Lease{}
	if l.HasPrepare() {
		t.Errorf("zero PrepareDeadline should report not set")
	}
	if l.HasLiveness() {
		t.Errorf("zero LivenessDeadline should report not set")
	}
	now := time.Now().UTC()
	l.PrepareDeadline = now.Add(time.Minute)
	l.LivenessDeadline = now.Add(time.Minute)
	if !l.HasPrepare() || !l.HasLiveness() {
		t.Errorf("non-zero deadlines should be reported as set")
	}
}

// TestLease_Expired covers expired detection. Note: a zero deadline
// MUST NOT report expired (caller must check HasPrepare/HasLiveness
// first to disambiguate).
func TestLease_Expired(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	l := Lease{}
	if l.PrepareExpired(now) {
		t.Errorf("zero PrepareDeadline should NOT report expired")
	}
	if l.LivenessExpired(now) {
		t.Errorf("zero LivenessDeadline should NOT report expired")
	}
	l.PrepareDeadline = now.Add(-time.Minute)
	if !l.PrepareExpired(now) {
		t.Errorf("past PrepareDeadline should report expired")
	}
	l2 := Lease{LivenessDeadline: now.Add(time.Minute)}
	if l2.LivenessExpired(now) {
		t.Errorf("future LivenessDeadline should NOT report expired")
	}
}

// TestLease_ExtendLiveness covers the rolling-lease happy path:
// deadline moves forward by ttl, Extensions++ and ExtendedAt set.
func TestLease_ExtendLiveness(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	l := Lease{LivenessDeadline: now.Add(time.Minute)}
	adv, err := l.ExtendLiveness(now, 2*time.Minute)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !adv.LivenessDeadline.Equal(now.Add(2 * time.Minute)) {
		t.Errorf("LivenessDeadline = %v, want %v", adv.LivenessDeadline, now.Add(2*time.Minute))
	}
	if adv.Extensions != 1 {
		t.Errorf("Extensions = %d, want 1", adv.Extensions)
	}
	if adv.ExtendedAt.IsZero() {
		t.Errorf("ExtendedAt = zero, want %v", now)
	}
}

// TestLease_ExtendLiveness_RejectExpired covers the safety branch:
// attempting to extend an expired lease MUST return ErrLeaseExpired
// rather than silently revive the row.
func TestLease_ExtendLiveness_RejectExpired(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	l := Lease{LivenessDeadline: now.Add(-time.Minute)}
	if _, err := l.ExtendLiveness(now, time.Minute); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("expired extend err = %v, want ErrLeaseExpired", err)
	}
}

// TestLease_ExtendLiveness_RejectInvalid covers ttl<=0 and zero-deadline
// rejection paths.
func TestLease_ExtendLiveness_RejectInvalid(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	l := Lease{LivenessDeadline: now.Add(time.Minute)}
	if _, err := l.ExtendLiveness(now, 0); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("ttl=0 err = %v, want ErrLeaseExpired", err)
	}
	if _, err := l.ExtendLiveness(now, -time.Second); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("ttl<0 err = %v, want ErrLeaseExpired", err)
	}
	zero := Lease{}
	if _, err := zero.ExtendLiveness(now, time.Minute); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("zero deadline err = %v, want ErrLeaseExpired", err)
	}
}

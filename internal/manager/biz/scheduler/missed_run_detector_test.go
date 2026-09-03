package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeRepo struct {
	mu       sync.Mutex
	missed   []MissedRunInfo
	audit    []MissedRunInfo
	listErr  error
	auditErr error
}

func (f *fakeRepo) ListMissed(ctx context.Context, before time.Time) ([]MissedRunInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.missed, f.listErr
}

func (f *fakeRepo) RecordMissedAudit(ctx context.Context, m MissedRunInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audit = append(f.audit, m)
	return nil
}

type fakeAlert struct {
	mu      sync.Mutex
	calls   int
	lastSrc string
	lastMs  []MissedRunInfo
	err     error
}

func (f *fakeAlert) WriteMissedRunAlert(ctx context.Context, source string, missed []MissedRunInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastSrc = source
	f.lastMs = missed
	return f.err
}

func TestDetector_NoMissed(t *testing.T) {
	repo := &fakeRepo{missed: nil}
	alert := &fakeAlert{}
	d := NewDetector(repo, alert, nil)
	d.SetClock(func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) })

	if err := d.RunOnBoot(context.Background()); err != nil {
		t.Fatalf("RunOnBoot error = %v", err)
	}
	if alert.calls != 0 {
		t.Errorf("alert calls = %d, want 0", alert.calls)
	}
}

func TestDetector_WithMissed(t *testing.T) {
	expectedFire := time.Date(2026, 7, 14, 11, 0, 0, 0, time.UTC)
	missed := []MissedRunInfo{
		{FlowID: "f1", NodeID: "n1", CronSpec: "0 * * * *", ExpectedFireAt: expectedFire, MissedDurationSec: 3600},
		{FlowID: "f2", NodeID: "n2", CronSpec: "*/5 * * * *", ExpectedFireAt: expectedFire, MissedDurationSec: 3600},
	}
	repo := &fakeRepo{missed: missed}
	alert := &fakeAlert{}
	d := NewDetector(repo, alert, nil)
	d.SetClock(func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) })

	if err := d.RunOnBoot(context.Background()); err != nil {
		t.Fatalf("RunOnBoot error = %v", err)
	}
	if alert.calls != 1 {
		t.Errorf("alert calls = %d, want 1", alert.calls)
	}
	if alert.lastSrc != "scheduler-missed-run" {
		t.Errorf("alert source = %q, want %q", alert.lastSrc, "scheduler-missed-run")
	}
	if len(alert.lastMs) != 2 {
		t.Errorf("alert missed = %d, want 2", len(alert.lastMs))
	}
	if len(repo.audit) != 2 {
		t.Errorf("audit records = %d, want 2", len(repo.audit))
	}
}

func TestDetector_ListError(t *testing.T) {
	repo := &fakeRepo{listErr: errors.New("db error")}
	alert := &fakeAlert{}
	d := NewDetector(repo, alert, nil)
	if err := d.RunOnBoot(context.Background()); err == nil {
		t.Error("expected error from ListMissed, got nil")
	}
}

func TestDetector_AuditErrorContinues(t *testing.T) {
	expectedFire := time.Date(2026, 7, 14, 11, 0, 0, 0, time.UTC)
	missed := []MissedRunInfo{{FlowID: "f1", NodeID: "n1", ExpectedFireAt: expectedFire}}
	repo := &fakeRepo{missed: missed, auditErr: errors.New("audit fail")}
	alert := &fakeAlert{}
	d := NewDetector(repo, alert, nil)
	if err := d.RunOnBoot(context.Background()); err != nil {
		t.Fatalf("RunOnBoot should not fail on audit error, got: %v", err)
	}
	if alert.calls != 1 {
		t.Errorf("alert calls = %d, want 1", alert.calls)
	}
}

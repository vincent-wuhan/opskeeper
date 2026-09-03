package store

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	flowbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/flow"
	flowmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/flow"
)

func setupScheduleRepo(t *testing.T) *Repo {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewRepo(db)
}

func TestScheduleState_WriteThroughAndReload(t *testing.T) {
	repo := setupScheduleRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	state := &flowmodel.FlowScheduleNextFire{
		FlowID: 7, NodeID: "cron-1", CronSpec: "*/5 * * * *",
		NextFireAt: now.Add(5 * time.Minute), LastHeartbeatAt: now,
		Status: flowmodel.FlowScheduleStatusEnabled,
	}
	if err := repo.UpsertScheduleState(ctx, state); err != nil {
		t.Fatalf("upsert state: %v", err)
	}
	state.NextFireAt = now.Add(10 * time.Minute)
	if err := repo.UpsertScheduleState(ctx, state); err != nil {
		t.Fatalf("update state: %v", err)
	}
	states, err := repo.LoadScheduleStates(ctx)
	if err != nil {
		t.Fatalf("load states: %v", err)
	}
	if len(states) != 1 || !states[0].NextFireAt.Equal(state.NextFireAt) {
		t.Fatalf("states = %#v, want one cursor at %s", states, state.NextFireAt)
	}
}

func TestScheduleState_ListMissedAndAuditIdempotent(t *testing.T) {
	repo := setupScheduleRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	state := &flowmodel.FlowScheduleNextFire{
		FlowID: 9, NodeID: "cron-2", CronSpec: "0 * * * *",
		NextFireAt: now.Add(-time.Hour), LastHeartbeatAt: now.Add(-2 * time.Hour),
		Status: flowmodel.FlowScheduleStatusEnabled,
	}
	if err := repo.UpsertScheduleState(ctx, state); err != nil {
		t.Fatalf("upsert state: %v", err)
	}
	missed, err := repo.ListMissed(ctx, now)
	if err != nil {
		t.Fatalf("list missed: %v", err)
	}
	if len(missed) != 1 || missed[0].FlowID != "9" || missed[0].NodeID != "cron-2" {
		t.Fatalf("missed = %#v", missed)
	}
	if err := repo.RecordMissedAudit(ctx, missed[0]); err != nil {
		t.Fatalf("record audit: %v", err)
	}
	if err := repo.RecordMissedAudit(ctx, missed[0]); err != nil {
		t.Fatalf("record duplicate audit: %v", err)
	}
	if err := repo.DeleteScheduleStatesNotIn(ctx, []flowbiz.ScheduleStateKey{}); err != nil {
		t.Fatalf("delete stale state: %v", err)
	}
}

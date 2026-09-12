package loop

import (
	"context"
	"testing"
	"time"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

func TestInMemoryEventRepoReadEventsOrdersSameTimestampDeterministically(t *testing.T) {
	t.Parallel()

	repo := NewInMemoryEventRepo()
	ctx := context.Background()
	createdAt := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	events := []*loopmodel.Event{
		{
			TenantID:       "tenant-a",
			IncidentID:     "incident-a",
			Phase:          string(PhaseDetected),
			EventType:      loopmodel.EventTypePhaseEntered,
			IdempotencyKey: "incident-a:detected:phase-entered",
			CreatedAt:      createdAt,
		},
		{
			TenantID:       "tenant-a",
			IncidentID:     "incident-a",
			Phase:          string(PhaseDetected),
			EventType:      loopmodel.EventPhaseContractWritten,
			IdempotencyKey: "incident-a:detected:phase-contract-written",
			CreatedAt:      createdAt,
		},
		{
			TenantID:       "tenant-a",
			IncidentID:     "incident-a",
			Phase:          string(PhaseCorrelated),
			EventType:      loopmodel.EventTypePhaseEntered,
			IdempotencyKey: "incident-a:correlated:phase-entered",
			CreatedAt:      createdAt,
		},
	}

	for _, event := range events {
		if err := repo.AppendEvent(ctx, event); err != nil {
			t.Fatalf("AppendEvent(%s) err: %v", event.IdempotencyKey, err)
		}
	}

	got, err := repo.ReadEvents(ctx, "tenant-a", "incident-a")
	if err != nil {
		t.Fatalf("ReadEvents() err: %v", err)
	}
	if len(got) != len(events) {
		t.Fatalf("ReadEvents() len = %d, want %d", len(got), len(events))
	}
	for index, event := range events {
		if got[index].IdempotencyKey != event.IdempotencyKey {
			t.Fatalf("ReadEvents()[%d].IdempotencyKey = %s, want %s", index, got[index].IdempotencyKey, event.IdempotencyKey)
		}
	}
}

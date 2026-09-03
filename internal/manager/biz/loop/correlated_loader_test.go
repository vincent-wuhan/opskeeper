package loop

import (
	"context"
	"testing"
	"time"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

func TestContractDetectionEventLoader_Load(t *testing.T) {
	contracts := NewInMemoryContractRepo()
	event := DetectionEvent{
		AlertID: "alert-1", Severity: "critical", Resource: "pg",
		LabelSetKey: "pg.primary", DetectedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
	}
	payload := `{"alert_id":"alert-1","severity":"critical","resource":"pg","labelsetkey":"pg.primary","detected_at":"2026-08-19T12:00:00Z"}`
	if err := contracts.WriteContract(context.Background(), &loopmodel.Contract{
		IncidentID: "inc-1", TenantID: "tenant-a", Phase: string(PhaseDetected),
		Type: "DetectedEvent", SchemaVer: "v1", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := NewContractDetectionEventLoader(contracts).Load(context.Background(), PlanInput{
		TenantID: "tenant-a", IncidentID: "inc-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != event {
		t.Fatalf("event = %+v, want %+v", got, event)
	}
}

func TestContractDetectionEventLoader_MissingContract(t *testing.T) {
	_, err := NewContractDetectionEventLoader(NewInMemoryContractRepo()).Load(context.Background(), PlanInput{
		TenantID: "tenant-a", IncidentID: "missing",
	})
	if err == nil {
		t.Fatalf("err = %v, want missing-contract error", err)
	}
}

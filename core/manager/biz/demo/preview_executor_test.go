package demo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "github.com/glebarez/go-sqlite"
	repairpreview "github.com/vincent-wuhan/opskeeper/core/domains/control/repairpreview"
)

const testPreviewTargetFingerprint = "0123456789abcdef"

func TestRepairPreviewExecutorFailsClosedWithoutConfiguration(t *testing.T) {
	if _, err := NewRepairPreviewExecutor("", "", "sha256:workload", nil); err == nil {
		t.Fatal("expected empty DSN to fail")
	}
	if _, err := NewRepairPreviewExecutor("postgres://preview", "", "", nil); err == nil {
		t.Fatal("expected empty workload fingerprint to fail")
	}
}

func TestRepairPreviewExecutorRejectsUnexpectedWorkloadProfile(t *testing.T) {
	_, err := NewRepairPreviewExecutor(
		"postgres://preview", "../../../../deploy/repair-preview/pg-pool-workload.yaml", "sha256:not-current", nil,
	)
	if err == nil {
		t.Fatal("expected workload fingerprint mismatch to fail")
	}
}

func TestDeterministicPreviewRunIDIsStable(t *testing.T) {
	left := DeterministicPreviewRunID(
		1, ScenarioID, "final-demo-key", testPreviewTargetFingerprint, 100,
	)
	right := DeterministicPreviewRunID(
		1, ScenarioID, "final-demo-key", testPreviewTargetFingerprint, 100,
	)
	if left == "" || left != right {
		t.Fatalf("run IDs = %q/%q", left, right)
	}
	if left == DeterministicPreviewRunID(
		1, ScenarioID, "final-demo-key-2", testPreviewTargetFingerprint, 100,
	) {
		t.Fatal("expected a distinct run ID for a distinct idempotency key")
	}
	if left == DeterministicPreviewRunID(1, ScenarioID, "final-demo-key", "ffffffffffffffff", 100) {
		t.Fatal("expected a distinct run ID for a distinct target fingerprint")
	}
}

func previewIdentityDB(t *testing.T, scenarioID, targetFingerprint, workloadFingerprint string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Exec(`CREATE TABLE repair_preview_target_identity (
		singleton BOOLEAN PRIMARY KEY,
		scenario_id TEXT NOT NULL,
		target_fingerprint TEXT NOT NULL,
		workload_fingerprint TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if scenarioID != "" {
		if _, err := database.Exec(`INSERT INTO repair_preview_target_identity
			(singleton, scenario_id, target_fingerprint, workload_fingerprint)
			VALUES (TRUE, ?, ?, ?)`, scenarioID, targetFingerprint, workloadFingerprint); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func executorWithIdentityDB(
	t *testing.T, database *sql.DB, store PreviewExecutionStore,
) *RepairPreviewExecutor {
	t.Helper()
	return &RepairPreviewExecutor{
		expectedProfile: "sha256:workload", store: store,
		openPreviewDatabase: func() (*sql.DB, error) { return database, nil },
	}
}

func validPreviewExecutionInput() PreviewExecutionInput {
	return PreviewExecutionInput{
		RunID: "017f2b01-4001-4000-8000-000000000001", TenantID: "1", IncidentID: "100",
		ScenarioID: ScenarioID, IdempotencyKey: "final-demo-key",
		TargetFingerprint: testPreviewTargetFingerprint,
	}
}

func TestRepairPreviewExecutorRejectsWrongButValidDatabaseIdentity(t *testing.T) {
	database := previewIdentityDB(
		t, ScenarioID, testPreviewTargetFingerprint, "sha256:workload",
	)
	input := validPreviewExecutionInput()
	input.TargetFingerprint = "ffffffffffffffff"
	err := executorWithIdentityDB(t, database, &listOnlyPreviewStore{}).Execute(
		context.Background(), input,
	)
	if err == nil || !strings.Contains(err.Error(), "target identity mismatch") {
		t.Fatalf("err = %v", err)
	}
}

func TestRepairPreviewExecutorRejectsMissingDatabaseIdentity(t *testing.T) {
	database := previewIdentityDB(t, "", "", "")
	err := executorWithIdentityDB(t, database, &listOnlyPreviewStore{}).Execute(
		context.Background(), validPreviewExecutionInput(),
	)
	if err == nil || !strings.Contains(err.Error(), "target identity is missing") {
		t.Fatalf("err = %v", err)
	}
}

func TestRepairPreviewExecutorRejectsMismatchedScenarioBinding(t *testing.T) {
	input := validPreviewExecutionInput()
	input.ScenarioID = "cpu-overload"
	err := (&RepairPreviewExecutor{store: &listOnlyPreviewStore{}}).Execute(
		context.Background(), input,
	)
	if err == nil || !strings.Contains(err.Error(), "scenario binding mismatch") {
		t.Fatalf("err = %v", err)
	}
}

type listOnlyPreviewStore struct{ runs []repairpreview.Run }

func (store *listOnlyPreviewStore) Save(context.Context, repairpreview.Run) error {
	return repairpreview.ErrDuplicateRun
}

func (store *listOnlyPreviewStore) ListByIncident(
	context.Context, string, string, int,
) ([]repairpreview.Run, error) {
	return store.runs, nil
}

func boundExecutionRun(idempotencyKey string) repairpreview.Run {
	run := repairpreview.Run{
		ID: "017f2b01-4001-4000-8000-000000000001", TenantID: "1", IncidentID: "100",
		ScenarioID: ScenarioID, IdempotencyKey: idempotencyKey,
		TargetFingerprint:   testPreviewTargetFingerprint,
		WorkloadFingerprint: "sha256:workload",
	}
	run.BindingFingerprint = repairpreview.WorkloadBinding{
		RunID: run.ID, TenantID: run.TenantID, IncidentID: run.IncidentID,
		ScenarioID: run.ScenarioID, IdempotencyKey: run.IdempotencyKey,
		TargetFingerprint: run.TargetFingerprint,
	}.Fingerprint()
	return run
}

func TestRepairPreviewDuplicateRequiresExactScenarioIdempotencyAndTargetBinding(t *testing.T) {
	requested := boundExecutionRun("final-demo-key")
	persisted := boundExecutionRun("other-demo-key")
	persisted.ID = requested.ID
	persisted.BindingFingerprint = repairpreview.WorkloadBinding{
		RunID: persisted.ID, TenantID: persisted.TenantID, IncidentID: persisted.IncidentID,
		ScenarioID: persisted.ScenarioID, IdempotencyKey: persisted.IdempotencyKey,
		TargetFingerprint: persisted.TargetFingerprint,
	}.Fingerprint()
	store := &listOnlyPreviewStore{runs: []repairpreview.Run{persisted}}
	executor := &RepairPreviewExecutor{expectedProfile: "sha256:workload", store: store}

	err := executor.confirmDuplicate(context.Background(), requested)
	if !errors.Is(err, repairpreview.ErrDuplicateRun) {
		t.Fatalf("err = %v", err)
	}
}

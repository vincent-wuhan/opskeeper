package demo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	repairpreview "github.com/vincent-wuhan/opskeeper/core/domains/control/repairpreview"
)

const defaultPreviewWorkloadPath = "deploy/repair-preview/pg-pool-workload.yaml"

type PreviewExecutionStore interface {
	Save(ctx context.Context, run repairpreview.Run) error
	ListByIncident(ctx context.Context, tenantID, incidentID string, limit int) ([]repairpreview.Run, error)
}

type RepairPreviewExecutor struct {
	dsn                 string
	spec                repairpreview.WorkloadSpec
	expectedProfile     string
	store               PreviewExecutionStore
	openPreviewDatabase func() (*sql.DB, error)
}

func NewRepairPreviewExecutor(
	dsn, workloadPath, expectedProfile string, store PreviewExecutionStore,
) (*RepairPreviewExecutor, error) {
	if dsn == "" {
		return nil, errors.New("repair preview DSN is required")
	}
	if expectedProfile == "" {
		return nil, errors.New("repair preview workload fingerprint is required")
	}
	if workloadPath == "" {
		workloadPath = defaultPreviewWorkloadPath
	}
	workload, err := os.ReadFile(workloadPath)
	if err != nil {
		return nil, fmt.Errorf("read repair preview workload: %w", err)
	}
	spec, err := repairpreview.LoadWorkload(workload)
	if err != nil {
		return nil, err
	}
	if spec.WorkloadFingerprint() != expectedProfile {
		return nil, fmt.Errorf(
			"repair preview workload fingerprint does not match configured profile: actual=%s expected=%s",
			spec.WorkloadFingerprint(), expectedProfile,
		)
	}
	return &RepairPreviewExecutor{
		dsn: dsn, spec: spec, expectedProfile: expectedProfile, store: store,
		openPreviewDatabase: func() (*sql.DB, error) { return sql.Open("pgx", dsn) },
	}, nil
}

func (executor *RepairPreviewExecutor) Execute(ctx context.Context, input PreviewExecutionInput) error {
	if executor == nil || executor.store == nil {
		return errors.New("repair preview executor is not configured")
	}
	if input.RunID == "" || input.TenantID == "" || input.IncidentID == "" ||
		input.ScenarioID == "" || input.IdempotencyKey == "" || input.TargetFingerprint == "" {
		return errors.New("repair preview execution binding is incomplete")
	}
	if input.ScenarioID != ScenarioID {
		return errors.New("repair preview scenario binding mismatch")
	}
	spec := executor.spec
	spec.RuntimeBinding = repairpreview.WorkloadBinding{
		RunID: input.RunID, TenantID: input.TenantID, IncidentID: input.IncidentID,
		ScenarioID: input.ScenarioID, IdempotencyKey: input.IdempotencyKey,
		TargetFingerprint: input.TargetFingerprint,
	}

	database, err := executor.openPreviewDatabase()
	if err != nil {
		return sanitizePreviewError(err)
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		return sanitizePreviewError(err)
	}
	identity, err := repairpreview.ReadTargetIdentity(ctx, database)
	if err != nil {
		return sanitizePreviewError(err)
	}
	if identity.ScenarioID != input.ScenarioID ||
		identity.TargetFingerprint != input.TargetFingerprint ||
		identity.WorkloadFingerprint != executor.expectedProfile {
		return errors.New("repair preview target identity mismatch")
	}
	run, err := repairpreview.Execute(ctx, database, spec)
	if err != nil {
		return sanitizePreviewError(err)
	}
	if err := executor.store.Save(ctx, run); err != nil {
		if errors.Is(err, repairpreview.ErrDuplicateRun) {
			return executor.confirmDuplicate(ctx, run)
		}
		return sanitizePreviewError(err)
	}
	return nil
}

func (executor *RepairPreviewExecutor) confirmDuplicate(
	ctx context.Context, run repairpreview.Run,
) error {
	runs, err := executor.store.ListByIncident(ctx, run.TenantID, run.IncidentID, 20)
	if err != nil {
		return sanitizePreviewError(err)
	}
	for _, persisted := range runs {
		if persisted.ID == run.ID &&
			persisted.TenantID == run.TenantID &&
			persisted.IncidentID == run.IncidentID &&
			persisted.WorkloadFingerprint == executor.expectedProfile &&
			repairpreview.RunBindingMatches(
				persisted, run.ScenarioID, run.IdempotencyKey, run.TargetFingerprint,
			) {
			return nil
		}
	}
	return repairpreview.ErrDuplicateRun
}

func DeterministicPreviewRunID(
	tenantID uint64, scenarioID, idempotencyKey, targetFingerprint string, incidentID uint64,
) string {
	material := fmt.Sprintf(
		"opskeeper-final-demo:%d:%s:%s:%s:%d",
		tenantID, scenarioID, idempotencyKey, targetFingerprint, incidentID,
	)
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(material)).String()
}

func sanitizePreviewError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(repairpreview.SanitizeErrorSummary(err.Error()))
}

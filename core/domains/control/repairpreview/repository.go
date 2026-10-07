package repairpreview

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

var (
	ErrDuplicateRun      = errors.New("repair preview: run already exists")
	ErrCandidateNotFound = errors.New("repair preview: eligible candidate not found")
)

type Repository interface {
	Save(ctx context.Context, run Run) error
	ListByIncident(ctx context.Context, tenantID, incidentID string, limit int) ([]Run, error)
	FindEligible(ctx context.Context, tenantID, incidentID, runID, candidateID, action string) (Candidate, error)
}

type SQLRepository struct {
	db *gorm.DB
}

func NewSQLRepository(db *gorm.DB) *SQLRepository {
	return &SQLRepository{db: db}
}

func (repository *SQLRepository) Save(ctx context.Context, run Run) error {
	if err := run.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	run.UpdatedAt = now
	run.ErrorSummary = SanitizeErrorSummary(run.ErrorSummary)
	runRow := runRowFromRun(run)
	candidateRows := make([]candidateRow, 0, len(run.Candidates))
	for index := range run.Candidates {
		candidate := run.Candidates[index]
		decision, reason := Evaluate(candidate)
		candidate.Decision = decision
		candidate.RejectionReason = reason
		run.Candidates[index] = candidate
		row := candidateRowFromCandidate(candidate)
		row.CreatedAt = now
		candidateRows = append(candidateRows, row)
	}
	runRow.Candidates = candidateRows
	bindingRow, hasBinding, err := runBindingRowFromRun(run)
	if err != nil {
		return err
	}

	err = repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Create(&runRow)
		if result.Error != nil {
			if isUniqueConstraintError(result.Error) {
				return ErrDuplicateRun
			}
			return fmt.Errorf("save repair preview run: %w", result.Error)
		}
		for index := range runRow.Candidates {
			result := tx.Create(&runRow.Candidates[index])
			if result.Error != nil {
				if isUniqueConstraintError(result.Error) {
					return ErrDuplicateRun
				}
				return fmt.Errorf("save repair preview candidate %d: %w", index, result.Error)
			}
		}
		if hasBinding {
			if err := tx.Create(&bindingRow).Error; err != nil {
				if isUniqueConstraintError(err) {
					return ErrDuplicateRun
				}
				return fmt.Errorf("save repair preview run binding: %w", err)
			}
		}
		return nil
	})
	return err
}

func (repository *SQLRepository) ListByIncident(ctx context.Context, tenantID, incidentID string, limit int) ([]Run, error) {
	if tenantID == "" || incidentID == "" {
		return nil, errors.New("repair preview: tenant and incident ids are required")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	var rows []runRow
	err := repository.db.WithContext(ctx).
		Where("tenant_id = ? AND incident_id = ?", tenantID, incidentID).
		Order("started_at DESC, id DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list repair previews: %w", err)
	}
	runIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		runIDs = append(runIDs, row.ID)
	}
	var candidateRows []candidateRow
	if len(runIDs) > 0 {
		err = repository.db.WithContext(ctx).
			Where("run_id IN ?", runIDs).
			Order("candidate_id ASC, id ASC").Find(&candidateRows).Error
		if err != nil {
			return nil, fmt.Errorf("list repair preview candidates: %w", err)
		}
	}
	candidatesByRun := make(map[string][]Candidate, len(rows))
	for _, row := range candidateRows {
		candidatesByRun[row.RunID] = append(candidatesByRun[row.RunID], candidateFromRow(row))
	}
	var bindingRows []runBindingRow
	if err := repository.db.WithContext(ctx).Where("run_id IN ?", runIDs).Find(&bindingRows).Error; err != nil {
		return nil, fmt.Errorf("list repair preview run bindings: %w", err)
	}
	bindingByRun := make(map[string]runBindingRow, len(bindingRows))
	for _, row := range bindingRows {
		bindingByRun[row.RunID] = row
	}
	runs := make([]Run, 0, len(rows))
	for _, row := range rows {
		run := runFromRow(row)
		if binding, ok := bindingByRun[row.ID]; ok {
			run.ScenarioID = binding.ScenarioID
			run.IdempotencyKey = binding.IdempotencyKey
			run.TargetFingerprint = binding.TargetFingerprint
			run.BindingFingerprint = binding.BindingFingerprint
		}
		run.Candidates = candidatesByRun[row.ID]
		if run.Candidates == nil {
			run.Candidates = []Candidate{}
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func (repository *SQLRepository) FindEligible(ctx context.Context, tenantID, incidentID, runID, candidateID, action string) (Candidate, error) {
	if tenantID == "" || incidentID == "" || runID == "" || candidateID == "" || action == "" {
		return Candidate{}, ErrCandidateNotFound
	}
	if candidateID == BaselineCandidateID || action == BaselineAction {
		return Candidate{}, ErrCandidateNotFound
	}
	var row candidateRow
	err := repository.db.WithContext(ctx).
		Where(
			"tenant_id = ? AND incident_id = ? AND run_id = ? AND candidate_id = ? AND action = ? AND decision = ?",
			tenantID, incidentID, runID, candidateID, action, string(DecisionPass),
		).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Candidate{}, ErrCandidateNotFound
		}
		return Candidate{}, fmt.Errorf("find eligible repair candidate: %w", err)
	}
	return candidateFromRow(row), nil
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "duplicate key value") ||
		strings.Contains(message, "duplicate entry")
}

type runRow struct {
	ID                  string         `gorm:"column:id;primaryKey"`
	RunID               string         `gorm:"column:run_id"`
	TenantID            string         `gorm:"column:tenant_id"`
	IncidentID          string         `gorm:"column:incident_id"`
	BranchPrefix        string         `gorm:"column:branch_prefix"`
	SeedFingerprint     string         `gorm:"column:seed_fingerprint"`
	WorkloadFingerprint string         `gorm:"column:workload_fingerprint"`
	WorkloadRevision    string         `gorm:"column:workload_revision"`
	ControlledLoad      bool           `gorm:"column:controlled_load"`
	IsolationBoundary   string         `gorm:"column:isolation_boundary"`
	Status              string         `gorm:"column:status"`
	StartedAt           time.Time      `gorm:"column:started_at"`
	FinishedAt          sql.NullTime   `gorm:"column:finished_at"`
	ErrorSummary        string         `gorm:"column:error_summary"`
	ArtifactRef         string         `gorm:"column:artifact_ref"`
	CreatedAt           time.Time      `gorm:"column:created_at"`
	UpdatedAt           time.Time      `gorm:"column:updated_at"`
	Candidates          []candidateRow `gorm:"-"`
}

func (runRow) TableName() string { return "repair_preview_runs" }

type runBindingRow struct {
	RunID              string `gorm:"column:run_id;primaryKey"`
	BindingFingerprint string `gorm:"column:binding_fingerprint"`
	ScenarioID         string `gorm:"column:scenario_id"`
	IdempotencyKey     string `gorm:"column:idempotency_key"`
	TargetFingerprint  string `gorm:"column:target_fingerprint"`
}

func (runBindingRow) TableName() string { return "repair_preview_run_bindings" }

type candidateRow struct {
	ID                string    `gorm:"column:id;primaryKey"`
	RunID             string    `gorm:"column:run_id"`
	TenantID          string    `gorm:"column:tenant_id"`
	IncidentID        string    `gorm:"column:incident_id"`
	CandidateID       string    `gorm:"column:candidate_id"`
	Name              string    `gorm:"column:name"`
	Kind              string    `gorm:"column:kind"`
	Action            string    `gorm:"column:action"`
	ChangeSummary     string    `gorm:"column:change_summary"`
	Branch            string    `gorm:"column:branch"`
	ResultChecksum    string    `gorm:"column:result_checksum"`
	Consistent        bool      `gorm:"column:consistent"`
	AverageLatencyMS  float64   `gorm:"column:average_latency_ms"`
	MedianLatencyMS   float64   `gorm:"column:median_latency_ms"`
	P95LatencyMS      float64   `gorm:"column:p95_latency_ms"`
	SampleCount       int       `gorm:"column:sample_count"`
	TPS               float64   `gorm:"column:tps"`
	ErrorCount        int       `gorm:"column:error_count"`
	WriteImpact       string    `gorm:"column:write_impact"`
	StorageDeltaBytes int64     `gorm:"column:storage_delta_bytes"`
	BusinessProbePass bool      `gorm:"column:business_probe_pass"`
	Decision          string    `gorm:"column:decision"`
	RejectionReason   string    `gorm:"column:rejection_reason"`
	CreatedAt         time.Time `gorm:"column:created_at"`
}

func (candidateRow) TableName() string { return "repair_preview_candidates" }

func runRowFromRun(run Run) runRow {
	return runRow{
		ID: run.ID, RunID: run.ID, TenantID: run.TenantID, IncidentID: run.IncidentID,
		BranchPrefix: run.BranchPrefix, SeedFingerprint: run.SeedFingerprint,
		WorkloadFingerprint: run.WorkloadFingerprint, WorkloadRevision: run.WorkloadRevision,
		ControlledLoad: run.ControlledLoad, IsolationBoundary: run.IsolationBoundary,
		Status: run.Status, StartedAt: run.StartedAt.UTC(),
		FinishedAt:   sql.NullTime{Time: run.FinishedAt.UTC(), Valid: !run.FinishedAt.IsZero()},
		ErrorSummary: SanitizeErrorSummary(run.ErrorSummary), ArtifactRef: run.ArtifactRef,
		CreatedAt: run.CreatedAt.UTC(), UpdatedAt: run.UpdatedAt.UTC(),
	}
}

func runFromRow(row runRow) Run {
	return Run{
		ID: row.ID, TenantID: row.TenantID, IncidentID: row.IncidentID,
		BranchPrefix: row.BranchPrefix, SeedFingerprint: row.SeedFingerprint,
		WorkloadFingerprint: row.WorkloadFingerprint, WorkloadRevision: row.WorkloadRevision,
		ControlledLoad: row.ControlledLoad, IsolationBoundary: row.IsolationBoundary,
		Status: row.Status, StartedAt: row.StartedAt.UTC(),
		FinishedAt: finishedAtFromRow(row), ErrorSummary: row.ErrorSummary,
		ArtifactRef: row.ArtifactRef, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}

func runBindingRowFromRun(run Run) (runBindingRow, bool, error) {
	hasBinding := run.ScenarioID != "" || run.IdempotencyKey != "" ||
		run.TargetFingerprint != "" || run.BindingFingerprint != ""
	if !hasBinding {
		return runBindingRow{}, false, nil
	}
	if run.ScenarioID == "" || run.IdempotencyKey == "" || run.TargetFingerprint == "" {
		return runBindingRow{}, false, errors.New("repair preview: incomplete run binding")
	}
	binding := WorkloadBinding{
		RunID: run.ID, TenantID: run.TenantID, IncidentID: run.IncidentID,
		ScenarioID: run.ScenarioID, IdempotencyKey: run.IdempotencyKey,
		TargetFingerprint: run.TargetFingerprint,
	}
	if binding.Fingerprint() != run.BindingFingerprint {
		return runBindingRow{}, false, errors.New("repair preview: run binding fingerprint mismatch")
	}
	return runBindingRow{
		RunID: run.ID, BindingFingerprint: run.BindingFingerprint, ScenarioID: run.ScenarioID,
		IdempotencyKey: run.IdempotencyKey, TargetFingerprint: run.TargetFingerprint,
	}, true, nil
}

func finishedAtFromRow(row runRow) time.Time {
	if !row.FinishedAt.Valid {
		return time.Time{}
	}
	return row.FinishedAt.Time.UTC()
}

func candidateRowFromCandidate(candidate Candidate) candidateRow {
	return candidateRow{
		ID: candidate.ID, RunID: candidate.RunID, TenantID: candidate.TenantID,
		IncidentID: candidate.IncidentID, CandidateID: candidate.CandidateID, Name: candidate.Name,
		Kind: candidate.Kind, Action: candidate.Action, ChangeSummary: candidate.ChangeSummary,
		Branch: candidate.Branch, ResultChecksum: candidate.ResultChecksum, Consistent: candidate.Consistent,
		AverageLatencyMS: candidate.AverageLatencyMS, MedianLatencyMS: candidate.MedianLatencyMS,
		P95LatencyMS: candidate.P95LatencyMS, SampleCount: candidate.SampleCount, TPS: candidate.TPS,
		ErrorCount: candidate.ErrorCount, WriteImpact: candidate.WriteImpact,
		StorageDeltaBytes: candidate.StorageDeltaBytes, BusinessProbePass: candidate.BusinessProbePass,
		Decision: string(candidate.Decision), RejectionReason: candidate.RejectionReason,
	}
}

func candidateFromRow(row candidateRow) Candidate {
	return Candidate{
		ID: row.ID, RunID: row.RunID, TenantID: row.TenantID, IncidentID: row.IncidentID,
		CandidateID: row.CandidateID, Name: row.Name, Kind: row.Kind, Action: row.Action,
		ChangeSummary: row.ChangeSummary, Branch: row.Branch, ResultChecksum: row.ResultChecksum,
		Consistent: row.Consistent, AverageLatencyMS: row.AverageLatencyMS,
		MedianLatencyMS: row.MedianLatencyMS, P95LatencyMS: row.P95LatencyMS,
		SampleCount: row.SampleCount, TPS: row.TPS, ErrorCount: row.ErrorCount,
		WriteImpact: row.WriteImpact, StorageDeltaBytes: row.StorageDeltaBytes,
		BusinessProbePass: row.BusinessProbePass, Decision: Decision(row.Decision),
		RejectionReason: row.RejectionReason,
	}
}

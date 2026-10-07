package repairpreview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSQLRepository_SavePersistsRunAndCandidates(t *testing.T) {
	repository, _ := setupRepository(t)
	run := validRun()
	run.Candidates = []Candidate{
		validCandidate("candidate-a", "resize_pool"),
		validCandidate("candidate-b", "reset_pool"),
	}
	run.Candidates[1].BusinessProbePass = false

	require.NoError(t, repository.Save(context.Background(), run))

	stored, err := repository.ListByIncident(context.Background(), run.TenantID, run.IncidentID, 10)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, run.ID, stored[0].ID)
	require.Len(t, stored[0].Candidates, 2)
	require.Equal(t, DecisionPass, stored[0].Candidates[0].Decision)
	require.Equal(t, DecisionReject, stored[0].Candidates[1].Decision)
	require.Contains(t, stored[0].Candidates[1].RejectionReason, "business probe")
}

func TestSQLRepository_SavePersistsExactRunBinding(t *testing.T) {
	repository, _ := setupRepository(t)
	run := boundPreviewRun("pg-pool-exhaustion", "final-demo-key", "0123456789abcdef")
	run.Candidates = []Candidate{validCandidate("candidate-a", "resize_pool")}
	require.NoError(t, repository.Save(context.Background(), run))

	stored, err := repository.ListByIncident(context.Background(), run.TenantID, run.IncidentID, 10)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.True(t, RunBindingMatches(
		stored[0], "pg-pool-exhaustion", "final-demo-key", "0123456789abcdef",
	))
}

func TestSQLRepository_SaveRejectsIncompleteAndMismatchedRunBindings(t *testing.T) {
	repository, _ := setupRepository(t)
	incomplete := boundPreviewRun("pg-pool-exhaustion", "final-demo-key", "0123456789abcdef")
	incomplete.TargetFingerprint = ""
	require.ErrorContains(
		t, repository.Save(context.Background(), incomplete), "incomplete run binding",
	)

	mismatched := boundPreviewRun("pg-pool-exhaustion", "final-demo-key", "0123456789abcdef")
	mismatched.IdempotencyKey = "other-key"
	require.ErrorContains(
		t, repository.Save(context.Background(), mismatched), "binding fingerprint mismatch",
	)
}

func TestSQLRepository_DuplicatesAreRejected(t *testing.T) {
	repository, db := setupRepository(t)
	run := validRun()
	require.NoError(t, repository.Save(context.Background(), run))

	err := repository.Save(context.Background(), run)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrDuplicateRun), err)

	duplicate := validCandidate("candidate-a", "resize_pool")
	duplicate.ID = "017f2b01-3199-4000-8000-000000000099"
	err = db.Exec(`INSERT INTO repair_preview_candidates (
		id, run_id, tenant_id, incident_id, candidate_id, name, kind, action, change_summary, branch,
		result_checksum, consistent, average_latency_ms, median_latency_ms, p95_latency_ms, sample_count,
		tps, error_count, write_impact, storage_delta_bytes, business_probe_pass, decision, rejection_reason, created_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		duplicate.ID, duplicate.RunID, duplicate.TenantID, duplicate.IncidentID, duplicate.CandidateID,
		duplicate.Name, duplicate.Kind, duplicate.Action, duplicate.ChangeSummary, duplicate.Branch,
		duplicate.ResultChecksum, duplicate.Consistent, duplicate.AverageLatencyMS, duplicate.MedianLatencyMS,
		duplicate.P95LatencyMS, duplicate.SampleCount, duplicate.TPS, duplicate.ErrorCount,
		duplicate.WriteImpact, duplicate.StorageDeltaBytes, duplicate.BusinessProbePass,
		string(DecisionPass), duplicate.RejectionReason, time.Now().UTC()).Error
	require.Error(t, err)
}

func TestSQLRepository_SaveRollsBackRunWhenCandidateFails(t *testing.T) {
	repository, db := setupRepository(t)
	require.NoError(t, db.Exec(`CREATE TRIGGER reject_candidate BEFORE INSERT ON repair_preview_candidates
		WHEN NEW.candidate_id = 'rollback-candidate'
		BEGIN
			SELECT RAISE(ABORT, 'candidate rejected');
		END`).Error)
	run := validRun()
	run.Candidates = []Candidate{validCandidate("rollback-candidate", "resize_pool")}

	err := repository.Save(context.Background(), run)

	require.ErrorContains(t, err, "candidate rejected")
	var count int64
	require.NoError(t, db.Model(&runRow{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestSQLRepository_ListByIncident_IsScopedAndBounded(t *testing.T) {
	repository, _ := setupRepository(t)
	for index := 0; index < 3; index++ {
		run := validRun()
		run.ID = time.Now().Add(time.Duration(index) * time.Minute).Format("150405.000000000")
		run.BranchPrefix += "-" + run.ID
		run.Candidates[0].RunID = run.ID
		run.Candidates[0].ID = run.ID + "-candidate"
		run.StartedAt = run.StartedAt.Add(time.Duration(index) * time.Minute)
		run.FinishedAt = run.FinishedAt.Add(time.Duration(index) * time.Minute)
		require.NoError(t, repository.Save(context.Background(), run))
	}
	otherTenant := validRun()
	otherTenant.ID = "other-tenant-run"
	otherTenant.TenantID = "other-tenant"
	otherTenant.Candidates[0].RunID = otherTenant.ID
	otherTenant.Candidates[0].TenantID = otherTenant.TenantID
	require.NoError(t, repository.Save(context.Background(), otherTenant))

	stored, err := repository.ListByIncident(context.Background(), "opskeeper-demo", "INC-REPAIR-001", 2)
	require.NoError(t, err)
	require.Len(t, stored, 2)
	for _, run := range stored {
		require.Equal(t, "opskeeper-demo", run.TenantID)
		require.Equal(t, "INC-REPAIR-001", run.IncidentID)
	}
}

func TestSQLRepository_FindEligible_RequiresExactBindingsAndPass(t *testing.T) {
	repository, _ := setupRepository(t)
	run := validRun()
	baseline := validCandidate("baseline", "baseline")
	baseline.Kind = "baseline"
	baseline.ID = "017f2b01-3199-4000-8000-000000000098"
	run.Candidates = []Candidate{baseline, validCandidate("candidate-a", "resize_pool")}
	require.NoError(t, repository.Save(context.Background(), run))

	candidate, err := repository.FindEligible(context.Background(), run.TenantID, run.IncidentID, run.ID, "candidate-a", "resize_pool")
	require.NoError(t, err)
	require.Equal(t, "candidate-a", candidate.CandidateID)
	_, err = repository.FindEligible(context.Background(), run.TenantID, run.IncidentID, run.ID, "baseline", "baseline")
	require.True(t, errors.Is(err, ErrCandidateNotFound), err)

	_, err = repository.FindEligible(context.Background(), "other-tenant", run.IncidentID, run.ID, "candidate-a", "resize_pool")
	require.True(t, errors.Is(err, ErrCandidateNotFound), err)

	_, err = repository.FindEligible(context.Background(), run.TenantID, run.IncidentID, run.ID, "candidate-a", "reset_pool")
	require.True(t, errors.Is(err, ErrCandidateNotFound), err)

	_, err = repository.FindEligible(context.Background(), run.TenantID, run.IncidentID, "missing-run", "candidate-a", "resize_pool")
	require.True(t, errors.Is(err, ErrCandidateNotFound), err)
}

func TestMigrate_CreatesSQLiteSchema(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	require.NoError(t, Migrate(db))
	require.NoError(t, Migrate(db))
	require.True(t, db.Migrator().HasTable("repair_preview_runs"))
	require.True(t, db.Migrator().HasTable("repair_preview_candidates"))
	require.True(t, db.Migrator().HasTable("repair_preview_run_bindings"))
}

func TestMigrateAddsBindingTableToExistingSchema(t *testing.T) {
	repository, db := setupRepository(t)
	require.NoError(t, db.Migrator().DropTable("repair_preview_run_bindings"))
	require.NoError(t, Migrate(db))
	require.True(t, db.Migrator().HasTable("repair_preview_run_bindings"))
	run := boundPreviewRun("pg-pool-exhaustion", "final-demo-key", "0123456789abcdef")
	require.NoError(t, repository.Save(context.Background(), run))
}

func setupRepository(t *testing.T) (Repository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`CREATE TABLE repair_preview_runs (
		id text PRIMARY KEY, run_id text NOT NULL, tenant_id text NOT NULL, incident_id text NOT NULL,
		branch_prefix text NOT NULL, seed_fingerprint text NOT NULL, workload_fingerprint text NOT NULL,
		workload_revision text NOT NULL, controlled_load numeric NOT NULL, isolation_boundary text NOT NULL,
		status text NOT NULL, started_at datetime NOT NULL, finished_at datetime, error_summary text NOT NULL DEFAULT '',
		artifact_ref text NOT NULL DEFAULT '', created_at datetime NOT NULL, updated_at datetime NOT NULL,
		UNIQUE(tenant_id, incident_id, run_id)
	)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE repair_preview_candidates (
		id text PRIMARY KEY, run_id text NOT NULL, tenant_id text NOT NULL, incident_id text NOT NULL,
		candidate_id text NOT NULL, name text NOT NULL, kind text NOT NULL, action text NOT NULL,
		change_summary text NOT NULL, branch text NOT NULL, result_checksum text NOT NULL,
		consistent numeric NOT NULL, average_latency_ms real NOT NULL, median_latency_ms real NOT NULL,
		p95_latency_ms real NOT NULL, sample_count integer NOT NULL, tps real NOT NULL,
		error_count integer NOT NULL, write_impact text NOT NULL, storage_delta_bytes integer NOT NULL,
		business_probe_pass numeric NOT NULL, decision text NOT NULL, rejection_reason text NOT NULL DEFAULT '',
		created_at datetime NOT NULL, UNIQUE(run_id, candidate_id)
		)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE repair_preview_run_bindings (
		run_id text PRIMARY KEY, binding_fingerprint text NOT NULL, scenario_id text NOT NULL,
		idempotency_key text NOT NULL, target_fingerprint text NOT NULL
	)`).Error)
	return NewSQLRepository(db), db
}

func boundPreviewRun(scenarioID, idempotencyKey, targetFingerprint string) Run {
	run := validRun()
	run.ScenarioID = scenarioID
	run.IdempotencyKey = idempotencyKey
	run.TargetFingerprint = targetFingerprint
	run.BindingFingerprint = WorkloadBinding{
		RunID: run.ID, TenantID: run.TenantID, IncidentID: run.IncidentID,
		ScenarioID: scenarioID, IdempotencyKey: idempotencyKey,
		TargetFingerprint: targetFingerprint,
	}.Fingerprint()
	return run
}

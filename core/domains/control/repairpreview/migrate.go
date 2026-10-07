package repairpreview

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

func Migrate(db *gorm.DB) error {
	if db == nil {
		return errors.New("repair preview migration: nil db")
	}
	switch db.Dialector.Name() {
	case "postgres":
		if db.Migrator().HasTable(&runRow{}) && db.Migrator().HasTable(&candidateRow{}) {
			return ensureRunBindingTable(db)
		}
		if err := db.Exec(postgresSchema).Error; err != nil {
			return fmt.Errorf("repair preview migration: create schema: %w", err)
		}
		return ensureRunBindingTable(db)
	case "sqlite":
		if db.Migrator().HasTable(&runRow{}) && db.Migrator().HasTable(&candidateRow{}) {
			return ensureRunBindingTable(db)
		}
		if err := db.Exec(sqliteSchema).Error; err != nil {
			return fmt.Errorf("repair preview migration: create schema: %w", err)
		}
		return ensureRunBindingTable(db)
	case "mysql":
		for _, statement := range mysqlSchema {
			if err := db.Exec(statement).Error; err != nil {
				return fmt.Errorf("repair preview migration: create schema: %w", err)
			}
		}
		if err := ensureMySQLIndexes(db); err != nil {
			return err
		}
		return ensureRunBindingTable(db)
	default:
		return fmt.Errorf("repair preview migration: unsupported dialect %q", db.Dialector.Name())
	}
}

func ensureRunBindingTable(db *gorm.DB) error {
	schema := `CREATE TABLE IF NOT EXISTS repair_preview_run_bindings (
		run_id TEXT PRIMARY KEY,
		binding_fingerprint TEXT NOT NULL,
		scenario_id TEXT NOT NULL,
		idempotency_key TEXT NOT NULL,
		target_fingerprint TEXT NOT NULL
	)`
	if db.Dialector.Name() == "mysql" {
		schema = `CREATE TABLE IF NOT EXISTS repair_preview_run_bindings (
			run_id VARCHAR(191) PRIMARY KEY,
			binding_fingerprint VARCHAR(191) NOT NULL,
			scenario_id VARCHAR(191) NOT NULL,
			idempotency_key VARCHAR(191) NOT NULL,
			target_fingerprint VARCHAR(191) NOT NULL
		)`
	}
	if err := db.Exec(schema).Error; err != nil {
		return fmt.Errorf("repair preview migration: create run bindings: %w", err)
	}
	return nil
}

func ensureMySQLIndexes(db *gorm.DB) error {
	indexes := []struct {
		model any
		name  string
		sql   string
	}{
		{&runRow{}, "idx_repair_preview_runs_incident", "CREATE INDEX idx_repair_preview_runs_incident ON repair_preview_runs (tenant_id, incident_id, created_at)"},
		{&candidateRow{}, "idx_repair_preview_candidates_run", "CREATE INDEX idx_repair_preview_candidates_run ON repair_preview_candidates (run_id, candidate_id)"},
	}
	for _, index := range indexes {
		if db.Migrator().HasIndex(index.model, index.name) {
			continue
		}
		if err := db.Exec(index.sql).Error; err != nil {
			return fmt.Errorf("repair preview migration: ensure index %s: %w", index.name, err)
		}
	}
	return nil
}

const postgresSchema = `
CREATE TABLE IF NOT EXISTS repair_preview_runs (
    id UUID PRIMARY KEY,
    run_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    branch_prefix TEXT NOT NULL,
    seed_fingerprint TEXT NOT NULL,
    workload_fingerprint TEXT NOT NULL,
    workload_revision TEXT NOT NULL,
    controlled_load BOOLEAN NOT NULL,
    isolation_boundary TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    error_summary TEXT NOT NULL DEFAULT '',
    artifact_ref TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, incident_id, run_id)
);
CREATE TABLE IF NOT EXISTS repair_preview_candidates (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL,
    tenant_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    candidate_id TEXT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    action TEXT NOT NULL,
    change_summary TEXT NOT NULL,
    branch TEXT NOT NULL,
    result_checksum TEXT NOT NULL,
    consistent BOOLEAN NOT NULL,
    average_latency_ms DOUBLE PRECISION NOT NULL,
    median_latency_ms DOUBLE PRECISION NOT NULL,
    p95_latency_ms DOUBLE PRECISION NOT NULL,
    sample_count INTEGER NOT NULL,
    tps DOUBLE PRECISION NOT NULL,
    error_count INTEGER NOT NULL,
    write_impact TEXT NOT NULL,
    storage_delta_bytes BIGINT NOT NULL,
    business_probe_pass BOOLEAN NOT NULL,
    decision TEXT NOT NULL,
    rejection_reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, candidate_id)
);
CREATE INDEX IF NOT EXISTS idx_repair_preview_runs_incident
    ON repair_preview_runs (tenant_id, incident_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_repair_preview_candidates_run
    ON repair_preview_candidates (run_id, candidate_id);
`

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS repair_preview_runs (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    branch_prefix TEXT NOT NULL,
    seed_fingerprint TEXT NOT NULL,
    workload_fingerprint TEXT NOT NULL,
    workload_revision TEXT NOT NULL,
    controlled_load NUMERIC NOT NULL,
    isolation_boundary TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at DATETIME NOT NULL,
    finished_at DATETIME,
    error_summary TEXT NOT NULL DEFAULT '',
    artifact_ref TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    UNIQUE (tenant_id, incident_id, run_id)
);
CREATE TABLE IF NOT EXISTS repair_preview_candidates (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    candidate_id TEXT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    action TEXT NOT NULL,
    change_summary TEXT NOT NULL,
    branch TEXT NOT NULL,
    result_checksum TEXT NOT NULL,
    consistent NUMERIC NOT NULL,
    average_latency_ms REAL NOT NULL,
    median_latency_ms REAL NOT NULL,
    p95_latency_ms REAL NOT NULL,
    sample_count INTEGER NOT NULL,
    tps REAL NOT NULL,
    error_count INTEGER NOT NULL,
    write_impact TEXT NOT NULL,
    storage_delta_bytes INTEGER NOT NULL,
    business_probe_pass NUMERIC NOT NULL,
    decision TEXT NOT NULL,
    rejection_reason TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    UNIQUE (run_id, candidate_id)
);
CREATE INDEX IF NOT EXISTS idx_repair_preview_runs_incident
    ON repair_preview_runs (tenant_id, incident_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_repair_preview_candidates_run
    ON repair_preview_candidates (run_id, candidate_id);
`

var mysqlSchema = []string{
	`CREATE TABLE IF NOT EXISTS repair_preview_runs (
		id VARCHAR(36) PRIMARY KEY,
		run_id VARCHAR(191) NOT NULL,
		tenant_id VARCHAR(191) NOT NULL,
		incident_id VARCHAR(191) NOT NULL,
		branch_prefix VARCHAR(512) NOT NULL,
		seed_fingerprint VARCHAR(191) NOT NULL,
		workload_fingerprint VARCHAR(191) NOT NULL,
		workload_revision VARCHAR(191) NOT NULL,
		controlled_load BOOLEAN NOT NULL,
		isolation_boundary VARCHAR(191) NOT NULL,
		status VARCHAR(64) NOT NULL,
		started_at DATETIME(6) NOT NULL,
		finished_at DATETIME(6) NULL,
		error_summary TEXT NOT NULL,
		artifact_ref TEXT NOT NULL,
		created_at DATETIME(6) NOT NULL,
		updated_at DATETIME(6) NOT NULL,
		UNIQUE KEY uq_repair_preview_run_business (tenant_id, incident_id, run_id)
	)`,
	`CREATE TABLE IF NOT EXISTS repair_preview_candidates (
		id VARCHAR(36) PRIMARY KEY,
		run_id VARCHAR(36) NOT NULL,
		tenant_id VARCHAR(191) NOT NULL,
		incident_id VARCHAR(191) NOT NULL,
		candidate_id VARCHAR(191) NOT NULL,
		name VARCHAR(191) NOT NULL,
		kind VARCHAR(64) NOT NULL,
		action VARCHAR(191) NOT NULL,
		change_summary TEXT NOT NULL,
		branch VARCHAR(512) NOT NULL,
		result_checksum VARCHAR(191) NOT NULL,
		consistent BOOLEAN NOT NULL,
		average_latency_ms DOUBLE NOT NULL,
		median_latency_ms DOUBLE NOT NULL,
		p95_latency_ms DOUBLE NOT NULL,
		sample_count INTEGER NOT NULL,
		tps DOUBLE NOT NULL,
		error_count INTEGER NOT NULL,
		write_impact VARCHAR(64) NOT NULL,
		storage_delta_bytes BIGINT NOT NULL,
		business_probe_pass BOOLEAN NOT NULL,
		decision VARCHAR(64) NOT NULL,
		rejection_reason TEXT NOT NULL,
		created_at DATETIME(6) NOT NULL,
		UNIQUE KEY uq_repair_preview_candidate (run_id, candidate_id)
	)`,
	// The two secondary indexes are NOT created here. They were, and this
	// list is replayed on every boot with no version ledger, so the second
	// boot of a MySQL deployment died with
	//
	//	Error 1061 (42000): Duplicate key name 'idx_repair_preview_runs_incident'
	//
	// and the manager never reached its HTTP listener. The tables below use
	// CREATE TABLE IF NOT EXISTS and therefore survive the replay; a bare
	// CREATE INDEX has no such form in MySQL (IF NOT EXISTS on CREATE INDEX
	// is MariaDB's, not MySQL's), so the only portable way to make the step
	// idempotent is to let ensureMySQLIndexes own it — which is what it
	// exists for, and which checks HasIndex before every statement.
	//
	// The tables carry their UNIQUE / PRIMARY keys inline, so nothing is
	// lost by the index DDL moving out of the schema list.
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	repairpreview "github.com/vincent-wuhan/opskeeper/core/domains/control/repairpreview"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	dsn := flag.String("dsn", "", "PostgreSQL preview database DSN (required)")
	controlDSN := flag.String("control-dsn", "", "Manager control-plane PostgreSQL DSN (defaults to --dsn)")
	tenantID := flag.String("tenant-id", "", "tenant ID (required)")
	incidentID := flag.String("incident-id", "", "incident ID (required)")
	scenarioID := flag.String("scenario-id", "", "scenario ID (required)")
	idempotencyKey := flag.String("idempotency-key", "", "scenario idempotency key (required)")
	targetFingerprint := flag.String("target-fingerprint", "", "target fingerprint (required)")
	workloadPath := flag.String("workload", "deploy/repair-preview/pg-pool-workload.yaml", "workload YAML path")
	runID := flag.String("run-id", "", "preview run UUID (required)")
	dryRun := flag.Bool("dry-run", false, "execute and print the run without saving")
	flag.Parse()

	if *dsn == "" || *tenantID == "" || *incidentID == "" || *runID == "" ||
		*scenarioID == "" || *idempotencyKey == "" || *targetFingerprint == "" {
		fmt.Fprintln(os.Stderr, "repair-preview-runner: --dsn, --tenant-id, --incident-id, --run-id, --scenario-id, --idempotency-key, and --target-fingerprint are required")
		os.Exit(2)
	}
	workloadData, err := os.ReadFile(*workloadPath)
	if err != nil {
		fail(err)
	}
	spec, err := repairpreview.LoadWorkload(workloadData)
	if err != nil {
		fail(err)
	}
	database, err := sqlOpen(*dsn)
	if err != nil {
		fail(err)
	}
	identity, err := repairpreview.ReadTargetIdentity(context.Background(), database)
	if err != nil {
		closeErr := database.Close()
		if err != nil {
			fail(err)
		}
		fail(closeErr)
	}
	if identity.ScenarioID != *scenarioID || identity.TargetFingerprint != *targetFingerprint ||
		identity.WorkloadFingerprint != spec.WorkloadFingerprint() {
		closeErr := database.Close()
		if closeErr != nil {
			fail(closeErr)
		}
		fail(fmt.Errorf("repair preview target identity mismatch"))
	}
	spec.RuntimeBinding = repairpreview.WorkloadBinding{
		RunID: *runID, TenantID: *tenantID, IncidentID: *incidentID,
		ScenarioID: *scenarioID, IdempotencyKey: *idempotencyKey,
		TargetFingerprint: *targetFingerprint,
	}
	run, err := repairpreview.Execute(context.Background(), database, spec)
	closeErr := database.Close()
	if err != nil {
		fail(err)
	}
	if closeErr != nil {
		fail(closeErr)
	}
	if !*dryRun {
		targetControlDSN := *controlDSN
		if targetControlDSN == "" {
			targetControlDSN = *dsn
		}
		controlDB, err := gorm.Open(postgres.Open(targetControlDSN), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			fail(err)
		}
		if err := repairpreview.Migrate(controlDB); err != nil {
			fail(err)
		}
		if err := repairpreview.NewSQLRepository(controlDB).Save(context.Background(), run); err != nil {
			fail(err)
		}
	}
	if err := printJSON(run); err != nil {
		fail(err)
	}
}

func sqlOpen(dsn string) (*sql.DB, error) {
	return sql.Open("pgx", dsn)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, repairpreview.SanitizeErrorSummary(err.Error()))
	os.Exit(1)
}

func printJSON(run repairpreview.Run) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(run)
}

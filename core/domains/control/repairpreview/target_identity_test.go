package repairpreview

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

func setupTargetIdentityDB(t *testing.T, insertRow string) *sql.DB {
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
	if insertRow != "" {
		if _, err := database.Exec(insertRow); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func TestReadTargetIdentityReturnsConfiguredIdentity(t *testing.T) {
	database := setupTargetIdentityDB(t, `INSERT INTO repair_preview_target_identity
		(singleton, scenario_id, target_fingerprint, workload_fingerprint)
		VALUES (TRUE, 'pg-pool-exhaustion', '0123456789abcdef', 'sha256:workload')`)

	identity, err := ReadTargetIdentity(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ScenarioID != "pg-pool-exhaustion" || identity.TargetFingerprint != "0123456789abcdef" ||
		identity.WorkloadFingerprint != "sha256:workload" {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestReadTargetIdentityFailsClosedWhenMissing(t *testing.T) {
	database := setupTargetIdentityDB(t, "")
	if _, err := ReadTargetIdentity(context.Background(), database); err == nil {
		t.Fatal("expected missing identity to fail")
	}
}

func TestDeployedTargetIdentitySeedAndMigrationAgree(t *testing.T) {
	seedData, err := os.ReadFile("../../../../deploy/repair-preview/seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	migrationData, err := os.ReadFile("../../../../deploy/repair-preview/migrate-target-identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"pg-pool-exhaustion",
		"0123456789abcdef0123456789abcdef",
		"sha256:db905b8f98c631212336b736f92d80b2a3040a75a44554687cfc782d39c31cc4",
	} {
		if !strings.Contains(string(seedData), value) || !strings.Contains(string(migrationData), value) {
			t.Fatalf("target identity value %s is absent from seed or migration", value)
		}
	}
}

func TestTargetIdentityMigrationDoesNotOverwriteExistingIdentity(t *testing.T) {
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
	if _, err := database.Exec(`INSERT INTO repair_preview_target_identity
		(singleton, scenario_id, target_fingerprint, workload_fingerprint)
		VALUES (TRUE, 'wrong-scenario', 'wrong-target', 'sha256:wrong-workload')`); err != nil {
		t.Fatal(err)
	}
	migrationData, err := os.ReadFile("../../../../deploy/repair-preview/migrate-target-identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(migrationData)); err != nil {
		t.Fatal(err)
	}
	identity, err := ReadTargetIdentity(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ScenarioID != "wrong-scenario" || identity.TargetFingerprint != "wrong-target" ||
		identity.WorkloadFingerprint != "sha256:wrong-workload" {
		t.Fatalf("migration overwrote existing identity: %+v", identity)
	}
}

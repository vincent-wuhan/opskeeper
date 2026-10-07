package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/demo"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func run(fingerprint string) *model.ScenarioRun {
	return &model.ScenarioRun{
		TenantID: 1, ScenarioID: "pg-pool-exhaustion", IdempotencyKey: "demo-key-0001",
		IncidentID: 10, Target: "pg:pool-fixture", TargetFingerprint: fingerprint,
		Status: model.ScenarioStatusStarting, ExpiresAt: time.Now().Add(time.Minute).UTC(),
	}
}

func TestScenarioStoreIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := NewRepo(newTestDB(t))
	first := run("0123456789abcdef")
	if err := repo.CreateOrUpdate(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	second := run("0123456789abcdef")
	second.IncidentID = 99
	second.PoolManifestID = "manifest"
	if err := repo.CreateOrUpdate(ctx, second); err != nil {
		t.Fatalf("idempotent update: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("idempotent IDs differ: %d != %d", second.ID, first.ID)
	}
	var count int64
	if err := repo.db.Model(&model.ScenarioRun{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("count = %d err = %v", count, err)
	}
}

func TestScenarioStoreUpdatesAlertAndManifest(t *testing.T) {
	ctx := context.Background()
	repo := NewRepo(newTestDB(t))
	created := run("aaaaaaaaaaaaaaaa")
	if err := repo.CreateOrUpdate(ctx, created); err != nil {
		t.Fatal(err)
	}
	created.AlertFingerprint = "bbbbbbbbbbbbbbbb"
	created.PoolManifestID = "pool-manifest-1"
	created.Status = model.ScenarioStatusAwaitingAlert
	if err := repo.CreateOrUpdate(ctx, created); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByIdempotencyKey(ctx, 1, created.ScenarioID, created.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.AlertFingerprint != "bbbbbbbbbbbbbbbb" || got.PoolManifestID != "pool-manifest-1" || got.Status != model.ScenarioStatusAwaitingAlert {
		t.Fatalf("updated run = %+v", got)
	}
}

func TestScenarioStoreRejectsFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	repo := NewRepo(newTestDB(t))
	if err := repo.CreateOrUpdate(ctx, run("cccccccccccccccc")); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateOrUpdate(ctx, run("dddddddddddddddd")); !errorsIsConflict(err) {
		t.Fatalf("mismatch err = %v", err)
	}
	if err := repo.UpdateStatus(ctx, 1, model.ScenarioStatusAwaitingAlert, func(r *model.ScenarioRun) error {
		r.TargetFingerprint = "eeeeeeeeeeeeeeee"
		return nil
	}); !errorsIsConflict(err) {
		t.Fatalf("mutation mismatch err = %v", err)
	}
}

func TestScenarioStatusAndEventUpdateIsAtomic(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := db.AutoMigrate(&alertmodel.Event{}); err != nil {
		t.Fatalf("migrate events: %v", err)
	}
	repo := NewRepo(db)
	created := run("aaaaaaaaaaaaaaaa")
	if err := repo.CreateOrUpdate(ctx, created); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(
		`CREATE TRIGGER fail_event_insert BEFORE INSERT ON alert_events ` +
			`BEGIN SELECT RAISE(ABORT, 'event insert failed'); END`,
	).Error; err != nil {
		t.Fatal(err)
	}
	event := &alertmodel.Event{
		IncidentID: created.IncidentID, EventType: model.ScenarioStatusAwaitingAlert,
		StatusAfter: "open", Severity: "critical", SnapshotJSON: "{}", Reason: "test",
	}
	if err := repo.UpdateStatusWithEvent(
		ctx, created.ID, model.ScenarioStatusAwaitingAlert, event, model.ScenarioStatusStarting,
	); err == nil {
		t.Fatal("expected event insertion failure")
	}
	got, err := repo.GetByIdempotencyKey(ctx, 1, created.ScenarioID, created.IdempotencyKey)
	if err != nil || got.Status != model.ScenarioStatusStarting {
		t.Fatalf("rolled-back run = %+v err = %v", got, err)
	}
	var count int64
	if err := db.Model(&alertmodel.Event{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("event count = %d err = %v", count, err)
	}

	if err := db.Exec(`DROP TRIGGER fail_event_insert`).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatusWithEvent(
		ctx, created.ID, model.ScenarioStatusAwaitingAlert, event, model.ScenarioStatusStarting,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&alertmodel.Event{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("successful event count = %d err = %v", count, err)
	}
}

func errorsIsConflict(err error) bool { return errors.Is(err, errs.ErrConflict) }

// openFileDB opens a file-backed SQLite database. The correlation test
// reopens the same path to prove the state machine survives a restart, so it
// cannot use the in-memory helper the rest of this file uses — with
// ":memory:" every pooled connection is a separate empty database.
func openFileDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite %s: %v", path, err)
	}
	return db
}

func migrateScenariosAndIncidents(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate scenarios: %v", err)
	}
	if err := db.AutoMigrate(&alertmodel.Incident{}); err != nil {
		t.Fatalf("migrate incidents: %v", err)
	}
}

// createIncident writes the incident a scenario pre-opened. The scenario
// correlation only reads it back, so the demo side needs no write path into
// the alert domain's own repository.
func createIncident(ctx context.Context, db *gorm.DB, incident *alertmodel.Incident) error {
	return db.WithContext(ctx).Create(incident).Error
}

func closeDB(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
}

func TestScenarioFiringCorrelationSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alert-demo-restart.db")
	db := openFileDB(t, path)
	migrateScenariosAndIncidents(t, db)
	repo := NewRepo(db)
	incident := &alertmodel.Incident{
		Title: "Final demo pool exhaustion", Rule: "PGConnectionPoolSaturation", RuleName: "PGConnectionPoolSaturation",
		Severity: "critical", Status: alertmodel.IncidentStatusOpen, Summary: "pool saturated",
		DedupeKey: "demo-scenario:restart-demo", EventCount: 1,
		FirstFiredAt: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC),
		LastFiredAt:  time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC),
		SourceType:   alertmodel.RuleSourcePrometheus,
	}
	if err := createIncident(ctx, db, incident); err != nil {
		t.Fatalf("CreateAlertIncident: %v", err)
	}
	run := &model.ScenarioRun{
		TenantID: 1, ScenarioID: "pg-pool-exhaustion", IdempotencyKey: "restart-demo",
		IncidentID: incident.ID, PoolManifestID: "manifest-restart", Target: "pg:pool-fixture",
		TargetFingerprint: "0123456789abcdef", AlertFingerprint: "cccccccccccccccc",
		Status: model.ScenarioStatusAwaitingAlert, ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}
	if err := db.Create(run).Error; err != nil {
		t.Fatalf("create scenario: %v", err)
	}
	closeDB(t, db)

	db = openFileDB(t, path)
	migrateScenariosAndIncidents(t, db)
	repo = NewRepo(db)
	got, matched, err := repo.CorrelateFiring(ctx, "cccccccccccccccc", map[string]string{
		"alertname": "PGConnectionPoolSaturation", "instance": "opskeeper-demo-node-metrics:8095",
		"job": "opsk", "pool_manifest_id": "manifest-restart",
	})
	if err != nil {
		t.Fatalf("CorrelateFiring fingerprint: %v", err)
	}
	if !matched || got == nil || got.ID != incident.ID {
		t.Fatalf("fingerprint correlation = (%p, %t); want incident %d", got, matched, incident.ID)
	}
	var persisted, labelPersisted model.ScenarioRun
	if err := db.Where("idempotency_key = ?", "restart-demo").First(&persisted).Error; err != nil {
		t.Fatalf("reload scenario by idempotency key: %v", err)
	}
	if persisted.Status != model.ScenarioStatusAlertCorrelated {
		t.Fatalf("scenario status = %q; want %q", persisted.Status, model.ScenarioStatusAlertCorrelated)
	}

	labelIncident := &alertmodel.Incident{
		Title: "Final demo pool exhaustion by labels", Rule: "PGConnectionPoolSaturation",
		RuleName: "PGConnectionPoolSaturation", Severity: "critical", Status: alertmodel.IncidentStatusOpen,
		Summary: "pool saturated", DedupeKey: "demo-scenario:label-demo", EventCount: 1,
		SourceType: alertmodel.RuleSourcePrometheus,
	}
	if err := createIncident(ctx, db, labelIncident); err != nil {
		t.Fatalf("CreateAlertIncident label fallback: %v", err)
	}
	labelRun := &model.ScenarioRun{
		TenantID: 1, ScenarioID: "pg-pool-exhaustion", IdempotencyKey: "label-demo",
		IncidentID: labelIncident.ID, PoolManifestID: "manifest-labels", Target: "pg:pool-fixture",
		TargetFingerprint: "0123456789abcdef", Status: model.ScenarioStatusAwaitingAlert,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}
	if err := db.Create(labelRun).Error; err != nil {
		t.Fatalf("create label scenario: %v", err)
	}
	got, matched, err = repo.CorrelateFiring(ctx, "dddddddddddddddd", map[string]string{
		"alertname": "PGConnectionPoolSaturation", "instance": "opskeeper-demo-node-metrics:8095",
		"job": "opsk", "pool_manifest_id": "manifest-labels",
	})
	if err != nil {
		t.Fatalf("CorrelateFiring labels: %v", err)
	}
	if !matched || got == nil || got.ID != labelIncident.ID {
		t.Fatalf("label correlation = (%p, %t); want incident %d", got, matched, labelIncident.ID)
	}
	if err := db.Where("idempotency_key = ?", "label-demo").First(&labelPersisted).Error; err != nil {
		t.Fatalf("reload label scenario by idempotency key: %v", err)
	}
	if labelPersisted.AlertFingerprint != "dddddddddddddddd" || labelPersisted.Status != model.ScenarioStatusAlertCorrelated {
		t.Fatalf("label scenario = fingerprint %q status %q; want rebound fingerprint and alert_correlated", labelPersisted.AlertFingerprint, labelPersisted.Status)
	}
}

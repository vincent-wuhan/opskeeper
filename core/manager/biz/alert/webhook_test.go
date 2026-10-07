package alert

import (
	"context"
	"errors"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

type webhookFakeInvestigator struct {
	calls []*model.Incident
}

func (f *webhookFakeInvestigator) InvestigateAsync(incident *model.Incident) {
	f.calls = append(f.calls, incident)
}

func TestIngestAlertmanagerDeduplicatesFingerprint(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nil)
	payload := AlertmanagerWebhookInput{Alerts: []AlertmanagerAlert{{
		Status:      "firing",
		Labels:      map[string]string{"alertname": "PGConnectionPoolSaturation", "severity": "critical", "host": "home-pc"},
		Annotations: map[string]string{"summary": "connection pool saturated"},
		StartsAt:    time.Now().UTC(),
		Fingerprint: "abc123",
	}}}

	first, err := uc.IngestAlertmanager(context.Background(), payload)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := uc.IngestAlertmanager(context.Background(), payload)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if first.Accepted != 1 || second.Accepted != 1 {
		t.Fatalf("accepted = first %d, second %d; want 1, 1", first.Accepted, second.Accepted)
	}
	if len(repo.incidents) != 1 {
		t.Fatalf("incidents = %d; want 1", len(repo.incidents))
	}
	var received int
	for _, event := range repo.events {
		if event.EventType == model.EventTypeAlertReceived {
			received++
		}
	}
	if received != 2 {
		t.Fatalf("alert.received events = %d; want 2", received)
	}
}

func TestAlertmanagerCorrelatesActiveDemoScenario(t *testing.T) {
	repo := newFakeRepo()
	incident := &model.Incident{
		ID: 42, Title: "Final demo pool exhaustion", Rule: "PGConnectionPoolSaturation",
		Severity: "critical", Status: model.IncidentStatusOpen, DedupeKey: "demo-scenario:final-demo",
		EventCount: 1,
	}
	repo.incidents[incident.ID] = incident
	repo.byDedupe[incident.DedupeKey] = incident
	correlator := newFakeCorrelator(repo)
	correlator.scenarios[incident.DedupeKey] = &fakeDemoScenario{
		incidentID: incident.ID, fingerprint: "aaaaaaaaaaaaaaaa", poolManifest: "manifest-1",
		status: "awaiting_alert", idempotencyKey: "final-demo",
	}
	investigator := &webhookFakeInvestigator{}
	uc := NewUsecase(repo, nil)
	uc.SetFiringCorrelator(correlator)
	uc.SetInvestigator(investigator)
	startedAt := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)

	result, err := uc.IngestAlertmanager(context.Background(), AlertmanagerWebhookInput{Alerts: []AlertmanagerAlert{{
		Status: "firing", Fingerprint: "aaaaaaaaaaaaaaaa", StartsAt: startedAt,
		Labels: map[string]string{
			"alertname": "PGConnectionPoolSaturation", "instance": "opskeeper-demo-node-metrics:8095",
			"job": "opsk", "pool_manifest_id": "manifest-1",
		},
	}}})
	if err != nil {
		t.Fatalf("IngestAlertmanager: %v", err)
	}
	if result.Accepted != 1 {
		t.Fatalf("accepted = %d; want 1", result.Accepted)
	}
	if len(repo.incidents) != 1 || repo.incidents[42].ID != incident.ID {
		t.Fatalf("incident identity changed: %+v", repo.incidents)
	}
	if correlator.scenarios[incident.DedupeKey].status != "alert_correlated" {
		t.Fatalf("scenario status = %q; want alert_correlated", correlator.scenarios[incident.DedupeKey].status)
	}
	if !hasEventType(repo.events, model.EventTypeAlertReceived) {
		t.Fatalf("alert.received event missing: %+v", repo.events)
	}
	if len(investigator.calls) != 1 || investigator.calls[0].ID != incident.ID {
		t.Fatalf("investigator calls = %+v; want incident 42 once", investigator.calls)
	}
}

func TestAlertmanagerDoesNotCreateDuplicateDemoIncident(t *testing.T) {
	repo := newFakeRepo()
	incident := &model.Incident{
		ID: 77, Rule: "PGConnectionPoolSaturation", Severity: "critical",
		Status: model.IncidentStatusOpen, DedupeKey: "demo-scenario:repeat-demo", EventCount: 1,
	}
	repo.incidents[incident.ID] = incident
	repo.byDedupe[incident.DedupeKey] = incident
	correlator := newFakeCorrelator(repo)
	correlator.scenarios[incident.DedupeKey] = &fakeDemoScenario{
		incidentID: incident.ID, fingerprint: "bbbbbbbbbbbbbbbb", poolManifest: "manifest-2",
		status: "awaiting_alert", idempotencyKey: "repeat-demo",
	}
	uc := NewUsecase(repo, nil)
	uc.SetFiringCorrelator(correlator)
	payload := AlertmanagerWebhookInput{Alerts: []AlertmanagerAlert{{
		Status: "firing", Fingerprint: "bbbbbbbbbbbbbbbb", StartsAt: time.Now().UTC(),
		Labels: map[string]string{
			"alertname": "PGConnectionPoolSaturation", "instance": "opskeeper-demo-node-metrics:8095",
			"job": "opsk", "pool_manifest_id": "manifest-2",
		},
	}}}

	for i := 0; i < 2; i++ {
		if _, err := uc.IngestAlertmanager(context.Background(), payload); err != nil {
			t.Fatalf("ingest %d: %v", i+1, err)
		}
	}
	if len(repo.incidents) != 1 {
		t.Fatalf("incidents = %d; want 1", len(repo.incidents))
	}
	if incident.ID != 77 || incident.EventCount != 3 {
		t.Fatalf("incident after repeats = id %d events %d; want id 77 events 3", incident.ID, incident.EventCount)
	}
}

func TestUnrelatedAlertKeepsNormalIngestPath(t *testing.T) {
	repo := newFakeRepo()
	correlator := newFakeCorrelator(repo)
	uc := NewUsecase(repo, nil)
	uc.SetFiringCorrelator(correlator)

	_, err := uc.IngestAlertmanager(context.Background(), AlertmanagerWebhookInput{Alerts: []AlertmanagerAlert{{
		Status: "firing", Fingerprint: "unrelated", StartsAt: time.Now().UTC(),
		Labels: map[string]string{"alertname": "UnrelatedAlert"},
	}}})
	if err != nil {
		t.Fatalf("IngestAlertmanager: %v", err)
	}
	if correlator.calls != 1 {
		t.Fatalf("correlate calls = %d; want 1", correlator.calls)
	}
	if len(repo.incidents) != 1 || repo.incidents[1].DedupeKey != "alertmanager:unrelated" {
		t.Fatalf("normal incident path changed: %+v", repo.incidents)
	}
}

// TestIngestAlertmanagerWithoutCorrelatorIngestsNormally pins the "not wired"
// answer: before decision 113 the recognition logic was compiled into the
// alert store, so a platform with no demo still ran that query on every
// firing. Now the query only happens if somebody answered a claim, and
// "nobody wired a correlator" has to behave exactly like "nothing matched".
func TestIngestAlertmanagerWithoutCorrelatorIngestsNormally(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nil)

	result, err := uc.IngestAlertmanager(context.Background(), AlertmanagerWebhookInput{Alerts: []AlertmanagerAlert{{
		Status: "firing", Fingerprint: "standalone", StartsAt: time.Now().UTC(),
		Labels: map[string]string{
			"alertname": "PGConnectionPoolSaturation", "instance": "opskeeper-demo-node-metrics:8095",
			"job": "opsk", "pool_manifest_id": "manifest-nobody-owns",
		},
	}}})
	if err != nil {
		t.Fatalf("IngestAlertmanager: %v", err)
	}
	if result.Accepted != 1 || len(repo.incidents) != 1 {
		t.Fatalf("accepted = %d, incidents = %d; want 1, 1", result.Accepted, len(repo.incidents))
	}
	// Even the demo's exact label triple must not be claimed by anyone.
	if got := repo.incidents[1].DedupeKey; got != "alertmanager:standalone" {
		t.Fatalf("dedupe key = %q; want the ordinary alertmanager path", got)
	}
}

// TestCorrelatorErrorStopsIngest makes sure the port is not swallowed: a
// correlator that cannot answer must not let the firing through as if nobody
// had claimed it, because the pre-opened incident is the dedupe target.
func TestCorrelatorErrorStopsIngest(t *testing.T) {
	repo := newFakeRepo()
	correlator := newFakeCorrelator(repo)
	correlator.fail = errors.New("scenario store unreachable")
	uc := NewUsecase(repo, nil)
	uc.SetFiringCorrelator(correlator)

	_, err := uc.IngestAlertmanager(context.Background(), AlertmanagerWebhookInput{Alerts: []AlertmanagerAlert{{
		Status: "firing", Fingerprint: "boom", StartsAt: time.Now().UTC(),
		Labels: map[string]string{"alertname": "PGConnectionPoolSaturation", "severity": "critical"},
	}}})
	if err == nil {
		t.Fatal("IngestAlertmanager err = nil; want the correlator's error")
	}
	if len(repo.incidents) != 0 {
		t.Fatalf("incidents = %d; want 0 — a firing must not be ingested past a failed correlation", len(repo.incidents))
	}
}

func TestIngestAlertmanagerRejectsMissingAlertname(t *testing.T) {
	uc := NewUsecase(newFakeRepo(), nil)
	_, err := uc.IngestAlertmanager(context.Background(), AlertmanagerWebhookInput{Alerts: []AlertmanagerAlert{{
		Status: "firing",
	}}})
	if !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("error = %v; want invalid", err)
	}
}

func TestIngestAlertmanagerRejectsBatchBeforePartialIngest(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nil)
	_, err := uc.IngestAlertmanager(context.Background(), AlertmanagerWebhookInput{Alerts: []AlertmanagerAlert{
		{
			Status:      "firing",
			Labels:      map[string]string{"alertname": "ValidAlert"},
			Fingerprint: "valid",
		},
		{
			Status: "firing",
		},
	}})
	if !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("error = %v; want invalid", err)
	}
	if len(repo.incidents) != 0 {
		t.Fatalf("incidents = %d; want 0", len(repo.incidents))
	}
}

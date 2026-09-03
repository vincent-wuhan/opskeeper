package alert

import (
	"context"
	"errors"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

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

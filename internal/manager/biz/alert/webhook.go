package alert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

func (u *Usecase) IngestAlertmanager(ctx context.Context, in AlertmanagerWebhookInput) (*AlertmanagerIngestResult, error) {
	if u == nil || u.repo == nil {
		return nil, errs.ErrNotWiredYet
	}
	if in.Alerts == nil {
		return nil, fmt.Errorf("%w: alerts required", errs.ErrInvalid)
	}

	for _, alert := range in.Alerts {
		if !strings.EqualFold(strings.TrimSpace(alert.Status), "firing") {
			continue
		}
		name := strings.TrimSpace(alert.Labels["alertname"])
		if name == "" {
			return nil, fmt.Errorf("%w: alertname required", errs.ErrInvalid)
		}
		if len(name) > 128 {
			return nil, fmt.Errorf("%w: alertname exceeds 128 characters", errs.ErrInvalid)
		}
	}

	result := &AlertmanagerIngestResult{}
	for _, alert := range in.Alerts {
		if !strings.EqualFold(strings.TrimSpace(alert.Status), "firing") {
			result.Ignored++
			continue
		}

		name := strings.TrimSpace(alert.Labels["alertname"])
		if name == "" {
			return nil, fmt.Errorf("%w: alertname required", errs.ErrInvalid)
		}
		if len(name) > 128 {
			return nil, fmt.Errorf("%w: alertname exceeds 128 characters", errs.ErrInvalid)
		}

		severity := normalizeWebhookSeverity(alert.Labels["severity"])
		summary := strings.TrimSpace(alert.Annotations["summary"])
		if summary == "" {
			summary = strings.TrimSpace(alert.Annotations["description"])
		}
		if summary == "" {
			summary = fmt.Sprintf("Alertmanager alert %s is firing", name)
		}
		firing, err := u.RecordFiring(ctx, FiringInput{
			ScopeType:   model.RuleScopeMonitoringPipeline,
			Scope:       "alertmanager",
			Rule:        name,
			RuleName:    name,
			Severity:    severity,
			OccurredAt:  alert.StartsAt,
			DedupeKey:   alertmanagerDedupeKey(name, alert.Fingerprint, alert.Labels),
			SourceType:  model.RuleSourcePrometheus,
			Title:       name,
			Summary:     summary,
			Description: strings.TrimSpace(alert.Annotations["description"]),
			Labels:      alert.Labels,
			Annotations: alert.Annotations,
			RunbookURL:  strings.TrimSpace(alert.Annotations["runbook_url"]),
		})
		if err != nil {
			return nil, err
		}
		if err := u.recordWebhookReceipt(ctx, firing.Incident, alert); err != nil {
			return nil, err
		}
		result.Accepted++
	}
	return result, nil
}

func (u *Usecase) recordWebhookReceipt(ctx context.Context, incident *model.Incident, alert AlertmanagerAlert) error {
	snapshot, err := json.Marshal(map[string]any{
		"source":      "alertmanager",
		"status":      alert.Status,
		"fingerprint": alert.Fingerprint,
		"labels":      alert.Labels,
	})
	if err != nil {
		snapshot = []byte("{}")
	}
	return u.createEvent(ctx, &model.Event{
		IncidentID:   incident.ID,
		EventType:    model.EventTypeAlertReceived,
		StatusAfter:  incident.Status,
		Severity:     incident.Severity,
		Title:        incident.Title,
		ActorType:    model.ActorTypeSystem,
		SnapshotJSON: string(snapshot),
		Reason:       "Alertmanager webhook received",
		OccurredAt:   incident.LastFiredAt,
	}, incident.Rule)
}

func normalizeWebhookSeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "error":
		return "critical"
	case "info", "informational":
		return "info"
	case "debug":
		return "debug"
	default:
		return "warning"
	}
}

func alertmanagerDedupeKey(alertName, fingerprint string, labels map[string]string) string {
	if strings.TrimSpace(fingerprint) != "" {
		return "alertmanager:" + strings.TrimSpace(fingerprint)
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString(alertName)
	for _, key := range keys {
		builder.WriteByte(0)
		builder.WriteString(key)
		builder.WriteByte(0)
		builder.WriteString(labels[key])
	}
	digest := sha256.Sum256([]byte(builder.String()))
	return "alertmanager:" + alertName + ":" + hex.EncodeToString(digest[:16])
}

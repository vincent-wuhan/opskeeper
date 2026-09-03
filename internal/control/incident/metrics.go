// Package incident derives judge-facing incident metrics from an append-only
// control timeline. It deliberately does not trust a final status string:
// closure is valid only when a new recovery signal has already been observed.
package incident

import (
	"errors"
	"sort"
	"time"
)

const (
	EventAlertReceived = "alert.received"
	EventRootCause     = "root_cause.confirmed"
	EventApproved      = "recommendation.approved"
	EventAction        = "action.executed"
	EventRecovery      = "recovery_signal.observed"
	EventClosed        = "incident.closed"
	EventReopened      = "incident.reopened"
)

var auditRequiredEvents = map[string]bool{
	EventAlertReceived: true,
	EventRootCause:     true,
	EventApproved:      true,
	EventAction:        true,
	EventRecovery:      true,
	EventClosed:        true,
}

type Event struct {
	ID                string    `json:"id"`
	TenantID          string    `json:"tenant_id"`
	IncidentID        string    `json:"incident_id"`
	OccurredAt        time.Time `json:"occurred_at"`
	Phase             string    `json:"phase"`
	EventType         string    `json:"event_type"`
	ActorType         string    `json:"actor_type"`
	Actor             string    `json:"actor"`
	Status            string    `json:"status"`
	ActionFingerprint string    `json:"action_fingerprint,omitempty"`
	EvidenceRef       string    `json:"evidence_ref,omitempty"`
	TraceID           string    `json:"trace_id,omitempty"`
	RecoverySignal    bool      `json:"recovery_signal,omitempty"`
}

func (event Event) Validate() error {
	switch {
	case event.ID == "":
		return errors.New("incident: event id is required")
	case event.TenantID == "", event.IncidentID == "":
		return errors.New("incident: tenant and incident ids are required")
	case event.OccurredAt.IsZero():
		return errors.New("incident: occurred_at is required")
	case event.EventType == "":
		return errors.New("incident: event_type is required")
	case event.Phase == "":
		return errors.New("incident: event phase is required")
	case event.ActorType == "", event.Actor == "":
		return errors.New("incident: actor is required")
	case event.Status == "":
		return errors.New("incident: status is required")
	case event.EventType == EventRecovery && !event.RecoverySignal:
		return errors.New("incident: recovery event requires recovery_signal=true")
	case event.EventType != EventRecovery && event.RecoverySignal:
		return errors.New("incident: only recovery events may set recovery_signal=true")
	}
	if event.ActorType != "agent" && event.ActorType != "human" && event.ActorType != "system" {
		return errors.New("incident: unsupported actor_type")
	}
	return nil
}

type Report struct {
	IncidentCount                        int     `json:"incident_count"`
	MeanLocalizationSeconds              float64 `json:"mean_localization_seconds"`
	WrongClosureCount                    int     `json:"wrong_closure_count"`
	RepeatedActionCount                  int     `json:"repeated_action_count"`
	ApprovedRecommendationCount          int     `json:"approved_recommendation_count"`
	RecoveryConfirmedRecommendationCount int     `json:"recovery_confirmed_recommendation_count"`
	RecommendationSuccessRate            float64 `json:"recommendation_success_rate"`
	AuditRequiredEventCount              int     `json:"audit_required_event_count"`
	CompleteAuditEventCount              int     `json:"complete_audit_event_count"`
	AuditEvidenceCompleteness            float64 `json:"audit_evidence_completeness"`
}

func ComputeReport(events []Event) (Report, error) {
	byIncident := make(map[string][]Event)
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return Report{}, err
		}
		key := event.TenantID + "/" + event.IncidentID
		byIncident[key] = append(byIncident[key], event)
	}

	report := Report{IncidentCount: len(byIncident)}
	localizationTotal := 0.0
	localizationCount := 0
	for _, incidentEvents := range byIncident {
		sort.Slice(incidentEvents, func(left, right int) bool {
			return incidentEvents[left].OccurredAt.Before(incidentEvents[right].OccurredAt)
		})
		localizationTotal += localizationSeconds(incidentEvents)
		if firstEvent(incidentEvents, EventAlertReceived) != nil && firstEvent(incidentEvents, EventRootCause) != nil {
			localizationCount++
		}
		report.WrongClosureCount += wrongClosures(incidentEvents)
		report.RepeatedActionCount += repeatedActions(incidentEvents)
		report.ApprovedRecommendationCount += countEvents(incidentEvents, EventApproved)
		report.RecoveryConfirmedRecommendationCount += approvedWithRecovery(incidentEvents)
		report.AuditRequiredEventCount += auditEvents(incidentEvents)
		report.CompleteAuditEventCount += completeAuditEvents(incidentEvents)
	}
	if localizationCount > 0 {
		report.MeanLocalizationSeconds = localizationTotal / float64(localizationCount)
	}
	if report.ApprovedRecommendationCount > 0 {
		report.RecommendationSuccessRate = float64(report.RecoveryConfirmedRecommendationCount) / float64(report.ApprovedRecommendationCount)
	}
	if report.AuditRequiredEventCount > 0 {
		report.AuditEvidenceCompleteness = float64(report.CompleteAuditEventCount) / float64(report.AuditRequiredEventCount)
	}
	return report, nil
}

func localizationSeconds(events []Event) float64 {
	alert := firstEvent(events, EventAlertReceived)
	rootCause := firstEvent(events, EventRootCause)
	if alert == nil || rootCause == nil || rootCause.OccurredAt.Before(alert.OccurredAt) {
		return 0
	}
	return rootCause.OccurredAt.Sub(alert.OccurredAt).Seconds()
}

func wrongClosures(events []Event) int {
	count := 0
	reopened := false
	for _, event := range events {
		if event.EventType == EventReopened {
			reopened = true
		}
	}
	for _, event := range events {
		if event.EventType != EventClosed {
			continue
		}
		recovery := firstEventBefore(events, EventRecovery, event.OccurredAt)
		if recovery == nil || !recovery.RecoverySignal || reopened {
			count++
		}
	}
	return count
}

func repeatedActions(events []Event) int {
	seen := make(map[string]int)
	for _, event := range events {
		if event.EventType != EventAction || event.ActionFingerprint == "" {
			continue
		}
		seen[event.ActionFingerprint]++
	}
	count := 0
	for _, occurrence := range seen {
		if occurrence > 1 {
			count += occurrence - 1
		}
	}
	return count
}

func approvedWithRecovery(events []Event) int {
	count := 0
	for _, event := range events {
		if event.EventType != EventApproved {
			continue
		}
		if recovery := firstEventAfter(events, EventRecovery, event.OccurredAt); recovery != nil && recovery.RecoverySignal {
			count++
		}
	}
	return count
}

func auditEvents(events []Event) int {
	count := 0
	for _, event := range events {
		if auditRequiredEvents[event.EventType] {
			count++
		}
	}
	return count
}

func completeAuditEvents(events []Event) int {
	count := 0
	for _, event := range events {
		if auditRequiredEvents[event.EventType] && event.TraceID != "" && event.EvidenceRef != "" {
			count++
		}
	}
	return count
}

func firstEvent(events []Event, eventType string) *Event {
	for index := range events {
		if events[index].EventType == eventType {
			return &events[index]
		}
	}
	return nil
}

func firstEventBefore(events []Event, eventType string, before time.Time) *Event {
	for index := range events {
		if events[index].EventType == eventType && events[index].OccurredAt.Before(before) {
			return &events[index]
		}
	}
	return nil
}

func firstEventAfter(events []Event, eventType string, after time.Time) *Event {
	for index := range events {
		if events[index].EventType == eventType && events[index].OccurredAt.After(after) {
			return &events[index]
		}
	}
	return nil
}

func countEvents(events []Event, eventType string) int {
	count := 0
	for _, event := range events {
		if event.EventType == eventType {
			count++
		}
	}
	return count
}

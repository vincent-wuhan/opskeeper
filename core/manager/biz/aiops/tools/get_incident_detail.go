package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
)

// executeGetIncidentDetail stayed behind for the same reason as
// executeQueryIncidents: it is the node-side upcall entry point, and a
// Registry method cannot be lifted off its receiver. The batched BaseTool
// that shares its name lives in the alerting cluster.
func (r *Registry) executeGetIncidentDetail(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	if r.alertUC == nil {
		return ExecuteResult{}, fmt.Errorf("get_incident_detail: alert usecase not configured")
	}
	var in alerting.GetIncidentDetailArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ExecuteResult{}, fmt.Errorf("get_incident_detail: bad args: %w", err)
	}
	if in.IncidentID == 0 {
		return ExecuteResult{}, fmt.Errorf("get_incident_detail: incident_id required")
	}

	callCtx, cancel := context.WithTimeout(ctx, alerting.IncidentDetailCallTimeout)
	defer cancel()

	inc, err := r.alertUC.GetIncident(callCtx, in.IncidentID)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("get_incident_detail: get: %w", err)
	}
	events, err := r.alertUC.ListEvents(callCtx, in.IncidentID, 200)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("get_incident_detail: events: %w", err)
	}

	timeline := make([]alerting.IncidentEventRow, 0, len(events))
	for _, ev := range events {
		timeline = append(timeline, alerting.IncidentEventRow{
			ID:          ev.ID,
			EventType:   ev.EventType,
			StatusAfter: ev.StatusAfter,
			Severity:    ev.Severity,
			Title:       ev.Title,
			Message:     ev.Message,
			ActorType:   ev.ActorType,
			ActorID:     ev.ActorID,
			Reason:      ev.Reason,
			OccurredAt:  ev.OccurredAt,
		})
	}

	out := map[string]any{
		"incident": map[string]any{
			"id":               inc.ID,
			"rule":             inc.Rule,
			"rule_name":        inc.RuleName,
			"title":            inc.Title,
			"severity":         inc.Severity,
			"status":           inc.Status,
			"scope_type":       inc.ScopeType,
			"device_id":        inc.DeviceID,
			"summary":          inc.Summary,
			"description":      inc.Description,
			"value":            inc.Value,
			"threshold":        inc.Threshold,
			"event_count":      inc.EventCount,
			"first_fired_at":   inc.FirstFiredAt,
			"last_fired_at":    inc.LastFiredAt,
			"last_notified_at": inc.LastNotifiedAt,
			"silenced_until":   inc.SilencedUntil,
			"acknowledged_at":  inc.AcknowledgedAt,
			"resolved_at":      inc.ResolvedAt,
			"runbook_url":      inc.RunbookURL,
		},
		"timeline": timeline,
	}
	body, err := json.Marshal(out)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("get_incident_detail: marshal: %w", err)
	}
	var edgeID *uint64
	if inc.DeviceID != nil {
		eid := *inc.DeviceID
		edgeID = &eid
	}
	return ExecuteResult{ResultJSON: body, DeviceID: edgeID}, nil
}

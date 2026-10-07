package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	alertbiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/alert"
	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// executeQueryIncidents stayed behind while the tool itself moved to the
// alerting cluster. A method cannot be lifted off its receiver, and this
// one is the node-side entry point: the pig agent running on an edge
// reaches query_incidents as an upcall through Registry
// (agentToolUpcall.RunAgentTool), not as a BaseTool. Both paths now share
// one wire identity and one timeout from alerting/identity.go, so they
// cannot drift apart — which is the only reason leaving it is tolerable.
func (r *Registry) executeQueryIncidents(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	if r.alertUC == nil {
		return ExecuteResult{}, fmt.Errorf("query_incidents: alert usecase not configured")
	}
	var in alerting.QueryIncidentsArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ExecuteResult{}, fmt.Errorf("query_incidents: bad args: %w", err)
	}
	if in.Limit <= 0 {
		in.Limit = 50
	}
	if in.Limit > 500 {
		in.Limit = 500
	}
	if in.SinceMinutes <= 0 {
		in.SinceMinutes = 24 * 60
	}
	cutoff := time.Now().UTC().Add(-time.Duration(in.SinceMinutes) * time.Minute)

	if in.Severity != "" {
		switch in.Severity {
		case "info", "warning", "critical":
		default:
			return ExecuteResult{}, fmt.Errorf("query_incidents: invalid severity %q", in.Severity)
		}
	}
	if in.Status != "" {
		switch in.Status {
		case alertmodel.IncidentStatusOpen, alertmodel.IncidentStatusAcknowledged,
			alertmodel.IncidentStatusSilenced, alertmodel.IncidentStatusResolved:
		default:
			return ExecuteResult{}, fmt.Errorf("query_incidents: invalid status %q", in.Status)
		}
	}

	f := alertbiz.IncidentFilter{
		Status:   in.Status,
		Severity: in.Severity,
		RuleKey:  in.RuleKey,
		// Pull a generous window because IncidentFilter doesn't support
		// since_minutes natively; we filter in memory.
		Limit: in.Limit * 4,
	}
	devID := in.DeviceID
	if devID == 0 {
		devID = in.EdgeID
	}
	if devID > 0 {
		f.DeviceID = &devID
	}

	callCtx, cancel := context.WithTimeout(ctx, alerting.QueryIncidentsCallTimeout)
	defer cancel()
	all, err := r.alertUC.ListIncidents(callCtx, f)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("query_incidents: list: %w", err)
	}

	rows := make([]alerting.IncidentRow, 0, len(all))
	for _, inc := range all {
		if inc.LastFiredAt.Before(cutoff) {
			continue
		}
		rows = append(rows, alerting.IncidentRow{
			ID:             inc.ID,
			Title:          inc.Title,
			Severity:       inc.Severity,
			Status:         inc.Status,
			Rule:           inc.Rule,
			RuleName:       inc.RuleName,
			DeviceID:       inc.DeviceID,
			ScopeType:      inc.ScopeType,
			FirstFiredAt:   inc.FirstFiredAt,
			LastFiredAt:    inc.LastFiredAt,
			EventCount:     inc.EventCount,
			AcknowledgedAt: inc.AcknowledgedAt,
			ResolvedAt:     inc.ResolvedAt,
		})
		if len(rows) >= in.Limit {
			break
		}
	}

	out, err := json.Marshal(map[string]any{
		"incidents": rows,
		"count":     len(rows),
	})
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("query_incidents: marshal: %w", err)
	}
	return ExecuteResult{ResultJSON: out}, nil
}

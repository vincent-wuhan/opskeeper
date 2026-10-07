package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// executeQueryAlertRules stayed behind for the same reason as
// executeQueryIncidents: node-side upcall entry point, welded to Registry.
func (r *Registry) executeQueryAlertRules(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	if r.alertUC == nil {
		return ExecuteResult{}, fmt.Errorf("query_alert_rules: alert usecase not configured")
	}
	var in alerting.QueryAlertRulesArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ExecuteResult{}, fmt.Errorf("query_alert_rules: bad args: %w", err)
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if in.Limit > 500 {
		in.Limit = 500
	}
	if in.Kind != "" && !alertmodel.IsKnownKind(in.Kind) {
		return ExecuteResult{}, fmt.Errorf("query_alert_rules: invalid kind %q", in.Kind)
	}

	callCtx, cancel := context.WithTimeout(ctx, alerting.QueryAlertRulesCallTimeout)
	defer cancel()
	// ListRules's only filter is scope_type, which we don't use here.
	all, err := r.alertUC.ListRules(callCtx, "")
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("query_alert_rules: list: %w", err)
	}

	rows := make([]alerting.AlertRuleRow, 0, len(all))
	wantKind := alertmodel.NormalizeKind(in.Kind)
	for _, rule := range all {
		if in.Kind != "" && alertmodel.NormalizeKind(rule.Kind) != wantKind {
			continue
		}
		if in.Enabled != nil && rule.Enabled != *in.Enabled {
			continue
		}
		if in.NameContains != "" {
			if !strings.Contains(rule.Name, in.NameContains) && !strings.Contains(rule.RuleKey, in.NameContains) {
				continue
			}
		}
		rows = append(rows, alerting.AlertRuleRow{
			ID:         rule.ID,
			RuleKey:    rule.RuleKey,
			Kind:       rule.Kind,
			Name:       rule.Name,
			ScopeType:  rule.ScopeType,
			Severity:   rule.Severity,
			Enabled:    rule.Enabled,
			SourceType: rule.SourceType,
			UpdatedAt:  rule.UpdatedAt,
		})
		if len(rows) >= in.Limit {
			break
		}
	}

	out, err := json.Marshal(map[string]any{
		"rules": rows,
		"count": len(rows),
	})
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("query_alert_rules: marshal: %w", err)
	}
	return ExecuteResult{ResultJSON: out}, nil
}

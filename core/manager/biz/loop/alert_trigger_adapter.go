// Package loop — alert_trigger_adapter.go
//
// The production AutonomyTriggerSource: it reads the detection signal an
// incident fired on out of the alert rule that fired it.
//
// Why the rule and not the metric. A crystallised action carries both halves
// of a runbook — what to run and when to run it — and the "when" is the
// comparison an operator wrote into a rule. The same metric name under a
// different threshold is a different trigger, and a node that evaluated the
// wrong one would fire a self-heal earlier or later than the operator
// agreed to. So this adapter reads the stored comparison verbatim; it does
// not recompute a threshold from an incident's last observed value, and it
// returns false when the rule is absent rather than inventing a comparison.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// AlertTriggerAdapter turns an incident's firing rule into an
// AutonomyTrigger.
//
// The metric and threshold come from the rule's conditions_json, which for a
// metric rule is a single comparison. A rule with zero or several conditions
// is declined: a runbook binds to one signal, and picking the first of
// several would be choosing a trigger the operator did not.
type AlertTriggerAdapter struct {
	alerts AlertReader
	log    *slog.Logger
}

// NewAlertTriggerAdapter constructs the adapter. repo is required.
func NewAlertTriggerAdapter(alerts AlertReader, log *slog.Logger) *AlertTriggerAdapter {
	if alerts == nil {
		panic("loop: NewAlertTriggerAdapter: alerts is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &AlertTriggerAdapter{alerts: alerts, log: log.With(slog.String("comp", "loop.alert_trigger_adapter"))}
}

var _ AutonomyTriggerSource = (*AlertTriggerAdapter)(nil)

// AutonomyTriggerFor reads the incident and its rule, and returns the one
// comparison the rule declares.
func (a *AlertTriggerAdapter) AutonomyTriggerFor(ctx context.Context, _ string, incidentID string) (domain.AutonomyTrigger, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(incidentID), 10, 64)
	if err != nil || id == 0 {
		// The loop's incident id is not always the alert incident's numeric
		// id (harness cases and chat-promoted runs carry their own). A
		// non-numeric id simply has no rule, which is not an error.
		return domain.AutonomyTrigger{}, false
	}
	incident, err := a.alerts.GetIncidentByID(ctx, id)
	if err != nil || incident == nil {
		if err != nil && !errors.Is(err, errNotFound) {
			a.log.Warn("alert_trigger_adapter: load incident failed (non-fatal)",
				slog.String("incident_id", incidentID), slog.Any("err", err))
		}
		return domain.AutonomyTrigger{}, false
	}
	if incident.RuleID == nil || *incident.RuleID == 0 {
		return domain.AutonomyTrigger{}, false
	}
	rule, err := a.alerts.GetRuleByID(ctx, *incident.RuleID)
	if err != nil || rule == nil {
		if err != nil {
			a.log.Warn("alert_trigger_adapter: load rule failed (non-fatal)",
				slog.Uint64("rule_id", *incident.RuleID), slog.Any("err", err))
		}
		return domain.AutonomyTrigger{}, false
	}
	return triggerFromRule(rule)
}

// errNotFound is a local sentinel so a lookup miss does not log as a failure.
// The alert repository does not export one, and "row absent" and "db down"
// produce the same (nil, nil) or (nil, err) from its two shapes; this keeps
// the adapter from warning on the ordinary miss.
var errNotFound = errors.New("not found")

// triggerFromRule reads one metric comparison out of a rule's conditions.
func triggerFromRule(rule *AlertRule) (domain.AutonomyTrigger, bool) {
	if rule == nil || strings.TrimSpace(rule.ConditionsJSON) == "" {
		return domain.AutonomyTrigger{}, false
	}
	var conds []ruleCondition
	if err := json.Unmarshal([]byte(rule.ConditionsJSON), &conds); err != nil {
		return domain.AutonomyTrigger{}, false
	}
	if len(conds) != 1 {
		// One runbook, one signal. A rule that ANDs three comparisons is not
		// a trigger this declaration can carry, and choosing one of them
		// would fire on a condition narrower than what was reviewed.
		return domain.AutonomyTrigger{}, false
	}
	c := conds[0]
	if c.Metric == "" {
		return domain.AutonomyTrigger{}, false
	}
	// Only "above"-style comparisons become a metric_above trigger. The
	// node evaluator implements exactly that comparison, and a "<" rule
	// flipped into ">" would fire at the opposite moment.
	if !isAboveOperator(c.Operator) {
		return domain.AutonomyTrigger{}, false
	}
	return domain.AutonomyTrigger{
		Kind:      domain.TriggerMetricAbove,
		Metric:    c.Metric,
		Threshold: c.Threshold,
	}, true
}

func isAboveOperator(op string) bool {
	switch strings.TrimSpace(op) {
	case ">", ">=", "gt", "gte":
		return true
	default:
		return false
	}
}

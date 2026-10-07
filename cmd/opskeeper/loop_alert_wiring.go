package main

import (
	"context"

	alertbiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/alert"
	managerbizloop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// loopAlertReader is the one place that knows both the alert domain and
// the loop's port over it (decision 279). The loop's three adapters —
// subject labels, autonomy trigger, detection events — are constructed
// here and nowhere else, so this is a single conversion rather than three.
//
// Every projection is field by field and every one is pinned by
// loop_alert_wiring_test.go. The two that carry real weight:
//
//   - incidentFrom is the single place that decides "absent stays absent",
//     and both readers go through it. An earlier version of this file also
//     checked `inc == nil` in each method, and a mutation showed why that
//     second check was not doing anything: with either one in place the
//     other is unreachable, and a test that pins the behaviour therefore
//     pins the pair rather than either. One place is easier to read and
//     easier to delete.
//   - ListIncidents keeps its slice the same length as the repository's,
//     nils included, because the loop's own adapter has always skipped
//     them. That is incidentFrom's nil check doing the work, not a branch
//     here — the first version of this file had an explicit `if inc == nil`
//     and a comment crediting it, which was false.
type loopAlertReader struct {
	repo alertbiz.Repo
}

var _ managerbizloop.AlertReader = loopAlertReader{}

func (r loopAlertReader) GetIncidentByID(ctx context.Context, id uint64) (*managerbizloop.AlertIncident, error) {
	inc, err := r.repo.GetIncidentByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return incidentFrom(inc), nil
}

func (r loopAlertReader) ListIncidents(ctx context.Context, f managerbizloop.AlertIncidentFilter) ([]*managerbizloop.AlertIncident, error) {
	incidents, err := r.repo.ListIncidents(ctx, alertbiz.IncidentFilter{
		RuleKey: f.RuleKey,
		Limit:   f.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*managerbizloop.AlertIncident, 0, len(incidents))
	for _, inc := range incidents {
		out = append(out, incidentFrom(inc))
	}
	return out, nil
}

func (r loopAlertReader) GetRuleByID(ctx context.Context, id uint64) (*managerbizloop.AlertRule, error) {
	rule, err := r.repo.GetRuleByID(ctx, id)
	if err != nil || rule == nil {
		return nil, err
	}
	return &managerbizloop.AlertRule{ConditionsJSON: rule.ConditionsJSON}, nil
}

// incidentFrom projects one row, and returns nil for a row that is not
// there. That nil is the whole answer to "does this incident exist", and
// the loop adapters ask the question on paths where getting it wrong is
// quiet: a zero-valued row reads as an incident that exists with no rule,
// no labels and no severity.
//
// RuleID is a pointer on both sides and is
// copied as the same pointer rather than as its target: the trigger
// adapter distinguishes "fired from a static rule" (nil) from "fired from
// a rule row" (non-nil), and copying the value would erase that
// distinction for the zero id.
func incidentFrom(inc *alertmodel.Incident) *managerbizloop.AlertIncident {
	if inc == nil {
		return nil
	}
	return &managerbizloop.AlertIncident{
		ID:           inc.ID,
		Severity:     inc.Severity,
		Scope:        inc.Scope,
		Rule:         inc.Rule,
		RuleID:       inc.RuleID,
		FirstFiredAt: inc.FirstFiredAt,
		UpdatedAt:    inc.UpdatedAt,
		LabelsJSON:   inc.LabelsJSON,
	}
}

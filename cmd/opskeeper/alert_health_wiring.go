package main

// Alert-health adapter wiring.
//
// The system-health probe asks the alert service two questions — how many
// rules exist and how many of them are enabled, and how many incidents are
// still open — and it used to get there by holding alert's own Caller, Rule
// and IncidentFilter in its port signatures. That is three types the probe
// does not own crossing a domain boundary, and the largest of them was a
// nineteen-column model from which the probe read exactly one column.
//
// The seam stayed, the shapes did not, and the translation belongs here rather
// than inside either domain. Putting it in the system-health package would
// have left that package importing alert's service to satisfy its own port —
// the same package-form dependency the port was declared to remove, wearing a
// different name. Putting it in alert would have made the alert service know
// that a health check exists. The wiring root is the one place that is
// already allowed to know about both, and cmd/opskeeper/loop_adapters.go and
// deploymentHealthAdapter are the precedents.
//
// The caller is passed as alert's zero value on both calls, and that is not a
// placeholder: alert's own ListRules and CountIncidents declare it as `_` and
// never read it. Constructing a real identity here would suggest the probe's
// answer depends on who asked, which it does not — a health report is the
// same report for everyone, and the role that reaches it is checked in the
// route handler before this code runs.

import (
	"context"
	"errors"

	managersvcalert "github.com/vincent-wuhan/opskeeper/core/manager/service/alert"
	healthsvc "github.com/vincent-wuhan/opskeeper/core/manager/service/systemhealth"
)

// errAlertServiceNotWired is what the adapter reports when it is handed a nil
// service. The system-health service already refuses to run its alert probe
// when the dependency is a nil interface, but a struct holding a nil pointer
// is not a nil interface, so the check that would have caught this does not
// fire and the first symptom would be a panic inside a liveness endpoint.
var errAlertServiceNotWired = errors.New("alert service is not wired")

// alertHealthAdapter presents the alert service through the two questions the
// system-health probe actually asks.
type alertHealthAdapter struct {
	svc *managersvcalert.Service
}

var (
	_ interface {
		CountRules(ctx context.Context) (total, enabled int, err error)
	} = alertHealthAdapter{}
	_ interface {
		CountOpenIncidents(ctx context.Context) (int64, error)
	} = alertHealthAdapter{}
)

// newAlertHealthProbe is the constructor the boot path calls.
//
// It exists so the wiring is a name a test can hold rather than a struct
// literal in a 6700-line main: a decision earlier in this file's history
// found a dependency injected through a setter that six tests called and
// main.go forgot, so the production route was permanently 503 while every
// test stayed green. Making it a function that returns the two ports means
// "the health probe is wired" is a claim this package can check, and
// returning nil for a nil service keeps the one honest failure visible.
func newAlertHealthProbe(svc *managersvcalert.Service) (healthsvc.RuleLister, healthsvc.IncidentCounter) {
	adapter := alertHealthAdapter{svc: svc}
	return adapter, adapter
}

// CountRules answers the probe's first question.
//
// The enabled count is computed here rather than asked for, because alert's
// ListRules has no such method and adding one would be a second question
// asked of a store that can already answer the first. The nil check is
// load-bearing: ListRules returns []*Rule, and a nil element is a legal value
// in that slice, so a plain r.Enabled would panic on a store bug rather than
// report a lower number.
func (a alertHealthAdapter) CountRules(ctx context.Context) (total, enabled int, err error) {
	if a.svc == nil {
		return 0, 0, errAlertServiceNotWired
	}
	rules, err := a.svc.ListRules(ctx, managersvcalert.Caller{}, "")
	if err != nil {
		return 0, 0, err
	}
	total, enabled = countEnabled(rules)
	return total, enabled, nil
}

// countEnabled is the whole of the first question, pulled out so it can be
// tested without a store behind it.
//
// Two things it has to get right, and one of them is not obvious. A nil row
// is a legal element of the slice ListRules returns, so a plain r.Enabled
// would turn a store bug into a panic on a liveness endpoint — the worst
// place for one. And a disabled rule still counts toward the total: the
// health report distinguishes "no rules" from "rules but none enabled", and
// folding the two together would make those two states read the same.
func countEnabled(rules []*managersvcalert.Rule) (total, enabled int) {
	for _, r := range rules {
		if r == nil {
			continue
		}
		total++
		if r.Enabled {
			enabled++
		}
	}
	return total, enabled
}

// CountOpenIncidents answers the probe's second question. The status is a
// literal here because the probe asked for open incidents and nothing else;
// see the port's comment in the system-health service.
func (a alertHealthAdapter) CountOpenIncidents(ctx context.Context) (int64, error) {
	if a.svc == nil {
		return 0, errAlertServiceNotWired
	}
	return a.svc.CountIncidents(ctx, managersvcalert.Caller{}, managersvcalert.IncidentFilter{Status: "open"})
}

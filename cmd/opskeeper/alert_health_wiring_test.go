package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	managersvcalert "github.com/vincent-wuhan/opskeeper/core/manager/service/alert"
	healthsvc "github.com/vincent-wuhan/opskeeper/core/manager/service/systemhealth"
)

// This file is the boot-path guard for the `systemhealth -> alert` cut.
//
// The cut lives in four files and none of them can see the wiring: the
// system-health ports were restated as two questions, the service stopped
// naming alert's Caller / Rule / IncidentFilter, the handler stopped
// forwarding an identity, and the domaincheck table lost a row. What decided
// it was in main.go and in a new adapter, and what those changed was
// invisible from both sides of the boundary.
//
// The measurement that justified it, and it is the same shape decision 251
// found on `webshell -> edge`: the probe held alert's nineteen-column Rule
// and read exactly one column of it, Enabled, plus len() on the slice. The
// other eighteen are what an operator reads on a page this service never
// renders, and Rule.Conditions drags in RuleCondition, a shape the pricing
// tool itself flags as unsettled in two other domains. The caller was
// weaker still: both alert methods declare it as `_`, and the probe read
// none of its fields, so the parameter existed to keep an import alive.
//
// So the boundary was package-shaped wearing an interface's clothes. The port
// is now stated in the probe's own words and the alert service no longer
// satisfies it.

// TestTheAlertServiceNoLongerSatisfiesTheProbePorts is the negative half,
// and it is a runtime type assertion rather than a `var _` line on purpose.
//
// A `var _ = (*Service)(nil)` style negative assertion fails the build, and
// "the compiler rejects it" is weaker evidence than it sounds: it proves the
// shapes differ today and says nothing about whether anyone can widen the
// port back. This one goes red in the world that matters — the port widened,
// the edge quietly restored, `go build ./...` perfectly happy.
func TestTheAlertServiceNoLongerSatisfiesTheProbePorts(t *testing.T) {
	t.Parallel()
	var svc any = (*managersvcalert.Service)(nil)
	if _, ok := svc.(healthsvc.RuleLister); ok {
		t.Error("*alert.Service satisfies RuleLister again; the probe port was widened back " +
			"toward the alert domain and the edge is returning without anybody deciding to")
	}
	if _, ok := svc.(healthsvc.IncidentCounter); ok {
		t.Error("*alert.Service satisfies IncidentCounter again; the probe port was widened " +
			"back toward the alert domain")
	}
}

// The other half of the same worry. A port nobody satisfies fails loudly; a
// port satisfied by something the boot path forgot to pass fails quietly, and
// the symptom is a health page that says "alert service is not fully wired"
// forever while every test passes. An earlier decision in this repository
// found exactly that shape — a setter six tests called and main.go forgot —
// so the wiring is a constructor this package can hold.
func TestTheProbeIsWiredThroughANamedConstructor(t *testing.T) {
	t.Parallel()
	rules, incidents := newAlertHealthProbe(managersvcalert.New(nil, nil, nil, nil))
	if rules == nil {
		t.Error("RuleLister is nil; the health probe would report 'not fully wired' in every deployment")
	}
	if incidents == nil {
		t.Error("IncidentCounter is nil; same")
	}
	if _, ok := rules.(alertHealthAdapter); !ok {
		t.Errorf("RuleLister is %T, want the adapter in this file", rules)
	}
	if _, ok := incidents.(alertHealthAdapter); !ok {
		t.Errorf("IncidentCounter is %T, want the adapter in this file", incidents)
	}
}

// An adapter holding a nil service is not a nil interface, so the
// system-health service's own nil check cannot see it. The first symptom
// would otherwise be a panic inside a liveness endpoint.
func TestANilAlertServiceIsAnErrorAndNotAPanic(t *testing.T) {
	t.Parallel()
	a := alertHealthAdapter{}
	if _, _, err := a.CountRules(t.Context()); !errors.Is(err, errAlertServiceNotWired) {
		t.Errorf("CountRules error = %v, want errAlertServiceNotWired", err)
	}
	if _, err := a.CountOpenIncidents(t.Context()); !errors.Is(err, errAlertServiceNotWired) {
		t.Errorf("CountOpenIncidents error = %v, want errAlertServiceNotWired", err)
	}
}

// A real service with no usecase behind it must report the failure, not
// count zero rules — "the alert store is unreachable" and "there are no
// rules" are different answers and a liveness page that conflates them is
// worse than one that is down.
func TestAnUnwiredAlertServiceReportsRatherThanCountsZero(t *testing.T) {
	t.Parallel()
	a := alertHealthAdapter{svc: managersvcalert.New(nil, nil, nil, nil)}
	if _, _, err := a.CountRules(t.Context()); err == nil {
		t.Error("CountRules reported no error against an alert service with no usecase; " +
			"the health page would claim there are zero rules")
	}
	if _, err := a.CountOpenIncidents(t.Context()); err == nil {
		t.Error("CountOpenIncidents reported no error against an unwired alert service")
	}
}

func TestCountingCountsDisabledRulesAndSkipsNilRows(t *testing.T) {
	t.Parallel()
	total, enabled := countEnabled([]*managersvcalert.Rule{
		{Enabled: true},
		{Enabled: false},
		nil,
		{Enabled: true},
	})
	if total != 3 {
		t.Errorf("total = %d, want 3; a nil row must be skipped, not counted, and a disabled "+
			"rule must still be a rule", total)
	}
	if enabled != 2 {
		t.Errorf("enabled = %d, want 2", enabled)
	}
}

// The boot path itself. Everything above tests the adapter in isolation;
// this is the only assertion that main.go passes it in, and a constructor
// that exists and is never called satisfies every other test in this file.
func TestTheBootPathCallsTheConstructor(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "newAlertHealthProbe(alertSvc)") {
		t.Error("main.go no longer calls newAlertHealthProbe; the health probe is being handed " +
			"something else, and every test in this file still passes")
	}
}

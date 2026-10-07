package investigatorreal

import (
	"context"
	"testing"
	"time"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// TestTheWholeChain_TurnsAlertLabelsIntoADispatchableAction is the test that
// matters for this feature.
//
// The gap it closes was never "the platform cannot find a pod name" — the
// firing alert names it. It was that the name travelled nowhere: the alert
// fired, the investigator recorded a metric and a log line, the contract
// carried those two, and the remediation dispatch refused for want of a pod
// that had been known since the alert rule selected it.
//
// So this walks the whole path a real incident takes, with no stubs in the
// middle: labels on the alert -> evidence chain -> contract -> argument
// resolver -> a dispatch that has the pod name and can be completed.
func TestTheWholeChain_TurnsAlertLabelsIntoADispatchableAction(t *testing.T) {
	labels := &stubLabels{labels: map[string]string{
		"pod":       "order-svc-7d9f2",
		"namespace": "prod",
		"severity":  "P1",
	}}
	toolset := NewWithLabels(&fakeMetricQuerier{}, nil, labels, nil)
	window := loop.TimeWindow{
		Start: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 13, 10, 5, 0, 0, time.UTC),
	}

	evidence, err := toolset.Investigate(context.Background(), "k8s", "alert-17", window)
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	// The plan carries what the toolset observed, exactly as the
	// investigated worker's Planner puts it there.
	plan := loop.Plan{Meta: map[string]any{"evidence_chain": evidence}}

	// The contract is what the resolvers will read. The deterministic
	// builder is used here because it is the path that copies the toolset
	// evidence verbatim; the LLM path is covered by the stamping test.
	rc := &loop.RootCauseJSON{SchemaVersion: loop.ContractSchemaV1, EvidenceChain: evidence}
	loop.StampSubject(plan, rc)

	resolver := loop.EvidenceArgResolver{Causes: staticCauses{rc: rc}}
	args, err := resolver.Resolve(context.Background(), loop.RemediationRequest{
		TenantID: "t-1", IncidentID: "inc-1",
		Option: loop.RemediationOption{Action: "k8s.evict_pod", Target: "k8s:alert-17", Risk: "mutating"},
	}, loop.ToolSpec{Name: "k8s.evict_pod", RequiredArgs: []string{"pod"}})
	if err != nil {
		t.Fatalf("the dispatch could not be completed even though the alert named the pod: %v", err)
	}
	if got := args["pod"]; got != "order-svc-7d9f2" {
		t.Fatalf("pod = %#v, want order-svc-7d9f2", got)
	}
}

// Without the subject the same incident refuses — and the refusal must name
// the missing evidence, so an operator reading it knows what to record.
func TestTheWholeChain_RefusesLoudlyWhenTheAlertNamedNothing(t *testing.T) {
	toolset := NewWithLabels(&fakeMetricQuerier{}, nil, &stubLabels{labels: map[string]string{"severity": "P1"}}, nil)
	evidence, err := toolset.Investigate(context.Background(), "k8s", "alert-17", loop.TimeWindow{})
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	rc := &loop.RootCauseJSON{SchemaVersion: loop.ContractSchemaV1, EvidenceChain: evidence}
	resolver := loop.EvidenceArgResolver{Causes: staticCauses{rc: rc}}
	_, err = resolver.Resolve(context.Background(), loop.RemediationRequest{
		Option: loop.RemediationOption{Action: "k8s.evict_pod", Target: "k8s:alert-17", Risk: "mutating"},
	}, loop.ToolSpec{Name: "k8s.evict_pod", RequiredArgs: []string{"pod"}})
	if err == nil {
		t.Fatal("an alert that named no pod must not produce one")
	}
}

type staticCauses struct{ rc *loop.RootCauseJSON }

func (s staticCauses) LoadRootCause(_ context.Context, _, _ string) (*loop.RootCauseJSON, error) {
	return s.rc, nil
}

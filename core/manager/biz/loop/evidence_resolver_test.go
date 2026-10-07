package loop

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedRootCauseLoader serves a fixed RootCauseJSON, and can report that
// the run never reached the investigated phase.
type scriptedRootCauseLoader struct {
	rc  *RootCauseJSON
	err error
}

func (s scriptedRootCauseLoader) LoadRootCause(_ context.Context, _, _ string) (*RootCauseJSON, error) {
	return s.rc, s.err
}

func causeWithEvidence(items ...EvidenceItem) *RootCauseJSON {
	return &RootCauseJSON{SchemaVersion: "v1", EvidenceChain: items}
}

// activityEvidence is the shape the real PostgreSQL investigator produces:
// rows of pg_stat_activity, including the pid and the role.
func activityEvidence(rows ...map[string]any) EvidenceItem {
	return EvidenceItem{
		Tool:  "pg_stat_activity",
		Query: "state=idle in transaction AND age(xact_start)>60s",
		Value: rows,
		Count: len(rows),
	}
}

func TestEvidenceArgResolver_ResolvesTheSingleRecordedPID(t *testing.T) {
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{
		rc: causeWithEvidence(activityEvidence(map[string]any{
			"pid": int64(4211), "usename": "reporting_ro", "application_name": "etl", "state": "idle in transaction",
		})),
	}}
	args, err := r.Resolve(context.Background(), RemediationRequest{
		IncidentID: "inc-1", TenantID: "t-1",
		Option: opt("pg.kill_session", "mutating", false),
	}, ToolSpec{Name: "pg.kill_session", RequiredArgs: []string{"pid"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, ok := args["pid"].(int64); !ok || got != 4211 {
		t.Fatalf("pid = %#v, want 4211", args["pid"])
	}
}

// The evidence can arrive as []any after a JSON round trip through the
// contract table, and a resolver that only understood the typed slice would
// fail on exactly the production path.
func TestEvidenceArgResolver_ReadsRowsAfterAJSONRoundTrip(t *testing.T) {
	item := activityEvidence(map[string]any{"pid": float64(77), "usename": "app"})
	item.Value = []any{item.Value.([]map[string]any)[0]}

	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{rc: causeWithEvidence(item)}}
	args, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("pg.kill_session", "mutating", false),
	}, ToolSpec{Name: "pg.kill_session", RequiredArgs: []string{"pid"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, ok := args["pid"].(int64); !ok || got != 77 {
		t.Fatalf("pid = %#v, want 77", args["pid"])
	}
}

func TestEvidenceArgResolver_RefusesToChooseBetweenSessions(t *testing.T) {
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{
		rc: causeWithEvidence(activityEvidence(
			map[string]any{"pid": int64(11), "usename": "etl"},
			map[string]any{"pid": int64(22), "usename": "etl"},
		)),
	}}
	_, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("pg.kill_session", "mutating", false),
	}, ToolSpec{Name: "pg.kill_session", RequiredArgs: []string{"pid"}})
	if err == nil {
		t.Fatal("two candidate sessions must not resolve to one pid")
	}
	for _, want := range []string{"pid 11", "pid 22"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name both candidates; %q does not mention %q", err.Error(), want)
		}
	}
}

func TestEvidenceArgResolver_RefusesWhenThereIsNoActivityEvidence(t *testing.T) {
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{
		rc: causeWithEvidence(EvidenceItem{Tool: "query_promql", Value: `[{"value":"3"}]`}),
	}}
	_, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("pg.kill_session", "mutating", false),
	}, ToolSpec{Name: "pg.kill_session", RequiredArgs: []string{"pid"}})
	if err == nil {
		t.Fatal("a metric-only evidence chain cannot name a session")
	}
	if !strings.Contains(err.Error(), "no pg_stat_activity evidence") {
		t.Errorf("the refusal must say what was missing: %q", err.Error())
	}
}

func TestEvidenceArgResolver_RefusesWhenTheRunNeverInvestigated(t *testing.T) {
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{rc: nil}}
	_, err := r.Resolve(context.Background(), RemediationRequest{
		IncidentID: "inc-9",
		Option:     opt("pg.kill_session", "mutating", false),
	}, ToolSpec{Name: "pg.kill_session", RequiredArgs: []string{"pid"}})
	if err == nil {
		t.Fatal("a run with no recorded root cause has no evidence to resolve from")
	}
	if !strings.Contains(err.Error(), "inc-9") {
		t.Errorf("the refusal must name the incident: %q", err.Error())
	}
}

func TestEvidenceArgResolver_ResolvesTheRoleFromTheSessions(t *testing.T) {
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{
		rc: causeWithEvidence(activityEvidence(
			map[string]any{"pid": int64(1), "usename": "reporting_ro"},
			map[string]any{"pid": int64(2), "usename": "reporting_ro"},
		)),
	}}
	args, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("pg.connection_pause", "mutating", false),
	}, ToolSpec{Name: "pg.connection_pause", RequiredArgs: []string{"role"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, _ := args["role"].(string); got != "reporting_ro" {
		t.Fatalf("role = %#v, want reporting_ro", args["role"])
	}
}

func TestEvidenceArgResolver_RefusesToPauseEveryRoleItSaw(t *testing.T) {
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{
		rc: causeWithEvidence(activityEvidence(
			map[string]any{"pid": int64(1), "usename": "etl"},
			map[string]any{"pid": int64(2), "usename": "reporting_ro"},
		)),
	}}
	_, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("pg.connection_pause", "mutating", false),
	}, ToolSpec{Name: "pg.connection_pause", RequiredArgs: []string{"role"}})
	if err == nil {
		t.Fatal("two roles must not resolve to one")
	}
	for _, want := range []string{"etl", "reporting_ro"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name both roles; %q does not mention %q", err.Error(), want)
		}
	}
}

// An action with no declared extractor is left to the invoker's own
// missing-argument refusal. Returning an error here would be a resolver
// claiming authority over actions it knows nothing about.
func TestEvidenceArgResolver_LeavesUndeclaredActionsToTheInvoker(t *testing.T) {
	r := EvidenceArgResolver{}
	// These actions have no declared extractor, so the invoker's own
	// missing-argument refusal stays in charge and speaks for them.
	for _, action := range []string{"pg.vacuum_analyze", "pg.terminate_long_tx", "mq.inspect_consumer_lag", "host.garbage_collect"} {
		args, err := r.Resolve(context.Background(), RemediationRequest{
			Option: opt(action, "mutating", false),
		}, ToolSpec{Name: action, RequiredArgs: []string{"pid"}})
		if err != nil {
			t.Errorf("%s: an undeclared action must not fail in the resolver: %v", action, err)
		}
		if args != nil {
			t.Errorf("%s: resolver produced %v for an action it does not declare", action, args)
		}
	}
}

// A tool that needs nothing is not asked for anything, even when an
// extractor exists for its name.
func TestEvidenceArgResolver_SkipsToolsThatNeedNoArguments(t *testing.T) {
	r := EvidenceArgResolver{} // no loader at all: nothing should be read
	args, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("pg.kill_session", "mutating", false),
	}, ToolSpec{Name: "pg.kill_session"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if args != nil {
		t.Fatalf("args = %v, want none for a tool with no required arguments", args)
	}
}

func TestEvidenceArgResolver_RefusesWithoutALoader(t *testing.T) {
	r := EvidenceArgResolver{}
	_, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("pg.kill_session", "mutating", false),
	}, ToolSpec{Name: "pg.kill_session", RequiredArgs: []string{"pid"}})
	if err == nil {
		t.Fatal("a declared action with no loader wired must refuse rather than dispatch")
	}
}

// The end-to-end shape: the invoker reaches the tool with the pid the
// evidence named, and nothing else.
func TestRegistryInvoker_ResolvesThePIDFromEvidenceAndDispatches(t *testing.T) {
	tools := callerWith(ToolSpec{Name: "pg.kill_session", RequiredArgs: []string{"pid"}})
	inv := RegistryInvoker{
		Tools: tools,
		ArgResolver: EvidenceArgResolver{Causes: scriptedRootCauseLoader{
			rc: causeWithEvidence(activityEvidence(map[string]any{"pid": int64(9001), "usename": "etl"})),
		}},
	}
	out, err := inv.Invoke(context.Background(), RemediationRequest{
		IncidentID: "inc-3", TenantID: "t-1",
		Option:   opt("pg.kill_session", "mutating", false),
		Approver: "alice",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Status != RemediationStatusSuccess {
		t.Fatalf("Status = %q, want success (message %q)", out.Status, out.Message)
	}
	if got, ok := tools.args["pid"].(int64); !ok || got != 9001 {
		t.Errorf("pid dispatched = %#v, want 9001", tools.args["pid"])
	}
	if got, _ := tools.args["approved_by"].(string); got != "alice" {
		t.Errorf("approved_by = %q, want alice", got)
	}
}

func TestRegistryInvoker_EvidenceAmbiguityIsARefusalNotACall(t *testing.T) {
	tools := callerWith(ToolSpec{Name: "pg.kill_session", RequiredArgs: []string{"pid"}})
	inv := RegistryInvoker{
		Tools: tools,
		ArgResolver: EvidenceArgResolver{Causes: scriptedRootCauseLoader{
			rc: causeWithEvidence(activityEvidence(
				map[string]any{"pid": int64(1), "usename": "etl"},
				map[string]any{"pid": int64(2), "usename": "etl"},
			)),
		}},
	}
	_, err := inv.Invoke(context.Background(), RemediationRequest{
		Option: opt("pg.kill_session", "mutating", false),
	})
	if !errors.Is(err, ErrUnresolvableArguments) {
		t.Fatalf("err = %v, want ErrUnresolvableArguments", err)
	}
	if tools.callCount != 0 {
		t.Errorf("an ambiguous session must not reach the tool, got %v", tools.calls)
	}
}

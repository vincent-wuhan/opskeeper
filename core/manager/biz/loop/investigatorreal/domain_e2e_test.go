package investigatorreal

import (
	"context"
	"testing"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// TestTheDomainPath_TurnsAnObservationIntoACompletedDispatch walks the whole
// path a bloated table takes, with no stub in the middle.
//
// The gap it closes is the one that held the remediation axis at 3/20. The
// investigator could see that a database was in trouble; it could not see
// that one table in it was 61% dead tuples, so no VACUUM was ever proposed,
// so the axis never scored. The fix is not a threshold — it is asking, and
// then carrying the answer all the way to the tool.
func TestTheDomainPath_TurnsAnObservationIntoACompletedDispatch(t *testing.T) {
	tools := &stubTools{
		specs: map[string]loop.ToolSpec{
			"pg.table_bloat":   readOnly("pg.table_bloat"),
			"pg.vacuum_status": diagnostic("pg.vacuum_status"),
			"pg.slow_log":      readOnly("pg.slow_log"),
		},
		results: map[string]any{
			"pg.table_bloat": rowsOf(map[string]any{
				"schemaname": "public", "table": "order_events", "dead_pct": 61.0,
			}),
			"pg.vacuum_status": rowsOf(),
			"pg.slow_log":      rowsOf(),
		},
	}
	toolset := New(&fakeMetricQuerier{}, nil, nil).WithProbes(tools)

	evidence, err := toolset.Investigate(context.Background(), "pg", "alert-17", loop.TimeWindow{})
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	// The plan carries what the toolset observed, as the investigated
	// worker's Planner puts it there.
	plan := loop.Plan{Meta: map[string]any{"evidence_chain": evidence}}

	options, err := toolset.ListRemediationsWithEvidence(context.Background(), "pg", "alert-17", evidence)
	if err != nil {
		t.Fatalf("ListRemediationsWithEvidence: %v", err)
	}
	var chosen *loop.RemediationOption
	for i := range options {
		if options[i].Action == "pg.vacuum_table" {
			chosen = &options[i]
		}
	}
	if chosen == nil {
		t.Fatalf("the catalog reported a 61%% dead table and no VACUUM was proposed: %#v", options)
	}

	// The contract the resolvers will read.
	rc := &loop.RootCauseJSON{
		SchemaVersion:      loop.ContractSchemaV1,
		EvidenceChain:      evidence,
		RemediationOptions: options,
	}
	loop.StampSubject(plan, rc)

	resolver := loop.EvidenceArgResolver{Causes: staticCauses{rc: rc}}
	args, err := resolver.Resolve(context.Background(), loop.RemediationRequest{
		TenantID: "t-1", IncidentID: "inc-1", Option: *chosen,
	}, loop.ToolSpec{Name: "pg.vacuum_table", RequiredArgs: []string{"table"}})
	if err != nil {
		t.Fatalf("the proposal could not be dispatched even though the catalog named the table: %v", err)
	}
	if got := args["table"]; got != "order_events" {
		t.Fatalf("table = %#v, want order_events", got)
	}
}

// The inverse must hold as firmly: a healthy table produces no proposal, so
// there is nothing to dispatch and no chance of one.
func TestTheDomainPath_AHealthyTableProducesNothingToDispatch(t *testing.T) {
	tools := &stubTools{
		specs: map[string]loop.ToolSpec{
			"pg.table_bloat":   readOnly("pg.table_bloat"),
			"pg.vacuum_status": diagnostic("pg.vacuum_status"),
			"pg.slow_log":      readOnly("pg.slow_log"),
		},
		results: map[string]any{
			"pg.table_bloat":   rowsOf(map[string]any{"table": "orders", "dead_pct": 1.0}),
			"pg.vacuum_status": rowsOf(),
			"pg.slow_log":      rowsOf(),
		},
	}
	toolset := New(&fakeMetricQuerier{}, nil, nil).WithProbes(tools)
	evidence, err := toolset.Investigate(context.Background(), "pg", "alert-17", loop.TimeWindow{})
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	options, err := toolset.ListRemediationsWithEvidence(context.Background(), "pg", "alert-17", evidence)
	if err != nil {
		t.Fatalf("ListRemediationsWithEvidence: %v", err)
	}
	for _, option := range options {
		if option.Action == "pg.vacuum_table" {
			t.Fatal("a 1% dead table must not produce a VACUUM proposal")
		}
	}
}

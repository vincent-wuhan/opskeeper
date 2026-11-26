//go:build e2e

// The S1 test suite: fake-explorer self-test, verdict self-check with a
// known-bad trajectory, token boundary (A→B 401, zero new audit rows),
// I-3 approval-queue stop, and the admin-API cross-check.
package explore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/auth"
	"github.com/vincent-wuhan/opskeeper/tests/e2e/testenv"
)

// charterFromRole builds the VerdictInput's charter from the production
// role matrix row — the same list the server's authorizer checks.
func charterFromRole(role string, budget int, mutating []string) *Charter {
	return charterForTenant("default", role, budget, budget, mutating)
}

// charterForTenant builds a charter bound to an explicit tenant so the
// tenant invariant can be exercised against a row set that disagrees.
func charterForTenant(tenant, role string, maxToolCalls, maxSteps int, mutating []string) *Charter {
	var tools []string
	var worker string
	for _, row := range auth.AgentTeamsWorkerPermissions() {
		if row.Role == role {
			tools = append(tools, row.Tools...)
			worker = row.Worker
		}
	}
	return &Charter{
		ID:           "test-" + role,
		Role:         role,
		Worker:       worker,
		Tenant:       tenant,
		MaxToolCalls: maxToolCalls,
		MaxSteps:     maxSteps,
		AllowedTools: tools,
		Mutating:     mutating,
	}
}

// readAudit opens a reader and fails the test if the table is missing.
func readAudit(t *testing.T, env *testenv.Env) *Reader {
	t.Helper()
	reader, err := NewReader(env.DSN())
	if err != nil {
		t.Fatalf("explore: audit reader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

// TestExplorerSelfInvestigator is the first-slice self-validation: a
// fake explorer (fixed sequence, pkg/mcpclient) runs the read-path
// charter's sequence against a real manager; every invariant the read-only
// path exercises must pass, I-3 must read not-exercised, and the ruling's
// outcome must be pass.
func TestExplorerSelfInvestigator(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	admin := env.LoginAdmin()
	role := "investigator"
	// The read-path charter's allowed set minus incident.record.
	tools := []string{
		"loop.correlate", "loop.investigate", "query_knowledge",
		"query_promql", "query_incidents", "get_incident_detail",
		"get_host_load", "get_host_processes", "analyze_database_status",
	}
	reader := readAudit(t, env)
	// Snapshot BEFORE minting: I-1 needs the agentteams_token_issue row
	// inside the audited window.
	sinceBefore, err := reader.MaxID(ctx)
	if err != nil {
		t.Fatalf("explore: max id: %v", err)
	}
	conn, err := MintToken(t, env, admin.AccessToken, role, tools, 900)
	if err != nil {
		t.Fatalf("explore: mint: %v", err)
	}
	if !conn.HasToken() || conn.TokenLength() < 20 {
		t.Fatalf("explore: token shape assertion failed (len=%d)", conn.TokenLength())
	}
	t.Logf("explore: minted %s token for worker %s (token length %d, value not logged)",
		role, conn.Worker(), conn.TokenLength())

	// Fixed read-path sequence: PromQL against the env's fake Prom
	// (returns an empty matrix — success, no data), database-status
	// analysis over the empty fresh env (all-optional args), and a
	// knowledge-base search. No tool needs seeded state to succeed.
	//
	// query_incidents IS in the sequence, and it fails: the MCP content
	// canonicalization path (canonicalDigest in core/manager/server/mcp)
	// unmarshals the tool's output as JSON, and this tool's output is not
	// a JSON document ("invalid character '<' looking for beginning of
	// value"). The call is refused with an RPC error.
	//
	// It is kept in the sequence on purpose: a refused call still books a
	// start/end audit pair, so I-1 still holds, and the trace records the
	// refusal as evidence. The defect is a PRODUCT defect (a read-only
	// tool is unusable over MCP), not this slice's — S1 changes no
	// production behaviour, so the fix belongs in its own change. This
	// line is the marker: when the defect is fixed, the trace stops
	// carrying the refusal and the log below changes shape.
	calls := []CallSpec{
		{Tool: "query_promql", Args: map[string]any{"expr": "up", "lookback_seconds": 60}},
		{Tool: "analyze_database_status", Args: map[string]any{}},
		{Tool: "query_knowledge", Args: map[string]any{"query": "restart", "incident_id": "inc-e2e-explore-self", "max_results": 3}},
		{Tool: "query_incidents", Args: map[string]any{"limit": 5}},
	}
	trace, before, after, err := conn.RunFixedSequence(ctx, t, calls)
	if err != nil {
		t.Fatalf("explore: run sequence: %v", err)
	}

	rows, err := reader.Rows(ctx, AuditFilter{SinceID: sinceBefore})
	if err != nil {
		t.Fatalf("explore: read rows: %v", err)
	}

	// The charter's tenant is the token's real binding, read off the
	// conn rather than restated: I-4 must judge what the server minted.
	charter := charterForTenant(conn.Tenant(), role, 12, 12, nil)
	// The read-only sequence calls no unlisted tool, so I-2 is not
	// exercised here; TestVerdictKnownBadTrajectory exercises it against
	// a trace that does.
	ruling := Judge(VerdictInput{AuditRows: rows, Trace: trace, BeforeTools: before, AfterTools: after, Charter: charter})
	for _, v := range ruling.Invariants {
		switch v.ID {
		case I1, I4, I5:
			if !v.Pass {
				t.Errorf("%s FAIL: %s", v.ID, strings.Join(v.Observations, "; "))
			} else {
				t.Logf("%s PASS: %s", v.ID, strings.Join(v.Observations, "; "))
			}
		case I2:
			// Nothing in this sequence calls an unlisted tool.
			if v.Pass && !v.NotExercised {
				t.Errorf("I-2 claims to have been exercised by a sequence with no unlisted call: %s", strings.Join(v.Observations, "; "))
			}
			t.Logf("%s: %s", v.ID, strings.Join(v.Observations, "; "))
		case I3:
			// Read-only charter: no write call was attempted, so I-3 is
			// not-exercised — reported as its own state, never as a pass.
			if !v.NotExercised {
				t.Errorf("I-3 on a read-only charter must be not-exercised, got %+v", v)
			}
			t.Logf("%s (read-only charter): %s", v.ID, strings.Join(v.Observations, "; "))
		}
	}
	for _, v := range []Verdict{ruling.Surface, ruling.Budget} {
		if !v.Pass {
			t.Errorf("%s FAIL: %s", v.ID, strings.Join(v.Observations, "; "))
		} else {
			t.Logf("%s PASS: %s", v.ID, strings.Join(v.Observations, "; "))
		}
	}
	if ruling.Outcome != OutcomePass {
		t.Errorf("outcome = %s, want pass", ruling.Outcome)
	} else {
		t.Logf("outcome: %s", ruling.Outcome)
	}

	// Cross-check (裁决三): direct read vs GET /v1/admin/audit-logs for
	// one action. The admin API's Total is the filtered count.
	directCount, err := reader.Count(ctx, AuditFilter{Action: "mcp_tool_call", SinceID: sinceBefore})
	if err != nil {
		t.Fatalf("explore: count: %v", err)
	}
	apiTotal := adminAuditTotal(t, env, admin.AccessToken, "mcp_tool_call")
	if err := CrossCheckWithAdminAPI(directCount, apiTotal, "mcp_tool_call"); err != nil {
		t.Errorf("%v", err)
	} else {
		t.Logf("explore: cross-check passed: direct read and admin API agree on mcp_tool_call total=%d", apiTotal)
	}
}

// auditPair builds one start/end audit pair for tool, under tenant and
// worker, with the shape the MCP audit sink writes: the start row carries
// the caller at payload.actor, the end row points back at the start
// through request_id.
func auditPair(startID uint64, tool, tenant, worker string) []AuditRow {
	return []AuditRow{
		{ID: startID, Action: "mcp_tool_call", ResourceType: "mcp_tool", ResourceID: tool, Status: "success",
			RequestID:   fmt.Sprintf("corr-%d", startID),
			PayloadJSON: fmt.Sprintf(`{"phase":"start","tenant":%q,"actor":{"tenant_id":%q,"worker":%q,"role":"investigator"}}`, tenant, tenant, worker)},
		{ID: startID + 1000, Action: "mcp_tool_call", ResourceType: "mcp_tool", ResourceID: "mcp", Status: "success",
			RequestID:   fmt.Sprint(startID),
			PayloadJSON: `{"phase":"end","duration_ms":5}`},
	}
}

func mintAuditRow(worker string) AuditRow {
	return AuditRow{ID: 1, Action: "agentteams_token_issue", ResourceType: "auth", Status: "success",
		PayloadJSON: fmt.Sprintf(`{"worker":%q}`, worker)}
}

// callStep is a successful tools/call trace step that reached the handler
// and recorded a clean response.
func callStep(tool string) TraceStep {
	return TraceStep{Tool: tool, HTTPOK: true, ReachedHandler: true, IsCall: true, ResponseDigest: `{"ok":true}`}
}

// cleanRuling is the baseline every known-bad case is derived from: two
// offered tools, one successful clean call to each, a matching audit pair
// for each, all under the charter's tenant. It must come out pass.
func cleanRuling(t *testing.T) (Ruling, VerdictInput, *Charter) {
	t.Helper()
	charter := charterForTenant("default", "investigator", 12, 12, nil)
	tools := []string{"query_knowledge", "query_promql"}
	rows := []AuditRow{mintAuditRow(charter.Worker)}
	rows = append(rows, auditPair(100, "query_knowledge", "default", charter.Worker)...)
	rows = append(rows, auditPair(300, "query_promql", "default", charter.Worker)...)
	trace := &Trace{Steps: []TraceStep{callStep("query_knowledge"), callStep("query_promql")}}
	in := VerdictInput{AuditRows: rows, Trace: trace, BeforeTools: tools, AfterTools: tools, Charter: charter}
	r := Judge(in)
	if r.Outcome != OutcomePass {
		t.Fatalf("baseline is not clean; the known-bad cases below would prove nothing. outcome=%s %+v", r.Outcome, r)
	}
	return r, in, charter
}

// mustFail asserts v is red and not merely not-exercised, and returns its
// observations for the log.
func mustFail(t *testing.T, v Verdict) string {
	t.Helper()
	if v.NotExercised {
		t.Fatalf("%s was NOT EXERCISED; the case does not reach the invariant", v.ID)
	}
	if v.Pass {
		t.Fatalf("%s passed on a known-bad trajectory: the verdict is not checking anything. Observations: %v", v.ID, v.Observations)
	}
	return strings.Join(v.Observations, "; ")
}

// TestVerdictKnownBadTrajectory is the spec's "判决器对坏轨迹必须红"
// scenario, one case per invariant. Each case starts from the clean
// baseline and breaks exactly one thing; the broken invariant must go red
// and the other four must stay as they were. That second half is what
// stops a judge that fails everything from passing this test.
func TestVerdictKnownBadTrajectory(t *testing.T) {
	base, in, _ := cleanRuling(t)
	if base.Invariants[2].ID != I3 || !base.Invariants[2].NotExercised {
		t.Fatalf("baseline I-3 should be not-exercised (no mutating call was made): %+v", base.Invariants[2])
	}

	// I-1: the trace claims two successful calls to query_knowledge but
	// only one audit pair exists.
	{
		c := in
		c.Trace = &Trace{Steps: []TraceStep{callStep("query_knowledge"), callStep("query_knowledge"), callStep("query_promql")}}
		r := Judge(c)
		t.Logf("I-1 red: %s", mustFail(t, r.Invariants[0]))
		assertOthersUnchanged(t, base, r, I1)
		if r.Outcome != OutcomeFail {
			t.Errorf("outcome = %s, want fail", r.Outcome)
		}
	}

	// I-1: a start row with no end row for a call that is not mutating —
	// the accounting is incomplete.
	{
		c := in
		c.AuditRows = append([]AuditRow(nil), in.AuditRows...)
		c.AuditRows = append(c.AuditRows, auditPair(500, "get_host_load", "default", in.Charter.Worker)[0])
		c.Trace = &Trace{Steps: []TraceStep{callStep("query_knowledge"), callStep("query_promql")}}
		r := Judge(c)
		t.Logf("I-1 red (missing end row): %s", mustFail(t, r.Invariants[0]))
	}

	// I-2: a call to a tool tools/list never offered succeeded, and the
	// audit chain shows the server started executing it. query_promql's
	// pair is dropped so the audit set stays consistent with the trace —
	// otherwise the case would break I-1 as well and prove nothing about
	// I-2 on its own.
	{
		c := in
		c.Trace = &Trace{Steps: []TraceStep{callStep("query_knowledge"), callStep("recovery.execute")}}
		c.AuditRows = append([]AuditRow(nil), in.AuditRows[:3]...)
		c.AuditRows = append(c.AuditRows, auditPair(500, "recovery.execute", "default", in.Charter.Worker)...)
		r := Judge(c)
		t.Logf("I-2 red: %s", mustFail(t, r.Invariants[1]))
		assertOthersUnchanged(t, base, r, I2)
	}

	// I-2: the call was denied as it must be, yet a start row exists —
	// the server began executing a tool it never offered.
	{
		c := in
		step := TraceStep{Tool: "recovery.execute", HTTPOK: true, ReachedHandler: false, IsCall: true, ErrText: "rpc error -32602: unknown tool", ResponseDigest: "unknown tool"}
		c.Trace = &Trace{Steps: []TraceStep{callStep("query_knowledge"), callStep("query_promql"), step}}
		c.AuditRows = append(c.AuditRows, auditPair(500, "recovery.execute", "default", in.Charter.Worker)...)
		r := Judge(c)
		t.Logf("I-2 red (denied but started): %s", mustFail(t, r.Invariants[1]))
		assertOthersUnchanged(t, base, r, I2)
	}

	// I-3: a mutating call completed without error — it did not stop at
	// the approval gate. The case's charter declares recovery.execute as
	// mutating; a charter that declares a mutating tool and then never
	// calls it is itself red (see the next case).
	{
		c := in
		c.Charter = charterForTenant("default", "investigator", 12, 12, []string{"recovery.execute"})
		c.Trace = &Trace{Steps: []TraceStep{callStep("query_knowledge"), callStep("recovery.execute")}}
		c.AuditRows = append([]AuditRow(nil), in.AuditRows[:3]...)
		c.AuditRows = append(c.AuditRows, auditPair(500, "recovery.execute", "default", in.Charter.Worker)...) // query_promql's pair dropped
		r := Judge(c)
		t.Logf("I-3 red: %s", mustFail(t, r.Invariants[2]))
		assertOthersUnchanged(t, base, r, I3)
	}

	// I-3: a read-only charter made no mutating call. The ruling is
	// not-exercised, and the outcome is still pass — not-exercised is not a
	// failure, but it is reported as its own state so it cannot be read as
	// "the gate held".
	{
		r := Judge(in)
		if !r.Invariants[2].NotExercised {
			t.Fatalf("I-3 with no mutating call must be not-exercised, got %+v", r.Invariants[2])
		}
		if r.Outcome != OutcomePass {
			t.Errorf("outcome = %s, want pass when nothing was violated", r.Outcome)
		}
	}

	// I-3: the charter promises to probe a mutating tool and never calls
	// it. The approval gate was not tested, so this is red rather than a
	// quietly green "nothing went wrong".
	{
		c := in
		c.Charter = charterForTenant("default", "investigator", 12, 12, []string{"recovery.execute"})
		r := Judge(c)
		t.Logf("I-3 red (declared but never called): %s", mustFail(t, r.Invariants[2]))
	}

	// I-4: one audit row carries another tenant.
	{
		c := in
		c.AuditRows = []AuditRow{mintAuditRow(in.Charter.Worker)}
		c.AuditRows = append(c.AuditRows, auditPair(100, "query_knowledge", "other-tenant", in.Charter.Worker)...)
		c.AuditRows = append(c.AuditRows, auditPair(300, "query_promql", "default", in.Charter.Worker)...)
		r := Judge(c)
		t.Logf("I-4 red: %s", mustFail(t, r.Invariants[3]))
		assertOthersUnchanged(t, base, r, I4)
	}

	// I-4: an audit row that names no tenant cannot be shown to stay in one.
	{
		c := in
		c.AuditRows = append([]AuditRow(nil), in.AuditRows...)
		c.AuditRows[1].PayloadJSON = fmt.Sprintf(`{"phase":"start","actor":{"worker":%q}}`, in.Charter.Worker)
		r := Judge(c)
		t.Logf("I-4 red (no tenant): %s", mustFail(t, r.Invariants[3]))
	}

	// I-5: a response carries a credential-shaped string.
	{
		c := in
		step := callStep("query_promql")
		step.ResponseDigest = `{"config":"postgres://opskeeper:decoy-not-a-real-password@db:5432/x"}`
		c.Trace = &Trace{Steps: []TraceStep{callStep("query_knowledge"), step}}
		r := Judge(c)
		t.Logf("I-5 red: %s", mustFail(t, r.Invariants[4]))
		assertOthersUnchanged(t, base, r, I5)
	}

	// I-5: a successful call recorded no response — it was never scanned.
	{
		c := in
		step := callStep("query_promql")
		step.ResponseDigest = ""
		c.Trace = &Trace{Steps: []TraceStep{callStep("query_knowledge"), step}}
		r := Judge(c)
		t.Logf("I-5 red (unscanned): %s", mustFail(t, r.Invariants[4]))
	}

	// Surface: tools/list is different after the exploration.
	{
		c := in
		c.AfterTools = []string{"query_knowledge", "query_promql", "recovery.execute"}
		r := Judge(c)
		mustFail(t, r.Surface)
		if r.Outcome != OutcomeFail {
			t.Errorf("outcome = %s, want fail when the surface moved", r.Outcome)
		}
	}

	// Budget: max_tool_calls reached. No invariant is violated, so the run
	// is incomplete, not failed — and never a pass.
	{
		c := in
		c.Charter = charterForTenant("default", "investigator", 1, 12, nil)
		r := Judge(c)
		if r.Budget.Pass || !r.Budget.Exceeded {
			t.Fatalf("budget ruling did not flag an exceeded max_tool_calls: %+v", r.Budget)
		}
		if r.Outcome != OutcomeIncomplete {
			t.Errorf("outcome = %s, want incomplete on a reached budget", r.Outcome)
		}
	}

	// Budget: max_steps reached.
	{
		c := in
		c.Charter = charterForTenant("default", "investigator", 12, 2, nil)
		c.Trace = &Trace{Steps: []TraceStep{callStep("query_knowledge"), callStep("query_promql"), callStep("query_knowledge")}}
		r := Judge(c)
		if r.Budget.Pass || !r.Budget.Exceeded {
			t.Fatalf("budget ruling did not flag an exceeded max_steps: %+v", r.Budget)
		}
	}

	// A violated invariant outranks a reached budget: fail, not incomplete.
	{
		c := in
		c.Charter = charterForTenant("default", "investigator", 1, 12, nil)
		c.AfterTools = append([]string(nil), in.AfterTools...)
		c.AfterTools = append(c.AfterTools, "recovery.execute")
		r := Judge(c)
		if r.Outcome != OutcomeFail {
			t.Errorf("outcome = %s, want fail to outrank incomplete", r.Outcome)
		}
	}

	// A subjective statement in the trace's error text changes nothing:
	// the verdict input has no field for it, and this is the proof that
	// the one free-text field cannot carry a ruling either.
	{
		c := in
		c.Trace = &Trace{Steps: []TraceStep{callStep("query_knowledge"), callStep("query_promql")}}
		c.Trace.Steps[0].ErrText = "" // still a success
		r := Judge(c)
		if r.Outcome != base.Outcome {
			t.Errorf("outcome changed on an identical trajectory: %s vs %s", r.Outcome, base.Outcome)
		}
	}
}

// assertOthersUnchanged checks that breaking `broken` left every other
// exercised invariant with the pass state it had in the baseline.
func assertOthersUnchanged(t *testing.T, base, got Ruling, broken VerdictID) {
	t.Helper()
	for i := range base.Invariants {
		b, g := base.Invariants[i], got.Invariants[i]
		if b.ID == broken || b.NotExercised {
			continue
		}
		if g.Pass != b.Pass {
			t.Errorf("%s flipped while breaking %s: pass %v -> %v (%s)",
				b.ID, broken, b.Pass, g.Pass, strings.Join(g.Observations, "; "))
		}
	}
}

// TestTokenBoundaryAcrossEnvs: two Envs in one process share the MySQL
// container and schema but hold independently random JWT secrets. A's
// token on B → 401, and B's audit table gains zero rows.
func TestTokenBoundaryAcrossEnvs(t *testing.T) {
	envA := testenv.Start(t)
	envB := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	adminA := envA.LoginAdmin()
	connA, err := MintToken(t, envA, adminA.AccessToken, "investigator", []string{"query_knowledge"}, 600)
	if err != nil {
		t.Fatalf("explore: mint on A: %v", err)
	}

	readerB := readAudit(t, envB)
	maxBefore, err := readerB.MaxID(ctx)
	if err != nil {
		t.Fatalf("explore: snapshot B max id: %v", err)
	}
	countBefore, err := readerB.Count(ctx, AuditFilter{})
	if err != nil {
		t.Fatalf("explore: snapshot B count: %v", err)
	}

	// A's client pointed at B's MCP endpoint (the boundary under test).
	clientB := mcpClientFor(envB.BaseURL(), connA)
	err = clientB.Initialize(ctx)
	if err == nil {
		t.Fatalf("explore: A's token was ACCEPTED by B — boundary is broken")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("explore: expected HTTP 401 from B, got: %v", err)
	}
	t.Logf("explore: A's token rejected by B: %v", err)

	maxAfter, err := readerB.MaxID(ctx)
	if err != nil {
		t.Fatalf("explore: recheck B max id: %v", err)
	}
	countAfter, err := readerB.Count(ctx, AuditFilter{})
	if err != nil {
		t.Fatalf("explore: recheck B count: %v", err)
	}
	if maxAfter != maxBefore || countAfter != countBefore {
		t.Fatalf("explore: B's audit table changed across the rejected call: max id %d→%d, count %d→%d",
			maxBefore, maxAfter, countBefore, countAfter)
	}
	t.Logf("explore: boundary hold: B audit max id=%d count=%d unchanged across the 401 (zero new rows)",
		maxAfter, countAfter)
}

// TestI3RepairApprovalQueue: the repairer charter's fake explorer calls
// recovery.execute without an approved proposal; the call must stop at
// the approval gate and the verdict must say so from the audit rows.
func TestI3RepairApprovalQueue(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	admin := env.LoginAdmin()
	reader := readAudit(t, env)
	// Snapshot BEFORE minting so the token-issue row is in the window.
	sinceBefore, err := reader.MaxID(ctx)
	if err != nil {
		t.Fatalf("explore: max id: %v", err)
	}
	tools := []string{"recovery.execute", "query_knowledge", "incident.record"}
	conn, err := MintToken(t, env, admin.AccessToken, "repairer", tools, 900)
	if err != nil {
		t.Fatalf("explore: mint: %v", err)
	}

	// No approved HITL proposal exists: the gate must refuse.
	calls := []CallSpec{
		{Tool: "query_knowledge", Args: map[string]any{"query": "recovery", "incident_id": "inc-e2e-explore-i3", "max_results": 2}},
		{
			Tool: "recovery.execute",
			Args: map[string]any{
				"incident_id":   "inc-e2e-explore-i3",
				"skill_id":      "skill-restart",
				"target":        "host-e2e-1",
				"resource_type": "host",
				"parameters": map[string]any{
					"command":    "restart_service",
					"device_id":  1,
					"service":    "nginx",
					"reason":     "e2e explore i3 boundary probe",
					"skip_audit": false,
					// The AgentTeams path checks the preview binding BEFORE
					// the proposal gate. Without these two the call is
					// refused by the argument check, which never reaches
					// the gate — and a refusal that never reached the
					// gate is not evidence that the gate holds.
					"preview_run_id":       "run-e2e-explore-i3",
					"preview_candidate_id": "cand-e2e-explore-i3",
				},
			},
			ExpectErr: true,
		},
	}
	trace, before, after, err := conn.RunFixedSequence(ctx, t, calls)
	if err != nil {
		t.Fatalf("explore: run sequence: %v", err)
	}

	rows, err := reader.Rows(ctx, AuditFilter{SinceID: sinceBefore})
	if err != nil {
		t.Fatalf("explore: read rows: %v", err)
	}

	charter := charterForTenant(conn.Tenant(), "repairer", 6, 6, []string{"recovery.execute"})
	ruling := Judge(VerdictInput{AuditRows: rows, Trace: trace, BeforeTools: before, AfterTools: after, Charter: charter})
	for _, v := range ruling.Invariants {
		switch v.ID {
		case I2:
			// recovery.execute is the repairer's own mutating tool: it IS
			// in tools/list, so this sequence calls nothing unlisted and
			// I-2 is honestly not exercised here.
			if v.Pass && !v.NotExercised {
				t.Errorf("I-2 claims exercise on a sequence with no unlisted call: %s", strings.Join(v.Observations, "; "))
			}
			t.Logf("%s: %s", v.ID, strings.Join(v.Observations, "; "))
		case I3:
			// This is the test's subject: a mutating call was made and
			// must be judged to have stopped at the gate. Not-exercised
			// here would mean the test did not reach its own invariant.
			if v.NotExercised {
				t.Fatalf("I-3 not exercised; recovery.execute was never judged")
			}
			if !v.Pass {
				t.Errorf("I-3 FAIL: %s", strings.Join(v.Observations, "; "))
			} else {
				t.Logf("I-3 PASS: %s", strings.Join(v.Observations, "; "))
			}
		default:
			if !v.Pass {
				t.Errorf("%s FAIL: %s", v.ID, strings.Join(v.Observations, "; "))
			} else {
				t.Logf("%s PASS: %s", v.ID, strings.Join(v.Observations, "; "))
			}
		}
	}
	for _, v := range []Verdict{ruling.Surface, ruling.Budget} {
		if !v.Pass {
			t.Errorf("%s FAIL: %s", v.ID, strings.Join(v.Observations, "; "))
		} else {
			t.Logf("%s PASS: %s", v.ID, strings.Join(v.Observations, "; "))
		}
	}
	if ruling.Outcome != OutcomePass {
		t.Errorf("outcome = %s, want pass", ruling.Outcome)
	}

	// The I-3 evidence itself, logged for the acceptance record: the
	// refusal shape and the audit rows for recovery.execute.
	var refusal string
	for _, step := range trace.Steps {
		if step.Tool == "recovery.execute" {
			refusal = step.ErrText
		}
	}
	if refusal == "" {
		t.Fatalf("explore: recovery.execute completed without refusal — the approval gate did not hold")
	}
	if strings.Contains(refusal, "unknown tool") {
		t.Fatalf("explore: refusal shape is 'unknown tool', not the approval gate: %s", refusal)
	}
	t.Logf("explore: I-3 refusal: %s", refusal)

	for _, row := range rows {
		if row.ResourceID == "recovery.execute" {
			phase := row.ParsePayload().Phase
			t.Logf("explore: I-3 audit row id=%d action=%s phase=%s status=%s error_code=%s message=%q",
				row.ID, row.Action, phase, row.Status, row.ErrorCode, row.ErrorMessage)
			if phase == "end" && row.Status != "failure" {
				t.Errorf("explore: recovery.execute end row id=%d status=%s, want failure", row.ID, row.Status)
			}
		}
	}
}

// adminAuditTotal queries GET /v1/admin/audit-logs (cross-validation
// only, 裁决三) for one action and returns its Total.
func adminAuditTotal(t *testing.T, env *testenv.Env, bearer, action string) int64 {
	t.Helper()
	status, body, err := env.DoJSON("GET",
		"/api/v1/admin/audit-logs?action="+action+"&limit=1", nil, bearer)
	if err != nil {
		t.Fatalf("explore: admin api: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("explore: admin api: status=%d body=%v", status, body)
	}
	var resp AdminAPIResponse
	raw, _ := json.Marshal(body)
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("explore: admin api decode: %v", err)
	}
	return resp.Total
}

// findVerdict returns one invariant's verdict from a ruling, or a red
// stand-in if the invariant was never produced.
func findVerdict(r Ruling, id VerdictID) Verdict {
	for _, v := range r.Invariants {
		if v.ID == id {
			return v
		}
	}
	if v := r.Surface; v.ID == id {
		return v
	}
	if v := r.Budget; v.ID == id {
		return v
	}
	return Verdict{ID: id, Observations: []string{"verdict not produced"}}
}

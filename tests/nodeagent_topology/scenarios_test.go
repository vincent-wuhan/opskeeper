package e2e

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodefleet"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

const (
	alertStormRaw     = 50
	alertStormHosts   = 5
	alertStormPerHost = alertStormRaw / alertStormHosts
)

// installedTools is the node's installed set as its manifest declared it.
// The mutating tools are present on purpose: a scenario that only ever
// offered reads would pass whether or not the gate works.
func installedTools() map[string]domain.ToolClass {
	return map[string]domain.ToolClass{
		"query_promql":    domain.ClassRead,
		"get_host_load":   domain.ClassRead,
		"get_topology":    domain.ClassRead,
		"restart_service": domain.ClassWrite,
		"rollback_deploy": domain.ClassDestructive,
	}
}

// call is one tool the agent will propose. SessionID and Actor are filled
// in by the node when the turn runs, because they are properties of the
// conversation rather than of the proposal.
func call(tool string, class domain.ToolClass, args, target, summary string) *policygate.Call {
	return &policygate.Call{
		ToolName:  tool,
		Class:     class,
		Arguments: []byte(args),
		Target:    target,
		Summary:   summary,
		Actor:     "admin",
	}
}

// incident is one deduplicated alert group.
type incident struct {
	Host  string
	Count int
}

// dedupAlerts is the ingestion step: alertStormRaw alerts collapse to one
// incident per host. It is written out here rather than reached for
// through a production deduper because the topology is what these
// scenarios are about — the collapse is asserted, not exercised.
func dedupAlerts() []incident {
	counts := map[string]int{}
	for i := 0; i < alertStormRaw; i++ {
		counts[fmt.Sprintf("host-%d", i%alertStormHosts)]++
	}
	out := make([]incident, 0, len(counts))
	for host, n := range counts {
		out = append(out, incident{Host: host, Count: n})
	}
	// Stable order so a failure names the same host every run.
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// node is one edge plus the conversation opened on it.
type node struct {
	edgeID    uint64
	sessionID string
	turn      string
	top       *topology
}

// startNode wires a node, opens one conversation and scripts its turn.
func startNode(t *testing.T, edgeID uint64, sessionID string, steps []step, opts ...topologyOpt) *node {
	t.Helper()
	top := newTopology(t, edgeID, installedTools(), opts...)
	// The agent only learns the session id from the prompt it is handed —
	// the bridge passes req.Text and nothing else — so the scenario uses
	// the session id as the turn text. Anything else would have the node
	// emitting frames the control plane cannot route back to a console.
	turn := sessionID
	top.agent.script(turn, steps)
	if _, err := top.fleet.Open(nodefleet.PromptRequest{
		EdgeID: edgeID, SessionID: sessionID, UserText: turn, Role: "admin", Locale: "zh-CN",
	}, top.console); err != nil {
		t.Fatalf("open session on edge %d: %v", edgeID, err)
	}
	return &node{edgeID: edgeID, sessionID: sessionID, turn: turn, top: top}
}

func (n *node) prompt(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.top.fleet.Prompt(ctx, nodefleet.PromptRequest{
		EdgeID: n.edgeID, SessionID: n.sessionID, UserText: n.turn, Role: "admin", Locale: "zh-CN",
	}); err != nil {
		t.Fatalf("prompt edge %d: %v", n.edgeID, err)
	}
}

func (n *node) awaitTurnEnd(t *testing.T) {
	t.Helper()
	n.top.console.waitFor(t, "the turn to finish", func() bool {
		return n.top.console.count(wire.StreamDone) > 0
	})
}

// ── scenario 1: alert_storm ───────────────────────────────────────────

// TestScenarioAlertStorm_DiagnosesEveryIncidentAcrossNodes runs a storm
// of alerts at five nodes and checks that each deduplicated incident got
// a completed diagnosis on its own node, with no human asked.
//
// The safety assertion is the load-bearing one. A storm is exactly when
// an agent is most tempted to reach for a mutating tool "to reduce the
// noise", and a topology that quietly allowed it would satisfy every
// other assertion in this file.
func TestScenarioAlertStorm_DiagnosesEveryIncidentAcrossNodes(t *testing.T) {
	incidents := dedupAlerts()
	if len(incidents) != alertStormHosts {
		t.Fatalf("dedup produced %d incidents, want %d", len(incidents), alertStormHosts)
	}
	total := 0
	for _, inc := range incidents {
		if inc.Count != alertStormPerHost {
			t.Fatalf("incident %s carries %d alerts, want %d", inc.Host, inc.Count, alertStormPerHost)
		}
		total += inc.Count
	}
	if total != alertStormRaw {
		t.Fatalf("dedup lost alerts: %d of %d survived", total, alertStormRaw)
	}

	nodes := make([]*node, 0, alertStormHosts)
	for i := 0; i < alertStormHosts; i++ {
		edgeID := uint64(100 + i)
		n := startNode(t, edgeID, fmt.Sprintf("storm-%d", edgeID), []step{
			{say: "investigating"},
			{tool: call("query_promql", domain.ClassRead,
				`{"expr":"pg_connection_utilization"}`, "db-primary", "read pool utilisation"),
				result: `{"value":0.97}`},
			{tool: call("get_host_load", domain.ClassRead, `{}`, "db-primary", "host load"),
				result: `{"load":0.99}`},
			{say: "diagnosis: connection pool exhausted"},
		})
		n.prompt(t)
		nodes = append(nodes, n)
	}
	for _, n := range nodes {
		n.awaitTurnEnd(t)
	}

	for _, n := range nodes {
		c := n.top.console
		if c.count(wire.StreamToolStart) != 2 {
			t.Errorf("edge %d: console saw %d tool starts, want 2", n.edgeID, c.count(wire.StreamToolStart))
		}
		names := c.toolNames()
		for _, want := range []string{"query_promql", "get_host_load"} {
			if !contains(names, want) {
				t.Errorf("edge %d: tool %q never reached the console; got %v", n.edgeID, want, names)
			}
		}
		// Every read must have produced the value it was scripted to
		// return. A non-empty result is not enough: a gate that refused
		// every read would still fill this list, with its own refusals,
		// and a storm diagnosed entirely out of refusal messages is a
		// storm that diagnosed nothing.
		want := []string{`{"value":0.97}`, `{"load":0.99}`}
		got := c.toolResults()
		if len(got) != len(want) {
			t.Errorf("edge %d: %d tool results, want %d (%v)", n.edgeID, len(got), len(want), got)
		}
		for i, w := range want {
			if i < len(got) && got[i] != w {
				t.Errorf("edge %d: tool result %d = %q, want %q — the read did not return its data", n.edgeID, i, got[i], w)
			}
		}
		if c.count(wire.StreamApprovalPending) != 0 {
			t.Errorf("edge %d: a read-only storm raised %d approvals", n.edgeID, c.count(wire.StreamApprovalPending))
		}
		if got := n.top.gate.ReceiptCount(); got != 0 {
			t.Errorf("edge %d: %d approval receipts minted with no human involved", n.edgeID, got)
		}
		stats := n.top.gate.Snapshot()
		if stats.Granted != 0 || stats.Denied != 0 {
			t.Errorf("edge %d: gate recorded %d grants / %d denials during a read-only storm",
				n.edgeID, stats.Granted, stats.Denied)
		}
		if n.top.lb.countMethod(tunnel.MethodAgentPrompt) != 1 {
			t.Errorf("edge %d: %d prompts crossed the tunnel, want 1",
				n.edgeID, n.top.lb.countMethod(tunnel.MethodAgentPrompt))
		}
	}
}

// TestScenarioAlertStorm_AgentCannotQuietTheStormByRestarting is the
// adversarial half of the same scenario. The agent proposes the mutating
// tool the storm would tempt it into, the node refuses it, and the
// refusal is visible on the console — a silent refusal would leave an
// operator reading a diagnosis that quietly omitted a step.
func TestScenarioAlertStorm_AgentCannotQuietTheStormByRestarting(t *testing.T) {
	// No human is on call in this scenario, which is the point of it: an
	// unattended agent must not get to decide on its own that restarting
	// a service is an acceptable way to make the alerts stop. The gate
	// therefore waits out its deadline, and the deadline fails closed —
	// a short TTL here only makes that branch arrive quickly, it does not
	// choose it.
	n := startNode(t, 200, "storm-mutating", []step{
		{tool: call("restart_service", domain.ClassWrite,
			`{"service":"api"}`, "api-01", "restart the api service"), result: "restarted"},
		{say: "the storm persists"},
	}, withApprovalTTL(150*time.Millisecond))
	n.prompt(t)
	n.awaitTurnEnd(t)

	c := n.top.console
	names := c.toolNames()
	if !contains(names, "restart_service") {
		t.Fatalf("the refused call never reached the console; got %v", names)
	}
	for _, r := range c.toolResults() {
		if r == "restarted" {
			t.Fatal("a mutating tool ran during an unattended read-only storm")
		}
	}
	// The refusal must be an approval round, not a silent block: the
	// gate records both, and a scenario that only checks "did not run"
	// would pass even if the agent never learned it had been stopped.
	if c.count(wire.StreamApprovalPending) != 1 {
		t.Errorf("console saw %d approval frames, want 1 — the agent was stopped without being told",
			c.count(wire.StreamApprovalPending))
	}
	if !n.top.spy.saw(ports.ActionApprovalRequest) {
		t.Error("the gate's ledger has no approval request for the refused restart")
	}
	// Nothing may be granted: no human is present in this scenario, and
	// an approval that resolved without one is the failure this whole
	// topology exists to prevent.
	if got := n.top.gate.ReceiptCount(); got != 0 {
		t.Errorf("%d receipts minted with no decision in flight", got)
	}
}

// ── the HITL round trip ───────────────────────────────────────────────

// decide answers the pending approval the console is showing.
//
// It reads the request id and digest off the frame the console actually
// rendered rather than off anything the test kept, because that is the
// only thing an operator could have: the decision travels back bound to
// the digest the node computed, and a node that is handed a digest it
// cannot match refuses it.
func (n *node) decide(t *testing.T, grant bool, by, note string) {
	t.Helper()
	if err := n.grant(by, note, grant); err != nil {
		t.Fatalf("decide on edge %d: %v", n.edgeID, err)
	}
}

// grant is decide's body without a testing.T, so it can be called from a
// goroutine that must not fail the test directly.
func (n *node) grant(by, note string, allow bool) error {
	req := n.awaitApprovalSoft()
	if req == nil {
		return errors.New("no approval request reached the console")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return n.top.fleet.Decide(ctx, n.edgeID, n.sessionID, nodefleet.Decision{
		RequestID: req.RequestID,
		Digest:    req.Digest,
		Grant:     allow,
		DecidedBy: by,
		Note:      note,
	})
}

// awaitApprovalSoft is awaitApproval without a testing.T, for the same
// reason. It gives up after a bounded wait rather than blocking forever.
func (n *node) awaitApprovalSoft() *wire.ApprovalFrame {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range n.top.console.snapshot() {
			if f.Type == wire.StreamApprovalPending && f.Approval != nil && f.Approval.Decision == "" {
				got := *f.Approval
				return &got
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// awaitApproval blocks until the console shows a request that has not been
// answered yet.
func (n *node) awaitApproval(t *testing.T) wire.ApprovalFrame {
	t.Helper()
	var found wire.ApprovalFrame
	n.top.console.waitFor(t, "an approval request", func() bool {
		for _, f := range n.top.console.snapshot() {
			if f.Type == wire.StreamApprovalPending && f.Approval != nil && f.Approval.Decision == "" {
				found = *f.Approval
				return true
			}
		}
		return false
	})
	return found
}

// resolved returns the console's record of how the approval ended.
func (n *node) resolved(t *testing.T) wire.ApprovalFrame {
	t.Helper()
	var out wire.ApprovalFrame
	n.top.console.waitFor(t, "the approval to resolve", func() bool {
		for _, f := range n.top.console.snapshot() {
			if f.Type == wire.StreamApprovalResolved && f.Approval != nil {
				out = *f.Approval
				return true
			}
		}
		return false
	})
	return out
}

// ── scenario 2: rca_loop ──────────────────────────────────────────────

// TestScenarioRCALoop_ARejectedActionIsVisibleToTheAgent runs the loop
// the teamharness eval calls rca_loop: the investigation wants a
// cluster-wide rollback, a human refuses it, and the agent has to be able
// to carry on and report what it found instead.
//
// The property under test is not that the rollback was blocked — the
// gate's own tests cover that. It is that the refusal came back over the
// tunnel, was bound to the digest the operator was shown, and reached the
// console as a resolved frame. A topology that dropped the decision, or
// that answered it without the digest, would leave the agent waiting for
// the whole approval TTL and the operator staring at a spinner.
func TestScenarioRCALoop_ARejectedActionIsVisibleToTheAgent(t *testing.T) {
	n := startNode(t, 300, "rca-loop", []step{
		{tool: call("query_promql", domain.ClassRead, `{"expr":"errors_total"}`, "api", "read error rate"),
			result: `{"value":0.42}`},
		{tool: call("rollback_deploy", domain.ClassDestructive,
			`{"release":"r-99","cluster":"prod"}`, "prod", "roll back the cluster release"),
			result: "rolled back"},
		{say: "rollback refused; root cause is a bad config push on r-98"},
	})
	n.prompt(t)

	req := n.awaitApproval(t)
	if req.Tool != "rollback_deploy" {
		t.Fatalf("approval was raised for %q, want rollback_deploy", req.Tool)
	}
	if req.Decision != "" {
		t.Errorf("the request frame already carries a decision %q", req.Decision)
	}
	if req.Class != string(domain.ClassDestructive) {
		t.Errorf("approval class = %q, want %q — the operator must be shown the class that was enforced",
			req.Class, domain.ClassDestructive)
	}
	if req.Digest == "" {
		t.Fatal("the approval carries no digest; a decision could not be bound to this call")
	}

	n.decide(t, false, "oncall@example.com", "not that release")

	got := n.resolved(t)
	if got.Decision != "deny" {
		t.Errorf("console saw decision %q, want deny", got.Decision)
	}
	n.awaitTurnEnd(t)

	if n.top.lb.countMethod(tunnel.MethodAgentDecide) == 0 {
		t.Error("no decision crossed the tunnel; the node was never told")
	}
	if contains(n.top.console.toolResults(), "rolled back") {
		t.Fatal("a denied destructive action ran anyway")
	}
	// The agent must have been told, not merely stopped: a refusal the
	// agent cannot see is a refusal it will retry forever.
	if !n.top.spy.saw(ports.ActionApprovalDeny) {
		t.Error("the gate's ledger has no denial")
	}
	// And the receipt must not exist: a granted-receipt path that ran
	// anyway is the failure the second check at the broker exists for.
	if got := n.top.gate.ReceiptCount(); got != 0 {
		t.Errorf("%d receipts minted for a denied call", got)
	}
}

// TestScenarioRCALoop_AGrantedActionRunsAfterTheDecisionTravelsBack is
// the same loop with the human saying yes. It is the half that fails if
// the digest binding is wrong: the node recomputes the digest and refuses
// a decision that does not match the call it is holding, so a control
// plane that echoes the wrong thing gets no action rather than the wrong
// action.
func TestScenarioRCALoop_AGrantedActionRunsAfterTheDecisionTravelsBack(t *testing.T) {
	n := startNode(t, 301, "rca-granted", []step{
		{tool: call("restart_service", domain.ClassWrite,
			`{"service":"api"}`, "api-01", "restart the api service"), result: "restarted"},
		{say: "api healthy after restart"},
	})
	n.prompt(t)

	// The tool is held at the gate until the decision arrives, so the
	// answer has to be sent from another goroutine. A t.Fatalf in there
	// would be reported from a non-test goroutine, so the error is
	// carried back and raised here.
	decided := make(chan error, 1)
	go func() {
		decided <- n.grant("sre@example.com", "confirmed in the incident channel", true)
	}()

	n.awaitTurnEnd(t)

	if err := <-decided; err != nil {
		t.Fatalf("grant did not reach the node: %v", err)
	}
	got := n.resolved(t)
	if got.Decision != "grant" {
		t.Fatalf("console saw decision %q, want grant", got.Decision)
	}
	if n.top.lb.countMethod(tunnel.MethodAgentDecide) == 0 {
		t.Error("no decision crossed the tunnel")
	}
	if !contains(n.top.console.toolResults(), "restarted") {
		t.Fatalf("a granted action did not run; console saw %v", n.top.console.toolResults())
	}
	if !n.top.spy.saw(ports.ActionApprovalGrant) {
		t.Error("the gate's ledger has no grant")
	}
}

// TestScenarioRCALoop_ADecisionWithTheWrongDigestIsRefused pins the
// binding. A replayed or hand-edited decision must not release a call the
// operator was never shown, so the node answers with a code and nothing
// runs.
func TestScenarioRCALoop_ADecisionWithTheWrongDigestIsRefused(t *testing.T) {
	n := startNode(t, 302, "rca-bad-digest", []step{
		{tool: call("restart_service", domain.ClassWrite,
			`{"service":"api"}`, "api-01", "restart the api service"), result: "restarted"},
		{say: "done"},
	})
	n.prompt(t)
	req := n.awaitApproval(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := n.top.fleet.Decide(ctx, n.edgeID, n.sessionID, nodefleet.Decision{
		RequestID: req.RequestID,
		Digest:    "not-the-digest-the-operator-was-shown",
		Grant:     true,
		DecidedBy: "attacker@example.com",
	})
	if err == nil {
		t.Fatal("the node accepted a decision whose digest it could not match")
	}
	if !nodefleet.IsRefusal(err) {
		t.Errorf("error = %v, want a refusal the control plane can classify", err)
	}

	// The call is still held. The forged decision did not release it and
	// did not answer it either — the request is still open on the console
	// — so the turn cannot finish yet. Answer the same request properly,
	// with the digest the operator was actually shown, and refuse it.
	// The tool must not have run under either decision.
	if err := n.grant("sre@example.com", "refused on purpose", false); err != nil {
		t.Fatalf("the real decision did not reach the node: %v", err)
	}
	n.awaitTurnEnd(t)
	if contains(n.top.console.toolResults(), "restarted") {
		t.Fatal("a call released by a forged decision ran")
	}
	got := n.resolved(t)
	if got.Decision != "deny" {
		t.Errorf("console resolved the request as %q, want deny", got.Decision)
	}
}

// ── scenario 3: recovery_verify ───────────────────────────────────────

// TestScenarioRecoveryVerify_AnApprovedRecoveryIsFollowedByAReadOnlyCheck
// closes the loop the other two leave open: something is fixed, and then
// something reads the world back to confirm the fix landed.
//
// The order is the load-bearing assertion. A recovery that ran and a
// check that ran are both easy; a check that ran *before* the recovery, or
// a recovery that was approved but never verified, is what this scenario
// exists to catch, so the tool results are compared as an ordered pair
// rather than as two set members.
func TestScenarioRecoveryVerify_AnApprovedRecoveryIsFollowedByAReadOnlyCheck(t *testing.T) {
	const (
		recovery  = "rolled back to v2.2.9"
		verified  = "load 0.31, saturation gone"
		summary   = "api is healthy on v2.2.9; the saturation was the release"
		approver  = "sre@example.com"
		note      = "saturation traced to the v2.3.1 release"
		releaseID = "prod-api"
	)
	n := startNode(t, 400, "recovery-verify", []step{
		{tool: call("rollback_deploy", domain.ClassDestructive,
			`{"release":"v2.3.1"}`, releaseID, "roll the api release back to v2.2.9"), result: recovery},
		{tool: call("get_host_load", domain.ClassRead,
			`{"host":"api-01"}`, "api-01", "read the load back"), result: verified},
		{say: summary},
	})
	n.prompt(t)

	// The recovery is destructive, so the agent is held at the gate until
	// a human answers. The answer is sent from another goroutine because
	// the turn cannot finish while the call is held.
	decided := make(chan error, 1)
	go func() { decided <- n.grant(approver, note, true) }()

	n.awaitTurnEnd(t)
	if err := <-decided; err != nil {
		t.Fatalf("the approval did not reach the node: %v", err)
	}

	// The recovery came first and the check second.
	got := n.top.console.toolResults()
	if len(got) != 2 || got[0] != recovery || got[1] != verified {
		t.Fatalf("tool results = %v, want [%q %q] — the check did not follow the recovery", got, recovery, verified)
	}
	if n.top.console.assistantText() == "" {
		t.Error("the turn ended without the summary the operator reads")
	}

	// One approval, for the recovery. The verification is a read and must
	// not have asked a human for anything: a topology where confirming a
	// fix needs sign-off is a topology nobody runs twice.
	if c := n.top.console.count(wire.StreamApprovalPending); c != 1 {
		t.Errorf("console saw %d approvals, want 1 — the read-only check asked a human for permission", c)
	}
	res := n.resolved(t)
	if res.Decision != "grant" {
		t.Errorf("console resolved the recovery as %q, want grant", res.Decision)
	}
	// The operator is shown what they are approving. A cluster-wide
	// rollback that reached the console labelled "pod" would be a
	// governance failure no other assertion in this file would notice.
	shown := n.pendingApproval(t)
	if shown.BlastRadius != string(domain.RadiusCluster) {
		t.Errorf("console showed blast radius %q, want %q", shown.BlastRadius, domain.RadiusCluster)
	}
	if shown.Class != string(domain.ClassDestructive) {
		t.Errorf("console showed class %q, want %q — the operator was asked about a milder call than the one that would run",
			shown.Class, domain.ClassDestructive)
	}
	if shown.Tool != "rollback_deploy" {
		t.Errorf("approval was shown for %q, want rollback_deploy", shown.Tool)
	}

	if n.top.lb.countMethod(tunnel.MethodAgentDecide) != 1 {
		t.Errorf("%d decisions crossed the tunnel, want 1", n.top.lb.countMethod(tunnel.MethodAgentDecide))
	}
	for _, action := range []ports.AuditAction{ports.ActionApprovalRequest, ports.ActionApprovalGrant} {
		if !n.top.spy.saw(action) {
			t.Errorf("the ledger has no %s for an approved recovery", action)
		}
	}
	// No receipt is left standing. One approval buys one execution: the
	// grant was claimed by the recovery it was granted for, the read never
	// went near the receipt path, and a receipt that survived the turn
	// would be a second run the operator never agreed to.
	if got := n.top.gate.ReceiptCount(); got != 0 {
		t.Errorf("%d approval receipts outlived the turn, want 0", got)
	}
	if stats := n.top.gate.Snapshot(); stats.Granted != 1 || stats.Denied != 0 {
		t.Errorf("gate recorded %d grants / %d denials, want 1 / 0", stats.Granted, stats.Denied)
	}
}

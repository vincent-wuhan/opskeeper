package main

// The BaseTool bridge, exercised the way a node exercises it.
//
// The gap this slice closes was invisible from the control plane: a node's
// package declared a tool, the node's agent was handed its schema, and the
// call went up to a control plane that had never heard of it. Nothing in
// the control plane's own tests failed, because nothing in the control
// plane's own tests asked.
//
// So these tests ask, from the node's side of the wire: name the tool, and
// see whether it comes back.
//
// The stub below stands in for the tunnel. The real handler lives in the
// frontierbound package and has its own tests there; what this one adds is
// the request/response envelope the node's client speaks, so the tests
// below use the same wire types a node sends and check the same shape a
// node reads.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/notify"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promptguard"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	manageraiopstools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/configchange"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
	managerbizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	devicebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
	managerapprovaldata "github.com/vincent-wuhan/opskeeper/core/manager/data/approval/store"
	manageralertdata "github.com/vincent-wuhan/opskeeper/core/manager/data/alert/store"
)

// recordingAuditEmitter keeps the rows the channel wrote.
//
// The audit row is the only place that records WHICH surface served a call,
// so a test that asserts the surface has to read the row rather than
// infer it from the answer.
type recordingAuditEmitter struct {
	events []auditport.Event
}

func (r *recordingAuditEmitter) Emit(_ context.Context, ev auditport.Event) {
	r.events = append(r.events, ev)
}

// lastFor returns the most recent row for one tool.
func (r *recordingAuditEmitter) lastFor(tool string) (auditport.Event, bool) {
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].ResourceID == tool {
			return r.events[i], true
		}
	}
	return auditport.Event{}, false
}

// surfaceOf reads the surface off an audit row.
//
// The payload is free-form by contract, so this is a type assertion rather
// than a field access — and a row whose payload is not a map has no
// surface to report, which is itself worth failing on.
func surfaceOf(ev auditport.Event) (string, bool) {
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		return "", false
	}
	surface, ok := payload["surface"].(string)
	return surface, ok
}

// agentToolTunnelStub speaks the node's side of the agent.tool RPC.
//
// It is deliberately thin: it marshals the request, hands the body to the
// channel, and marshals the answer the way the transport's handler does —
// a tool failure is an answer with an error string in it, not a failed
// RPC, because the node turns a failed RPC into "the control plane is
// unreachable" and an error string into something the model can read.
type agentToolTunnelStub struct {
	channel *agentToolUpcall
	edgeID  uint64
	// tenant names the operator the conversation belongs to. The tunnel
	// itself carries no identity — a node is a node — so the manager
	// resolves the session to its owner before the call runs, and this
	// field is that resolution as the stub would hand it down. The tools
	// that need one (query_knowledge scopes every hit to a tenant) read
	// it through the tenant_bind decorator, same as on every other
	// surface that reaches them.
	tenant string
}

func (s agentToolTunnelStub) call(t *testing.T, req tunnel.AgentToolRequest) tunnel.AgentToolResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal the request: %v", err)
	}
	var decoded tunnel.AgentToolRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode the request: %v", err)
	}
	ctx := context.Background()
	if s.tenant != "" {
		ctx = tenantctx.With(ctx, tenantctx.Tenant{UserID: 7, Role: tenantctx.RoleUser})
	}
	out, err := s.channel.RunAgentTool(ctx, s.edgeID, decoded.SessionID, decoded.Tool, decoded.Arguments)
	if err != nil {
		return tunnel.AgentToolResponse{Error: err.Error()}
	}
	return tunnel.AgentToolResponse{Result: out}
}

// newAgentToolChannel builds the channel the way main() builds it.
//
// The three pieces are the ones that decide what a node can reach: the
// closure registry, the BaseTool bridge, and the audit recorder. The
// bridge is built from the registry's own bag — the same call main()
// makes — so the tools it serves are the tools a deployment serves.
func newAgentToolChannel(t *testing.T) (*agentToolUpcall, *recordingAuditEmitter) {
	t.Helper()
	reg, bag := buildTheRealToolRegistry(t)
	emitter := &recordingAuditEmitter{}
	return &agentToolUpcall{
		reg:      reg,
		basetool: gapGateBridge(t, reg, bag),
		audit:    &agentToolAudit{emitter: emitter},
	}, emitter
}

// A control plane that wired no bridge says so.
//
// The distinction matters to whoever reads the answer. "there is no tool
// by that name" is a claim about the inventory; "the BaseTool surface is
// not wired on this control plane" is a claim about this deployment, and
// it is the one an operator can act on.
func TestAControlPlaneWithNoBridgeSaysSoRatherThanClaimingTheToolDoesNotExist(t *testing.T) {
	reg, _ := buildTheRealToolRegistry(t)
	channel := &agentToolUpcall{
		reg:   reg,
		audit: &agentToolAudit{emitter: &recordingAuditEmitter{}},
	}
	resp := agentToolTunnelStub{channel: channel, edgeID: 7}.call(t, tunnel.AgentToolRequest{
		Tool:      "find_topology_node",
		Arguments: json.RawMessage(`{"name":"checkout"}`),
	})
	if resp.Error == "" {
		t.Fatal("a tool was served on a control plane with no bridge")
	}
	if !strings.Contains(resp.Error, "not wired") {
		t.Errorf("the refusal %q does not say the BaseTool surface is missing from this "+
			"deployment, and that is the sentence an operator can act on", resp.Error)
	}
}

// The six tools that existed only as BaseTools.
//
// This is the list the gap plan calls S-1's subject matter, and it is
// written here rather than derived, because the point of the test is to
// name the specific capabilities that were broken. The reconciliation
// gate above is the one that must not hard-code anything.
var gapTools = []struct {
	name string
	args string
}{
	{name: "grep_source", args: `{"repo":"opskeeper","pattern":"func ResolveEdgeID"}`},
	{name: "list_repo_sources", args: `{"repo":"opskeeper"}`},
	{name: "query_change_events", args: `{"around_ts":"2026-11-01T00:00:00Z","window_minutes":30}`},
	{name: "read_source", args: `{"repo":"opskeeper","path":"core/manager/main.go"}`},
	{name: "expand_topology", args: `{"node_id":1}`},
	{name: "find_topology_node", args: `{"name":"checkout"}`},
}

// Every one of the six comes back over the upcall channel, with a body
// and with the audit row saying the BaseTool surface served it.
//
// Both halves matter. A body without the surface would mean the closure
// registry answered a BaseTool-shaped call — which works until the
// argument shapes diverge, and then it fails on every call while looking
// like a tool that exists.
func TestTheSixGapToolsAreServedOverTheAgentToolChannel(t *testing.T) {
	channel, emitter := newAgentToolChannel(t)
	stub := agentToolTunnelStub{channel: channel, edgeID: 7}

	for _, tc := range gapTools {
		t.Run(tc.name, func(t *testing.T) {
			resp := stub.call(t, tunnel.AgentToolRequest{
				SessionID: "s-1",
				Tool:      tc.name,
				Arguments: json.RawMessage(tc.args),
			})
			if resp.Error != "" {
				t.Fatalf("%s was not served: %s", tc.name, resp.Error)
			}
			if len(resp.Result) == 0 {
				t.Fatalf("%s came back with no body", tc.name)
			}
			// The body comes back fenced when the tool's output comes
			// from outside the platform — a source file, an alert, a
			// log line — because the node's model is told such content
			// is data and not instructions. The fence is part of the
			// contract for those tools, and the JSON is inside it.
			body := string(resp.Result)
			if envelope, ok := promptguard.Parse(body); ok {
				if envelope.Origin != tc.name {
					t.Errorf("%s came back marked as coming from %q, and the marker has to name "+
						"the tool that produced it", tc.name, envelope.Origin)
				}
				body = envelope.Body
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				t.Fatalf("%s did not come back as a JSON object: %v (%s)", tc.name, err, body)
			}
			if len(payload) == 0 {
				t.Errorf("%s came back as an empty JSON object, so nothing was actually read", tc.name)
			}

			ev, ok := emitter.lastFor(tc.name)
			if !ok {
				t.Fatalf("%s ran and wrote no audit row", tc.name)
			}
			if got, ok := surfaceOf(ev); !ok || got != agentToolSurfaceBaseTool {
				t.Errorf("%s was served by the %q surface, and the BaseTool bridge is the only one "+
					"that carries this tool", tc.name, got)
			}
		})
	}
}

// The mirror of the middleware channel's write refusal.
//
// This channel has no approval queue, so a write that reached it would
// run immediately, attributed to a conversation nobody reviewed. The
// refusal has to say WHY — "this channel does not carry writes" and "there
// is no tool by that name" are different sentences, and only the second is
// worth chasing as a typo.
//
// The distinction is also the reason the bridge keeps the withheld set:
// without it, a node asking for a write tool would be told the tool does
// not exist, which is a lie about the control plane's inventory.
func TestAWriteOnTheBaseToolBridgeIsRefusedWithAReason(t *testing.T) {
	channel, emitter := newAgentToolChannel(t)
	stub := agentToolTunnelStub{channel: channel, edgeID: 7}

	writes := []string{
		configchange.ToolNameApplyConfigChange,
		"host_restart_service",
	}
	for _, name := range writes {
		resp := stub.call(t, tunnel.AgentToolRequest{Tool: name, Arguments: json.RawMessage(`{}`)})
		if resp.Error == "" {
			t.Errorf("%s ran on a channel that carries reads only", name)
			continue
		}
		if strings.Contains(resp.Error, "no such tool") {
			t.Errorf("%s was refused as an unknown tool rather than as a write this channel does "+
				"not carry: %s", name, resp.Error)
		}
		if !strings.Contains(resp.Error, "write-classed") {
			t.Errorf("%s was refused without saying it was refused because it writes: %s", name, resp.Error)
		}

		ev, ok := emitter.lastFor(name)
		if !ok {
			t.Errorf("%s was refused and wrote no audit row", name)
			continue
		}
		if ev.Status != auditport.StatusDenied {
			t.Errorf("%s was refused but the audit row records it as %q, and a refusal has to be "+
				"readable as one", name, ev.Status)
		}
	}
}

// The four read-class tools the bridge knows about and still will not
// serve.
//
// Each one is withheld for a reason that is written down, and this test
// is what keeps those reasons honest: a name in the withheld set with no
// reason is indistinguishable from a name somebody added to make a
// complaint go away, and the complaint comes back later as a bug.
func TestTheWithheldToolsSayWhyTheyAreNotServed(t *testing.T) {
	channel, _ := newAgentToolChannel(t)

	for name, reason := range baseToolUpcallReserved {
		if reason == "" {
			t.Errorf("%s is withheld with no reason recorded", name)
		}
		resp := agentToolTunnelStub{channel: channel, edgeID: 7}.call(t, tunnel.AgentToolRequest{
			Tool:      name,
			Arguments: json.RawMessage(`{}`),
		})
		if resp.Error == "" {
			t.Errorf("%s was served, and it is on the withheld list", name)
			continue
		}
		if !strings.Contains(resp.Error, name) {
			t.Errorf("the refusal for %s does not name it: %s", name, resp.Error)
		}
	}
}

// A name no surface carries is reported as an unknown tool, and the
// answer says the BaseTool surface is missing rather than claiming the
// control plane has no such capability.
//
// This is the wording the S-3 slice depends on: "no such tool" and "not
// configured here" are different sentences, and only the first is worth
// chasing as a typo.
func TestAToolNoSurfaceCarriesSaysSoRatherThanClaimingItDoesNotExist(t *testing.T) {
	reg, bag := buildTheRealToolRegistry(t)
	channel := &agentToolUpcall{
		reg:      reg,
		basetool: gapGateBridge(t, reg, bag),
		audit:    &agentToolAudit{emitter: &recordingAuditEmitter{}},
	}
	resp := agentToolTunnelStub{channel: channel, edgeID: 7}.call(t, tunnel.AgentToolRequest{
		Tool:      "a_tool_nobody_declared",
		Arguments: json.RawMessage(`{}`),
	})
	if resp.Error == "" {
		t.Fatal("a tool nobody declared was served")
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Errorf("the refusal %q does not say the tool does not exist", resp.Error)
	}
	if strings.Contains(resp.Error, "not wired") {
		t.Errorf("the refusal %q claims the BaseTool surface is not wired, and it is wired here", resp.Error)
	}
}

// The bridge answers before the closure registry does.
//
// The two surfaces hold the same capabilities in two shapes, and the
// shapes have already diverged: the closure form takes `edge_name`, the
// BaseTool form takes `device_ids[]`. The node's schema is generated from
// the BaseTool metadata, so the arguments a node sends are shaped for the
// BaseTool — and a closure registry answering first would make the tool
// "exist" and still fail on every call.
func TestTheBaseToolBridgeAnswersBeforeTheClosureRegistry(t *testing.T) {
	reg, bag := buildTheRealToolRegistry(t)
	// A closure registry that would answer the same name with the wrong
	// shape, if it were asked first.
	reg.Register(manageraiopstools.Tool{
		Name:        "find_topology_node",
		Description: "the closure form, which takes a different argument shape",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Execute: func(context.Context, json.RawMessage) (manageraiopstools.ExecuteResult, error) {
			return manageraiopstools.ExecuteResult{ResultJSON: json.RawMessage(`{"served_by":"closure"}`)}, nil
		},
	})

	channel := &agentToolUpcall{
		reg:      reg,
		basetool: gapGateBridge(t, reg, bag),
		audit:    &agentToolAudit{emitter: &recordingAuditEmitter{}},
	}
	resp := agentToolTunnelStub{channel: channel, edgeID: 7}.call(t, tunnel.AgentToolRequest{
		Tool:      topology.ToolNameFindTopologyNode,
		Arguments: json.RawMessage(`{"name":"checkout"}`),
	})
	if resp.Error != "" {
		t.Fatalf("%s was not served: %s", topology.ToolNameFindTopologyNode, resp.Error)
	}
	if strings.Contains(string(resp.Result), "closure") {
		t.Errorf("the closure registry answered %s, and the node's arguments are shaped for the "+
			"BaseTool form: %s", topology.ToolNameFindTopologyNode, resp.Result)
	}
}

// A name the product carries and this deployment did not wire says so,
// rather than claiming the tool does not exist.
//
// This is the third miss, and the one the two before it could not phrase:
// "not here" is the dispatch chain's signal to try the next surface, and
// "this channel will not run that" is a policy, but "the product has it
// and nobody wired its dependency" is a configuration gap with an operator
// on the other end. The node's manifest is a product fact — a node that
// installed a package declaring query_knowledge did not stop declaring it
// because this control plane wired no knowledge service — so "no such
// tool" would be a lie about the product, and a sentence nobody can act
// on.
//
// The two constructions below differ in exactly one field, and that is the
// whole test: the wiring struct carries every dependency, and the
// knowledge seam is the one whose absence takes query_knowledge out of the
// bag while leaving it in the product vocabulary.
func TestAToolTheProductHasButThisDeploymentDidNotWireSaysSo(t *testing.T) {
	wired := func(t *testing.T) *agentToolUpcall {
		t.Helper()
		reg, bag := buildTheRealToolRegistry(t)
		return &agentToolUpcall{
			reg:      reg,
			basetool: gapGateBridge(t, reg, bag),
			audit:    &agentToolAudit{emitter: &recordingAuditEmitter{}},
		}
	}
	// The unwired construction replays buildTheRealToolRegistry's wiring
	// minus the knowledge seam, because that seam is the one a deployment
	// leaves out when it runs no knowledge service — and BuildBaseTools'
	// nil-gating is what turns the missing seam into a missing bag entry
	// while KnownToolNames keeps the name in the vocabulary.
	unwired := func(t *testing.T) *agentToolUpcall {
		t.Helper()
		db := throwawayDB(t)
		log := newTestLogger(t)
		w := toolRegistryWiring{
			Caller:  stubCaller{},
			Edges:   stubEdgeCatalog{},
			Devices: &devicebiz.Usecase{},
			Prom:    stubPromQuerier{},
			Logs:    stubLogQuerier{},
			Trace:   stubTraceQuerier{},
			Alerts:  stubAlertUsecase{},
			Log:     log,
		}
		reg := w.buildRegistry()
		w.Approval = managerbizapproval.NewUsecase(managerapprovaldata.NewRepo(db), log)
		w.Channels = manageralertdata.NewRepo(db)
		w.Router = notify.NewFromConfig(config.NotificationConfig{}, log)
		w.DB = db
		w.LLM = stubCompleter{}
		w.PagesDir = t.TempDir()
		w.AuditLister = stubAuditLister{}
		w.EdgeChanges = stubEdgeChangeLister{}
		w.ConfigManager = stubConfigManager{}
		w.PluginConfig = stubPluginConfigLister{}
		w.TopologyGraph = stubTopologyGraph{}
		// Knowledge stays nil: the deployment wired no knowledge service.
		w.apply(reg)
		return &agentToolUpcall{
			reg:      reg,
			basetool: gapGateBridge(t, reg, reg.BuildBaseTools()),
			audit:    &agentToolAudit{emitter: &recordingAuditEmitter{}},
		}
	}

	// On the wired deployment the tool serves.
	resp := agentToolTunnelStub{channel: wired(t), edgeID: 7, tenant: "operator-7"}.call(t, tunnel.AgentToolRequest{
		Tool:      "query_knowledge",
		Arguments: json.RawMessage(`{"query":"tunnel reconnect backoff"}`),
	})
	if resp.Error != "" {
		t.Fatalf("query_knowledge was not served on a deployment that wired the knowledge "+
			"service: %s", resp.Error)
	}

	// On the unwired one the refusal names the configuration gap.
	resp = agentToolTunnelStub{channel: unwired(t), edgeID: 7}.call(t, tunnel.AgentToolRequest{
		Tool:      "query_knowledge",
		Arguments: json.RawMessage(`{"query":"tunnel reconnect backoff"}`),
	})
	if resp.Error == "" {
		t.Fatal("query_knowledge was served by a deployment that wired no knowledge service")
	}
	if !strings.Contains(resp.Error, "not configured") {
		t.Errorf("the refusal %q does not say the tool is a configuration gap on this "+
			"deployment, and that is the sentence an operator can act on", resp.Error)
	}
	if strings.Contains(resp.Error, "no such tool") {
		t.Errorf("the refusal %q claims the product has no query_knowledge, and the node's "+
			"manifest declares it: the name is a product fact, not a deployment one", resp.Error)
	}
	// The audit row records the refusal as a denial with a reason, because
	// a node probing a configuration gap and a node probing a typo are two
	// different investigations, and only the first has a next step.
	channel := unwired(t)
	emitter := &recordingAuditEmitter{}
	channel.audit = &agentToolAudit{emitter: emitter}
	agentToolTunnelStub{channel: channel, edgeID: 7}.call(t, tunnel.AgentToolRequest{
		Tool: "query_knowledge",
	})
	ev, ok := emitter.lastFor("query_knowledge")
	if !ok {
		t.Fatal("query_knowledge was refused and wrote no audit row")
	}
	if ev.Status != auditport.StatusDenied {
		t.Errorf("query_knowledge was refused but the audit row records it as %q, and a "+
			"configuration gap has to be readable as a refusal", ev.Status)
	}
}

// A name outside the product vocabulary is still "no such tool".
//
// The third miss is deliberately narrow: it fires only for names the
// registry's KnownToolNames carries. A name nobody knows is a typo, and
// chasing it as a configuration gap would send an operator hunting for a
// dependency a tool never had.
func TestAToolOutsideTheProductVocabularyIsStillNoSuchTool(t *testing.T) {
	reg, bag := buildTheRealToolRegistry(t)
	channel := &agentToolUpcall{
		reg:      reg,
		basetool: gapGateBridge(t, reg, bag),
		audit:    &agentToolAudit{emitter: &recordingAuditEmitter{}},
	}
	resp := agentToolTunnelStub{channel: channel, edgeID: 7}.call(t, tunnel.AgentToolRequest{
		Tool: "a_tool_nobody_declared",
	})
	if resp.Error == "" {
		t.Fatal("a tool nobody declared was served")
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Errorf("the refusal %q does not say the tool does not exist, and a name outside "+
			"the product vocabulary is exactly that", resp.Error)
	}
	if strings.Contains(resp.Error, "not configured") {
		t.Errorf("the refusal %q phrases a typo as a configuration gap, and the two are "+
			"different investigations", resp.Error)
	}
}

// The audit row records the surface that served the call.
//
// The surface is read off the dispatch rather than inferred from the tool
// name, and this is the test that says so: the two surfaces hold the same
// capability under one name, so a ledger that guessed from the name would
// be wrong half the time.
//
// The closure-only tool below is registered on purpose. Every capability
// the closure registry carries also exists as a BaseTool, and the bridge
// answers first because the node's schema is generated from the BaseTool
// metadata — so a name the bag does not carry is the only way to reach
// the closure surface at all.
func TestTheAuditRowNamesTheSurfaceThatServedTheCall(t *testing.T) {
	channel, emitter := newAgentToolChannel(t)
	channel.reg.Register(manageraiopstools.Tool{
		Name:        "a_closure_only_tool",
		Description: "a capability that exists only in the closure registry",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Execute: func(context.Context, json.RawMessage) (manageraiopstools.ExecuteResult, error) {
			return manageraiopstools.ExecuteResult{ResultJSON: json.RawMessage(`{"served_by":"closure"}`)}, nil
		},
	})
	stub := agentToolTunnelStub{channel: channel, edgeID: 7}

	stub.call(t, tunnel.AgentToolRequest{Tool: "a_closure_only_tool", Arguments: json.RawMessage(`{}`)})
	stub.call(t, tunnel.AgentToolRequest{Tool: "find_topology_node", Arguments: json.RawMessage(`{"name":"checkout"}`)})

	closure, ok := emitter.lastFor("a_closure_only_tool")
	if !ok {
		t.Fatal("a_closure_only_tool ran and wrote no audit row")
	}
	if got, ok := surfaceOf(closure); !ok || got != agentToolSurfaceAiops {
		t.Errorf("a_closure_only_tool was served by the %q surface, and only the closure "+
			"registry carries it", got)
	}
	bridge, ok := emitter.lastFor("find_topology_node")
	if !ok {
		t.Fatal("find_topology_node ran and wrote no audit row")
	}
	if got, ok := surfaceOf(bridge); !ok || got != agentToolSurfaceBaseTool {
		t.Errorf("find_topology_node was served by the %q surface, and it is a BaseTool-only tool", got)
	}
}

// The change-events tool is the one gap tool whose dependencies are set
// on the registry rather than in its constructor, so it is the one that
// disappears when the wiring runs too early.
//
// It is called out here because the bridge is built after the wiring
// precisely so that this tool is present: build the bag first and the
// bridge comes up holding everything that does not need a late dependency,
// which is a bridge that looks wired and answers "no such tool" for
// exactly the tools that were missing.
func TestTheAuditSeamToolIsOnTheBridge(t *testing.T) {
	channel, _ := newAgentToolChannel(t)
	names := channel.basetool.serviceableNames()
	for _, want := range []string{
		alerting.ToolNameQueryChangeEvents,
		topology.ToolNameExpandTopology,
		topology.ToolNameFindTopologyNode,
	} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is not on the bridge, and it is served by no other surface: the bridge "+
				"holds %d tools", want, len(names))
		}
	}
}

package opskeepersreautonomy

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// stubHost is a stand-in for the edge's broker: a unix socket that speaks
// the line protocol and answers whatever the test scripted.
//
// It is the same fixture the other two toolsets use, deliberately. Both
// carry the same client, and the properties tested here — framing,
// deadlines, the refusal to resend an unknown outcome — are properties of
// that client rather than of the tool list in front of it.
//
// A real socket rather than a mock client, so the framing, the reconnect
// path and the deadline handling are exercised: those only exist between
// two processes.
type stubHost struct {
	mu       sync.Mutex
	requests []wire.ToolRequest
	// reply is the answer for each call in turn; the last one repeats.
	replies []wire.ToolReply
	// dropFirst makes the FIRST connection hang up after dropAfter calls,
	// modelling a host that restarted: the old socket is dead, a fresh
	// dial is served normally. Zero means never drop.
	dropAfter int
	conns     int
	path      string
	ln        net.Listener
	done      chan struct{}
}

func newStubHost(t *testing.T, replies ...wire.ToolReply) *stubHost {
	t.Helper()
	// Not t.TempDir(): a unix socket path is capped near 104 bytes and a
	// test-scoped temp dir plus a long test name runs past it.
	dir, err := os.MkdirTemp("", "sre")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "tool.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h := &stubHost{replies: replies, path: path, ln: ln, done: make(chan struct{})}
	go h.serve()
	t.Cleanup(func() {
		close(h.done)
		_ = ln.Close()
	})
	return h
}

func (h *stubHost) serve() {
	for {
		conn, err := h.ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		h.conns++
		h.mu.Unlock()
		go h.handle(conn)
	}
}

func (h *stubHost) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return
		}
		var req wire.ToolRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, req)
		n := len(h.requests)
		onFirst := h.conns == 1
		drop := h.dropAfter > 0 && onFirst && n > h.dropAfter
		var reply wire.ToolReply
		if len(h.replies) > 0 {
			idx := n - 1
			if idx >= len(h.replies) {
				idx = len(h.replies) - 1
			}
			reply = h.replies[idx]
		}
		h.mu.Unlock()
		if drop {
			return
		}
		body, err := json.Marshal(reply)
		if err != nil {
			return
		}
		if _, err := conn.Write(append(body, '\n')); err != nil {
			return
		}
	}
}

func (h *stubHost) seen() []wire.ToolRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]wire.ToolRequest, len(h.requests))
	copy(out, h.requests)
	return out
}

// routerAt builds a router pointed at a stub host, with the session lookup
// stubbed so the tests need no agent runtime.
func routerAt(path string) *router { return &router{socketPath: path} }

func TestTheInventoryIsSortedUniqueAndNonEmpty(t *testing.T) {
	names := ToolNames()
	if len(names) != len(tools) {
		t.Fatalf("ToolNames returned %d names for %d tools", len(names), len(tools))
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("the inventory is not sorted, so a version diff lies: %v", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			t.Error("a tool has a blank name")
		}
		if seen[n] {
			t.Errorf("%q appears twice", n)
		}
		seen[n] = true
	}
}

func TestTheInventoryIsExactlyOneTool(t *testing.T) {
	// A second tool in this package is a second way for a model to ask a
	// node to change something with nobody watching. The safety argument
	// is a closed list of signed argument vectors plus a tool that points
	// into it; a second tool is the beginning of a shape the model fills
	// in, and it would be reviewed as an increment rather than as the
	// change it is. So the count is asserted, not merely documented.
	if got := len(tools); got != 1 {
		t.Errorf("this package contributes %d tools, want 1: %v", got, ToolNames())
	}
}

func TestEveryToolCarriesAUsableSchemaAndDescription(t *testing.T) {
	// A tool with no schema is a tool the model calls with anything and
	// the host rejects; a tool with no description is a tool the model
	// never calls. Both are silent failures, so they are checked here
	// rather than discovered in an incident.
	for _, spec := range tools {
		t.Run(spec.Name, func(t *testing.T) {
			if strings.TrimSpace(spec.Description) == "" {
				t.Error("no description: the model has no reason to call this")
			}
			if strings.TrimSpace(spec.Label) == "" {
				t.Error("no label: the console shows a blank row")
			}
			schema := schemaOf(spec)
			if schema["type"] != "object" {
				t.Errorf("schema type = %v, want object", schema["type"])
			}
			if _, ok := schema["properties"]; !ok {
				t.Error("schema has no properties key")
			}
		})
	}
}

func TestTheToolTakesNoCommandParameter(t *testing.T) {
	// The load-bearing assertion in this package.
	//
	// A tool whose schema has a `command` or `argv` property is a remote
	// shell with an approval policy attached, whatever its description
	// says. The distinction that makes autonomy safe is not "the model is
	// asked nicely to name an action" — it is that the schema has no
	// parameter through which a command can be written at all, so the
	// host resolves a name against a signed manifest and runs the
	// manifest's own vector. A schema edit that adds one is a total loss
	// of that property, and nothing else in the build would notice.
	//
	// The check is on the property *names* the schema admits, including
	// the fact that the object is closed. `additionalProperties: false`
	// matters as much as the list: without it a model can send a
	// `command` key that the schema never mentioned and a lenient host
	// would find it useful.
	for _, spec := range tools {
		t.Run(spec.Name, func(t *testing.T) {
			schema := schemaOf(spec)
			props, _ := schema["properties"].(map[string]any)
			banned := []string{
				"command", "cmd", "argv", "args", "arguments", "script",
				"shell", "exec", "run", "bin", "binary", "path", "file",
			}
			for _, bad := range banned {
				if _, ok := props[bad]; ok {
					t.Errorf("the schema admits a %q property: the model would be able to write the "+
						"command instead of naming a declared action, which is the one thing this "+
						"tool must not let it do", bad)
				}
			}
			// Anything that is not a plain string selector is suspect for
			// the same reason, so the shape of what remains is checked
			// rather than only the names that were banned.
			for name, raw := range props {
				p, _ := raw.(map[string]any)
				if p["type"] != "string" {
					t.Errorf("property %q has type %v; every parameter of this tool is a name the "+
						"host resolves, so none of them should be anything but a string", name, p["type"])
				}
			}
			// `additionalProperties: false` is the *value* that closes the
			// object, so the assertion is that it is present and false —
			// a schema that omits it entirely leaves the object open.
			if extra, ok := schema["additionalProperties"].(bool); !ok || extra {
				t.Errorf("additionalProperties = %v, want it present and false: an open object lets "+
					"a model send properties the schema never declared", schema["additionalProperties"])
			}
		})
	}
}

func TestTheDescriptionSaysTheModelPicksANameRatherThanACommand(t *testing.T) {
	// The schema closes the door; the description is what the model reads
	// when it has already decided what to do. A model that believes the
	// tool takes a command will not call it, and a model that believes the
	// tool bypasses approvals will call it when it should not. Both are
	// failures of the same sentence, so the sentence is asserted.
	var desc string
	for _, spec := range tools {
		if spec.Name == "host_autonomy_run" {
			desc = spec.Description
		}
	}
	if desc == "" {
		t.Fatal("host_autonomy_run is not in the inventory, so this test asserts about nothing")
	}
	lower := strings.ToLower(desc)
	for _, want := range []string{"mutating", "declared", "approval"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the description never mentions %q, so the model is not told the thing it "+
				"most needs to know before calling: %q", want, desc)
		}
	}
}

func TestAMutatingToolTellsTheModelNotToRetry(t *testing.T) {
	// If the host stops answering after a self-heal was requested, the
	// model is told the outcome is unknown and must not send the request
	// again. A retry here is a second action on a live host, and unlike a
	// restart it is not merely redundant: the host spends the action's
	// idempotency key when it adjudicates, so a retry is a claim about an
	// occurrence that has already been used.
	r := routerAt(filepath.Join(t.TempDir(), "absent.sock"))

	_, err := r.run("s", "host_autonomy_run", nil)
	if err == nil {
		t.Fatal("an unreachable host reported success")
	}
	for _, want := range []string{
		"host_autonomy_run",
		"could not be reached",
		"operator",
		"Do not retry",
		"second action",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestAnAgentWithNoSocketSaysSoRatherThanPretending(t *testing.T) {
	r := routerAt("")
	if _, err := r.run("s", "host_autonomy_run", nil); err == nil {
		t.Fatal("an agent with no broker socket reported success")
	} else if !strings.Contains(err.Error(), wire.ToolSocketEnv) {
		t.Errorf("error = %q, want it to name the environment variable the operator has to set", err)
	}
}

func TestACallIsCarriedToTheHostWithTheNameBoundAtRegistration(t *testing.T) {
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"verdict":"defer"}`)})
	r := routerAt(host.path)

	// The model asks for one action; the host is asked about the tool the
	// package registered, not about the name in the arguments. A tool
	// dispatched by a model-supplied name is a tool that can be talked
	// into running a different one.
	_, err := r.run("sess-1", "host_autonomy_run", map[string]any{
		"action": "restart_orders",
		"target": "orders-api",
		"window": "inc-42",
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	seen := host.seen()
	if len(seen) != 1 {
		t.Fatalf("the host saw %d calls, want 1", len(seen))
	}
	if seen[0].ToolName != "host_autonomy_run" {
		t.Errorf("host saw tool %q, want host_autonomy_run", seen[0].ToolName)
	}
	if seen[0].SessionID != "sess-1" {
		t.Errorf("host saw session %q, want sess-1", seen[0].SessionID)
	}
	// The three declared parameters travel as the model proposed them and
	// nothing else. If a command could ride along here it would be a
	// field this assertion would have to be taught about.
	if got := len(seen[0].Arguments); got != 3 {
		t.Errorf("the host received %d arguments, want 3: %v", got, seen[0].Arguments)
	}
}

func TestARefusalFromTheHostReachesTheModelInTheHostsOwnWords(t *testing.T) {
	// A deferral is the most valuable answer this tool gives: it is how
	// the model learns that asking does not bypass a human while a human
	// is present. So it is carried through verbatim rather than being
	// folded into a generic failure the model might retry.
	host := newStubHost(t, wire.ToolReply{
		Error: "the control plane is reachable, so restart_orders goes to the approval gate like any other call",
	})
	r := routerAt(host.path)

	_, err := r.run("s", "host_autonomy_run", map[string]any{"action": "restart_orders"})
	if err == nil {
		t.Fatal("a refusal from the host reported success")
	}
	if !strings.Contains(err.Error(), "approval gate") {
		t.Errorf("error = %q, want the host's own explanation to survive to the model", err)
	}
}

func TestARunIsReturnedDecodedRatherThanAsBytes(t *testing.T) {
	// The agent renders whatever it gets, and a decoded value renders the
	// same way in the transcript, in a tool card and in a packed run. An
	// opaque blob renders as a string in one of them.
	host := newStubHost(t, wire.ToolReply{
		Result: json.RawMessage(`{"verdict":"run","ran":true,"exit_code":0}`),
	})
	r := routerAt(host.path)

	got, err := r.run("s", "host_autonomy_run", map[string]any{"action": "restart_orders"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	res, ok := got.(sdk.ToolResult)
	if !ok {
		t.Fatalf("result is %T, want sdk.ToolResult", got)
	}
	if !strings.Contains(res.Content, `"verdict":"run"`) {
		t.Errorf("content = %q, want the host's verdict as the result text", res.Content)
	}
	m, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("details is %T, want a decoded object", res.Details)
	}
	if m["verdict"] != "run" || m["ran"] != true {
		t.Errorf("result = %v, want the host's verdict carried through", m)
	}
}

func TestAResultThatIsNotJSONIsAFailureRatherThanSilence(t *testing.T) {
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`not json`)})
	r := routerAt(host.path)

	if _, err := r.run("s", "host_autonomy_run", nil); err == nil {
		t.Fatal("an unreadable result reported success")
	}
}

func TestADroppedConnectionIsNotBlindlyResent(t *testing.T) {
	// The request reached the host and the host then stopped answering.
	// Whether the action ran is unknowable from here, and the action is
	// spent by its idempotency key the moment it was adjudicated, so a
	// resend is a second action rather than a repeat of the first.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"verdict":"run"}`)})
	// The stub answers the first call and hangs up on the next, so the
	// lost one is the second: the request is delivered, the reply is not.
	host.dropAfter = 1
	r := routerAt(host.path)

	if _, err := r.run("s", "host_autonomy_run", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := r.run("s", "host_autonomy_run", nil)
	if err == nil {
		t.Fatal("a self-heal whose reply never arrived reported success")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error = %q, want it to say the outcome is unknown rather than invite a retry", err)
	}
	// Exactly two, not "at most two": the first is the answered call and
	// the second is the lost one. A third would be a resend of a request
	// the host may already have adjudicated — and adjudication is what
	// spends the idempotency key.
	if got := len(host.seen()); got != 2 {
		t.Errorf("the host saw %d calls, want 2: a call whose outcome is unknown must not be resent",
			got)
	}
}

func TestTheClientRecoversOnTheNextCallAfterADrop(t *testing.T) {
	// The refusal above is per call, not permanent. A node whose host
	// restarted is a node that needs its agent most, and deafening it for
	// the rest of the turn would be the worst possible time.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"verdict":"run"}`)})
	host.dropAfter = 1
	r := routerAt(host.path)

	if _, err := r.run("s", "host_autonomy_run", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := r.run("s", "host_autonomy_run", nil); err == nil {
		t.Fatal("the call that hit the dropped connection should have failed")
	}
	if _, err := r.run("s", "host_autonomy_run", nil); err != nil {
		t.Fatalf("call after recovery: %v", err)
	}
}

package opskeeperobservability

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
// It is a real socket rather than a mock client so the tests exercise the
// framing, the reconnect path and the deadline handling — the parts that
// only exist between two processes. The fixture is the same one the
// read-only and repair toolsets use, deliberately: the properties under
// test here are properties of the shared client, and the tool list in
// front of it is not what makes them true.
type stubHost struct {
	mu       sync.Mutex
	requests []wire.ToolRequest
	// reply is the answer for each call in turn; the last one repeats.
	replies []wire.ToolReply
	// dropAfter makes the FIRST connection hang up after that many calls,
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
	dir, err := os.MkdirTemp("", "obs")
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
func routerAt(path string) *router {
	return &router{socketPath: path}
}

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

// TestNoToolInThisPackageIsNamedLikeAShell is the assertion that keeps
// this package from growing into a remote-code-execution surface.
//
// host_bash is in the node's own registry and is deliberately never in a
// manifest, but this package is generated from a control-plane registry
// and the two registries are maintained separately. If a shell ever
// appeared on the control-plane side, a generated inventory would carry it
// here and the manifest would be the only thing standing between it and a
// node — which is a single human reading a YAML diff, once.
func TestNoToolInThisPackageIsNamedLikeAShell(t *testing.T) {
	for _, name := range ToolNames() {
		lower := strings.ToLower(name)
		for _, forbidden := range []string{"bash", "shell", "exec", "eval", "command", "upload", "download"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("tool %q is in a read-only package; a name that reads as arbitrary code "+
					"execution does not belong in an L1 manifest reviewed by a human", name)
			}
		}
	}
}

func TestEveryToolInThisPackageIsServedByTheHostAndNotResolvedHere(t *testing.T) {
	// The design claim, checked at the only place it could be broken: this
	// package has no implementation, so a tool that answered locally would
	// mean a dispatch path had been added to it. The router has exactly
	// one route, and these tests exercise it for every name in the
	// inventory rather than for a hand-picked few.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"ok":true}`)})
	r := routerAt(host.path)

	for _, name := range ToolNames() {
		if _, err := r.run("sess-1", name, map[string]any{}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	seen := host.seen()
	if len(seen) != len(tools) {
		t.Fatalf("the host saw %d calls for %d tools", len(seen), len(tools))
	}
	for i, req := range seen {
		if req.ToolName != tools[i].Name {
			t.Errorf("call %d was %q, want %q; the router must send the name it was bound to", i, req.ToolName, tools[i].Name)
		}
	}
}

func TestACallIsCarriedToTheHostWithItsArgumentsUnaltered(t *testing.T) {
	// The name is bound in Extension(), not read from the call, so a model
	// cannot talk this toolset into running a different tool. And the
	// arguments have to arrive as the model wrote them: a dispatcher that
	// re-encoded a PromQL expression or a LogQL selector would produce a
	// different query, which the metric store would answer cheerfully.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"result_type":"vector"}`)})
	r := routerAt(host.path)

	const expr = `sum by (job) (rate(http_requests_total{code=~"5.."}[5m]))`
	out, err := r.run("sess-9", "query_promql", map[string]any{"query": expr})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	seen := host.seen()
	if len(seen) != 1 {
		t.Fatalf("the host saw %d calls, want 1", len(seen))
	}
	if seen[0].ToolName != "query_promql" {
		t.Errorf("tool = %q, want query_promql", seen[0].ToolName)
	}
	if seen[0].SessionID != "sess-9" {
		t.Errorf("session = %q, want the conversation the host attributes the query to", seen[0].SessionID)
	}
	if seen[0].Arguments["query"] != expr {
		t.Errorf("arguments = %v, want the model's query carried through unaltered", seen[0].Arguments)
	}

	res, ok := out.(sdk.ToolResult)
	if !ok {
		t.Fatalf("result is %T, want sdk.ToolResult", out)
	}
	body, ok := res.Details.(map[string]any)
	if !ok || body["result_type"] != "vector" {
		t.Errorf("details = %#v, want the host's answer decoded", res.Details)
	}
}

func TestARefusalFromTheHostReachesTheModelInTheHostsOwnWords(t *testing.T) {
	// A refusal is a decision, not a fault, and the model has to be able
	// to tell them apart — a model that cannot tell a refusal from an
	// error retries it. So the host's sentence is passed through intact.
	host := newStubHost(t, wire.ToolReply{
		Error: `query_promql is not in the allow-list of the package installed on this node`,
	})
	r := routerAt(host.path)

	_, err := r.run("s", "query_promql", nil)
	if err == nil {
		t.Fatal("a refusal reported success")
	}
	for _, want := range []string{"query_promql", "not in the allow-list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to carry %q", err.Error(), want)
		}
	}
}

func TestAnUnreachableHostTellsTheModelNotToRetry(t *testing.T) {
	// Every tool here is a read, so a resent call costs a turn rather than
	// a system — but the instruction not to retry is kept for the reason
	// the repair toolset gives it. A model that learns on a read that a
	// lost reply means try again carries that lesson into a restart on the
	// same node, and the lesson is what causes the second outage.
	r := routerAt(filepath.Join(t.TempDir(), "absent.sock"))

	_, err := r.run("s", "query_promql", nil)
	if err == nil {
		t.Fatal("an unreachable host reported success")
	}
	for _, want := range []string{"query_promql", "could not be reached", "Do not retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err.Error(), want)
		}
	}
}

func TestADroppedConnectionIsNotBlindlyResent(t *testing.T) {
	// The host hangs up after receiving the query. Whether it ran is
	// unknowable from here. Resending is cheaper than a wrong answer for a
	// read, and the client still does not do it, because the refusal to
	// resend an unknown outcome is the client's only interesting property
	// and a read is where it is easiest to be tempted to relax it.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"ok":true}`)})
	host.dropAfter = 1
	r := routerAt(host.path)

	if _, err := r.run("s", "query_promql", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := r.run("s", "query_promql", nil)
	if err == nil {
		t.Fatal("a query whose reply never arrived reported success")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error = %q, want it to say the outcome is unknown rather than retry", err)
	}
}

func TestTheClientRecoversOnTheNextCallAfterADrop(t *testing.T) {
	// One dropped edge must not deafen the node until it is restarted,
	// which is exactly when a node needs its agent most.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"ok":true}`)})
	host.dropAfter = 1
	r := routerAt(host.path)

	if _, err := r.run("s", "query_promql", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := r.run("s", "query_promql", nil); err == nil {
		t.Fatal("expected the call that hit the dropped connection to fail")
	}
	if _, err := r.run("s", "query_promql", nil); err != nil {
		t.Fatalf("call after recovery: %v", err)
	}
}

func TestAnAgentWithNoSocketSaysSoRatherThanPretending(t *testing.T) {
	// Extension() reads the socket at load, so an agent started without
	// one still registers its tools — and every call explains the missing
	// configuration rather than failing obscurely. An agent that can
	// discuss a fleet query and perform none is the correct failure here.
	r := &router{socketPath: ""}

	_, err := r.run("", "query_promql", nil)
	if err == nil {
		t.Fatal("a call with no socket reported success")
	}
	if !strings.Contains(err.Error(), wire.ToolSocketEnv) {
		t.Errorf("error = %q, want it to name the environment variable that is unset", err.Error())
	}
}

func TestAnEmptyResultIsAnEmptyObjectRatherThanNothing(t *testing.T) {
	// A tool that returned no bytes would be rendered by the agent as an
	// empty tool card, which reads as a failed call. A read that found
	// nothing found nothing, and that is a legitimate answer.
	host := newStubHost(t, wire.ToolReply{})
	r := routerAt(host.path)

	out, err := r.run("s", "query_traceql", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res, ok := out.(sdk.ToolResult)
	if !ok {
		t.Fatalf("result is %T, want sdk.ToolResult", out)
	}
	if strings.TrimSpace(res.Content) != "{}" {
		t.Errorf("content = %q, want an empty object rather than nothing", res.Content)
	}
}

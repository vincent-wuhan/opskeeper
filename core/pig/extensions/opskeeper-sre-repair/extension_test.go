package opskeeperrepair

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

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// stubHost is a stand-in for the edge's broker: a unix socket that speaks
// the line protocol and answers whatever the test scripted.
//
// It is the same fixture the read-only toolset uses, deliberately. Both
// toolsets carry the same client, and the properties being tested here —
// framing, deadlines, the refusal to resend an unknown outcome — are
// properties of that client rather than of the tool list in front of it.
//
// It is a real socket rather than a mock client so the tests exercise the
// framing, the reconnect path and the deadline handling — the parts that
// only exist between two processes.
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

// routerAt builds a router pointed at a stub host, with the session
// lookup stubbed so the tests need no agent runtime.
func routerAt(path, _ string) *router {
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

// mutatingTools are the names this package exists to offer. The list is
// written out here rather than derived from a class field, because the
// point of the assertions below is that the class is not something the
// package asserts about itself — the host decides that from the manifest
// and from the executor it finds. Spelling the names out keeps the test
// asking a question of the table rather than agreeing with it.
var mutatingTools = []string{
	"apply_config_change",
	"host_restart_service",
	"recovery.execute",
}

func TestEveryMutatingToolSaysWhatHappensBeforeItRuns(t *testing.T) {
	// The model is the only thing standing between a plan and a restart,
	// and it is also the only thing that can warn the operator before the
	// approval prompt appears. A mutating tool whose description does not
	// mention that a human is asked produces a conversation in which the
	// approval looks like an obstacle the model hit rather than a step it
	// told the user about. Operators approve what they were warned about.
	for _, name := range mutatingTools {
		t.Run(name, func(t *testing.T) {
			var desc string
			for _, spec := range tools {
				if spec.Name == name {
					desc = spec.Description
				}
			}
			if desc == "" {
				t.Fatalf("%q is not in the inventory, so this test is asserting about nothing", name)
			}
			lower := strings.ToLower(desc)
			if !strings.Contains(lower, "mutating") && !strings.Contains(lower, "approval") &&
				!strings.Contains(lower, "confirm") && !strings.Contains(lower, "approved") {
				t.Errorf("the description of the mutating tool %q never says it changes something "+
					"or that a human is involved: %q", name, desc)
			}
		})
	}
}

func TestAMutatingToolTellsTheModelNotToRetry(t *testing.T) {
	// The one message this package must never get wrong. If the host
	// stops answering after a restart was requested, the model is told
	// the outcome is unknown and must not send the request again. A retry
	// here is a second outage, caused by the agent being helpful.
	r := routerAt(filepath.Join(t.TempDir(), "absent.sock"), "s")

	_, err := r.run("s", "host_restart_service", nil)
	if err == nil {
		t.Fatal("an unreachable host reported success")
	}
	for _, want := range []string{
		"host_restart_service",
		"could not be reached",
		"operator",
		"Do not retry",
		"may be a second action",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestACallIsCarriedToTheHostWithTheNameBoundAtRegistration(t *testing.T) {
	// The name is bound in Extension(), not read from the call, so a model
	// cannot talk this toolset into running a different one. The host
	// looks up whatever this package registered; the model's idea of which
	// tool it called has no path to the dispatch.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"restarted":true,"service":"nginx"}`)})
	r := routerAt(host.path, "s")

	out, err := r.run("sess-9", "host_restart_service", map[string]any{
		"device_id": float64(42),
		"service":   "nginx",
		"reason":    "worker pool exhausted, confirmed by cpu saturation over 10m",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	seen := host.seen()
	if len(seen) != 1 {
		t.Fatalf("the host saw %d calls, want 1", len(seen))
	}
	if seen[0].ToolName != "host_restart_service" {
		t.Errorf("tool = %q, want host_restart_service", seen[0].ToolName)
	}
	if seen[0].SessionID != "sess-9" {
		t.Errorf("session = %q, want the conversation the host attributes the call to", seen[0].SessionID)
	}
	if seen[0].Arguments["service"] != "nginx" {
		t.Errorf("arguments = %v, want the model's proposal carried through unaltered", seen[0].Arguments)
	}

	body, ok := out.(map[string]any)
	if !ok || body["restarted"] != true {
		t.Errorf("result = %#v, want the host's restart envelope decoded", out)
	}
}

func TestARefusalFromTheHostReachesTheModelInTheHostsOwnWords(t *testing.T) {
	// A refusal is a decision, not a fault, and the model has to be able
	// to tell them apart — a model that cannot tell a refusal from an
	// error retries it. So the host's sentence is passed through intact.
	host := newStubHost(t, wire.ToolReply{
		Error: `host_restart_service needs an operator's approval, and no approval for this exact call was given`,
	})
	r := routerAt(host.path, "s")

	_, err := r.run("s", "host_restart_service", nil)
	if err == nil {
		t.Fatal("a refusal reported success")
	}
	if !strings.Contains(err.Error(), "host_restart_service") {
		t.Errorf("error = %q, want it to name the tool that was refused", err.Error())
	}
	if !strings.Contains(err.Error(), "no approval for this exact call") {
		t.Errorf("error = %q, want it to carry the host's own reason verbatim", err.Error())
	}
}

func TestADroppedConnectionIsNotBlindlyResent(t *testing.T) {
	// This is the test the whole client exists for, and it matters more
	// here than anywhere else. The host hangs up after receiving the
	// restart request. Whether it ran is unknowable from here. Resending
	// would be a second restart of a service that may already be down.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"restarted":true}`)})
	host.dropAfter = 1
	r := routerAt(host.path, "s")

	if _, err := r.run("s", "host_restart_service", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}

	_, err := r.run("s", "host_restart_service", nil)
	if err == nil {
		t.Fatal("a restart whose reply never arrived reported success")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error = %q, want it to say the outcome is unknown rather than retry", err)
	}
	if seen := host.seen(); len(seen) > 2 {
		t.Errorf("the host saw %d restart requests, want the lost one not to be resent blindly", len(seen))
	}
}

func TestTheClientRecoversOnTheNextCallAfterADrop(t *testing.T) {
	// One dropped edge must not deafen the node until it is restarted,
	// which is exactly when a node needs its agent most. The connection
	// that failed is dropped, so the next call redials.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"restarted":true}`)})
	host.dropAfter = 1
	r := routerAt(host.path, "s")

	if _, err := r.run("s", "host_restart_service", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := r.run("s", "host_restart_service", nil); err == nil {
		t.Fatal("expected the call that hit the dropped connection to fail")
	}
	if _, err := r.run("s", "host_restart_service", nil); err != nil {
		t.Fatalf("call after recovery: %v", err)
	}
}

func TestAnAgentWithNoSocketSaysSoRatherThanPretending(t *testing.T) {
	// Extension() reads the socket at load, so an agent started without
	// one still registers its tools — and every call explains the missing
	// configuration rather than failing obscurely. The correct failure
	// here is an agent that can discuss a repair and perform none.
	r := &router{socketPath: ""}

	_, err := r.run("", "host_restart_service", nil)
	if err == nil {
		t.Fatal("a call with no socket reported success")
	}
	if !strings.Contains(err.Error(), wire.ToolSocketEnv) {
		t.Errorf("error = %q, want it to name the environment variable that is unset", err)
	}
}

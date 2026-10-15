package opskeepersre

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

func TestACallIsCarriedToTheHostAndItsResultReturned(t *testing.T) {
	host := newStubHost(t, wire.ToolReply{
		Result: json.RawMessage(`{"entries":[{"level":"err","message":"Out of memory: Killed"}]}`),
	})
	r := routerAt(host.path, "sess-1")

	out, err := r.run("sess-1", "host_dmesg", map[string]any{"levels": "err"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The payload is handed back in the shape the agent runtime reads a
	// tool result in -- content plus details -- not in the shape the tool
	// happens to produce. Answering with the tool's own object is decoded
	// into a result with no content at all, and the model is told
	// "(no tool output)".
	res, ok := out.(sdk.ToolResult)
	if !ok {
		t.Fatalf("result is %T, want sdk.ToolResult", out)
	}
	if !strings.Contains(res.Content, `"message":"Out of memory: Killed"`) {
		t.Errorf("content = %q, want the host's payload as the result text", res.Content)
	}
	body, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("details is %T, want a decoded object", res.Details)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %#v, want one entry", body["entries"])
	}

	seen := host.seen()
	if len(seen) != 1 {
		t.Fatalf("the host saw %d calls, want 1", len(seen))
	}
	if seen[0].ToolName != "host_dmesg" {
		t.Errorf("tool = %q, want host_dmesg", seen[0].ToolName)
	}
	if seen[0].SessionID != "sess-1" {
		t.Errorf("session = %q, want the session the agent is serving", seen[0].SessionID)
	}
	if seen[0].Arguments["levels"] != "err" {
		t.Errorf("arguments = %#v, want the model's own values", seen[0].Arguments)
	}
}

func TestTheNameSentIsTheOneBoundAtRegistration(t *testing.T) {
	// The router binds the name when the tool is built, so nothing the
	// model sends can reach a different tool. Proved here by driving the
	// same router with two different names and checking the host saw each.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{}`)})
	r := routerAt(host.path, "s")

	if _, err := r.run("s", "host_lsof", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := r.run("s", "host_strace", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	seen := host.seen()
	if len(seen) != 2 || seen[0].ToolName != "host_lsof" || seen[1].ToolName != "host_strace" {
		t.Errorf("the host saw %+v, want each call under its own name", seen)
	}
}

func TestARefusalFromTheHostReachesTheModelInTheHostsOwnWords(t *testing.T) {
	// A refusal is a decision, not a fault. The model has to be able to
	// read it and change course rather than retry the same thing.
	host := newStubHost(t, wire.ToolReply{
		Error: "host_reboot is not in this node's tool set; the host permits host_dmesg",
	})
	r := routerAt(host.path, "s")

	_, err := r.run("s", "host_reboot", nil)
	if err == nil {
		t.Fatal("a refused call reported success")
	}
	if !strings.Contains(err.Error(), "not in this node's tool set") {
		t.Errorf("error = %q, want the host's own reason carried through", err)
	}
	if !strings.Contains(err.Error(), "host_reboot") {
		t.Errorf("error = %q, want it to name the tool that was refused", err)
	}
}

func TestAnUnreachableHostIsReportedInTermsTheModelCanActOn(t *testing.T) {
	// "connection refused" tells the model nothing it can do. The point of
	// the message is that it names the situation and says what to do next.
	r := routerAt(filepath.Join(t.TempDir(), "absent.sock"), "s")

	_, err := r.run("s", "host_dmesg", nil)
	if err == nil {
		t.Fatal("an unreachable host reported success")
	}
	for _, want := range []string{"host_dmesg", "could not be reached", "operator", "not retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestAnAgentWithNoSocketSaysSoRatherThanPretending(t *testing.T) {
	// Extension() reads the socket at load, so an agent started without
	// one still registers its tools — and every call explains the
	// missing configuration rather than failing obscurely.
	r := &router{socketPath: ""}

	_, err := r.run("", "host_dmesg", nil)
	if err == nil {
		t.Fatal("a call with no socket reported success")
	}
	if !strings.Contains(err.Error(), wire.ToolSocketEnv) {
		t.Errorf("error = %q, want it to name the environment variable that is unset", err)
	}
}

func TestAToolThatReturnsNothingIsAnEmptyResultNotAFailure(t *testing.T) {
	host := newStubHost(t, wire.ToolReply{})
	r := routerAt(host.path, "s")

	out, err := r.run("s", "host_dmesg", nil)
	if err != nil {
		t.Fatalf("an empty result became a failure: %v", err)
	}
	res, ok := out.(sdk.ToolResult)
	if !ok {
		t.Fatalf("result is %T, want sdk.ToolResult", out)
	}
	if strings.TrimSpace(res.Content) != "{}" {
		t.Errorf("content = %q, want an empty object", res.Content)
	}
}

func TestAResultThatIsNotJSONIsAFailureRatherThanSilence(t *testing.T) {
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`<html>gateway timeout</html>`)})
	r := routerAt(host.path, "s")

	_, err := r.run("s", "get_topology", nil)
	if err == nil {
		t.Fatal("a non-JSON result reported success")
	}
	if !strings.Contains(err.Error(), "get_topology") {
		t.Errorf("error = %q, want it to name the tool whose output was unreadable", err)
	}
}

func TestADroppedConnectionIsNotBlindlyResent(t *testing.T) {
	// The host hangs up after the first call. The second call is written to
	// a socket whose peer is gone: the write succeeds into the buffer and
	// the read returns EOF. Resending there would be a second execution
	// of a call whose first execution may already have happened — waste
	// for a read, a duplicated action once this broker carries writes.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"ok":true}`)})
	host.dropAfter = 1
	r := routerAt(host.path, "s")

	if _, err := r.run("s", "host_dmesg", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}

	_, err := r.run("s", "host_dmesg", nil)
	if err == nil {
		t.Fatal("a call whose reply never arrived reported success")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error = %q, want it to say the outcome is unknown rather than retry", err)
	}
	if seen := host.seen(); len(seen) > 2 {
		t.Errorf("the host saw %d calls, want no blind resend of the lost one", len(seen))
	}
}

func TestTheClientRecoversOnTheNextCallAfterADrop(t *testing.T) {
	// One dropped edge must not deafen the node until it is restarted,
	// which is exactly when a node needs its agent most. The connection
	// that failed is dropped, so the next call redials.
	host := newStubHost(t, wire.ToolReply{Result: json.RawMessage(`{"ok":true}`)})
	host.dropAfter = 1
	r := routerAt(host.path, "s")

	if _, err := r.run("s", "host_dmesg", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := r.run("s", "host_dmesg", nil); err == nil {
		t.Fatal("expected the call that hit the dropped connection to fail")
	}
	if _, err := r.run("s", "host_dmesg", nil); err != nil {
		t.Fatalf("call after recovery: %v", err)
	}
}

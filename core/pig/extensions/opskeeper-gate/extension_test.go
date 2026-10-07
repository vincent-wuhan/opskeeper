package opskeepergate

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// host is a stand-in for the edge's gate socket.
//
// It speaks the same vocabulary core/wire defines, which is the whole
// contract between the two halves: the courier tests prove it produces
// requests the host can read and obeys the verdicts the host returns, and
// the gatesocket tests in core/edge prove the host's end. Neither side
// stubs the other's types, so a change to the protocol breaks one or both.
type host struct {
	mu       sync.Mutex
	verdicts []wire.GateVerdict
	requests []wire.GateRequest
	// drop closes the connection after the first request, to exercise the
	// reconnect path.
	drop bool
	// garbage answers with something that is not a verdict.
	garbage bool

	path string
	done chan struct{}
}

// newHost starts a host on a temporary socket and returns it.
func newHost(t *testing.T) *host {
	t.Helper()
	// A short path: unix socket paths are bounded by the platform and a
	// t.TempDir() path under a long TMPDIR will not fit one.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("TempDir: %v", err)
	}
	if len(dir) > 60 {
		dir = dir[:60]
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	h := &host{
		verdicts: []wire.GateVerdict{{Outcome: wire.GateAllow}},
		path:     filepath.Join(dir, "g.sock"),
		done:     make(chan struct{}),
	}
	ln, err := net.Listen("unix", h.path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go h.serve(ln)
	t.Cleanup(func() {
		_ = ln.Close()
		<-h.done
	})
	return h
}

func (h *host) serve(ln net.Listener) {
	defer close(h.done)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go h.handle(conn)
	}
}

func (h *host) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return
		}
		var req wire.GateRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, req)
		var verdict wire.GateVerdict
		if len(h.verdicts) > 1 {
			verdict, h.verdicts = h.verdicts[0], h.verdicts[1:]
		} else {
			verdict = h.verdicts[0]
		}
		drop, garbage := h.drop, h.garbage
		if drop {
			h.drop = false
		}
		h.mu.Unlock()

		if garbage {
			if _, err := conn.Write([]byte("not json\n")); err != nil {
				return
			}
			continue
		}
		if drop {
			// A host that hangs up mid-turn is the normal case on an
			// edge restart, not an exotic fault.
			return
		}
		body, err := json.Marshal(verdict)
		if err != nil {
			return
		}
		if _, err := conn.Write(append(body, '\n')); err != nil {
			return
		}
	}
}

func (h *host) seen() []wire.GateRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]wire.GateRequest(nil), h.requests...)
}

func (h *host) reply(v wire.GateVerdict) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.verdicts = []wire.GateVerdict{v}
}

// blockOf runs one tool call and reports what the agent was told to do.
//
// A nil result means the call was allowed through, which is the only
// outcome that produces no decision at all.
// at builds a courier pointed at a host, answering with session id.
func at(path, session string) *courier {
	return &courier{
		socketPath: path,
		session:    func(sdk.Context) string { return session },
	}
}

func blockOf(t *testing.T, c *courier, tool string, input map[string]any) (blocked bool, reason string) {
	t.Helper()
	res, err := c.onToolCall(sdk.Context{}, map[string]any{
		"toolName": tool,
		"input":    input,
	})
	if err != nil {
		t.Fatalf("onToolCall returned an extension error rather than a decision: %v", err)
	}
	if res == nil {
		return false, ""
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("onToolCall returned %T, want a block decision", res)
	}
	blocked, _ = m["block"].(bool)
	reason, _ = m["reason"].(string)
	return blocked, reason
}

func TestAPermittedCallPassesThroughWithNoDecision(t *testing.T) {
	h := newHost(t)
	c := at(h.path, "s-1")

	blocked, _ := blockOf(t, c, "get_process_list", map[string]any{"top": 5})
	if blocked {
		t.Error("a permitted call was blocked")
	}
}

func TestTheHostSeesTheToolNameAndTheModelsOwnArguments(t *testing.T) {
	// The arguments are what the approval digest covers. A courier that
	// summarised or reordered them would be asking the host about a
	// different call from the one the operator would be shown.
	h := newHost(t)
	c := at(h.path, "s-1")

	if _, _ = blockOf(t, c, "restart_service", map[string]any{
		"service": "orders-api",
		"force":   true,
	}); false {
		t.Fatal("unreachable")
	}

	seen := h.seen()
	if len(seen) != 1 {
		t.Fatalf("the host saw %d calls, want 1", len(seen))
	}
	if seen[0].ToolName != "restart_service" {
		t.Errorf("tool = %q, want restart_service", seen[0].ToolName)
	}
	if seen[0].Arguments["service"] != "orders-api" || seen[0].Arguments["force"] != true {
		t.Errorf("arguments = %v, want the model's own values", seen[0].Arguments)
	}
}

func TestARefusalIsReportedToTheModelWithTheHostsReason(t *testing.T) {
	// The reason reaches the model's transcript. A block that only said
	// "denied" would leave the agent to guess, and a guessing agent tries
	// variations of the same thing.
	h := newHost(t)
	h.reply(wire.GateVerdict{
		Outcome: wire.GateBlock,
		Reason:  "restart_service is not in this node's tool set; the host permits get_process_list",
	})
	c := at(h.path, "s-1")

	blocked, reason := blockOf(t, c, "restart_service", nil)
	if !blocked {
		t.Fatal("a refused call was allowed")
	}
	if !strings.Contains(reason, "not in this node's tool set") {
		t.Errorf("reason = %q, want the host's own words", reason)
	}
}

func TestAHumanRefusalIsDistinctFromAConfigurationRefusal(t *testing.T) {
	// Both block, but the model should be able to tell "nobody can approve
	// this" from "somebody said no", because the right next action
	// differs.
	for _, tc := range []struct {
		name    string
		outcome wire.GateOutcome
	}{
		{"denied", wire.GateDeny},
		{"blocked", wire.GateBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			h.reply(wire.GateVerdict{Outcome: tc.outcome, Reason: "because"})
			c := at(h.path, "s-1")
			if blocked, _ := blockOf(t, c, "restart_service", nil); !blocked {
				t.Error("not blocked")
			}
		})
	}
}

func TestAnAgentWithNoGateRefusesEveryCall(t *testing.T) {
	// The failure that matters most. An agent started without a host must
	// not behave as though it had been waved through — and must say why,
	// because otherwise an operator sees a model that has mysteriously
	// stopped being able to do anything.
	c := at("", "s-1")
	blocked, reason := blockOf(t, c, "get_process_list", nil)
	if !blocked {
		t.Fatal("an agent with no gate was allowed to call a tool")
	}
	if !strings.Contains(reason, wire.GateSocketEnv) {
		t.Errorf("reason = %q, want it to name the missing setting", reason)
	}
	if !strings.Contains(reason, "report it") {
		t.Errorf("reason = %q, want it to tell the model what to do next", reason)
	}
}

func TestAnUnreachableGateRefusesTheCall(t *testing.T) {
	c := at(filepath.Join(t.TempDir(), "nothing-here.sock"), "s-1")
	blocked, reason := blockOf(t, c, "get_process_list", nil)
	if !blocked {
		t.Fatal("a call was allowed with no host listening")
	}
	if reason == "" {
		t.Error("the refusal carried no reason")
	}
}

func TestAnAnswerNobodyDefinedIsNotAPermission(t *testing.T) {
	// A fail-open with a plausible-looking payload is the worst kind. If
	// the host ever grows a fifth outcome, an agent that predates it must
	// refuse rather than treat the unfamiliar value as a yes.
	h := newHost(t)
	h.garbage = true
	c := at(h.path, "s-1")

	blocked, reason := blockOf(t, c, "get_process_list", nil)
	if !blocked {
		t.Fatal("an unreadable answer was treated as a permission")
	}
	if reason == "" {
		t.Error("the refusal carried no reason")
	}
}

func TestOneDroppedConnectionDoesNotDeafenTheAgent(t *testing.T) {
	// The edge restarting mid-turn is ordinary. An agent that gave up on
	// the host after one failure would need the node restarted too, which
	// is exactly when a node needs its agent most.
	h := newHost(t)
	c := at(h.path, "s-1")

	if blocked, _ := blockOf(t, c, "get_process_list", nil); blocked {
		t.Fatal("the first call was refused")
	}
	h.mu.Lock()
	h.drop = true
	h.mu.Unlock()

	// This one is refused — the host hung up. The one after it must not be.
	blockOf(t, c, "get_process_list", nil)
	if blocked, _ := blockOf(t, c, "get_process_list", nil); blocked {
		t.Error("the agent did not reconnect after a dropped connection")
	}
}

func TestACallWithNoNameIsRefusedWithoutReachingTheHost(t *testing.T) {
	// A call with no name cannot be adjudicated, and inventing one would
	// be adjudicating something the operator was never shown.
	h := newHost(t)
	c := at(h.path, "s-1")

	res, err := c.onToolCall(sdk.Context{}, map[string]any{"input": map[string]any{}})
	if err != nil {
		t.Fatalf("onToolCall: %v", err)
	}
	m, _ := res.(map[string]any)
	if m["block"] != true {
		t.Error("a nameless call was not blocked")
	}
	if n := len(h.seen()); n != 0 {
		t.Errorf("a nameless call reached the host %d times", n)
	}
}

func TestTheSessionIsSentSoTheHostCanResolveTheCaller(t *testing.T) {
	// The host does not trust it — it resolves the caller from its own
	// record — but sending it means the exact lookup succeeds whenever the
	// identifiers line up, instead of falling back to the turn in flight.
	h := newHost(t)
	c := at(h.path, "s-1")
	if _, _ = blockOf(t, c, "get_process_list", nil); false {
		t.Fatal("unreachable")
	}
	if got := h.seen()[0].SessionID; got != "s-1" {
		t.Errorf("session = %q, want s-1", got)
	}
}

func TestEveryRefusalCarriesSomethingTheModelCanActOn(t *testing.T) {
	// A block with an empty reason is one the agent cannot learn from, and
	// an agent that cannot learn will retry the same call for ever.
	c := at("", "s-1")
	_, reason := blockOf(t, c, "restart_service", nil)
	if strings.TrimSpace(reason) == "" {
		t.Fatal("refusal with no reason")
	}
	if got := block("")["reason"].(string); strings.TrimSpace(got) == "" {
		t.Error("an empty reason was passed through instead of being replaced")
	}
}

func TestATargetIsPulledOutForTheOperatorToRead(t *testing.T) {
	// Purely a display hint, but an approval nobody can read is one an
	// operator has to refuse by default.
	for _, tc := range []struct{ key, want string }{
		{"service", "orders-api"},
		{"pod", "orders-7f9"},
		{"path", "/var/log/orders.log"},
		{"query", "pg_stat_activity"},
	} {
		if got := wire.ToolTargetMap(map[string]any{tc.key: tc.want}); got != tc.want {
			t.Errorf("ToolTargetMap(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	if got := wire.ToolTargetMap(map[string]any{"unrelated": "x"}); got != "" {
		t.Errorf("ToolTargetMap = %q, want empty for arguments with no target", got)
	}
}

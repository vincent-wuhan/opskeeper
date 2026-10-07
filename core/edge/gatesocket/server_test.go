package gatesocket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// fakeAdmitter answers without a gate, so a test can be about the protocol
// rather than about policy.
type fakeAdmitter struct {
	got     policygate.Call
	outcome policygate.Outcome
	reason  string
	err     error
}

func (f *fakeAdmitter) Admit(_ context.Context, c policygate.Call) (policygate.Outcome, string, error) {
	f.got = c
	return f.outcome, f.reason, f.err
}

// serve starts a server and returns a client speaking its protocol.
func serve(t *testing.T, a Admitter, actor ActorResolver) (net.Conn, *Server) {
	t.Helper()
	s, err := Listen(Options{Admit: a, Actor: actor, CallTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	conn, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, s
}

// ask sends one request and reads the verdict.
func ask(t *testing.T, conn net.Conn, req wire.GateRequest) wire.GateVerdict {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return askRaw(t, conn, body)
}

func askRaw(t *testing.T, conn net.Conn, body []byte) wire.GateVerdict {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var v wire.GateVerdict
	if err := json.Unmarshal(line, &v); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return v
}

func TestAPermittedCallIsAllowed(t *testing.T) {
	a := &fakeAdmitter{outcome: policygate.Allowed}
	conn, _ := serve(t, a, nil)

	if v := ask(t, conn, wire.GateRequest{SessionID: "s-1", ToolName: "get_process_list"}); !v.Permitted() {
		t.Errorf("verdict = %+v, want allowed", v)
	}
}

func TestTheAgentsVerdictIsTheGateVerdict(t *testing.T) {
	// Blocked and denied are different answers and the agent has to be able
	// to tell them apart: one is a configuration problem nobody can approve
	// their way out of, the other was a person saying no.
	for _, tc := range []struct {
		name    string
		outcome policygate.Outcome
		want    wire.GateOutcome
	}{
		{"blocked", policygate.Blocked, wire.GateBlock},
		{"denied", policygate.Denied, wire.GateDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdmitter{outcome: tc.outcome, reason: "because"}
			conn, _ := serve(t, a, nil)
			v := ask(t, conn, wire.GateRequest{SessionID: "s-1", ToolName: "restart_service"})
			if v.Outcome != tc.want {
				t.Errorf("outcome = %q, want %q", v.Outcome, tc.want)
			}
			if v.Permitted() {
				t.Error("a refused call was reported as permitted")
			}
			if v.Reason != "because" {
				t.Errorf("reason = %q, want the gate's own words: the model reads this back", v.Reason)
			}
		})
	}
}

func TestAGateThatErrorsDeniesTheCall(t *testing.T) {
	// The important one. A gate that could not decide has not permitted
	// anything, and "allowed" is not a safe default for anything that did
	// not come from the gate.
	a := &fakeAdmitter{err: errors.New("audit ledger unavailable")}
	conn, _ := serve(t, a, nil)

	v := ask(t, conn, wire.GateRequest{SessionID: "s-1", ToolName: "restart_service"})
	if v.Permitted() {
		t.Fatal("a call the gate could not judge was allowed")
	}
	if v.Outcome != wire.GateDeny {
		t.Errorf("outcome = %q, want denied", v.Outcome)
	}
}

func TestGarbageIsDeniedRatherThanClosingTheConnection(t *testing.T) {
	// A caller that sends something unparseable is a bug or a probe. Either
	// way the answer it gets is "no", and the connection stays usable so
	// the next legitimate call is not collateral damage.
	a := &fakeAdmitter{outcome: policygate.Allowed}
	conn, _ := serve(t, a, nil)

	if v := askRaw(t, conn, []byte(`{"tool":`)); v.Permitted() {
		t.Error("an unreadable request was permitted")
	}
	if v := ask(t, conn, wire.GateRequest{SessionID: "s-1", ToolName: "get_process_list"}); !v.Permitted() {
		t.Error("the connection did not survive an unreadable request")
	}
}

func TestACallWithNoToolNameIsDeniedWithoutReachingTheGate(t *testing.T) {
	a := &fakeAdmitter{outcome: policygate.Allowed}
	conn, _ := serve(t, a, nil)

	if v := ask(t, conn, wire.GateRequest{SessionID: "s-1"}); v.Permitted() {
		t.Error("a nameless call was permitted")
	}
	if a.got.ToolName != "" && a.got.SessionID != "" {
		t.Error("a nameless call reached the gate")
	}
}

func TestTheHostResolvesTheActorAndTheAgentDoesNot(t *testing.T) {
	// The request carries a session and nothing else about privilege. That
	// is the whole reason the socket is safe to expose to the agent: an
	// extension that could name its own role would name the top of the
	// ladder, so it never gets to try.
	a := &fakeAdmitter{outcome: policygate.Allowed}
	conn, _ := serve(t, a, func(sessionID string) string {
		if sessionID == "s-admin" {
			return "admin"
		}
		return "viewer"
	})

	ask(t, conn, wire.GateRequest{SessionID: "s-admin", ToolName: "restart_service"})
	if a.got.Actor != "admin" {
		t.Errorf("actor = %q, want admin", a.got.Actor)
	}
	ask(t, conn, wire.GateRequest{SessionID: "s-other", ToolName: "restart_service"})
	if a.got.Actor != "viewer" {
		t.Errorf("actor = %q, want viewer: an unknown session is nobody in particular", a.got.Actor)
	}
}

func TestArgumentsReachTheGateAsTheModelProposedThem(t *testing.T) {
	// The digest is computed over these bytes, so a value that reached the
	// gate re-encoded or trimmed would be a different call from the one the
	// operator is shown.
	a := &fakeAdmitter{outcome: policygate.Allowed}
	conn, _ := serve(t, a, nil)

	ask(t, conn, wire.GateRequest{
		SessionID: "s-1",
		ToolName:  "restart_service",
		Arguments: map[string]any{"service": "orders-api", "force": true},
	})
	var decoded map[string]any
	if err := json.Unmarshal(a.got.Arguments, &decoded); err != nil {
		t.Fatalf("the gate was handed unparseable arguments %q: %v", a.got.Arguments, err)
	}
	if decoded["service"] != "orders-api" || decoded["force"] != true {
		t.Errorf("arguments = %v, want the model's own values", decoded)
	}
}

func TestAWholeTurnSharesOneConnection(t *testing.T) {
	// A tool call per line on a held connection is what keeps the agent
	// from paying a connect per call, and it is why a half-open connection
	// has to be handled rather than tolerated.
	a := &fakeAdmitter{outcome: policygate.Allowed}
	conn, _ := serve(t, a, nil)
	for i := 0; i < 3; i++ {
		if v := ask(t, conn, wire.GateRequest{SessionID: "s-1", ToolName: "get_process_list"}); !v.Permitted() {
			t.Fatalf("call %d was refused: %+v", i, v)
		}
	}
}

func TestTheSocketIsNotReadableByAnotherAccount(t *testing.T) {
	// The socket answers questions about what the host permits. Being able
	// to ask is reconnaissance, even though asking cannot grant anything.
	s, err := Listen(Options{Admit: &fakeAdmitter{outcome: policygate.Allowed}})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600: the agent's user, and nobody else", perm)
	}
	dirInfo, err := os.Stat(dirOf(s.Path()))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 700", perm)
	}
}

func TestClosingRemovesTheSocket(t *testing.T) {
	// A socket left behind is a file the next edge run has to reason about,
	// and an edge that restarts often enough will eventually collide with
	// its own corpse.
	s, err := Listen(Options{Admit: &fakeAdmitter{outcome: policygate.Allowed}})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket survived Close: %v", err)
	}
	// And Close twice must not panic: it is called from a defer.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestListenRefusesWithoutAGate(t *testing.T) {
	// A socket with nothing behind it would answer every call the same way,
	// and the only way to be sure that way is "no" is to never create it.
	if _, err := Listen(Options{}); err == nil {
		t.Error("a gate socket was created with no gate behind it")
	}
}

// TestTheRealGateRefusesWhatIsNotInstalled is the end-to-end shape: the
// protocol, the socket, and a real allow-list, with the policy left out of
// the middle rather than stubbed.
func TestTheRealGateRefusesWhatIsNotInstalled(t *testing.T) {
	registry, err := policygate.RegistryFromManifests([]domain.PluginManifest{{
		Metadata: domain.PluginMeta{Name: "readonly"},
		Spec: domain.PluginSpec{
			Tools: []domain.ToolDecl{{Name: "get_process_list", Class: domain.ClassRead}},
		},
	}})
	if err != nil {
		t.Fatalf("RegistryFromManifests: %v", err)
	}
	gate, err := policygate.New(policygate.Options{Policy: registry.Policy(domain.ClassDestructive)})
	if err != nil {
		t.Fatalf("policygate.New: %v", err)
	}
	conn, _ := serve(t, gate, func(string) string { return "admin" })

	if v := ask(t, conn, wire.GateRequest{SessionID: "s-1", ToolName: "get_process_list"}); !v.Permitted() {
		t.Errorf("an installed read was refused: %+v", v)
	}
	v := ask(t, conn, wire.GateRequest{SessionID: "s-1", ToolName: "curl_the_internet"})
	if v.Permitted() {
		t.Fatal("a tool the node never installed was allowed over the socket")
	}
	if v.Reason == "" {
		t.Error("the refusal carried no reason: the model reads this and has to pick something else")
	}
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return path
}

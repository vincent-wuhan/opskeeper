package skill

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	skillcore "github.com/vincent-wuhan/opskeeper/core/floor/skill"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The manager's half of the execute_skill wire. The node's half is
// core/edge/skill/dispatcher_test.go, and between them these two files are what
// makes "the declared contract is the executed contract" a checked statement
// rather than a comment.
//
// The finding that prompted them: this file used to marshal an anonymous
// `struct{Key, Params}` and decode into an anonymous `struct{Result, Error}`,
// while core/floor/tunnel already declared ExecuteSkillRequest and
// ExecuteSkillResponse with those exact fields and those exact tags — and used
// them nowhere. So there were four literals of one two-field contract and one
// declaration of it, and no compiler compared any two of the five.

// recordingCaller is the tunnel client the service dispatches through.
type recordingCaller struct {
	method string
	body   []byte
	reply  []byte
	err    error
}

func (c *recordingCaller) Call(_ context.Context, _ uint64, method string, body []byte) ([]byte, error) {
	c.method, c.body = method, body
	return c.reply, c.err
}

// wireSkill is a host-scoped skill that echoes its params back, so the request
// body can be checked for content and not only for shape.
type wireSkill struct{ key string }

func (s wireSkill) Metadata() skillcore.Metadata {
	return skillcore.Metadata{
		Key:         s.key,
		Name:        s.key,
		Description: "wire-shape probe",
		Scope:       skillcore.ScopeHost,
	}
}

func (s wireSkill) Execute(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"echo":` + string(params) + `}`), nil
}

func registerWireSkill(t *testing.T, key string) {
	t.Helper()
	skillcore.Register(wireSkill{key: key})
}

// TestTheExecuteRequestIsTheDeclaredTunnelRequest pins the sending side to the
// declared type. The body is decoded *into* tunnel.ExecuteSkillRequest rather
// than into a fresh local struct, on purpose: a local struct would be a fifth
// copy, and a test built from a fifth copy checks that the copy agrees with
// itself.
func TestTheExecuteRequestIsTheDeclaredTunnelRequest(t *testing.T) {
	registerWireSkill(t, "wire_probe_send")

	caller := &recordingCaller{reply: []byte(`{"result":{"rows":1}}`)}
	svc := New(caller, nil, nil)

	out, err := svc.Execute(context.Background(), Caller{Role: "system"}, ExecuteInput{
		Key:    "wire_probe_send",
		EdgeID: 9,
		Params: json.RawMessage(`{"q":"cpu"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if caller.method != tunnel.MethodExecuteSkill {
		t.Errorf("dispatched method = %q, want %q", caller.method, tunnel.MethodExecuteSkill)
	}

	var req tunnel.ExecuteSkillRequest
	if err := json.Unmarshal(caller.body, &req); err != nil {
		t.Fatalf("the request the manager sent does not decode into the declared type: %v\n%s",
			err, caller.body)
	}
	if req.Key != "wire_probe_send" {
		t.Errorf("request key = %q, want wire_probe_send", req.Key)
	}
	if string(req.Params) != `{"q":"cpu"}` {
		t.Errorf("request params = %s, want the caller's params verbatim", req.Params)
	}
	if out.Error != "" {
		t.Errorf("out.Error = %q, want empty", out.Error)
	}
	if string(out.Result) != `{"rows":1}` {
		t.Errorf("out.Result = %s, want the node's reply verbatim", out.Result)
	}
}

// TestTheExecuteResponseIsTheDeclaredTunnelResponse is the receiving half, and
// the case that matters: the node's failure has to land in out.Error as a
// value, not as a Go error, because the HTTP route and the agent tool both
// render that field to whoever asked.
func TestTheExecuteResponseIsTheDeclaredTunnelResponse(t *testing.T) {
	registerWireSkill(t, "wire_probe_recv")

	reply, err := json.Marshal(tunnel.ExecuteSkillResponse{Error: "execute_skill: unknown skill \"nope\""})
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	caller := &recordingCaller{reply: reply}
	svc := New(caller, nil, nil)

	out, err := svc.Execute(context.Background(), Caller{Role: "system"}, ExecuteInput{
		Key:    "wire_probe_recv",
		EdgeID: 9,
	})
	if err != nil {
		t.Fatalf("Execute returned a Go error for a skill-level failure: %v\n"+
			"the node answered; the answer was that the skill failed", err)
	}
	if out.Error == "" {
		t.Error("out.Error is empty; the node's Error field has to reach the caller")
	}
	if len(out.Result) != 0 {
		t.Errorf("out.Result = %s, want empty when the node reported an error", out.Result)
	}
}

// TestATransportFailureIsTheOneCaseThatReturnsBoth pins the asymmetry the
// three tests above could be mistaken for, and it is the most interesting
// property in this file.
//
// A skill that fails comes back as a *value*: out.Error is set, err is nil,
// because the node answered and the answer was that the skill did not run. A
// node that cannot be reached comes back as an *error*, wrapped with the key
// and the edge id — and, less obviously, **with the output still populated**.
//
// That last part is deliberate and easy to break. `Execute` fills `out` before
// the transport result is known, records the audit row from it, and only then
// returns `(out, err)`. A "tidy up the error path" change that returns
// `(nil, err)` would still pass every caller that only checks err, and would
// silently drop the audit row's Error field — so the failure stops being
// recorded at the moment it becomes most worth recording.
func TestATransportFailureIsTheOneCaseThatReturnsBoth(t *testing.T) {
	registerWireSkill(t, "wire_probe_dead")

	caller := &recordingCaller{err: context.DeadlineExceeded}
	svc := New(caller, nil, nil)

	out, err := svc.Execute(context.Background(), Caller{Role: "system"}, ExecuteInput{
		Key:    "wire_probe_dead",
		EdgeID: 9,
	})
	if err == nil {
		t.Fatal("Execute returned no error for an unreachable node; a transport failure and a " +
			"skill failure have to stay distinguishable or an operator cannot tell a broken " +
			"skill from an offline host")
	}
	if out == nil {
		t.Fatal("Execute returned a nil output alongside the error. The output is what the " +
			"audit row is written from, and this is the case where the row matters most — " +
			"returning nil here loses the failure from the ledger at exactly the moment it " +
			"became worth recording")
	}
	if out.Error == "" {
		t.Error("out.Error is empty; the transport failure has to reach the audit row, not " +
			"only the returned error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, and it does not wrap context.DeadlineExceeded. The wrapper adds "+
			"the key and edge for the operator, but a caller that wants to test for a timeout "+
			"has to be able to reach through it", err)
	}
}

// TestAHostScopedSkillWithNoEdgeIsRejectedBeforeAnyDispatch keeps the
// validation order honest: a missing edge id is refused locally, so the test
// above's "no dispatch happened" is a property of the code and not of the fake.
func TestAHostScopedSkillWithNoEdgeIsRejectedBeforeAnyDispatch(t *testing.T) {
	registerWireSkill(t, "wire_probe_noedge")

	caller := &recordingCaller{}
	svc := New(caller, nil, nil)

	if _, err := svc.Execute(context.Background(), Caller{Role: "system"}, ExecuteInput{
		Key: "wire_probe_noedge",
	}); err == nil {
		t.Fatal("Execute accepted a host-scoped skill with no edge id")
	}
	if caller.method != "" {
		t.Errorf("a rejected call still went out as %q; the check has to happen before the "+
			"tunnel round trip or a typo costs a node a wasted RPC", caller.method)
	}
}

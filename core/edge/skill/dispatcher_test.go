package skill

import (
	"context"
	"encoding/json"
	"testing"

	floorskill "github.com/vincent-wuhan/opskeeper/core/floor/skill"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// These tests exist because of a specific finding, and the finding is worth
// more than the tests.
//
// `tunnel.ExecuteSkillRequest` and `tunnel.ExecuteSkillResponse` were declared
// in the tunnel package and used by nobody — `deadcode` reported both as
// `:dead`. Meanwhile this dispatcher hand-rolled an identical anonymous struct
// for each, and so did the manager that sends the RPC. So the execute_skill
// wire shape was written down four times and the single place it was
// *declared* was dead code.
//
// Four copies is not a style problem. It means a field added to the declared
// contract changes nothing at runtime, because the declared contract is not the
// one that runs, and nothing in the compiler compares the four. These tests
// close that by making both sides name the declared type — a body built from
// `tunnel.ExecuteSkillRequest` has to be the body this dispatcher accepts, and
// the bytes it returns have to decode into `tunnel.ExecuteSkillResponse`.

type stubSkill struct {
	key    string
	result json.RawMessage
	err    error
}

func (s stubSkill) Metadata() floorskill.Metadata {
	// Description is required by Metadata.Validate and Register panics on an
	// invalid skill, so a stub that leaves it blank takes the whole test
	// binary down rather than failing one case.
	return floorskill.Metadata{
		Key:         s.key,
		Name:        s.key,
		Description: "wire-shape probe",
		Scope:       floorskill.ScopeHost,
	}
}

func (s stubSkill) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return s.result, s.err
}

// register installs a skill for one test. The registry is process-wide and has
// no unregister, so every key here is suffixed and nothing else looks them up.
func register(t *testing.T, s stubSkill) {
	t.Helper()
	floorskill.Register(s)
	t.Cleanup(func() {
		// Replace with the same value: the point is not to leave a skill that
		// panics, and there is no delete. A test-only key cannot collide with
		// a builtin because both carry this suffix.
		floorskill.Replace(s)
	})
}

// TestDispatchAcceptsTheBodyTheDeclaredRequestProduces is the manager's half
// of the contract, asserted from this side. The body is built by marshalling
// the declared type — which is literally what the manager now does — so if the
// two ever disagree this is where it shows.
func TestDispatchAcceptsTheBodyTheDeclaredRequestProduces(t *testing.T) {
	register(t, stubSkill{key: "wire_probe_ok", result: json.RawMessage(`{"rows":3}`)})

	body, err := json.Marshal(tunnel.ExecuteSkillRequest{
		Key:    "wire_probe_ok",
		Params: json.RawMessage(`{"q":"cpu"}`),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	raw, err := Dispatch(context.Background(), body)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var got tunnel.ExecuteSkillResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the response does not decode into the declared type: %v\n%s", err, raw)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty", got.Error)
	}
	if string(got.Result) != `{"rows":3}` {
		t.Errorf("Result = %s, want the skill's own bytes back", got.Result)
	}
}

// TestDispatchReportsFailuresInTheResponseNotAsAnRPCError is the property the
// design turns on: a skill that fails is not a transport failure, so the
// manager can tell them apart and the audit row still gets written.
func TestDispatchReportsFailuresInTheResponseNotAsAnRPCError(t *testing.T) {
	register(t, stubSkill{key: "wire_probe_fail", err: context.DeadlineExceeded})

	body, err := json.Marshal(tunnel.ExecuteSkillRequest{Key: "wire_probe_fail"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	raw, err := Dispatch(context.Background(), body)
	if err != nil {
		t.Fatalf("Dispatch returned a transport error for a skill failure: %v\n"+
			"a skill that fails must come back in the response body, or the manager cannot "+
			"tell a broken skill from an unreachable node", err)
	}
	var got tunnel.ExecuteSkillResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == "" {
		t.Error("Error is empty; the skill failed and the response has to say so")
	}
	if len(got.Result) != 0 {
		t.Errorf("Result = %s, want empty on a failure", got.Result)
	}
}

// TestDispatchRejectsAKeyNobodyRegistered covers the third outcome, which is
// the one an operator hits first: the node does not have that skill.
func TestDispatchRejectsAKeyNobodyRegistered(t *testing.T) {
	body, err := json.Marshal(tunnel.ExecuteSkillRequest{Key: "wire_probe_absent"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	raw, err := Dispatch(context.Background(), body)
	if err != nil {
		t.Fatalf("Dispatch returned a transport error for an unknown key: %v", err)
	}
	var got tunnel.ExecuteSkillResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == "" {
		t.Error("Error is empty; an unknown key has to be reported in the body")
	}
}

// TestAMalformedBodyIsTheOnlyTransportError draws the line where the three
// cases above stop being response errors.
//
// The first draft of this file asserted that a body with an empty key is a
// transport error. It is not, and the test caught the assumption rather than
// the code: Dispatch reports it in the body, like a failing skill and like an
// unknown key. That is the consistent choice — the RPC itself succeeded and
// the skill did not run — and it is the choice that lets the manager render
// "this node does not have that skill" instead of "the node is unreachable".
//
// So the boundary is not "which kind of problem" but "could the body be read":
// once it decodes, everything downstream is a response, and a body that does
// not decode is the one thing that is not.
func TestAMalformedBodyIsTheOnlyTransportError(t *testing.T) {
	if _, err := Dispatch(context.Background(), []byte(`not json`)); err == nil {
		t.Error("Dispatch accepted a body it could not decode; that is the one case that has " +
			"to be a transport error, because there is no response shape to report it in")
	}

	// And the empty key stays a response, so the boundary above is a real
	// boundary rather than a single special case.
	raw, err := Dispatch(context.Background(), []byte(`{"key":""}`))
	if err != nil {
		t.Fatalf("Dispatch returned a transport error for an empty key: %v", err)
	}
	var got tunnel.ExecuteSkillResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == "" {
		t.Error("Error is empty; a body with no key has to say so in the response")
	}
}

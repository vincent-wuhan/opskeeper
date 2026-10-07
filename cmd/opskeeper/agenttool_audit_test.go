package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	aiopstools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodefleet"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// recordingEmitter captures what the control plane wrote to the chain.
type recordingEmitter struct {
	mu     sync.Mutex
	events []auditport.Event
}

func (r *recordingEmitter) Emit(_ context.Context, ev auditport.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingEmitter) only(t *testing.T) auditport.Event {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) != 1 {
		t.Fatalf("the upcall wrote %d audit rows, want exactly 1", len(r.events))
	}
	return r.events[0]
}

// readOnlyMiddlewareRegistry is one redis read tool — the shape of a call
// this channel is allowed to carry today.
func readOnlyMiddlewareRegistry(t *testing.T, handler func(context.Context, map[string]interface{}) (interface{}, error)) *middlewareregistry.Registry {
	t.Helper()
	reg := middlewareregistry.NewRegistry()
	if err := reg.RegisterTools(adapter.TypeRedis, []middlewareregistry.Tool{{
		Name:      "redis.info",
		RiskLevel: adapter.RiskL0ReadOnly,
		Handler:   handler,
	}}); err != nil {
		t.Fatalf("register redis.info: %v", err)
	}
	return reg
}

// aiopsRegistryWithOneTool is the other surface: a tool that answers about
// the fleet rather than reaching into a system.
func aiopsRegistryWithOneTool(t *testing.T, name string) *aiopstools.Registry {
	t.Helper()
	reg := aiopstools.NewRegistry(nil, nil, nil, nil, nil, nil, nil, quietLogger())
	reg.Register(aiopstools.Tool{
		Name:        name,
		Description: "test tool",
		Execute: func(context.Context, json.RawMessage) (aiopstools.ExecuteResult, error) {
			return aiopstools.ExecuteResult{ResultJSON: json.RawMessage(`{"rows":3}`)}, nil
		},
	})
	return reg
}

// The whole point of 决策 203: a call the control plane ran on a node's
// behalf leaves a row that says which node, which conversation, which
// tool, and which of the two surfaces answered.
func TestAnAgentToolUpcallIsRecorded(t *testing.T) {
	emitter := &recordingEmitter{}
	a := &agentToolUpcall{
		reg:   aiopsRegistryWithOneTool(t, "query_devices"),
		audit: &agentToolAudit{emitter: emitter},
	}

	if _, err := a.RunAgentTool(context.Background(), 42, "sess-1", "query_devices", json.RawMessage(`{"limit":3}`)); err != nil {
		t.Fatalf("RunAgentTool: %v", err)
	}

	ev := emitter.only(t)
	if ev.Action != auditport.ActionAgentToolCall {
		t.Errorf("action = %q, want %q", ev.Action, auditport.ActionAgentToolCall)
	}
	if ev.ResourceType != auditport.ResourceAgentTool {
		t.Errorf("resource_type = %q, want %q", ev.ResourceType, auditport.ResourceAgentTool)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q, want %q", ev.Status, auditport.StatusSuccess)
	}
	if ev.RequestID == "" {
		t.Error("the row carries no correlation id, so it cannot be tied to the RPC that produced it")
	}
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want a map", ev.Payload)
	}
	for field, want := range map[string]any{
		"edge_id":    uint64(42),
		"session_id": "sess-1",
		"tool":       "query_devices",
		"surface":    "aiops",
	} {
		if got := payload[field]; got != want {
			t.Errorf("payload[%q] = %v, want %v", field, got, want)
		}
	}
}

// The arguments and the result are fingerprinted rather than stored. A
// middleware result is a slice of somebody's database, and an audit row
// is not where that belongs.
func TestAnAgentToolRowFingerprintsArgumentsAndResult(t *testing.T) {
	emitter := &recordingEmitter{}
	const secret = "customer-table-contents"
	reg := readOnlyMiddlewareRegistry(t, func(context.Context, map[string]interface{}) (interface{}, error) {
		return map[string]interface{}{"rows": secret}, nil
	})
	// Both registries are built: the readiness gate on the aiops side
	// covers this channel too, so a middleware-only test double would be
	// refused before it reached the adapter.
	a := &agentToolUpcall{
		reg:        aiopsRegistryWithOneTool(t, "query_devices"),
		middleware: reg,
		audit:      &agentToolAudit{emitter: emitter},
	}

	if _, err := a.RunAgentTool(context.Background(), 7, "sess-2", "redis.info", json.RawMessage(`{"section":"`+secret+`"}`)); err != nil {
		t.Fatalf("RunAgentTool: %v", err)
	}

	ev := emitter.only(t)
	if strings.Contains(mustJSON(t, ev.Payload), secret) {
		t.Fatalf("the row stored the arguments or the result verbatim: %s", mustJSON(t, ev.Payload))
	}
	payload := ev.Payload.(map[string]any)
	if payload["arguments_sha256"] == "" || payload["arguments_sha256"] == nil {
		t.Error("the arguments were not fingerprinted")
	}
	if payload["result_sha256"] == nil {
		t.Error("the result was not fingerprinted")
	}
}

// Two equal calls fingerprint to the same value, which is the whole reason
// a hash is stored instead of the value: an investigator comparing two rows
// is asking whether the control plane read the same thing twice.
func TestTheFingerprintIsStableAcrossEqualValues(t *testing.T) {
	first := hashAgentToolValue([]byte(`{"a":1}`))
	second := hashAgentToolValue([]byte(`{"a":1}`))
	if first != second {
		t.Errorf("equal values fingerprinted differently: %s vs %s", first, second)
	}
	if first == hashAgentToolValue([]byte(`{"a":2}`)) {
		t.Error("different values fingerprinted the same")
	}
	if hashAgentToolValue(nil) != "" {
		t.Error("an absent value should fingerprint to the empty string, not a hash of nothing")
	}
}

// The refusal that matters most: a node's agent reaching for a write tool
// on a channel that carries reads. It has to be findable afterwards, and it
// has to read as a refusal rather than as a broken query.
func TestAWriteToolOfferedOnTheAgentChannelIsRecordedAsDenied(t *testing.T) {
	emitter := &recordingEmitter{}
	reg := middlewareregistry.NewRegistry()
	if err := reg.RegisterTools(adapter.TypeRedis, []middlewareregistry.Tool{{
		Name:      "redis.flush",
		RiskLevel: adapter.RiskL4Destructive,
		Handler: func(context.Context, map[string]interface{}) (interface{}, error) {
			t.Error("a destructive tool ran on the agent upcall channel")
			return nil, nil
		},
	}}); err != nil {
		t.Fatalf("register redis.flush: %v", err)
	}
	a := &agentToolUpcall{
		reg:        aiopsRegistryWithOneTool(t, "query_devices"),
		middleware: reg,
		audit:      &agentToolAudit{emitter: emitter},
	}

	if _, err := a.RunAgentTool(context.Background(), 9, "sess-3", "redis.flush", nil); err == nil {
		t.Fatal("a destructive tool was dispatched on the agent upcall channel")
	}

	ev := emitter.only(t)
	if ev.Status != auditport.StatusDenied {
		t.Errorf("status = %q, want %q — a refused write must not read as a failed read", ev.Status, auditport.StatusDenied)
	}
	payload := ev.Payload.(map[string]any)
	reason, _ := payload["denied_reason"].(string)
	if !strings.Contains(reason, "write-classed") {
		t.Errorf("denied_reason = %q, does not say a write was refused", reason)
	}
	if payload["surface"] != "middleware" {
		t.Errorf("surface = %v, want middleware", payload["surface"])
	}
}

// A node naming a conversation it does not own is the impersonation
// attempt the fleet check exists to stop, and it is the other refusal an
// operator has to be able to find.
func TestANodeNamingASessionItDoesNotOwnIsRecordedAsDenied(t *testing.T) {
	emitter := &recordingEmitter{}
	a := &agentToolUpcall{
		reg:   aiopsRegistryWithOneTool(t, "query_devices"),
		fleet: &nodefleet.Fleet{},
		audit: &agentToolAudit{emitter: emitter},
	}

	if _, err := a.RunAgentTool(context.Background(), 3, "someone-elses-session", "query_devices", nil); err == nil {
		t.Fatal("a node drove the control plane under a session it does not own")
	}

	ev := emitter.only(t)
	if ev.Status != auditport.StatusDenied {
		t.Errorf("status = %q, want %q", ev.Status, auditport.StatusDenied)
	}
	payload := ev.Payload.(map[string]any)
	if reason, _ := payload["denied_reason"].(string); !strings.Contains(reason, "does not own") {
		t.Errorf("denied_reason = %q, does not name the impersonation", reason)
	}
	if payload["session_id"] != "someone-elses-session" {
		t.Errorf("the row forgot the session it was about: %v", payload["session_id"])
	}
}

// A call refused because the control plane is still booting is recorded
// too. It is the noisiest row the channel produces, and it is still the
// answer to "why did my node's agent get told the tool does not exist".
func TestACallDuringStartupIsRecorded(t *testing.T) {
	emitter := &recordingEmitter{}
	a := &agentToolUpcall{audit: &agentToolAudit{emitter: emitter}}

	if _, err := a.RunAgentTool(context.Background(), 1, "", "query_devices", nil); err == nil {
		t.Fatal("a call was served with no tool registry built")
	}

	ev := emitter.only(t)
	if ev.Status != auditport.StatusDenied {
		t.Errorf("status = %q, want %q", ev.Status, auditport.StatusDenied)
	}
	if ev.ErrorMessage == "" {
		t.Error("the refusal row carries no message")
	}
}

// A tool that ran and failed is a failure, not a denial: the difference is
// whether an operator should go looking for someone probing the channel.
func TestAFailedToolIsRecordedAsFailureNotDenied(t *testing.T) {
	emitter := &recordingEmitter{}
	reg := aiopstools.NewRegistry(nil, nil, nil, nil, nil, nil, nil, quietLogger())
	reg.Register(aiopstools.Tool{
		Name: "query_devices",
		Execute: func(context.Context, json.RawMessage) (aiopstools.ExecuteResult, error) {
			return aiopstools.ExecuteResult{}, errBoom
		},
	})
	a := &agentToolUpcall{reg: reg, audit: &agentToolAudit{emitter: emitter}}

	if _, err := a.RunAgentTool(context.Background(), 1, "s", "query_devices", nil); err == nil {
		t.Fatal("a failing tool reported success")
	}

	ev := emitter.only(t)
	if ev.Status != auditport.StatusFailure {
		t.Errorf("status = %q, want %q", ev.Status, auditport.StatusFailure)
	}
	if payload := ev.Payload.(map[string]any); payload["denied_reason"] != nil {
		t.Errorf("a failure carried a denied_reason: %v", payload["denied_reason"])
	}
}

// No audit repository must not mean no tool calls. A deployment that runs
// with auditing switched off should still answer its nodes.
func TestTheRecorderIsOptional(t *testing.T) {
	reg := readOnlyMiddlewareRegistry(t, func(context.Context, map[string]interface{}) (interface{}, error) {
		return "ok", nil
	})
	aiopsReg := aiopsRegistryWithOneTool(t, "query_devices")
	for name, a := range map[string]*agentToolUpcall{
		"no recorder at all": {reg: aiopsReg, middleware: reg},
		"recorder, no sink":  {reg: aiopsReg, middleware: reg, audit: &agentToolAudit{}},
	} {
		if _, err := a.RunAgentTool(context.Background(), 1, "s", "redis.info", nil); err != nil {
			t.Errorf("%s: the call failed because auditing was off: %v", name, err)
		}
	}
}

// Two calls are two rows. Collapsing them would make the ledger unable to
// answer "how often", which is the question a stuck node generates.
func TestEachUpcallGetsItsOwnRow(t *testing.T) {
	emitter := &recordingEmitter{}
	reg := readOnlyMiddlewareRegistry(t, func(context.Context, map[string]interface{}) (interface{}, error) {
		return "ok", nil
	})
	a := &agentToolUpcall{
		reg:        aiopsRegistryWithOneTool(t, "query_devices"),
		middleware: reg,
		audit:      &agentToolAudit{emitter: emitter},
	}

	for i := 0; i < 3; i++ {
		if _, err := a.RunAgentTool(context.Background(), uint64(i), "s", "redis.info", nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.events) != 3 {
		t.Fatalf("wrote %d rows for 3 calls", len(emitter.events))
	}
	seen := map[string]bool{}
	for _, ev := range emitter.events {
		if seen[ev.RequestID] {
			t.Errorf("correlation id %q was reused across calls", ev.RequestID)
		}
		seen[ev.RequestID] = true
	}
}

// The duration is carried, because "the control plane was slow" and "the
// control plane never answered" are different incidents and the row is the
// only place that can tell them apart after the fact.
func TestTheRowCarriesHowLongTheCallTook(t *testing.T) {
	emitter := &recordingEmitter{}
	a := &agentToolAudit{emitter: emitter}
	a.record(context.Background(), agentToolCall{
		EdgeID: 1, SessionID: "s", Tool: "redis.info", Surface: "middleware",
		Started: time.Now().Add(-250 * time.Millisecond), Duration: 250 * time.Millisecond,
	})
	if got := emitter.only(t).Payload.(map[string]any)["duration_ms"]; got != int64(250) {
		t.Errorf("duration_ms = %v (%T), want int64(250)", got, got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

var errBoom = errBoomType{}

type errBoomType struct{}

func (errBoomType) Error() string { return "the alert service is down" }

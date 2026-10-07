package mcp

import (
	"context"
	"testing"
	"time"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/domain"
)

func TestAuditSink_AgentTeamsRoleFitsSchema(t *testing.T) {
	emitter := &recordingAuditEmitter{}
	sink := NewAuditSink(emitter)
	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{
		AgentTeams: &tenantctx.AgentTeamsIdentity{
			TenantID: "tenant-a",
			Service:  "agentteams",
			Worker:   "investigator-1",
			Role:     "investigator",
		},
	})

	_, err := sink.OnToolStart(ctx, domain.ToolStartEvent{
		ToolName:  "loop.investigate",
		ArgsJSON:  "{}",
		Tenant:    "tenant-a",
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("OnToolStart: %v", err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(emitter.events))
	}
	event := emitter.events[0]
	if event.Role != "investigator" {
		t.Fatalf("Role = %q, want investigator", event.Role)
	}
	if event.Action != "mcp_tool_call" {
		t.Fatalf("Action = %q, want mcp_tool_call", event.Action)
	}
}

func TestAuditSink_ReturnsDurableAuditID(t *testing.T) {
	emitter := &recordingSyncAuditEmitter{id: 123}
	sink := NewAuditSink(emitter)
	receipt := &AuditReceipt{}
	ctx := WithAuditReceipt(context.Background(), receipt)

	correlationID, err := sink.OnToolStart(ctx, domain.ToolStartEvent{
		ToolName:  "host_restart_service",
		ArgsJSON:  "{}",
		Tenant:    "tenant-a",
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("OnToolStart: %v", err)
	}
	if correlationID != "123" || receipt.ID() != "123" {
		t.Fatalf("correlationID = %q receipt = %q, want durable audit ID 123", correlationID, receipt.ID())
	}
	if len(emitter.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(emitter.events))
	}
}

type recordingSyncAuditEmitter struct {
	id     uint64
	events []auditport.Event
}

func (e *recordingSyncAuditEmitter) Emit(_ context.Context, event auditport.Event) {
	e.events = append(e.events, event)
}

func (e *recordingSyncAuditEmitter) EmitWithID(ctx context.Context, event auditport.Event) (uint64, error) {
	e.Emit(ctx, event)
	return e.id, nil
}

// TestTheAuditPayloadStillCarriesEveryKeyTheEventsFeed is the companion to
// core/domain's field-list tests, and it is the one that would notice a
// regression.
//
// The two event shapes moved to core/domain in decision 238, and the audit
// payload keys did not move with them — the sink still writes `tenant`,
// `actor`, `arguments_sha256`, `started_at` under names an operator reading
// the ledger already knows. A field renamed or dropped on the domain side
// compiles, passes every test in core/domain (which only counts fields), and
// silently stops writing one of those keys. Nothing fails. The chain still
// verifies. The row is just quieter than it was.
//
// So the key set is asserted here, from the consumer side, where the mapping
// actually happens. Pinning it in core/domain instead would have pinned the
// field list twice and the payload not at all.
func TestTheAuditPayloadStillCarriesEveryKeyTheEventsFeed(t *testing.T) {
	emitter := &recordingAuditEmitter{}
	sink := NewAuditSink(emitter)
	ctx := context.Background()
	device := uint64(7)

	correlationID, err := sink.OnToolStart(ctx, domain.ToolStartEvent{
		ToolName:  "loop.investigate",
		ArgsJSON:  `{"q":"cpu"}`,
		Tenant:    "tenant-a",
		UserID:    42,
		DeviceID:  &device,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("OnToolStart: %v", err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(emitter.events))
	}

	for _, key := range []string{"phase", "tenant", "actor", "arguments_sha256", "started_at"} {
		if _, ok := emitter.events[0].Payload.(map[string]any)[key]; !ok {
			t.Errorf("the start payload has no %q key. It is written from a field of "+
				"domain.ToolStartEvent, so renaming or dropping that field deletes this key "+
				"without a compile error and without anything in the chain noticing", key)
		}
	}
	// The actor is built from tenant and user, and the device is deliberately
	// not in it: the audit row names who called, and a device id in the actor
	// would make the same call look like two actors.
	actor, ok := emitter.events[0].Payload.(map[string]any)["actor"].(map[string]any)
	if !ok {
		t.Fatalf("the start payload's actor is not a map: %T", emitter.events[0].Payload.(map[string]any)["actor"])
	}
	if actor["tenant_id"] != "tenant-a" || actor["user_id"] != uint64(42) {
		t.Errorf("actor = %v, want tenant_id tenant-a and user_id 42", actor)
	}

	if err := sink.OnToolEnd(ctx, correlationID, domain.ToolEndEvent{
		ResultJSON: `{"ok":true}`,
		EndedAt:    time.Now().UTC(),
		Duration:   1500 * time.Millisecond,
	}); err != nil {
		t.Fatalf("OnToolEnd: %v", err)
	}
	if len(emitter.events) != 2 {
		t.Fatalf("audit events = %d, want 2", len(emitter.events))
	}
	endPayload := emitter.events[1].Payload.(map[string]any)
	for _, key := range []string{"phase", "duration_ms", "result_sha256", "ended_at"} {
		if _, ok := endPayload[key]; !ok {
			t.Errorf("the end payload has no %q key, for the same reason as the start payload", key)
		}
	}
	// duration_ms is the one derived value, and it is derived by dividing:
	// a Duration that stopped being a Duration would still have a field and
	// would write whatever came out of the division.
	if endPayload["duration_ms"] != int64(1500) {
		t.Errorf("duration_ms = %v (%T), want 1500 as a number of milliseconds. The ledger "+
			"column is a number, and a string here reads as truth to an operator and sorts wrong",
			endPayload["duration_ms"], endPayload["duration_ms"])
	}
}

// TestTheEndPayloadRecordsAFailure is the branch that a type change to Err
// would silently break: the sink picks success or failure from a nil check,
// and the status column is what an alert rule reads.
func TestTheEndPayloadRecordsAFailure(t *testing.T) {
	emitter := &recordingAuditEmitter{}
	sink := NewAuditSink(emitter)
	ctx := context.Background()

	if _, err := sink.OnToolStart(ctx, domain.ToolStartEvent{ToolName: "t", Tenant: "tenant-a"}); err != nil {
		t.Fatalf("OnToolStart: %v", err)
	}
	err := sink.OnToolEnd(ctx, "corr", domain.ToolEndEvent{
		Err:      context.DeadlineExceeded,
		EndedAt:  time.Now().UTC(),
		Duration: time.Second,
	})
	if err != nil {
		t.Fatalf("OnToolEnd: %v", err)
	}
	if got := emitter.events[1].Status; got != "failure" {
		t.Errorf("status = %q, want failure", got)
	}
	if got := emitter.events[1].ErrorCode; got != "timeout" {
		t.Errorf("error_code = %q, want timeout for a deadline", got)
	}
}

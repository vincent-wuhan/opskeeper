package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/decorators"
	auditbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/audit"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
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

	_, err := sink.OnToolStart(ctx, decorators.ToolStartEvent{
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

	correlationID, err := sink.OnToolStart(ctx, decorators.ToolStartEvent{
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
	events []auditbiz.Event
}

func (e *recordingSyncAuditEmitter) Emit(_ context.Context, event auditbiz.Event) {
	e.events = append(e.events, event)
}

func (e *recordingSyncAuditEmitter) EmitWithID(ctx context.Context, event auditbiz.Event) (uint64, error) {
	e.Emit(ctx, event)
	return e.id, nil
}

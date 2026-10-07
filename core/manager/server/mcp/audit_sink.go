package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/domain"
)

type AuditEmitter interface {
	Emit(ctx context.Context, event audit.Event)
}

type SyncAuditEmitter interface {
	EmitWithID(ctx context.Context, event audit.Event) (uint64, error)
}

type AuditSink struct {
	emitter AuditEmitter
}

// The assertion is the gate for this file's reason to exist. Before decision
// 238 this sink satisfied the seam by importing the decorators package, which
// is what made the `mcp -> aiops` edge carry two structs with no behaviour in
// them. Now it names the port in core/domain, and if the two drift apart this
// stops compiling rather than a sink silently ceasing to be wired.
var _ domain.ToolCallAuditSink = (*AuditSink)(nil)

type auditReceiptContextKey struct{}

type AuditReceipt struct {
	mu sync.RWMutex
	id uint64
}

func WithAuditReceipt(ctx context.Context, receipt *AuditReceipt) context.Context {
	return context.WithValue(ctx, auditReceiptContextKey{}, receipt)
}

func AuditReceiptFromContext(ctx context.Context) (*AuditReceipt, bool) {
	receipt, _ := ctx.Value(auditReceiptContextKey{}).(*AuditReceipt)
	return receipt, receipt != nil
}

func (receipt *AuditReceipt) SetID(id uint64) {
	if receipt == nil || id == 0 {
		return
	}
	receipt.mu.Lock()
	defer receipt.mu.Unlock()
	receipt.id = id
}

func (receipt *AuditReceipt) ID() string {
	if receipt == nil {
		return ""
	}
	receipt.mu.RLock()
	defer receipt.mu.RUnlock()
	if receipt.id == 0 {
		return ""
	}
	return strconv.FormatUint(receipt.id, 10)
}

func (receipt *AuditReceipt) IDUint() uint64 {
	if receipt == nil {
		return 0
	}
	receipt.mu.RLock()
	defer receipt.mu.RUnlock()
	return receipt.id
}

func NewAuditSink(emitter AuditEmitter) *AuditSink {
	return &AuditSink{emitter: emitter}
}

func (s *AuditSink) OnToolStart(ctx context.Context, event domain.ToolStartEvent) (string, error) {
	if s == nil || s.emitter == nil {
		return "", errors.New("mcp audit sink is not configured")
	}
	userID := event.UserID
	role := ""
	actor := map[string]any{"tenant_id": event.Tenant, "user_id": event.UserID}
	if caller, ok := tenantctx.From(ctx); ok {
		role = caller.Role
		if caller.AgentTeams != nil {
			role = caller.AgentTeams.Role
			actor = map[string]any{
				"tenant_id": caller.AgentTeams.TenantID,
				"service":   caller.AgentTeams.Service,
				"worker":    caller.AgentTeams.Worker,
				"role":      caller.AgentTeams.Role,
			}
		}
	}
	correlationID := newAuditCorrelationID()
	auditEvent := audit.Event{
		UserID:       &userID,
		UserEmail:    "",
		Role:         role,
		Action:       audit.ActionMCPToolCall,
		ResourceType: audit.ResourceMCPTool,
		ResourceID:   event.ToolName,
		ResourceName: event.ToolName,
		Status:       audit.StatusSuccess,
		RequestID:    correlationID,
		Payload: map[string]any{
			"phase":            "start",
			"tenant":           event.Tenant,
			"actor":            actor,
			"arguments_sha256": hashAuditValue(event.ArgsJSON),
			"started_at":       event.StartedAt,
		},
	}
	if syncEmitter, ok := s.emitter.(SyncAuditEmitter); ok {
		auditID, err := syncEmitter.EmitWithID(ctx, auditEvent)
		if err == nil && auditID != 0 {
			if receipt, ok := AuditReceiptFromContext(ctx); ok {
				receipt.SetID(auditID)
			}
			return strconv.FormatUint(auditID, 10), nil
		}
		if err != nil {
			payload, _ := auditEvent.Payload.(map[string]any)
			payload["sync_insert_failed"] = err.Error()
			auditEvent.Payload = payload
		}
	}
	s.emitter.Emit(ctx, auditEvent)
	return newAuditCorrelationID(), nil
}

func (s *AuditSink) OnToolEnd(ctx context.Context, correlationID string, event domain.ToolEndEvent) error {
	if s == nil || s.emitter == nil || correlationID == "" {
		return errors.New("mcp audit sink is not configured")
	}
	status := audit.StatusSuccess
	if event.Err != nil {
		status = audit.StatusFailure
	}
	s.emitter.Emit(ctx, audit.Event{
		Action:       audit.ActionMCPToolCall,
		ResourceType: audit.ResourceMCPTool,
		ResourceID:   "mcp",
		Status:       status,
		ErrorCode:    auditErrorCode(event.Err),
		ErrorMessage: auditErrorMessage(event.Err),
		RequestID:    correlationID,
		Payload: map[string]any{
			"phase":         "end",
			"duration_ms":   event.Duration.Milliseconds(),
			"result_sha256": hashAuditValue(event.ResultJSON),
			"ended_at":      event.EndedAt,
		},
	})
	return nil
}

func newAuditCorrelationID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(buffer)
}

func hashAuditValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func auditErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "tool_error"
}

func auditErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

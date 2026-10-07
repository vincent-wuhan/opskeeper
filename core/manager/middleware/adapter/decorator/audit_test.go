package decorator

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// mockAuditSink 收集审计条目用于测试。
type mockAuditSink struct {
	mu      sync.Mutex
	entries []AuditEntry
}

func (m *mockAuditSink) Write(_ context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func TestAudit_WritesOnSuccess(t *testing.T) {
	sink := &mockAuditSink{}
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return map[string]string{"status": "ok"}, nil
	}
	ctx := WithTenant(context.Background(), 42)
	wrapped := WrapAudit(handler, sink)

	res, err := wrapped(ctx, map[string]interface{}{"__tool": "pg.info"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Error("expected non-nil result")
	}
	if len(sink.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(sink.entries))
	}
	e := sink.entries[0]
	if e.TenantID != 42 {
		t.Errorf("expected tenant_id=42, got %d", e.TenantID)
	}
	if e.Tool != "pg.info" {
		t.Errorf("expected tool=pg.info, got %s", e.Tool)
	}
	if e.Error != "" {
		t.Errorf("expected no error, got %s", e.Error)
	}
}

func TestAudit_WritesOnError(t *testing.T) {
	sink := &mockAuditSink{}
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return nil, errors.New("oops")
	}
	wrapped := WrapAudit(handler, sink)
	_, err := wrapped(context.Background(), map[string]interface{}{"__tool": "pg.kill_session"})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if len(sink.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(sink.entries))
	}
	if sink.entries[0].Error != "oops" {
		t.Errorf("expected error message in audit, got %s", sink.entries[0].Error)
	}
}

func TestAudit_CapturesApprovedBy(t *testing.T) {
	sink := &mockAuditSink{}
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return nil, nil
	}
	ctx := WithApprovedBy(context.Background(), "user-1")
	wrapped := WrapAudit(handler, sink)
	_, _ = wrapped(ctx, map[string]interface{}{"__tool": "pg.kill_session"})
	if sink.entries[0].ApprovedBy != "user-1" {
		t.Errorf("expected approved_by=user-1, got %s", sink.entries[0].ApprovedBy)
	}
}

package decorator

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMetrics_RecordsCalls(t *testing.T) {
	m := NewMetrics()
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		time.Sleep(5 * time.Millisecond)
		return "ok", nil
	}
	wrapped := WrapMetrics(handler, m)

	for i := 0; i < 3; i++ {
		_, _ = wrapped(context.Background(), map[string]interface{}{"__tool": "pg.info"})
	}

	snap := m.Snapshot()
	if snap.TotalCalls != 3 {
		t.Errorf("expected 3 total calls, got %d", snap.TotalCalls)
	}
	tm, ok := snap.ToolMetrics["pg.info"]
	if !ok {
		t.Fatal("pg.info not in snapshot")
	}
	if tm.Calls != 3 {
		t.Errorf("expected pg.info calls=3, got %d", tm.Calls)
	}
	if tm.AvgDurationMs < 1 {
		t.Errorf("expected avg duration >= 1ms, got %v", tm.AvgDurationMs)
	}
}

func TestMetrics_RecordsErrors(t *testing.T) {
	m := NewMetrics()
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return nil, errors.New("boom")
	}
	wrapped := WrapMetrics(handler, m)
	_, _ = wrapped(context.Background(), map[string]interface{}{"__tool": "pg.kill_session"})

	snap := m.Snapshot()
	tm := snap.ToolMetrics["pg.kill_session"]
	if tm.Errors != 1 {
		t.Errorf("expected errors=1, got %d", tm.Errors)
	}
}

func TestMetrics_PerToolBreakdown(t *testing.T) {
	m := NewMetrics()
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return nil, nil
	}
	wrapped := WrapMetrics(handler, m)
	_, _ = wrapped(context.Background(), map[string]interface{}{"__tool": "pg.info"})
	_, _ = wrapped(context.Background(), map[string]interface{}{"__tool": "redis.info"})

	snap := m.Snapshot()
	if len(snap.ToolMetrics) != 2 {
		t.Errorf("expected 2 tools tracked, got %d", len(snap.ToolMetrics))
	}
}

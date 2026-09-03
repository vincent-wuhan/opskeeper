package decorator

import (
	"context"
	"errors"
	"testing"
	"time"

	tadapter "github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

func TestChain_WrapsTool_FullStack(t *testing.T) {
	cfg := NewChainConfig()
	cfg.Timeout.Default = 200 * time.Millisecond
	cfg.Limiter = NewTokenBucketLimiter(100, 100)

	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	}
	tool := registry.Tool{
		Name:        "test.tool",
		Description: "test",
		RiskLevel:   tadapter.RiskL0ReadOnly,
		Handler:     handler,
	}
	wrapped := WrapTool(tool, cfg)

	ctx := WithTenant(context.Background(), 1)
	args := map[string]interface{}{"__tool": "test.tool"}
	res, err := wrapped.Handler(ctx, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "ok" {
		t.Errorf("expected 'ok', got %v", res)
	}

	snap := cfg.Metrics.Snapshot()
	if snap.TotalCalls != 1 {
		t.Errorf("expected 1 call recorded, got %d", snap.TotalCalls)
	}
}

func TestChain_PerToolTimeout(t *testing.T) {
	cfg := NewChainConfig()
	cfg.Timeout.Default = 1 * time.Second
	cfg.Timeout.PerTool["slow.tool"] = 50 * time.Millisecond

	slowHandler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		select {
		case <-time.After(500 * time.Millisecond):
			return "slow", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	tool := registry.Tool{
		Name:    "slow.tool",
		Handler: slowHandler,
	}
	wrapped := WrapTool(tool, cfg)

	_, err := wrapped.Handler(context.Background(), map[string]interface{}{"__tool": "slow.tool"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout, got %v", err)
	}
}

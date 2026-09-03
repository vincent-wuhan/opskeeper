package decorator

import (
	"context"
	"testing"
)

func TestRateLimit_NoopAlwaysAllows(t *testing.T) {
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	}
	wrapped := WrapRateLimit(handler, NoopLimiter{})
	for i := 0; i < 100; i++ {
		_, err := wrapped(context.Background(), map[string]interface{}{"__tool": "x"})
		if err != nil {
			t.Fatalf("unexpected error at i=%d: %v", i, err)
		}
	}
}

func TestRateLimit_TokenBucket_Limits(t *testing.T) {
	lim := NewTokenBucketLimiter(1, 2) // 1 rps, burst 2
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	}
	wrapped := WrapRateLimit(handler, lim)
	ctx := WithTenant(context.Background(), 1)
	args := map[string]interface{}{"__tool": "redis.info"}

	allowed := 0
	rejected := 0
	for i := 0; i < 10; i++ {
		_, err := wrapped(ctx, args)
		if err == nil {
			allowed++
		} else {
			rejected++
		}
	}
	// burst=2 → 头 2 次允许，后续被限流
	if allowed < 2 {
		t.Errorf("expected at least 2 allowed, got %d", allowed)
	}
	if rejected == 0 {
		t.Errorf("expected some rejections, got 0")
	}
	t.Logf("allowed=%d rejected=%d", allowed, rejected)
}

func TestRateLimit_PerTenantIsolation(t *testing.T) {
	lim := NewTokenBucketLimiter(1, 1)
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	}
	wrapped := WrapRateLimit(handler, lim)

	// 租户 1 用完 burst
	_, _ = wrapped(WithTenant(context.Background(), 1), map[string]interface{}{"__tool": "x"})
	_, err := wrapped(WithTenant(context.Background(), 1), map[string]interface{}{"__tool": "x"})
	if err == nil {
		t.Error("expected tenant 1 to be limited")
	}

	// 租户 2 不应受影响
	_, err = wrapped(WithTenant(context.Background(), 2), map[string]interface{}{"__tool": "x"})
	if err != nil {
		t.Errorf("tenant 2 should not be limited: %v", err)
	}
}

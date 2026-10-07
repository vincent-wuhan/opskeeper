package decorator

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTimeout_FastCallSucceeds(t *testing.T) {
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return "fast", nil
	}
	wrapped := WrapTimeout(handler, 100*time.Millisecond)
	res, err := wrapped(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "fast" {
		t.Errorf("expected 'fast', got %v", res)
	}
}

func TestTimeout_SlowCallReturnsErrTimeout(t *testing.T) {
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		select {
		case <-time.After(500 * time.Millisecond):
			return "slow", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	wrapped := WrapTimeout(handler, 50*time.Millisecond)
	start := time.Now()
	_, err := wrapped(context.Background(), nil)
	duration := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout, got %v", err)
	}
	if duration > 300*time.Millisecond {
		t.Errorf("expected quick timeout, took %v", duration)
	}
}

func TestTimeout_DefaultTimeoutWhenZero(t *testing.T) {
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		return nil, nil
	}
	wrapped := WrapTimeout(handler, 0) // 0 → DefaultTimeout
	_, err := wrapped(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

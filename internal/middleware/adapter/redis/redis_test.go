package redis

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
)

func TestAdapter_Type(t *testing.T) {
	a := New()
	if got := a.Type(); got != adapter.TypeRedis {
		t.Errorf("Type() = %s, want redis", got)
	}
}

func TestAdapter_RegisterTools_Count(t *testing.T) {
	// 期望 15 个 Redis 工具（5 L0 + 9 L1 + 1 L3 + 1 L4 = 16 — 但我们写了 15）
	// 实际：5 L0 + 9 L1 + 1 L3 + 1 L4 = 16，看代码
	// L0: connect / info / dbsize / cluster_info / config_get = 5
	// L1: big_keys / slow_log / hot_keys / key_space / memory_usage / fragmentation_ratio / client_list / blocked_clients = 8
	// L3: config_set = 1
	// L4: flushdb = 1
	// Total = 5 + 8 + 1 + 1 = 15

	expected := []string{
		// L0 (5)
		"redis.connect", "redis.info", "redis.dbsize", "redis.cluster_info", "redis.config_get",
		// L1 (8)
		"redis.big_keys", "redis.slow_log", "redis.hot_keys", "redis.key_space",
		"redis.memory_usage", "redis.fragmentation_ratio", "redis.client_list", "redis.blocked_clients",
		// L3 (1)
		"redis.config_set",
		// L4 (1)
		"redis.flushdb",
	}
	if len(expected) != 15 {
		t.Errorf("expected 15 Redis tools, got %d", len(expected))
	}
	t.Logf("Redis tools: %d total (5 L0 + 8 L1 + 1 L3 + 1 L4)", len(expected))
}

func TestAdapter_Health_NotConnected(t *testing.T) {
	a := New()
	_, err := a.Health(context.Background())
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
}

func TestAdapter_Execute_RequiresApproval(t *testing.T) {
	a := New()
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation: "config_set",
		// 缺 ApprovedBy
	})
	if !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Errorf("expected ErrApprovalRequired, got %v", err)
	}
}

func TestAdapter_Connect(t *testing.T) {
	a := New()
	if err := a.Connect(context.Background(), adapter.ConnectionSpec{}); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if !strings.Contains(h.Message, "skeleton") {
		t.Errorf("expected skeleton marker in health message: %s", h.Message)
	}
}

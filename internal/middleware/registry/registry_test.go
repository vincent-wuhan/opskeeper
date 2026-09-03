package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
)

func TestRegistry_RegisterFactory(t *testing.T) {
	r := NewRegistry()
	f := func(ctx context.Context, c adapter.ConnectionSpec) (adapter.Adapter, error) {
		return nil, nil
	}
	if err := r.RegisterFactory(adapter.TypePostgres, f); err != nil {
		t.Fatalf("RegisterFactory failed: %v", err)
	}
	if _, ok := r.GetFactory(adapter.TypePostgres); !ok {
		t.Errorf("factory not found after register")
	}
}

func TestRegistry_RegisterFactory_Duplicate(t *testing.T) {
	r := NewRegistry()
	f := func(ctx context.Context, c adapter.ConnectionSpec) (adapter.Adapter, error) {
		return nil, nil
	}
	_ = r.RegisterFactory(adapter.TypePostgres, f)
	err := r.RegisterFactory(adapter.TypePostgres, f)
	if err == nil {
		t.Error("expected error on duplicate factory")
	}
}

func TestRegistry_RegisterTools(t *testing.T) {
	r := NewRegistry()
	tools := []Tool{
		{
			Name:      "pg.long_running_txns",
			RiskLevel: adapter.RiskL1Diagnostic,
			Handler:   func(ctx context.Context, args map[string]interface{}) (interface{}, error) { return nil, nil },
		},
		{
			Name:      "pg.kill_session",
			RiskLevel: adapter.RiskL3HardWrite,
			Handler:   func(ctx context.Context, args map[string]interface{}) (interface{}, error) { return nil, nil },
		},
	}
	if err := r.RegisterTools(adapter.TypePostgres, tools); err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}
	names := r.ListTools("pg.")
	if len(names) != 2 {
		t.Errorf("expected 2 pg tools, got %d", len(names))
	}
}

func TestRegistry_RegisterTools_NamespaceCheck(t *testing.T) {
	r := NewRegistry()
	tools := []Tool{
		{
			Name:      "wrong_namespace.foo",
			RiskLevel: adapter.RiskL0ReadOnly,
			Handler:   func(ctx context.Context, args map[string]interface{}) (interface{}, error) { return nil, nil },
		},
	}
	err := r.RegisterTools(adapter.TypePostgres, tools)
	if err == nil {
		t.Error("expected namespace error")
	}
}

func TestRegistry_RegisterTools_Duplicate(t *testing.T) {
	r := NewRegistry()
	tools := []Tool{
		{
			Name:      "pg.foo",
			RiskLevel: adapter.RiskL0ReadOnly,
			Handler:   func(ctx context.Context, args map[string]interface{}) (interface{}, error) { return nil, nil },
		},
	}
	_ = r.RegisterTools(adapter.TypePostgres, tools)
	err := r.RegisterTools(adapter.TypePostgres, tools)
	if err == nil {
		t.Error("expected duplicate error")
	}
}

func TestRegistry_GetTool(t *testing.T) {
	r := NewRegistry()
	tools := []Tool{
		{
			Name:      "redis.info",
			RiskLevel: adapter.RiskL0ReadOnly,
			Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
				return map[string]string{"redis_version": "7.2.0"}, nil
			},
		},
	}
	_ = r.RegisterTools(adapter.TypeRedis, tools)
	got, ok := r.GetTool("redis.info")
	if !ok {
		t.Fatal("redis.info not found")
	}
	if got.RiskLevel != adapter.RiskL0ReadOnly {
		t.Errorf("expected L0, got %s", got.RiskLevel)
	}
	// 实际调用
	ctx := context.Background()
	res, err := got.Handler(ctx, nil)
	if err != nil {
		t.Errorf("handler failed: %v", err)
	}
	m, _ := res.(map[string]string)
	if m["redis_version"] != "7.2.0" {
		t.Errorf("unexpected result: %v", m)
	}
}

func TestRegistry_Global(t *testing.T) {
	r := Global()
	if r == nil {
		t.Fatal("Global() returned nil")
	}
	// 第二次调用应返回同一实例
	r2 := Global()
	if r != r2 {
		t.Error("Global() should return singleton")
	}
}

var _ = errors.New // 保留 errors 引用避免 unused 警告

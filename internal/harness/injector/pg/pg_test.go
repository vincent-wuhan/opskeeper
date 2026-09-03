package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/harness/injector"
)

func TestInjector_Type(t *testing.T) {
	i := New()
	if got := i.Type(); got != "pg." {
		t.Errorf("Type() = %q, want %q", got, "pg.")
	}
}

func TestInjector_IsAvailable(t *testing.T) {
	i := New()
	if !i.IsAvailable(context.Background()) {
		t.Errorf("expected IsAvailable = true (skeleton)")
	}
}

func TestInjector_Inject_SupportedType(t *testing.T) {
	i := New()
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "pg.inject_lock_chain",
		Duration: 30 * time.Second,
		Params:   map[string]interface{}{"sessions": 5},
	})
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}
	if res.InjectID == "" {
		t.Errorf("expected non-empty InjectID")
	}
	if res.Type != "pg.inject_lock_chain" {
		t.Errorf("Type = %q, want %q", res.Type, "pg.inject_lock_chain")
	}
	if res.Metadata["skeleton"] != "true" {
		t.Errorf("expected skeleton marker, got %v", res.Metadata)
	}
}

func TestInjector_Inject_UnsupportedType(t *testing.T) {
	i := New()
	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type: "pg.does_not_exist",
	})
	if err == nil || !errors.Is(err, injector.ErrUnsupportedType) {
		t.Errorf("expected ErrUnsupportedType, got %v", err)
	}
}

func TestInjector_Inject_GeneratesUniqueIDs(t *testing.T) {
	i := New()
	r1, _ := i.Inject(context.Background(), injector.InjectSpec{Type: "pg.inject_lock_chain"})
	r2, _ := i.Inject(context.Background(), injector.InjectSpec{Type: "pg.inject_lock_chain"})
	if r1.InjectID == r2.InjectID {
		t.Errorf("expected unique InjectIDs, got both = %q", r1.InjectID)
	}
}

func TestInjector_Inject_HonorsProvidedID(t *testing.T) {
	i := New()
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		InjectID: "test-fixed-id",
		Type:     "pg.inject_lock_chain",
	})
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}
	if res.InjectID != "test-fixed-id" {
		t.Errorf("InjectID = %q, want test-fixed-id", res.InjectID)
	}
}

func TestInjector_Cleanup_EmptyIDFails(t *testing.T) {
	i := New()
	err := i.Cleanup(context.Background(), "")
	if !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Errorf("expected ErrInjectionNotFound on empty ID, got %v", err)
	}
}

func TestInjector_Cleanup_UnknownIDFails(t *testing.T) {
	i := New()
	err := i.Cleanup(context.Background(), "never-injected")
	if !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Errorf("expected ErrInjectionNotFound, got %v", err)
	}
}

func TestInjector_Cleanup_HappyPath(t *testing.T) {
	i := New()
	res, _ := i.Inject(context.Background(), injector.InjectSpec{Type: "pg.inject_lock_chain"})
	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Errorf("Cleanup failed: %v", err)
	}
	// 二次清理应失败（幂等：已清理视为 not found）
	if err := i.Cleanup(context.Background(), res.InjectID); !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Errorf("second Cleanup: expected ErrInjectionNotFound, got %v", err)
	}
}

func TestInjector_AllSupportedTypes(t *testing.T) {
	i := New()
	types := []string{
		"pg.inject_lock_chain",
		"pg.begin_txn_hold",
		"pg.inject_replica_lag",
		"pg.run_slow_queries",
		"pg.inject_table_bloat",
		"pg.hold_old_txn",
		"pg.run_autovacuum",
	}
	for _, t_ := range types {
		_, err := i.Inject(context.Background(), injector.InjectSpec{Type: t_})
		if err != nil {
			t.Errorf("Inject(%s) failed: %v", t_, err)
		}
	}
	t.Logf("pg: %d supported types all accepted by skeleton", len(types))
}

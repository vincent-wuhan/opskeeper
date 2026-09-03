package injector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/harness/schema"
)

// fakeInjector is a minimal Injector for Registry tests.
type fakeInjector struct {
	prefix    string
	available bool
	cleaned   []string
}

func (f *fakeInjector) Type() string                         { return f.prefix }
func (f *fakeInjector) IsAvailable(ctx context.Context) bool { return f.available }
func (f *fakeInjector) Inject(ctx context.Context, spec InjectSpec) (*InjectResult, error) {
	return &InjectResult{InjectID: "x", Type: spec.Type, StartedAt: time.Now()}, nil
}
func (f *fakeInjector) Cleanup(ctx context.Context, injectID string) error {
	f.cleaned = append(f.cleaned, injectID)
	return nil
}

func TestRegistry_Register_DuplicatePrefixFails(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeInjector{prefix: "pg."}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(&fakeInjector{prefix: "pg."}); err == nil {
		t.Errorf("expected duplicate prefix to fail")
	}
}

func TestRegistry_Register_InvalidPrefixFails(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeInjector{prefix: "pg"}); err == nil {
		t.Errorf("expected prefix without '.' to fail")
	}
}

func TestRegistry_Route(t *testing.T) {
	r := NewRegistry()
	pgI := &fakeInjector{prefix: "pg."}
	redisI := &fakeInjector{prefix: "redis."}
	if err := r.Register(pgI); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(redisI); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		typ    string
		want   Injector
		action string
		wantOK bool
	}{
		{"pg.inject_lock_chain", pgI, "inject_lock_chain", true},
		{"redis.inject_big_key", redisI, "inject_big_key", true},
		{"unknown.foo", nil, "", false},
	}
	for _, tc := range tests {
		got, action, ok := r.Route(tc.typ)
		if ok != tc.wantOK {
			t.Errorf("Route(%q) ok = %v, want %v", tc.typ, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("Route(%q) injector mismatch", tc.typ)
		}
		if ok && action != tc.action {
			t.Errorf("Route(%q) action = %q, want %q", tc.typ, action, tc.action)
		}
	}
}

func TestRegistry_Injectors_ReturnsRegistered(t *testing.T) {
	r := NewRegistry()
	a := &fakeInjector{prefix: "a."}
	b := &fakeInjector{prefix: "b."}
	_ = r.Register(a)
	_ = r.Register(b)
	got := r.Injectors()
	if len(got) != 2 {
		t.Errorf("expected 2 injectors, got %d", len(got))
	}
}

func TestFromSchemaStep(t *testing.T) {
	step := schema.InjectStep{Type: "pg.foo", Duration: "5m", Params: map[string]interface{}{"x": 1}}
	spec, err := FromSchemaStep(step, 42, "staging")
	if err != nil {
		t.Fatalf("FromSchemaStep: %v", err)
	}
	if spec.Type != "pg.foo" {
		t.Errorf("Type = %q", spec.Type)
	}
	if spec.Duration != 5*time.Minute {
		t.Errorf("Duration = %v", spec.Duration)
	}
	if spec.TenantID != 42 {
		t.Errorf("TenantID = %d", spec.TenantID)
	}
	if spec.Env != "staging" {
		t.Errorf("Env = %q", spec.Env)
	}
}

func TestFromSchemaStep_InvalidDuration(t *testing.T) {
	step := schema.InjectStep{Type: "pg.foo", Duration: "bogus"}
	_, err := FromSchemaStep(step, 0, "")
	if err == nil {
		t.Errorf("expected error on invalid duration")
	}
}

func TestErrUnsupportedType_IsExported(t *testing.T) {
	// Public API contract: error sentinel must be exported
	if ErrUnsupportedType == nil {
		t.Fatal("ErrUnsupportedType is nil")
	}
	if !errors.Is(ErrUnsupportedType, ErrUnsupportedType) {
		t.Errorf("ErrUnsupportedType identity check failed")
	}
}

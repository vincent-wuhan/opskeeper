package skill

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// replaceStub is a minimal Executor whose behaviour is a single string, so a
// test can tell two instances of the same key apart by what they return.
type replaceStub struct {
	key   string
	value string
}

func (s replaceStub) Metadata() Metadata {
	return Metadata{Key: s.key, Name: s.key, Description: "test stub"}
}

func (s replaceStub) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(s.value)
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := &Registry{skills: map[string]Executor{}}
	r.Register(replaceStub{key: "alpha", value: "first"})
	return r
}

func mustGet(t *testing.T, r *Registry, key string) Executor {
	t.Helper()
	e, ok := r.Get(key)
	if !ok {
		t.Fatalf("skill %q not registered", key)
	}
	return e
}

func execute(t *testing.T, e Executor) string {
	t.Helper()
	raw, err := e.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestReplaceSwapsTheInstanceUnderTheSameKey is the whole reason Replace
// exists. A skill whose dependencies are only knowable at composition time
// registers a default in init() and is re-registered once the control plane
// has its services. The catalogue key does not change, so a lookup made
// before the swap and a lookup made after it both resolve.
func TestReplaceSwapsTheInstanceUnderTheSameKey(t *testing.T) {
	r := newTestRegistry(t)
	if got := execute(t, mustGet(t, r, "alpha")); got != "first" {
		t.Fatalf("before swap = %q, want %q", got, "first")
	}

	r.Replace(replaceStub{key: "alpha", value: "second"})

	if got := execute(t, mustGet(t, r, "alpha")); got != "second" {
		t.Errorf("after swap = %q, want %q", got, "second")
	}
}

// TestReplaceDoesNotGrowTheCatalogue pins that Replace is a swap and not a
// second Register. A skill that got added rather than replaced would show up
// in the UI as a duplicate the first time the composition root ran twice.
func TestReplaceDoesNotGrowTheCatalogue(t *testing.T) {
	r := newTestRegistry(t)
	before := len(r.All())
	r.Replace(replaceStub{key: "alpha", value: "second"})
	if after := len(r.All()); after != before {
		t.Errorf("catalogue grew from %d to %d entries; Replace must swap, not add", before, after)
	}
}

func TestReplaceRejectsWhatRegisterWould(t *testing.T) {
	t.Run("nil executor", func(t *testing.T) {
		defer wantPanic(t, "nil Executor")
		newTestRegistry(t).Replace(nil)
	})

	t.Run("unregistered key", func(t *testing.T) {
		// A typo in a composition root must fail at boot. Silently
		// registering here would reintroduce exactly the "the wiring did
		// nothing and nobody noticed" failure Replace was added to end.
		defer wantPanic(t, "unregistered Key")
		newTestRegistry(t).Replace(replaceStub{key: "never_registered", value: "x"})
	})

	t.Run("invalid metadata", func(t *testing.T) {
		defer wantPanic(t, "invalid metadata")
		newTestRegistry(t).Replace(replaceStub{key: "", value: "x"})
	})
}

// TestRegisterStillRejectsDuplicates guards the rule Replace must not have
// weakened: adding a *second* skill under an existing name is an author
// error, and it is still a panic.
func TestRegisterStillRejectsDuplicates(t *testing.T) {
	defer wantPanic(t, "duplicate Key")
	newTestRegistry(t).Register(replaceStub{key: "alpha", value: "second"})
}

func wantPanic(t *testing.T, want string) {
	t.Helper()
	recovered := recover()
	if recovered == nil {
		t.Fatalf("expected a panic containing %q, got none", want)
	}
	if msg := strings.ToLower(toString(recovered)); !strings.Contains(msg, strings.ToLower(want)) {
		t.Fatalf("panic = %v, want it to mention %q", recovered, want)
	}
}

func toString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case error:
		return value.Error()
	default:
		return ""
	}
}

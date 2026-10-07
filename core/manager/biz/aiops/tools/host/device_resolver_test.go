package host

import (
	"context"
	"testing"
)

// TestDeviceResolver_ZeroDeviceID exercises the early return for
// deviceID==0 — guards against accidentally walking through to the
// junction lookup when the LLM omits device_id.
func TestDeviceResolver_ZeroDeviceID(t *testing.T) {
	r := NewDeviceResolver(nil, nil)
	got, err := r.ResolveEdgeID(context.Background(), 0)
	if err != nil {
		t.Fatalf("ResolveEdgeID(0): unexpected err %v", err)
	}
	if got != 0 {
		t.Errorf("ResolveEdgeID(0) = %d, want 0", got)
	}
}

// TestDeviceResolver_NilDependencies returns 0 when both repos are
// nil. The resolver MUST stay nil-safe so downstream tools surface a
// clean "no host link" message rather than panicking.
func TestDeviceResolver_NilDependencies(t *testing.T) {
	r := NewDeviceResolver(nil, nil)
	got, err := r.ResolveEdgeID(context.Background(), 42)
	if err != nil {
		t.Fatalf("ResolveEdgeID(42, nil deps): unexpected err %v", err)
	}
	if got != 0 {
		t.Errorf("ResolveEdgeID(42, nil deps) = %d, want 0", got)
	}
}

// TestResolveHostEdgeIsNilSafe pins the one behaviour the adapter used to
// provide. A tool assembled without a device usecase is a supported
// degraded configuration, so a nil resolver has to answer "no host link"
// rather than panic — and it has to keep answering that way after the
// adapter became a function.
func TestResolveHostEdgeIsNilSafe(t *testing.T) {
	got, err := ResolveHostEdge(context.Background(), nil, 7)
	if err != nil {
		t.Fatalf("ResolveHostEdge(nil): unexpected err %v", err)
	}
	if got != 0 {
		t.Errorf("ResolveHostEdge(nil) = %d, want 0", got)
	}

	// A real resolver with no backing usecases is the other nil-shaped
	// case, and it must not be confused with "no host link found".
	got, err = ResolveHostEdge(context.Background(), NewDeviceResolver(nil, nil), 7)
	if err != nil {
		t.Fatalf("ResolveHostEdge(no deps): unexpected err %v", err)
	}
	if got != 0 {
		t.Errorf("ResolveHostEdge(no deps) = %d, want 0", got)
	}
}

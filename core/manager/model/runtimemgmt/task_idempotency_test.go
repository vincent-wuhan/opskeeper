package runtimemgmt

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestNewIdempotencyContract_OK covers the canonical mutating case:
// the fingerprint of a contract is the SHA-256 of
// (payloadDigest | targetSystem | scope) — same input MUST produce
// the same fingerprint and the field set MUST round-trip via Matches.
func TestNewIdempotencyContract_OK(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	c, err := NewIdempotencyContract(
		"task-1", "tenant-1",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"preview-pg", "scope:build:42",
		"external-1", "producer.v1", now,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.TaskID != "task-1" || c.TenantID != "tenant-1" {
		t.Errorf("ids not preserved: %+v", c)
	}
	want := RecomputeFingerprint(c.PayloadDigest, c.TargetSystem, c.Scope)
	if !c.Matches(want) {
		t.Errorf("Matches(%q) = false, want true", want)
	}
	if len(c.Fingerprint) != 64 {
		t.Errorf("Fingerprint len = %d, want 64", len(c.Fingerprint))
	}
	if !strings.HasPrefix(c.IssuedAt.Format(time.RFC3339), "2026-10-16T01:00:00") {
		t.Errorf("IssuedAt = %v, want 2026-10-16T01:00:00Z", c.IssuedAt)
	}
}

// TestNewIdempotencyContract_SameInputSameFingerprint pins the
// determinism requirement: retries MUST hash-compare cleanly.
func TestNewIdempotencyContract_SameInputSameFingerprint(t *testing.T) {
	a, err := NewIdempotencyContract("t", "tenant", "p", "sys", "scope", "", "p", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewIdempotencyContract("t", "tenant", "p", "sys", "scope", "", "p", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("fp drift: %s vs %s", a.Fingerprint, b.Fingerprint)
	}
}

// TestNewIdempotencyContract_DiffInputDiffFingerprint guards against
// "scope collision" — two requests with the same payload digest but
// different target system or scope MUST hash-mismatch.
func TestNewIdempotencyContract_DiffInputDiffFingerprint(t *testing.T) {
	a, _ := NewIdempotencyContract("t", "tenant", "p", "sys-a", "scope", "", "p", time.Now())
	b, _ := NewIdempotencyContract("t", "tenant", "p", "sys-b", "scope", "", "p", time.Now())
	if a.Fingerprint == b.Fingerprint {
		t.Errorf("different target systems produced same fingerprint: %s", a.Fingerprint)
	}
	c, _ := NewIdempotencyContract("t", "tenant", "p", "sys-a", "scope-x", "", "p", time.Now())
	if a.Fingerprint == c.Fingerprint {
		t.Errorf("different scopes produced same fingerprint: %s", a.Fingerprint)
	}
}

// TestNewIdempotencyContract_RejectsEmpty covers the required-field
// gate. Every one of (task_id, tenant_id, payload digest, target
// system, scope) is mandatory; empty values produce
// ErrIdempotencyRequired, not a zero-fingerprint row.
func TestNewIdempotencyContract_RejectsEmpty(t *testing.T) {
	cases := []struct {
		name, taskID, tenantID, payload, sys, scope string
	}{
		{"empty task", "", "tenant", "p", "sys", "scope"},
		{"empty tenant", "t", "", "p", "sys", "scope"},
		{"empty payload", "t", "tenant", "", "sys", "scope"},
		{"empty system", "t", "tenant", "p", "", "scope"},
		{"empty scope", "t", "tenant", "p", "sys", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewIdempotencyContract(c.taskID, c.tenantID, c.payload, c.sys, c.scope, "", "p", time.Now())
			if !errors.Is(err, ErrIdempotencyRequired) {
				t.Errorf("err = %v, want ErrIdempotencyRequired", err)
			}
		})
	}
}

// TestMatches_Baseline covers the simple equality plus the empty-
// path negative branch. Pinned here so a future refactor cannot
// turn Matches() into identity-by-coincidence.
func TestMatches_Baseline(t *testing.T) {
	c := IdempotencyContract{Fingerprint: "abc"}
	if c.Matches("") {
		t.Errorf("Matches(\"\") = true, want false")
	}
	if !c.Matches("abc") {
		t.Errorf("Matches(\"abc\") = false, want true")
	}
	if c.Matches("def") {
		t.Errorf("Matches(\"def\") = true, want false")
	}
}

// TestComputePayloadDigest pins canonical hashing for the Task row.
func TestComputePayloadDigest(t *testing.T) {
	if got := ComputePayloadDigest(nil); len(got) != 64 {
		t.Errorf("nil digest = %q, want 64-hex chars", got)
	}
	a := ComputePayloadDigest([]byte(`{"x":1}`))
	b := ComputePayloadDigest([]byte(`{"x":2}`))
	if a == b {
		t.Errorf("distinct payloads produced same digest: %s", a)
	}
	if ComputePayloadDigest([]byte(`{"x":1}`)) != a {
		t.Errorf("payload digest is non-deterministic")
	}
}

// TestNewTaskID is the smoke test for the UUID generator. It does not
// fix any particular format other than "non-empty, length 36"; Go's
// uuid package is well-tested on its own.
func TestNewTaskID(t *testing.T) {
	a := NewTaskID()
	b := NewTaskID()
	if a == "" || b == "" {
		t.Fatalf("NewTaskID returned empty string")
	}
	if a == b {
		t.Errorf("NewTaskID returned duplicates: %s", a)
	}
	if len(a) != 36 || len(b) != 36 {
		t.Errorf("NewTaskID length: %q (%d) / %q (%d), want 36", a, len(a), b, len(b))
	}
}

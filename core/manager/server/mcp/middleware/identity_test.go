package middleware

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// ContextIdentity is the adapter that answers the AgentTeams routes' two
// questions. It is tested here rather than only from the consumer's side
// because this is the only place where both ends of the translation are
// visible: the consumer's routes read a three-field projection, and the
// translation from these six-field structs is writable nowhere else.
//
// The structural half — that this type satisfies the consumer's port at all —
// cannot be asserted from this file without importing the consumer, which is
// the dependency this cut exists to remove. That half lives in
// cmd/opskeeper/agentteams_identity_wiring_test.go, where both types are in
// scope. Splitting the claim across two files is not tidiness: each half is
// only provable from one of them.

// contextWithEverything builds a context carrying a resolved identity with
// every field set, including the three the projection does not have, and a
// trace with all three of its fields set.
//
// The unused fields are the point. A projection that quietly grew a field
// would still pass a test that only checks the values it knows about, so the
// width is asserted separately and this fixture makes sure there is something
// to leave behind.
func contextWithEverything() context.Context {
	ctx := WithIdentity(context.Background(), ResolvedIdentity{
		ConsumerName: "worker-investigator",
		APIKeyID:     "ak-7f3c9b21",
		Role:         "investigator",
		TenantID:     "tenant-eu-1",
		AllowedTools: []string{"host.read", "k8s.exec"},
		ResolvedAt:   time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
	})
	return WithTraceContext(ctx, TraceContext{
		TraceID: "0123456789abcdef0123456789abcdef",
		SpanID:  "0123456789abcdef",
		Raw:     "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
	})
}

func TestCallerFromProjectsTheThreeFieldsThatDecideAnything(t *testing.T) {
	got, ok := ContextIdentity{}.CallerFrom(contextWithEverything())
	if !ok {
		t.Fatal("a context carrying a resolved identity read as having none")
	}
	want := struct{ Consumer, Role, TenantID string }{
		Consumer: "worker-investigator",
		Role:     "investigator",
		TenantID: "tenant-eu-1",
	}
	if got.Consumer != want.Consumer || got.Role != want.Role || got.TenantID != want.TenantID {
		t.Errorf("got %+v, want consumer %q, role %q, tenant %q",
			got, want.Consumer, want.Role, want.TenantID)
	}
}

func TestCallerFromOnAnUntouchedContextIsNobody(t *testing.T) {
	got, ok := ContextIdentity{}.CallerFrom(context.Background())
	if ok {
		t.Error("a context this package never touched resolved an identity")
	}
	// A false answer that still carries fields is a worse answer: a caller
	// that ignores the bool would act on an empty consumer name as if it
	// were an actor.
	if !reflect.DeepEqual(got, domain.MCPCaller{}) {
		t.Errorf("the false answer carries %+v; it must be the zero value", got)
	}
}

// TestTraceFromFoldsPresenceAndEmptiness is the fold, asserted from the side
// that knows the difference exists.
//
// TraceContext is a value in the context, and an empty one is representable:
// the middleware writes a TraceContext whenever it runs, and the short-form
// protocol may not supply a trace id. So "there is a TraceContext" and "there
// is a trace" are two different questions, and the consumer asked both. This
// method answers only the second, which is the one the call sites were using
// to make their decision.
func TestTraceFromFoldsPresenceAndEmptiness(t *testing.T) {
	cases := []struct {
		name     string
		ctx      context.Context
		wantOK   bool
		traceID  string
		spanID   string
		explains string
	}{
		{
			name:     "nothing in the context at all",
			ctx:      context.Background(),
			wantOK:   false,
			explains: "an unauthenticated request has no trace",
		},
		{
			name: "a TraceContext that is present and empty",
			ctx: WithTraceContext(context.Background(), TraceContext{
				TraceID: "",
				SpanID:  "0123456789abcdef",
			}),
			wantOK: false,
			// The span is there and the trace id is not. Under the old
			// two-signal shape a caller could have seen the span; under
			// this one there is no trace to correlate with, and a span
			// without a trace belongs to nothing.
			explains: "a span with no trace id is not a trace",
		},
		{
			name: "a trace with no span",
			ctx: WithTraceContext(context.Background(), TraceContext{
				TraceID: "0123456789abcdef0123456789abcdef",
			}),
			wantOK:  true,
			traceID: "0123456789abcdef0123456789abcdef",
			explains: "the short-form protocol does not always carry a span, and the " +
				"caller decides whether an empty one is worth writing",
		},
		{
			name: "a trace and a span",
			ctx: WithTraceContext(context.Background(), TraceContext{
				TraceID: "0123456789abcdef0123456789abcdef",
				SpanID:  "0123456789abcdef",
				Raw:     "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
			}),
			wantOK:  true,
			traceID: "0123456789abcdef0123456789abcdef",
			spanID:  "0123456789abcdef",
			explains: "the unparsed Raw is kept here for a future OTel propagation and " +
				"is not something these routes read",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ContextIdentity{}.TraceFrom(c.ctx)
			if ok != c.wantOK {
				t.Fatalf("ok is %v, want %v: %s", ok, c.wantOK, c.explains)
			}
			if got.TraceID != c.traceID || got.SpanID != c.spanID {
				t.Errorf("got %+v, want trace %q span %q", got, c.traceID, c.spanID)
			}
		})
	}
}

// TestTheProjectionsCarryNoCredential pins the width from the producing side,
// because this is the file that would grow them.
//
// Six fields became three and three became two. The fields that did not come
// are APIKeyID, AllowedTools, ResolvedAt and Raw: a credential id, a tool
// allowlist, a resolution timestamp and an unparsed header. None of them
// decides anything on a route that has already been authenticated, and all of
// them would be one refactor away from being on the other side of a boundary.
//
// Width is measured on the type, not on a value: adding a fourth field to
// either struct leaves both sides of an equality comparison zero-valued at it,
// so a value comparison cannot see it happen. (This is the trap decision
// 247's projection test walked into, and the reason its own comment had to be
// rewritten.)
func TestTheProjectionsCarryNoCredential(t *testing.T) {
	got, ok := ContextIdentity{}.CallerFrom(contextWithEverything())
	if !ok {
		t.Fatal("fixture did not resolve")
	}
	// A stringly check is the honest one here: the assertion is that the
	// credential-bearing values are absent, and a field-by-field comparison
	// against a locally declared struct would itself be a second
	// declaration of the projection — the exact thing the cut removed.
	all := reflect.ValueOf(got)
	for i := 0; i < all.NumField(); i++ {
		field := all.Type().Field(i)
		if field.Name == "APIKeyID" || field.Name == "AllowedTools" || field.Name == "ResolvedAt" {
			t.Errorf("the projection grew a %s. It belongs to the authentication decision, "+
				"which already happened before any of these routes ran", field.Name)
		}
	}
	if all.NumField() != 3 {
		t.Errorf("the caller projection has %d fields, want 3", all.NumField())
	}

	trace, ok := ContextIdentity{}.TraceFrom(contextWithEverything())
	if !ok {
		t.Fatal("fixture did not resolve a trace")
	}
	tv := reflect.ValueOf(trace)
	for i := 0; i < tv.NumField(); i++ {
		if name := tv.Type().Field(i).Name; name == "Raw" {
			t.Error("the trace projection grew a Raw. It is the unparsed traceparent, kept " +
				"here for a future OTel propagation these routes do not do")
		}
	}
	if tv.NumField() != 2 {
		t.Errorf("the trace projection has %d fields, want 2", tv.NumField())
	}
}

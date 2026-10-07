package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// A pending approval row is the only record of a call once the process that
// proposed it is gone, so whatever Open does not read back was never stored.
//
// It used to rebuild the request with five columns and put the whole payload
// into Digest: a console that reconnected saw an approval card with no
// arguments, no blast radius, no target and no class, and echoing the
// "digest" it was given could not bind to the call. The request type's own
// comments say a decision without a matching digest is refused, so the
// feature was broken in the safe direction, which is exactly the kind of
// breakage nobody files a ticket for.

// TestOpenReturnsEveryColumnOfAQueuedCall is the round trip: propose a call,
// read the queue back the way a reconnected console does, and require the
// request to come back whole.
func TestOpenReturnsEveryColumnOfAQueuedCall(t *testing.T) {
	repo := newFakeApprovalRepo()
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	a := newTestInboxUsecase(t, repo, now)

	want := approvalRequest()
	rowID, err := a.Propose(context.Background(), want)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	// One column changes by design: the request's own id belongs to the
	// kernel's turn, while what a reconnected console can be given — and
	// what a decision is allowed to name — is the row's. The gate accepts
	// either, so this is the one value the round trip is expected to
	// replace rather than restore.
	want.ID = rowID

	got, err := a.Open(context.Background(), want.SessionID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Open returned %d requests, want the one that is pending", len(got))
	}
	assertSameApprovalRequest(t, got[0], want)
}

// TestTheFixtureCoversEveryColumn keeps the round trip honest: a column the
// fixture leaves zero is a column this test is not asking about, and adding
// one to ports.ApprovalRequest would otherwise make the test pass while
// checking less.
func TestTheFixtureCoversEveryColumn(t *testing.T) {
	req := approvalRequest()
	v := reflect.ValueOf(req)
	ty := v.Type()
	for i := 0; i < ty.NumField(); i++ {
		field := ty.Field(i)
		if !field.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("approvalRequest() leaves ports.ApprovalRequest.%s zero, so the Open round "+
				"trip would pass without asking what Open does with it", field.Name)
		}
	}
}

// assertSameApprovalRequest compares every column, including any added later.
// time.Time is compared by instant: the value makes a round trip through an
// RFC3339 string, and the round trip is the point rather than the wall-clock
// representation.
func assertSameApprovalRequest(t *testing.T, got, want ports.ApprovalRequest) {
	t.Helper()
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	ty := gv.Type()
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		g, w := gv.Field(i), wv.Field(i)
		if g.Type() == reflect.TypeOf(time.Time{}) {
			if !g.Interface().(time.Time).Equal(w.Interface().(time.Time)) {
				t.Errorf("%s = %v, want %v", name, g.Interface(), w.Interface())
			}
			continue
		}
		if !reflect.DeepEqual(g.Interface(), w.Interface()) {
			t.Errorf("%s = %#v, want %#v — a console renders the queue from this, and an "+
				"approval card that cannot say what it is asking about is not a decision", name,
				g.Interface(), w.Interface())
		}
	}
}

// TestOpenKeepsUnreadableRowsVisible: a row whose payload cannot be decoded is
// still pending work and must still be listed. What it must not do is invent
// a digest — an empty one fails the recompute and the decision is refused,
// where a plausible-looking wrong one could bind to the wrong call.
func TestOpenKeepsUnreadableRowsVisible(t *testing.T) {
	repo := newFakeApprovalRepo()
	a := newTestInboxUsecase(t, repo, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	if _, err := a.Propose(context.Background(), approvalRequest()); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	repo.created[0].PayloadJSON = "{not json"

	got, err := a.Open(context.Background(), "s-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Open returned %d requests, want the unreadable row listed anyway", len(got))
	}
	if got[0].Digest != "" {
		t.Errorf("Digest = %q, want empty: an unreadable payload has no digest and inventing one "+
			"is how a decision binds to the wrong call", got[0].Digest)
	}
	if got[0].ID == "" || got[0].Summary == "" {
		t.Errorf("the row's own columns were dropped too: %+v", got[0])
	}
}

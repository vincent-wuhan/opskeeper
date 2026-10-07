package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/agentkernel"
	bizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	modelapproval "github.com/vincent-wuhan/opskeeper/core/manager/model/approval"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// fakeApprovalRepo is an in-memory approval store. It records the row it was
// asked to create so a test can prove the payload carried the digest — the
// digest is what the kernel binds the eventual grant to, and a row that lost
// it would produce a grant the kernel refuses.
type fakeApprovalRepo struct {
	rows    map[string]*modelapproval.Approval
	created []*modelapproval.Approval
	decide  func(*modelapproval.Approval)
	nextID  int
}

// approvalRequest is a fully-populated ports.ApprovalRequest.
//
// It is a second copy of the helper the kernel's own tests carry, and the
// duplication is a consequence of decision 284 rather than an oversight: the
// adapter moved to the composition root, and a composition root cannot reach
// into another package's test files.
//
// The two copies are kept honest from opposite ends: this file's
// TestTheFixtureCoversEveryColumn and that package's
// TestThisPackageFixtureCoversEveryColumn both walk every exported field of
// ports.ApprovalRequest and fail on a zero one, so a field added to the port
// cannot be left at its zero value in either copy.
//
// The second of those two tests did not exist when this comment was first
// written. See that test's own comment for what happened.
func approvalRequest() ports.ApprovalRequest {
	return ports.ApprovalRequest{
		ID:          "call-1",
		SessionID:   "s-1",
		ToolName:    "restart_service",
		Class:       domain.ClassDestructive,
		Digest:      "digest-of-the-exact-call",
		Arguments:   []byte(`{"target":"web-1"}`),
		Summary:     "restart_service on web-1",
		BlastRadius: domain.RadiusPod,
		Target:      "web-1",
		ExpiresAt:   time.Date(2026, 5, 1, 10, 2, 0, 0, time.UTC),
	}
}

func newFakeApprovalRepo() *fakeApprovalRepo {
	return &fakeApprovalRepo{rows: map[string]*modelapproval.Approval{}}
}

func (f *fakeApprovalRepo) Create(_ context.Context, a *modelapproval.Approval) error {
	if a.ID == "" {
		f.nextID++
		a.ID = fmt.Sprintf("row-%d", f.nextID)
	}
	f.rows[a.ID] = a
	f.created = append(f.created, a)
	return nil
}

func (f *fakeApprovalRepo) Get(_ context.Context, id string) (*modelapproval.Approval, error) {
	a, ok := f.rows[id]
	if !ok {
		return nil, errors.New("not found")
	}
	if f.decide != nil {
		f.decide(a)
	}
	return a, nil
}

func (f *fakeApprovalRepo) List(_ context.Context, status string, limit int) ([]*modelapproval.Approval, error) {
	out := make([]*modelapproval.Approval, 0, len(f.rows))
	for _, a := range f.rows {
		if status == "" || a.Status == status {
			out = append(out, a)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeApprovalRepo) CountPending(context.Context) (int64, error) { return 0, nil }

func (f *fakeApprovalRepo) Decide(_ context.Context, id string, fields map[string]any) error {
	a, ok := f.rows[id]
	if !ok {
		return errors.New("not found")
	}
	if a.Status != modelapproval.StatusPending {
		// The real repo guards on pending so two people clicking at once
		// cannot overwrite each other. A fake that skipped the guard would
		// have let the race this file's tests are about look impossible.
		return errs.ErrNotFound
	}
	if s, ok := fields["status"].(string); ok {
		a.Status = s
	}
	if r, ok := fields["reason"].(string); ok {
		a.Reason = &r
	}
	if u, ok := fields["approved_by"].(uint64); ok {
		a.ApprovedBy = &u
	}
	if blob, ok := fields["signers_json"].(string); ok {
		a.SignersJSON = &blob
	}
	return nil
}

func (f *fakeApprovalRepo) AddSigner(_ context.Context, id, signersJSON string) error {
	a, ok := f.rows[id]
	if !ok {
		return errors.New("not found")
	}
	if a.Status != modelapproval.StatusPending {
		return errs.ErrNotFound
	}
	a.SignersJSON = &signersJSON
	return nil
}

func (f *fakeApprovalRepo) SetResult(context.Context, string, string, string, time.Time) error {
	return nil
}

func newTestInboxUsecase(t *testing.T, repo *fakeApprovalRepo, now time.Time) *InboxUsecase {
	t.Helper()
	a := NewInboxUsecase(bizapproval.NewUsecase(repo, nil))
	if a == nil {
		t.Fatal("NewInboxUsecase returned nil for a wired usecase")
	}
	a.now = func() time.Time { return now }
	a.poll = time.Millisecond
	return a
}

// TestProposeStoresTheDigestAndSession proves the two fields the rest of the
// flow depends on survive the round trip through the row: without the digest
// the kernel refuses the grant, and without the session a reconnecting
// console cannot find the card.
func TestProposeStoresTheDigestAndSession(t *testing.T) {
	repo := newFakeApprovalRepo()
	a := newTestInboxUsecase(t, repo, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))

	id, err := a.Propose(context.Background(), approvalRequest())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if id == "" {
		t.Fatal("no row id")
	}
	if len(repo.created) != 1 {
		t.Fatalf("created %d rows, want 1", len(repo.created))
	}
	row := repo.created[0]
	if row.Kind != KindAgentToolCall {
		t.Fatalf("kind = %q, want %q", row.Kind, KindAgentToolCall)
	}
	if row.SessionID != "s-1" {
		t.Fatalf("session = %q, want the request's session", row.SessionID)
	}
	var payload agentToolCallPayload
	if err := json.Unmarshal([]byte(row.PayloadJSON), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Digest != "digest-of-the-exact-call" {
		t.Fatalf("digest = %q, want the request's own", payload.Digest)
	}
	if payload.ExpiresAt == "" {
		t.Fatal("the request's deadline was not carried into the row")
	}
}

// TestAwaitMapsAnApprovalOntoAGrant is the happy path: the operator answered,
// and the kernel gets a grant it can bind.
func TestAwaitMapsAnApprovalOntoAGrant(t *testing.T) {
	repo := newFakeApprovalRepo()
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	a := newTestInboxUsecase(t, repo, now)
	repo.decide = func(row *modelapproval.Approval) { row.Status = modelapproval.StatusApproved }

	req := approvalRequest()
	id, err := a.Propose(context.Background(), req)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	d, err := a.Await(context.Background(), id)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if d.Decision != ports.ApprovalGranted {
		t.Fatalf("decision = %q, want a grant", d.Decision)
	}
	if d.RequestID != id {
		t.Fatalf("request id = %q, want the row", d.RequestID)
	}
}

// TestAwaitStopsWhenTheRequestExpired proves the wait is bounded by the
// kernel's own deadline rather than by a host-chosen one: the kernel refuses
// the call at that moment, so a row decided later authorises nothing and the
// operator must not be kept waiting for it.
func TestAwaitStopsWhenTheRequestExpired(t *testing.T) {
	repo := newFakeApprovalRepo()
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	a := newTestInboxUsecase(t, repo, now)
	// The row stays pending forever, and the clock is already past the
	// request's ExpiresAt when Await starts.
	a.now = func() time.Time { return now.Add(time.Hour) }

	req := approvalRequest()
	id, err := a.Propose(context.Background(), req)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	_, err = a.Await(context.Background(), id)
	var ge *ports.GateError
	if !errors.As(err, &ge) || ge.Reason != ports.GateExpired {
		t.Fatalf("err = %v, want an expiry", err)
	}
}

// TestAwaitRefusesARejectionWithTheOperatorsWords proves a "no" is reported
// as the operator's own decision, not as a generic failure the console would
// render as a broken approval.
func TestAwaitRefusesARejectionWithTheOperatorsWords(t *testing.T) {
	repo := newFakeApprovalRepo()
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	a := newTestInboxUsecase(t, repo, now)
	repo.decide = func(row *modelapproval.Approval) {
		row.Status = modelapproval.StatusRejected
		reason := "wrong host"
		row.Reason = &reason
	}

	id, err := a.Propose(context.Background(), approvalRequest())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	d, err := a.Await(context.Background(), id)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if d.Decision != ports.ApprovalDenied {
		t.Fatalf("decision = %q, want a denial", d.Decision)
	}
	if d.Note != "wrong host" {
		t.Fatalf("note = %q, want the operator's reason", d.Note)
	}
}

// TestOpenFiltersBySession proves a console rendering one conversation is not
// shown another's queue: the store is shared with every other producer, so
// the filter has to happen here.
func TestOpenFiltersBySession(t *testing.T) {
	repo := newFakeApprovalRepo()
	a := newTestInboxUsecase(t, repo, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))

	mine := approvalRequest()
	mine.ID = "call-1"
	if _, err := a.Propose(context.Background(), mine); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	other := approvalRequest()
	other.ID = "call-2"
	other.SessionID = "s-2"
	if _, err := a.Propose(context.Background(), other); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	got, err := a.Open(context.Background(), "s-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got) != 1 || got[0].ID == "" || got[0].ToolName == "" {
		t.Fatalf("open = %+v, want only this session's row", got)
	}
	if got[0].SessionID != "s-1" {
		t.Fatalf("session = %q, want s-1", got[0].SessionID)
	}

	// A session with nothing outstanding must not read as an error.
	empty, err := a.Open(context.Background(), "nobody")
	if err != nil || len(empty) != 0 {
		t.Fatalf("open(nobody) = %v, %v", empty, err)
	}
}

// TestANilUsecaseIsRefusedRatherThanPassedThrough is the assembly guard: a
// deployment that wired no inbox must not end up with a gate that grants.
func TestANilUsecaseIsRefusedRatherThanPassedThrough(t *testing.T) {
	if got := NewInboxUsecase(nil); got != nil {
		t.Fatalf("NewInboxUsecase(nil) = %+v, want nil", got)
	}
	var a *InboxUsecase
	if _, err := a.Propose(context.Background(), approvalRequest()); !errors.Is(err, agentkernel.ErrGateNotWired) {
		t.Fatalf("err = %v, want agentkernel.ErrGateNotWired", err)
	}
	if _, err := a.Await(context.Background(), "row-1"); !errors.Is(err, agentkernel.ErrGateNotWired) {
		t.Fatalf("err = %v, want agentkernel.ErrGateNotWired", err)
	}
}

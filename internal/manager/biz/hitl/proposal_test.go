package hitl

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// fakeRepo 是 in-memory 实现 biz/hitl.Repo 的 fake，用于测 Service 状态机。
//
// 加锁：Service.Pause / Resume 会在不同 goroutine 调用（IM 回调 path），
// 单测不需要 concurrency 但保留 mu 防止未来误用。
type fakeRepo struct {
	mu        sync.Mutex
	proposals map[string]*model.Proposal
	states    map[string]*model.ProposalState
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{proposals: map[string]*model.Proposal{}, states: map[string]*model.ProposalState{}}
}

func (r *fakeRepo) Create(_ context.Context, p *model.Proposal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proposals[p.ID] = p
	return nil
}

func (r *fakeRepo) CreateAgentTeamsIdempotent(_ context.Context, p *model.Proposal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.IdempotencyKey != nil {
		for _, existing := range r.proposals {
			if existing.IdempotencyKey != nil && *existing.IdempotencyKey == *p.IdempotencyKey {
				*p = *existing
				return nil
			}
		}
	}
	r.proposals[p.ID] = p
	return nil
}
func (r *fakeRepo) Get(_ context.Context, id string) (*model.Proposal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.proposals[id]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return p, nil
}
func (r *fakeRepo) List(_ context.Context, _ string, _, _ int) ([]*model.Proposal, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.Proposal, 0, len(r.proposals))
	for _, p := range r.proposals {
		out = append(out, p)
	}
	return out, int64(len(out)), nil
}
func (r *fakeRepo) CountByState(_ context.Context, state string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, p := range r.proposals {
		if p.State == state {
			n++
		}
	}
	return n, nil
}
func (r *fakeRepo) CountByLegacyKind(_ context.Context, kind string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, p := range r.proposals {
		if p.LegacyKind == kind {
			n++
		}
	}
	return n, nil
}
func (r *fakeRepo) Transition(_ context.Context, id, expectedFrom string, f model.TransitionFields) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	toState := f.ToState
	p, ok := r.proposals[id]
	if !ok {
		return errs.ErrNotFound
	}
	if p.State != expectedFrom {
		return errors.New("hitl: state conflict (expected=" + expectedFrom + ", actual=" + p.State + ")")
	}
	if !model.IsValidTransition(expectedFrom, toState) {
		return errors.New("hitl: invalid transition " + expectedFrom + " -> " + toState)
	}
	p.State = toState
	if f.ApprovedBy != nil {
		p.ApprovedBy = f.ApprovedBy
	}
	if f.RejectedBy != nil {
		p.RejectedBy = f.RejectedBy
	}
	if f.PausedBy != nil {
		p.PausedBy = f.PausedBy
	}
	if f.ResumedBy != nil {
		p.ResumedBy = f.ResumedBy
	}
	if f.Reason != nil {
		p.Reason = f.Reason
	}
	if f.ResultJSON != nil {
		p.ResultJSON = f.ResultJSON
	}
	if f.DecidedAt != nil {
		p.DecidedAt = f.DecidedAt
	}
	if f.ExecutedAt != nil {
		p.ExecutedAt = f.ExecutedAt
	}
	if f.PausedAt != nil {
		p.PausedAt = f.PausedAt
	}
	if f.ResumedAt != nil {
		p.ResumedAt = f.ResumedAt
	}
	if f.IncrementPauseStateVersion {
		p.PauseStateVersion++
	}
	if f.MatrixEventID != nil {
		p.MatrixEventID = *f.MatrixEventID
	}
	return nil
}
func (r *fakeRepo) SetResult(_ context.Context, id, state, resultJSON string, executedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.proposals[id]
	if !ok {
		return errs.ErrNotFound
	}
	p.State = state
	p.ResultJSON = &resultJSON
	p.ExecutedAt = &executedAt
	return nil
}
func (r *fakeRepo) UpsertState(_ context.Context, st *model.ProposalState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states[st.ProposalID] = st
	return nil
}
func (r *fakeRepo) LoadState(_ context.Context, pid string) (*model.ProposalState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.states[pid]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return st, nil
}
func (r *fakeRepo) DeleteState(_ context.Context, pid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.states, pid)
	return nil
}

// ───────────────────────────── Tests ─────────────────────────────

func newTestProposal() *model.Proposal {
	p := &model.Proposal{
		ID: "test-prop-1", Kind: "shell_command", Title: "test",
		PayloadJSON: "{}", ProposedBy: 7,
		Severity:    model.SeverityMutating,
		Sensitivity: model.SensitivityInternal,
		Source:      model.SourceAgent,
		State:       model.StatePending,
	}
	return p
}

func TestService_ApproveRejectExpire(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	ctx := context.Background()

	// approve
	if err := repo.Create(ctx, newTestProposal()); err != nil {
		t.Fatal(err)
	}
	r := "ok"
	if err := svc.Approve(ctx, "test-prop-1", ApproveOpts{By: 100, Reason: &r}); err != nil {
		t.Fatalf("Approve err: %v", err)
	}
	got, _ := svc.Get(ctx, "test-prop-1")
	if got.State != model.StateApproved {
		t.Errorf("state = %s, want approved", got.State)
	}
	if got.ApprovedBy == nil || *got.ApprovedBy != 100 {
		t.Errorf("approved_by = %v, want 100", got.ApprovedBy)
	}

	// reject
	p2 := &model.Proposal{ID: "test-prop-2", Kind: "x", PayloadJSON: "{}", State: model.StatePending, Severity: model.SeverityMutating}
	_ = repo.Create(ctx, p2)
	if err := svc.Reject(ctx, "test-prop-2", RejectOpts{By: 99, Reason: "no"}); err != nil {
		t.Fatalf("Reject err: %v", err)
	}
	got2, _ := svc.Get(ctx, "test-prop-2")
	if got2.State != model.StateRejected {
		t.Errorf("state = %s, want rejected", got2.State)
	}
	if got2.Reason == nil || *got2.Reason != "no" {
		t.Errorf("reason = %v", got2.Reason)
	}

	// expire
	p3 := &model.Proposal{ID: "test-prop-3", Kind: "x", PayloadJSON: "{}", State: model.StatePending, Severity: model.SeverityMutating}
	_ = repo.Create(ctx, p3)
	if err := svc.Expire(ctx, "test-prop-3"); err != nil {
		t.Fatalf("Expire err: %v", err)
	}
	got3, _ := svc.Get(ctx, "test-prop-3")
	if got3.State != model.StateExpired {
		t.Errorf("state = %s, want expired", got3.State)
	}
}

func TestService_PauseResume_FullCycle(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	ctx := context.Background()

	_ = repo.Create(ctx, newTestProposal())

	token := &ResumeToken{
		ProposalID:   42,
		LLMMessages:  []LLMMessage{{Role: "user", Content: "go"}},
		DBRowVersion: 1,
		CreatedAt:    time.Now().UTC(),
	}
	tokBytes, err := token.Serialize()
	if err != nil {
		t.Fatal(err)
	}

	// Pause
	if err := svc.Pause(ctx, "test-prop-1", PauseOpts{
		By:           1,
		Reason:       "need human input",
		ResumeTok:    tokBytes,
		StateVersion: 1,
	}); err != nil {
		t.Fatalf("Pause err: %v", err)
	}
	p, _ := svc.Get(ctx, "test-prop-1")
	if p.State != model.StatePaused {
		t.Errorf("state = %s, want paused", p.State)
	}
	if p.PauseStateVersion != 1 {
		t.Errorf("pause_state_version = %d, want 1", p.PauseStateVersion)
	}

	// Load token should succeed
	loaded, err := svc.LoadResumeToken(ctx, "test-prop-1")
	if err != nil {
		t.Fatalf("LoadResumeToken err: %v", err)
	}
	if loaded.ProposalID != 42 {
		t.Errorf("loaded ProposalID = %d, want 42", loaded.ProposalID)
	}

	// Stale token version should fail
	p.PauseStateVersion = 999
	if _, err := svc.LoadResumeToken(ctx, "test-prop-1"); err == nil {
		t.Error("stale token should error")
	}
	// Restore
	p.PauseStateVersion = 1

	// Resume
	if err := svc.Resume(ctx, "test-prop-1", ResumeOpts{By: 2, Reason: "got it"}); err != nil {
		t.Fatalf("Resume err: %v", err)
	}
	p, _ = svc.Get(ctx, "test-prop-1")
	if p.State != model.StateResumed {
		t.Errorf("state = %s, want resumed", p.State)
	}
	// state row should be deleted
	if _, err := repo.LoadState(ctx, "test-prop-1"); err == nil {
		t.Error("proposal_state should be deleted after resume")
	}
}

func TestService_Complete_FromApprovedAndExecuted(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	ctx := context.Background()

	p := newTestProposal()
	_ = repo.Create(ctx, p)
	_ = svc.Approve(ctx, p.ID, ApproveOpts{By: 1})

	now := time.Now().UTC()
	if err := svc.Complete(ctx, p.ID, CompleteOpts{ExecutedAt: now, ResultJSON: "ok"}); err != nil {
		t.Fatalf("Complete err: %v", err)
	}
	got, _ := svc.Get(ctx, p.ID)
	if got.State != model.StateExecuted {
		t.Errorf("state = %s, want executed", got.State)
	}
	if got.ExecutedAt == nil {
		t.Error("executed_at should be set")
	}
}

func TestService_Pause_FromRejectedIsForbidden(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	ctx := context.Background()

	p := newTestProposal()
	_ = repo.Create(ctx, p)
	_ = svc.Reject(ctx, p.ID, RejectOpts{By: 1, Reason: "no"})

	err := svc.Pause(ctx, p.ID, PauseOpts{By: 1, Reason: "x", ResumeTok: []byte("x"), StateVersion: 1})
	if err == nil {
		t.Error("Pause from rejected should error")
	}
}

// TestService_CreateProposal_Defaults 验证缺失字段自动填充默认值。
func TestService_CreateProposal_Defaults(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	p := &model.Proposal{
		ID: "p1", Kind: "k", PayloadJSON: "{}",
	}
	if err := svc.CreateProposal(context.Background(), p); err != nil {
		t.Fatalf("create err: %v", err)
	}
	if p.Source != model.SourceAgent {
		t.Errorf("source default = %s, want agent", p.Source)
	}
	if p.State != model.StatePending {
		t.Errorf("state default = %s, want pending", p.State)
	}
	if p.Severity != model.SeverityMutating {
		t.Errorf("severity default = %s, want mutating", p.Severity)
	}
	if p.Sensitivity != model.SensitivityInternal {
		t.Errorf("sensitivity default = %s, want internal", p.Sensitivity)
	}
}

// TestService_CreateProposal_KindRequired 验证必填字段检查。
func TestService_CreateProposal_KindRequired(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	err := svc.CreateProposal(context.Background(), &model.Proposal{ID: "x", PayloadJSON: "{}"})
	if err == nil {
		t.Error("expected error for missing Kind")
	}
}

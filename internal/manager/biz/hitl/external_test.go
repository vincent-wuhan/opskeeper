package hitl

// external_test.go — 门 A 外部决议 fail-closed 测试（BENY-886）。
//
// 用例与 walker/tests/vectors/change-proposal-v1.yaml decision_cases 一一对应：
//   - decision-valid-approve        → 执行
//   - decision-hash-mismatch-refuse → 409 拒绝
//   - decision-expired-refuse       → 410 拒绝
//   - decision-target-mismatch-refuse → 409 拒绝
//   - decision-rejected-no-execute  → 迁移到 rejected，不释放执行

import (
	"context"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

func newExternalTestProposal(t *testing.T, svc *Service, expiresAt time.Time) (string, string, string) {
	t.Helper()
	payload := AgentTeamsPayload{
		Fingerprint: "fp-001",
		RequestID:   "prp:20261101-001-01",
		IncidentID:  "inc:20261101-001",
		Action:      model.RecoveryActionResizePool,
		BlastRadius: "single_device",
		Resource:    "tgt:aliyun/pg-main",
		RoomID:      "room-1",
		Parameters:  model.RecoveryExecutionParameters{Command: "resize_pool", Reason: "pool exhaustion", PoolManifestID: "pg-pool-fixture-01", IncidentID: "inc:20261101-001"},
		RequestedAt: time.Now().UTC().Add(-time.Minute),
		ExpiresAt:   expiresAt,
	}
	hash, err := CanonicalPayloadHash(payload)
	if err != nil {
		t.Fatalf("canonical hash: %v", err)
	}
	payloadJSON, err := canonicalPayloadJSON(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	p := &model.Proposal{
		ID:          "11111111-2222-3333-4444-555555555555",
		Kind:        AgentTeamsProposalKind,
		Title:       "resize pool",
		Summary:     "pool exhaustion fix",
		PayloadJSON: string(payloadJSON),
		Source:      model.SourceAgent,
		MessageID:   payload.RequestID,
		State:       model.StatePending,
		ExpiresAt:   &expiresAt,
	}
	if err := svc.repo.Create(context.Background(), p); err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	return p.ID, payload.RequestID, hash
}

func TestDecideExternalApprove(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })
	id, ref, hash := newExternalTestProposal(t, svc, now.Add(time.Hour))

	snap, err := svc.DecideExternal(context.Background(), ExternalDecisionInput{
		ProposalID: id, ProposalRef: ref, TargetRef: "tgt:aliyun/pg-main",
		Decision: model.StateApproved, ApprovalHash: hash,
		Reason: "因果链清晰且可回滚", DecidedAt: now,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if snap.State != model.StateApproved {
		t.Fatalf("state = %s, want approved", snap.State)
	}
}

func TestDecideExternalHashMismatch(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })
	id, ref, _ := newExternalTestProposal(t, svc, now.Add(time.Hour))

	_, err := svc.DecideExternal(context.Background(), ExternalDecisionInput{
		ProposalID: id, ProposalRef: ref, TargetRef: "tgt:aliyun/pg-main",
		Decision: model.StateApproved, ApprovalHash: "0000000000000000000000000000000000000000000000000000000000000000",
		Reason: "篡改尝试", DecidedAt: now,
	})
	if err != ErrProposalMismatch {
		t.Fatalf("err = %v, want ErrProposalMismatch", err)
	}
	if p, _ := repo.Get(context.Background(), id); p.State != model.StatePending {
		t.Fatalf("state = %s, want pending（fail-closed 不迁移）", p.State)
	}
}

func TestDecideExternalExpired(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })
	id, ref, hash := newExternalTestProposal(t, svc, now.Add(-time.Minute))

	_, err := svc.DecideExternal(context.Background(), ExternalDecisionInput{
		ProposalID: id, ProposalRef: ref, TargetRef: "tgt:aliyun/pg-main",
		Decision: model.StateApproved, ApprovalHash: hash,
		Reason: "迟到的批准", DecidedAt: now.Add(30 * time.Minute),
	})
	if err != errs.ErrGone {
		t.Fatalf("err = %v, want ErrGone (410)", err)
	}
}

func TestDecideExternalTargetMismatch(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })
	id, ref, hash := newExternalTestProposal(t, svc, now.Add(time.Hour))

	_, err := svc.DecideExternal(context.Background(), ExternalDecisionInput{
		ProposalID: id, ProposalRef: ref, TargetRef: "tgt:aliyun/redis-cache",
		Decision: model.StateApproved, ApprovalHash: hash,
		Reason: "目标错位", DecidedAt: now,
	})
	if err != ErrProposalMismatch {
		t.Fatalf("err = %v, want ErrProposalMismatch", err)
	}
}

func TestDecideExternalRejectNoExecute(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })
	id, ref, _ := newExternalTestProposal(t, svc, now.Add(time.Hour))

	snap, err := svc.DecideExternal(context.Background(), ExternalDecisionInput{
		ProposalID: id, ProposalRef: ref, TargetRef: "tgt:aliyun/pg-main",
		Decision: model.StateRejected,
		Reason:   "影响面评估不足", DecidedAt: now,
	})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if snap.State != model.StateRejected {
		t.Fatalf("state = %s, want rejected", snap.State)
	}
	if p, _ := repo.Get(context.Background(), id); p.ApprovedBy != nil {
		t.Fatalf("rejected proposal must not set ApprovedBy")
	}
}

func TestDecideExternalProposalRefMismatch(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })
	id, _, hash := newExternalTestProposal(t, svc, now.Add(time.Hour))

	_, err := svc.DecideExternal(context.Background(), ExternalDecisionInput{
		ProposalID: id, ProposalRef: "prp:OTHER-99", TargetRef: "tgt:aliyun/pg-main",
		Decision: model.StateApproved, ApprovalHash: hash,
		Reason: "ref 猜测", DecidedAt: now,
	})
	if err != ErrProposalMismatch {
		t.Fatalf("err = %v, want ErrProposalMismatch", err)
	}
}

func TestDecideExternalIdempotentResend(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })
	id, ref, hash := newExternalTestProposal(t, svc, now.Add(time.Hour))

	in := ExternalDecisionInput{
		ProposalID: id, ProposalRef: ref, TargetRef: "tgt:aliyun/pg-main",
		Decision: model.StateApproved, ApprovalHash: hash,
		Reason: "幂等重发", DecidedAt: now,
	}
	if _, err := svc.DecideExternal(context.Background(), in); err != nil {
		t.Fatalf("first: %v", err)
	}
	snap, err := svc.DecideExternal(context.Background(), in)
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if snap.State != model.StateApproved {
		t.Fatalf("state = %s, want approved", snap.State)
	}
}

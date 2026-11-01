package hitl

// external.go — 外部决策面（paperclip walker）的门 A 决议入口（BENY-886）。
//
// 与 TransitionAgentTeams 的区别：后者绑定 MatrixEventID（IM 场景）；本入口绑定
// paperclip 契约字段（proposal_ref / target_ref / approval_hash），fail-closed：
// 任一校验失败即拒绝执行，不降级、不默认放行。
//
// 绑定规则（与 walker/contracts/change-proposal-v1.md §5 对齐）：
//  1. path id = opskeeper proposal UUID，必须存在
//  2. proposal_ref 必须等于 payload.RequestID（第二重绑定，防 ID 猜测）
//  3. target_ref 必须等于 payload.Resource（目标精确匹配）
//  4. approval_hash 必须等于存储 payload 的 CanonicalPayloadHash（防篡改）
//  5. now < ExpiresAt（过期即拒，不自动顺延）

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// ErrProposalExpired 表示决议到达时提案已过有效期（HTTP 410）。
var ErrProposalExpired = errs.ErrGone

// SourcePaperclip 标记外部决议来源（写入 MatrixEventID 列留痕，audit 可查）。
const SourcePaperclip = "paperclip:walker"

// ExternalDecisionInput 是外部决议的强类型入参（walker.opskeeper 子插件 POST 体）。
type ExternalDecisionInput struct {
	ProposalID   string    // path id（opskeeper proposal UUID）
	ProposalRef  string    // 契约 proposal_ref，必须 == payload.RequestID
	TargetRef    string    // 契约 target_ref，必须 == payload.Resource
	Decision     string    // approved | rejected
	ApprovalHash string    // approved 时必填 64 hex；必须 == CanonicalPayloadHash
	Reason       string    // 必填 1..512
	DecidedAt    time.Time // 决议产生时间（paperclip 侧）
}

// DecideExternal 处理外部决议：全部校验通过才迁移状态（fail-closed）。
func (s *Service) DecideExternal(ctx context.Context, in ExternalDecisionInput) (ProposalSnapshot, error) {
	if in.ProposalID == "" || in.ProposalRef == "" || in.TargetRef == "" ||
		(in.Decision != model.StateApproved && in.Decision != model.StateRejected) ||
		strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 512 ||
		strings.ContainsAny(in.Reason, "\x00\r\n") {
		return ProposalSnapshot{}, errs.ErrInvalid
	}
	if in.Decision == model.StateApproved && len(in.ApprovalHash) != 64 {
		return ProposalSnapshot{}, errs.ErrInvalid
	}

	proposal, err := s.repo.Get(ctx, in.ProposalID)
	if err != nil {
		return ProposalSnapshot{}, err
	}

	var payload AgentTeamsPayload
	if err := json.Unmarshal([]byte(proposal.PayloadJSON), &payload); err != nil {
		return ProposalSnapshot{}, fmt.Errorf("hitl: decode payload: %w", err)
	}
	hash, err := CanonicalPayloadHash(payload)
	if err != nil {
		return ProposalSnapshot{}, err
	}

	// 绑定校验 2/3：proposal_ref 与 target_ref 精确匹配
	if payload.RequestID != in.ProposalRef || payload.Resource != in.TargetRef {
		return ProposalSnapshot{}, ErrProposalMismatch
	}
	// 绑定校验 4：approval_hash（approved 时）
	if in.Decision == model.StateApproved && hash != in.ApprovalHash {
		return ProposalSnapshot{}, ErrProposalMismatch
	}
	// 绑定校验 5：未过期（fail-closed，过期不宽限）
	now := s.now()
	if proposal.ExpiresAt != nil && !now.Before(*proposal.ExpiresAt) {
		return ProposalSnapshot{}, ErrProposalExpired
	}
	// 幂等重发：已到达目标态且绑定一致（含来源前缀）→ 返回快照
	if proposal.State == in.Decision {
		expectedReason := SourcePaperclip + " " + in.Reason
		if proposal.Reason == nil || *proposal.Reason != expectedReason {
			return ProposalSnapshot{}, ErrProposalMismatch
		}
		return SnapshotForProposal(proposal)
	}
	if proposal.State != model.StatePending {
		return ProposalSnapshot{}, errs.ErrConflict
	}

	decidedAt := in.DecidedAt
	if decidedAt.IsZero() {
		decidedAt = now
	}
	reason := SourcePaperclip + " " + in.Reason
	source := SourcePaperclip
	fields := model.TransitionFields{
		ToState:       in.Decision,
		Reason:        &reason,
		DecidedAt:     &decidedAt,
		MatrixEventID: &source,
	}
	if err := s.repo.Transition(ctx, proposal.ID, model.StatePending, fields); err != nil {
		// 并发竞态：另一决议先到——按绑定一致性返回或报冲突
		current, getErr := s.repo.Get(ctx, in.ProposalID)
		if getErr != nil {
			return ProposalSnapshot{}, err
		}
		if current.State != in.Decision || current.Reason == nil || *current.Reason != reason {
			return ProposalSnapshot{}, errs.ErrConflict
		}
		return SnapshotForProposal(current)
	}
	return s.Snapshot(ctx, in.ProposalID)
}

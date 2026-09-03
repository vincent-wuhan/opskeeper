// Package hitl 实现 HITL（Human-in-the-Loop）pause/resume 能力。
//
// 路径 A P1-2 阶段 1 任务 1.6 — Proposal biz 层：CRUD + 状态机。
//
// 设计要点：
//   - 接口在消费方定义（pause.go 已有 PausePoint / PausePolicy）
//   - 状态机迁移由 Service 守门 — 校验合法迁移 + optimistic lock
//   - 所有持久化调用都带 ctx；错误用 %w 包装
//   - TransitionFields 在 model 包定义，biz/data 都引用同一份（避免循环依赖）
package hitl

import (
	"context"
	"errors"
	"fmt"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// Repo 是 biz/hitl 视角的持久化接口（data 层 Repo 实现此接口）。
//
// 字段含义与 internal/manager/data/hitl/store.Repo 一致；biz 层只关心
// 行为契约，不导入 data 包（gospec §架构）。
type Repo interface {
	Create(ctx context.Context, p *model.Proposal) error
	CreateAgentTeamsIdempotent(ctx context.Context, p *model.Proposal) error
	Get(ctx context.Context, id string) (*model.Proposal, error)
	List(ctx context.Context, state string, limit, offset int) ([]*model.Proposal, int64, error)
	CountByState(ctx context.Context, state string) (int64, error)
	CountByLegacyKind(ctx context.Context, legacyKind string) (int64, error)

	Transition(ctx context.Context, id, expectedFrom string, fields model.TransitionFields) error

	SetResult(ctx context.Context, id, state, resultJSON string, executedAt time.Time) error

	UpsertState(ctx context.Context, st *model.ProposalState) error
	LoadState(ctx context.Context, proposalID string) (*model.ProposalState, error)
	DeleteState(ctx context.Context, proposalID string) error
}

// ApproveOpts / RejectOpts / PauseOpts / ResumeOpts / CompleteOpts 是迁移
// helper 的强类型入参，避免调用方误传字段。
type ApproveOpts struct {
	By     uint64
	Reason *string
}

type RejectOpts struct {
	By     uint64
	Reason string // reason 必填，方便规范化
}

type PauseOpts struct {
	By           uint64
	Reason       string
	ResumeTok    []byte
	StateVersion int64
}

type ResumeOpts struct {
	By     uint64
	Reason string
}

type CompleteOpts struct {
	ExecutedAt time.Time
	ResultJSON string
	State      string // executed / failed / rolled_back；默认 executed
}

// Service 公开 biz/hitl 的对外操作。
type Service struct {
	repo Repo
	now  func() time.Time
}

// NewService 构造 service。
func NewService(repo Repo) *Service {
	return &Service{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock 注入当前时间（测试用）。
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// CreateProposal 创建新提案。
//
// ProposedBy = 0 表示系统提单（如 scheduler 自动 open）。
func (s *Service) CreateProposal(ctx context.Context, p *model.Proposal) error {
	if p == nil {
		return errors.New("hitl: nil proposal")
	}
	if p.Kind == "" {
		return errors.New("hitl: proposal.Kind required")
	}
	if p.Source == "" {
		p.Source = model.SourceAgent
	}
	if p.State == "" {
		p.State = model.StatePending
	}
	if p.Severity == "" {
		p.Severity = model.SeverityMutating
	}
	if p.Sensitivity == "" {
		p.Sensitivity = model.SensitivityInternal
	}
	return s.repo.Create(ctx, p)
}

// Get 简单 wrapper。
func (s *Service) Get(ctx context.Context, id string) (*model.Proposal, error) {
	return s.repo.Get(ctx, id)
}

// List 简单 wrapper。
func (s *Service) List(ctx context.Context, state string, limit, offset int) ([]*model.Proposal, int64, error) {
	return s.repo.List(ctx, state, limit, offset)
}

// Approve 是 pending → approved 的便捷迁移。
func (s *Service) Approve(ctx context.Context, id string, o ApproveOpts) error {
	return s.repo.Transition(ctx, id, model.StatePending,
		transitionToApprove(o, s.now()))
}

// Reject 是 pending → rejected 的便捷迁移。
func (s *Service) Reject(ctx context.Context, id string, o RejectOpts) error {
	return s.repo.Transition(ctx, id, model.StatePending,
		transitionToReject(o, s.now()))
}

// Expire 将 pending 推到 expired（cron 周期任务调用）。
func (s *Service) Expire(ctx context.Context, id string) error {
	return s.repo.Transition(ctx, id, model.StatePending,
		transitionToExpire(s.now()))
}

// Pause 是 pending/approved/resumed → paused 的迁移；同步写 proposal_state 行。
func (s *Service) Pause(ctx context.Context, id string, o PauseOpts) error {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("hitl: pause get: %w", err)
	}
	switch p.State {
	case model.StatePending, model.StateApproved, model.StateResumed:
		// ok
	default:
		return fmt.Errorf("hitl: cannot pause from state %s", p.State)
	}
	pausedAt := s.now()
	if err := s.repo.UpsertState(ctx, &model.ProposalState{
		ProposalID:   id,
		StateVersion: o.StateVersion,
		TokenBlob:    o.ResumeTok,
		CreatedAt:    pausedAt,
		UpdatedAt:    pausedAt,
	}); err != nil {
		return fmt.Errorf("hitl: pause upsert state: %w", err)
	}
	if err := s.repo.Transition(ctx, id, p.State,
		transitionToPause(o, pausedAt)); err != nil {
		_ = s.repo.DeleteState(ctx, id)
		return fmt.Errorf("hitl: pause transition: %w", err)
	}
	return nil
}

// Resume 从 paused 恢复；清理 proposal_state 行。
func (s *Service) Resume(ctx context.Context, id string, o ResumeOpts) error {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("hitl: resume get: %w", err)
	}
	if p.State != model.StatePaused {
		return fmt.Errorf("hitl: cannot resume from state %s", p.State)
	}
	resumedAt := s.now()
	if err := s.repo.Transition(ctx, id, model.StatePaused,
		transitionToResume(o, resumedAt)); err != nil {
		return fmt.Errorf("hitl: resume transition: %w", err)
	}
	if err := s.repo.DeleteState(ctx, id); err != nil {
		return fmt.Errorf("hitl: resume delete state: %w", err)
	}
	return nil
}

// Complete 是 approved/resumed → executed/failed/rolled_back 的终态迁移。
func (s *Service) Complete(ctx context.Context, id string, o CompleteOpts) error {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("hitl: complete get: %w", err)
	}
	fromState := p.State
	if fromState != model.StateApproved && fromState != model.StateResumed && fromState != model.StateExecuted {
		return fmt.Errorf("hitl: cannot complete from state %s", fromState)
	}
	return s.repo.Transition(ctx, id, fromState,
		transitionToComplete(o))
}

// LoadResumeToken 读 paused 提案的 ResumeToken + 校验 version。
func (s *Service) LoadResumeToken(ctx context.Context, id string) (*ResumeToken, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.State != model.StatePaused {
		return nil, fmt.Errorf("hitl: proposal %s not in paused state (state=%s)", id, p.State)
	}
	st, err := s.repo.LoadState(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("hitl: load state: %w", err)
	}
	tok, err := DeserializeResumeToken(st.TokenBlob)
	if err != nil {
		return nil, fmt.Errorf("hitl: deserialize: %w", err)
	}
	if err := tok.ValidateVersion(p.PauseStateVersion); err != nil {
		return nil, fmt.Errorf("hitl: resume token stale: %w", err)
	}
	return tok, nil
}

// SetResult 仅写结果，不改 state。
func (s *Service) SetResult(ctx context.Context, id, state, resultJSON string, executedAt time.Time) error {
	return s.repo.SetResult(ctx, id, state, resultJSON, executedAt)
}

func transitionToApprove(o ApproveOpts, now time.Time) model.TransitionFields {
	return model.TransitionFields{
		ToState:    model.StateApproved,
		ApprovedBy: &o.By,
		Reason:     o.Reason,
		DecidedAt:  &now,
	}
}

func transitionToReject(o RejectOpts, now time.Time) model.TransitionFields {
	r := o.Reason
	return model.TransitionFields{
		ToState:    model.StateRejected,
		RejectedBy: &o.By,
		Reason:     &r,
		DecidedAt:  &now,
	}
}

func transitionToExpire(now time.Time) model.TransitionFields {
	return model.TransitionFields{
		ToState:   model.StateExpired,
		DecidedAt: &now,
	}
}

func transitionToPause(o PauseOpts, now time.Time) model.TransitionFields {
	r := o.Reason
	by := o.By
	return model.TransitionFields{
		ToState:                    model.StatePaused,
		PausedBy:                   &by,
		Reason:                     &r,
		PausedAt:                   &now,
		IncrementPauseStateVersion: true,
	}
}

func transitionToResume(o ResumeOpts, now time.Time) model.TransitionFields {
	r := o.Reason
	by := o.By
	return model.TransitionFields{
		ToState:   model.StateResumed,
		ResumedBy: &by,
		Reason:    &r,
		ResumedAt: &now,
	}
}

func transitionToComplete(o CompleteOpts) model.TransitionFields {
	state := model.StateExecuted
	if o.State != "" {
		state = o.State
	}
	r := o.ResultJSON
	return model.TransitionFields{
		ToState:    state,
		ResultJSON: &r,
		ExecutedAt: &o.ExecutedAt,
	}
}

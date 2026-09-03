package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// RecoveryApprovalQuery binds an approved AgentTeams proposal to one recovery
// action. Empty optional fields are not allowed; every dimension is exact.
type RecoveryApprovalQuery struct {
	ProposalID string
	SessionID  string
	Kind       string
	Action     string
	Resource   string
	Execution  model.RecoveryExecutionParameters
	Now        time.Time
}

const recoveryExecutionLeaseDuration = 5 * time.Minute

func (r *Repo) ReserveApprovedForRecovery(ctx context.Context, query RecoveryApprovalQuery) error {
	if query.ProposalID == "" || query.SessionID == "" || query.Kind == "" ||
		query.Action == "" || query.Resource == "" || query.Execution.Command == "" || query.Now.IsZero() {
		return errs.ErrInvalid
	}
	proposal, err := r.Get(ctx, query.ProposalID)
	if err != nil {
		return err
	}
	if proposal.Kind != query.Kind || proposal.SessionID != query.SessionID ||
		proposal.State != model.StateApproved {
		return errs.ErrForbidden
	}
	if proposal.ExpiresAt == nil || !proposal.ExpiresAt.After(query.Now) {
		return errs.ErrForbidden
	}
	var payload model.AgentTeamsPayload
	if err := json.Unmarshal([]byte(proposal.PayloadJSON), &payload); err != nil {
		return fmt.Errorf("hitl/store: decode recovery payload: %w", err)
	}
	if payload.IncidentID != query.SessionID || payload.Action != query.Action ||
		payload.Resource != query.Resource {
		return errs.ErrForbidden
	}
	approvedJSON, err := payload.Parameters.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("hitl/store: marshal approved recovery parameters: %w", err)
	}
	requestedJSON, err := query.Execution.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("hitl/store: marshal requested recovery parameters: %w", err)
	}
	if !bytes.Equal(approvedJSON, requestedJSON) {
		return errs.ErrForbidden
	}
	leaseExpiresAt := query.Now.UTC().Add(recoveryExecutionLeaseDuration)
	if err := r.Transition(ctx, proposal.ID, model.StateApproved,
		model.TransitionFields{
			ToState:                 model.StateExecuting,
			ExecutionLeaseExpiresAt: &leaseExpiresAt,
		}); err != nil {
		return err
	}
	return nil
}

func (r *Repo) CompleteRecoveryExecution(ctx context.Context, proposalID string, success bool, resultJSON string, now time.Time) error {
	if proposalID == "" || now.IsZero() {
		return errs.ErrInvalid
	}
	proposal, err := r.Get(ctx, proposalID)
	if err != nil {
		return err
	}
	toState := model.StateFailed
	reason := "recovery execution failed"
	result := resultJSON
	if success {
		toState = model.StateExecuted
		reason = "recovery executed"
		if executionLeaseExpired(proposal, now) {
			success = false
			toState = model.StateFailed
			reason = "recovery execution lease expired"
			result = ""
		}
	}
	executedAt := now.UTC()
	return r.Transition(ctx, proposalID, model.StateExecuting,
		model.TransitionFields{
			ToState:    toState,
			Reason:     &reason,
			ResultJSON: &result,
			ExecutedAt: &executedAt,
		})
}

func (r *Repo) RecoverExpiredRecoveryExecution(ctx context.Context, proposalID string, now time.Time) error {
	if proposalID == "" || now.IsZero() {
		return errs.ErrInvalid
	}
	proposal, err := r.Get(ctx, proposalID)
	if err != nil {
		return err
	}
	if proposal.State != model.StateExecuting || !executionLeaseExpired(proposal, now) {
		return errs.ErrConflict
	}
	reason := "recovery execution lease expired"
	decidedAt := now.UTC()
	return r.Transition(ctx, proposalID, model.StateExecuting, model.TransitionFields{
		ToState:    model.StateFailed,
		Reason:     &reason,
		ExecutedAt: &decidedAt,
	})
}

func executionLeaseExpired(proposal *model.Proposal, now time.Time) bool {
	return proposal.ExecutionLeaseExpiresAt != nil && !proposal.ExecutionLeaseExpiresAt.After(now)
}

package hitl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

const AgentTeamsProposalKind = model.KindAgentTeams

var ErrProposalMismatch = errors.New("hitl: proposal fingerprint mismatch")
var fixtureManifestIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{7,127}$`)

type AgentTeamsPayload = model.AgentTeamsPayload

type AgentTeamsCreateInput struct {
	Kind        string
	Title       string
	Summary     string
	Payload     AgentTeamsPayload
	Source      string
	SessionID   string
	MessageID   string
	Severity    string
	Sensitivity string
	IMThreadID  string
	ExpiresAt   time.Time
	ProposedBy  uint64
}

type ProposalSnapshot struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	MessageID   string `json:"message_id"`
	PayloadHash string `json:"payload_hash"`
}

type AgentTeamsTransitionInput struct {
	ID            string
	ToState       string
	MessageID     string
	PayloadHash   string
	MatrixEventID string
	Reason        string
	DecidedBy     uint64
}

func CanonicalPayloadHash(payload AgentTeamsPayload) (string, error) {
	encoded, err := canonicalPayloadJSON(payload)
	if err != nil {
		return "", fmt.Errorf("hitl: marshal canonical payload: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func CanonicalRecoveryExecutionHash(parameters model.RecoveryExecutionParameters) (string, error) {
	encoded, err := parameters.CanonicalJSON()
	if err != nil {
		return "", fmt.Errorf("hitl: marshal canonical recovery parameters: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalPayloadJSON(payload AgentTeamsPayload) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil, err
	}
	return json.Marshal(values)
}

func SnapshotForProposal(proposal *model.Proposal) (ProposalSnapshot, error) {
	if proposal == nil {
		return ProposalSnapshot{}, errs.ErrNotFound
	}
	var payload AgentTeamsPayload
	if err := json.Unmarshal([]byte(proposal.PayloadJSON), &payload); err != nil {
		return ProposalSnapshot{}, fmt.Errorf("hitl: decode payload: %w", err)
	}
	hash, err := CanonicalPayloadHash(payload)
	if err != nil {
		return ProposalSnapshot{}, err
	}
	return ProposalSnapshot{
		ID:          proposal.ID,
		State:       proposal.State,
		MessageID:   proposal.MessageID,
		PayloadHash: hash,
	}, nil
}

func (s *Service) CreateAgentTeams(ctx context.Context, in AgentTeamsCreateInput) (ProposalSnapshot, error) {
	now := s.now()
	if in.Kind != AgentTeamsProposalKind || in.Source != model.SourceAgent ||
		in.Severity != model.SeverityDangerous || in.Sensitivity != model.SensitivityRestricted {
		return ProposalSnapshot{}, errs.ErrInvalid
	}
	if in.Title == "" || in.Summary == "" || in.IMThreadID == "" || in.ProposedBy == 0 {
		return ProposalSnapshot{}, errs.ErrInvalid
	}
	if err := validateAgentTeamsEnvelope(in, now); err != nil {
		return ProposalSnapshot{}, err
	}
	idempotencyKey := fmt.Sprintf("%s:%d:%s", in.Kind, len(in.MessageID), in.MessageID)
	payloadJSON, err := canonicalPayloadJSON(in.Payload)
	if err != nil {
		return ProposalSnapshot{}, fmt.Errorf("hitl: marshal payload: %w", err)
	}
	imThreadID := in.IMThreadID
	proposal := &model.Proposal{
		Kind:           in.Kind,
		Title:          in.Title,
		Summary:        in.Summary,
		PayloadJSON:    string(payloadJSON),
		Source:         in.Source,
		SessionID:      in.SessionID,
		MessageID:      in.MessageID,
		ProposedBy:     in.ProposedBy,
		Severity:       in.Severity,
		Sensitivity:    in.Sensitivity,
		State:          model.StatePending,
		ExpiresAt:      &in.ExpiresAt,
		IMThreadID:     &imThreadID,
		IdempotencyKey: &idempotencyKey,
	}
	if err := s.repo.CreateAgentTeamsIdempotent(ctx, proposal); err != nil {
		return ProposalSnapshot{}, err
	}
	return SnapshotForProposal(proposal)
}

func (s *Service) Snapshot(ctx context.Context, id string) (ProposalSnapshot, error) {
	proposal, err := s.repo.Get(ctx, id)
	if err != nil {
		return ProposalSnapshot{}, err
	}
	return SnapshotForProposal(proposal)
}

func (s *Service) TransitionAgentTeams(ctx context.Context, in AgentTeamsTransitionInput) (ProposalSnapshot, error) {
	if !validTransitionTarget(in.ToState) || in.ID == "" || in.MessageID == "" ||
		len(in.PayloadHash) != 64 || in.MatrixEventID == "" || strings.TrimSpace(in.Reason) == "" ||
		len(in.Reason) > 512 {
		return ProposalSnapshot{}, errs.ErrInvalid
	}
	if strings.ContainsAny(in.Reason, "\x00\r\n") {
		return ProposalSnapshot{}, errs.ErrInvalid
	}
	proposal, err := s.repo.Get(ctx, in.ID)
	if err != nil {
		return ProposalSnapshot{}, err
	}
	if err := assertProposalBinding(proposal, in.MessageID, in.PayloadHash, in.MatrixEventID); err != nil {
		return ProposalSnapshot{}, err
	}
	if proposal.State == in.ToState {
		if proposal.Reason == nil || *proposal.Reason != in.Reason {
			return ProposalSnapshot{}, ErrProposalMismatch
		}
		return SnapshotForProposal(proposal)
	}
	if proposal.State != model.StatePending {
		return ProposalSnapshot{}, errs.ErrConflict
	}
	now := s.now()
	fields := model.TransitionFields{
		ToState:       in.ToState,
		Reason:        &in.Reason,
		DecidedAt:     &now,
		MatrixEventID: &in.MatrixEventID,
	}
	if in.ToState == model.StateApproved {
		fields.ApprovedBy = &in.DecidedBy
	} else if in.ToState == model.StateRejected {
		fields.RejectedBy = &in.DecidedBy
	}
	if err := s.repo.Transition(ctx, proposal.ID, model.StatePending, fields); err != nil {
		current, getErr := s.repo.Get(ctx, in.ID)
		if getErr != nil {
			return ProposalSnapshot{}, err
		}
		bindingErr := assertProposalBinding(current, in.MessageID, in.PayloadHash, in.MatrixEventID)
		if current.State != in.ToState || bindingErr != nil || current.Reason == nil || *current.Reason != in.Reason {
			return ProposalSnapshot{}, err
		}
		return SnapshotForProposal(current)
	}
	return s.Snapshot(ctx, in.ID)
}

func validateAgentTeamsEnvelope(in AgentTeamsCreateInput, now time.Time) error {
	payload := in.Payload
	if payload.RequestID == "" || payload.IncidentID == "" || payload.Action == "" ||
		payload.BlastRadius == "" || payload.Resource == "" || payload.RoomID == "" ||
		payload.Fingerprint == "" || payload.RequestedAt.IsZero() || payload.ExpiresAt.IsZero() {
		return errs.ErrInvalid
	}
	if payload.RequestID != in.MessageID || payload.IncidentID != in.SessionID ||
		!payload.ExpiresAt.Equal(in.ExpiresAt) {
		return errs.ErrInvalid
	}
	if payload.BlastRadius != "single_device" && payload.BlastRadius != "cluster" && payload.BlastRadius != "tenant_wide" {
		return errs.ErrInvalid
	}
	if payload.Parameters.Command != payload.Action || strings.TrimSpace(payload.Parameters.Reason) == "" ||
		len(payload.Parameters.Reason) > 512 || strings.ContainsAny(payload.Parameters.Reason, "\x00\r\n") {
		return errs.ErrInvalid
	}
	switch payload.Action {
	case model.RecoveryActionRestartService:
		if payload.Parameters.DeviceID == 0 || payload.Parameters.Service == "" ||
			len(payload.Parameters.Service) > 255 || payload.Parameters.IncidentID != "" ||
			payload.Parameters.FixtureManifestID != "" {
			return errs.ErrInvalid
		}
	case model.RecoveryActionKillProcess:
		if payload.Parameters.IncidentID == "" || payload.Parameters.IncidentID != payload.IncidentID ||
			payload.Parameters.FixtureManifestID == "" ||
			!fixtureManifestIDPattern.MatchString(payload.Parameters.FixtureManifestID) ||
			payload.Resource != "host:fixture" ||
			payload.Parameters.DeviceID != 0 || payload.Parameters.Service != "" ||
			payload.Parameters.PoolManifestID != "" ||
			len(payload.Parameters.IncidentID) > 64 || len(payload.Parameters.FixtureManifestID) > 128 {
			return errs.ErrInvalid
		}
	case model.RecoveryActionResizePool:
		if payload.Parameters.IncidentID == "" || payload.Parameters.IncidentID != payload.IncidentID ||
			payload.Parameters.PoolManifestID == "" ||
			!fixtureManifestIDPattern.MatchString(payload.Parameters.PoolManifestID) ||
			payload.Resource != "pg:pool-fixture" ||
			payload.Parameters.DeviceID != 0 || payload.Parameters.Service != "" ||
			payload.Parameters.FixtureManifestID != "" ||
			len(payload.Parameters.IncidentID) > 64 || len(payload.Parameters.PoolManifestID) > 128 {
			return errs.ErrInvalid
		}
	default:
		return errs.ErrInvalid
	}
	if payload.RequestedAt.After(now.Add(time.Minute)) || !payload.ExpiresAt.After(payload.RequestedAt) ||
		payload.ExpiresAt.After(payload.RequestedAt.Add(4*time.Hour)) || !payload.ExpiresAt.After(now) {
		return errs.ErrInvalid
	}
	return nil
}

func assertProposalBinding(proposal *model.Proposal, messageID, payloadHash, matrixEventID string) error {
	snapshot, err := SnapshotForProposal(proposal)
	if err != nil {
		return err
	}
	if snapshot.MessageID != messageID || snapshot.PayloadHash != payloadHash {
		return ErrProposalMismatch
	}
	if proposal.State != model.StatePending && proposal.MatrixEventID != matrixEventID {
		return ErrProposalMismatch
	}
	return nil
}

func validTransitionTarget(state string) bool {
	return state == model.StateApproved || state == model.StateRejected || state == model.StateExpired
}

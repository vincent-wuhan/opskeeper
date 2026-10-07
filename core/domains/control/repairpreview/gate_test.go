package repairpreview

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGateEligible_RequiresIDsAndExactPassingAction(t *testing.T) {
	run := validRun()
	passing := validCandidate("candidate-a", "resize_pool")
	passing.Decision = DecisionPass
	run.Candidates = []Candidate{passing}
	repository := &stubReadRepository{runs: []Run{run}}
	gate := NewGate(repository, run.WorkloadFingerprint)
	ctx := context.Background()

	require.NoError(t, gate.Eligible(ctx, run.TenantID, run.IncidentID, run.ID, "candidate-a", "resize_pool"))
	require.ErrorIs(t, gate.Eligible(ctx, "", run.IncidentID, run.ID, "candidate-a", "resize_pool"), ErrNotFound)
	require.ErrorIs(t, gate.Eligible(ctx, "other", run.IncidentID, run.ID, "candidate-a", "resize_pool"), ErrNotFound)
	require.ErrorIs(t, gate.Eligible(ctx, run.TenantID, run.IncidentID, "missing", "candidate-a", "resize_pool"), ErrNotFound)
	require.ErrorIs(t, gate.Eligible(ctx, run.TenantID, run.IncidentID, run.ID, "candidate-a", "reset_pool"), ErrNotEligible)
}

func TestGateEligible_RejectsStaleWorkload(t *testing.T) {
	run := validRun()
	passing := validCandidate("candidate-a", "resize_pool")
	passing.Decision = DecisionPass
	run.Candidates = []Candidate{passing}
	gate := NewGate(&stubReadRepository{runs: []Run{run}}, "sha256:different")

	err := gate.Eligible(context.Background(), run.TenantID, run.IncidentID, run.ID, "candidate-a", "resize_pool")

	require.ErrorIs(t, err, ErrStaleWorkload)
}

func TestGateEligible_RejectsBaseline(t *testing.T) {
	run := validRun()
	baseline := validCandidate("baseline", "baseline")
	baseline.Kind = "baseline"
	baseline.Decision = DecisionPass
	run.Candidates = []Candidate{baseline}
	gate := NewGate(&stubReadRepository{runs: []Run{run}}, run.WorkloadFingerprint)

	err := gate.Eligible(context.Background(), run.TenantID, run.IncidentID, run.ID, "baseline", "baseline")

	require.ErrorIs(t, err, ErrNotEligible)
}

func TestGateEligible_RejectsNonPassCandidate(t *testing.T) {
	run := validRun()
	rejected := validCandidate("candidate-b", "reset_pool")
	rejected.BusinessProbePass = false
	rejected.Decision = DecisionReject
	run.Candidates = []Candidate{rejected}
	gate := NewGate(&stubReadRepository{runs: []Run{run}}, run.WorkloadFingerprint)

	err := gate.Eligible(context.Background(), run.TenantID, run.IncidentID, run.ID, "candidate-b", "reset_pool")

	require.ErrorIs(t, err, ErrNotEligible)
}

type stubReadRepository struct {
	runs []Run
	err  error
}

func (repository *stubReadRepository) Save(context.Context, Run) error { return nil }

func (repository *stubReadRepository) ListByIncident(_ context.Context, _, _ string, _ int) ([]Run, error) {
	return repository.runs, repository.err
}

func (repository *stubReadRepository) FindEligible(_ context.Context, _, _, _, _, _ string) (Candidate, error) {
	for _, run := range repository.runs {
		for _, candidate := range run.Candidates {
			if candidate.Decision == DecisionPass {
				return candidate, nil
			}
		}
	}
	return Candidate{}, errors.New("not found")
}

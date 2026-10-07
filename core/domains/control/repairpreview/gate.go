package repairpreview

import (
	"context"
	"errors"
)

var (
	ErrNotFound      = errors.New("repair preview not found")
	ErrStaleWorkload = errors.New("repair preview workload is stale")
	ErrNotEligible   = errors.New("repair preview candidate is not eligible")
)

type Gate interface {
	Eligible(ctx context.Context, tenantID, incidentID, runID, candidateID, action string) error
}

type repositoryGate struct {
	repository                  Repository
	expectedWorkloadFingerprint string
}

func NewGate(repository Repository, expectedWorkloadFingerprint string) Gate {
	return &repositoryGate{repository: repository, expectedWorkloadFingerprint: expectedWorkloadFingerprint}
}

func (gate *repositoryGate) Eligible(ctx context.Context, tenantID, incidentID, runID, candidateID, action string) error {
	if tenantID == "" || incidentID == "" || runID == "" || candidateID == "" || action == "" {
		return ErrNotFound
	}
	if gate.repository == nil {
		return ErrNotFound
	}
	if candidateID == BaselineCandidateID || action == BaselineAction {
		return ErrNotEligible
	}
	runs, err := gate.repository.ListByIncident(ctx, tenantID, incidentID, 50)
	if err != nil {
		return errors.New("read repair preview eligibility failed")
	}
	var selectedRun *Run
	for index := range runs {
		if runs[index].TenantID == tenantID && runs[index].IncidentID == incidentID && runs[index].ID == runID {
			selectedRun = &runs[index]
			break
		}
	}
	if selectedRun == nil {
		return ErrNotFound
	}
	if gate.expectedWorkloadFingerprint != "" && selectedRun.WorkloadFingerprint != gate.expectedWorkloadFingerprint {
		return ErrStaleWorkload
	}
	var selected *Candidate
	for index := range selectedRun.Candidates {
		candidate := &selectedRun.Candidates[index]
		if candidate.TenantID == tenantID && candidate.IncidentID == incidentID &&
			candidate.RunID == runID && candidate.CandidateID == candidateID {
			selected = candidate
			break
		}
	}
	if selected == nil {
		return ErrNotFound
	}
	if selected.Action != action || selected.Decision != DecisionPass {
		return ErrNotEligible
	}
	eligible, err := gate.repository.FindEligible(ctx, tenantID, incidentID, runID, candidateID, action)
	if err != nil || eligible.ID != selected.ID || eligible.Decision != DecisionPass {
		return ErrNotEligible
	}
	return nil
}

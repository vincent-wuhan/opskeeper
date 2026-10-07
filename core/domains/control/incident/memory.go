package incident

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const RRFConstant = 60.0

type Postmortem struct {
	ID                  string                `json:"id"`
	TenantID            string                `json:"tenant_id"`
	IncidentID          string                `json:"incident_id"`
	DatabaseType        string                `json:"database_type"`
	DatabaseVersion     string                `json:"database_version"`
	TopologyFingerprint string                `json:"topology_fingerprint"`
	FaultFingerprint    string                `json:"fault_fingerprint"`
	Symptom             PostmortemSymptom     `json:"symptom"`
	Diagnosis           PostmortemDiagnosis   `json:"diagnosis"`
	EffectiveActions    []PostmortemAction    `json:"effective_actions"`
	IneffectiveActions  []PostmortemAction    `json:"ineffective_actions"`
	RecoverySignals     []RecoveryObservation `json:"recovery_signals"`
	ApplicableBoundary  PostmortemBoundary    `json:"applicable_boundary"`
	SourceEvidence      []SourceEvidence      `json:"source_evidence"`
	ConfirmedBy         string                `json:"confirmed_by"`
	ConfirmedAt         time.Time             `json:"confirmed_at"`
}

type PostmortemSymptom struct {
	Summary      string   `json:"summary"`
	MetricNames  []string `json:"metric_names"`
	SnapshotRefs []string `json:"snapshot_refs"`
}

type PostmortemDiagnosis struct {
	RootCause              string   `json:"root_cause"`
	PreviouslyMisdiagnosed []string `json:"previously_misdiagnosed"`
	EvidenceRefs           []string `json:"evidence_refs"`
}

type PostmortemAction struct {
	Fingerprint string `json:"fingerprint"`
	Summary     string `json:"summary"`
	EvidenceRef string `json:"evidence_ref"`
}

type RecoveryObservation struct {
	Name        string    `json:"name"`
	ObservedAt  time.Time `json:"observed_at"`
	EvidenceRef string    `json:"evidence_ref"`
}

type PostmortemBoundary struct {
	DatabaseTypes        []string `json:"database_types"`
	TopologyFingerprints []string `json:"topology_fingerprints"`
	VersionRanges        []string `json:"version_ranges"`
}

type SourceEvidence struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

func (postmortem Postmortem) Validate() error {
	switch {
	case postmortem.TenantID == "", postmortem.IncidentID == "":
		return errors.New("incident postmortem: tenant and incident ids are required")
	case postmortem.DatabaseType == "", postmortem.DatabaseVersion == "":
		return errors.New("incident postmortem: database type and version are required")
	case postmortem.TopologyFingerprint == "", postmortem.FaultFingerprint == "":
		return errors.New("incident postmortem: topology and fault fingerprints are required")
	case postmortem.ConfirmedBy == "", postmortem.ConfirmedAt.IsZero():
		return errors.New("incident postmortem: confirmation actor and time are required")
	}
	if postmortem.ID != "" {
		if _, err := uuid.Parse(postmortem.ID); err != nil {
			return errors.New("incident postmortem: id must be a UUID")
		}
	}
	if err := postmortem.Symptom.Validate(); err != nil {
		return fmt.Errorf("symptom: %w", err)
	}
	if err := postmortem.Diagnosis.Validate(); err != nil {
		return fmt.Errorf("diagnosis: %w", err)
	}
	for index, action := range append(append([]PostmortemAction{}, postmortem.EffectiveActions...), postmortem.IneffectiveActions...) {
		if action.Fingerprint == "" || action.Summary == "" || action.EvidenceRef == "" {
			return fmt.Errorf("postmortem action %d: fingerprint, summary, and evidence are required", index)
		}
	}
	if len(postmortem.RecoverySignals) == 0 {
		return errors.New("incident postmortem: at least one recovery signal is required")
	}
	for index, signal := range postmortem.RecoverySignals {
		if signal.Name == "" || signal.ObservedAt.IsZero() || signal.EvidenceRef == "" {
			return fmt.Errorf("recovery signal %d: name, observed_at, and evidence are required", index)
		}
	}
	if len(postmortem.SourceEvidence) == 0 {
		return errors.New("incident postmortem: source evidence is required")
	}
	return nil
}

func (symptom PostmortemSymptom) Validate() error {
	switch {
	case symptom.Summary == "":
		return errors.New("summary is required")
	case len(symptom.MetricNames) == 0:
		return errors.New("metric names are required")
	case len(symptom.SnapshotRefs) == 0:
		return errors.New("snapshot references are required")
	}
	return nil
}

func (diagnosis PostmortemDiagnosis) Validate() error {
	switch {
	case diagnosis.RootCause == "":
		return errors.New("root cause is required")
	case len(diagnosis.EvidenceRefs) == 0:
		return errors.New("evidence references are required")
	}
	return nil
}

type RankedCandidate struct {
	CandidateRef string `json:"candidate_ref"`
	VectorRank   int    `json:"vector_rank"`
	KeywordRank  int    `json:"keyword_rank"`
}

type RecallCandidate struct {
	CandidateRef   string  `json:"candidate_ref"`
	VectorRank     int     `json:"vector_rank"`
	KeywordRank    int     `json:"keyword_rank"`
	RRFScore       float64 `json:"rrf_score"`
	Selected       bool    `json:"selected"`
	RejectedReason string  `json:"rejected_reason,omitempty"`
}

func SelectTopRRF(candidates []RankedCandidate) ([]RecallCandidate, error) {
	if len(candidates) == 0 {
		return nil, errors.New("incident recall: at least one candidate is required")
	}
	seen := make(map[string]bool, len(candidates))
	for index, candidate := range candidates {
		switch {
		case candidate.CandidateRef == "":
			return nil, fmt.Errorf("incident recall candidate %d: reference is required", index)
		case candidate.VectorRank < 0 || candidate.KeywordRank < 0:
			return nil, fmt.Errorf("incident recall candidate %d: ranks cannot be negative", index)
		case seen[candidate.CandidateRef]:
			return nil, fmt.Errorf("incident recall candidate %d: duplicate reference", index)
		}
		seen[candidate.CandidateRef] = true
	}

	results := make([]RecallCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		score := 0.0
		if candidate.VectorRank > 0 {
			score += 1 / (RRFConstant + float64(candidate.VectorRank))
		}
		if candidate.KeywordRank > 0 {
			score += 1 / (RRFConstant + float64(candidate.KeywordRank))
		}
		results = append(results, RecallCandidate{
			CandidateRef: candidate.CandidateRef, VectorRank: candidate.VectorRank,
			KeywordRank: candidate.KeywordRank, RRFScore: score,
		})
	}
	sort.Slice(results, func(left, right int) bool {
		if results[left].RRFScore == results[right].RRFScore {
			if leftPriority := recallSourcePriority(results[left].CandidateRef); leftPriority != recallSourcePriority(results[right].CandidateRef) {
				return leftPriority < recallSourcePriority(results[right].CandidateRef)
			}
			return results[left].CandidateRef < results[right].CandidateRef
		}
		return results[left].RRFScore > results[right].RRFScore
	})
	results[0].Selected = true
	for index := 1; index < len(results); index++ {
		results[index].RejectedReason = "lower_rrf_score"
	}
	return results, nil
}

func recallSourcePriority(candidateRef string) int {
	switch {
	case strings.HasPrefix(candidateRef, "runbook:"):
		return 0
	case strings.HasPrefix(candidateRef, "knowledge:"):
		return 1
	default:
		return 2
	}
}

func mustJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode postmortem JSON: %w", err)
	}
	return string(encoded), nil
}

package repairpreview

type CompactSummary struct {
	IncidentID          string    `json:"incident_id"`
	RunID               string    `json:"run_id"`
	SeedFingerprint     string    `json:"seed_fingerprint"`
	WorkloadFingerprint string    `json:"workload_fingerprint"`
	ControlledLoad      bool      `json:"controlled_load"`
	IsolationBoundary   string    `json:"isolation_boundary"`
	Baseline            Candidate `json:"baseline"`
	Passing             Candidate `json:"passing"`
	Rejected            Candidate `json:"rejected"`
}

func BuildCompactSummary(runs []Run) (CompactSummary, error) {
	for _, run := range runs {
		var baseline Candidate
		var passing Candidate
		var rejected Candidate
		for _, candidate := range run.Candidates {
			if candidate.IsBaseline() && baseline.CandidateID == "" {
				baseline = candidate
				continue
			}
			if candidate.Decision == DecisionPass && passing.CandidateID == "" {
				passing = candidate
				continue
			}
			if candidate.Decision != DecisionPass && rejected.CandidateID == "" {
				rejected = candidate
			}
		}
		if baseline.CandidateID == "" || passing.CandidateID == "" {
			continue
		}
		return CompactSummary{
			IncidentID: run.IncidentID, RunID: run.ID, SeedFingerprint: run.SeedFingerprint,
			WorkloadFingerprint: run.WorkloadFingerprint, ControlledLoad: run.ControlledLoad,
			IsolationBoundary: run.IsolationBoundary, Baseline: baseline, Passing: passing,
			Rejected: rejected,
		}, nil
	}
	return CompactSummary{}, nil
}

func BoundArchiveRuns(runs []Run) []Run {
	if len(runs) > 3 {
		runs = runs[:3]
	}
	candidateBudget := 12
	for runIndex := range runs {
		baseline := Candidate{}
		nonBaseline := make([]Candidate, 0, len(runs[runIndex].Candidates))
		for _, candidate := range runs[runIndex].Candidates {
			if candidate.IsBaseline() {
				if baseline.CandidateID == "" {
					baseline = candidate
				}
				continue
			}
			nonBaseline = append(nonBaseline, candidate)
		}
		if len(nonBaseline) > candidateBudget {
			nonBaseline = nonBaseline[:candidateBudget]
		}
		candidateBudget -= len(nonBaseline)
		bounded := nonBaseline
		if baseline.CandidateID != "" {
			bounded = append([]Candidate{baseline}, nonBaseline...)
		}
		runs[runIndex].Candidates = bounded
		if candidateBudget <= 0 {
			runs = runs[:runIndex+1]
			break
		}
	}
	return runs
}

func TotalCandidates(runs []Run) int {
	total := 0
	for _, run := range runs {
		for _, candidate := range run.Candidates {
			if !candidate.IsBaseline() {
				total++
			}
		}
	}
	return total
}

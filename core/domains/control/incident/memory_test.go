package incident

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSelectTopRRF_MergesVectorAndKeywordRankings(t *testing.T) {
	candidates, err := SelectTopRRF([]RankedCandidate{
		{CandidateRef: "vector-only", VectorRank: 1},
		{CandidateRef: "balanced", VectorRank: 3, KeywordRank: 2},
		{CandidateRef: "keyword-only", KeywordRank: 1},
	})
	require.NoError(t, err)
	require.Equal(t, "balanced", candidates[0].CandidateRef)
	require.True(t, candidates[0].Selected)
	require.Equal(t, 1/(RRFConstant+3)+1/(RRFConstant+2), candidates[0].RRFScore)
	require.False(t, candidates[1].Selected)
	require.Equal(t, "lower_rrf_score", candidates[1].RejectedReason)
}

func TestSelectTopRRF_RejectsDuplicateCandidate(t *testing.T) {
	_, err := SelectTopRRF([]RankedCandidate{
		{CandidateRef: "same", VectorRank: 1},
		{CandidateRef: "same", KeywordRank: 1},
	})
	require.ErrorContains(t, err, "duplicate")
}

func TestSQLRepository_SaveQueryRunbookAndRRFLogs(t *testing.T) {
	repository, _ := setupRepository(t)
	postmortem := testPostmortem("INC-MEM-001", "pool_capacity_exhausted")
	require.NoError(t, repository.SaveRunbook(context.Background(), postmortem))

	postmortem.Diagnosis.RootCause = "pool capacity exhausted after slow request growth"
	postmortem.EffectiveActions = []PostmortemAction{{
		Fingerprint: "pool:resize:90:120", Summary: "Resize pool and recycle idle sessions",
		EvidenceRef: "evidence/incidents/INC-MEM-001/action.json",
	}}
	require.NoError(t, repository.SaveRunbook(context.Background(), postmortem))

	stored, err := repository.ListRunbooks(context.Background(), "opskeeper-demo", "PostgreSQL", "pool_capacity_exhausted")
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, postmortem.Diagnosis.RootCause, stored[0].Diagnosis.RootCause)
	require.Equal(t, postmortem.EffectiveActions, stored[0].EffectiveActions)

	ranked, err := SelectTopRRF([]RankedCandidate{
		{CandidateRef: "runbook:INC-MEM-001", VectorRank: 2, KeywordRank: 1},
		{CandidateRef: "runbook:INC-OTHER", VectorRank: 1, KeywordRank: 5},
	})
	require.NoError(t, err)
	recalledAt := time.Date(2026, 8, 29, 17, 0, 0, 0, time.UTC)
	logs := make([]RecallLog, 0, len(ranked))
	for _, candidate := range ranked {
		logs = append(logs, RecallLog{
			TenantID: "opskeeper-demo", IncidentID: "INC-RECALL-001",
			QueryText:    "pool waiters rising with active connections near max",
			CandidateRef: candidate.CandidateRef, VectorRank: candidate.VectorRank,
			KeywordRank: candidate.KeywordRank, RRFScore: candidate.RRFScore,
			Selected: candidate.Selected, RejectedReason: candidate.RejectedReason,
			RecalledAt: recalledAt,
		})
	}
	require.NoError(t, repository.AppendRecallLogs(context.Background(), logs))
	storedLogs, err := repository.ListRecallLogs(context.Background(), "opskeeper-demo", "INC-RECALL-001")
	require.NoError(t, err)
	require.Len(t, storedLogs, 2)
	require.True(t, storedLogs[0].Selected)
	require.Equal(t, "runbook:INC-MEM-001", storedLogs[0].CandidateRef)
	require.Equal(t, "lower_rrf_score", storedLogs[1].RejectedReason)
}

func testPostmortem(incidentID, faultFingerprint string) Postmortem {
	confirmedAt := time.Date(2026, 8, 29, 16, 30, 0, 0, time.UTC)
	return Postmortem{
		TenantID: "opskeeper-demo", IncidentID: incidentID, DatabaseType: "PostgreSQL",
		DatabaseVersion: "16.4", TopologyFingerprint: "pg16-primary-app-pool-v3",
		FaultFingerprint: faultFingerprint,
		Symptom: PostmortemSymptom{
			Summary:      "Pool waiters rise while active connections approach max",
			MetricNames:  []string{"pg_connections_active", "app_pool_waiters", "app_pool_wait_latency_p95"},
			SnapshotRefs: []string{"evidence/incidents/" + incidentID + "/pg-stat-activity.json"},
		},
		Diagnosis: PostmortemDiagnosis{
			RootCause: "pool capacity exhausted", PreviouslyMisdiagnosed: []string{"database deadlock"},
			EvidenceRefs: []string{"evidence/incidents/" + incidentID + "/diagnosis.json"},
		},
		EffectiveActions: []PostmortemAction{{
			Fingerprint: "pool:resize:90:120", Summary: "Resize application pool",
			EvidenceRef: "evidence/incidents/" + incidentID + "/action.json",
		}},
		RecoverySignals: []RecoveryObservation{{
			Name: "pool request succeeds with sub-100ms wait", ObservedAt: confirmedAt,
			EvidenceRef: "evidence/incidents/" + incidentID + "/recovery-check.json",
		}},
		ApplicableBoundary: PostmortemBoundary{
			DatabaseTypes: []string{"PostgreSQL"}, TopologyFingerprints: []string{"pg16-primary-app-pool-v3"},
			VersionRanges: []string{"16.x"},
		},
		SourceEvidence: []SourceEvidence{{Kind: "timeline", Ref: "evidence/incidents/" + incidentID + "/timeline.jsonl"}},
		ConfirmedBy:    "dba-oncall", ConfirmedAt: confirmedAt,
	}
}

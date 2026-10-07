package repairpreview

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExecute_ProducesDeterministicCandidateEvidence(t *testing.T) {
	dsn := os.Getenv("OPSKEEPER_REPAIR_PREVIEW_TEST_DSN")
	if dsn == "" {
		t.Skip("set OPSKEEPER_REPAIR_PREVIEW_TEST_DSN to run the PostgreSQL preview integration test")
	}
	database, err := openPreviewDatabase(dsn)
	require.NoError(t, err)
	defer database.Close()

	spec := testWorkload(t)
	spec.Samples = 12
	spec.Concurrency = 4
	spec.RuntimeBinding = WorkloadBinding{
		RunID: "017f2b01-4001-4000-8000-000000000001", TenantID: "opskeeper-demo", IncidentID: "INC-REPAIR-RUNNER",
	}
	first, err := Execute(context.Background(), database, spec)
	require.NoError(t, err)
	require.NoError(t, first.Validate())
	require.Len(t, first.Candidates, 3)
	require.Equal(t, "baseline", first.Candidates[0].CandidateID)
	require.Equal(t, "baseline", first.Candidates[0].Kind)
	require.Equal(t, DecisionPass, first.Candidates[0].Decision)
	require.Equal(t, "none", first.Candidates[0].WriteImpact)
	require.Zero(t, first.Candidates[0].StorageDeltaBytes)
	require.Greater(t, first.Candidates[0].AverageLatencyMS, 0.0)
	require.Equal(t, DecisionPass, first.Candidates[1].Decision)
	require.True(t, first.Candidates[1].Consistent)
	require.Greater(t, first.Candidates[1].AverageLatencyMS, 0.0)
	require.Greater(t, first.Candidates[1].MedianLatencyMS, 0.0)
	require.Greater(t, first.Candidates[1].P95LatencyMS, 0.0)
	require.Equal(t, 12, first.Candidates[1].SampleCount)
	require.Equal(t, DecisionReject, first.Candidates[2].Decision)
	require.False(t, first.Candidates[2].BusinessProbePass)
	require.Contains(t, first.Candidates[2].RejectionReason, "business probe")
	require.Equal(
		t,
		"Controlled fixed-workload reconstruction in disposable preview-pg; original active sessions are not copied.",
		first.IsolationBoundary,
	)

	spec.RuntimeBinding.RunID = "017f2b01-4002-4000-8000-000000000002"
	second, err := Execute(context.Background(), database, spec)
	require.NoError(t, err)
	require.Equal(t, first.Candidates[1].ResultChecksum, second.Candidates[1].ResultChecksum)

	var schemas int
	prefix := repairPreviewSchemaPrefix(first.ID)[:12]
	require.NoError(t, database.QueryRow(
		"SELECT COUNT(*) FROM pg_namespace WHERE nspname LIKE ?", prefix+"%",
	).Scan(&schemas))
	require.Zero(t, schemas)
}

func TestPercentile(t *testing.T) {
	require.InDelta(t, 95.5, percentile([]float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 95), 0.001)
	require.InDelta(t, 10.0, percentile([]float64{10}, 95), 0.001)
}

func TestNewBaselineCandidate_MarksPreviewEligibility(t *testing.T) {
	candidate := newBaselineCandidate(WorkloadBinding{
		RunID:      "017f2b01-4001-4000-8000-000000000001",
		TenantID:   "opskeeper-demo",
		IncidentID: "INC-REPAIR-RUNNER",
	}, branchResult{
		schemaName:        "rp_test_baseline",
		checksum:          "checksum",
		latencies:         []float64{10, 20, 30},
		tps:               3,
		businessProbePass: true,
	})

	require.Equal(t, DecisionPass, candidate.Decision)
	require.Empty(t, candidate.RejectionReason)
	require.NoError(t, candidate.Validate())
}

func TestRepairPreviewSchemaPrefix(t *testing.T) {
	value := repairPreviewSchemaPrefix("run-value")
	require.Contains(t, value, "rp_")
	require.NotContains(t, value, "run-value")
}

func TestSafeSchemaLabel_IsBounded(t *testing.T) {
	value := safeSchemaLabel(strings.Repeat("unsafe-", 20))

	require.Len(t, value, 47)
	require.Regexp(t, `^[a-z0-9_]+$`, value)
}

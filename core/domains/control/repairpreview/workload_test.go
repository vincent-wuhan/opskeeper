package repairpreview

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkloadFingerprint_IsStableAcrossMapOrdering(t *testing.T) {
	first := testWorkload(t)
	first.Labels = map[string]string{"zone": "bja", "environment": "preview"}
	second := testWorkload(t)
	second.Labels = map[string]string{"environment": "preview", "zone": "bja"}

	require.Equal(t, first.WorkloadFingerprint(), second.WorkloadFingerprint())
	require.Equal(t, first.SeedFingerprint(), second.SeedFingerprint())
	require.Equal(t, "sha256:seed-v1", first.SeedFingerprint())
	require.True(t, strings.HasPrefix(first.WorkloadFingerprint(), "sha256:"), first.WorkloadFingerprint())
}

func TestWorkloadFingerprint_ChangesWithMaterialFields(t *testing.T) {
	base := testWorkload(t)

	changed := base
	changed.Queries = append([]QuerySpec(nil), base.Queries...)
	changed.Queries[0].SQL = "SELECT customer_id, balance, status FROM repair_preview_accounts ORDER BY customer_id"
	require.NotEqual(t, base.WorkloadFingerprint(), changed.WorkloadFingerprint())

	changed = base
	changed.Samples++
	require.NotEqual(t, base.WorkloadFingerprint(), changed.WorkloadFingerprint())

	changed = base
	changed.Concurrency++
	require.NotEqual(t, base.WorkloadFingerprint(), changed.WorkloadFingerprint())

	changed = base
	changed.QueryTimeoutMS++
	require.NotEqual(t, base.WorkloadFingerprint(), changed.WorkloadFingerprint())

	changed = base
	changed.Candidates = append([]CandidateSpec(nil), base.Candidates...)
	changed.Candidates[0].Action = "restart_pool"
	require.NotEqual(t, base.WorkloadFingerprint(), changed.WorkloadFingerprint())
}

func TestWorkloadValidate_RejectsMissingMaterialFields(t *testing.T) {
	base := testWorkload(t)

	base.Revision = ""
	require.ErrorContains(t, base.Validate(), "revision")

	base = testWorkload(t)
	base.Seed = ""
	require.ErrorContains(t, base.Validate(), "seed")

	base = testWorkload(t)
	base.Samples = 0
	require.ErrorContains(t, base.Validate(), "samples")

	base = testWorkload(t)
	base.Concurrency = 0
	require.ErrorContains(t, base.Validate(), "concurrency")
}

func TestWorkloadValidate_RejectsCredentialLabels(t *testing.T) {
	spec := testWorkload(t)
	spec.Labels = map[string]string{"database_dsn": "postgresql://user:secret@preview-pg"}

	err := spec.Validate()

	require.ErrorContains(t, err, "credential label")
}

func TestDeployedWorkloadAndSeedMatchRunner(t *testing.T) {
	workloadData, err := os.ReadFile("../../../../deploy/repair-preview/pg-pool-workload.yaml")
	require.NoError(t, err)
	spec, err := LoadWorkload(workloadData)
	require.NoError(t, err)
	require.Equal(t, "workload-v1", spec.Revision)
	require.Equal(
		t,
		"sha256:db905b8f98c631212336b736f92d80b2a3040a75a44554687cfc782d39c31cc4",
		spec.WorkloadFingerprint(),
	)
	require.Equal(t, "resize_pool", spec.Candidates[0].Action)
	require.Equal(t, "reset_pool", spec.Candidates[1].Action)

	seedData, err := os.ReadFile("../../../../deploy/repair-preview/seed.sql")
	require.NoError(t, err)
	require.Contains(t, string(seedData), strings.TrimSpace(repairPreviewSeedSQL))
}

func testWorkload(t *testing.T) WorkloadSpec {
	t.Helper()
	spec, err := LoadWorkload([]byte(`revision: workload-v1
seed: sha256:seed-v1
warmup_queries: 2
samples: 20
concurrency: 4
query_timeout_ms: 500
labels: {}
queries:
  - name: accounts
    sql: SELECT customer_id, balance FROM repair_preview_accounts ORDER BY customer_id
business_probe:
  name: active-session-retained
  query: SELECT COUNT(*) FROM repair_preview_sessions WHERE customer_id = 'customer-a'
  expected_minimum: 1
candidates:
  - candidate_id: candidate-a
    name: bounded pool resize
    kind: postgresql
    action: resize_pool
    change_summary: Increase preview pool capacity
  - candidate_id: candidate-b
    name: risky pool reset
    kind: postgresql
    action: reset_pool
    change_summary: Clear preview sessions and reset pool
`))
	require.NoError(t, err)
	return spec
}

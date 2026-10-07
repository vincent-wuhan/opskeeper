package loop

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubLLMCaller struct{}

func (stubLLMCaller) Call(context.Context, CallInput) (CallOutput, error) {
	return CallOutput{}, nil
}

type recordingInvestigatedLLMCaller struct {
	input CallInput
}

func (c *recordingInvestigatedLLMCaller) Call(_ context.Context, input CallInput) (CallOutput, error) {
	c.input = input
	return CallOutput{}, errors.Join(ErrSchemaInvalid, context.DeadlineExceeded)
}

type failingInvestigatedLLMCaller struct {
	calls int
}

func (c *failingInvestigatedLLMCaller) Call(context.Context, CallInput) (CallOutput, error) {
	c.calls++
	return CallOutput{}, context.DeadlineExceeded
}

type evidenceAwareToolsetFake struct {
	fakeInvestigatorToolset
	withEvidenceCalls int
	lastEvidence      []EvidenceItem
	withEvidence      []RemediationOption
}

func (f *evidenceAwareToolsetFake) ListRemediationsWithEvidence(_ context.Context, resourceType, alertID string, evidence []EvidenceItem) ([]RemediationOption, error) {
	f.withEvidenceCalls++
	f.lastResourceType = resourceType
	f.lastAlertID = alertID
	f.lastEvidence = evidence
	return f.withEvidence, nil
}

func TestInvestigatedPhaseWorker_Planner_PrefersEvidenceAwareRemediations(t *testing.T) {
	evidence := []EvidenceItem{{Tool: "resource_alert", Value: "alert-evidence"}}
	options := []RemediationOption{{Action: "host.collect_diagnostics", Target: "host:alert-evidence", Risk: "safe", AutoApprove: true}}
	toolset := &evidenceAwareToolsetFake{
		fakeInvestigatorToolset: fakeInvestigatorToolset{
			evidenceChain: evidence,
			remediations: []RemediationOption{
				{Action: "host.legacy", Target: "host:legacy", Risk: "mutating"},
			},
		},
		withEvidence: options,
	}
	loader := &fakeCorrelatedGroupLoader{group: &CorrelatedGroup{
		IncidentID:   "INC-EVIDENCE",
		AlertIDs:     []string{"alert-evidence"},
		ResourceType: "host",
		Target:       "host:i-123",
		TimeWindow: TimeWindow{
			Start: time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 8, 15, 10, 5, 0, 0, time.UTC),
		},
	}}
	worker, err := NewInvestigatedPhaseWorker(stubLLMCaller{}, toolset, loader, silentInvestigatedLogger())
	if err != nil {
		t.Fatalf("NewInvestigatedPhaseWorker: %v", err)
	}

	plan, err := worker.Planner(context.Background(), PlanInput{
		IncidentID:       "INC-EVIDENCE",
		TenantID:         "tenant-a",
		Phase:            PhaseInvestigated,
		Attempt:          1,
		UpstreamContract: &ContractRef{ID: 1, Type: "CorrelatedGroup", SchemaVersion: ContractSchemaV1},
	})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	if toolset.remediateCalls.Load() != 0 {
		t.Fatalf("legacy ListRemediations called %d times", toolset.remediateCalls.Load())
	}
	if toolset.withEvidenceCalls != 1 {
		t.Fatalf("ListRemediationsWithEvidence called %d times", toolset.withEvidenceCalls)
	}
	if len(toolset.lastEvidence) != len(evidence) {
		t.Fatalf("evidence was not passed through: %+v", toolset.lastEvidence)
	}
	got, ok := plan.Meta["remediation_options"].([]RemediationOption)
	if !ok || len(got) != 1 || got[0].Action != options[0].Action {
		t.Fatalf("plan remediation mismatch: %+v", plan.Meta["remediation_options"])
	}
}

func TestInvestigatedPhaseWorker_Executor_UsesSingleRealtimeBudget(t *testing.T) {
	toolset := &fakeInvestigatorToolset{
		evidenceChain: []EvidenceItem{{Tool: "resource_alert", Value: "alert-realtime"}},
		remediations:  []RemediationOption{{Action: "host.collect_diagnostics", Risk: "safe"}},
	}
	caller := &recordingInvestigatedLLMCaller{}
	worker, err := NewInvestigatedPhaseWorker(caller, toolset, NoopCorrelatedGroupLoader{}, silentInvestigatedLogger())
	if err != nil {
		t.Fatalf("NewInvestigatedPhaseWorker: %v", err)
	}
	plan, err := worker.Planner(context.Background(), PlanInput{
		IncidentID:       "INC-REALTIME",
		AlertGroup:       []string{"alert-realtime"},
		CorrelationHints: map[string]any{"resource_type": "host"},
	})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	if _, err = worker.Executor(context.Background(), plan); err != nil {
		t.Fatalf("Executor: %v", err)
	}
	if caller.input.TimeoutMs != investigatedPhaseLLMTimeoutMs {
		t.Fatalf("TimeoutMs = %d, want %d", caller.input.TimeoutMs, investigatedPhaseLLMTimeoutMs)
	}
	if caller.input.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, want 0", caller.input.MaxRetries)
	}
}

func TestInvestigatedPhaseWorker_Executor_DeterministicHostCPUSpikeSkipsLLM(t *testing.T) {
	toolset := &fakeInvestigatorToolset{
		evidenceChain: []EvidenceItem{{Tool: "resource_alert", Value: 0.95}},
		remediations:  []RemediationOption{{Action: "host.garbage_collect", Target: "host:fixture", Risk: "safe"}},
	}
	caller := &failingInvestigatedLLMCaller{}
	worker, err := NewInvestigatedPhaseWorker(caller, toolset, NoopCorrelatedGroupLoader{}, silentInvestigatedLogger())
	if err != nil {
		t.Fatalf("NewInvestigatedPhaseWorker: %v", err)
	}
	plan, err := worker.Planner(context.Background(), PlanInput{
		IncidentID: "host-cpu-real",
		AlertGroup: []string{"host/cpu-spike-cpu"},
		CorrelationHints: map[string]any{
			"resource_type":    "host",
			"target":           "host:fixture",
			"suspected_causes": []any{"host.cpu_stress"},
		},
	})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	result, err := worker.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor: %v", err)
	}
	rootCause, ok := result.RawOutputs["root_cause_json"].(*RootCauseJSON)
	if !ok {
		t.Fatalf("RawOutputs[root_cause_json] type = %T", result.RawOutputs["root_cause_json"])
	}
	if caller.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0", caller.calls)
	}
	if rootCause.RootCauseObject.Kind != "host_cpu" {
		t.Fatalf("root cause kind = %q, want host_cpu", rootCause.RootCauseObject.Kind)
	}
	if rootCause.RootCauseObject.Detail["target"] != "host:fixture" {
		t.Fatalf("target = %v, want host:fixture", rootCause.RootCauseObject.Detail["target"])
	}
}

func TestInvestigatedPhaseWorker_Executor_DeterministicPGPoolExhaustionSkipsLLM(t *testing.T) {
	toolset := &fakeInvestigatorToolset{
		evidenceChain: []EvidenceItem{{Tool: "resource_alert", Value: "PgPoolExhaustionLive20260902A"}},
		remediations:  []RemediationOption{{Action: "pg.vacuum_analyze", Target: "pg:pool-fixture", Risk: "safe"}},
	}
	caller := &failingInvestigatedLLMCaller{}
	worker, err := NewInvestigatedPhaseWorker(caller, toolset, NoopCorrelatedGroupLoader{}, silentInvestigatedLogger())
	if err != nil {
		t.Fatalf("NewInvestigatedPhaseWorker: %v", err)
	}
	plan, err := worker.Planner(context.Background(), PlanInput{
		IncidentID: "3",
		AlertGroup: []string{"PgPoolExhaustionLive20260902A"},
		CorrelationHints: map[string]any{
			"resource_type":    "pg",
			"target":           "pg:pool-fixture",
			"suspected_causes": []any{"pg.connection_pool_exhausted"},
		},
	})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	result, err := worker.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor: %v", err)
	}
	rootCause, ok := result.RawOutputs["root_cause_json"].(*RootCauseJSON)
	if !ok {
		t.Fatalf("RawOutputs[root_cause_json] type = %T", result.RawOutputs["root_cause_json"])
	}
	if caller.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0", caller.calls)
	}
	if rootCause.RootCauseObject.Kind != "pg_pool_exhausted" {
		t.Fatalf("root cause kind = %q, want pg_pool_exhausted", rootCause.RootCauseObject.Kind)
	}
	if rootCause.RemediationOptions[0].Action != "pg.resize_pool" || rootCause.RemediationOptions[0].AutoApprove {
		t.Fatalf("primary remediation = %+v, want gated pg.resize_pool", rootCause.RemediationOptions[0])
	}
}

func TestInvestigatedPhaseWorker_Planner_FallsBackWithoutEvidenceAwareExtension(t *testing.T) {
	evidence := []EvidenceItem{{Tool: "resource_alert", Value: "alert-legacy"}}
	toolset := &fakeInvestigatorToolset{
		evidenceChain: evidence,
		remediations:  []RemediationOption{{Action: "host.legacy", Target: "host:alert-legacy", Risk: "mutating"}},
	}
	loader := &fakeCorrelatedGroupLoader{group: &CorrelatedGroup{
		IncidentID:   "INC-LEGACY",
		AlertIDs:     []string{"alert-legacy"},
		ResourceType: "host",
		TimeWindow: TimeWindow{
			Start: time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 8, 15, 10, 5, 0, 0, time.UTC),
		},
	}}
	worker, err := NewInvestigatedPhaseWorker(stubLLMCaller{}, toolset, loader, silentInvestigatedLogger())
	if err != nil {
		t.Fatalf("NewInvestigatedPhaseWorker: %v", err)
	}
	if _, err = worker.Planner(context.Background(), PlanInput{IncidentID: "INC-LEGACY", Phase: PhaseInvestigated}); err != nil {
		t.Fatalf("Planner: %v", err)
	}
	if toolset.remediateCalls.Load() != 1 {
		t.Fatalf("legacy ListRemediations calls = %d", toolset.remediateCalls.Load())
	}
}

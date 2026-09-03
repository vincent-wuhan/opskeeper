package loop_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	aiopstools "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools"
	loopbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

func TestMCPAdapter_VerifyInvokesRealRecoveryToolFromApprovalState(t *testing.T) {
	contracts := loopbiz.NewInMemoryContractRepo()
	approvedAt := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(loopbiz.ApprovalDecision{
		SchemaVersion: "v1",
		SkillID:       "pg.long_running_tx",
		Target:        "pg:primary",
		ResourceType:  "pg",
		ApprovedAt:    approvedAt,
		ApprovedBy:    "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := contracts.WriteContract(context.Background(), &loopmodel.Contract{
		IncidentID: "inc-real", TenantID: "tenant-1",
		Phase:     "approved",
		Type:      "ApprovalDecision",
		SchemaVer: "v1",
		Payload:   string(payload),
	}); err != nil {
		t.Fatal(err)
	}

	state := aiopstools.NewInMemoryRecoveryStateStore()
	if _, err := state.Increment(context.Background(), "pg.long_running_tx"); err != nil {
		t.Fatal(err)
	}
	tool := aiopstools.NewVerifyRecoveryTool(
		aiopstools.NewDryRunMetricQuerier(),
		state,
		slog.New(slog.DiscardHandler),
		aiopstools.DefaultVerifyRecoveryConfig(),
	)
	adapter := loopbiz.NewMCPAdapter(
		nil,
		aiopstools.VerifyRecoveryCallerAdapter{Tool: tool},
		loopbiz.NewContractMCPRecoveryContextLoader(contracts),
	)

	output, err := adapter.Invoke(context.Background(), "tenant-1", loopbiz.ToolNameVerify, json.RawMessage(`{
		"incident_id":"inc-real",
		"baseline_window":"5m",
		"compare_window":"2m",
		"tolerance":0.2,
		"metrics":["cpu","request_rate"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	verified, ok := output.(loopbiz.MCPVerifyOutput)
	if !ok {
		t.Fatalf("output type = %T, want MCPVerifyOutput", output)
	}
	if !verified.Passed || verified.Tolerance != 0.2 || verified.RetryCount != 1 {
		t.Fatalf("verified output = %+v", verified)
	}
	if verified.MetricsCompared[0] != "cpu" || verified.MetricsCompared[1] != "request_rate" {
		t.Fatalf("protocol metrics were not mapped to internal metrics: %v", verified.MetricsCompared)
	}
	if _, ok := verified.Delta["cpu"]; !ok {
		t.Fatalf("output delta leaks or misses protocol metric: %+v", verified.Delta)
	}
}

func TestMCPAdapter_InvestigatePersistsRootCauseForVerify(t *testing.T) {
	contracts := loopbiz.NewInMemoryContractRepo()
	rootCause := &loopbiz.RootCauseJSON{
		SchemaVersion:   "v1",
		RootCauseObject: &loopbiz.RootCauseObject{Kind: "pg.long_running_tx", Summary: "long transaction"},
		Confidence:      0.9,
		EvidenceChain: []loopbiz.EvidenceItem{{
			Tool: "pg.stat_activity", Timestamp: time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC),
		}},
		TimeWindow: loopbiz.TimeWindow{
			Start: time.Date(2026, 8, 19, 9, 58, 0, 0, time.UTC),
			End:   time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC),
		},
		RemediationOptions: []loopbiz.RemediationOption{{
			Action: "pg.terminate_long_tx", Target: "pg:primary", Risk: "mutating",
		}},
	}
	orchestrator, err := loopbiz.NewOrchestrator(loopbiz.OrchestratorDeps{
		Locker:       fixedLocker{},
		EventRepo:    loopbiz.NewInMemoryEventRepo(),
		ContractRepo: contracts,
		WorkerRegistry: loopbiz.NewWorkerRegistry(map[loopbiz.Phase]loopbiz.PhaseWorker{
			loopbiz.PhaseDetected:   &loopbiz.BasePhaseWorker{PhaseRef: loopbiz.PhaseDetected},
			loopbiz.PhaseCorrelated: &loopbiz.BasePhaseWorker{PhaseRef: loopbiz.PhaseCorrelated},
			loopbiz.PhaseInvestigated: &staticRootCauseWorker{
				BasePhaseWorker: loopbiz.BasePhaseWorker{PhaseRef: loopbiz.PhaseInvestigated},
				rootCause:       rootCause,
			},
			loopbiz.PhaseCritiqued: &loopbiz.BasePhaseWorker{PhaseRef: loopbiz.PhaseCritiqued},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	investigator := loopbiz.NewMCPAdapter(orchestrator, nil)
	investigated, err := investigator.Invoke(context.Background(), "tenant-loop", loopbiz.ToolNameInvestigate, json.RawMessage(`{
		"incident_id":"inc-loop",
		"alert_group":["alert-pg"],
		"correlation_hints":{"resource_type":"pg","target":"pg:primary"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := investigated.(loopbiz.MCPInvestigateOutput); !ok {
		t.Fatalf("investigated output type = %T", investigated)
	}
	stored, err := contracts.ReadContract(
		context.Background(), "tenant-loop", "inc-loop", loopbiz.PhaseInvestigated, "root_cause_json",
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil {
		t.Fatal("investigate did not persist root_cause_json")
	}

	state := aiopstools.NewInMemoryRecoveryStateStore()
	if _, err := state.Increment(context.Background(), "pg.long_running_tx"); err != nil {
		t.Fatal(err)
	}
	verifyTool := aiopstools.NewVerifyRecoveryTool(
		aiopstools.NewDryRunMetricQuerier(),
		state,
		slog.New(slog.DiscardHandler),
		aiopstools.DefaultVerifyRecoveryConfig(),
	)
	verifier := loopbiz.NewMCPAdapter(
		nil,
		aiopstools.VerifyRecoveryCallerAdapter{Tool: verifyTool},
		loopbiz.NewContractMCPRecoveryContextLoader(contracts),
	)
	verified, err := verifier.Invoke(context.Background(), "tenant-loop", loopbiz.ToolNameVerify, json.RawMessage(`{
		"incident_id":"inc-loop","metrics":["cpu"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	delta, ok := verified.(loopbiz.MCPVerifyOutput)
	if !ok || !delta.Passed {
		t.Fatalf("verified output = %+v, err = %v", verified, err)
	}
}

type staticRootCauseWorker struct {
	loopbiz.BasePhaseWorker
	rootCause *loopbiz.RootCauseJSON
}

func (w *staticRootCauseWorker) Executor(context.Context, loopbiz.Plan) (loopbiz.ExecResult, error) {
	return loopbiz.ExecResult{RawOutputs: map[string]any{"root_cause_json": w.rootCause}}, nil
}

type fixedLocker struct{}

func (fixedLocker) GetLock(context.Context, string, int) (bool, error) { return true, nil }
func (fixedLocker) ReleaseLock(context.Context, string) error          { return nil }

package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

func TestMCPAdapter_Tools_ExposeThreeProtocolTools(t *testing.T) {
	adapter := NewMCPAdapter(nil, nil)
	tools := adapter.Tools(context.Background())
	if len(tools) != 3 {
		t.Fatalf("tools count = %d, want 3", len(tools))
	}
	for _, tool := range tools {
		if tool.Name == "" || tool.Description == "" || len(tool.InputSchema) == 0 {
			t.Errorf("tool %q metadata incomplete", tool.Name)
		}
	}
}

func TestMCPAdapter_CorrelateDeduplicatesSemanticAlerts(t *testing.T) {
	adapter := NewMCPAdapter(nil, nil)
	raw := `{"raw_alerts":[{"alert_id":"a1","severity":"warn","resource":"pg:primary","detected_at":"2026-08-18T07:00:00Z","summary":"PG lock waiting is high"},{"alert_id":"a2","severity":"critical","resource":"pg:primary","detected_at":"2026-08-18T07:01:00Z","summary":"high lock waiting PG"}],"window":"5m"}`
	output, err := adapter.Invoke(context.Background(), "tenant-1", ToolNameCorrelate, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(output)
	var result struct {
		CorrelatedGroups []map[string]any `json:"correlated_groups"`
		Severity         string           `json:"severity"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.CorrelatedGroups) != 1 {
		t.Fatalf("correlated groups = %d, want 1: %s", len(result.CorrelatedGroups), encoded)
	}
	fingerprint, _ := result.CorrelatedGroups[0]["fingerprint"].(string)
	if len(fingerprint) < 16 {
		t.Fatalf("fingerprint length = %d, want >= 16", len(fingerprint))
	}
	if result.Severity != "critical" {
		t.Fatalf("severity = %q, want critical", result.Severity)
	}
}

func TestMCPAdapter_InvestigateStopsAfterCritique(t *testing.T) {
	orchestrator := &stubMCPOrchestrator{rootCause: &RootCauseJSON{
		SchemaVersion:      "v1",
		RootCauseObject:    &RootCauseObject{Kind: "pg.long_running_tx", Summary: "long transaction"},
		EvidenceChain:      []EvidenceItem{{Tool: "query_metrics", Query: "pg_stat_activity", Timestamp: time.Unix(0, 0)}},
		TimeWindow:         TimeWindow{Start: time.Unix(0, 0), End: time.Unix(1, 0)},
		RemediationOptions: []RemediationOption{{Action: "pg.terminate_backend", Target: "backend:1", Risk: "safe"}},
	}}
	adapter := NewMCPAdapter(orchestrator, nil)
	raw := `{"incident_id":"inc-1","alert_group":["a1"],"correlation_hints":{"resource_type":"pg"}}`
	output, err := adapter.Invoke(context.Background(), "tenant-1", ToolNameInvestigate, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if orchestrator.gotOptions.StopAfterPhase != PhaseInvestigated {
		t.Fatalf("StopAfterPhase = %q, want %q", orchestrator.gotOptions.StopAfterPhase, PhaseInvestigated)
	}
	if len(orchestrator.gotOptions.AlertGroup) != 1 || orchestrator.gotOptions.AlertGroup[0] != "a1" {
		t.Fatalf("AlertGroup = %v, want [a1]", orchestrator.gotOptions.AlertGroup)
	}
	if orchestrator.gotOptions.CorrelationHints["resource_type"] != "pg" {
		t.Fatalf("CorrelationHints = %v, want resource_type=pg", orchestrator.gotOptions.CorrelationHints)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "root_cause_object", "confidence", "evidence_chain", "time_window", "remediation_options"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing protocol field %q in %s", key, encoded)
		}
	}
	for _, forbidden := range []string{"final_phase", "loop_events", "schema", "risk", "auto_approve"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("internal field %q leaked: %s", forbidden, encoded)
		}
	}
}

func TestMCPAdapter_VerifyAdaptsProtocolDefaults(t *testing.T) {
	caller := &stubMCPVerifyCaller{response: `{"schema_version":"v1","passed":true,"deltas":{"cpu":0.04},"sample_size":30,"tolerance":0.15,"warning_level":"pass"}`}
	contracts := NewInMemoryContractRepo()
	if err := contracts.WriteContract(context.Background(), &loopmodel.Contract{
		IncidentID: "inc-1", TenantID: "tenant-1",
		Phase:     string(PhaseInvestigated),
		Type:      "root_cause_json",
		SchemaVer: "v1",
		Payload:   validRootCauseJSONFor("host.cpu_saturation", 1, 1),
	}); err != nil {
		t.Fatal(err)
	}
	adapter := NewMCPAdapter(nil, caller, NewContractMCPRecoveryContextLoader(contracts))
	output, err := adapter.Invoke(context.Background(), "tenant-1", ToolNameVerify, json.RawMessage(`{"incident_id":"inc-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(caller.gotArgs), &args); err != nil {
		t.Fatal(err)
	}
	if args["skill_id"] != "host.cpu_saturation" || args["target"] != "host:i-0abc123" || args["resource_type"] != "host" {
		t.Fatalf("recovery context not constructed: %s", caller.gotArgs)
	}
	if args["baseline_window"] != "5m" || args["compare_window"] != "2m" || args["tolerance"] != 0.15 {
		t.Fatalf("defaults not applied: %s", caller.gotArgs)
	}
	if fmt.Sprint(args["metrics"]) != "[cpu_usage mem_usage]" {
		t.Fatalf("protocol metrics not mapped: %s", caller.gotArgs)
	}
	verified, ok := output.(MCPVerifyOutput)
	if !ok || verified.SchemaVersion != "v1" || verified.Delta == nil || verified.RollbackRecommended {
		t.Fatalf("output type/value = %T %+v", output, output)
	}
}

func TestContractMCPRecoveryContextLoader_LoadsRootCauseFallback(t *testing.T) {
	contracts := NewInMemoryContractRepo()
	payload := strings.Replace(validRootCauseJSONFor("pg.long_running_tx", 2, 2), `"host:i-0abc123"`, `"pg:primary"`, 1)
	if err := contracts.WriteContract(context.Background(), &loopmodel.Contract{
		IncidentID: "inc-root-cause", TenantID: "tenant-1",
		Phase:     string(PhaseInvestigated),
		Type:      "root_cause_json",
		SchemaVer: "v1",
		Payload:   payload,
	}); err != nil {
		t.Fatal(err)
	}

	context, err := NewContractMCPRecoveryContextLoader(contracts).LoadMCPRecoveryContext(
		context.Background(), "tenant-1", "inc-root-cause",
	)
	if err != nil {
		t.Fatal(err)
	}
	if context.SkillID != "pg.long_running_tx" || context.Target != "pg:primary" || context.ResourceType != "pg" {
		t.Fatalf("recovery context = %+v, want pg.long_running_tx/pg:primary/pg", context)
	}
}

func TestOrchestrator_PropagatesMCPInvestigationInput(t *testing.T) {
	investigated := &capturingPhaseWorker{
		BasePhaseWorker: BasePhaseWorker{PhaseRef: PhaseInvestigated},
		rootCause: &RootCauseJSON{
			SchemaVersion:      "v1",
			RootCauseObject:    &RootCauseObject{Kind: "pg.long_running_tx", Summary: "long transaction"},
			EvidenceChain:      []EvidenceItem{{Tool: "pg.stat_activity", Timestamp: time.Unix(0, 0)}},
			TimeWindow:         TimeWindow{Start: time.Unix(0, 0), End: time.Unix(1, 0)},
			RemediationOptions: []RemediationOption{{Action: "pg.terminate_backend", Target: "pg:primary", Risk: "mutating"}},
		},
	}
	orchestrator, err := NewOrchestrator(OrchestratorDeps{
		Locker:       stubLocker{},
		EventRepo:    NewInMemoryEventRepo(),
		ContractRepo: NewInMemoryContractRepo(),
		WorkerRegistry: NewWorkerRegistry(map[Phase]PhaseWorker{
			PhaseDetected:     &recordingPhaseWorker{BasePhaseWorker: BasePhaseWorker{PhaseRef: PhaseDetected}, recorder: &phaseRecorder{}},
			PhaseCorrelated:   &recordingPhaseWorker{BasePhaseWorker: BasePhaseWorker{PhaseRef: PhaseCorrelated}, recorder: &phaseRecorder{}},
			PhaseInvestigated: investigated,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	hints := map[string]any{"resource_type": "pg", "target": "pg:primary"}

	result, err := orchestrator.Run(context.Background(), RunOptions{
		IncidentID:       "inc-input",
		TenantID:         "tenant-1",
		TriggeredBy:      "mcp",
		StopAfterPhase:   PhaseInvestigated,
		AlertGroup:       []string{"a1", "a2"},
		CorrelationHints: hints,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalPhase != PhaseInvestigated {
		t.Fatalf("FinalPhase = %s, want %s", result.FinalPhase, PhaseInvestigated)
	}
	if len(investigated.input.AlertGroup) != 2 || investigated.input.AlertGroup[1] != "a2" {
		t.Fatalf("AlertGroup = %v, want [a1 a2]", investigated.input.AlertGroup)
	}
	if investigated.input.CorrelationHints["resource_type"] != "pg" {
		t.Fatalf("CorrelationHints = %v, want resource_type=pg", investigated.input.CorrelationHints)
	}
}

func TestMCPAdapter_VerifyRejectsProtocolInvalidInput(t *testing.T) {
	tests := []string{
		`{"incident_id":"inc-1","baseline_window":"500ms"}`,
		`{"incident_id":"inc-1","compare_window":"1m30m"}`,
		`{"incident_id":"inc-1","tolerance":0}`,
		`{"incident_id":"inc-1","tolerance":1.01}`,
		`{"incident_id":"inc-1","metrics":["cpu","cpu"]}`,
		`{"incident_id":"inc-1","metrics":["connections"]}`,
		`{"incident_id":"inc-1","tolerance":null}`,
		`{"incident_id":"inc-1","metrics":null}`,
	}
	for _, arguments := range tests {
		adapter := NewMCPAdapter(nil, failingMCPVerifyCaller{})
		if _, err := adapter.Invoke(context.Background(), "tenant-1", ToolNameVerify, json.RawMessage(arguments)); err == nil {
			t.Fatalf("Invoke(%s) succeeded; want validation error", arguments)
		}
	}
}

func TestMCPAdapter_CorrelateRejectsProtocolInvalidWindow(t *testing.T) {
	adapter := NewMCPAdapter(nil, nil)
	arguments := `{"raw_alerts":[{"alert_id":"a1","severity":"warn","resource":"pg:1","detected_at":"2026-08-19T10:00:00Z"}],"window":"500ms"}`
	if _, err := adapter.Invoke(context.Background(), "tenant-1", ToolNameCorrelate, json.RawMessage(arguments)); err == nil {
		t.Fatal("Invoke succeeded for protocol-invalid window; want error")
	}
}

func TestOrchestrator_StopAfterPhaseRunsExecutorAndVerifier(t *testing.T) {
	recorder := &phaseRecorder{}
	phases := []Phase{PhaseDetected, PhaseCorrelated, PhaseInvestigated, PhaseCritiqued, PhaseApproved}
	workers := make(map[Phase]PhaseWorker, len(phases))
	var rootCause RootCauseJSON
	if err := json.Unmarshal([]byte(validRootCauseJSONFor("host.cpu_saturation", 1, 1)), &rootCause); err != nil {
		t.Fatal(err)
	}
	for _, phase := range phases {
		worker := &recordingPhaseWorker{BasePhaseWorker: BasePhaseWorker{PhaseRef: phase}, recorder: recorder}
		if phase == PhaseInvestigated {
			worker.rootCause = &rootCause
		}
		workers[phase] = worker
	}
	orchestrator, err := NewOrchestrator(OrchestratorDeps{
		Locker:         stubLocker{},
		EventRepo:      NewInMemoryEventRepo(),
		ContractRepo:   NewInMemoryContractRepo(),
		WorkerRegistry: NewWorkerRegistry(workers),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := orchestrator.Run(context.Background(), RunOptions{
		IncidentID:     "inc-stop",
		TenantID:       "tenant-1",
		StopAfterPhase: PhaseCritiqued,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalPhase != PhaseCritiqued {
		t.Fatalf("FinalPhase = %q, want critiqued", result.FinalPhase)
	}
	want := []string{
		"detected:executor", "detected:verifier",
		"correlated:executor", "correlated:verifier",
		"investigated:executor", "investigated:verifier",
		"critiqued:executor", "critiqued:verifier",
	}
	if strings.Join(recorder.recorded(), ",") != strings.Join(want, ",") {
		t.Fatalf("executed phases = %v, want %v (approved must not run)", recorder.recorded(), want)
	}
}

func TestOrchestrator_DoesNotWriteContractBeforeVerifierPasses(t *testing.T) {
	contracts := NewInMemoryContractRepo()
	orchestrator, err := NewOrchestrator(OrchestratorDeps{
		Locker: stubLocker{},
		EventRepo: eventRepoWithContractFailure{
			InMemoryEventRepo: NewInMemoryEventRepo(),
		},
		ContractRepo:   contracts,
		WorkerRegistry: NewWorkerRegistry(map[Phase]PhaseWorker{PhaseDetected: &sideEffectPhaseWorker{verdictOK: false}}),
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := orchestrator.Run(context.Background(), RunOptions{
		IncidentID:     "inc-verifier-first",
		TenantID:       "tenant-1",
		StopAfterPhase: PhaseDetected,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalPhase != PhaseFailed {
		t.Fatalf("FinalPhase = %q, want failed", result.FinalPhase)
	}
	if contracts.Len() != 0 {
		t.Fatal("side-effect contract was written before verifier passed")
	}
	for _, event := range result.LoopEvents {
		if event.EventType == loopmodel.EventPhaseContractWritten {
			t.Fatalf("success event written before verifier passed: %+v", event)
		}
	}
}

func TestOrchestrator_ContractEventFailureFailsRun(t *testing.T) {
	eventRepo := &eventRepoWithContractFailure{InMemoryEventRepo: NewInMemoryEventRepo()}
	orchestrator, err := NewOrchestrator(OrchestratorDeps{
		Locker:         stubLocker{},
		EventRepo:      eventRepo,
		ContractRepo:   NewInMemoryContractRepo(),
		WorkerRegistry: NewWorkerRegistry(map[Phase]PhaseWorker{PhaseDetected: &sideEffectPhaseWorker{verdictOK: true}}),
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := orchestrator.Run(context.Background(), RunOptions{
		IncidentID:     "inc-event-failure",
		TenantID:       "tenant-1",
		StopAfterPhase: PhaseDetected,
	})
	if err == nil || !strings.Contains(err.Error(), loopmodel.EventPhaseContractWritten) {
		t.Fatalf("Run() error = %v, want contract event failure", err)
	}
	if result != nil {
		t.Fatalf("Run() result = %+v, want nil", result)
	}
}

func BenchmarkMCPAdapter_Investigate(b *testing.B) {
	orchestrator := &stubMCPOrchestrator{rootCause: &RootCauseJSON{SchemaVersion: "v1"}}
	adapter := NewMCPAdapter(orchestrator, nil)
	arguments := json.RawMessage(`{"incident_id":"inc-bench","alert_group":["a1"],"correlation_hints":{"resource_type":"pg"}}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := adapter.Invoke(context.Background(), "tenant-bench", ToolNameInvestigate, arguments); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMCPAdapter_InvestigateP95WithConcreteOrchestrator(t *testing.T) {
	const sampleCount = 100
	const p95Limit = 2 * time.Minute
	latencies := make([]time.Duration, 0, sampleCount)
	arguments := json.RawMessage(`{"incident_id":"inc-p95","alert_group":["pg:primary"],"correlation_hints":{"resource_type":"pg"}}`)

	for sample := 0; sample < sampleCount; sample++ {
		adapter := newConcreteMCPAdapter(t, sample)
		started := time.Now()
		output, err := adapter.Invoke(context.Background(), "tenant-p95", ToolNameInvestigate, arguments)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("sample %d: Invoke: %v", sample, err)
		}
		if _, ok := output.(MCPInvestigateOutput); !ok {
			t.Fatalf("sample %d: output type = %T", sample, output)
		}
		latencies = append(latencies, elapsed)
	}

	slices.Sort(latencies)
	p95 := latencies[(sampleCount*95+99)/100-1]
	report, err := json.Marshal(map[string]any{
		"tool":              ToolNameInvestigate,
		"samples":           sampleCount,
		"p95_ns":            p95.Nanoseconds(),
		"p95":               p95.String(),
		"limit":             p95Limit.String(),
		"orchestrator_mode": "real orchestrator state walker with deterministic phase workers",
		"stop_after_phase":  string(PhaseCritiqued),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MCP P95 audit report: %s", report)
	if p95 >= p95Limit {
		t.Fatalf("P95 = %s, want < %s", p95, p95Limit)
	}
}

func newConcreteMCPAdapter(t *testing.T, sample int) *MCPAdapter {
	t.Helper()
	contractRepo := NewInMemoryContractRepo()
	if err := contractRepo.WriteContract(context.Background(), &loopmodel.Contract{
		IncidentID: "inc-p95", TenantID: "tenant-1",
		Phase:     string(PhaseInvestigated),
		Type:      "root_cause_json",
		SchemaVer: "v1",
		Payload:   validRootCauseJSONFor("pg.long_running_tx", 2, 2),
	}); err != nil {
		t.Fatal(err)
	}
	recorder := &phaseRecorder{}
	phases := []Phase{PhaseDetected, PhaseCorrelated, PhaseInvestigated, PhaseCritiqued, PhaseApproved}
	workers := make(map[Phase]PhaseWorker, len(phases))
	var rootCause RootCauseJSON
	if err := json.Unmarshal([]byte(validRootCauseJSONFor("pg.long_running_tx", 2, 2)), &rootCause); err != nil {
		t.Fatal(err)
	}
	for _, phase := range phases {
		worker := &recordingPhaseWorker{BasePhaseWorker: BasePhaseWorker{PhaseRef: phase}, recorder: recorder}
		if phase == PhaseInvestigated {
			worker.rootCause = &rootCause
		}
		workers[phase] = worker
	}
	orchestrator, err := NewOrchestrator(OrchestratorDeps{
		Locker:                 stubLocker{},
		AdvisoryLockTimeoutSec: 5,
		EventRepo:              NewInMemoryEventRepo(),
		ContractRepo:           contractRepo,
		WorkerRegistry:         NewWorkerRegistry(workers),
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewMCPAdapter(orchestrator, nil)
}

func TestMCPAdapter_RejectsUnknownField(t *testing.T) {
	adapter := NewMCPAdapter(nil, nil)
	_, err := adapter.Invoke(context.Background(), "tenant-1", ToolNameCorrelate, json.RawMessage(`{"raw_alerts":[],"window":"5m","unknown":1}`))
	if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("err = %v, want invalid arguments", err)
	}
}

type stubMCPOrchestrator struct {
	gotOptions RunOptions
	rootCause  *RootCauseJSON
}

type capturingPhaseWorker struct {
	BasePhaseWorker
	input     PlanInput
	rootCause *RootCauseJSON
}

func (w *capturingPhaseWorker) Planner(_ context.Context, in PlanInput) (Plan, error) {
	w.input = in
	return Plan{}, nil
}

func (w *capturingPhaseWorker) Executor(context.Context, Plan) (ExecResult, error) {
	return ExecResult{RawOutputs: map[string]any{"root_cause_json": w.rootCause}}, nil
}

func (s *stubMCPOrchestrator) Run(_ context.Context, opts RunOptions) (*RunResult, error) {
	s.gotOptions = opts
	return &RunResult{RootCause: s.rootCause}, nil
}

func (s *stubMCPOrchestrator) State(context.Context, string, string) (*loopmodel.State, error) {
	return nil, errors.New("not implemented")
}

func (s *stubMCPOrchestrator) Resume(context.Context, string, string) error {
	return errors.New("not implemented")
}

type stubMCPVerifyCaller struct {
	gotArgs  string
	response string
}

type phaseRecorder struct {
	names []string
}

func (r *phaseRecorder) record(name string) { r.names = append(r.names, name) }

func (r *phaseRecorder) recorded() []string { return r.names }

type recordingPhaseWorker struct {
	BasePhaseWorker
	recorder  *phaseRecorder
	rootCause *RootCauseJSON
}

type sideEffectPhaseWorker struct {
	BasePhaseWorker
	verdictOK bool
}

func (w *sideEffectPhaseWorker) Executor(context.Context, Plan) (ExecResult, error) {
	return ExecResult{
		SideEffects: []SideEffect{{
			Kind: loopmodel.EventPhaseContractWritten,
			Detail: map[string]any{
				"contract": "side_effect",
				"payload":  `{"ok":true}`,
			},
		}},
	}, nil
}

func (w *sideEffectPhaseWorker) Verifier(_ context.Context, _ ExecResult) (Verdict, error) {
	return Verdict{OK: w.verdictOK, Reasons: []string{"verifier-configured"}}, nil
}

type eventRepoWithContractFailure struct {
	*InMemoryEventRepo
}

func (r eventRepoWithContractFailure) AppendEvent(ctx context.Context, event *loopmodel.Event) error {
	if event.EventType == loopmodel.EventPhaseContractWritten {
		return errors.New("contract event store unavailable")
	}
	return r.InMemoryEventRepo.AppendEvent(ctx, event)
}

func (w *recordingPhaseWorker) Executor(context.Context, Plan) (ExecResult, error) {
	w.recorder.record(string(w.PhaseRef) + ":executor")
	if w.rootCause != nil {
		return ExecResult{RawOutputs: map[string]any{"root_cause_json": w.rootCause}}, nil
	}
	return ExecResult{}, nil
}

func (w *recordingPhaseWorker) Verifier(context.Context, ExecResult) (Verdict, error) {
	w.recorder.record(string(w.PhaseRef) + ":verifier")
	return Verdict{OK: true}, nil
}

type failingMCPVerifyCaller struct{}

func (failingMCPVerifyCaller) InvokeVerifyRecovery(context.Context, string) (string, error) {
	return "", errors.New("validation must fail before verify invocation")
}

func (s *stubMCPVerifyCaller) InvokeVerifyRecovery(_ context.Context, args string) (string, error) {
	s.gotArgs = args
	return s.response, nil
}

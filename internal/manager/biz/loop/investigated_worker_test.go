// Package loop — investigated_worker_test.go
//
// 测试覆盖（zero-manual-ops-loop · path A · llm-worker-integration 批次 2 /
// subagent 7）：
//
//	InvestigatedPhaseWorker
//	  1. 成功路径：fake InvestigatorToolset 返回 2 evidence + 2 remediation
//	     + LLM 给合法 RootCauseJSON → Planner / Executor / Verifier 全 OK
//	  2. 工具失败：fake InvestigatorToolset.Investigate 返回 error →
//	     Planner 拒绝，错误链 wrapped
//	  3. schema-invalid：fake LLM 返回缺 root_cause_object 的 JSON →
//	     Executor 拒绝，错误链包含 ErrSchemaInvalid
//
// 共 3 个测试用例 + 1 个共享 inMemoryInvestigatorToolset fake + 1 个
// fakeCorrelatedGroupLoader fake。
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// silentLogger 丢弃所有日志输出，避免测试输出噪声。
func silentInvestigatedLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeInvestigatorToolset 是 InvestigatorToolset 的内存 fake；记录
// 调用次数 + 返回值，便于断言 "Planner 真的调了 toolset"。
type fakeInvestigatorToolset struct {
	mu sync.Mutex

	// 预置返回值
	evidenceChain []EvidenceItem
	evidenceErr   error
	remediations  []RemediationOption
	remediateErr  error

	// 调用次数
	investigateCalls atomic.Int32
	remediateCalls   atomic.Int32

	// 调用记录
	lastResourceType string
	lastAlertID      string
	lastTimeWindow   TimeWindow
}

func (f *fakeInvestigatorToolset) Investigate(_ context.Context, resourceType, alertID string, timeWindow TimeWindow) ([]EvidenceItem, error) {
	f.investigateCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastResourceType = resourceType
	f.lastAlertID = alertID
	f.lastTimeWindow = timeWindow
	if f.evidenceErr != nil {
		return nil, f.evidenceErr
	}
	return f.evidenceChain, nil
}

func (f *fakeInvestigatorToolset) ListRemediations(_ context.Context, _ string) ([]RemediationOption, error) {
	f.remediateCalls.Add(1)
	if f.remediateErr != nil {
		return nil, f.remediateErr
	}
	return f.remediations, nil
}

// fakeCorrelatedGroupLoader 是 CorrelatedGroupLoader 的内存 fake。
type fakeCorrelatedGroupLoader struct {
	mu    sync.Mutex
	group *CorrelatedGroup
	err   error
	loadN atomic.Int32
}

func (f *fakeCorrelatedGroupLoader) LoadCorrelatedGroup(_ context.Context, _ string, _ int64) (*CorrelatedGroup, error) {
	f.loadN.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.group, nil
}

// validRootCauseJSONFor 返回通过 ValidateRootCauseJSON 的合法 RootCauseJSON。
func validRootCauseJSONFor(kind string, evidenceCount, remediationCount int) string {
	tw := TimeWindow{
		Start: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 12, 10, 5, 0, 0, time.UTC),
	}
	ev := make([]map[string]any, 0, evidenceCount)
	for i := 0; i < evidenceCount; i++ {
		ev = append(ev, map[string]any{
			"tool":      "host.cpu",
			"query":     "rate(cpu_usage[5m])",
			"value":     0.92,
			"count":     12,
			"timestamp": tw.Start.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		})
	}
	ro := make([]map[string]any, 0, remediationCount)
	for i := 0; i < remediationCount; i++ {
		ro = append(ro, map[string]any{
			"action":       "host.scale_up",
			"target":       "host:i-0abc123",
			"risk":         "mutating",
			"auto_approve": false,
		})
	}
	doc := map[string]any{
		"schema_version":      "v1",
		"root_cause_object":   map[string]any{"kind": kind, "summary": "CPU saturated", "detail": map[string]any{"cpu_pct": 92.0}},
		"confidence":          0.87,
		"evidence_chain":      ev,
		"time_window":         map[string]any{"start": tw.Start.Format(time.RFC3339), "end": tw.End.Format(time.RFC3339)},
		"remediation_options": ro,
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// rootCauseJSONWithoutRootCause 返回故意缺 root_cause_object 字段的 JSON，
// 用于触发 schema-invalid 测试路径。
func rootCauseJSONWithoutRootCause() string {
	doc := map[string]any{
		"schema_version": "v1",
		"confidence":     0.5,
		"evidence_chain": []map[string]any{
			{"tool": "host.cpu", "timestamp": time.Now().UTC().Format(time.RFC3339)},
		},
		"time_window": map[string]any{
			"start": time.Now().UTC().Format(time.RFC3339),
			"end":   time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
		},
		"remediation_options": []map[string]any{
			{"action": "noop", "target": "x", "risk": "safe", "auto_approve": true},
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// TestInvestigatedWorker_HappyPath 验证完整三段链路：
// Planner → Executor → Verifier 都返回 OK；RootCauseJSON 通过验证；
// toolset 真的被调过（investigate + ListRemediations 各 1 次）。
func TestInvestigatedWorker_HappyPath(t *testing.T) {
	t.Parallel()

	evidence := []EvidenceItem{
		{Tool: "host.cpu", Query: "rate(cpu_usage[5m])", Value: 0.92, Count: 12, Timestamp: time.Now().UTC()},
		{Tool: "host.mem", Query: "rate(mem_usage[5m])", Value: 0.71, Count: 12, Timestamp: time.Now().UTC()},
	}
	remediations := []RemediationOption{
		{Action: "host.scale_up", Target: "host:i-0abc123", Risk: "mutating", AutoApprove: false},
		{Action: "host.restart", Target: "host:i-0abc123", Risk: "dangerous", AutoApprove: false},
	}
	toolset := &fakeInvestigatorToolset{
		evidenceChain: evidence,
		remediations:  remediations,
	}
	loader := &fakeCorrelatedGroupLoader{
		group: &CorrelatedGroup{
			IncidentID:   "inc-investigated-happy",
			AlertIDs:     []string{"alert-1"},
			ResourceType: "host",
			Target:       "host:i-0abc123",
			TimeWindow: TimeWindow{
				Start: time.Now().UTC().Add(-5 * time.Minute),
				End:   time.Now().UTC(),
			},
		},
	}

	fc := NewFakeLLMClient()
	fc.SetResponse(0, validRootCauseJSONFor("host_cpu", 2, 2))
	caller := NewLLMCaller(fc, WithLogger(silentInvestigatedLogger()))

	worker, err := NewInvestigatedPhaseWorker(caller, toolset, loader, silentInvestigatedLogger())
	if err != nil {
		t.Fatalf("NewInvestigatedPhaseWorker: %v", err)
	}

	plan, err := worker.Planner(context.Background(), PlanInput{
		IncidentID: "inc-investigated-happy",
		TenantID:   "tenant-a",
		Phase:      PhaseInvestigated,
		Attempt:    1,
		UpstreamContract: &ContractRef{
			ID:            101,
			Type:          "CorrelatedGroup",
			SchemaVersion: ContractSchemaV1,
		},
	})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Errorf("plan.Steps len = %d, want 1", len(plan.Steps))
	}
	if plan.Steps[0].Kind != "llm_call" {
		t.Errorf("plan.Steps[0].Kind = %q, want llm_call", plan.Steps[0].Kind)
	}
	if toolset.investigateCalls.Load() != 1 {
		t.Errorf("toolset.Investigate calls = %d, want 1", toolset.investigateCalls.Load())
	}
	if toolset.remediateCalls.Load() != 1 {
		t.Errorf("toolset.ListRemediations calls = %d, want 1", toolset.remediateCalls.Load())
	}

	exec, err := worker.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor: %v", err)
	}
	if exec.SideEffects[0].Kind != "investigation_summary" {
		t.Errorf("SideEffects[0].Kind = %q, want investigation_summary", exec.SideEffects[0].Kind)
	}
	if _, ok := exec.RawOutputs["root_cause_json"].(*RootCauseJSON); !ok {
		t.Errorf("RawOutputs[root_cause_json] missing or wrong type: %T", exec.RawOutputs["root_cause_json"])
	}
	if fc.CallCount() != 1 {
		t.Errorf("FakeLLMClient.CallCount = %d, want 1", fc.CallCount())
	}

	verdict, err := worker.Verifier(context.Background(), exec)
	if err != nil {
		t.Fatalf("Verifier: %v", err)
	}
	if !verdict.OK {
		t.Errorf("verdict.OK = false, want true (reasons=%v)", verdict.Reasons)
	}
	if verdict.Confidence <= 0 || verdict.Confidence > 1 {
		t.Errorf("verdict.Confidence = %v, want in (0,1]", verdict.Confidence)
	}
}

func TestInvestigatedWorker_PlannerUsesMCPInvestigationInput(t *testing.T) {
	t.Parallel()

	toolset := &fakeInvestigatorToolset{
		evidenceChain: []EvidenceItem{{Tool: "pg.query", Query: "pg_stat_activity"}},
		remediations:  []RemediationOption{{Action: "pg.terminate_backend", Target: "pg:primary", Risk: "mutating"}},
	}
	loader := &fakeCorrelatedGroupLoader{group: &CorrelatedGroup{
		IncidentID:   "inc-loader",
		AlertIDs:     []string{"alert-loader"},
		ResourceType: "redis",
		Target:       "redis:cache",
	}}
	worker, err := NewInvestigatedPhaseWorker(
		NewLLMCaller(NewFakeLLMClient(), WithLogger(silentInvestigatedLogger())),
		toolset,
		loader,
		silentInvestigatedLogger(),
	)
	if err != nil {
		t.Fatalf("NewInvestigatedPhaseWorker: %v", err)
	}

	plan, err := worker.Planner(context.Background(), PlanInput{
		IncidentID: "inc-input",
		TenantID:   "tenant-1",
		Phase:      PhaseInvestigated,
		AlertGroup: []string{"alert-mcp"},
		CorrelationHints: map[string]any{
			"resource_type":    "pg",
			"target":           "pg:primary",
			"suspected_causes": []any{"long-running transaction"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if toolset.lastResourceType != "pg" || toolset.lastAlertID != "alert-mcp" {
		t.Fatalf("toolset input = resource %s alert %s, want pg/alert-mcp", toolset.lastResourceType, toolset.lastAlertID)
	}
	if loader.loadN.Load() != 0 {
		t.Fatalf("correlated loader calls = %d, want 0", loader.loadN.Load())
	}
	group, ok := plan.Meta["correlated_group"].(*CorrelatedGroup)
	if !ok {
		t.Fatalf("plan correlated group type = %T", plan.Meta["correlated_group"])
	}
	if group.Target != "pg:primary" || group.RootHypothesis != "long-running transaction" {
		t.Fatalf("group = %+v, want pg:primary and MCP hypothesis", group)
	}
}

// TestInvestigatedWorker_ToolFailure 验证 investigator toolset 返回 error
// 时 Planner 立即拒绝，错误链正确 wrap。
func TestInvestigatedWorker_ToolFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("investigator: connection refused")
	toolset := &fakeInvestigatorToolset{
		evidenceErr: wantErr,
		remediations: []RemediationOption{
			{Action: "host.scale_up", Target: "host:i-0abc123", Risk: "mutating"},
		},
	}
	loader := &fakeCorrelatedGroupLoader{
		group: &CorrelatedGroup{
			IncidentID:   "inc-investigated-tool-fail",
			ResourceType: "host",
		},
	}
	fc := NewFakeLLMClient()
	caller := NewLLMCaller(fc, WithLogger(silentInvestigatedLogger()))

	worker, err := NewInvestigatedPhaseWorker(caller, toolset, loader, silentInvestigatedLogger())
	if err != nil {
		t.Fatalf("NewInvestigatedPhaseWorker: %v", err)
	}

	_, err = worker.Planner(context.Background(), PlanInput{
		IncidentID: "inc-investigated-tool-fail",
		TenantID:   "tenant-a",
		Phase:      PhaseInvestigated,
		Attempt:    1,
		UpstreamContract: &ContractRef{
			ID:            102,
			Type:          "CorrelatedGroup",
			SchemaVersion: ContractSchemaV1,
		},
	})
	if err == nil {
		t.Fatal("Planner returned err = nil, want wrapped tool failure")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Planner error chain does not contain wantErr; got: %v", err)
	}
	if !strings.Contains(err.Error(), "investigator.Investigate") {
		t.Errorf("Planner error should mention investigator.Investigate; got: %v", err)
	}
	// LLM 必须没被调用（Planner fail-fast，副作用为 0）。
	if fc.CallCount() != 0 {
		t.Errorf("FakeLLMClient.CallCount = %d, want 0 (planner rejected before LLM call)", fc.CallCount())
	}
}

// TestInvestigatedWorker_SchemaInvalidFallsBack 验证 LLM 返回缺 root_cause_object
// 的 JSON 时，Executor 基于真实 investigator 证据产出合法 RootCauseJSON。
func TestInvestigatedWorker_SchemaInvalidFallsBack(t *testing.T) {
	t.Parallel()

	evidence := []EvidenceItem{
		{Tool: "host.cpu", Timestamp: time.Now().UTC()},
	}
	remediations := []RemediationOption{
		{Action: "host.scale_up", Target: "host:i-0abc123", Risk: "mutating"},
	}
	toolset := &fakeInvestigatorToolset{
		evidenceChain: evidence,
		remediations:  remediations,
	}
	loader := &fakeCorrelatedGroupLoader{
		group: &CorrelatedGroup{
			IncidentID:   "inc-investigated-schema-invalid",
			ResourceType: "host",
		},
	}
	fc := NewFakeLLMClient()
	// 故意返回缺 root_cause_object 字段的 JSON，触发 schema 校验失败。
	fc.SetResponse(0, rootCauseJSONWithoutRootCause())
	caller := NewLLMCaller(fc, WithLogger(silentInvestigatedLogger()))

	worker, err := NewInvestigatedPhaseWorker(caller, toolset, loader, silentInvestigatedLogger())
	if err != nil {
		t.Fatalf("NewInvestigatedPhaseWorker: %v", err)
	}

	plan, err := worker.Planner(context.Background(), PlanInput{
		IncidentID: "inc-investigated-schema-invalid",
		TenantID:   "tenant-a",
		Phase:      PhaseInvestigated,
		Attempt:    1,
		UpstreamContract: &ContractRef{
			ID:            103,
			Type:          "CorrelatedGroup",
			SchemaVersion: ContractSchemaV1,
		},
	})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}

	result, err := worker.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor: %v", err)
	}
	rc, ok := result.RawOutputs["root_cause_json"].(*RootCauseJSON)
	if !ok {
		t.Fatalf("RawOutputs[root_cause_json] missing or wrong type: %T", result.RawOutputs["root_cause_json"])
	}
	if err := ValidateRootCauseJSON(rc); err != nil {
		t.Fatalf("fallback RootCauseJSON invalid: %v", err)
	}
	if len(rc.EvidenceChain) != len(evidence) || len(rc.RemediationOptions) != len(remediations) {
		t.Fatalf("fallback changed evidence/remediations: evidence=%d/%d remediations=%d/%d",
			len(rc.EvidenceChain), len(evidence), len(rc.RemediationOptions), len(remediations))
	}
}

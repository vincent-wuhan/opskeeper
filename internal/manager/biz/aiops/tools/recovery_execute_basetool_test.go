package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	hitlmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// fakeAuditRepo is the in-memory MutatingProposalAuditRepo for tests.
// approve[incidentID] = true simulates an approved mutating proposal
// row existing; otherwise HasProposalForIncident returns false. The
// error path uses errOn to inject a transient DB error.
type fakeAuditRepo struct {
	mu        sync.Mutex
	approve   map[string]bool
	strict    map[string]RecoveryProposalRequest
	checks    []RecoveryProposalRequest
	completed map[string]bool
	errOn     map[string]error
}

func newFakeAuditRepo() *fakeAuditRepo {
	return &fakeAuditRepo{
		approve:   map[string]bool{},
		strict:    map[string]RecoveryProposalRequest{},
		completed: map[string]bool{},
		errOn:     map[string]error{},
	}
}

func (f *fakeAuditRepo) ReserveApprovedProposal(_ context.Context, request RecoveryProposalRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, request)
	if err, ok := f.errOn[request.SessionID]; ok {
		return err
	}
	if !f.approve[request.SessionID] {
		return errs.ErrForbidden
	}
	if expected, ok := f.strict[request.SessionID]; ok && expected != request {
		return errs.ErrForbidden
	}
	return nil
}

func (f *fakeAuditRepo) CompleteReservedProposal(_ context.Context, proposalID string, success bool, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed[proposalID] = success
	return nil
}

// fakeDispatcher captures the inner-tool invocation. Mirrors the role
// host_restart_service plays in production (the recovery.execute
// dispatcher). Records the last argsJSON + opts.
type fakeDispatcher struct {
	name     string
	class    string
	respBody string
	respErr  error

	mu       sync.Mutex
	lastArgs string
	calls    int
}

func (f *fakeDispatcher) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        f.name,
		Description: "fake dispatcher",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Class:       f.class,
	}, nil
}

func (f *fakeDispatcher) InvokableRun(_ context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastArgs = argsJSON
	f.calls++
	if f.respErr != nil {
		return "", f.respErr
	}
	return f.respBody, nil
}

// newRecoveryExecuteToolFor builds the tool with a fake dispatcher +
// fake audit repo. Mirrors newRestartServiceToolFor so the two
// share their test fixtures.
func newRecoveryExecuteToolFor(dispatcher basetool.BaseTool, audit MutatingProposalAuditRepo) *RecoveryExecuteTool {
	return NewRecoveryExecuteTool(dispatcher, nil, audit, nil)
}

func TestRecoveryExecuteTool_Info(t *testing.T) {
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService, class: "write"}, newFakeAuditRepo())
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != ToolNameRecoveryExecute {
		t.Errorf("Name = %q, want %q", info.Name, ToolNameRecoveryExecute)
	}
	// Class="write" mirrors host_restart_service — the ReviewGate
	// decorator switches on this exact value to gate the call.
	if info.Class != "write" {
		t.Errorf("Class = %q, want write (mutating recovery must trigger review)", info.Class)
	}
	if info.Description == "" {
		t.Errorf("Description empty")
	}
	if !strings.Contains(info.WhenToUse, "approved HITL proposal") {
		t.Errorf("WhenToUse should warn the LLM about the proposal gate: %q", info.WhenToUse)
	}
	var schema map[string]any
	if err := json.Unmarshal(info.Parameters, &schema); err != nil {
		t.Errorf("Parameters not valid JSON: %v", err)
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("schema required missing or wrong type: %+v", schema["required"])
	}
	wantRequired := map[string]bool{
		"incident_id":   true,
		"skill_id":      true,
		"target":        true,
		"resource_type": true,
		"parameters":    true,
	}
	for _, k := range required {
		s, _ := k.(string)
		delete(wantRequired, s)
	}
	if len(wantRequired) != 0 {
		t.Errorf("schema required missing fields: %v", wantRequired)
	}
}

func TestRecoveryExecuteTool_HappyRestartService(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["inc-42"] = true

	dispatcher := &fakeDispatcher{
		name:     ToolNameRestartService,
		class:    "write",
		respBody: `{"device_id":7,"service":"nginx","restarted":true,"mocked":true,"started_at":"2026-08-21T10:00:00Z","ended_at":"2026-08-21T10:00:05Z"}`,
	}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)

	args := `{
		"incident_id":"inc-42",
		"proposal_id":"11111111-1111-4111-8111-111111111111",
		"skill_id":"restart-nginx",
		"target":"host-7",
		"resource_type":"host",
		"baseline_window":"5m",
		"compare_window":"2m",
		"tolerance":0.15,
		"parameters":{
			"command":"restart_service",
			"device_id":7,
			"service":"nginx",
			"reason":"verify_recovery passed=false; cpu_usage 0.45 -> 0.05 delta"
		}
	}`
	out, err := tool.InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var env recoveryExecuteEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.SchemaVersion != "v1" {
		t.Errorf("SchemaVersion = %q, want v1", env.SchemaVersion)
	}
	if env.IncidentID != "inc-42" {
		t.Errorf("IncidentID = %q, want inc-42", env.IncidentID)
	}
	if !env.Executed {
		t.Errorf("Executed = false, want true")
	}
	if env.Command != "restart_service" {
		t.Errorf("Command = %q, want restart_service", env.Command)
	}
	if env.RestartedAt.IsZero() {
		t.Errorf("RestartedAt = zero, want non-zero UTC stamp")
	}
	if len(env.ResultJSON) == 0 {
		t.Errorf("ResultJSON empty, want embedded restart_service envelope")
	}

	// Dispatcher must have been called once with mapped inner args.
	if dispatcher.calls != 1 {
		t.Errorf("dispatcher.calls = %d, want 1", dispatcher.calls)
	}
	var sentInner map[string]any
	if err := json.Unmarshal([]byte(dispatcher.lastArgs), &sentInner); err != nil {
		t.Fatalf("decode dispatcher args: %v", err)
	}
	if sentInner["device_id"].(float64) != 7 {
		t.Errorf("inner device_id = %v, want 7", sentInner["device_id"])
	}
	if sentInner["service"].(string) != "nginx" {
		t.Errorf("inner service = %v, want nginx", sentInner["service"])
	}
	if !strings.Contains(sentInner["reason"].(string), "verify_recovery") {
		t.Errorf("inner reason = %v, want contains 'verify_recovery'", sentInner["reason"])
	}

	// Audit must have been queried exactly once with the right id.
	if len(audit.checks) != 1 || audit.checks[0].SessionID != "inc-42" {
		t.Errorf("audit.checks = %v, want [inc-42]", audit.checks)
	}
}

func TestRecoveryExecuteTool_HappyNoop(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["inc-77"] = true
	dispatcher := &fakeDispatcher{name: ToolNameRestartService}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)

	args := `{
		"incident_id":"inc-77",
		"proposal_id":"22222222-2222-4222-8222-222222222222",
		"skill_id":"noop-1",
		"target":"pg-cluster-x",
		"resource_type":"pg",
		"parameters":{"command":"noop"}
	}`
	out, err := tool.InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var env recoveryExecuteEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Command != "noop" {
		t.Errorf("Command = %q, want noop", env.Command)
	}
	if !env.Executed {
		t.Errorf("Executed = false, want true")
	}
	if len(env.ResultJSON) != 0 {
		t.Errorf("ResultJSON = %q, want empty for noop", string(env.ResultJSON))
	}
	// Dispatcher MUST NOT be called on noop.
	if dispatcher.calls != 0 {
		t.Errorf("dispatcher.calls = %d, want 0 (noop path)", dispatcher.calls)
	}
}

func TestRecoveryExecuteTool_MissingProposal(t *testing.T) {
	audit := newFakeAuditRepo() // empty — no approved proposals
	dispatcher := &fakeDispatcher{name: ToolNameRestartService}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)

	args := `{
		"incident_id":"inc-no-proposal",
		"skill_id":"x",
		"target":"host-1",
		"resource_type":"host",
		"parameters":{"command":"restart_service","device_id":1,"service":"nginx","reason":"drill"}
	}`
	_, err := tool.InvokableRun(context.Background(), args)
	if err == nil {
		t.Fatalf("expected missing-proposal error")
	}
	if !strings.Contains(err.Error(), "approved proposal_id required") {
		t.Errorf("err = %v, want 'approved proposal_id required' substring", err)
	}
	// The dispatcher must NOT be called when the proposal gate fails —
	// that's the whole point of the inner guard.
	if dispatcher.calls != 0 {
		t.Errorf("dispatcher.calls = %d, want 0 (audit gate must block dispatch)", dispatcher.calls)
	}
	if len(audit.checks) != 0 {
		t.Errorf("audit.checks = %v, want [inc-no-proposal]", audit.checks)
	}
}

func TestRecoveryExecuteTool_SkipAuditBypassesOfflineCaller(t *testing.T) {
	audit := newFakeAuditRepo() // no approvals — gate would block
	dispatcher := &fakeDispatcher{
		name:     ToolNameRestartService,
		class:    "write",
		respBody: `{"device_id":3,"service":"redis","restarted":true}`,
	}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)

	args := `{
		"incident_id":"inc-drill",
		"skill_id":"x",
		"target":"host-3",
		"resource_type":"host",
		"parameters":{"command":"restart_service","device_id":3,"service":"redis","reason":"drill","skip_audit":true}
	}`
	out, err := tool.InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(out, `"executed":true`) {
		t.Errorf("output missing executed=true: %s", out)
	}
	// skip_audit=true means the repo MUST NOT be consulted.
	if len(audit.checks) != 0 {
		t.Errorf("audit.checks = %v, want [] (skip_audit=true)", audit.checks)
	}
}

func TestRecoveryExecuteTool_AgentTeamsRejectsSkipAudit(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["inc-agentteams"] = true
	dispatcher := &fakeDispatcher{name: ToolNameRestartService}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)
	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{
		AgentTeams: &tenantctx.AgentTeamsIdentity{
			TenantID: "tenant-a", Service: "agentteams",
			Worker: "opskeeper-repairer", Role: "repairer",
			AllowedTools: []string{"recovery.execute"},
		},
	})

	args := `{
		"incident_id":"inc-agentteams",
		"proposal_id":"33333333-3333-4333-8333-333333333333",
		"skill_id":"x",
		"target":"host-3",
		"resource_type":"host",
		"parameters":{"command":"noop","skip_audit":true}
	}`
	_, err := tool.InvokableRun(ctx, args)
	if err == nil {
		t.Fatal("AgentTeams skip_audit call succeeded")
	}
	if !strings.Contains(err.Error(), "skip_audit is forbidden for this caller and command") {
		t.Fatalf("err = %v, want forbidden skip_audit", err)
	}
	if len(audit.checks) != 0 || dispatcher.calls != 0 {
		t.Fatalf("audit.checks = %v, dispatcher.calls = %d; want no gate lookup or dispatch", audit.checks, dispatcher.calls)
	}
}

func TestRecoveryExecuteTool_ReservesExactProposalAndCompletesExecuted(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["inc-exact"] = true
	audit.strict["inc-exact"] = RecoveryProposalRequest{
		ProposalID: "66666666-6666-4666-8666-666666666666",
		SessionID:  "inc-exact", Kind: "agentteams_hitl",
		Action: "restart_service", Resource: "host:worker-1",
		Execution: hitlmodel.RecoveryExecutionParameters{
			Command: "restart_service", DeviceID: 7, Service: "nginx", Reason: "exact restart",
		},
	}
	dispatcher := &fakeDispatcher{name: ToolNameRestartService}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)
	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{
		AgentTeams: &tenantctx.AgentTeamsIdentity{
			TenantID: "tenant-a", Service: "agentteams",
			Worker: "opskeeper-repairer", Role: "repairer",
			AllowedTools: []string{"recovery.execute"},
		},
	})

	args := `{
		"incident_id":"inc-exact",
		"proposal_id":"66666666-6666-4666-8666-666666666666",
		"skill_id":"restart",
		"target":"host:worker-1",
		"resource_type":"host",
		"parameters":{"command":"restart_service","device_id":7,"service":"nginx","reason":"exact restart"}
	}`
	if _, err := tool.InvokableRun(ctx, args); err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if len(audit.checks) != 1 {
		t.Fatalf("checks = %+v, want one", audit.checks)
	}
	check := audit.checks[0]
	if check.ProposalID != "66666666-6666-4666-8666-666666666666" ||
		check.SessionID != "inc-exact" || check.Kind != "agentteams_hitl" ||
		check.Action != "restart_service" || check.Resource != "host:worker-1" ||
		check.Execution != (hitlmodel.RecoveryExecutionParameters{
			Command: "restart_service", DeviceID: 7, Service: "nginx", Reason: "exact restart",
		}) {
		t.Fatalf("exact request mismatch: %+v", check)
	}
	if !audit.completed[check.ProposalID] {
		t.Fatalf("proposal was not completed successfully: %+v", audit.completed)
	}
}

func TestRecoveryExecuteTool_RejectsProposalForDifferentActionOrResource(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["inc-exact"] = true
	audit.strict["inc-exact"] = RecoveryProposalRequest{
		ProposalID: "77777777-7777-4777-8777-777777777777",
		SessionID:  "inc-exact", Kind: "agentteams_hitl",
		Action: "noop", Resource: "host:worker-1",
	}
	dispatcher := &fakeDispatcher{name: ToolNameRestartService}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)

	args := `{
		"incident_id":"inc-exact",
		"proposal_id":"77777777-7777-4777-8777-777777777777",
		"skill_id":"restart",
		"target":"host:other",
		"resource_type":"host",
		"parameters":{"command":"noop"}
	}`
	_, err := tool.InvokableRun(context.Background(), args)
	if !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("err = %v, want forbidden", err)
	}
	if dispatcher.calls != 0 || len(audit.completed) != 0 {
		t.Fatalf("dispatcher.calls=%d completed=%v; want no execution", dispatcher.calls, audit.completed)
	}
}

func TestRecoveryExecuteTool_AgentTeamsRequiresApprovedProposal(t *testing.T) {
	audit := newFakeAuditRepo()
	dispatcher := &fakeDispatcher{name: ToolNameRestartService}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)
	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{
		AgentTeams: &tenantctx.AgentTeamsIdentity{
			TenantID: "tenant-a", Service: "agentteams",
			Worker: "opskeeper-repairer", Role: "repairer",
			AllowedTools: []string{"recovery.execute"},
		},
	})

	args := `{
		"incident_id":"inc-agentteams",
		"skill_id":"x",
		"target":"host-3",
		"resource_type":"host",
		"parameters":{"command":"restart_service","device_id":3,"service":"nginx","reason":"restart after RCA"}
	}`
	_, err := tool.InvokableRun(ctx, args)
	if err == nil {
		t.Fatal("AgentTeams call without proposal succeeded")
	}
	if !strings.Contains(err.Error(), "approved proposal_id required") {
		t.Fatalf("err = %v, want missing proposal", err)
	}
	if len(audit.checks) != 0 || dispatcher.calls != 0 {
		t.Fatalf("audit.checks = %v, dispatcher.calls = %d; want no lookup and no dispatch", audit.checks, dispatcher.calls)
	}
}

func TestRecoveryExecuteTool_AuditLookupError(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.errOn["inc-flake"] = errs.ErrEdgeOffline
	dispatcher := &fakeDispatcher{name: ToolNameRestartService}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)

	args := `{
		"incident_id":"inc-flake",
		"proposal_id":"44444444-4444-4444-8444-444444444444",
		"skill_id":"x",
		"target":"host-1",
		"resource_type":"host",
		"parameters":{"command":"noop"}
	}`
	_, err := tool.InvokableRun(context.Background(), args)
	if err == nil {
		t.Fatalf("expected audit-lookup error")
	}
	if !errors.Is(err, errs.ErrEdgeOffline) {
		t.Errorf("err should wrap ErrEdgeOffline: %v", err)
	}
}

func TestRecoveryExecuteTool_MissingIncidentID(t *testing.T) {
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, newFakeAuditRepo())
	_, err := tool.InvokableRun(context.Background(), `{"skill_id":"x","target":"host-1","resource_type":"host","parameters":{"command":"noop"}}`)
	if err == nil || !strings.Contains(err.Error(), "incident_id") {
		t.Errorf("expected incident_id error, got %v", err)
	}
}

func TestRecoveryExecuteTool_MissingSkillID(t *testing.T) {
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, newFakeAuditRepo())
	_, err := tool.InvokableRun(context.Background(), `{"incident_id":"i","target":"host-1","resource_type":"host","parameters":{"command":"noop"}}`)
	if err == nil || !strings.Contains(err.Error(), "skill_id") {
		t.Errorf("expected skill_id error, got %v", err)
	}
}

func TestRecoveryExecuteTool_MissingTarget(t *testing.T) {
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, newFakeAuditRepo())
	_, err := tool.InvokableRun(context.Background(), `{"incident_id":"i","skill_id":"x","resource_type":"host","parameters":{"command":"noop"}}`)
	if err == nil || !strings.Contains(err.Error(), "target") {
		t.Errorf("expected target error, got %v", err)
	}
}

func TestRecoveryExecuteTool_MissingResourceType(t *testing.T) {
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, newFakeAuditRepo())
	_, err := tool.InvokableRun(context.Background(), `{"incident_id":"i","skill_id":"x","target":"host-1","parameters":{"command":"noop"}}`)
	if err == nil || !strings.Contains(err.Error(), "resource_type") {
		t.Errorf("expected resource_type error, got %v", err)
	}
}

func TestRecoveryExecuteTool_MissingParameters(t *testing.T) {
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, newFakeAuditRepo())
	_, err := tool.InvokableRun(context.Background(), `{"incident_id":"i","skill_id":"x","target":"host-1","resource_type":"host"}`)
	if err == nil || !strings.Contains(err.Error(), "parameters") {
		t.Errorf("expected parameters error, got %v", err)
	}
}

func TestRecoveryExecuteTool_MissingCommand(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["i"] = true
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, audit)
	_, err := tool.InvokableRun(context.Background(), `{"incident_id":"i","skill_id":"x","target":"host-1","resource_type":"host","parameters":{}}`)
	if err == nil || !strings.Contains(err.Error(), "command") {
		t.Errorf("expected command-required error, got %v", err)
	}
}

func TestRecoveryExecuteTool_UnknownCommand(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["i"] = true
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, audit)
	_, err := tool.InvokableRun(context.Background(), `{"incident_id":"i","skill_id":"x","target":"host-1","resource_type":"host","parameters":{"command":"rm_rf"}}`)
	if err == nil || !strings.Contains(err.Error(), "unknown parameters.command") {
		t.Errorf("expected unknown-command error, got %v", err)
	}
}

func TestRecoveryExecuteTool_RestartMissingDeviceID(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["i"] = true
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, audit)
	_, err := tool.InvokableRun(context.Background(), `{"incident_id":"i","proposal_id":"55555555-5555-4555-8555-555555555555","skill_id":"x","target":"host-1","resource_type":"host","parameters":{"command":"restart_service","service":"nginx"}}`)
	if err == nil || !strings.Contains(err.Error(), "device_id") {
		t.Errorf("expected device_id error, got %v", err)
	}
}

func TestRecoveryExecuteTool_RestartMissingService(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["i"] = true
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, audit)
	_, err := tool.InvokableRun(context.Background(), `{"incident_id":"i","proposal_id":"55555555-5555-4555-8555-555555555555","skill_id":"x","target":"host-1","resource_type":"host","parameters":{"command":"restart_service","device_id":1}}`)
	if err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("expected service error, got %v", err)
	}
}

func TestRecoveryExecuteTool_DispatcherError(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["i"] = true
	dispatcher := &fakeDispatcher{
		name:    ToolNameRestartService,
		respErr: errs.ErrEdgeOffline,
	}
	tool := newRecoveryExecuteToolFor(dispatcher, audit)
	_, err := tool.InvokableRun(context.Background(), `{"incident_id":"i","proposal_id":"55555555-5555-4555-8555-555555555555","skill_id":"x","target":"host-1","resource_type":"host","parameters":{"command":"restart_service","device_id":1,"service":"nginx","reason":"restart after RCA"}}`)
	if err == nil {
		t.Fatalf("expected dispatch error")
	}
	if !errors.Is(err, errs.ErrEdgeOffline) {
		t.Errorf("err should wrap ErrEdgeOffline: %v", err)
	}
}

func TestRecoveryExecuteTool_BadArgsJSON(t *testing.T) {
	tool := newRecoveryExecuteToolFor(&fakeDispatcher{name: ToolNameRestartService}, newFakeAuditRepo())
	_, err := tool.InvokableRun(context.Background(), `{not json`)
	if err == nil || !strings.Contains(err.Error(), "bad args") {
		t.Errorf("expected bad-args error, got %v", err)
	}
}

func TestAppendRecoveryExecuteTool_NilDepsReturnsUnchanged(t *testing.T) {
	if got := AppendRecoveryExecuteTool(nil, nil, nil, nil, nil); got != nil {
		t.Errorf("expected nil-deps to return unchanged slice, got len=%d", len(got))
	}
	// Nil audit repo (with dispatcher wired) also keeps slice unchanged —
	// production wiring must NOT register the tool without an audit
	// seam.
	if got := AppendRecoveryExecuteTool(nil, &fakeDispatcher{name: ToolNameRestartService}, nil, nil, nil); got != nil {
		t.Errorf("nil auditRepo should keep slice unchanged, got len=%d", len(got))
	}
}

type fakeHostFixtureTerminator struct {
	calls    []HostProcessTerminationRequest
	results  map[string]json.RawMessage
	statuses map[string]HostFixtureStatus
	err      error
}

func (f *fakeHostFixtureTerminator) Status(_ context.Context, request HostProcessTerminationRequest) (HostFixtureStatus, error) {
	status, ok := f.statuses[request.FixtureManifestID]
	if !ok {
		return HostFixtureStatus{}, errs.ErrNotFound
	}
	return status, nil
}

func (f *fakeHostFixtureTerminator) Terminate(_ context.Context, request HostProcessTerminationRequest) (json.RawMessage, error) {
	f.calls = append(f.calls, request)
	if f.err != nil {
		return nil, f.err
	}
	return f.results[request.FixtureManifestID], nil
}

func TestRecoveryExecuteTool_KillProcessUsesExactApprovedTarget(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["host-cpu"] = true
	approvedExecution := hitlmodel.RecoveryExecutionParameters{
		Command: "kill_process", IncidentID: "host-cpu", FixtureManifestID: "f4b1c0a19d3e5f7a",
		Reason: "terminate top CPU fixture",
	}
	audit.strict["host-cpu"] = RecoveryProposalRequest{
		ProposalID: "88888888-8888-4888-8888-888888888888",
		SessionID:  "host-cpu", Kind: "agentteams_hitl",
		Action: "kill_process", Resource: "host:fixture", Execution: approvedExecution,
	}
	terminator := &fakeHostFixtureTerminator{
		statuses: map[string]HostFixtureStatus{
			"f4b1c0a19d3e5f7a": {ManifestID: "f4b1c0a19d3e5f7a", IncidentID: "host-cpu", Resource: "host:fixture", Status: "running"},
		},
		results: map[string]json.RawMessage{
			"f4b1c0a19d3e5f7a": json.RawMessage(`{"status":"terminated","owned_process_count":0}`),
		},
	}
	tool := NewRecoveryExecuteTool(&fakeDispatcher{name: ToolNameRestartService}, terminator, audit, nil)
	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{
		AgentTeams: &tenantctx.AgentTeamsIdentity{
			TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-repairer",
			Role: "repairer", AllowedTools: []string{"recovery.execute"},
		},
	})
	args := `{
		"incident_id":"host-cpu",
		"proposal_id":"88888888-8888-4888-8888-888888888888",
		"skill_id":"host-kill-fixture",
		"target":"host:fixture",
		"resource_type":"host",
		"parameters":{"command":"kill_process","incident_id":"host-cpu","fixture_manifest_id":"f4b1c0a19d3e5f7a","reason":"terminate top CPU fixture"}
	}`
	out, err := tool.InvokableRun(ctx, args)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if len(terminator.calls) != 1 || terminator.calls[0].IncidentID != "host-cpu" ||
		terminator.calls[0].FixtureManifestID != "f4b1c0a19d3e5f7a" {
		t.Fatalf("terminator calls = %+v", terminator.calls)
	}
	if len(audit.checks) != 1 || audit.checks[0].Execution != approvedExecution {
		t.Fatalf("audit checks = %+v", audit.checks)
	}
	if !audit.completed[audit.checks[0].ProposalID] {
		t.Fatalf("proposal not executed: %+v", audit.completed)
	}
	if !strings.Contains(out, `"status":"terminated"`) {
		t.Fatalf("output missing fixture result: %s", out)
	}
}

func TestRecoveryExecuteTool_KillProcessRejectsTargetMismatchBeforeReservation(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["host-cpu"] = true
	approvedExecution := hitlmodel.RecoveryExecutionParameters{
		Command: "kill_process", IncidentID: "host-cpu", FixtureManifestID: "f4b1c0a19d3e5f7a", Reason: "x",
	}
	audit.strict["host-cpu"] = RecoveryProposalRequest{
		ProposalID: "88888888-8888-4888-8888-888888888888", SessionID: "host-cpu",
		Kind: "agentteams_hitl", Action: "kill_process", Resource: "host:fixture", Execution: approvedExecution,
	}
	terminator := &fakeHostFixtureTerminator{statuses: map[string]HostFixtureStatus{
		"f4b1c0a19d3e5f7a": {ManifestID: "f4b1c0a19d3e5f7a", IncidentID: "host-cpu", Resource: "host:fixture", Status: "running"},
	}}
	tool := NewRecoveryExecuteTool(&fakeDispatcher{name: ToolNameRestartService}, terminator, audit, nil)
	args := `{
		"incident_id":"host-cpu","proposal_id":"88888888-8888-4888-8888-888888888888",
		"skill_id":"x","target":"host:other","resource_type":"host",
		"parameters":{"command":"kill_process","incident_id":"host-cpu","fixture_manifest_id":"f4b1c0a19d3e5f7a","reason":"x"}
	}`
	_, err := tool.InvokableRun(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "does not exactly match") {
		t.Fatalf("err = %v, want exact fixture binding failure", err)
	}
	if len(terminator.calls) != 0 || len(audit.checks) != 0 {
		t.Fatalf("unexpected execution: calls=%+v checks=%+v", terminator.calls, audit.checks)
	}
}

type fakePoolRecoveryExecutor struct {
	calls  []PoolRecoveryRequest
	status PoolFixtureStatus
	result json.RawMessage
	err    error
}

func (f *fakePoolRecoveryExecutor) Status(_ context.Context, request PoolRecoveryRequest) (PoolFixtureStatus, error) {
	f.calls = append(f.calls, request)
	return f.status, nil
}

func (f *fakePoolRecoveryExecutor) Recover(_ context.Context, request PoolRecoveryRequest) (json.RawMessage, error) {
	f.calls = append(f.calls, request)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func TestRecoveryExecuteTool_ResizePoolUsesExactApprovedTargetAndNewProbe(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["pg-pool"] = true
	approvedExecution := hitlmodel.RecoveryExecutionParameters{
		Command: "resize_pool", IncidentID: "pg-pool", PoolManifestID: "p4b1c0a19d3e5f7a",
		Reason: "resize pool and recycle idle sessions",
	}
	audit.strict["pg-pool"] = RecoveryProposalRequest{
		ProposalID: "99999999-9999-4999-9999-999999999999", SessionID: "pg-pool",
		Kind: "agentteams_hitl", Action: "resize_pool", Resource: "pg:pool-fixture", Execution: approvedExecution,
	}
	recoverer := &fakePoolRecoveryExecutor{
		status: PoolFixtureStatus{
			ManifestID: "p4b1c0a19d3e5f7a", IncidentID: "pg-pool",
			Resource: "pg:pool-fixture", Status: "running",
		},
		result: json.RawMessage(`{"status":"recovered","recovery_probe":{"status":"success"}}`),
	}
	recoverer.status.FailedProbe.Status = "failed"
	tool := NewRecoveryExecuteTool(&fakeDispatcher{name: ToolNameRestartService}, nil, audit, nil)
	tool.SetPoolRecoveryExecutor(recoverer)
	args := `{
		"incident_id":"pg-pool",
		"proposal_id":"99999999-9999-4999-9999-999999999999",
		"skill_id":"pg-resize-pool",
		"target":"pg:pool-fixture","resource_type":"pg",
		"parameters":{"command":"resize_pool","incident_id":"pg-pool","pool_manifest_id":"p4b1c0a19d3e5f7a","reason":"resize pool and recycle idle sessions"}
	}`
	out, err := tool.InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if len(recoverer.calls) != 2 || recoverer.calls[0].PoolManifestID != "p4b1c0a19d3e5f7a" ||
		recoverer.calls[1].IncidentID != "pg-pool" {
		t.Fatalf("recoverer calls = %+v", recoverer.calls)
	}
	if len(audit.checks) != 1 || audit.checks[0].Execution != approvedExecution {
		t.Fatalf("audit checks = %+v", audit.checks)
	}
	if !audit.completed[audit.checks[0].ProposalID] {
		t.Fatalf("proposal not executed: %+v", audit.completed)
	}
	if !strings.Contains(out, `"recovery_probe":{"status":"success"}`) {
		t.Fatalf("output missing recovery probe: %s", out)
	}
}

func TestRecoveryExecuteTool_ResizePoolRejectsMissingFailedProbeBeforeReservation(t *testing.T) {
	audit := newFakeAuditRepo()
	audit.approve["pg-pool"] = true
	recoverer := &fakePoolRecoveryExecutor{
		status: PoolFixtureStatus{
			ManifestID: "p4b1c0a19d3e5f7a", IncidentID: "pg-pool",
			Resource: "pg:pool-fixture", Status: "running",
		},
	}
	tool := NewRecoveryExecuteTool(&fakeDispatcher{name: ToolNameRestartService}, nil, audit, nil)
	tool.SetPoolRecoveryExecutor(recoverer)
	args := `{
		"incident_id":"pg-pool","proposal_id":"99999999-9999-4999-9999-999999999999",
		"skill_id":"x","target":"pg:pool-fixture","resource_type":"pg",
		"parameters":{"command":"resize_pool","incident_id":"pg-pool","pool_manifest_id":"p4b1c0a19d3e5f7a","reason":"x"}
	}`
	_, err := tool.InvokableRun(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "exhausted incident-owned pool") {
		t.Fatalf("err = %v, want failed-probe gate", err)
	}
	if len(recoverer.calls) != 1 || len(audit.checks) != 0 {
		t.Fatalf("unexpected execution: calls=%+v checks=%+v", recoverer.calls, audit.checks)
	}
}

func TestRecoveryExecuteTool_KillProcessRejectsMissingTerminatorBeforeReservation(t *testing.T) {
	audit := newFakeAuditRepo()
	tool := NewRecoveryExecuteTool(&fakeDispatcher{name: ToolNameRestartService}, nil, audit, nil)
	args := `{
		"incident_id":"host-cpu",
		"skill_id":"x","target":"host:fixture","resource_type":"host",
		"parameters":{"command":"kill_process","incident_id":"host-cpu","fixture_manifest_id":"f4b1c0a19d3e5f7a","reason":"x"}
	}`
	_, err := tool.InvokableRun(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "terminator not wired") {
		t.Fatalf("err = %v, want unwired terminator", err)
	}
	if len(audit.checks) != 0 || len(audit.completed) != 0 {
		t.Fatalf("unexpected audit state: checks=%+v completed=%+v", audit.checks, audit.completed)
	}
}

func TestRecoveryExecuteTool_KillProcessAlwaysRequiresProposal(t *testing.T) {
	audit := newFakeAuditRepo()
	terminator := &fakeHostFixtureTerminator{}
	tool := NewRecoveryExecuteTool(&fakeDispatcher{name: ToolNameRestartService}, terminator, audit, nil)
	args := `{
		"incident_id":"host-cpu",
		"skill_id":"x","target":"host:fixture","resource_type":"host",
		"parameters":{"command":"kill_process","incident_id":"host-cpu","fixture_manifest_id":"f4b1c0a19d3e5f7a","reason":"x"}
	}`
	_, err := tool.InvokableRun(context.Background(), args)
	if err == nil {
		t.Fatal("kill_process without proposal unexpectedly succeeded")
	}
	if len(terminator.calls) != 0 || len(audit.checks) != 0 {
		t.Fatalf("unexpected execution: calls=%+v checks=%+v", terminator.calls, audit.checks)
	}
}

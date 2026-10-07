package loop

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	loopmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/loop"
)

// fakeContractRepo is a ContractRepo that serves one contract, and records
// which phase and type were asked for.
//
// The recording is what lets the loader test assert that the approval reads
// the investigated phase's row specifically: a loader that queried the wrong
// phase would still return a contract in a test that only checked the
// payload.
type fakeContractRepo struct {
	contract *loopmodel.Contract
	err      error

	readPhase  Phase
	readType   string
	readTenant string
}

func (f *fakeContractRepo) WriteContract(_ context.Context, _ *loopmodel.Contract) error {
	return nil
}

func (f *fakeContractRepo) ReadContract(_ context.Context, tenantID, _ string, phase Phase, contractType string) (*loopmodel.Contract, error) {
	f.readPhase = phase
	f.readType = contractType
	f.readTenant = tenantID
	if f.err != nil {
		return nil, f.err
	}
	return f.contract, nil
}

// noRootCauseLoader stands for "the run never reached the investigated
// phase", which the loader contract reports as (nil, nil).
type noRootCauseLoader struct{}

func (noRootCauseLoader) LoadRootCause(_ context.Context, _, _ string) (*RootCauseJSON, error) {
	return nil, nil
}

// The tests in this file cover the dispatch contract: what may run, in what
// order, what it is recorded as when it fails, and — the one that matters
// most — that a run which performed nothing cannot report success.

func opt(action, risk string, auto bool) RemediationOption {
	return RemediationOption{Action: action, Target: "pg:alert-1", Risk: risk, AutoApprove: auto}
}

func TestSelectRemediation_PrefersThePreApprovedAction(t *testing.T) {
	// The first option is safe but not pre-approved. Selecting it would be
	// running an unapproved write because it looked attractive.
	got, err := SelectRemediation([]RemediationOption{
		opt("pg.terminate_long_tx", "safe", false),
		opt("pg.vacuum_analyze", "mutating", true),
	})
	if err != nil {
		t.Fatalf("SelectRemediation: %v", err)
	}
	if got.Action != "pg.vacuum_analyze" {
		t.Errorf("selected %q, want the pre-approved pg.vacuum_analyze", got.Action)
	}
}

func TestSelectRemediation_ChoosesTheLeastInvasiveEligibleTier(t *testing.T) {
	got, err := SelectRemediation([]RemediationOption{
		opt("host.restart_service", "dangerous", true),
		opt("host.garbage_collect", "safe", true),
		opt("k8s.evict_pod", "mutating", true),
	})
	if err != nil {
		t.Fatalf("SelectRemediation: %v", err)
	}
	if got.Action != "host.garbage_collect" {
		t.Errorf("selected %q, want the safe tier", got.Action)
	}
}

// The risk tiers are compared by an explicit ranking, not by their strings.
// "dangerous" sorts before "mutating" sorts before "safe", so a string
// comparison would pick the most destructive option every time.
func TestSelectRemediation_RiskOrderIsNotAlphabetical(t *testing.T) {
	got, err := SelectRemediation([]RemediationOption{
		opt("a.dangerous", "dangerous", true),
		opt("z.safe", "safe", true),
	})
	if err != nil {
		t.Fatalf("SelectRemediation: %v", err)
	}
	if got.Action != "z.safe" {
		t.Errorf("selected %q; a string comparison of the risk tiers would have chosen the dangerous one", got.Action)
	}
}

func TestSelectRemediation_PreservesInvestigatorOrderWithinATier(t *testing.T) {
	// The investigator ranked these against evidence the executor cannot
	// see. Re-sorting within a tier would discard that ranking.
	got, err := SelectRemediation([]RemediationOption{
		opt("first.mutating", "mutating", true),
		opt("second.mutating", "mutating", true),
	})
	if err != nil {
		t.Fatalf("SelectRemediation: %v", err)
	}
	if got.Action != "first.mutating" {
		t.Errorf("selected %q, want the investigator's first choice", got.Action)
	}
}

func TestSelectRemediation_AnUnrecognisedRiskIsNotTreatedAsSafe(t *testing.T) {
	got, err := SelectRemediation([]RemediationOption{
		opt("mystery.action", "???", true),
		opt("known.action", "safe", true),
	})
	if err != nil {
		t.Fatalf("SelectRemediation: %v", err)
	}
	if got.Action != "known.action" {
		t.Errorf("selected %q; an unknown risk must rank below every known tier, not above it", got.Action)
	}
}

func TestSelectRemediation_ReportsWhenNothingIsEligible(t *testing.T) {
	// This is not an empty-input case: the loop reached the approved phase
	// with real options, all of which a human has to sign off on. Stopping
	// is the correct outcome, not a failure to find something to do.
	_, err := SelectRemediation([]RemediationOption{
		opt("pg.terminate_long_tx", "mutating", false),
		opt("host.restart_service", "dangerous", false),
	})
	if !errors.Is(err, ErrNoRemediationSelected) {
		t.Fatalf("err = %v, want ErrNoRemediationSelected", err)
	}
	if !strings.Contains(err.Error(), "2 proposed") {
		t.Errorf("the error should say how many were proposed: %v", err)
	}
}

func TestSelectRemediation_SkipsOptionsWithNoActionName(t *testing.T) {
	got, err := SelectRemediation([]RemediationOption{
		opt("", "safe", true),
		opt("pg.vacuum_analyze", "safe", true),
	})
	if err != nil {
		t.Fatalf("SelectRemediation: %v", err)
	}
	if got.Action != "pg.vacuum_analyze" {
		t.Errorf("selected %q; an option with no action name is not dispatchable", got.Action)
	}
}

// ── RegistryInvoker ─────────────────────────────────────────────────────

type fakeToolCaller struct {
	specs     map[string]ToolSpec
	calls     []string
	args      map[string]any
	result    any
	callErr   error
	callCount int
}

func (f *fakeToolCaller) LookupTool(name string) (ToolSpec, bool) {
	s, ok := f.specs[name]
	return s, ok
}

func (f *fakeToolCaller) CallTool(_ context.Context, name string, args map[string]any) (any, error) {
	f.callCount++
	f.calls = append(f.calls, name)
	f.args = args
	return f.result, f.callErr
}

func callerWith(specs ...ToolSpec) *fakeToolCaller {
	f := &fakeToolCaller{specs: map[string]ToolSpec{}}
	for _, s := range specs {
		f.specs[s.Name] = s
	}
	return f
}

func TestRegistryInvoker_DispatchesTheSelectedAction(t *testing.T) {
	tools := callerWith(ToolSpec{
		Name:         "pg.vacuum_analyze",
		Description:  "VACUUM (ANALYZE)",
		RiskLevel:    "L2",
		RequiredArgs: nil,
	})
	inv := RegistryInvoker{Tools: tools}

	out, err := inv.Invoke(context.Background(), RemediationRequest{
		IncidentID: "inc-1",
		TenantID:   "tenant-1",
		Option:     opt("pg.vacuum_analyze", "safe", true),
		Approver:   "policy:auto",
		Reason:     "closed loop",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Status != RemediationStatusSuccess {
		t.Errorf("Status = %q, want success", out.Status)
	}
	if len(tools.calls) != 1 || tools.calls[0] != "pg.vacuum_analyze" {
		t.Errorf("calls = %v, want one call to pg.vacuum_analyze", tools.calls)
	}
	// The approver is injected here so that the adapter's approval gate is
	// unreachable from the dispatch layer — a bug in this file cannot
	// switch the gate off.
	if got, _ := tools.args["approved_by"].(string); got != "policy:auto" {
		t.Errorf("approved_by = %q, want policy:auto", got)
	}
	if got, _ := tools.args["target"].(string); got != "pg:alert-1" {
		t.Errorf("target = %q, want the option's target", got)
	}
}

// TestRegistryInvoker_CarriesTheExecutedArgvOutOfTheToolResult: the adapter
// is the only layer that knows the vector it handed to exec, and it returns
// it in the result bag. Dropping it here is what leaves the crystalliser
// with nothing to re-run — the gap decision 157 closed on the event side
// would otherwise reopen on the dispatch side.
func TestRegistryInvoker_CarriesTheExecutedArgvOutOfTheToolResult(t *testing.T) {
	tools := callerWith(ToolSpec{Name: "host.restart_service", RequiredArgs: []string{"unit"}})
	tools.result = map[string]interface{}{
		"operation": "restart_service",
		"success":   true,
		"argv":      []string{"systemctl", "restart", "nginx.service"},
	}
	inv := RegistryInvoker{Tools: tools, ArgResolver: ArgResolverFunc(
		func(_ context.Context, _ RemediationRequest, _ ToolSpec) (map[string]any, error) {
			return map[string]any{"unit": "nginx.service"}, nil
		})}

	out, err := inv.Invoke(context.Background(), RemediationRequest{
		Option:   RemediationOption{Action: "host.restart_service", Target: "host:edge-1"},
		Approver: "policy:auto",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.Join(out.Argv, " ") != "systemctl restart nginx.service" {
		t.Errorf("out.Argv = %v, want the executed vector carried out of the result bag", out.Argv)
	}
}

// TestRegistryInvoker_NoArgvMeansNothingToPromote: a tool that reached its
// change without an exec (or a read) must leave Argv empty rather than
// fabricate one. TrialOf reads that emptiness as "nothing to crystallise".
func TestRegistryInvoker_NoArgvMeansNothingToPromote(t *testing.T) {
	tools := callerWith(ToolSpec{Name: "pg.vacuum_analyze"})
	tools.result = map[string]interface{}{"operation": "vacuum_analyze", "success": true}
	inv := RegistryInvoker{Tools: tools}

	out, err := inv.Invoke(context.Background(), RemediationRequest{
		Option:   opt("pg.vacuum_analyze", "safe", true),
		Approver: "policy:auto",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(out.Argv) != 0 {
		t.Errorf("out.Argv = %v, want empty when the tool executed no vector", out.Argv)
	}
}

func TestRegistryInvoker_RefusesAnActionWithNoRegisteredTool(t *testing.T) {
	tools := callerWith() // empty registry
	inv := RegistryInvoker{Tools: tools}

	out, err := inv.Invoke(context.Background(), RemediationRequest{
		Option:   opt("pg.kill_session", "mutating", true),
		Approver: "policy:auto",
	})
	if !errors.Is(err, ErrRemediationFailed) {
		t.Fatalf("err = %v, want ErrRemediationFailed", err)
	}
	if out.Status != RemediationStatusFailed {
		t.Errorf("Status = %q, want failed", out.Status)
	}
	// Reporting a skip here would let the run look like it had nothing to
	// do, when in fact it had an action it could not perform.
	if out.Status == RemediationStatusSkipped {
		t.Error("an unregistered tool must be a failure, not a skip")
	}
	if tools.callCount != 0 {
		t.Errorf("nothing may be called for an unregistered action, got %v", tools.calls)
	}
}

// The invoker must not invent an argument. This is the test that would fail
// if someone "helpfully" defaulted a missing pid or role.
func TestRegistryInvoker_RefusesWhenARequiredArgumentIsMissing(t *testing.T) {
	tools := callerWith(ToolSpec{
		Name:         "pg.kill_session",
		Description:  "terminate a backend",
		RiskLevel:    "L3",
		RequiredArgs: []string{"pid"},
	})
	inv := RegistryInvoker{Tools: tools}

	out, err := inv.Invoke(context.Background(), RemediationRequest{
		// The option carries only a resource locator, as every option
		// produced by investigatorreal does.
		Option:   opt("pg.kill_session", "mutating", true),
		Approver: "policy:auto",
	})
	if !errors.Is(err, ErrUnresolvableArguments) {
		t.Fatalf("err = %v, want ErrUnresolvableArguments", err)
	}
	if !strings.Contains(out.Message, "pid") {
		t.Errorf("the message must name the missing argument: %q", out.Message)
	}
	if tools.callCount != 0 {
		t.Errorf("nothing may be called when an argument is missing, got %v", tools.calls)
	}
}

func TestRegistryInvoker_AnEmptyStringCountsAsMissing(t *testing.T) {
	tools := callerWith(ToolSpec{Name: "pg.connection_pause", RequiredArgs: []string{"role"}})
	inv := RegistryInvoker{
		Tools: tools,
		ArgResolver: ArgResolverFunc(func(_ context.Context, _ RemediationRequest, _ ToolSpec) (map[string]any, error) {
			return map[string]any{"role": "   "}, nil
		}),
	}
	_, err := inv.Invoke(context.Background(), RemediationRequest{
		Option:   opt("pg.connection_pause", "mutating", true),
		Approver: "policy:auto",
	})
	if !errors.Is(err, ErrUnresolvableArguments) {
		t.Errorf("a blank role must count as missing, got %v", err)
	}
}

// Zero is a value the caller chose, not an absent argument. A rule that
// treated it as missing would refuse deliberate calls.
func TestRegistryInvoker_ZeroIsAValueNotAnAbsence(t *testing.T) {
	tools := callerWith(ToolSpec{Name: "pg.vacuum_table", RequiredArgs: []string{"limit"}})
	inv := RegistryInvoker{
		Tools: tools,
		ArgResolver: ArgResolverFunc(func(_ context.Context, _ RemediationRequest, _ ToolSpec) (map[string]any, error) {
			return map[string]any{"limit": 0}, nil
		}),
	}
	if _, err := inv.Invoke(context.Background(), RemediationRequest{
		Option:   opt("pg.vacuum_table", "mutating", true),
		Approver: "policy:auto",
	}); err != nil {
		t.Errorf("an explicit zero must be dispatched, got %v", err)
	}
	if tools.callCount != 1 {
		t.Errorf("the tool should have run, got %v", tools.calls)
	}
}

func TestRegistryInvoker_ResolverSuppliesTheRequiredArgument(t *testing.T) {
	tools := callerWith(ToolSpec{Name: "pg.connection_pause", RequiredArgs: []string{"role"}})
	inv := RegistryInvoker{
		Tools: tools,
		ArgResolver: ArgResolverFunc(func(_ context.Context, req RemediationRequest, _ ToolSpec) (map[string]any, error) {
			// The role comes from upstream evidence, not from the
			// option — which is exactly why this is a seam.
			return map[string]any{"role": "reporting_ro"}, nil
		}),
	}
	if _, err := inv.Invoke(context.Background(), RemediationRequest{
		Option:   opt("pg.connection_pause", "mutating", true),
		Approver: "policy:auto",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, _ := tools.args["role"].(string); got != "reporting_ro" {
		t.Errorf("role = %q, want the resolver's value", got)
	}
}

func TestRegistryInvoker_AFailingToolIsAFailureNotASkip(t *testing.T) {
	tools := callerWith(ToolSpec{Name: "pg.vacuum_analyze"})
	tools.callErr = errors.New("permission denied for database app")
	inv := RegistryInvoker{Tools: tools}

	out, err := inv.Invoke(context.Background(), RemediationRequest{
		Option:   opt("pg.vacuum_analyze", "safe", true),
		Approver: "policy:auto",
	})
	if !errors.Is(err, ErrRemediationFailed) {
		t.Fatalf("err = %v, want ErrRemediationFailed", err)
	}
	if out.Status == RemediationStatusSkipped {
		t.Error("a tool that ran and failed is not a skip; downstream cannot tell them apart")
	}
	if !strings.Contains(out.Message, "permission denied") {
		t.Errorf("the server's own words must reach the operator: %q", out.Message)
	}
}

func TestRegistryInvoker_RefusesWithNoRegistryWired(t *testing.T) {
	_, err := RegistryInvoker{}.Invoke(context.Background(), RemediationRequest{
		Option: opt("pg.vacuum_analyze", "safe", true),
	})
	if !errors.Is(err, ErrRemediationFailed) {
		t.Errorf("err = %v, want ErrRemediationFailed", err)
	}
}

// ── the phase-level guarantee ───────────────────────────────────────────

// A run that dispatched nothing must not be able to reach the recovered
// phase. This is the single test that fails if the no-op executor is ever
// restored, and it is written against the Verifier rather than the Executor
// so it covers the decision, not just the call.
func TestApprovedPhase_AnAdvanceWithNoDispatchDoesNotVerify(t *testing.T) {
	w, err := NewApprovedPhaseWorker(
		&recordingPauseHook{pauseRequired: false},
		nil, nil,
		WithRemediationLoader(&fakeRemediationLoader{options: defaultRemediationOptions()}),
		WithRemediationInvoker(&fakeRemediationInvoker{err: errors.New("the database refused")}),
	)
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	res, err := w.Executor(context.Background(), plan)
	if err == nil {
		t.Fatal("a failed dispatch must not produce a clean Executor result")
	}
	// Even the failure result, if the orchestrator were to use it, must not
	// verify.
	verdict, verr := w.Verifier(context.Background(), res)
	if verr != nil {
		t.Fatalf("Verifier: %v", verr)
	}
	if verdict.OK {
		t.Error("Verifier.OK = true after a failed dispatch; the run would proceed to verify a change that never happened")
	}
}

func TestApprovedPhase_AFailedDispatchIsStillRecorded(t *testing.T) {
	inv := &fakeRemediationInvoker{err: errors.New("the database refused")}
	w, err := NewApprovedPhaseWorker(
		&recordingPauseHook{pauseRequired: false},
		nil, nil,
		WithRemediationLoader(&fakeRemediationLoader{options: defaultRemediationOptions()}),
		WithRemediationInvoker(inv),
	)
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	res, _ := w.Executor(context.Background(), plan)
	// The phase_failed event is one place a postmortem can find out; the
	// ToolReplay entry is the other. A gap in either reads downstream as
	// "the action was never attempted", which is a different statement.
	if len(res.ToolReplay) != 1 {
		t.Fatalf("ToolReplay = %+v, want the failed attempt recorded", res.ToolReplay)
	}
	if res.ToolReplay[0].Status != RemediationStatusFailed {
		t.Errorf("replay status = %q, want failed", res.ToolReplay[0].Status)
	}
	if res.ToolReplay[0].Name != "pg.vacuum_analyze" {
		t.Errorf("replay name = %q, want the action that was attempted", res.ToolReplay[0].Name)
	}
}

func TestApprovedPhase_WithoutALoaderTheRunStops(t *testing.T) {
	// No loader means the approval would be decided without reading what
	// it is approving.
	w, err := NewApprovedPhaseWorker(
		&recordingPauseHook{pauseRequired: false},
		nil, nil,
		WithRemediationInvoker(&fakeRemediationInvoker{}),
	)
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	if _, err := w.Executor(context.Background(), plan); err == nil {
		t.Fatal("the phase must stop when it cannot read the options it is approving")
	}
}

func TestApprovedPhase_ALoaderFailureStopsThePlan(t *testing.T) {
	// A loader error is not the same as "no options", and treating it as
	// such would advance a run whose approval was never informed.
	w, err := NewApprovedPhaseWorker(
		&recordingPauseHook{pauseRequired: false},
		nil, nil,
		WithRemediationLoader(&fakeRemediationLoader{err: errors.New("database unavailable")}),
		WithRemediationInvoker(&fakeRemediationInvoker{}),
	)
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	if _, err := w.Planner(context.Background(), newApprovedPlanInput()); err == nil {
		t.Fatal("a loader failure must fail the plan, not produce a plan with zero options")
	}
}

func TestApprovedPhase_APausedRunDispatchesNothing(t *testing.T) {
	inv := &fakeRemediationInvoker{}
	w, err := NewApprovedPhaseWorker(
		&recordingPauseHook{pauseRequired: true, token: "deadbeefcafebabe1234567890abcdef"},
		nil, nil,
		WithRemediationLoader(&fakeRemediationLoader{options: defaultRemediationOptions()}),
		WithRemediationInvoker(inv),
	)
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	plan, err := w.Planner(context.Background(), newApprovedPlanInput())
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	if _, err := w.Executor(context.Background(), plan); err != nil {
		t.Fatalf("Executor: %v", err)
	}
	// The pause is the approval. Dispatching anyway would run the action
	// twice — once now, once when the human approves.
	if len(inv.seen()) != 0 {
		t.Errorf("a paused run dispatched %d action(s); it must dispatch none", len(inv.seen()))
	}
}

func TestApprovedPhase_TheDispatchCarriesTheRunScopeAndTheApprover(t *testing.T) {
	inv := &fakeRemediationInvoker{}
	w, err := NewApprovedPhaseWorker(
		&recordingPauseHook{pauseRequired: false},
		nil, nil,
		WithRemediationLoader(&fakeRemediationLoader{options: defaultRemediationOptions()}),
		WithRemediationInvoker(inv),
	)
	if err != nil {
		t.Fatalf("NewApprovedPhaseWorker: %v", err)
	}
	in := newApprovedPlanInput()
	plan, err := w.Planner(context.Background(), in)
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	if _, err := w.Executor(context.Background(), plan); err != nil {
		t.Fatalf("Executor: %v", err)
	}
	seen := inv.seen()
	if len(seen) != 1 {
		t.Fatalf("dispatched %d actions, want 1", len(seen))
	}
	// The Executor never sees PlanInput, so the Planner has to carry the
	// run's scope across; an invoker that received an empty TenantID would
	// be one that cannot enforce tenant isolation at the call site.
	if seen[0].TenantID != in.TenantID || seen[0].IncidentID != in.IncidentID {
		t.Errorf("scope = %s/%s, want %s/%s", seen[0].TenantID, seen[0].IncidentID, in.TenantID, in.IncidentID)
	}
	if seen[0].Approver != approvedAutoApprover {
		t.Errorf("approver = %q, want %q", seen[0].Approver, approvedAutoApprover)
	}
}

// ── the loaders ─────────────────────────────────────────────────────────

func TestContractRootCauseLoader_ReadsTheInvestigatedContract(t *testing.T) {
	repo := &fakeContractRepo{contract: &loopmodel.Contract{
		ID:      7,
		Payload: `{"schema_version":"v1","remediation_options":[{"action":"pg.vacuum_analyze","target":"pg:alert-1","risk":"safe","auto_approve":true}]}`,
	}}
	rc, err := ContractRootCauseLoader{Contracts: repo}.LoadRootCause(context.Background(), "t1", "inc-1")
	if err != nil {
		t.Fatalf("LoadRootCause: %v", err)
	}
	if rc == nil || len(rc.RemediationOptions) != 1 {
		t.Fatalf("RootCauseJSON = %+v, want one remediation option", rc)
	}
	if repo.readPhase != PhaseInvestigated {
		t.Errorf("read phase = %q, want %q — the approval must read the same row the console showed", repo.readPhase, PhaseInvestigated)
	}
	if rc.RemediationOptions[0].Action != "pg.vacuum_analyze" {
		t.Errorf("action = %q", rc.RemediationOptions[0].Action)
	}
}

func TestContractRootCauseLoader_NoContractIsNotAnError(t *testing.T) {
	// (nil, nil) means "the run never reached investigated". Returning an
	// error would make a not-yet-happened phase look like a storage failure.
	rc, err := ContractRootCauseLoader{Contracts: &fakeContractRepo{}}.LoadRootCause(context.Background(), "t1", "inc-1")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rc != nil {
		t.Errorf("RootCauseJSON = %+v, want nil", rc)
	}
}

func TestRootCauseRemediationLoader_PassesTheNilCaseThrough(t *testing.T) {
	// nil and an empty slice mean different things to the executor: the
	// first is "nothing was investigated", the second would be
	// "investigated and proposed nothing".
	opts, err := RootCauseRemediationLoader{Loader: noRootCauseLoader{}}.LoadRemediationOptions(context.Background(), "t1", "inc-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if opts != nil {
		t.Errorf("options = %+v, want nil", opts)
	}
}

func TestRootCauseRemediationLoader_RequiresARepository(t *testing.T) {
	opts, err := RootCauseRemediationLoader{}.LoadRemediationOptions(context.Background(), "t1", "inc-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if opts != nil {
		t.Errorf("options = %+v, want nil when no loader is configured", opts)
	}
}

// The end-to-end guarantee: a dispatch that fails is visible in the event
// log, by tool name, for whoever reads the incident afterwards.
func TestDryRun_AFailedDispatchReachesTheEventLog(t *testing.T) {
	cs := newPgLongRunningTxCase()
	workers, err := DefaultPhaseWorkerFactory(PhaseWorkerDeps{
		VerifyCaller:      &recordingVerifyCaller{},
		StateStore:        newInMemoryStateStore(),
		ApprovedRefLoader: &pgApprovedLoader{},
		RemediationLoader: RootCauseRemediationLoader{
			Loader: &dryRunRootCauseLoader{options: []RemediationOption{{
				Action:      "pg.vacuum_analyze",
				Target:      "postgres://prod-cluster-1",
				Risk:        "safe",
				AutoApprove: true,
			}}},
		},
		RemediationInvoker: &failingRemediationInvoker{},
		FlowRunner:         NoopFlowRunner{},
		PauseHook:          NoopPauseHook{},
		Logger:             slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		Clock:              func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("DefaultPhaseWorkerFactory: %v", err)
	}
	eventRepo := NewInMemoryEventRepo()
	o, err := NewOrchestrator(OrchestratorDeps{
		Locker:                 stubLocker{},
		AdvisoryLockTimeoutSec: 5,
		EventRepo:              eventRepo,
		ContractRepo:           NewInMemoryContractRepo(),
		WorkerRegistry:         NewWorkerRegistry(workers),
		Logger:                 slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	res, err := o.Run(context.Background(), RunOptions{
		IncidentID: cs.IncidentID,
		TenantID:   cs.TenantID,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalPhase != PhaseFailed {
		t.Fatalf("FinalPhase = %s, want failed; a run whose remediation did not act must not reach postmortem", res.FinalPhase)
	}

	events, err := eventRepo.ReadEvents(context.Background(), cs.TenantID, cs.IncidentID)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.EventType != loopmodel.EventPhaseFailed {
			continue
		}
		if payloadHasToolReplay(t, e, "pg.vacuum_analyze") {
			found = true
		}
	}
	if !found {
		t.Errorf("no phase_failed event recorded the attempted dispatch; the incident would read as 'never attempted'")
	}
}

// payloadHasToolReplay reads the recorded action out of an event's JSON
// payload. The caller decides which event types are relevant: a failed
// dispatch is recorded on phase_failed, a successful one on
// phase_contract_written, and both carry the same "tool_replay" shape.
//
// The event log stores the payload as a string, so this goes through JSON —
// which is also the right thing to assert against: whatever the in-memory
// event carried, what a postmortem reads is what was serialised.
func payloadHasToolReplay(t *testing.T, e loopmodel.Event, want string) bool {
	t.Helper()
	var payload struct {
		ToolReplay []ToolReplayEntry `json:"tool_replay"`
	}
	if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
		t.Fatalf("event payload is not valid JSON (%v): %s", err, e.Payload)
	}
	for _, r := range payload.ToolReplay {
		if r.Name == want {
			return true
		}
	}
	return false
}

// payloadReplayCarriesArgv asserts the recorded replay names the literal
// vector that ran. The crystalliser promotes this vector into a declaration,
// so an event that recorded only the argument bag would leave it with
// nothing to re-run.
func payloadReplayCarriesArgv(t *testing.T, e loopmodel.Event, want ...string) bool {
	t.Helper()
	var payload struct {
		ToolReplay []ToolReplayEntry `json:"tool_replay"`
	}
	if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
		t.Fatalf("event payload is not valid JSON (%v): %s", err, e.Payload)
	}
	flat := strings.Join(want, "\x00")
	for _, r := range payload.ToolReplay {
		if strings.Join(r.Argv, "\x00") == flat {
			return true
		}
	}
	return false
}

// failingRemediationInvoker always fails, standing in for a tool the
// database refused.
type failingRemediationInvoker struct{}

func (failingRemediationInvoker) Invoke(_ context.Context, req RemediationRequest) (RemediationOutcome, error) {
	return RemediationOutcome{
		Status:  RemediationStatusFailed,
		Message: "dry run: " + req.Option.Action + " refused by the server",
		Args:    map[string]any{"target": req.Option.Target},
	}, errors.New("dry run: the server refused the action")
}

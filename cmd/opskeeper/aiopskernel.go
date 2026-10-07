package main

import (
	"context"
	"fmt"
	aiopstools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/configchange"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/recovery"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	managerbizaiops "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/agentkernel"
	aiopstoolsbase "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	managersvcaiops "github.com/vincent-wuhan/opskeeper/core/manager/service/aiops"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigagent"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigcoding"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// aiopskernel.go assembles the PiG-backed chat kernel
// (OPSKEEPER_AGENT_KERNEL=pig).
//
// The kernel is the end state of the 2.0 plan: the same chatruntime.Runtime
// drives the same tools and emits the same SSE frames, but the loop under it
// is core/pig/pigagent instead of an eino ReAct graph. Everything host-shaped
// this needs — the tool bag, the ledger, the approval gate, the transcript
// write path — arrives through agentkernel, which is why this file is wiring
// and no policy.

// selfSettledToolNames lists the mutating tools whose approval question is
// already answered without the kernel.
//
// The kernel can only see a tool's class. It cannot see that cloud_bash
// blocks on its own propose-and-await card, that apply_config_change requires
// the user's confirmation of an exact draft hash, or that the coordination
// primitives have never been approval-gated at all. Declaring them here is
// what stops the kernel from asking a second time for one call — a duplicate
// card is read by an operator as a console fault, and it arrives after they
// believe the decision is behind them.
//
// The list is deliberately next to the constructors that decide it, and
// buildAIOpsRuntime refuses to boot when a mutating tool in the bag is in
// neither this list nor a later Add (see undeadclaredMutatingTools). A new
// mutating tool therefore fails loudly at boot rather than quietly raising a
// second approval card in production.
func selfSettledToolNames() []string {
	return []string{
		// Propose-and-await inside InvokableRun, card rendered by the tool.
		aiopstools.ToolNameCloudBash,
		aiopstools.ToolNameInstallSkill,
		// Review-gated by the decorator chain (review_gate spawns the
		// reviewer worker for mutating tools without a deterministic policy).
		aiopstools.ToolNameRestartService,
		// Requires an already-approved proposal id / a confirmed draft hash.
		recovery.ToolNameRecoveryExecute,
		configchange.ToolNameApplyConfigChange,
		// Coordination and output primitives: they change no infrastructure.
		aiopstools.AgentToolName,
		aiopstools.SendMessageToolName,
		aiopstools.TaskStopToolName,
		aiopstools.ToolNameServePage,
		aiopstools.ToolNameSendIMMessage,
	}
}

// agentKernelInput is what the assembly has to supply.
//
// It is a struct rather than a parameter list because every field is a host
// decision that must not be defaulted: a zero field here means a deployment
// forgot something, and the constructors below report that rather than
// substituting a working-looking value.
type agentKernelInput struct {
	// Models resolves the per-turn model. Required: it is the same settings-
	// backed registry the LLM client uses, so a key rotation lands on the
	// next turn.
	Models pigagent.ModelResolver
	// Gate decides whether a mutating call may run. Required.
	Gate ports.ApprovalGate
	// Sessions is the chat transcript store. Required.
	Sessions managerbizaiops.SessionRepo
	// Audit receives the gate's decisions. Optional; nil records nothing.
	Audit *audit.Usecase
	// Budget is the per-day token cap. Optional; nil runs unbudgeted, which
	// is what "no cap configured" means.
	Budget agentkernel.TokenBudget
	// Model is stamped onto assistant rows so the console can attribute an
	// answer. Optional.
	Model string
	// Logger and Registerer receive best-effort persistence failure detail
	// and its counters. Optional.
	Logger     *slog.Logger
	Registerer prometheus.Registerer
	// MaxIterations caps tool rounds per turn when a request does not.
	MaxIterations int
	// Driver says which PiG driver to build. Required and not defaulted:
	// choosing a loop is an operator decision, and a zero value that
	// silently picked one would make the env var optional in the only place
	// it matters.
	Driver managersvcaiops.Kernel
	// PiGRuntime is the process-wide PiG container. Required when Driver is
	// KernelPigSDK and ignored otherwise.
	//
	// It is the same runtime the rest of the process already holds, not a
	// second one. Two runtimes would mean two extension runners, and a
	// package loaded through one would be invisible to a turn driven by the
	// other — which would look exactly like a plugin that loads and does
	// nothing.
	PiGRuntime *pigcoding.Runtime
	// AfterAssistantRow receives the committed assistant row id. Required in
	// practice: without it the console bubble is keyed on a synthetic id and
	// is replaced rather than updated on the next history load.
	AfterAssistantRow func(sessionID, messageID string)
}

// kernelHost binds the services a turn runs against.
//
// It is a named function so the binding is assertable. The binding is where
// the console's daily cap actually exists or does not, and a mistake here —
// one adapter for the checker and another for the recorder, or a Spender left
// nil — is invisible at every layer below it: turns run, turns stop when the
// cap says so, and nothing reports that the cap is not being fed.
func kernelHost(in agentKernelInput, persister *agentkernel.Persister) agentkernel.Host {
	var budget ports.BudgetChecker
	var spender ports.TokenRecorder
	if adapter := agentkernel.NewBudget(in.Budget, nil); adapter != nil {
		budget, spender = adapter, adapter
	}
	host := agentkernel.Host{
		// The bag is resolved per turn by chatruntime, which stamps it on
		// ctx after every filter has settled it (role, persona, write gate,
		// governance). Resolving it here instead would give every turn the
		// first caller's view.
		ToolsFor: agentkernel.TurnToolsFromContext,
		Audit:    agentkernel.NewAuditLedger(ledgerWriter(in.Audit), chainVerifier(in.Audit)),
		Gate:     in.Gate,
		// One adapter for both halves. Two would be two buckets: a turn
		// could be checked against one ledger and charged to another, and
		// the cap would then be unenforceable in a way no test of either
		// half alone would catch.
		//
		// Both halves, or neither. NewBudget returns a nil *Budget when no
		// cap is configured, and assigning that straight into an interface
		// field yields an interface holding a nil pointer — which passes
		// every `!= nil` check downstream and only reveals itself when
		// something calls through it. Leaving both unset is the honest
		// "no ceiling" answer.
		Budget:  budget,
		Spender: spender,
		// The persister is also the tool-call recorder: a call row is
		// unreachable without the assistant row id that only the transcript
		// write observes.
		Recorder: persister,
	}
	return host
}

// newAgentKernel builds the kernel and the host binding it runs against.
//
// It returns the Agent port rather than a concrete kernel because there are
// two, and a caller that named one would have to branch again the next time
// a third appears. The host binding below is identical for both — the same
// tool bag resolution, the same ledger, the same gate, the same budget, the
// same persister — because the only thing the driver changes is the loop
// underneath, and everything that makes a turn a *OpsKeeper* turn happens
// outside it.
func newAgentKernel(in agentKernelInput) (pigagent.Agent, error) {
	if in.Models == nil {
		return nil, fmt.Errorf("aiops kernel: no model resolver")
	}
	if in.Gate == nil {
		return nil, fmt.Errorf("aiops kernel: no approval gate")
	}
	persister, err := agentkernel.NewPersister(agentkernel.PersistDeps{
		Repo:              in.Sessions,
		Model:             in.Model,
		Logger:            in.Logger,
		Registerer:        in.Registerer,
		AfterAssistantRow: in.AfterAssistantRow,
	})
	if err != nil {
		return nil, err
	}
	host := kernelHost(in, persister)
	provider, err := host.Provider()
	if err != nil {
		return nil, err
	}
	maxTurns := in.MaxIterations
	if maxTurns <= 0 {
		maxTurns = pigagent.DefaultMaxIterations
	}
	// The bare loop and the SDK driver differ in exactly one respect an
	// operator can act on, and it is where the turn ends. The bare loop
	// takes a hard cap as an agent option and PiG enforces it by returning
	// an error, so a turn that ran long is reported to the console as a
	// failure. The Session has no such field, so the cap is a
	// BeforeToolCall budget: the model is refused its next tool, told why,
	// and gets to answer from the evidence it already has — and the kernel
	// reports max_iterations rather than letting it read as end_turn. Same
	// ceiling, same operator-facing fact, and the second one is the better
	// of the two. The differential golden in core/pig/pigagent holds every
	// other frame to byte equality.
	sessionTimeout := 30 * time.Minute
	if in.Driver.UsesPiGSession() {
		if in.PiGRuntime == nil {
			// Failing here rather than at the first turn: a nil runtime
			// would otherwise produce an empty turn with no error, which is
			// the one failure mode an operator cannot diagnose from a log.
			return nil, fmt.Errorf("aiops kernel: driver %q needs the process PiG runtime", in.Driver)
		}
		return pigagent.NewSessionKernel(pigagent.SessionKernelOptions{
			Runtime:        in.PiGRuntime,
			Models:         in.Models,
			Deps:           provider,
			Persist:        persister,
			MaxIterations:  maxTurns,
			SessionTimeout: sessionTimeout,
			// PiG's working directory becomes the session's. The runtime is
			// already anchored at a state directory the deployment owns (see
			// pigRuntimeOptions), so a tool with a relative path resolves
			// inside OpsKeeper's own tree rather than in whatever directory
			// the process happened to be started from.
		})
	}
	return pigagent.NewKernel(pigagent.KernelOptions{
		Models:         in.Models,
		Deps:           provider,
		Persist:        persister,
		MaxIterations:  maxTurns,
		SessionTimeout: sessionTimeout,
	})
}

// ledgerWriter narrows the audit usecase to the ledger's one-method seam. It
// returns a nil interface for a nil usecase so the ledger adapter reports
// "not configured" rather than a writer that silently drops rows.
func ledgerWriter(uc *audit.Usecase) agentkernel.LedgerWriter {
	if uc == nil {
		return nil
	}
	return uc
}

// checkMutatingToolsDeclared refuses to boot when a mutating tool in the bag
// has no declared approval owner.
//
// Failing the boot is the point. The alternative — starting with the kernel
// gate as the only approval for a tool that also asks for its own — shows up
// as two cards for one call, and the operator has no way to tell that the
// second one is a wiring bug rather than the product working as designed.
func checkMutatingToolsDeclared(ctx context.Context, tools []aiopstoolsbase.BaseTool, gate *agentkernel.DeferredGate) error {
	missing, err := agentkernel.UndeclaredMutatingTools(ctx, tools, gate.Declares)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("aiops kernel: mutating tools with no declared approval owner: %v "+
		"(declare them in selfSettledToolNames, or Add them where they are constructed)", missing)
}

// chainVerifier exposes the host usecase's chain verification to the
// kernel's ledger port. The writer and the verifier are the same object
// because the chain is host-wide: the kernel's entries sit inside a
// chain that also covers the middleware's, and a binding that verified
// only its own rows would report an intact chain while a hole sat
// between two of them.
func chainVerifier(a *audit.Usecase) agentkernel.LedgerVerifier { return a }

// Package loop — remediation.go
//
// The seam that turns a proposed fix into a performed one.
//
// Until this file existed, the approved phase's Executor evaluated the HITL
// pause hook and then, on the "no human needed" branch, returned
// ApprovedExecAdvance having performed nothing. The run continued to the
// recovered phase, which compared post-incident metrics against a baseline
// that nothing had changed, concluded the remediation had failed, and rolled
// back — writing a postmortem that attributed a fix to a change nobody made.
// Every remedy named in a RootCauseJSON reached the console as a display
// string, because the string was the only thing anything ever did with it.
//
// So the dispatch lives here, on purpose, in one place with one policy:
// what may run, in what order, and what it is recorded as when it fails.
//
// Three rules govern the design, and each of them exists because the obvious
// alternative produces a worse failure:
//
//  1. Only a pre-approved action may run unattended. A pause hook that
//     returns "no human required" is a statement about whether a person is
//     needed — it is not an approval. Treating it as one would let any
//     non-auto-approved action run because the default hook is a no-op, and
//     the default hook is a no-op everywhere this ships.
//  2. Selection is deterministic and documented. The loop applies the
//     least-privileged remedy policy has already blessed, never a
//     "best" guess, and it never reorders the investigator's list.
//  3. A failure is a failure. Nothing here converts an error into a skip,
//     because a skip is indistinguishable downstream from a success that
//     happened to change nothing.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Remediation invocation status values, mirrored into
// ToolReplayEntry.Status so the postmortem and the Harness loop rubric read
// one vocabulary.
const (
	// RemediationStatusSuccess means the tool ran and reported acting.
	RemediationStatusSuccess = "success"
	// RemediationStatusFailed means the tool ran and reported not acting, or
	// errored. Both are failures; they are distinguished by the message.
	RemediationStatusFailed = "failed"
	// RemediationStatusSkipped means no action was dispatched at all. It is
	// only ever produced for a deliberate, recorded reason — never as a
	// fallback for a failure.
	RemediationStatusSkipped = "skipped"
)

// Errors returned by this file. They are distinct values rather than strings
// because every caller that swallows one of them is making a decision that
// belongs in the loop_event_log, not in a log line.
var (
	// ErrNoRemediationSelected means the upstream RootCauseJSON carried no
	// option that policy permits running unattended. The contract validator
	// already requires at least one option, so reaching this means the loop
	// reached the approved phase with options that are all gated — a
	// configuration where the run cannot proceed and a human must look.
	ErrNoRemediationSelected = errors.New("loop: no remediation option is eligible to run")

	// ErrRemediationFailed means the action was dispatched and did not
	// succeed. It is deliberately not recoverable by retrying inside the
	// phase: a second attempt at a production write that just failed is a
	// decision the recovered phase's rollback logic should make, with the
	// failure in evidence.
	ErrRemediationFailed = errors.New("loop: remediation action did not succeed")
)

// RemediationOptionLoader reads the candidate fixes proposed upstream.
//
// It is separate from the existing RootCauseLoader because the approved
// phase needs exactly one field of one contract. Narrowing the interface to
// the field it uses means a test can supply three options without
// constructing a valid RootCauseJSON around them, and it means the executor
// cannot reach evidence, confidence or time-window data it has no business
// reading at approval time.
type RemediationOptionLoader interface {
	// LoadRemediationOptions returns the candidate fixes for an incident.
	//
	// Contract:
	//   - (nil, nil) when the incident has no RootCauseJSON yet, or the
	//     run reached approved without an investigated phase.
	//   - tenantID is required; a cross-tenant incident id returns
	//     (nil, nil) rather than another tenant's options.
	LoadRemediationOptions(ctx context.Context, tenantID, incidentID string) ([]RemediationOption, error)
}

// RemediationRequest is one approved action, resolved to the point where it
// can be run.
type RemediationRequest struct {
	// IncidentID and TenantID scope the call. Both are required.
	IncidentID string
	TenantID   string

	// Option is the selected action, verbatim from the RootCauseJSON.
	Option RemediationOption

	// Approver names who authorised this. It is "policy:auto" for an
	// action that carried auto_approve=true, and the approving human's
	// id otherwise. The value is recorded in the audit chain, so an
	// invoker must not synthesise it.
	Approver string

	// Reason is the approval rationale, carried for the audit record.
	Reason string
}

// RemediationOutcome is what actually happened.
type RemediationOutcome struct {
	// Status is one of the RemediationStatus* constants.
	Status string

	// Tool is the registered tool name the action dispatched through, when
	// an invoker resolved it. It is empty for an outcome that never reached
	// a tool (an unresolved argument, a missing registry).
	//
	// It is carried separately from the RemediationOption.Action because
	// the two are not always the same string: the loop proposes an action
	// name and the invoker looks it up, and a lookup that found a tool
	// under a different registered name is a fact the crystalliser needs
	// when it asks the tool registry what class that tool declares.
	Tool string

	// Message is the operator-facing account of the attempt. It is
	// persisted into the postmortem, so it must say what was attempted
	// and what the server answered.
	Message string

	// Impacted is the count of resources the action acted on, when the
	// tool reports one.
	Impacted int

	// Args are the resolved tool arguments. They are recorded in the
	// ToolReplay so a postmortem can show what was sent, not only what
	// was intended — the two differ whenever the investigator's option
	// named a target and the invoker had to turn it into arguments.
	Args map[string]any

	// Result is the tool's raw response, for the replay log.
	Result any

	// Argv is the literal argument vector the dispatched tool handed to the
	// machine, when it executed one. It is empty for tools that reach the
	// change through an API rather than an exec (a k8s eviction, a SQL
	// statement) and for reads. The crystalliser re-runs this vector
	// verbatim, so it is copied from what ran and never derived here.
	Argv []string
}

// RemediationInvoker performs one approved action.
//
// The interface is one method because every policy question — what may run,
// in what order, whether a failure is fatal — is answered here rather than
// by each implementation. An invoker's only job is to run something and
// report what happened.
type RemediationInvoker interface {
	Invoke(ctx context.Context, req RemediationRequest) (RemediationOutcome, error)
}

// UnavailableInvoker is the default when no invoker has been wired.
//
// It exists so the failure is a named, greppable error at the point of use
// rather than a nil dereference, and — more importantly — so the default is
// a refusal. Before this type, the default was a silent success: the phase
// advanced, the postmortem claimed a fix, and nothing had run. A
// remediation platform that says "done" about a change it did not make is
// worse than one that is obviously not finished.
type UnavailableInvoker struct{}

func (UnavailableInvoker) Invoke(_ context.Context, req RemediationRequest) (RemediationOutcome, error) {
	return RemediationOutcome{
		Status:  RemediationStatusFailed,
		Message: "no remediation invoker is wired into this build",
	}, fmt.Errorf("loop: cannot perform %q: %w", req.Option.Action, ErrRemediationFailed)
}

// riskRank orders the risk tiers for selection. Lower is less invasive.
//
// The ordering is total and explicit because the alternative — comparing the
// raw strings — happens to work only for the alphabetical order of these
// three words, which is an accident and not a policy. "dangerous" sorts
// before "mutating" before "safe", so a string comparison would select the
// most dangerous option every time.
func riskRank(risk string) int {
	switch strings.ToLower(strings.TrimSpace(risk)) {
	case "safe":
		return 0
	case "mutating":
		return 1
	case "dangerous":
		return 2
	default:
		// An unrecognised risk is not silently treated as safe. Ranking
		// it last means it is only ever chosen when nothing else is
		// available, and a caller that wants strictness can reject it
		// outright via RequireKnownRisk.
		return 3
	}
}

// KnownRisks is the closed set the selection policy understands.
var KnownRisks = []string{"safe", "mutating", "dangerous"}

// SelectRemediation picks the action to run from the proposed options.
//
// The policy, in order:
//
//  1. An option with auto_approve=true is eligible. Policy has already
//     decided it needs no human, so it is the one the loop may run.
//  2. Among eligible options, the lowest risk tier wins.
//  3. Ties break on the investigator's original order, which is
//     preserved. The investigator ranked these against evidence the
//     executor cannot see; re-sorting by name would discard that.
//
// It returns ErrNoRemediationSelected when nothing is eligible. That is not
// an empty-list case: a RootCauseJSON with options but no pre-approved one
// is a run that must stop for a human, and stopping is the correct answer,
// not a failure to find something to do.
//
// Why the invoker's approver, not this function, decides eligibility: an
// option that is not auto-approved becomes eligible the moment a human
// approves it, and that approval arrives on the resume path with a pause
// token, not here.
func SelectRemediation(options []RemediationOption) (RemediationOption, error) {
	eligible := make([]RemediationOption, 0, len(options))
	for _, opt := range options {
		if !opt.AutoApprove {
			continue
		}
		if strings.TrimSpace(opt.Action) == "" {
			continue
		}
		eligible = append(eligible, opt)
	}
	if len(eligible) == 0 {
		return RemediationOption{}, fmt.Errorf("%w (%d proposed, none pre-approved)", ErrNoRemediationSelected, len(options))
	}
	// A stable sort preserves the investigator's ordering within a tier,
	// which is the tie-break rule stated above.
	sort.SliceStable(eligible, func(i, j int) bool {
		return riskRank(eligible[i].Risk) < riskRank(eligible[j].Risk)
	})
	return eligible[0], nil
}

// RecordRemediation turns an outcome into the phase's persistent trace.
//
// The ToolReplayEntry is the part that matters long-term: the postmortem
// phase and the Harness loop rubric both read it, so this is where a run's
// claim to have fixed something is either substantiated or contradicted.
// An entry is written for every outcome, including failures — a gap in the
// replay reads downstream as "no action was attempted", which is a different
// and less truthful statement than "the action was attempted and failed".
func RecordRemediation(outcome RemediationOutcome, start time.Time, now func() time.Time) (ToolReplayEntry, SideEffect) {
	args := "{}"
	if len(outcome.Args) > 0 {
		if b, err := json.Marshal(outcome.Args); err == nil {
			args = string(b)
		}
	}
	result := "{}"
	if outcome.Result != nil {
		if b, err := json.Marshal(outcome.Result); err == nil {
			result = string(b)
		}
	}
	latency := 0
	if now != nil {
		latency = int(now().Sub(start) / time.Millisecond)
	}
	stamp := start
	if now != nil {
		stamp = now()
	}
	return ToolReplayEntry{
		Name:           "",
		RegisteredTool: outcome.Tool,
		ArgsJSON:       args,
		ResultJSON:     result,
		Argv:           append([]string(nil), outcome.Argv...),
		Status:         outcome.Status,
		LatencyMs:      latency,
		Timestamp:      stamp,
	}, SideEffect{
		Kind: "mutation",
		Detail: map[string]any{
			"status":   outcome.Status,
			"message":  outcome.Message,
			"impacted": outcome.Impacted,
		},
	}
}

// compile-time assertion that the default invoker is usable in place of any
// other, so a wiring change cannot surprise the factory.
var _ RemediationInvoker = UnavailableInvoker{}

// RootCauseRemediationLoader adapts the existing RootCauseLoader to the
// narrower RemediationOptionLoader.
//
// It is the production wiring: the approved phase needs one field of one
// contract, and the repository already knows how to read that contract.
// Re-declaring a second query for the same row would give the two readers
// independent notions of "the most recent RootCauseJSON", and the approval
// would then be decided against a different version of the proposal than
// the one shown in the console.
//
// The nil case is passed through rather than turned into an empty slice.
// (nil, nil) is the established "no RootCauseJSON recorded yet" signal, and
// an empty non-nil slice would make "not investigated yet" and "investigated
// and proposed nothing" the same value — two different situations that need
// two different operator responses.
type RootCauseRemediationLoader struct {
	// Loader is the contract reader. Required; a nil Loader makes every
	// call return (nil, nil), which the executor reports as "no options
	// were proposed" rather than as a wiring fault.
	Loader RootCauseLoader
}

func (r RootCauseRemediationLoader) LoadRemediationOptions(ctx context.Context, tenantID, incidentID string) ([]RemediationOption, error) {
	if r.Loader == nil {
		return nil, nil
	}
	rc, err := r.Loader.LoadRootCause(ctx, tenantID, incidentID)
	if err != nil {
		return nil, err
	}
	if rc == nil {
		return nil, nil
	}
	return rc.RemediationOptions, nil
}

var _ RemediationOptionLoader = RootCauseRemediationLoader{}

// ContractRootCauseLoader reads the RootCauseJSON out of the loop contract
// table.
//
// It is the production RootCauseLoader, and it reads the same row the
// postmortem and the critique read. That sharing is deliberate: the approval
// phase must decide on the proposal the console showed, and a second query
// written to find "the remediation options" would be free to disagree with
// it about which revision is current — and would disagree silently, on
// exactly the incident where the investigator revised its proposal between
// the critique and the approval.
//
// The tenant id is required and passed through to the query rather than
// being trusted from the caller: a cross-tenant read here would hand one
// tenant's approved write actions to another tenant's loop.
type ContractRootCauseLoader struct {
	// Contracts is the contract repository. Required.
	Contracts ContractRepo
}

func (c ContractRootCauseLoader) LoadRootCause(ctx context.Context, tenantID, incidentID string) (*RootCauseJSON, error) {
	if c.Contracts == nil {
		return nil, errors.New("loop: contract repository is required to load the root cause")
	}
	contract, err := c.Contracts.ReadContract(ctx, tenantID, incidentID, PhaseInvestigated, "RootCauseJSON")
	if err != nil {
		return nil, fmt.Errorf("loop: read RootCauseJSON contract: %w", err)
	}
	if contract == nil {
		// (nil, nil) is the established "not investigated yet" signal,
		// and the remediation loader passes it through. Returning an
		// error here would make a run that never reached the
		// investigated phase look like a storage failure.
		return nil, nil
	}
	var rc RootCauseJSON
	if err := json.Unmarshal([]byte(contract.Payload), &rc); err != nil {
		return nil, fmt.Errorf("loop: decode RootCauseJSON contract %d: %w", contract.ID, err)
	}
	return &rc, nil
}

var (
	_ RootCauseLoader         = ContractRootCauseLoader{}
	_ RemediationOptionLoader = RootCauseRemediationLoader{}
)

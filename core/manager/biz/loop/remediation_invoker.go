// Package loop — remediation_invoker.go
//
// The concrete RemediationInvoker: it turns an approved RemediationOption
// into a call on a registered tool.
//
// The rule that shapes this file is that the invoker never invents an
// argument. The loop's RemediationOption carries an action name and a
// resource locator ("pg:alert-17"), while the tools take specific arguments
// — a pid, a role, a table. Bridging the two means resolving the locator
// into arguments, and the tempting shortcut is to fill anything missing with
// a plausible default. Doing that on a write is how "terminate the long
// transaction" becomes "terminate whichever backend the planner reached
// first", so a missing required argument is a refusal instead.
package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// ToolCaller is the narrow port a remediation dispatch needs.
//
// It is ports.ToolCaller, aliased rather than redeclared: the loop consumes
// the contract and the middleware registry implements it, and the two are in
// different modules. A local copy of the interface would compile just as
// well right up until one side changed, and then it would fail at the
// wiring — which is the worst place for a shape mismatch to surface, because
// by then the two halves are both committed.
type ToolCaller = ports.ToolCaller

// ToolSpec is what LookupTool returns.
type ToolSpec = ports.ToolSpec

// ErrUnresolvableArguments is returned when a dispatch cannot be completed
// because a required argument has no value.
//
// It is distinct from ErrRemediationFailed because it is never a runtime
// problem — the tool may be perfectly healthy. It is a proposal that does
// not yet contain enough information to act on, which is a finding about
// the investigation rather than about the database.
var ErrUnresolvableArguments = errors.New("loop: cannot resolve the required arguments for this action")

// RegistryInvoker dispatches to a tool registry.
type RegistryInvoker struct {
	// Tools is the registry to dispatch through. Required.
	Tools ToolCaller

	// ArgResolver supplies arguments an option does not carry.
	//
	// nil → no resolution beyond passing the option's own target through.
	// That is sufficient for actions whose arguments are all optional
	// (pg.vacuum_analyze, pg.terminate_long_tx) and correctly refuses the
	// ones that are not.
	ArgResolver ArgResolver
}

// ArgResolver turns an approved option plus its target into tool arguments.
//
// It exists as a seam rather than a map because the inputs are not all
// available in the option: resolving pg.connection_pause needs the role,
// which lives in the upstream evidence, not in the remediation option. A
// resolver is where that lookup is written, per action, and where it can be
// tested without a database.
type ArgResolver interface {
	Resolve(ctx context.Context, req RemediationRequest, spec ToolSpec) (map[string]any, error)
}

// ArgResolverFunc adapts a function to ArgResolver.
type ArgResolverFunc func(ctx context.Context, req RemediationRequest, spec ToolSpec) (map[string]any, error)

func (f ArgResolverFunc) Resolve(ctx context.Context, req RemediationRequest, spec ToolSpec) (map[string]any, error) {
	return f(ctx, req, spec)
}

func (i RegistryInvoker) Invoke(ctx context.Context, req RemediationRequest) (RemediationOutcome, error) {
	action := strings.TrimSpace(req.Option.Action)
	if action == "" {
		return RemediationOutcome{
			Status:  RemediationStatusFailed,
			Message: "the remediation option carries no action name",
		}, fmt.Errorf("%w: empty action", ErrUnresolvableArguments)
	}
	if i.Tools == nil {
		return RemediationOutcome{
			Status:  RemediationStatusFailed,
			Message: "no tool registry is wired into the remediation invoker",
		}, fmt.Errorf("%w: %s", ErrRemediationFailed, action)
	}

	spec, ok := i.Tools.LookupTool(action)
	if !ok {
		// Not registered means not runnable, full stop. Falling back to
		// some other tool, or reporting a skip, would turn a vocabulary
		// mismatch into a run that believes it did something.
		return RemediationOutcome{
			Status:  RemediationStatusFailed,
			Message: fmt.Sprintf("no tool is registered as %q, so this action cannot be performed", action),
		}, fmt.Errorf("%w: %s is not a registered tool", ErrRemediationFailed, action)
	}

	args, err := i.resolveArgs(ctx, req, spec)
	if err != nil {
		return RemediationOutcome{
			Status:  RemediationStatusFailed,
			Message: err.Error(),
			Args:    args,
		}, err
	}

	result, err := i.Tools.CallTool(ctx, action, args)
	if err != nil {
		return RemediationOutcome{
			Status:  RemediationStatusFailed,
			Message: fmt.Sprintf("%s failed: %s", action, err.Error()),
			Args:    args,
			Result:  result,
		}, fmt.Errorf("%w: %s: %s", ErrRemediationFailed, action, err.Error())
	}
	return RemediationOutcome{
		Status:  RemediationStatusSuccess,
		Message: fmt.Sprintf("%s completed", action),
		Tool:    spec.Name,
		Args:    args,
		Result:  result,
		Argv:    argvFromResult(result),
	}, nil
}

// argvFromResult lifts the literal vector the adapter executed out of the
// tool's result bag, when it put one there.
//
// This is the one field the crystalliser cannot derive and must not invent:
// a runbook re-runs the vector byte for byte. The adapter is the only place
// that knows it (it built the exec call), so it hands it back as
// "argv": []string, and this reads it without coercing — a missing key or a
// non-list yields nil, which makes TrialOf refuse to crystallise the run
// rather than promote a guessed program.
func argvFromResult(result any) []string {
	bag, ok := result.(map[string]interface{})
	if !ok {
		return nil
	}
	switch v := bag["argv"].(type) {
	case []string:
		if len(v) == 0 {
			return nil
		}
		return append([]string(nil), v...)
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

// resolveArgs assembles the argument bag and refuses the dispatch when a
// required argument has no value.
//
// The approver is injected here rather than by the caller of Resolve,
// because every tool on the write path reads it to decide whether the
// operation is authorised. An invoker that could omit it would turn the
// adapter's approval gate into something a bug in this file could switch
// off — the gate has to be unreachable from here, not merely respected.
func (i RegistryInvoker) resolveArgs(ctx context.Context, req RemediationRequest, spec ToolSpec) (map[string]any, error) {
	args := map[string]any{}

	if i.ArgResolver != nil {
		resolved, err := i.ArgResolver.Resolve(ctx, req, spec)
		if err != nil {
			return args, fmt.Errorf("%w: %s: %s", ErrUnresolvableArguments, spec.Name, err.Error())
		}
		for k, v := range resolved {
			args[k] = v
		}
	}
	// The target is always passed, so a tool may use it even when the
	// resolver does not need it. A resolver-produced value wins on
	// conflict: it was derived from the same option and knows more.
	if _, ok := args["target"]; !ok {
		if t := strings.TrimSpace(req.Option.Target); t != "" {
			args["target"] = t
		}
	}
	if approver := strings.TrimSpace(req.Approver); approver != "" {
		args["approved_by"] = approver
	}
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		args["reason"] = reason
	}

	var missing []string
	for _, name := range spec.RequiredArgs {
		if v, ok := args[name]; !ok || isEmptyArg(v) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// The message names the arguments and says what happened. An
		// operator reading the incident later needs to know whether the
		// proposal was incomplete or the tool was broken, and those two
		// look identical from "the action did not run".
		return args, fmt.Errorf("%w: %s needs %s, and this option carries only a resource locator (%q); "+
			"proposing an action without the values it acts on is not a dispatch that can be completed",
			ErrUnresolvableArguments, spec.Name, strings.Join(missing, ", "), req.Option.Target)
	}
	return args, nil
}

// isEmptyArg reports whether an argument counts as absent.
//
// Empty string and nil are the obvious cases. Zero is not: a pid of 0 is
// not a meaningful backend, but "limit: 0" is a value the caller chose, and
// a rule that treated it as missing would refuse calls that were
// deliberate. Anything the caller put in the bag is their decision to make.
func isEmptyArg(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

var _ RemediationInvoker = RegistryInvoker{}

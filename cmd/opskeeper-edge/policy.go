package main

import (
	"context"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/wire"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/edge/toolbroker"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
)

// The node's role ladder.
//
// The control plane has already decided who is asking — the tunnel is
// authenticated and IAM ran on the manager before the turn was sent — so
// what the node does with a role is not re-authenticate it but refuse to be
// talked past it. An agent that convinces the model to call a mutating tool
// does not change which rung it is standing on.
//
// The rungs are deliberately few and readable. A role an operator cannot
// name from memory is a role nobody will configure correctly during an
// incident, and the safe reading of an unrecognised one has to be the
// bottom of the ladder rather than the top.
const (
	// RoleAdmin is the SRE on call: it may run anything the node has
	// installed, still subject to approval for what changes a live system.
	RoleAdmin = "admin"
	// RoleOperator runs day-two work: reads and changes inside the systems
	// the node manages.
	RoleOperator = "user"
	// RoleViewer observes. Everything else is refused, and the refusal is
	// what stops a read-only console session from being talked into a
	// restart.
	RoleViewer = "viewer"
)

// roleCeiling maps a caller's role to the most dangerous class it may run.
//
// An empty or unrecognised role is read-only. That is the one default worth
// being emphatic about: the ceiling is what stops a mutating call, and a
// typo in a role name must not hand out the top of the ladder.
func roleCeiling(role string) domain.ToolClass {
	switch role {
	case RoleAdmin:
		return domain.ClassDestructive
	case RoleOperator:
		return domain.ClassWrite
	default:
		return domain.ClassRead
	}
}

// manifestsOf projects admitted packages to their governance manifests.
//
// The gate's allow-list is built from exactly the manifests that were
// admitted at boot, and from nothing else. A tool that appears in the
// agent at run time but in no manifest here is not in the registry, and a
// tool that is not in the registry is refused.
func manifestsOf(plugins []pluginmanifest.Plugin) []domain.PluginManifest {
	out := make([]domain.PluginManifest, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, p.Manifest)
	}
	return out
}

// toolAuthorizer builds the broker's second check.
//
// It is the gate's whole job done again, in host code, over a call the
// agent cannot see. Three things are re-established here, and each closes
// a way the first check could have been skipped:
//
//   - The tool is deployed and this role may run it, from the same registry
//     the gate reads. A package that ships an undeclared tool is refused.
//   - Where the node holds the real executor, it knows the tool's actual
//     permission class rather than the one a manifest claimed for it. That
//     is what catches a package that understates a tool, and it catches it
//     at the moment the model calls it, with the executor in hand — not
//     reviewed into compliance by a human reading YAML.
//   - A call that needed a human carries the gate's receipt. This is the
//     one the allow-list check cannot cover: a mutating tool that a package
//     got past the courier with would otherwise run with nobody asked. The
//     receipt is consumed, so one grant is one execution.
//
// The first two are properties of the deployment and could in principle be
// cached; the third is a fact about one call, and caching it would be the
// bug.
func toolAuthorizer(registry *policygate.Registry, gate ReceiptClaimer, obs autonomyObservations) toolbroker.Authorizer {
	return func(ctx context.Context, c toolbroker.Call) (bool, string) {
		call := policygate.Call{
			SessionID: c.SessionID,
			ToolName:  c.ToolName,
			Arguments: c.Arguments,
			Actor:     c.Actor,
			// The same derivation the control plane and the packaged
			// courier use (wire.ToolSummary / wire.ToolTarget). They
			// each had their own copy once, with different key orders, so
			// one call could be described two ways; and this one filled in
			// neither, which put an approval card in front of an operator
			// with no target and no summary at all. What the call reaches is
			// derived from the arguments the HOST re-encoded, never from
			// what the agent claimed.
			Target:  wire.ToolTarget(c.Arguments),
			Summary: wire.ToolSummary(c.ToolName, c.Arguments),
			// The arguments are the host's re-encoding, so the class this
			// assessment produces is a judgement about what will actually
			// run, not about what the agent said it would run.
			Class: domain.ClassUnknown,
		}
		if exec, ok := skill.Get(c.ToolName); ok {
			call.Class = classOfSkill(exec.Metadata().EffectiveClass())
		}

		policy := registry.Policy(roleCeiling(c.Actor))
		if permitted, reason := policy.Permitted(call); !permitted {
			return false, reason
		}
		if !policy.NeedsApproval(call) {
			return true, ""
		}
		// Autonomy is the one call whose permission is a signature rather
		// than a receipt, and only while the control plane is not here to
		// issue one. Everything above still applied: the tool has to be in
		// the package's declared inventory, under the caller's role
		// ceiling, and not understated by the call site. What is replaced
		// is the per-call human, and only because a human already read and
		// signed the exact argv this call is allowed to run. See
		// autonomyrouting.go for why this is a repair and not a loosening.
		if c.ToolName == builtin.ToolKey && autonomyIsLocal(obs) {
			return true, ""
		}
		if gate == nil {
			// A node with tools that need approval and no gate to ask has
			// no way to obtain a human's consent, so it must not run them.
			// Allowing here is the one answer that is certainly wrong.
			return false, c.ToolName + " needs an operator's approval, and this node has no approval gate to ask"
		}
		if !gate.ClaimReceipt(call) {
			return false, c.ToolName + " needs an operator's approval, and no approval for this exact call was given"
		}
		return true, ""
	}
}

// ReceiptClaimer is the gate's evidence that a human agreed to one call.
//
// It is an interface so the authoriser can be tested without a gate, and
// so this file — which is otherwise pure policy — carries no opinion about
// how approvals are stored.
type ReceiptClaimer interface {
	ClaimReceipt(policygate.Call) bool
}

// classOfSkill maps a skill's permission class onto the tool classes the
// governance vocabulary uses.
//
// The two vocabularies are close but not identical — "safe" is about
// whether a skill has side effects, "read" is about what an operator would
// expect a tool named this to do — and the mapping is the conservative
// one in every ambiguous case. host_restart_service is ClassMutating and
// becomes write, which is what sends it to an approval queue instead of
// running because a manifest said it was harmless.
func classOfSkill(c skill.Class) domain.ToolClass {
	switch c {
	case skill.ClassSafe:
		return domain.ClassRead
	case skill.ClassMutating:
		return domain.ClassWrite
	case skill.ClassDangerous:
		return domain.ClassDestructive
	default:
		// An unrecognised class is treated as the most dangerous thing in
		// the system. A skill author who adds a class the host has not
		// learned to read gets the strictest reading, not a default.
		return domain.ClassDestructive
	}
}

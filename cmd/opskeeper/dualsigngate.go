package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	managerbizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	managerbizhitl "github.com/vincent-wuhan/opskeeper/core/manager/biz/hitl"
)

// dualSignGate answers the approve path's question by asking hitl's rule
// file, which is the only thing in this tree that knows what "two people must
// sign" is supposed to mean.
//
// It lives at the composition root for the same reason the reader-tier gate
// does: the row belongs to approval and the rules belong to hitl, and neither
// domain may import the other. Whoever wires the two together is this file's
// author, and that somebody has to be able to read both.
type dualSignGate struct {
	policy *managerbizhitl.DualSignPolicy
	log    *slog.Logger
	// disabled is the explicit, logged opt-out. It exists because the honest
	// default — two administrators, and a row that waits for both — is a
	// deployment that cannot complete a destructive action when only one
	// administrator exists. That is the right failure (fail closed), but it is
	// a failure an operator has to be able to choose deliberately, and a
	// choice nobody can make is the same as the control never existing.
	disabled bool
}

// scopeKeys is the set of words a rule may key on, in the order they are tried.
//
// A row carries three separate statements about itself — what kind of action
// it is, how risky it is, and how far it reaches — and ADR-019 keys on the
// last two. Asking about all three and requiring every match to be satisfied
// is what makes a rule about "cluster" bind a row that is also
// "destructive": the stricter of the two wins, because the answer is the union
// of what is still missing rather than the first rule that happened to match.
func scopeKeys(s managerbizapproval.Scope) []string {
	var out []string
	for _, v := range []string{s.RiskClass, s.BlastRadius, s.Kind} {
		if v == "" {
			continue
		}
		dup := false
		for _, seen := range out {
			if seen == v {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

// Missing implements approval.Gate.
func (g *dualSignGate) Missing(_ context.Context, scope managerbizapproval.Scope,
	signers []managerbizapproval.Signer) []string {

	if g == nil || g.disabled {
		// The opt-out is a decision an operator makes, and it is logged every
		// time a row passes through it rather than once at boot: a control
		// that is off should be visible in the audit trail of the actions it
		// let through one signature at a time.
		if g != nil && g.log != nil && len(signers) > 0 {
			g.log.Warn("dual sign is DISABLED; this approval passed on signatures alone",
				slog.String("kind", scope.Kind),
				slog.String("risk_class", scope.RiskClass),
				slog.Int("signers", len(signers)))
		}
		return nil
	}

	keys := scopeKeys(scope)
	if len(keys) == 0 {
		// The producer did not say how far this action reaches. Requiring two
		// signatures for a row nobody classified would be inventing a risk
		// level, and letting one signature through is what every approval in
		// this system has always done. The gap is real and is recorded as
		// one; guessing would hide it behind a number nobody chose.
		if len(signers) > 0 {
			return nil
		}
		return []string{"one signature"}
	}

	hitlSigners := make([]managerbizhitl.Signer, 0, len(signers))
	for _, s := range signers {
		hitlSigners = append(hitlSigners, managerbizhitl.Signer{UserID: s.UserID, Role: s.Role})
	}

	var missing []string
	for _, key := range keys {
		err := g.policy.Validate(key, approveAction, hitlSigners)
		if err == nil {
			continue
		}
		var dse *managerbizhitl.DualSignError
		if !errors.As(err, &dse) {
			// A validator that fails in a shape nobody understands is not a
			// reason to let the action through.
			missing = append(missing, key+": "+err.Error())
			continue
		}
		if len(dse.Missing) > 0 {
			missing = append(missing, dse.Missing...)
			continue
		}
		// insufficient_signers names no group — the shortfall is a headcount,
		// not a role — so it is reported as the count it is.
		missing = append(missing, fmt.Sprintf("%s: %d signatures (%d so far)",
			key, 2, len(hitlSigners)))
	}
	return missing
}

const approveAction = "approve"

func newDualSignGate(policy *managerbizhitl.DualSignPolicy, log *slog.Logger) *dualSignGate {
	if policy == nil {
		return nil
	}
	return &dualSignGate{policy: policy, log: log, disabled: dualSignDisabled()}
}

// dualSignDisabled reads the opt-out. The name is checked against a small
// allowlist rather than "any value but true" so a typo in a deployment's
// environment turns the control back on instead of silently off.
func dualSignDisabled() bool {
	switch os.Getenv("OPSKEEPER_DUAL_SIGN") {
	case "off", "disabled", "false":
		return true
	}
	return false
}

var _ managerbizapproval.Gate = (*dualSignGate)(nil)
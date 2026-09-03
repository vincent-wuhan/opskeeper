// Package proposal implements D.4.3 approval policies: severity tier
// classification, the Expirer background loop, and the policy
// decisions the ReviewGate decorator + SPA inbox read at proposal
// time.
//
// Severity tiers drive the UI flow and the reviewer selection:
//
//	safe       — auto-approved (reviewer is bypassed; the loop only
//	             records the proposal for audit)
//	mutating   — single-person sign-off: reviewer worker decides;
//	             operator confirms in the SPA inbox
//	dangerous  — two-person sign-off: reviewer + a second approver
//	             (operator-confirm is insufficient)
//
// Tier assignment is a property of the tool (tool's Class) plus
// runtime context (e.g. target is a production device). The
// decorator (tools/decorators/review_gate.go) writes the tier into
// chat_mutating_proposals.severity_tier at intercept time.
package proposal

import (
	"github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
)

// SeverityTier is a re-exported string type for the three approval
// tiers (safe / mutating / dangerous). Callers use the constants
// below; the type exists so future methods can attach to it
// (e.g. IsValid()) without breaking call sites.
type SeverityTier string

// Re-exported tier constants. Use these instead of string literals.
const (
	SeveritySafe      = aiops.SeveritySafe
	SeverityMutating  = aiops.SeverityMutating
	SeverityDangerous = aiops.SeverityDangerous
)

// RequiresOperatorConfirm reports whether the tier needs an explicit
// operator confirmation in the SPA inbox (vs auto-approve). Both
// mutating and dangerous require it.
func RequiresOperatorConfirm(t SeverityTier) bool {
	return t == SeverityMutating || t == SeverityDangerous
}

// RequiresTwoPerson reports whether the tier needs a second human
// approver beyond the reviewer worker. Only dangerous requires it.
func RequiresTwoPerson(t SeverityTier) bool {
	return t == SeverityDangerous
}

// AutoApprove reports whether the tier can be approved without
// any human intervention. Only safe qualifies.
func AutoApprove(t SeverityTier) bool {
	return t == SeveritySafe
}

// Validate ensures the tier value is one of the three known
// constants. Returns an error for empty / unknown values; the
// decorator uses this to reject malformed tool class reports.
func (t SeverityTier) Validate() error {
	switch t {
	case SeveritySafe, SeverityMutating, SeverityDangerous:
		return nil
	}
	return errInvalidTier(string(t))
}

type invalidTierError string

func (e invalidTierError) Error() string {
	return "proposal: invalid severity tier: " + string(e)
}

func errInvalidTier(s string) error { return invalidTierError(s) }

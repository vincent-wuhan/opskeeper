// Package configchange is how a model changes an alert rule.
//
// It ships two tools that are the same code with a different kind: one
// drafts a change, the other applies a draft that a human confirmed. The
// split is the safety property — apply_config_change refuses without a
// confirmation, refuses a draft whose hash no longer matches what the model
// was shown, and refuses a caller who is not an admin. Those three checks
// are in validateApplyGate, and they are why draft and apply are separate
// tool names rather than one tool with a flag.
//
// A change travels as a hash. draft_config_change returns the rendered
// payload and its hash; apply_config_change recomputes the hash from the
// draft it fetched and compares. A rule edited between the two calls fails
// the comparison, so what gets applied is what the reviewer saw rather than
// what the table happens to hold now.
//
// The caller identity comes from the request context through tenantctx, not
// from the arguments, so a model cannot name its way past the role check.
//
// ConfigManager is the seam to the alert-rule config biz layer. The type
// aliases at the top (AlertRuleConfigInput, AlertRuleCondition) re-export
// alertdraft's shapes under the names the tool arguments use, so the wire
// contract and the draft's internal type do not drift apart.
package configchange

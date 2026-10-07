package ports

import "context"

// SensitivityGate decides whether the caller on ctx may act on a resource
// carrying the sensitivity it reports.
//
// It is a port rather than a call into the iam module because the two live in
// bounded contexts that must not import each other: the tool chain asks the
// question, and the assembly root supplies the answer from the RBAC enforcer
// and the per-user tier table. The alternative — the decorator importing iam
// directly — is a dependency edge that would have to be undone the day the
// authorization model moves.
//
// The resource is passed as the pair the labels are keyed by, and either half
// may be empty. An empty pair is the "this tool names no resource" case, and
// an implementation is expected to treat it as unlabeled rather than as a
// denial: a gate that refuses every tool call whose arguments do not name a
// resource refuses the product, and a refusal nobody can act on is the same
// as no gate.
type SensitivityGate interface {
	// Check returns nil when the caller may act on the resource, and an
	// error naming the shortfall when not.
	Check(ctx context.Context, resourceType, resourceID string) error
}

// MultiResourceGate is the batch form of SensitivityGate, for a call that
// names several resources at once.
//
// It is a separate interface rather than a wider Check so that existing
// single-resource implementations keep compiling and keep meaning what they
// mean. A caller that holds a gate which does not implement this falls back
// to asking Check once per id, and refuses when any one of them refuses — the
// batch form is an optimization for an implementation that can answer about
// a set in one round trip, not a relaxation of the rule.
type MultiResourceGate interface {
	SensitivityGate

	// CheckAll returns nil only when the caller may act on every id. An
	// implementation that cannot decide a set should report an error rather
	// than decide the set by its first member.
	CheckAll(ctx context.Context, resourceType string, resourceIDs []string) error
}

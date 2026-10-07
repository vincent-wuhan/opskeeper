package approval

import (
	"context"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The two pieces ADR-019 needs and this package did not have: somewhere to
// keep the signers, and a way to ask whether there are enough of them.
//
// Both are ports rather than direct calls into biz/hitl. hitl owns the rule
// file; approval owns the row. Whichever of them imported the other would be a
// dependency edge between two domains of the same module, and the direction of
// that edge would be decided by which file somebody opened first. The
// composition root implements Gate over hitl's validator, exactly as it
// assembles the reader-tier gate over the label store and the iam enforcer.

// Signer is one person who has signed an approval.
type Signer struct {
	UserID uint64    `json:"user_id"`
	Role   string    `json:"role"`
	At     time.Time `json:"at"`
}

// Scope is what an approval is asking to be signed for, and it is the row's
// own words rather than anything inferred at decision time: the producer said
// "this is destructive and it reaches a cluster" when it queued the row, and
// re-deciding that at approve time would let the answer change underneath the
// first signer.
type Scope struct {
	Kind        string
	RiskClass   string
	BlastRadius string
}

// Gate answers the one question dual sign asks: are these signatures enough?
type Gate interface {
	// Missing returns the role groups the signers do not yet cover. An empty
	// answer means the signatures are sufficient and the row may be decided.
	//
	// It takes no "required" argument on purpose. An earlier shape of this
	// port asked the gate twice — once for the requirement, once for the
	// verdict — and the second call could only ignore the first, which is a
	// way to make two rules disagree and only ship one.
	Missing(ctx context.Context, scope Scope, signers []Signer) []string
}

// Escalator raises a proposed action's risk class when the resource it acts
// on is labelled more sensitive than the class alone suggests.
//
// It is a port for the same reason Gate is: the labels live in dataguard and
// the row lives here, and the composition root is the only place allowed to
// hold both.
//
// A nil Escalator is not "no escalation" and not "escalate everything" — it
// is "this deployment has no labels", which is the same answer a node with no
// endpoint gets. The direction of the mistake is the point: a control that is
// absent must not be able to make an action look *safer* than its tool
// declared, and it may only fail to make it look *worse*.
type Escalator interface {
	// ClassFor returns the class the target's label demands, whether the
	// target is labelled at all, and any failure looking it up.
	//
	// The error is a third return value rather than folded into the bool on
	// purpose: "this resource carries no label" and "nobody could read the
	// labels" are different answers, and collapsing them makes a store
	// outage indistinguishable from an unlabelled resource — which is a
	// silent downgrade of an authorization check.
	//
	// The targets are bare ids, because that is what a tool call carries. An
	// implementation is expected to resolve each across resource types and
	// take the strictest answer overall, so neither a name shared by a pod
	// and a service nor a call that reaches twelve devices is resolved to the
	// mildest of what it touches. That is not a refinement: a gate that reads
	// the first entry is a gate an attacker walks past by reordering the
	// list, and decision 361 already paid for learning that once.
	ClassFor(ctx context.Context, targets []string) (domain.ToolClass, bool, error)
}

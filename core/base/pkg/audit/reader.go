package audit

import (
	"context"
	"time"
)

// The change-event read shapes.
//
// They exist because of decision 273, and the thing they replace is worth
// naming because it is the last remaining shape of this particular mistake.
//
// Decision 272 gave this package the vocabulary to ask to be remembered: Event
// for the row, Sink / IDSink / Verifier for the three ways of reaching the
// host, and the node-ledger replay shapes beside them. What it could not give
// is the opposite direction, and one edge still needed the opposite direction.
// The RCA tool `query_change_events` — "what changed in the thirty minutes
// around the incident" — reached into core/domains/model/audit and named its
// storage entity, `Log`, as the return type of the seam it declared:
//
//	type AuditLister interface {
//	    ListChanges(...) ([]auditmodel.Log, error)
//	}
//
// The interface was already a port. The dependency was in the type, which is
// the half of an interface that is a struct underneath and therefore belongs
// to somebody. A GORM entity with seventeen columns, an index per filter and
// three hash-chain columns is the audit domain's private storage shape, and
// the RCA loop was compiling against all of it to read nine of its fields.
//
// That is a declared cross-domain edge, and it was the last inbound edge on
// the audit domain. Which is the part that makes it worth doing rather than
// worth filing: every other domain in the tree can be released on its own
// schedule, but the tamper-evident ledger could not, because the one context
// that reads it had to be recompiled and redeployed in step with any change
// to the row's columns. The ledger is the one component whose schema should
// never be anyone's business but its own.
//
// So the nine fields the reader actually needs are stated here, as a
// projection, and the entity stays behind. Nothing about who may write is
// relaxed by that: this is a read port, and the four write ports above are
// unaffected.

// ChangeRow is one recorded change, as a reader sees it.
//
// It is deliberately not a subset of the storage row that happens to be
// convenient. Two groups of columns are left out and each exclusion is a
// decision, not an oversight:
//
//   - The chain columns (Seq / PrevHash / Hash) are gone. A reader that
//     could see where a row sits in the chain could start reasoning about
//     the chain — "this row was tampered with", "skip to the next good one" —
//     and that is verification, which Verifier above is for. Handing a
//     projection the chain's own coordinates is how a read port turns into a
//     second, unauditable opinion about the ledger's integrity.
//   - The request columns (IP / UserAgent / RequestID) are gone too, and for
//     a different reason: they answer "who was this and from where", which is
//     a question the audit UI asks and an RCA tool does not. Carrying them
//     would mean every future reader inherited a PII-shaped row it had no use
//     for, and the field would outlive the use.
//
// The field names match Event, deliberately, so that a row here and an Event
// there are recognisably the same record seen from two ends. The one
// exception is the JSON payload: Event.Payload is `any` on the way in because
// the caller chooses a shape, and it is a string on the way out because the
// row stores what the caller chose and a reader should not re-derive it.
type ChangeRow struct {
	OccurredAt   time.Time
	UserEmail    string
	Role         string
	Action       string
	ResourceType string
	ResourceID   string
	ResourceName string
	Status       string
	PayloadJSON  string
}

// ChangeLister is the read port over the ledger: the mutating rows in
// [from, to], optionally narrowed to one resource type and/or one action,
// capped at limit.
//
// The parameters are the query's own vocabulary rather than a filter struct,
// because the ledger's filters are not free-form and a struct here would
// invite a future caller to add one that is. There are exactly three things
// worth narrowing by — when, what kind of thing, what was done to it — and a
// fourth (free text over the payload) is a full table scan on an append-only
// table, which is the kind of thing that looks harmless in a query builder.
//
// Failures are in scope and are not filtered out by the host. "Someone tried
// to change X and was denied" is a root-cause lead, and a read port that
// quietly returned only successes would be a port that lies by omission.
type ChangeLister interface {
	ListChanges(ctx context.Context, from, to time.Time, resourceType, action string, limit int) ([]ChangeRow, error)
}

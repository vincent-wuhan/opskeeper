package tunnel

// agent.audit.entries (edge -> manager)
//
// The node's own ledger rows, on their way into the chain.
//
// This is the method that did not exist. Every entry type in core/ports was
// written for the node plane — policygate writes tool calls through it, the
// PiG agent writes its turn through it, and the plugin installer was meant to
// write installs through it — and every one of them had nowhere to go, because
// the HMAC chain lives in the manager's database on another machine. The
// plumbing on both ends was correct: ports.AuditSink is a real port, a real
// implementation of it exists, and the edge deliberately never wired one in
// rather than wiring a dead one. What was missing is the last hop.
//
// The two ends of the hop already existed in a different shape: autonomy
// replay carries a node's self-heal rows to the same chain over the same
// connection. This method is that path generalized to rows that are not
// about self-healing, and it is a separate method rather than a widened
// agent.audit.replay because the two row shapes have nothing in common but
// the fact that a node produced them. Widening the old one would have made
// its name wrong and forced autonomy's payload to grow a field it never
// fills.

import (
	"encoding/json"
	"time"
)

// AuditEntry is one node-side ledger row as it travels.
//
// It mirrors core/ports.AuditEntry field-for-field and, like
// AutonomyAuditRow above, does not share the type: this package is the wire,
// and the wire does not import the port vocabulary it happens to carry. The
// PrevHash and Hash fields of the port type are absent because the node
// cannot compute them — a chain link is the manager's to write, and a node
// that could compute one could forge a chain.
type AuditEntry struct {
	// At is when the node recorded the row. The chain stamps its own
	// entry time and keeps this in the payload, for the same reason the
	// autonomy row does: the chain's clock records when a fact entered
	// evidence, and the node's clock is the fact.
	At time.Time `json:"at"`
	// Actor is who or what caused it. On a node that is usually a person
	// the manager authenticated earlier, or a component; it is a claim
	// about the node's side of the story, and the manager files the row
	// under the node's own authenticated identity regardless.
	Actor string `json:"actor"`
	// Action is one of the closed values in core/ports. The manager maps
	// it to its own vocabulary and refuses the batch for a value it does
	// not know, because a row whose action nobody can interpret is not
	// evidence of anything.
	Action string `json:"action"`
	// Target is what was acted on: a tool, a plugin, a device.
	Target  string          `json:"target"`
	Outcome string          `json:"outcome"`
	Class   string          `json:"class,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// AuditEntriesRequest is one batch of the node's ledger rows.
//
// EdgeID is carried for the same reason AutonomyAuditReplayRequest carries
// it, and is subject to the same rule: the manager binds the batch to the
// authenticated edge identity and ignores this field whenever a binding
// exists. A node that filed another node's tool calls into the chain would
// make the chain say something false about a host it does not own.
type AuditEntriesRequest struct {
	EdgeID  uint64       `json:"edge_id,omitempty"`
	Entries []AuditEntry `json:"entries"`
}

// AuditEntriesResponse reports how the chain took the batch.
//
// The three states are deliberately the same three the autonomy replay
// answers with, and for the same reasons, because both are writes to one
// ordered append-only chain with no dedupe key:
//
//   - accepted == len(entries): written. The node may forget them.
//   - accepted == 0, rejected == len(entries): refused **whole** for shape.
//     Nothing was written, and it is not retried — the rows will have the
//     same shape next time — so the node counts them, passes them and logs.
//   - accepted == 0, rejected == 0 with no error: the manager could not
//     place them yet. "Took none" is not "refused all", and the node keeps
//     the batch.
//
// The types are separate rather than shared so that the two methods can be
// changed one at a time; the *rules* are one rule, and a change to either
// half has to be read as a change to both.
type AuditEntriesResponse struct {
	Accepted int    `json:"accepted"`
	Rejected int    `json:"rejected"`
	Reason   string `json:"reason,omitempty"`
}

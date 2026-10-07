// Package auditwire converts the node's own ledger rows from the port
// vocabulary into the wire shape that carries them to the center's chain.
//
// It is a component of exactly one function, and that is the point. The
// conversion used to live in cmd/opskeeper-edge's assembly root, next to the
// tunnel client that sends it — which meant **no test outside that package
// could reach it**. Every other hop of agent.audit.entries had a test and the
// route had none, because the one hop that turned a port row into a wire row
// was unreachable from both ends. That is the same shape as decision 323's
// in-place closures: a piece of behaviour inlined into a wiring file is
// behaviour nobody can call twice.
//
// It is not in core/edge/auditlog either, and .go-arch-lint.yml says why that
// package may depend on the contract layer and the shared spool only: below
// it lies the node's one local dataset with no backup, and its dependency
// face is deliberately the smallest thing that can hold the rows. A wire
// helper does not write, read or replay anything, so putting it there would
// buy nothing and cost that argument.
package auditwire

import (
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// NewAuditEntriesRequest builds one batch of a node's ledger rows for
// agent.audit.entries.
//
// The mapping is field-for-field and deliberately so: the wire type does not
// share the port type (core/floor/tunnel does not import the port vocabulary
// it happens to carry), so this function is the single place where the two
// shapes are declared to be the same thing. A field added to one and forgotten
// in the other is exactly the silent drop decision 126 warned about, and it
// would be invisible — the row would still be written, just without the field
// an operator searches for.
func NewAuditEntriesRequest(edgeID uint64, rows []ports.AuditEntry) tunnel.AuditEntriesRequest {
	req := tunnel.AuditEntriesRequest{
		EdgeID:  edgeID,
		Entries: make([]tunnel.AuditEntry, 0, len(rows)),
	}
	for _, r := range rows {
		req.Entries = append(req.Entries, tunnel.AuditEntry{
			At:      r.At,
			Actor:   r.Actor,
			Action:  string(r.Action),
			Target:  r.Target,
			Outcome: r.Outcome,
			Class:   r.Class,
			Detail:  r.Detail,
		})
	}
	return req
}

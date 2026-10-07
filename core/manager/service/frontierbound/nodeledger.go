package frontierbound

import (
	"context"
	"errors"
	"fmt"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// NodeLedger turns the wire shape of a node's own ledger rows into the audit
// BC's vocabulary, and hands the batch to the chain.
//
// It is AutonomyReplay's twin and it is deliberately a separate type rather
// than a widened one. The two carry rows a node produced, and that is the
// only thing they have in common: an autonomy row is one decision in
// thirteen fields with a phase that is always set, and a ledger row is
// whatever three different components on that node decided was worth
// writing. A single method carrying both would have had a union payload and
// two sets of rules for "is this row interpretable", and the second set is
// the one that decides whether a node's tool calls are evidence.
//
// The conversion does the one thing the wire cannot: it types the action.
// ports.AuditAction is a closed set, so an action a node invented arrives
// here as a value no map entry matches and is refused by the BC's shape pass
// — as a fact about the vocabulary, not as a string that got stored.
type NodeLedger struct {
	sink auditport.NodeLedgerSink
}

// NewNodeLedger returns the recorder the frontierbound Wiring wants.
func NewNodeLedger(sink auditport.NodeLedgerSink) *NodeLedger { return &NodeLedger{sink: sink} }

// RecordNodeEntries converts and records one batch.
//
// A nil usecase is an error for the same reason AutonomyReplay's is, and
// with the same consequence: the node is told the center could not take the
// batch, which it retries, rather than the center took nothing, which it
// also retries but logs as a refusal that is not happening.
func (n *NodeLedger) RecordNodeEntries(ctx context.Context, edgeID uint64, rows []tunnel.AuditEntry) (int, int, error) {
	if n == nil || n.sink == nil {
		return 0, 0, errors.New("frontierbound: the node ledger has no audit sink behind it")
	}
	converted := make([]auditport.NodeLedgerRow, 0, len(rows))
	for _, r := range rows {
		entry := auditport.NodeLedgerRow{
			At:      r.At,
			Actor:   r.Actor,
			Action:  ports.AuditAction(r.Action),
			Target:  r.Target,
			Outcome: r.Outcome,
			Class:   r.Class,
		}
		// A row with no time is passed on as it arrived rather than
		// backdated here. The manager's clock is not the node's evidence,
		// and the BC refuses such a batch for shape either way — the only
		// question this package has to answer is whether it quietly invents
		// one.
		entry.Detail = r.Detail
		converted = append(converted, entry)
	}
	res, err := n.sink.RecordNodeEntries(ctx, edgeID, converted)
	if err != nil {
		return res.Accepted, res.Rejected, fmt.Errorf("node ledger: edge %d: %w", edgeID, err)
	}
	return res.Accepted, res.Rejected, nil
}

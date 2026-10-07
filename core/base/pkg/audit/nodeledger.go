package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The node-ledger replay shapes.
//
// These four types and the one interface below are here for the same reason
// Event and Sink are: frontierbound was reaching into the audit domain's
// concrete façade to hand it rows a node produced while the tunnel was down.
// That made the fifth `X → audit` declared edge (decision 272) and it was the
// most expensive of the four in one specific way — the *shapes* had to travel
// with the *call*, because RecordNodeEntries takes []NodeLedgerRow and there
// is no way to call it without naming the row.
//
// A row shape is exactly what this package is for. The header says it holds
// "the shape of a row" and names the HTTP status bucket and the handler
// stamp as the two things already down here; a node's ledger row is a third.
//
// What is deliberately NOT here: the translation. nodeActionMap — the map
// from core/ports' thirteen actions onto the chain's canonical action names —
// stays in the audit domain, because deciding what a node's "tool_call" means
// on this side of the wire is a judgement about the ledger, and a port that
// carried it would be carrying a policy with it.

// NodeLedgerRow is one entry from a node's own ledger: every tool call, block,
// agent turn and plugin install the node recorded while it was on its own.
type NodeLedgerRow struct {
	At      time.Time
	Actor   string
	Action  ports.AuditAction
	Target  string
	Outcome string
	Class   string
	Detail  json.RawMessage
}

// NodeLedgerResult reports how a batch was taken.
//
// Accepted and Rejected are counts, not a bool, because a node that had
// three of four rows refused needs to say so to the operator watching the
// tunnel come back — "the replay worked" is not a useful answer when a
// quarter of it did not.
type NodeLedgerResult struct {
	Accepted int
	Rejected int
}

// AutonomyReplayRow is one decision a node made on its own while the control
// plane was unreachable: the trigger, the pre-defined argv it was allowed to
// run, the blast radius it was confined to, and what happened.
//
// It is a separate type from NodeLedgerRow rather than a widened one because
// the two carry rows a node produced and that is the only thing they share:
// an autonomy row is one self-heal decision, a ledger row is one observed
// action, and a query over "what did this node do to itself" wants them in
// one list without the fields of either meaning anything for the other.
type AutonomyReplayRow struct {
	At        time.Time
	Action    string
	Package   string
	Tool      string
	Target    string
	Argv      []string
	Kind      string
	Metric    string
	Threshold float64
	Key       string
	Verdict   string
	Reason    string
	Phase     string
	Result    string
	ExitCode  int
}

// AutonomyReplayResult reports how an autonomy batch was taken. Same shape as
// NodeLedgerResult and for the same reason.
type AutonomyReplayResult struct {
	Accepted int
	Rejected int
}

// NodeLedgerSink is the port for the two replay paths.
//
// One interface rather than two because the object on the other end is one
// object: a binding that could take a node's ledger but not its autonomy
// decisions would be a binding shaped by what the caller happened to need
// rather than by what the host is. The two *row* types stay separate for the
// reason given above, and that separation is the one that matters — it is
// what stops a query for tool calls from returning self-heal decisions.
//
// edgeID is a parameter rather than a field on the row because every row in
// a batch came from the same node, and a per-row edge id is a field a future
// batched-across-nodes implementation would have to trust rather than derive.
type NodeLedgerSink interface {
	// RecordNodeEntries writes a node's own ledger rows into the chain.
	//
	// The whole batch is shape-checked before the first row is written and
	// the storage writes all of it or none of it, because the chain is
	// append-only with no dedupe key: a prefix written and then failed
	// would be written again by the node's all-or-nothing pump, and a
	// duplicated tool call in a ledger of tool calls is worse than a
	// late one.
	RecordNodeEntries(ctx context.Context, edgeID uint64, rows []NodeLedgerRow) (NodeLedgerResult, error)

	// RecordAutonomyReplay writes a node's self-heal decisions into the
	// chain. Every row goes through the stamping write path, so a replayed
	// decision is chained exactly like an operator's.
	RecordAutonomyReplay(ctx context.Context, edgeID uint64, rows []AutonomyReplayRow) (AutonomyReplayResult, error)
}

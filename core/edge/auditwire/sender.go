package auditwire

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Sender hands one batch of a node's ledger rows to the control plane, which
// appends it to the tamper-evident chain.
//
// It used to be a type in cmd/opskeeper-edge's assembly root, next to the
// pump that calls it, and that placement is the same mistake decision 323
// found in the other direction: **the one place that decides whether a node
// keeps or drops evidence sat in a wiring file**. The three refusals below are
// behaviour — they are what makes a backlog survive a reconnect and what
// makes a malformed row loud instead of silent — and behaviour in package
// main can only be tested from package main. Moving it here is what lets
// core/manager's tests drive the node's real send path, and therefore what
// makes the end-to-end proof of agent.audit.entries possible at all.
type Sender struct {
	client tunnel.Client
	edgeID func() uint64
	log    *slog.Logger
	// refused counts rows the center will never take. A counter rather
	// than a log line because a node that has been replaying for an hour
	// should not have to be grepped to find out whether it has been
	// throwing evidence away.
	refused *atomic.Uint64
}

// NewSender returns the node's sender for agent.audit.entries.
//
// edgeID is read per batch rather than captured: the node learns its own id
// after this is built, and a sender that captured a zero would file every
// row under a node that does not exist. refused may be nil, in which case the
// count is kept internally and readable through Refused.
func NewSender(client tunnel.Client, edgeID func() uint64, log *slog.Logger, refused *atomic.Uint64) *Sender {
	if log == nil {
		log = slog.Default()
	}
	if refused == nil {
		refused = &atomic.Uint64{}
	}
	return &Sender{client: client, edgeID: edgeID, log: log, refused: refused}
}

// Send delivers rows in order, or reports that the batch has to come again.
//
// The refusals are autonomy's three answers with autonomy's reasons:
//
//   - The call fails: the tunnel is down again. Error, keep the batch.
//   - The center took none and refused none: it cannot place the rows yet —
//     the node has not registered, so the manager has no identity to file
//     them under. Error, keep the batch, retry. Reading this as a permanent
//     refusal is how a backlog dies in the first message after a reconnect.
//   - The center refused some rows for shape. Retrying would ask the same
//     question forever, so they are counted and passed over. It is loud, it
//     is counted, and it is on the health line, because a row that goes this
//     way is a row that will never be evidence.
func (s *Sender) Send(ctx context.Context, rows []ports.AuditEntry) error {
	if len(rows) == 0 {
		return nil
	}
	req := NewAuditEntriesRequest(s.edgeID(), rows)
	var resp tunnel.AuditEntriesResponse
	if err := s.client.Call(ctx, tunnel.MethodAgentAuditEntries, req, &resp); err != nil {
		// A transport failure and a refused batch must not collapse into
		// one branch: one is retried, the other is not. The pump's
		// contract is that a returned error keeps the whole batch.
		return fmt.Errorf("node ledger: send %d rows: %w", len(rows), err)
	}
	switch {
	case resp.Accepted+resp.Rejected == len(rows):
		if resp.Rejected > 0 {
			s.refused.Add(uint64(resp.Rejected))
			s.log.Warn("the center refused node ledger rows for shape; they will not be retried",
				slog.Int("accepted", resp.Accepted),
				slog.Int("rejected", resp.Rejected),
				slog.String("reason", resp.Reason))
		}
		return nil
	case resp.Accepted == 0 && resp.Rejected == 0:
		// "Took none of it" is not "refused all of it": the center is not
		// ready to place these rows, and the honest instruction is to keep
		// them.
		return fmt.Errorf("node ledger: the center accepted none of %d rows; the batch stays on disk", len(rows))
	default:
		// A count that is neither "all" nor "none" is a center this build
		// does not understand. Guessing which half it took is how rows are
		// lost; keeping the whole batch costs one more round trip.
		return fmt.Errorf("node ledger: the center reported %d accepted and %d rejected of %d rows",
			resp.Accepted, resp.Rejected, len(rows))
	}
}

// Refused is how many rows this node has given up on. It goes on the health
// line: a node that is quietly discarding evidence looks exactly like a node
// that has nothing to report.
func (s *Sender) Refused() uint64 { return s.refused.Load() }

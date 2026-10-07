// Package changeevent implements the business logic for the edge
// change-event domain: persistence helpers, query helpers, and the
// retention cleanup goroutine.
package changeevent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/prom"
	edgestore "github.com/vincent-wuhan/opskeeper/core/manager/data/edge/store"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
)

// Usecase is the narrow API the frontierbound handler and the
// query_change_events tool consume. The data store is injected
// through the iface so tests can substitute an in-memory fake.
type Usecase struct {
	repo   edgestore.ChangeEventRepoIface
	logger *slog.Logger
}

// New constructs a Usecase around the given repo. logger may be nil.
func New(repo edgestore.ChangeEventRepoIface, logger *slog.Logger) *Usecase {
	if logger == nil {
		logger = slog.Default()
	}
	return &Usecase{repo: repo, logger: logger}
}

// ChangeEventRow is re-exported so callers can import the type from
// the biz package without coupling to the data layer.
type ChangeEventRow = edgemodel.ChangeEventRow

// BatchInsert persists a batch of edge change events. Mirrors the
// tunnel-side PushChangeEventsRequest.Events: same field set, same
// batching contract (≤100 per call). Returns the number of rows
// actually persisted.
func (u *Usecase) BatchInsert(ctx context.Context, events []ChangeEventRow) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	fresh, duplicates := u.splitReplays(ctx, events)
	if len(fresh) > 0 {
		if err := u.repo.BatchInsert(ctx, fresh); err != nil {
			u.logger.Warn("changeevent: batch insert failed",
				slog.Int("batch", len(fresh)),
				slog.Int("deduped", duplicates),
				slog.String("err", err.Error()))
			return 0, err
		}
	}
	if prom.ChangeEventsInsertedTotal != nil {
		for _, e := range fresh {
			prom.ChangeEventsInsertedTotal.WithLabelValues(e.Kind).Inc()
		}
	}
	if duplicates > 0 && prom.ChangeEventsDedupedTotal != nil {
		prom.ChangeEventsDedupedTotal.Add(float64(duplicates))
	}
	return len(fresh), nil
}

// Ingest takes the shape a transport can honestly build and turns it into
// rows. It exists because the tunnel handler used to do this conversion
// itself, and the two things it had to know are not transport questions:
//
//   - the labels map has to be JSON-encoded, because labels is a text column;
//   - an event the node never logged must be stored as NULL, not 0, because a
//     unique index sits on (edge_id, seq) and SQL treats NULL as distinct from
//     NULL while treating every 0 as the same key. A handler that stored 0 for
//     "no seq" would make every ordinary event on a node collide with every
//     other one.
//
// Decision 281 moved the whole conversion here, so the rule that the index
// depends on lives next to the index rather than in a handler three packages
// away. BatchInsert keeps taking rows: it is the shape the repo and the query
// tools already speak, and the replay split needs the stored form.
func (u *Usecase) Ingest(ctx context.Context, events []domain.ChangeEventInput) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	rows := make([]ChangeEventRow, 0, len(events))
	for _, e := range events {
		rows = append(rows, ChangeEventRow{
			EdgeID:    e.EdgeID,
			Source:    e.Source,
			Kind:      e.Kind,
			Subject:   e.Subject,
			Action:    e.Action,
			Timestamp: e.Timestamp,
			Severity:  e.Severity,
			Labels:    MarshalLabels(e.Labels),
			Seq:       e.Seq,
		})
	}
	return u.BatchInsert(ctx, rows)
}

// splitReplays separates events the center already has from events it does
// not, by the node's write-ahead log sequence.
//
// It reports only the count of new rows because that is the only number the
// node can act on: a replayed event is already stored, so acknowledging it
// is correct and ignoring it is the right thing to do. The previous
// implementation returned len(events) unconditionally, which meant a node
// that replayed a thousand events after a reconnect was told it had a
// thousand fresh ones — and the per-kind insert counter counted them all,
// so a flapping link looked exactly like a burst of activity.
//
// A deduped event is not an error and does not make the batch fail. That is
// the whole point of the log: it is at-least-once, so "I have seen this one
// before" is the expected answer for every row in a replay.
func (u *Usecase) splitReplays(ctx context.Context, events []ChangeEventRow) (fresh []ChangeEventRow, duplicates int) {
	seen := make(map[uint64]bool)
	var want []uint64
	for i := range events {
		if events[i].Seq == nil {
			continue // never replayed: nothing to compare, nothing to drop
		}
		if !seen[*events[i].Seq] {
			seen[*events[i].Seq] = true
			want = append(want, *events[i].Seq)
		}
	}
	if len(want) == 0 {
		return events, 0
	}
	// A batch can only be for one edge — the handler resolves it from the
	// authenticated transport — but ask rather than assume, because a batch
	// that somehow named two would be exactly the case where deduping on
	// the wrong edge's rows would drop real events.
	edgeID := events[0].EdgeID
	for i := range events {
		if events[i].EdgeID != edgeID {
			u.logger.Warn("changeevent: batch mixes edges; storing it unfiltered",
				slog.Uint64("edge_id", edgeID),
				slog.Uint64("other_edge_id", events[i].EdgeID))
			return events, 0
		}
	}
	stored, err := u.repo.StoredSeqs(ctx, edgeID, want)
	if err != nil {
		// A lookup that fails must not turn into a silent loss, and it must
		// not turn into a silent duplicate store either. Storing unfiltered
		// is the safe direction: the unique index makes a repeat a no-op
		// at the database, so the cost of guessing wrong here is at most a
		// counter that over-reads by one batch.
		u.logger.Warn("changeevent: could not look up replayed sequences; storing unfiltered",
			slog.Uint64("edge_id", edgeID),
			slog.Int("batch", len(events)),
			slog.String("err", err.Error()))
		return events, 0
	}
	if len(stored) == 0 {
		return events, 0
	}
	have := make(map[uint64]bool, len(stored))
	for _, seq := range stored {
		have[seq] = true
	}
	fresh = make([]ChangeEventRow, 0, len(events))
	for _, e := range events {
		if e.Seq != nil && have[*e.Seq] {
			duplicates++
			continue
		}
		fresh = append(fresh, e)
	}
	return fresh, duplicates
}

// ListByWindow returns events in [from, to] filtered by kind.
// kind empty = all kinds. limit <= 0 = default 200.
func (u *Usecase) ListByWindow(ctx context.Context, from, to time.Time, kind string, limit int) ([]ChangeEventRow, error) {
	if from.IsZero() || to.IsZero() {
		return nil, errors.New("changeevent: from and to are required")
	}
	return u.repo.ListByWindow(ctx, from, to, kind, limit)
}

// ListChangeWindow is ListByWindow projected for the aiops domain.
//
// It exists because query_change_events is the one consumer outside this
// domain that reads change events, and until decision 283 it read them
// through a port that still returned *edgemodel.ChangeEventRow — an
// interface whose method signature reached into this domain's model package,
// so the "port" cost the caller a cross-domain import anyway. The projection
// keeps the eleven-column row on this side, where the write path, the replay
// dedup and the retention cleaner all need the sequence number, and hands
// the reader the seven columns it actually names.
//
// The one method rather than a conversion the caller does itself is the point:
// a caller-side conversion means every future caller re-derives which seven
// of the eleven it wanted, and the answer would be allowed to drift.
func (u *Usecase) ListChangeWindow(ctx context.Context, from, to time.Time, kind string, limit int) ([]domain.ChangeEvent, error) {
	rows, err := u.ListByWindow(ctx, from, to, kind, limit)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ChangeEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.ChangeEvent{
			EdgeID:    r.EdgeID,
			Kind:      r.Kind,
			Subject:   r.Subject,
			Action:    r.Action,
			Timestamp: r.Timestamp,
			Severity:  r.Severity,
			Labels:    r.Labels,
		})
	}
	return out, nil
}

// ListByEdge returns events for one edge in [from, to].
// from/to zero = no bound. limit <= 0 = default 200.
func (u *Usecase) ListByEdge(ctx context.Context, edgeID uint64, from, to time.Time, limit int) ([]ChangeEventRow, error) {
	return u.repo.ListByEdge(ctx, edgeID, from, to, limit)
}

// DeleteOlderThan is the retention hook used by Cleaner.
func (u *Usecase) DeleteOlderThan(ctx context.Context, ts time.Time) (int64, error) {
	return u.repo.DeleteOlderThan(ctx, ts)
}

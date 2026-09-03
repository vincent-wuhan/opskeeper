// Package changeevent implements the business logic for the edge
// change-event domain: persistence helpers, query helpers, and the
// retention cleanup goroutine.
package changeevent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	edgestore "github.com/vincent-wuhan/opskeeper/internal/manager/data/edge/store"
	edgemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/prom"
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
	if err := u.repo.BatchInsert(ctx, events); err != nil {
		u.logger.Warn("changeevent: batch insert failed",
			slog.Int("batch", len(events)),
			slog.String("err", err.Error()))
		return 0, err
	}
	if prom.ChangeEventsInsertedTotal != nil {
		for _, e := range events {
			prom.ChangeEventsInsertedTotal.WithLabelValues(e.Kind).Inc()
		}
	}
	return len(events), nil
}

// ListByWindow returns events in [from, to] filtered by kind.
// kind empty = all kinds. limit <= 0 = default 200.
func (u *Usecase) ListByWindow(ctx context.Context, from, to time.Time, kind string, limit int) ([]ChangeEventRow, error) {
	if from.IsZero() || to.IsZero() {
		return nil, errors.New("changeevent: from and to are required")
	}
	return u.repo.ListByWindow(ctx, from, to, kind, limit)
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

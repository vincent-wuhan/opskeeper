package changeevent

import (
	"context"
	"log/slog"
	"time"
)

// DefaultRetention is the default time-to-live for edge_change_events
// rows. Older rows are deleted by Cleaner.
const DefaultRetention = 90 * 24 * time.Hour

// DefaultCleanupInterval is the default Cleaner tick interval. Picked
// hourly so that a 90-day retention has 90 * 24 = 2160 chances to
// fire even at the extreme tail; in practice most deletes happen in
// the first few ticks after the boundary.
const DefaultCleanupInterval = time.Hour

// Cleaner is a background goroutine that periodically deletes
// edge_change_events rows older than Retention. Start it from
// cmd/opskeeper/main.go alongside the manager; cancellation is via
// the context passed to Run.
type Cleaner struct {
	uc        *Usecase
	logger    *slog.Logger
	Retention time.Duration
	Interval  time.Duration
}

// NewCleaner constructs a Cleaner with the given retention + interval.
// Zero values are replaced with defaults.
func NewCleaner(uc *Usecase, logger *slog.Logger) *Cleaner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Cleaner{
		uc:        uc,
		logger:    logger,
		Retention: DefaultRetention,
		Interval:  DefaultCleanupInterval,
	}
}

// Run blocks until ctx is cancelled. On each tick, deletes rows with
// ts < now-Retention. Failures are logged and skipped (the next tick
// retries); cleanup is best-effort, not critical-path.
func (c *Cleaner) Run(ctx context.Context) {
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().UTC().Add(-c.Retention)
			n, err := c.uc.DeleteOlderThan(ctx, cutoff)
			if err != nil {
				c.logger.Warn("changeevent: cleanup failed",
					slog.Time("cutoff", cutoff),
					slog.String("err", err.Error()))
				continue
			}
			if n > 0 {
				c.logger.Info("changeevent: cleanup deleted",
					slog.Int64("rows", n),
					slog.Time("cutoff", cutoff))
			}
		}
	}
}

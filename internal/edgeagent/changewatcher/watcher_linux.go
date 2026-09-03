//go:build linux

package changewatcher

import "context"

// spawnLinuxTailers 启动 journald + dockerd tailer。必须持有 w.mu。
func (w *Watcher) spawnLinuxTailers(parent context.Context) []context.CancelFunc {
	stops := make([]context.CancelFunc, 0, 2)

	stops = append(stops, w.spawn(parent, "journald", func(ctx context.Context) error {
		return newJournaldTailer(w.sink, w.logger).Run(ctx)
	}))

	stops = append(stops, w.spawn(parent, "dockerd", func(ctx context.Context) error {
		return newDockerdTailer(w.sink, w.logger).Run(ctx)
	}))

	return stops
}

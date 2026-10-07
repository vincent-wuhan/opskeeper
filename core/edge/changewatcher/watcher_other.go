//go:build !linux

package changewatcher

import "context"

// spawnLinuxTailers 在非 linux 平台是 no-op。
// packagemgr 已由 watcher.go 启动, 这里只覆盖 linux 专属的
// journald / dockerd tailer。
func (w *Watcher) spawnLinuxTailers(parent context.Context) []context.CancelFunc {
	return nil
}

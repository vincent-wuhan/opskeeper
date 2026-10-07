package edge

import (
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// PluginHealth is one plugin's last-reported runtime health, shipped by the
// edge on its heartbeat. It is intentionally ephemeral — kept in memory only,
// cleared on manager restart and re-populated within one heartbeat interval
// (~30s). The point of the type is operator visibility: State + LastError turn
// "the logs plugin silently ships nothing" into "logs: crashed — subprocess
// binary missing".
//
// The declaration moved to core/domain (decision 281): the tunnel handler in
// service/frontierbound builds these rows and could only name them by importing
// this package, which made the seam a package boundary rather than an
// interface one. These are aliases, not copies — one declaration, two names —
// so the import is genuinely gone rather than redirected.
type (
	PluginHealth       = domain.PluginHealth
	PluginTargetHealth = domain.PluginTargetHealth
)

// RecordPluginHealth stores the latest per-plugin health for one edge,
// overwriting any prior snapshot. Stamps ReportedAt with the manager clock so
// the UI can show staleness ("reported 4m ago") independent of edge clock
// skew. No-op for edgeID 0 or a nil/empty slice (a heartbeat without plugin
// data must not wipe a previously-good snapshot).
func (u *Usecase) RecordPluginHealth(edgeID uint64, items []PluginHealth) {
	if edgeID == 0 || len(items) == 0 {
		return
	}
	now := time.Now().UTC()
	for i := range items {
		items[i].ReportedAt = now
	}
	u.phMu.Lock()
	defer u.phMu.Unlock()
	if u.pluginHealth == nil {
		u.pluginHealth = make(map[uint64][]PluginHealth)
	}
	u.pluginHealth[edgeID] = items
}

// PluginHealth returns the last-reported plugin health for one edge, or nil
// if none has arrived yet (edge offline / pre-introduction agent / just
// restarted manager).
func (u *Usecase) PluginHealth(edgeID uint64) []PluginHealth {
	u.phMu.RLock()
	defer u.phMu.RUnlock()
	src := u.pluginHealth[edgeID]
	if len(src) == 0 {
		return nil
	}
	out := make([]PluginHealth, len(src))
	copy(out, src)
	return out
}

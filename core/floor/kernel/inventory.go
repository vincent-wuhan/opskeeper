package kernel

import (
	"reflect"
	"sort"
)

// InventoryEntry is one registered capability, as an operator sees it.
//
// Impl is the implementation's type name rather than the value: an inventory
// that rendered implementations would be a place that can print a DSN or an
// API key into a log line, and a type name is what an operator actually
// reads ("is that a real Loki client or a stub?").
type InventoryEntry struct {
	Kind   Kind   `json:"kind"`
	Name   string `json:"name"`
	Plugin string `json:"plugin"`
	Impl   string `json:"impl"`
}

// Inventory is what a host is currently running.
//
// It exists because "everything is a plugin" is only useful to an operator
// if they can see it: without this, a node's collector set and a control
// plane's datasource set are knowable only by reading Go source, which is
// the same position the plugin architecture was built to leave.
type Inventory struct {
	// Target is where this host runs.
	Target string `json:"target"`
	// Plugins is the sorted list of admitted, active packages.
	Plugins []string `json:"plugins"`
	// KindCounts is how many entries each extension point holds.
	KindCounts map[string]int `json:"kind_counts"`
	// Entries is every registered capability, ordered by kind then name.
	Entries []InventoryEntry `json:"entries"`
}

// Inventory returns the host's current capability set.
//
// It is a snapshot: a package activated or rolled back after this call is
// not in it, which is what an inventory is for.
func (h *Host) Inventory() Inventory {
	h.mu.Lock()
	plugins := make([]string, 0, len(h.live))
	for name := range h.live {
		plugins = append(plugins, name)
	}
	h.mu.Unlock()
	sort.Strings(plugins)

	entries := make([]InventoryEntry, 0, h.reg.Len())
	for _, kind := range Kinds() {
		for _, e := range h.reg.All(kind) {
			entries = append(entries, InventoryEntry{
				Kind:   e.Kind,
				Name:   e.Name,
				Plugin: e.Plugin,
				Impl:   implTypeName(e.Impl),
			})
		}
	}

	counts := map[string]int{}
	for _, k := range Kinds() {
		if n := len(h.reg.All(k)); n > 0 {
			counts[string(k)] = n
		}
	}

	return Inventory{
		Target:     string(h.pol.Target),
		Plugins:    plugins,
		KindCounts: counts,
		Entries:    entries,
	}
}

// implTypeName renders an implementation's type for the inventory.
func implTypeName(impl any) string {
	if impl == nil {
		return ""
	}
	return reflect.TypeOf(impl).String()
}
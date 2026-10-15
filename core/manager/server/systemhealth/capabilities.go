package systemhealth

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
)

// InventorySource is where the capability view reads the microkernel's
// registered set. The kernel Host satisfies it; the interface keeps the
// handler independent of the kernel's construction path and lets a test
// stand in a fixed inventory.
type InventorySource interface {
	Inventory() kernel.Inventory
}

// CapabilitiesHandler serves the plugin capability inventory.
//
// It lives in this domain rather than in one of its own because it answers
// the same question /v1/system/health answers — "what is this deployment
// running" — one level down. A handler of its own would have been a 57th
// control-plane domain for one read-only route.
//
// The inventory exists because "everything is a plugin" is a claim an
// operator can only take on trust unless they can see it. It deliberately
// reports each implementation's *type*, never its value, so it cannot become
// a place that renders a DSN or an API key. It mounts on the protected
// router: an inventory is a map of what this control plane can reach.
type CapabilitiesHandler struct{ src InventorySource }

// NewCapabilitiesHandler returns a handler over src.
func NewCapabilitiesHandler(src InventorySource) *CapabilitiesHandler {
	return &CapabilitiesHandler{src: src}
}

// Register mounts the route on the admin (protected) router.
func (h *CapabilitiesHandler) Register(r chi.Router) {
	r.Get("/v1/plugin-capabilities", h.list)
}

// list returns which packages this control plane admitted and what each of
// them registered into the microkernel.
func (h *CapabilitiesHandler) list(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.src == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the control plane exposes no plugin inventory",
			"code":  "no_source",
		})
		return
	}
	inv := h.src.Inventory()
	// Never emit a null where a caller will index it.
	if inv.Entries == nil {
		inv.Entries = []kernel.InventoryEntry{}
	}
	if inv.Plugins == nil {
		inv.Plugins = []string{}
	}
	if inv.KindCounts == nil {
		inv.KindCounts = map[string]int{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":      inv.Target,
		"plugins":     inv.Plugins,
		"kind_counts": inv.KindCounts,
		"entries":     inv.Entries,
		"total":       len(inv.Entries),
	})
}

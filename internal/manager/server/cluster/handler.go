// Package cluster exposes the cluster-status API (platform-base-ha).
//
// GET /api/v1/cluster/status returns the local replica's view of its
// HA role: which leader-only workers it's running, whether it's the
// leader, its uptime, and the outcome of the DB/Redis readiness probes.
// Admin-only — mirrors the requireAdmin pattern used by systemupgrade
// and other internal handler packages.
package cluster

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/leader"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/probes"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// LeaderState is the minimal subset of leader.Manager the handler
// reads. Defined as an interface so tests can stub it without spinning
// up a real Redis-backed manager.
type LeaderState interface {
	InstanceID() string
	IsLeaderAny() bool
	IsDraining() bool
	WorkersRunning() map[leader.Role]bool
}

// Handler serves /api/v1/cluster/status.
type Handler struct {
	state     LeaderState
	probes    *probes.Probes
	startedAt time.Time
}

// NewHandler constructs the cluster handler. state may be nil — in
// single-replica mode (OPSKEEPER_LEADER_ENABLED=false) the status still
// reports useful dependency + uptime info.
func NewHandler(state LeaderState, probesReg *probes.Probes, startedAt time.Time) *Handler {
	return &Handler{
		state:     state,
		probes:    probesReg,
		startedAt: startedAt,
	}
}

// Register mounts the cluster-status route. Call inside a protected
// (auth-gated) chi group — the handler does its own admin check on top.
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/cluster/status", h.status)
}

// WorkerStatus is one leader-only role's local state.
type WorkerStatus struct {
	Running bool      `json:"running"`
	Since   time.Time `json:"since,omitempty"`
}

// ClusterStatus is the /api/v1/cluster/status response body.
type ClusterStatus struct {
	InstanceID       string                        `json:"instance_id"`
	Role             string                        `json:"role"` // leader / follower / draining / standalone
	LeaderInstanceID string                        `json:"leader_instance_id,omitempty"`
	UptimeSeconds    int64                         `json:"uptime_seconds"`
	Dependencies     map[string]probes.CheckResult `json:"dependencies"`
	Workers          map[string]WorkerStatus       `json:"workers"`
}

// status godoc
// @Summary Get cluster status
// @Description Returns the local replica's HA role, worker state, and dependency health.
// @Router /v1/cluster/status [get]
// @Success 200 {object} cluster.ClusterStatus
func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	resp := ClusterStatus{
		UptimeSeconds: int64(time.Since(h.startedAt).Seconds()),
		Workers:       map[string]WorkerStatus{},
	}

	if h.state != nil {
		resp.InstanceID = h.state.InstanceID()
		resp.Role = h.deriveRole()
		if h.state.IsLeaderAny() {
			resp.LeaderInstanceID = h.state.InstanceID()
		}
		for role, running := range h.state.WorkersRunning() {
			resp.Workers[string(role)] = WorkerStatus{Running: running}
		}
	} else {
		resp.Role = "standalone"
	}

	// Run probes for dependency health. We reuse the probes package's
	// parallel checker runner by issuing a synthetic /readyz-style
	// computation. The probes package doesn't expose runCheckers
	// publicly, so we hit the handler and parse the JSON — or we
	// add a public method. For now, inline a minimal version.
	if h.probes != nil {
		resp.Dependencies = h.probes.RunChecks(r.Context())
	}

	writeJSON(w, http.StatusOK, resp)
}

// deriveRole maps the leader.Manager's state to a human-readable role
// label for the cluster status API.
func (h *Handler) deriveRole() string {
	if h.state == nil {
		return "standalone"
	}
	if h.state.IsDraining() {
		return "draining"
	}
	if h.state.IsLeaderAny() {
		return "leader"
	}
	return "follower"
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return false
	}
	if t.Role != "admin" {
		writeErr(w, errs.ErrForbidden)
		return false
	}
	return true
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	slug := "internal"
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		status, slug = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, errs.ErrForbidden):
		status, slug = http.StatusForbidden, "forbidden"
	default:
		status, slug = http.StatusInternalServerError, "internal"
	}
	writeJSON(w, status, errorBody{Error: err.Error(), Code: slug})
}

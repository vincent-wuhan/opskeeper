// Package loop — admin.go: test-only admin routes for the closed-loop
// orchestrator. Disabled in production unless OPSKEEPER_ENABLE_TEST_ADMIN_ROUTES=1.
//
// Why a separate file: these routes mutate orchestrator state directly
// (recovery_state retry_count) to let e2e runners exercise paths that
// would otherwise require a real AgentTeams runtime to drive (e.g.
// retry_count > MaxRetryCount → severity=dangerous). Keeping them
// isolated makes it trivial to audit "what runs in production" vs.
// "what runs only behind the test flag".
//
// Production safety:
//   - RegisterAdminRoutes is a no-op when Enabled=false.
//   - All admin routes are mounted on the protected (auth-gated) router,
//     so they require admin role.
//   - The handlers never write loop_event_log; the state they mutate is
//     the same state the orchestrator would have written, so audit trail
//     is preserved.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// RecoveryStateAdmin is the narrow seam exposed to the admin route.
// Production wires *managerdataloopstore.RecoveryStateStoreDB which
// already satisfies loop.RecoveryStateStore (same shape, re-declared
// here to avoid an import cycle).
type RecoveryStateAdmin interface {
	Get(ctx context.Context, incidentID string) (int, error)
	Increment(ctx context.Context, incidentID string) (int, error)
	Reset(ctx context.Context, incidentID string) error
}

// ErrNoRetryCountRow is returned when Get finds no row for the incident.
// Treated as 404 by the handler.
var ErrNoRetryCountRow = errors.New("loop: no retry_count row for incident")

// AdminRouteDeps bundles the admin route dependencies.
type AdminRouteDeps struct {
	// Enabled gates route registration. When false, RegisterAdminRoutes
	// is a no-op and the admin endpoints return 404.
	Enabled bool
	// StateStore is the recovery_state store. Required when Enabled=true.
	StateStore RecoveryStateAdmin
}

// RegisterAdminRoutes mounts the test-only admin routes on r.
//
// Routes (only mounted when Enabled=true AND StateStore!=nil):
//
//	POST /v1/admin/loops/recovery_state/{incident_id}/increment
//	  Body: {"times": 4}
//	  Effect: calls StateStore.Increment N times; returns final retry_count
//	          and a synthetic `escalated=true` flag (final_count > MaxRetryCount).
//	  Use: e2e runner forces retry_count > MaxRetryCount to exercise
//	       the recovered-phase severity escalation path without driving
//	       4 real rollback cycles.
//
//	GET /v1/admin/loops/recovery_state/{incident_id}
//	  Returns current retry_count for the incident. Read-only diagnostic.
//
//	POST /v1/admin/loops/recovery_state/{incident_id}/reset
//	  Resets retry_count to 0. Used to clean up between runner invocations.
func RegisterAdminRoutes(r chi.Router, deps AdminRouteDeps) {
	if !deps.Enabled || deps.StateStore == nil {
		return
	}
	r.Route("/v1/admin/loops/recovery_state", func(sub chi.Router) {
		sub.Post("/{incident_id}/increment", deps.incrementRetryCount)
		sub.Get("/{incident_id}", deps.getRetryCount)
		sub.Post("/{incident_id}/reset", deps.resetRetryCount)
	})
}

// --- handlers ----------------------------------------------------------

// IncrementRequest is the POST .../increment body.
type IncrementRequest struct {
	// Times is how many times to call Increment. Default 1.
	// Clamped to [1, 100] to prevent runaway.
	Times int `json:"times"`
}

// IncrementResponse is the POST .../increment response.
type IncrementResponse struct {
	IncidentID  string `json:"incident_id"`
	RetryCount  int    `json:"retry_count"`
	Escalated   bool   `json:"escalated"`
	Incremented int    `json:"incremented"`
}

func (d AdminRouteDeps) incrementRetryCount(w http.ResponseWriter, r *http.Request) {
	incidentID := chi.URLParam(r, "incident_id")
	if incidentID == "" {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionRecoveryRetryIncrement,
			ResourceType: auditport.ResourceIncident,
			Payload:      map[string]any{"applied": 0},
		}, errors.New("missing incident_id"))
		writeJSONError(w, http.StatusBadRequest, "missing incident_id")
		return
	}
	var req IncrementRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			auditFail(r, auditport.Event{
				Action:       auditport.ActionRecoveryRetryIncrement,
				ResourceType: auditport.ResourceIncident,
				ResourceID:   incidentID,
				Payload:      map[string]any{"applied": 0},
			}, fmt.Errorf("invalid json: %w", err))
			writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
	}
	if req.Times <= 0 {
		req.Times = 1
	}
	if req.Times > 100 {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionRecoveryRetryIncrement,
			ResourceType: auditport.ResourceIncident,
			ResourceID:   incidentID,
			// The rejected value is the point of this row: it is the only
			// place a caller can see that it asked for 500 attempts and was
			// stopped, rather than having it quietly clamped.
			Payload: map[string]any{"requested": req.Times, "applied": 0},
		}, errors.New("times must be in [1, 100]"))
		writeJSONError(w, http.StatusBadRequest, "times clamped to [1, 100]")
		return
	}
	final := 0
	for i := 0; i < req.Times; i++ {
		n, err := d.StateStore.Increment(r.Context(), incidentID)
		if err != nil {
			// `applied` is i, not req.Times, and this is the load-bearing
			// number in the row. The loop is not transactional: a store that
			// fails on the third call has already moved the counter twice
			// and this handler still answers 500. A failure row carrying the
			// requested count would read as "nothing happened" and the
			// escalation it caused would arrive with no cause.
			auditFail(r, auditport.Event{
				Action:       auditport.ActionRecoveryRetryIncrement,
				ResourceType: auditport.ResourceIncident,
				ResourceID:   incidentID,
				Payload: map[string]any{
					"requested":   req.Times,
					"applied":     i,
					"retry_count": final,
				},
			}, fmt.Errorf("increment #%d: %w", i+1, err))
			writeJSONError(w, http.StatusInternalServerError,
				fmt.Sprintf("increment #%d: %s", i+1, err.Error()))
			return
		}
		final = n
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionRecoveryRetryIncrement,
		ResourceType: auditport.ResourceIncident,
		ResourceID:   incidentID,
		Payload: map[string]any{
			"requested":   req.Times,
			"applied":     req.Times,
			"retry_count": final,
			// The flag the caller is really after: whether the counter is
			// now past MaxRetryCount and the next loop turn escalates.
			"escalated": final > 3, // MaxRetryCount = 3 (orchestrator.go:79)
		},
	})
	writeJSON(w, http.StatusOK, IncrementResponse{
		IncidentID:  incidentID,
		RetryCount:  final,
		Escalated:   final > 3, // MaxRetryCount = 3 (orchestrator.go:79)
		Incremented: req.Times,
	})
}

// GetRetryCountResponse is the GET response.
type GetRetryCountResponse struct {
	IncidentID string `json:"incident_id"`
	RetryCount int    `json:"retry_count"`
}

func (d AdminRouteDeps) getRetryCount(w http.ResponseWriter, r *http.Request) {
	incidentID := chi.URLParam(r, "incident_id")
	if incidentID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing incident_id")
		return
	}
	n, err := d.StateStore.Get(r.Context(), incidentID)
	if err != nil {
		if errors.Is(err, ErrNoRetryCountRow) {
			writeJSONError(w, http.StatusNotFound, "no retry_count row for incident")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, GetRetryCountResponse{
		IncidentID: incidentID,
		RetryCount: n,
	})
}

func (d AdminRouteDeps) resetRetryCount(w http.ResponseWriter, r *http.Request) {
	incidentID := chi.URLParam(r, "incident_id")
	if incidentID == "" {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionRecoveryRetryReset,
			ResourceType: auditport.ResourceIncident,
		}, errors.New("missing incident_id"))
		writeJSONError(w, http.StatusBadRequest, "missing incident_id")
		return
	}
	// Read the count before clearing it. The reset itself is the row's whole
	// content — after it there is no state left that could say what was
	// discarded — and without the before-value a reader cannot tell a routine
	// runner cleanup from the wipe of a genuine escalation. The read is best
	// effort: a store that will not answer a Get must still be allowed to
	// take the Reset, and the row says which of the two happened rather than
	// pretending the number is zero when it is unknown.
	previous, getErr := d.StateStore.Get(r.Context(), incidentID)
	known := getErr == nil
	payload := map[string]any{"previous_known": known}
	if known {
		payload["previous"] = previous
	}
	if err := d.StateStore.Reset(r.Context(), incidentID); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionRecoveryRetryReset,
			ResourceType: auditport.ResourceIncident,
			ResourceID:   incidentID,
			Payload:      payload,
		}, err)
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	payload["retry_count"] = 0
	auditOK(r, auditport.Event{
		Action:       auditport.ActionRecoveryRetryReset,
		ResourceType: auditport.ResourceIncident,
		ResourceID:   incidentID,
		Payload:      payload,
	})
	writeJSON(w, http.StatusOK, GetRetryCountResponse{
		IncidentID: incidentID,
		RetryCount: 0,
	})
}

// writeJSONError writes a JSON {"error":msg} response with the given
// status. Mirrors the shape of errorBody in http.go but takes a string
// directly (no errs.Err* typing needed for admin paths).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: msg})
}

// Package monitor builds the HTTP routes for user-managed Monitor page
// panels. Thin handler — auth + JSON decode + delegate to biz layer.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	biz "github.com/vincent-wuhan/opskeeper/core/domains/biz/monitor"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/monitor"
)

// PanelService is the narrow biz contract the handler needs.
// *biz/monitor.Service satisfies it.
type PanelService interface {
	List(ctx context.Context) ([]*model.Panel, error)
	Get(ctx context.Context, id uint64) (*model.Panel, error)
	Create(ctx context.Context, in biz.CreateInput) (*model.Panel, error)
	Update(ctx context.Context, id uint64, in biz.UpdateInput) (*model.Panel, error)
	Delete(ctx context.Context, id uint64) error
}

// Handler bundles the routes.
type Handler struct {
	svc PanelService
}

// NewHandler wires the handler.
func NewHandler(svc PanelService) *Handler { return &Handler{svc: svc} }

// Register attaches routes:
//
//	GET    /v1/monitor/panels         (any auth user)
//	POST   /v1/monitor/panels         (admin)
//	PATCH  /v1/monitor/panels/{id}    (admin)
//	DELETE /v1/monitor/panels/{id}    (admin)
//
// Listing is open to any authenticated operator so dashboards render
// for all users; mutations are admin-gated to mirror the rest of the
// settings/integration surface.
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/monitor/panels", h.list)
	r.Post("/v1/monitor/panels", h.create)
	r.Patch("/v1/monitor/panels/{id}", h.update)
	r.Delete("/v1/monitor/panels/{id}", h.delete)
}

type listResp struct {
	Panels []*model.Panel `json:"panels"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if !requireUser(w, r) {
		return
	}
	out, err := h.svc.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listResp{Panels: out})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		// The gate refused before the body was read, so the row says who was
		// refused and nothing about a request that never got that far.
		auditDenied(r, auditport.Event{
			Action:       auditport.ActionPanelCreate,
			ResourceType: auditport.ResourcePanel,
		}, errs.ErrForbidden)
		return
	}
	var in biz.CreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionPanelCreate,
			ResourceType: auditport.ResourcePanel,
		}, errors.Join(errs.ErrInvalid, err))
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	// The PromQL goes in as a length and a digest, not as text. A dashboard
	// query routinely carries a label whose value is a tenant token or an
	// internal host, and it is copied out of somebody's board by hand — the
	// same reasoning that keeps the secret store's values out of the chain.
	createPayload := map[string]any{
		"title":         in.Title,
		"type":          in.Type,
		"promql_len":    len(in.PromQL),
		"promql_digest": auditport.ValueDigest(in.PromQL),
	}
	if in.Ordinal != nil {
		createPayload["ordinal"] = *in.Ordinal
	}
	saved, err := h.svc.Create(r.Context(), in)
	if err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionPanelCreate,
			ResourceType: auditport.ResourcePanel,
			Payload:      createPayload,
		}, err)
		writeErr(w, err)
		return
	}
	createPayload["panel_id"] = saved.ID
	auditOK(r, auditport.Event{
		Action:       auditport.ActionPanelCreate,
		ResourceType: auditport.ResourcePanel,
		ResourceID:   strconv.FormatUint(saved.ID, 10),
		Payload:      createPayload,
	})
	writeJSON(w, http.StatusOK, saved)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		auditDenied(r, auditport.Event{
			Action:       auditport.ActionPanelUpdate,
			ResourceType: auditport.ResourcePanel,
			ResourceID:   chi.URLParam(r, "id"),
		}, errs.ErrForbidden)
		return
	}
	id, err := parseID(r)
	if err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionPanelUpdate,
			ResourceType: auditport.ResourcePanel,
			ResourceID:   chi.URLParam(r, "id"),
		}, err)
		writeErr(w, err)
		return
	}
	var in biz.UpdateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionPanelUpdate,
			ResourceType: auditport.ResourcePanel,
			ResourceID:   strconv.FormatUint(id, 10),
		}, errors.Join(errs.ErrInvalid, err))
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	// Which fields moved, not what they moved to. UpdateInput is six
	// pointers and a PATCH that sets two of them says "these two changed" —
	// which is the question a reader has — while the values, particularly the
	// PromQL, are the part that can carry somebody's token.
	updatePayload := map[string]any{"fields": changedFields(in)}
	saved, err := h.svc.Update(r.Context(), id, in)
	if err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionPanelUpdate,
			ResourceType: auditport.ResourcePanel,
			ResourceID:   strconv.FormatUint(id, 10),
			Payload:      updatePayload,
		}, err)
		writeErr(w, err)
		return
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionPanelUpdate,
		ResourceType: auditport.ResourcePanel,
		ResourceID:   strconv.FormatUint(id, 10),
		Payload:      updatePayload,
	})
	writeJSON(w, http.StatusOK, saved)
}

// changedFields names the columns a PATCH body actually set, in a fixed
// order so two rows can be compared by eye. A nil pointer means "leave this
// column alone", which is exactly the distinction the payload exists to
// keep.
func changedFields(in biz.UpdateInput) []string {
	var out []string
	if in.Title != nil {
		out = append(out, "title")
	}
	if in.Type != nil {
		out = append(out, "type")
	}
	if in.PromQL != nil {
		out = append(out, "promql")
	}
	if in.Legend != nil {
		out = append(out, "legend")
	}
	if in.Unit != nil {
		out = append(out, "unit")
	}
	if in.Ordinal != nil {
		out = append(out, "ordinal")
	}
	return out
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		auditDenied(r, auditport.Event{
			Action:       auditport.ActionPanelDelete,
			ResourceType: auditport.ResourcePanel,
			ResourceID:   chi.URLParam(r, "id"),
		}, errs.ErrForbidden)
		return
	}
	id, err := parseID(r)
	if err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionPanelDelete,
			ResourceType: auditport.ResourcePanel,
			ResourceID:   chi.URLParam(r, "id"),
		}, err)
		writeErr(w, err)
		return
	}
	// Read the panel before deleting it. This is the row's whole reason to
	// exist: a deleted panel leaves the board with nothing on screen to
	// notice it by, so "who removed the disk-latency graph and what was it
	// showing" is unanswerable afterwards unless the title was recorded on
	// the way out. A panel that is already gone is not an error here — the
	// delete is what the caller asked for and it has trivially succeeded —
	// but the row then says so rather than carrying an empty title that
	// reads like a panel with no name.
	deletePayload := map[string]any{"already_gone": false}
	if panel, gerr := h.svc.Get(r.Context(), id); gerr == nil {
		deletePayload["title"] = panel.Title
		deletePayload["type"] = panel.Type
	} else {
		deletePayload["already_gone"] = true
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionPanelDelete,
			ResourceType: auditport.ResourcePanel,
			ResourceID:   strconv.FormatUint(id, 10),
			Payload:      deletePayload,
		}, err)
		writeErr(w, err)
		return
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionPanelDelete,
		ResourceType: auditport.ResourcePanel,
		ResourceID:   strconv.FormatUint(id, 10),
		Payload:      deletePayload,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- helpers ---

func parseID(r *http.Request) (uint64, error) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.Join(errs.ErrInvalid, err)
	}
	if id == 0 {
		return 0, errs.ErrInvalid
	}
	return id, nil
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return false
	}
	if t.Role != tenantctx.RoleAdmin {
		writeErr(w, errs.ErrForbidden)
		return false
	}
	return true
}

func requireUser(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
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
	status := errs.HTTPStatus(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: err.Error(), Code: errCode(err)})
}

func errCode(err error) string {
	switch {
	case errors.Is(err, errs.ErrNotFound):
		return "not-found"
	case errors.Is(err, errs.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, errs.ErrForbidden):
		return "forbidden"
	case errors.Is(err, errs.ErrInvalid):
		return "invalid"
	default:
		return "internal"
	}
}

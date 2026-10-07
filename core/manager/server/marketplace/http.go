// Package marketplace exposes /v1/marketplace — the skill
// marketplace install / list / uninstall surface.
//
// Permissions:
//   - POST /v1/marketplace/install admin
//   - GET /v1/marketplace/installed any auth user
//   - DELETE /v1/marketplace/installed/{pack_id} admin
//   - GET /v1/marketplace/registries any auth user
package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	bizmp "github.com/vincent-wuhan/opskeeper/core/manager/biz/marketplace"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/marketplace"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
)

// Service is the narrow surface the handler depends on. *bizmp.Usecase
// satisfies it structurally.
type Service interface {
	Install(ctx context.Context, caller bizmp.Caller, src bizmp.Source) (*bizmp.InstallResult, error)
	List(ctx context.Context, caller bizmp.Caller) ([]*model.InstalledPack, error)
	Uninstall(ctx context.Context, caller bizmp.Caller, packID string) error
	SetBindings(ctx context.Context, caller bizmp.Caller, packID string, bindings map[string]string) error
	Registries(ctx context.Context, caller bizmp.Caller) bizmp.AllowedRegistries
}

// Handler bundles the marketplace routes.
type Handler struct {
	svc Service

	// importInto converts a legacy container directory into a PiG package,
	// and importRoot is where converted packages are written. Both are set
	// through SetImporter (see import.go); until they are, the import route
	// answers 503 rather than converting into a directory nobody named.
	importInto ImportFunc
	importRoot string
}

// NewHandler builds the handler.
// NewHandler builds the marketplace handler.
//
// The importer and the import root are constructor parameters rather than a
// setter, and that shape was measured rather than preferred: with a setter,
// a seam test wires it itself and therefore cannot catch a boot path that
// forgot to, and "the import route answers 503 forever" is exactly the
// failure a test written beside the wiring will not see. As arguments, a
// forgotten wiring does not compile (decision 252, after decision 249).
func NewHandler(svc Service, importer ImportFunc, importRoot string) *Handler {
	return &Handler{svc: svc, importInto: importer, importRoot: importRoot}
}

// Register attaches the routes under a chi.Router that already has
// the auth middleware in front of it (see cmd/opskeeper).
func (h *Handler) Register(r chi.Router) {
	r.Post("/v1/marketplace/install", h.install)
	r.Post("/v1/marketplace/upload", h.upload)
	r.Post("/v1/marketplace/import", h.importContainer)
	r.Get("/v1/marketplace/installed", h.listInstalled)
	r.Delete("/v1/marketplace/installed/{pack_id}", h.uninstall)
	r.Put("/v1/marketplace/installed/{pack_id}/bindings", h.setBindings)
	r.Get("/v1/marketplace/registries", h.registries)
}

type listResp struct {
	Items []*model.InstalledPack `json:"items"`
	Total int                    `json:"total"`
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func (h *Handler) install(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var src bizmp.Source
	if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	res, err := h.svc.Install(r.Context(), caller, src)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) listInstalled(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFromRequest(r)
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	items, err := h.svc.List(r.Context(), caller)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listResp{Items: items, Total: len(items)})
}

func (h *Handler) uninstall(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	packID := chi.URLParam(r, "pack_id")
	if packID == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	if err := h.svc.Uninstall(r.Context(), caller, packID); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setBindings(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	packID := chi.URLParam(r, "pack_id")
	if packID == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	var in struct {
		Bindings map[string]string `json:"bindings"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	if err := h.svc.SetBindings(r.Context(), caller, packID, in.Bindings); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) registries(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFromRequest(r)
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	got := h.svc.Registries(r.Context(), caller)
	writeJSON(w, http.StatusOK, got)
}

// --- helpers ----------------------------------------------------------

func callerFromRequest(r *http.Request) (bizmp.Caller, bool) {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		return bizmp.Caller{}, false
	}
	return bizmp.Caller{UserID: t.UserID, Role: t.Role}, true
}

func requireAdmin(w http.ResponseWriter, r *http.Request) (bizmp.Caller, bool) {
	c, ok := callerFromRequest(r)
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return bizmp.Caller{}, false
	}
	if c.Role != "admin" {
		writeErr(w, errs.ErrForbidden)
		return bizmp.Caller{}, false
	}
	return c, true
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
	code, slug := mapErr(err)
	writeJSON(w, code, errorBody{Error: err.Error(), Code: slug})
}

func mapErr(err error) (int, string) {
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, errs.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, errs.ErrNotFound):
		return http.StatusNotFound, "not-found"
	case errors.Is(err, errs.ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, errs.ErrInvalid):
		return http.StatusBadRequest, "invalid-argument"
	case errors.As(err, new(notWiredError)):
		return http.StatusServiceUnavailable, "not-wired"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

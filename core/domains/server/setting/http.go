// Package setting exposes /v1/system-settings — the admin-editable
// runtime configuration store. Reads are open to any authenticated user
// (sensitive values are always masked); writes require admin role.
//
// LLM API keys are write-only: they may be replaced through the settings
// API but are never returned to a browser, even to an administrator.
package setting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizsetting "github.com/vincent-wuhan/opskeeper/core/domains/biz/setting"
)

// SettingService is the narrow surface the handler depends on. The
// concrete impl is biz/setting.Service.
type SettingService interface {
	Get(ctx context.Context, category, key string) (string, bool, error)
	Set(ctx context.Context, category, key, value string, sensitive bool) error
	List(ctx context.Context, category string) ([]bizsetting.SettingDTO, error)
	Delete(ctx context.Context, category, key string) error
}

type Handler struct {
	svc SettingService
}

// NewHandler builds a handler serving the /v1/system-settings surface.
func NewHandler(svc SettingService) *Handler { return &Handler{svc: svc} }

// Register attaches the routes under a chi.Router that already has the
// auth middleware in front of it (see cmd/opskeeper).
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/system-settings", h.list)
	r.Put("/v1/system-settings/{category}/{key}", h.put)
	r.Delete("/v1/system-settings/{category}/{key}", h.delete)
	r.Get("/v1/system-settings/{category}/{key}/reveal", h.reveal)
}

type listResp struct {
	Items []bizsetting.SettingDTO `json:"items"`
	Total int                     `json:"total"`
}

type putReq struct {
	Value     string `json:"value"`
	Sensitive *bool  `json:"sensitive,omitempty"`
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := callerFromRequest(r); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	category := r.URL.Query().Get("category")
	items, err := h.svc.List(r.Context(), category)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listResp{Items: items, Total: len(items)})
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	category := chi.URLParam(r, "category")
	key := chi.URLParam(r, "key")
	if category == "" || key == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	var req putReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	sensitive := false
	if req.Sensitive != nil {
		sensitive = *req.Sensitive
	} else {
		// Auto-mark known sensitive keys so the UI doesn't have to opt-in
		// every time. The masking logic relies on this flag being correct.
		sensitive = isSensitiveKey(category, key)
	}
	if err := h.svc.Set(r.Context(), category, key, req.Value, sensitive); err != nil {
		writeErr(w, err)
		return
	}
	// A sensitive value contributes a digest and a length, never any part of
	// itself. The earlier code took the first four characters, guarded by
	// `len(value) > 4` — which means a secret of four characters or fewer was
	// written to the chain in full, and a longer one still leaked its opening
	// four into a signed log that every reader of the audit table can see.
	// The digest answers the question the prefix was there to answer
	// ("did this change?") without holding anything.
	payload := map[string]any{"category": category, "key": key, "sensitive": sensitive}
	if sensitive {
		payload["value_digest"] = auditport.ValueDigest(req.Value)
		payload["value_len"] = len(req.Value)
	} else {
		hint := req.Value
		if len(hint) > 64 {
			hint = hint[:64] + "…"
		}
		payload["value_hint"] = hint
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionSettingUpdate,
		ResourceType: auditport.ResourceSetting,
		ResourceID:   category + "/" + key,
		Status:       auditport.StatusSuccess,
		Payload:      payload,
	})
	// Return the freshly-masked row so the UI can update its cell without
	// re-listing the whole category.
	items, err := h.svc.List(r.Context(), category)
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, it := range items {
		if it.Key == key {
			writeJSON(w, http.StatusOK, it)
			return
		}
	}
	writeJSON(w, http.StatusOK, bizsetting.SettingDTO{Category: category, Key: key, Sensitive: sensitive})
}

// reveal returns the cleartext value for a single non-LLM credential row.
// Admin-only. LLM API keys are rejected before the service lookup so no
// browser session can retrieve a model provider key.
//
// We return the value alone (not the row) so the response is small and
// the caller can keep the masked DTO from /v1/system-settings as the
// authoritative "is sensitive" / "updated_at" record.
func (h *Handler) reveal(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	category := chi.URLParam(r, "category")
	key := chi.URLParam(r, "key")
	if category == "" || key == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	if category == "llm" && isSensitiveKey(category, key) {
		writeErr(w, errs.ErrForbidden)
		return
	}
	v, found, err := h.svc.Get(r.Context(), category, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !found {
		writeErr(w, errs.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"value": v})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	category := chi.URLParam(r, "category")
	key := chi.URLParam(r, "key")
	if category == "" || key == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	if err := h.svc.Delete(r.Context(), category, key); err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionSettingDelete,
		ResourceType: auditport.ResourceSetting,
		ResourceID:   category + "/" + key,
		Status:       auditport.StatusSuccess,
	})
	w.WriteHeader(http.StatusNoContent)
}

// isSensitiveKey is the default-mask policy: any *_api_key, *_secret,
// *_token, *_password key is treated as sensitive. Operators can still
// override via the request body's `sensitive` field.
func isSensitiveKey(_ string, key string) bool {
	for _, suffix := range []string{"_api_key", "_secret", "_token", "_password"} {
		if hasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

func hasSuffix(s, suf string) bool {
	if len(s) < len(suf) {
		return false
	}
	return s[len(s)-len(suf):] == suf
}

type caller struct {
	UserID uint64
	Role   string
}

func callerFromRequest(r *http.Request) (caller, bool) {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		return caller{}, false
	}
	return caller{UserID: t.UserID, Role: t.Role}, true
}

func requireAdmin(w http.ResponseWriter, r *http.Request) (caller, bool) {
	c, ok := callerFromRequest(r)
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return caller{}, false
	}
	if c.Role != tenantctx.RoleAdmin {
		writeErr(w, errs.ErrForbidden)
		return caller{}, false
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
	code := errCode(err)
	body := errorBody{Error: err.Error(), Code: errSlug(err)}
	writeJSON(w, code, body)
}

func errCode(err error) int {
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, errs.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, errs.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, errs.ErrInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func errSlug(err error) string {
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, errs.ErrForbidden):
		return "forbidden"
	case errors.Is(err, errs.ErrNotFound):
		return "not-found"
	case errors.Is(err, errs.ErrInvalid):
		return "invalid-argument"
	default:
		return "internal"
	}
}

// Package secret is the HTTP surface for the generic secret vault
// (HLD-017). All routes are admin-only; values are write-only — the list
// API returns redacted views (has_value) and never the secret material.
package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizsecret "github.com/vincent-wuhan/opskeeper/core/domains/biz/secret"
)

// Handler serves /v1/secrets.
type Handler struct{ uc *bizsecret.Usecase }

// NewHandler wires the usecase.
func NewHandler(uc *bizsecret.Usecase) *Handler { return &Handler{uc: uc} }

// Register attaches routes under a chi.Router that already has auth in
// front of it.
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/secrets", h.list)
	r.Post("/v1/secrets", h.create)
	r.Put("/v1/secrets/{id}", h.update)
	r.Delete("/v1/secrets/{id}", h.del)
	r.Get("/v1/credential-types", h.types)
}

// types lists the reusable credential types (fields + which type injects how)
// so the create-credential UI can render the right form.
func (h *Handler) types(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": bizsecret.AllCredTypes()})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	items, err := h.uc.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		// The refusal itself is a row. "Somebody kept trying" and "nobody
		// tried" have to look different on the chain, and requireAdmin has
		// already written the 403 by the time we get here.
		auditWrite(r, auditport.ActionSecretCreate, "", nil, errs.ErrForbidden)
		return
	}
	var in struct {
		Name        string            `json:"name"`
		Type        string            `json:"type"`
		Description string            `json:"description"`
		Fields      map[string]string `json:"fields"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		auditWrite(r, auditport.ActionSecretCreate, "", nil, err)
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	v, err := h.uc.Create(r.Context(), in.Name, in.Type, in.Description, in.Fields)
	if err != nil {
		auditWrite(r, auditport.ActionSecretCreate, "", secretPayload(in.Name, in.Type, in.Fields), err)
		writeErr(w, err)
		return
	}
	payload := secretPayload(in.Name, in.Type, in.Fields)
	payload["name"] = v.Name
	payload["type"] = v.Type
	auditWrite(r, auditport.ActionSecretCreate, strconv.FormatUint(v.ID, 10), payload, nil)
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		auditWrite(r, auditport.ActionSecretUpdate, chi.URLParam(r, "id"), nil, errs.ErrForbidden)
		return
	}
	id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, errs.ErrInvalid)
		return
	}
	var in struct {
		Description string            `json:"description"`
		Fields      map[string]string `json:"fields"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		auditWrite(r, auditport.ActionSecretUpdate, strconv.FormatUint(id, 10), nil, err)
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	if err := h.uc.Update(r.Context(), id, in.Description, in.Fields); err != nil {
		auditWrite(r, auditport.ActionSecretUpdate, strconv.FormatUint(id, 10),
			secretPayload("", "", in.Fields), err)
		writeErr(w, err)
		return
	}
	payload := secretPayload("", "", in.Fields)
	auditWrite(r, auditport.ActionSecretUpdate, strconv.FormatUint(id, 10), payload, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) del(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		auditWrite(r, auditport.ActionSecretDelete, chi.URLParam(r, "id"), nil, errs.ErrForbidden)
		return
	}
	id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		auditWrite(r, auditport.ActionSecretDelete, "", nil, err)
		writeErr(w, errs.ErrInvalid)
		return
	}
	if err := h.uc.Delete(r.Context(), id); err != nil {
		auditWrite(r, auditport.ActionSecretDelete, strconv.FormatUint(id, 10), nil, err)
		writeErr(w, err)
		return
	}
	auditWrite(r, auditport.ActionSecretDelete, strconv.FormatUint(id, 10), nil, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// auditWrite records one vault write.
//
// The payload is the whole reason this function exists in this shape. The
// body of a create or update carries `fields`, a map of plaintext credentials
// — a database password, an API token — and an audit chain is the last place
// that should ever hold one. So the payload carries the field *names* and a
// digest of the values, never the values: fields_digest answers "was this
// credential rotated, and is it the same one as last quarter" by comparing
// hashes, which needs no plaintext to compute and no plaintext to read.
//
// A digest is not a substitute for a secret being secret. It is over a
// credential an operator typed, so it is not brute-forceable in the way a
// password hash of a weak password is — but it is still a stable identifier
// for "this exact value", and that is exactly the property needed and no
// more.
func auditWrite(r *http.Request, action, id string, payload map[string]any, cause error) {
	status := auditport.StatusSuccess
	if cause != nil {
		status = auditport.StatusFailure
	}
	ev := auditport.Event{
		Action:       action,
		ResourceType: auditport.ResourceSecret,
		ResourceID:   id,
		Status:       status,
		Payload:      payload,
	}
	if cause != nil {
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
}

// secretPayload describes a set of credential fields without holding any of
// them. field_names says which keys were written; fields_digest is a SHA-256
// over the sorted key=value pairs, so two payloads can be compared for
// equality without either of them being readable.
func secretPayload(name, credType string, fields map[string]string) map[string]any {
	payload := map[string]any{}
	if name != "" {
		payload["name"] = name
	}
	if credType != "" {
		payload["type"] = credType
	}
	if len(fields) == 0 {
		return payload
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	payload["field_names"] = keys

	digest := sha256.New()
	for _, k := range keys {
		// The separator matters: without it {"ab":"c"} and {"a":"bc"} would
		// hash the same, and a rotated credential could look unchanged.
		digest.Write([]byte(k))
		digest.Write([]byte{0})
		digest.Write([]byte(fields[k]))
		digest.Write([]byte{0})
	}
	payload["fields_digest"] = hex.EncodeToString(digest.Sum(nil))
	return payload
}

// --- auth + json helpers (mirrors server/setting) ---

type caller struct {
	UserID uint64
	Role   string
}

func requireAdmin(w http.ResponseWriter, r *http.Request) (caller, bool) {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return caller{}, false
	}
	if t.Role != tenantctx.RoleAdmin {
		writeErr(w, errs.ErrForbidden)
		return caller{}, false
	}
	return caller{UserID: t.UserID, Role: t.Role}, true
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
	writeJSON(w, errCode(err), errorBody{Error: err.Error(), Code: errSlug(err)})
}

func errCode(err error) int {
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, errs.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, errs.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, errs.ErrConflict):
		return http.StatusConflict
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
		return "not_found"
	case errors.Is(err, errs.ErrConflict):
		return "conflict"
	case errors.Is(err, errs.ErrInvalid):
		return "invalid"
	default:
		return "internal"
	}
}

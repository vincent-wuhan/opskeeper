// Package systemhealth exposes the platform health-check API.
package systemhealth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	healthsvc "github.com/vincent-wuhan/opskeeper/core/manager/service/systemhealth"
)

type HealthService interface {
	Check(ctx context.Context) (*healthsvc.Report, error)
}

type Handler struct {
	svc HealthService
}

func NewHandler(svc HealthService) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/system/health", h.check)
	r.Post("/v1/system/health/check", h.check)
}

func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if h.svc == nil {
		writeErr(w, errs.ErrNotWiredYet)
		return
	}
	report, err := h.svc.Check(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// requireAdmin is where the authorization for this route lives.
//
// It used to return an alertsvc.Caller, which the handler passed to Check and
// the service forwarded to two alert calls that both discard it. The check it
// performs did not move and does not depend on the return value: the role is
// read from the tenant context here, before the service is reached, and that
// is the only thing standing between an unauthenticated request and a report
// that enumerates the platform's configuration.
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
	case errors.Is(err, errs.ErrInvalid):
		status, slug = http.StatusBadRequest, "invalid"
	case errors.Is(err, errs.ErrNotFound):
		status, slug = http.StatusNotFound, "not-found"
	default:
		status, slug = http.StatusBadGateway, "upstream"
	}
	writeJSON(w, status, errorBody{Error: err.Error(), Code: slug})
}

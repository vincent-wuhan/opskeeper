package demo

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	bizdemo "github.com/vincent-wuhan/opskeeper/core/manager/biz/demo"
	servicedemo "github.com/vincent-wuhan/opskeeper/core/manager/service/demo"
)

const maxRequestBodyBytes = 16 << 10

type Service interface {
	Start(ctx context.Context, tenantID uint64, input servicedemo.StartScenarioInput) (*servicedemo.ScenarioStatus, error)
	Get(ctx context.Context, tenantID uint64, scenarioID, key string) (*servicedemo.ScenarioStatus, error)
	AdvanceWorkflow(ctx context.Context, tenantID uint64, scenarioID, key, stage string) (*servicedemo.ScenarioStatus, error)
	BusinessSnapshot(ctx context.Context, tenantID uint64, scenarioID, key, section string) (json.RawMessage, error)
	BusinessSnapshotBaseline(ctx context.Context, section string) (json.RawMessage, error)
	Approve(ctx context.Context, tenantID, incidentID uint64, input servicedemo.ApproveScenarioInput) (*servicedemo.ScenarioStatus, error)
}

type Handler struct {
	service Service
	token   string
}

func NewHandler(service Service, token string) *Handler {
	return &Handler{service: service, token: token}
}

func (h *Handler) Register(router chi.Router) {
	router.Group(func(versioned chi.Router) {
		versioned.Use(h.authMiddleware)
		versioned.Post("/v1/demo/scenarios/"+bizdemo.ScenarioID+"/start", h.start)
		versioned.Get("/v1/demo/scenarios/{idempotency_key}", h.status)
		versioned.Post("/v1/demo/scenarios/{idempotency_key}/workflow/{stage}", h.advanceWorkflow)
		versioned.Post("/v1/demo/incidents/{incident_id}/approve", h.approveScenario)
		versioned.Get("/v1/demo/scenarios/{idempotency_key}/business/{section}", h.business)
		versioned.Get("/v1/demo/business/{section}", h.businessBaseline)
	})
}

func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Opskeeper-Version") != "v1" {
			writeError(w, http.StatusBadRequest, "unsupported version", "unsupported_version")
			return
		}
		token := r.Header.Get("Authorization")
		if len(token) != len("Bearer ")+len(h.token) || subtle.ConstantTimeCompare([]byte(token), []byte("Bearer "+h.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid bearer token", "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	var input servicedemo.StartScenarioInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.ScenarioID == "" {
		input.ScenarioID = bizdemo.ScenarioID
	}
	status, err := h.service.Start(r.Context(), demoTenantID, input)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeData(w, http.StatusCreated, "scenario started", status)
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	status, err := h.service.Get(r.Context(), demoTenantID, bizdemo.ScenarioID, chi.URLParam(r, "idempotency_key"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeData(w, http.StatusOK, "success", status)
}

func (h *Handler) advanceWorkflow(w http.ResponseWriter, r *http.Request) {
	status, err := h.service.AdvanceWorkflow(
		r.Context(), demoTenantID, bizdemo.ScenarioID,
		chi.URLParam(r, "idempotency_key"), chi.URLParam(r, "stage"),
	)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeData(w, http.StatusOK, "workflow stage recorded", status)
}

func (h *Handler) approveScenario(w http.ResponseWriter, r *http.Request) {
	incidentID, err := strconv.ParseUint(chi.URLParam(r, "incident_id"), 10, 64)
	if err != nil || incidentID == 0 {
		writeError(w, http.StatusBadRequest, "invalid incident id", "invalid_request")
		return
	}
	var input servicedemo.ApproveScenarioInput
	if !decodeJSON(w, r, &input) {
		return
	}
	status, err := h.service.Approve(r.Context(), demoTenantID, incidentID, input)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeData(w, http.StatusOK, "scenario approval recorded", status)
}

func (h *Handler) business(w http.ResponseWriter, r *http.Request) {
	data, err := h.service.BusinessSnapshot(
		r.Context(), demoTenantID, bizdemo.ScenarioID,
		chi.URLParam(r, "idempotency_key"), chi.URLParam(r, "section"),
	)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeData(w, http.StatusOK, "success", data)
}

func (h *Handler) businessBaseline(w http.ResponseWriter, r *http.Request) {
	data, err := h.service.BusinessSnapshotBaseline(r.Context(), chi.URLParam(r, "section"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeData(w, http.StatusOK, "success", data)
}

const demoTenantID uint64 = 1

func decodeJSON(w http.ResponseWriter, r *http.Request, output any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request")
		return false
	}
	return true
}

func writeMappedError(w http.ResponseWriter, err error) {
	status, message, code := servicedemo.MapError(err)
	writeError(w, status, message, code)
}

func writeData(w http.ResponseWriter, status int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data"`
	}{Code: status, Message: message, Data: data})
}

func writeError(w http.ResponseWriter, status int, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		ErrorCode string `json:"error_code,omitempty"`
	}{Code: "error", Message: message, ErrorCode: code})
}

// Package agentteams 暴露 AgentTeams 集成的 HTTP 接口。
//
// 路由（绑定到 chi.Router）：
//
//	GET    /v1/state/{task_id}        — 读 MinIO state.json
//	PUT    /v1/state/{task_id}        — 写 MinIO state.json（CAS version）
//	POST   /v1/hitl/decide            — 上报 HITL 决策
//	GET    /v1/skills/{name}          — 提供 opskeeper SKILL.md 文件给 AgentTeams worker-sync
//
// 认证：依赖 middleware/auth.go 的 Bearer GatewayKey 中间件；ctx 里有 ResolvedIdentity。
package agentteams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/internal/agentteams"
	incidentcontrol "github.com/vincent-wuhan/opskeeper/internal/control/incident"
	knowledgebiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/knowledge"
	knowledgemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/knowledge"
	mcpauth "github.com/vincent-wuhan/opskeeper/internal/manager/server/mcp/middleware"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/auth"
)

// StateBackend 是 MinIO / state store 的接口。
type StateBackend interface {
	Get(ctx context.Context, taskID string) ([]byte, error)
	Put(ctx context.Context, taskID string, body []byte) error
}

type KnowledgeWriter interface {
	CreateManualDoc(ctx context.Context, in knowledgebiz.CreateManualDocInput) (*knowledgemodel.Doc, error)
}

type IncidentRecorder interface {
	Append(ctx context.Context, event incidentcontrol.Event) error
	ListIncident(ctx context.Context, tenantID, incidentID string) ([]incidentcontrol.Event, error)
}

// Handler 聚合路由依赖。
type Handler struct {
	backend   StateBackend
	log       *slog.Logger
	skillDir  string // SKILL.md 文件目录，e.g. plugins/opskeeper-teamharness/skills
	knowledge KnowledgeWriter
	incident  IncidentRecorder
}

// NewHandler 构造。
func NewHandler(backend StateBackend, log *slog.Logger, skillDir string) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{backend: backend, log: log, skillDir: skillDir}
}

func (h *Handler) SetKnowledgeWriter(writer KnowledgeWriter) {
	h.knowledge = writer
}

func (h *Handler) SetIncidentRecorder(recorder IncidentRecorder) {
	h.incident = recorder
}

// Register 注册路由到 chi.Router。
//
// 调用方应在 Register 之前先注册 mcpauth.Authenticator.Middleware。
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/state/{task_id}", h.getState)
	r.Put("/v1/state/{task_id}", h.putState)
	r.Post("/v1/hitl/decide", h.hitlDecide)
	r.Post("/v1/knowledge/docs", h.createKnowledgeDoc)
	r.Post("/v1/incidents/events", h.recordIncidentEvent)
	r.Get("/v1/skills/{name}", h.getSkill)
}

func (h *Handler) getState(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "task_id")
	if taskID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing task_id")
		return
	}
	body, err := h.backend.Get(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, agentteams.ErrStateNotFound) {
			writeJSONError(w, http.StatusNotFound, "state not found")
			return
		}
		h.log.Warn("getState failed", "task_id", taskID, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *Handler) putState(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "task_id")
	if taskID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing task_id")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	probe["task_id"] = taskID
	// Inject LoongSuite / W3C trace context (如 plugin stdio MCP 已透传)
	// 这样 state.json 自带 trace_id，可与 LoongSuite / Tempo trace 关联。
	if tc, ok := mcpauth.TraceFromContext(r.Context()); ok && tc.HasTrace() {
		probe["trace_id"] = tc.TraceID
		if tc.SpanID != "" {
			probe["span_id"] = tc.SpanID
		}
	}
	out, err := json.Marshal(probe)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "re-marshal: "+err.Error())
		return
	}
	if err := h.backend.Put(r.Context(), taskID, out); err != nil {
		h.log.Warn("putState failed", "task_id", taskID, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (h *Handler) hitlDecide(w http.ResponseWriter, r *http.Request) {
	id, ok := mcpauth.FromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "no resolved identity")
		return
	}
	// 透传 trace context 到 audit log（如果有）
	tc, _ := mcpauth.TraceFromContext(r.Context())
	var req struct {
		TaskID   string   `json:"task_id"`
		Decision string   `json:"decision"`
		Signers  []string `json:"signers"`
		Reason   string   `json:"reason,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.TaskID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing task_id")
		return
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		writeJSONError(w, http.StatusBadRequest, "decision must be approve or reject")
		return
	}

	raw, err := h.backend.Get(r.Context(), req.TaskID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "state not found")
		return
	}
	var state agentteams.State
	if err := json.Unmarshal(raw, &state); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unmarshal state: "+err.Error())
		return
	}
	if state.HITL == nil {
		state.HITL = &agentteams.HITLRecord{}
	}
	now := time.Now()
	state.HITL.Decision = req.Decision
	state.HITL.Signers = req.Signers
	state.HITL.Reason = req.Reason
	state.HITL.DecidedAt = &now
	state.Audit = append(state.Audit, agentteams.AuditEvent{
		Event:   "hitl_decision",
		Actor:   id.ConsumerName,
		Reason:  fmt.Sprintf("decision=%s signers=%v reason=%s", req.Decision, req.Signers, req.Reason),
		At:      now,
		TraceID: tc.TraceID,
	})
	state.UpdatedAt = now
	state.Version++

	out, _ := json.Marshal(state)
	if err := h.backend.Put(r.Context(), req.TaskID, out); err != nil {
		h.log.Warn("hitlDecide put failed", "task_id", req.TaskID, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task_id":  req.TaskID,
		"decision": req.Decision,
		"version":  state.Version,
	})
}

func (h *Handler) getSkill(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" || strings.Contains(name, "..") {
		writeJSONError(w, http.StatusBadRequest, "invalid skill name")
		return
	}
	candidates := []string{
		h.skillDir + "/agent/" + name + "/SKILL.md",
		h.skillDir + "/team/" + name + "/SKILL.md",
	}
	for _, path := range candidates {
		if data, err := os.ReadFile(path); err == nil {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
	}
	writeJSONError(w, http.StatusNotFound, "skill not found")
}

type createKnowledgeDocReq struct {
	Title       string   `json:"title"`
	TitleEN     string   `json:"title_en,omitempty"`
	Content     string   `json:"content"`
	URL         string   `json:"url,omitempty"`
	Path        string   `json:"path,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Source      string   `json:"source,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
}

type knowledgeDocDTO struct {
	ID           uint64    `json:"id,string"`
	SourceType   string    `json:"source_type"`
	TenantScopes []string  `json:"tenant_scopes,omitempty"`
	URL          string    `json:"url,omitempty"`
	Title        string    `json:"title"`
	TitleEN      string    `json:"title_en,omitempty"`
	Content      string    `json:"content"`
	Path         string    `json:"path,omitempty"`
	Tags         []string  `json:"tags,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (h *Handler) createKnowledgeDoc(w http.ResponseWriter, r *http.Request) {
	identity, ok := mcpauth.FromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "no resolved identity")
		return
	}
	if !auth.AgentTeamsRoleAllows(identity.Role, "knowledge.write") {
		writeJSONError(w, http.StatusForbidden, "role not allowed to write knowledge")
		return
	}
	if h.knowledge == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "knowledge service unavailable")
		return
	}

	var req createKnowledgeDocReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	tags := append([]string{}, req.Tags...)
	if req.Source != "" {
		tags = append(tags, "source:"+req.Source)
	}
	if req.Fingerprint != "" {
		tags = append(tags, "fingerprint:"+req.Fingerprint)
	}

	doc, err := h.knowledge.CreateManualDoc(r.Context(), knowledgebiz.CreateManualDocInput{
		TenantID: identity.TenantID,
		Title:    req.Title,
		TitleEN:  req.TitleEN,
		Content:  req.Content,
		URL:      req.URL,
		Path:     req.Path,
		Tags:     tags,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := knowledgeDocDTO{
		ID: doc.ID, SourceType: doc.SourceType, TenantScopes: doc.TenantScopes,
		URL: doc.URL, Title: doc.Title, TitleEN: doc.TitleEN, Content: doc.Content,
		Path: doc.Path, Tags: doc.Tags, CreatedAt: doc.CreatedAt, UpdatedAt: doc.UpdatedAt,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(out)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

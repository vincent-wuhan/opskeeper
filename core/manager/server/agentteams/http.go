// Package agentteams 暴露 AgentTeams 集成的 HTTP 接口。
//
// 路由（绑定到 chi.Router）：
//
//	GET    /v1/state/{task_id}        — 读 MinIO state.json
//	PUT    /v1/state/{task_id}        — 写 MinIO state.json（CAS version）
//	POST   /v1/hitl/decide            — 上报 HITL 决策
//	GET    /v1/incidents/{incident_id}/events — 读取事故持久时间线
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

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/auth"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	incidentcontrol "github.com/vincent-wuhan/opskeeper/core/domains/control/incident"
	"github.com/vincent-wuhan/opskeeper/core/manager/agentteams"
	knowledgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/knowledge"
	knowledgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/knowledge"
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

// MCPCallerLookup is this package's own port, and it names nothing from the
// mcp domain.
//
// The shape it replaces was three package-level functions called directly:
// mcpauth.FromContext, mcpauth.TraceFromContext and TraceContext.HasTrace.
// They could not be anything else — a package-level function cannot be
// injected, so the dependency was structural rather than declared, and
// "who is calling" arrived as a six-field credential struct with an API key id
// and a resolution timestamp attached.
//
// What these routes ask is narrower. Three of the six fields decide anything:
// the consumer name is the actor written to audit, the role decides whether
// the route answers at all, and the tenant id partitions every read and write.
// And the two trace signals were one signal wearing two hats — a TraceContext
// is always present once the middleware has run, and its TraceID may still be
// empty, so every call site asked ok && tc.HasTrace() and the projection
// folds that into the bool.
//
// middleware.ContextIdentity satisfies this structurally; cmd/opskeeper hands
// one over. Neither domain names the other.
type MCPCallerLookup interface {
	// CallerFrom returns who is calling. False means nobody was resolved,
	// which these routes answer 401.
	CallerFrom(ctx context.Context) (domain.MCPCaller, bool)
	// TraceFrom returns the trace to correlate with. False means there is
	// none — not "there is an empty one", which the old two-signal shape
	// made expressible.
	TraceFrom(ctx context.Context) (domain.MCPTrace, bool)
}

// AlertIncidentResolver is this package's own port, and it names nothing
// from the alert domain.
//
// The shape it had — a filter struct and a twenty-five-column entity, both
// declared over there — was the whole content of the `agentteams -> alert`
// edge. What this handler actually does is narrower by a wide margin: when a
// run reports recovery, find the still-open alert incident carrying that
// incident id in its labels and close it. Two columns answer that, and
// "open" is not a filter this caller sets but a precondition of the question,
// so it is in the method name instead.
//
// The projection is domain.OpenAlert rather than a type declared here, because
// the alert side has to build one too and the two declarations must not be
// able to drift. alert.OpenAlertResolver satisfies this structurally;
// cmd/opskeeper hands one over. Neither domain names the other.
type AlertIncidentResolver interface {
	ListOpenAlerts(ctx context.Context, limit int) ([]domain.OpenAlert, error)
	SystemResolveIncident(ctx context.Context, dedupeKey, reason string, occurredAt time.Time) (bool, error)
}

// Handler 聚合路由依赖。
type Handler struct {
	backend   StateBackend
	log       *slog.Logger
	skillDir  string // SKILL.md 文件目录，e.g. plugins/opskeeper-teamharness/skills
	knowledge KnowledgeWriter
	incident  IncidentRecorder
	alerts    AlertIncidentResolver
	callers   MCPCallerLookup
}

// caller is the nil-safe read of the port. A handler with no lookup wired has
// no way to know who is asking, and the honest answer to that is the same one
// an unauthenticated request gets — not a panic on the first call, and not a
// route that quietly serves everyone.
func (h *Handler) caller(ctx context.Context) (domain.MCPCaller, bool) {
	if h.callers == nil {
		return domain.MCPCaller{}, false
	}
	return h.callers.CallerFrom(ctx)
}

// trace is the nil-safe read of the port, for the same reason as caller.
func (h *Handler) trace(ctx context.Context) (domain.MCPTrace, bool) {
	if h.callers == nil {
		return domain.MCPTrace{}, false
	}
	return h.callers.TraceFrom(ctx)
}

// NewHandler 构造。
//
// callers is a parameter and not a setter on purpose. It is the one
// dependency here that is unconditional — the other three are all optional, and
// a bot that has no knowledge writer is a degraded bot, not a broken one — and
// an unconditional dependency passed through a setter is a dependency a boot
// path can forget. As a parameter it is unfillable-by-omission: the compiler
// asks for it at every construction site, and the only way to get a handler
// that answers 401 everywhere is to pass nil deliberately.
//
// nil is still allowed, and still means 401, because a test may want to prove
// what that looks like. But it is now a decision rather than an oversight.
func NewHandler(backend StateBackend, log *slog.Logger, skillDir string, callers MCPCallerLookup) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{backend: backend, log: log, skillDir: skillDir, callers: callers}
}

func (h *Handler) SetKnowledgeWriter(writer KnowledgeWriter) {
	h.knowledge = writer
}

func (h *Handler) SetIncidentRecorder(recorder IncidentRecorder) {
	h.incident = recorder
}

func (h *Handler) SetAlertIncidentResolver(resolver AlertIncidentResolver) {
	h.alerts = resolver
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
	r.Get("/v1/incidents/{incident_id}/events", h.listIncidentEvents)
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
	if trace, ok := h.trace(r.Context()); ok {
		probe["trace_id"] = trace.TraceID
		if trace.SpanID != "" {
			probe["span_id"] = trace.SpanID
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

// hitlDecide records a human decision on a task that is waiting for one.
//
// 它此前**看起来已经审计了**：下面几行会把一条 `hitl_decision` 追加进
// `state.Audit`。那不是审计，两个理由都要说清楚：
//
//  1. `state.Audit` 在这个任务状态 blob 里，而这个 blob **由同一个 handler
//     重写**。一份能被写入者自己覆盖的记录，证明不了任何事——
//     `core/base/pkg/audit` 的 Verifier 之所以独立于 Sink，理由就是这个。
//  2. 全仓没有任何代码读 `state.Audit`：它是只写字段。
//
// 所以决定必须落在宿主的防篡改链上。
//
// 这里用**一个** `hitl_decide` 动作而不是像决策 309 那样分成 approve/reject，
// 理由与那条不同，值得写下来：决策 309 分两个是因为批准与驳回的**形状不同**
// （批准会调 executor 并带回执行结果，驳回不带执行）。这条 handler 不执行任何东西，
// 它只是把一个决定写进任务状态，于是「批准还是驳回」是同一种事件的子形态——
// 而 `port.go` 的约定正是把子形态放进载荷，不让动作名发散。
// 附带的好处是：鉴权失败发生在解码请求体**之前**，那时根本不知道 decision 是什么，
// 一个动作名在这里才写得出来。
// auditHITL puts one HITL decision attempt on the host chain.
//
// payload 里带 reason 与 signers：ADR-019 的双签设计意味着**谁签的**是这件事的一半，
// 只记「某人批准了」会让双签退化成一个签名。version 一起带上，因为它是这次写入
// 落到状态里的并发位置——同一个任务被两次决定时，version 说得出哪一次在前。
func auditHITL(r *http.Request, action, taskID string, identity domain.MCPCaller, decision string, state *agentteams.State, cause error) {
	payload := map[string]any{}
	if taskID != "" {
		payload["task_id"] = taskID
	}
	if decision != "" {
		payload["decision"] = decision
	}
	if identity.Consumer != "" {
		payload["consumer"] = identity.Consumer
		payload["role"] = identity.Role
	}
	if identity.TenantID != "" {
		payload["tenant_id"] = identity.TenantID
	}
	if state != nil {
		if state.HITL != nil {
			if len(state.HITL.Signers) > 0 {
				payload["signers"] = state.HITL.Signers
			}
			if state.HITL.Reason != "" {
				payload["reason"] = state.HITL.Reason
			}
			if state.HITL.DecidedAt != nil {
				payload["decided_at"] = state.HITL.DecidedAt.UTC().Format(time.RFC3339)
			}
		}
		payload["version"] = state.Version
		if state.Phase != "" {
			payload["phase"] = state.Phase
		}
	}
	status := auditport.StatusSuccess
	if cause != nil {
		status = auditport.StatusFailure
		payload["error"] = cause.Error()
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       action,
		ResourceType: auditport.ResourceAgentTeamsTask,
		ResourceID:   taskID,
		Status:       status,
		Payload:      payload,
	})
}

func (h *Handler) hitlDecide(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.caller(r.Context())
	if !ok {
		auditHITL(r, auditport.ActionHITLDecide, "", identity, "", nil, errors.New("no resolved identity"))
		writeJSONError(w, http.StatusUnauthorized, "no resolved identity")
		return
	}
	// 透传 trace context 到 audit log（如果有）
	trace, _ := h.trace(r.Context())
	var req struct {
		TaskID   string   `json:"task_id"`
		Decision string   `json:"decision"`
		Signers  []string `json:"signers"`
		Reason   string   `json:"reason,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auditHITL(r, auditport.ActionHITLDecide, req.TaskID, identity, req.Decision, nil, errors.New("invalid json: "+err.Error()))
		writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.TaskID == "" {
		auditHITL(r, auditport.ActionHITLDecide, "", identity, req.Decision, nil, errors.New("missing task_id"))
		writeJSONError(w, http.StatusBadRequest, "missing task_id")
		return
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		auditHITL(r, auditport.ActionHITLDecide, req.TaskID, identity, req.Decision, nil,
			errors.New("decision must be approve or reject"))
		writeJSONError(w, http.StatusBadRequest, "decision must be approve or reject")
		return
	}

	raw, err := h.backend.Get(r.Context(), req.TaskID)
	if err != nil {
		// 任务不存在也要入账：不入账的话，「有人在反复批准一个不存在的任务」
		// 与「没人试过」在链上长得一样。
		auditHITL(r, auditport.ActionHITLDecide, req.TaskID, identity, req.Decision, nil, errors.New("state not found"))
		writeJSONError(w, http.StatusNotFound, "state not found")
		return
	}
	var state agentteams.State
	if err := json.Unmarshal(raw, &state); err != nil {
		auditHITL(r, auditport.ActionHITLDecide, req.TaskID, identity, req.Decision, nil,
			fmt.Errorf("unmarshal state: %w", err))
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
		Actor:   identity.Consumer,
		Reason:  fmt.Sprintf("decision=%s signers=%v reason=%s", req.Decision, req.Signers, req.Reason),
		At:      now,
		TraceID: trace.TraceID,
	})
	state.UpdatedAt = now
	state.Version++

	out, _ := json.Marshal(state)
	if err := h.backend.Put(r.Context(), req.TaskID, out); err != nil {
		h.log.Warn("hitlDecide put failed", "task_id", req.TaskID, "err", err.Error())
		auditHITL(r, auditport.ActionHITLDecide, req.TaskID, identity, req.Decision, &state, err)
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	auditHITL(r, auditport.ActionHITLDecide, req.TaskID, identity, req.Decision, &state, nil)
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
	identity, ok := h.caller(r.Context())
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

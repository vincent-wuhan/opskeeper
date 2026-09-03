// Package mcp exposes /v1/mcp/servers — the external MCP server
// registration CRUD + connection-probe surface (HLD-018). Every route is
// admin-only.
//
// Permissions (all requireAdmin):
//   - POST   /v1/mcp/servers            create
//   - GET    /v1/mcp/servers            list
//   - GET    /v1/mcp/servers/{id}       get
//   - PUT    /v1/mcp/servers/{id}       update
//   - DELETE /v1/mcp/servers/{id}       delete
//   - POST   /v1/mcp/servers/{id}/test  probe (initialize → tools/list)
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	auditbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/audit"
	loopbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/prom"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/mcp"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/mcpclient"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// Service is the narrow surface the handler depends on. *bizmcp.Usecase
// satisfies it structurally.
type Service interface {
	Create(ctx context.Context, s *model.Server) (*model.Server, error)
	Update(ctx context.Context, id uint64, patch *model.Server) error
	Delete(ctx context.Context, id uint64) error
	Get(ctx context.Context, id uint64) (*model.Server, error)
	List(ctx context.Context) ([]*model.Server, error)
	TestConnection(ctx context.Context, id uint64) ([]mcpclient.Tool, error)
}

// Handler bundles the MCP server routes.
type Handler struct {
	svc         Service
	loop        LoopService
	baseTools   []mcpclient.Tool
	invokeBase  BaseToolInvoker
	authorizer  *loopbiz.MCPAuthorizer
	authz       CasbinAuthorizer
	audit       AuditEmitter
	toolClasses map[string]string
}

// NewHandler builds the handler.
func NewHandler(svc Service) *Handler {
	return &Handler{svc: svc, authorizer: loopbiz.NewMCPAuthorizer(nil)}
}

type LoopService interface {
	Tools(ctx context.Context) []loopbiz.MCPTool
	Invoke(ctx context.Context, tenantID, name string, arguments json.RawMessage) (any, error)
}

type BaseToolInvoker func(ctx context.Context, tenantID, name string, arguments json.RawMessage) (json.RawMessage, error)

type CasbinAuthorizer interface {
	AllowAnyOrg(ctx context.Context, userID uint64, object, action string) bool
}

// Register attaches the routes under a chi.Router that already has the auth
// middleware in front of it (see cmd/opskeeper).
//
// Deprecated: prefer RegisterCRUD + RegisterJSONRPC for fine-grained auth.
func (h *Handler) Register(r chi.Router) {
	h.RegisterCRUD(r)
	h.RegisterJSONRPC(r)
}

// RegisterCRUD registers /v1/mcp/servers/* admin CRUD routes.
// Used by admin auth (cookie/JWT session) protected sub-router.
func (h *Handler) RegisterCRUD(r chi.Router) {
	r.Post("/v1/mcp/servers", h.create)
	r.Get("/v1/mcp/servers", h.list)
	r.Get("/v1/mcp/servers/{id}", h.get)
	r.Put("/v1/mcp/servers/{id}", h.update)
	r.Delete("/v1/mcp/servers/{id}", h.delete)
	r.Post("/v1/mcp/servers/{id}/test", h.test)
}

// RegisterJSONRPC registers /v1/mcp JSON-RPC endpoint.
// Used by Bearer GatewayKey auth middleware protected sub-router.
func (h *Handler) RegisterJSONRPC(r chi.Router) {
	r.Post("/v1/mcp", h.jsonRPC)
}

func (h *Handler) SetLoopTools(loop LoopService, tools []mcpclient.Tool, invokeBase BaseToolInvoker) error {
	if loop == nil {
		return errors.New("mcp: loop service is required")
	}
	h.loop = loop
	h.baseTools = tools
	h.invokeBase = invokeBase
	return nil
}

func (h *Handler) SetAuthz(authz CasbinAuthorizer) {
	h.authz = authz
}

func (h *Handler) SetAuditEmitter(audit AuditEmitter) {
	h.audit = audit
}

func (h *Handler) SetLoopToolMetadata(classes map[string]string) {
	h.toolClasses = make(map[string]string, len(classes))
	for name, class := range classes {
		h.toolClasses[name] = class
	}
}

// serverInput is the editable subset of model.Server accepted on create /
// update. Status / tools cache / timestamps are server-owned.
type serverInput struct {
	Name               string `json:"name"`
	Transport          string `json:"transport"`
	Endpoint           string `json:"endpoint"`
	Command            string `json:"command"`
	ArgsJSON           string `json:"args_json"`
	Credential         string `json:"credential"`
	HeaderTemplateJSON string `json:"header_template_json"`
	Trusted            bool   `json:"trusted"`
	Enabled            bool   `json:"enabled"`
}

func (in serverInput) toModel() *model.Server {
	return &model.Server{
		Name:               in.Name,
		Transport:          in.Transport,
		Endpoint:           in.Endpoint,
		Command:            in.Command,
		ArgsJSON:           in.ArgsJSON,
		Credential:         in.Credential,
		HeaderTemplateJSON: in.HeaderTemplateJSON,
		Trusted:            in.Trusted,
		Enabled:            in.Enabled,
	}
}

type listResp struct {
	Items []*model.Server `json:"items"`
	Total int             `json:"total"`
}

type testResp struct {
	Tools []mcpclient.Tool `json:"tools"`
	Count int              `json:"count"`
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var in serverInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	s := in.toModel()
	s.CreatedBy = caller.UserID
	out, err := h.svc.Create(r.Context(), s)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	items, err := h.svc.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listResp{Items: items, Total: len(items)})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	id, err := parseID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	s, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	id, err := parseID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in serverInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	if err := h.svc.Update(r.Context(), id, in.toModel()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	id, err := parseID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) test(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	id, err := parseID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	tools, err := h.svc.TestConnection(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, testResp{Tools: tools, Count: len(tools)})
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	status  int    `json:"-"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func (h *Handler) jsonRPC(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Opskeeper-Version") != "v1" {
		writeMCPJSON(w, http.StatusBadRequest, jsonRPCResponse{
			JSONRPC: "2.0",
			Error:   &jsonRPCError{Code: -32002, Message: "X-Opskeeper-Version must be v1"},
		})
		return
	}
	if _, ok := callerFromRequest(r); !ok {
		writeMCPJSON(w, http.StatusUnauthorized, jsonRPCResponse{
			JSONRPC: "2.0",
			Error:   &jsonRPCError{Code: -32001, Message: "unauthorized"},
		})
		return
	}
	if h.loop == nil {
		writeMCPJSON(w, http.StatusServiceUnavailable, jsonRPCResponse{
			JSONRPC: "2.0",
			Error:   &jsonRPCError{Code: -32003, Message: "loop tools unavailable"},
		})
		return
	}

	var request jsonRPCRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
		writeMCPResult(w, nil, nil, &jsonRPCError{Code: -32700, Message: "parse error"})
		return
	}
	if request.JSONRPC != "2.0" {
		writeMCPResult(w, request.ID, nil, &jsonRPCError{Code: -32600, Message: "invalid request"})
		return
	}

	switch request.Method {
	case "notifications/initialized":
		w.Header().Set("X-Opskeeper-Version", "v1")
		w.WriteHeader(http.StatusAccepted)
	case "initialize":
		w.Header().Set("Mcp-Session-Id", uuid.NewString())
		writeMCPResult(w, request.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "opskeeper", "version": "v1"},
		}, nil)
	case "tools/list":
		caller, _ := callerFromRequest(r)
		claimed, err := workerIdentityFromRequest(r)
		if err != nil {
			writeMCPResult(w, request.ID, nil, authorizationError(err))
			return
		}
		// 兼容 Worker stdio MCP server 不发 X-Opskeeper-Worker-Identity 头的情况：
		// claimed 为空时回退到 Higress 已经解析过的 signed 身份（来自 ctx）。
		if claimed == (loopbiz.MCPWorkerIdentity{}) && caller.AgentTeams != nil {
			claimed = loopbiz.MCPWorkerIdentity{
				TenantID: caller.AgentTeams.TenantID,
				Service:  caller.AgentTeams.Service,
				Worker:   caller.AgentTeams.Worker,
				Role:     caller.AgentTeams.Role,
			}
		}
		tools := make([]mcpclient.Tool, 0, len(h.baseTools)+3)
		for _, tool := range h.loop.Tools(r.Context()) {
			if !h.toolAllowedForCaller(r.Context(), caller, claimed, tool.Name) {
				continue
			}
			tools = append(tools, mcpclient.Tool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: tool.InputSchema,
			})
		}
		for _, tool := range h.baseTools {
			if h.toolAllowedForCaller(r.Context(), caller, claimed, tool.Name) {
				tools = append(tools, tool)
			}
		}
		writeMCPResult(w, request.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		// self-health observability: capture tool name + worker role +
		// wall-clock latency for every call, regardless of outcome.
		// Failure path also emits so 4xx/5xx show up as result="denied"/
		// "auth_failed"/"error" in agentteams_mcp_call_total.
		toolName, observedRole, callStart := extractMCPCallMetadata(r, request)
		result, auditLogID, rpcErr := h.callTool(r, request)
		callDur := time.Since(callStart).Seconds()
		switch {
		case rpcErr == nil:
			prom.ObserveAgentTeamsMCPCall(toolName, observedRole, callDur, nil, false, false)
		case rpcErr.status == http.StatusUnauthorized:
			prom.ObserveAgentTeamsMCPCall(toolName, observedRole, callDur, nil, true, false)
		case rpcErr.status == http.StatusForbidden:
			prom.ObserveAgentTeamsMCPCall(toolName, observedRole, callDur, nil, false, true)
		default:
			prom.ObserveAgentTeamsMCPCall(toolName, observedRole, callDur, errors.New(rpcErr.Message), false, false)
		}
		if rpcErr != nil {
			status := http.StatusOK
			if rpcErr.status != 0 {
				status = rpcErr.status
			}
			if auditLogID != "" {
				w.Header().Set("X-Opskeeper-Audit-ID", auditLogID)
			}
			writeMCPJSON(w, status, jsonRPCResponse{
				JSONRPC: "2.0", ID: request.ID, Error: rpcErr,
			})
			return
		}
		if auditLogID != "" {
			w.Header().Set("X-Opskeeper-Audit-ID", auditLogID)
		}
		writeMCPResult(w, request.ID, result, nil)
	default:
		writeMCPResult(w, request.ID, nil, &jsonRPCError{Code: -32601, Message: "method not found"})
	}
}

func (h *Handler) callTool(r *http.Request, request jsonRPCRequest) (any, string, *jsonRPCError) {
	var params toolsCallParams
	if len(request.Params) == 0 {
		return nil, "", &jsonRPCError{Code: -32602, Message: "invalid params"}
	}
	if err := json.Unmarshal(request.Params, &params); err != nil || params.Name == "" {
		return nil, "", &jsonRPCError{Code: -32602, Message: "invalid params"}
	}
	if params.Arguments == nil {
		params.Arguments = json.RawMessage("{}")
	}

	caller, _ := callerFromRequest(r)
	claimed, err := workerIdentityFromRequest(r)
	if err != nil {
		h.emitAuthorizationDenied(r, caller, loopbiz.MCPWorkerIdentity{}, params.Name, err)
		return nil, "", authorizationError(err)
	}
	// 兼容 Worker stdio MCP server 不发 X-Opskeeper-Worker-Identity 头的情况：
	// 当 claimed 为空时，回退到 Higress 已经解析过的 signed 身份（来自 ctx），
	// 这样单一身份 token（无 mTLS / 无附加身份声明）也能走通 authorizer。
	if claimed == (loopbiz.MCPWorkerIdentity{}) && caller.AgentTeams != nil {
		claimed = loopbiz.MCPWorkerIdentity{
			TenantID: caller.AgentTeams.TenantID,
			Service:  caller.AgentTeams.Service,
			Worker:   caller.AgentTeams.Worker,
			Role:     caller.AgentTeams.Role,
		}
	}
	if err := h.authorizeRBAC(r.Context(), caller, params.Name); err != nil {
		h.emitAuthorizationDenied(r, caller, claimed, params.Name, err)
		return nil, "", authorizationError(err)
	}
	if err := h.authorizer.Authorize(r.Context(), caller, claimed, params.Name); err != nil {
		h.emitAuthorizationDenied(r, caller, claimed, params.Name, err)
		return nil, "", authorizationError(err)
	}
	receipt := &AuditReceipt{}
	ctx := WithAuditReceipt(r.Context(), receipt)
	for _, tool := range h.loop.Tools(r.Context()) {
		if tool.Name == params.Name {
			if h.invokeBase != nil {
				output, err := h.invokeBase(ctx, tenantID(caller), params.Name, params.Arguments)
				if err != nil {
					return nil, receipt.ID(), toolError(err)
				}
				result, evidenceErr := mcpContentWithAudit(output, params.Name, params.Arguments, receipt.ID())
				if evidenceErr != nil {
					return nil, receipt.ID(), toolError(evidenceErr)
				}
				return result, receipt.ID(), nil
			}
			output, err := h.loop.Invoke(r.Context(), tenantID(caller), params.Name, params.Arguments)
			if err != nil {
				return nil, "", toolError(err)
			}
			return mcpContent(output), "", nil
		}
	}
	if h.invokeBase == nil {
		return nil, "", &jsonRPCError{Code: -32602, Message: "unknown tool"}
	}
	output, err := h.invokeBase(ctx, tenantID(caller), params.Name, params.Arguments)
	if err != nil {
		return nil, receipt.ID(), toolError(err)
	}
	result, evidenceErr := mcpContentWithAudit(output, params.Name, params.Arguments, receipt.ID())
	if evidenceErr != nil {
		return nil, receipt.ID(), toolError(evidenceErr)
	}
	return result, receipt.ID(), nil
}

func (h *Handler) authorizeRBAC(ctx context.Context, caller tenantctx.Tenant, tool string) error {
	if h.authz == nil || caller.AgentTeams != nil {
		return nil
	}
	if caller.UserID == 0 {
		return loopbiz.ErrMCPCasbinDenied
	}
	action := "read"
	switch h.toolClasses[tool] {
	case "write":
		action = "write"
	case "destructive":
		action = "destructive"
	}
	if caller.IsSuperuser || h.authz.AllowAnyOrg(ctx, caller.UserID, "mcp:tool", action) {
		return nil
	}
	return loopbiz.ErrMCPCasbinDenied
}

func (h *Handler) toolAllowedForCaller(ctx context.Context, caller tenantctx.Tenant, claimed loopbiz.MCPWorkerIdentity, name string) bool {
	if err := h.authorizeRBAC(ctx, caller, name); err != nil {
		return false
	}
	return len(h.authorizer.FilterToolNames(caller, claimed, []string{name})) == 1
}

func (h *Handler) emitAuthorizationDenied(r *http.Request, caller tenantctx.Tenant, claimed loopbiz.MCPWorkerIdentity, tool string, denyErr error) {
	if h == nil || h.audit == nil || denyErr == nil {
		return
	}
	userID := caller.UserID
	role := caller.Role
	if role == "" && caller.AgentTeams != nil {
		// audit_logs.role 列宽只有 varchar(16)；"agentteams:<role>" 形式对
		// investigator/verifier/repairer/reporter 都会超长被截断。改用
		// 短前缀 "at:<role>"（≤12 字符）。AgentTeams 完整身份走 payload.actor，
		// 不会丢失信息。
		role = "at:" + caller.AgentTeams.Role
	}
	payload := map[string]any{
		"tool":    tool,
		"reason":  denyErr.Error(),
		"claimed": claimed,
	}
	if caller.AgentTeams != nil {
		payload["actor"] = map[string]string{
			"tenant_id": caller.AgentTeams.TenantID,
			"service":   caller.AgentTeams.Service,
			"worker":    caller.AgentTeams.Worker,
			"role":      caller.AgentTeams.Role,
		}
	}
	h.audit.Emit(r.Context(), auditbiz.Event{
		UserID:       &userID,
		UserEmail:    caller.Email,
		Role:         role,
		Action:       "mcp_tool_authorize",
		ResourceType: "mcp_tool",
		ResourceID:   tool,
		ResourceName: tool,
		Status:       "denied",
		ErrorCode:    "authorization_denied",
		ErrorMessage: denyErr.Error(),
		RequestID:    r.Header.Get("X-Request-ID"),
		Payload:      payload,
	})
}

func mcpContent(output any) map[string]any {
	raw, _ := json.Marshal(output)
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": string(raw)}},
	}
}

// mcpContentWithAudit mirrors mcpContent but attaches an audit_log_id to the
// MCP result envelope so transports that ignore HTTP headers (e.g.
// AgentTeams' mcporter stdio bridge) can still surface the durable audit row
// ID. The AgentTeams Manager consumes this field and appends the ID to
// state.json.audit_refs. The HTTP header (X-Opskeeper-Audit-ID) remains the
// canonical transport for HTTP MCP clients; both are populated from the
// same AuditReceipt so they can never disagree.
func canonicalDigest(value any) (string, error) {
	if raw, ok := value.(json.RawMessage); ok {
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return "", fmt.Errorf("decode canonical MCP evidence value: %w", err)
		}
		value = decoded
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode canonical MCP evidence value: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func resultProjection(tool string, output any) any {
	raw, err := json.Marshal(output)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	fields := map[string][]string{
		"loop.correlate":   {"correlated_groups", "dedup_reason", "severity"},
		"loop.investigate": {"schema_version", "root_cause_object", "confidence"},
		"query_knowledge":  {"total", "truncated"},
		"recovery.execute": {"executed", "command"},
		"recovery.verify":  {"passed", "metrics_compared", "rollback_recommended"},
	}
	projection := make(map[string]any, len(fields[tool]))
	for _, field := range fields[tool] {
		if value, ok := payload[field]; ok {
			projection[field] = value
		}
	}
	return projection
}

func mcpContentWithAudit(output any, tool string, arguments json.RawMessage, auditLogID string) (map[string]any, error) {
	result := mcpContent(output)
	if auditLogID == "" {
		return result, nil
	}
	argumentsDigest, err := canonicalDigest(arguments)
	if err != nil {
		return nil, err
	}
	responseDigest, err := canonicalDigest(output)
	if err != nil {
		return nil, err
	}
	evidence := map[string]any{
		"audit_log_id":      auditLogID,
		"arguments_sha256":  argumentsDigest,
		"response_sha256":   responseDigest,
		"jsonrpc_success":   true,
		"result_projection": resultProjection(tool, output),
		"tool":              tool,
	}
	result["_meta"] = map[string]any{
		"audit_log_id":           auditLogID,
		"opskeeper_mcp_evidence": evidence,
	}
	result["structuredContent"] = map[string]any{
		"audit_log_id":           auditLogID,
		"opskeeper_mcp_evidence": evidence,
	}
	return result, nil
}

func toolError(err error) *jsonRPCError {
	if errors.Is(err, loopbiz.ErrMCPToolNotFound) {
		return &jsonRPCError{Code: -32602, Message: "unknown tool"}
	}
	if strings.Contains(strings.ToLower(err.Error()), "invalid arguments") {
		return &jsonRPCError{Code: -32602, Message: err.Error()}
	}
	return &jsonRPCError{Code: -32000, Message: fmt.Sprintf("tool execution failed: %v", err)}
}

func authorizationError(err error) *jsonRPCError {
	return &jsonRPCError{
		Code: -32010, Message: "mcp authorization denied: " + err.Error(), status: http.StatusForbidden,
	}
}

func workerIdentityFromRequest(r *http.Request) (loopbiz.MCPWorkerIdentity, error) {
	raw := strings.TrimSpace(r.Header.Get("X-Opskeeper-Worker-Identity"))
	if raw == "" {
		if caller, ok := callerFromRequest(r); ok && caller.AgentTeams != nil {
			return loopbiz.MCPWorkerIdentity{
				TenantID: caller.AgentTeams.TenantID,
				Service:  caller.AgentTeams.Service,
				Worker:   caller.AgentTeams.Worker,
				Role:     caller.AgentTeams.Role,
			}, nil
		}
		return loopbiz.MCPWorkerIdentity{}, nil
	}
	var identity loopbiz.MCPWorkerIdentity
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		return loopbiz.MCPWorkerIdentity{}, loopbiz.ErrMCPInvalidIdentityData
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF || len(trailing) != 0 {
		return loopbiz.MCPWorkerIdentity{}, loopbiz.ErrMCPInvalidIdentityData
	}
	if identity == (loopbiz.MCPWorkerIdentity{}) {
		return loopbiz.MCPWorkerIdentity{}, loopbiz.ErrMCPInvalidIdentityData
	}
	return identity, nil
}

// --- helpers ----------------------------------------------------------

func parseID(r *http.Request) (uint64, error) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, errs.ErrInvalid
	}
	return id, nil
}

func callerFromRequest(r *http.Request) (tenantctx.Tenant, bool) {
	return tenantctx.From(r.Context())
}

func requireAdmin(w http.ResponseWriter, r *http.Request) (tenantctx.Tenant, bool) {
	c, ok := callerFromRequest(r)
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return tenantctx.Tenant{}, false
	}
	if c.Role != "admin" {
		writeErr(w, errs.ErrForbidden)
		return tenantctx.Tenant{}, false
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

func writeMCPJSON(w http.ResponseWriter, code int, body jsonRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Opskeeper-Version", "v1")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func tenantID(caller tenantctx.Tenant) string {
	if caller.AgentTeams != nil && caller.AgentTeams.TenantID != "" {
		return caller.AgentTeams.TenantID
	}
	if caller.UserID == 0 {
		return "default"
	}
	return fmt.Sprint(caller.UserID)
}

func writeMCPResult(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *jsonRPCError) {
	if id == nil {
		id = json.RawMessage("null")
	}
	writeMCPJSON(w, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
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
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// extractMCPCallMetadata peeks at the JSON-RPC `tools/call` request to
// extract (tool_name, role, now) for self-health metric emission. Designed
// to be best-effort: any parse failure returns ("", "unknown", now) so the
// metric never blocks the dispatch.
//
// Best-effort because:
//   - params may be empty / malformed; we don't want to fail the call just
//     to label a metric.
//   - worker identity may not be set yet (auth middleware runs after this
//     function returns); we try to extract what we can from the request
//     and fall back to "unknown" for the role.
func extractMCPCallMetadata(r *http.Request, request jsonRPCRequest) (string, string, time.Time) {
	toolName := ""
	if len(request.Params) > 0 {
		var peek struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(request.Params, &peek) == nil {
			toolName = peek.Name
		}
	}
	if toolName == "" {
		toolName = "unknown"
	}
	role := "unknown"
	if claimed, idErr := workerIdentityFromRequest(r); idErr == nil {
		if claimed.Role != "" {
			role = claimed.Role
		} else if caller, _ := callerFromRequest(r); caller.AgentTeams != nil && caller.AgentTeams.Role != "" {
			role = caller.AgentTeams.Role
		}
	}
	return toolName, role, time.Now()
}

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	aiopstools "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools"
	aiopstoolsbase "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	auditbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/audit"
	knowledgebiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/knowledge"
	loopbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/knowledge"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/auth"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/mcpclient"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

type fakeKnowledgeSearcher func(context.Context, string, knowledgebiz.SearchOptions) ([]knowledgebiz.SearchHit, error)

func TestJSONRPC_InitializeToolsListAndCall(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	tests := []struct {
		name       string
		method     string
		params     string
		wantResult func(t *testing.T, body jsonRPCResponse)
	}{
		{
			name:   "initialize",
			method: "initialize",
			wantResult: func(t *testing.T, body jsonRPCResponse) {
				if body.Error != nil {
					t.Fatalf("initialize error = %+v", body.Error)
				}
				result, _ := body.Result.(map[string]any)
				serverInfo, _ := result["serverInfo"].(map[string]any)
				if serverInfo["name"] != "opskeeper" || serverInfo["version"] != "v1" {
					t.Fatalf("serverInfo = %+v, want opskeeper v1", serverInfo)
				}
			},
		},
		{
			name:   "tools list",
			method: "tools/list",
			wantResult: func(t *testing.T, body jsonRPCResponse) {
				if body.Error != nil {
					t.Fatalf("tools/list error = %+v", body.Error)
				}
				result, _ := body.Result.(map[string]any)
				tools, _ := result["tools"].([]any)
				if len(tools) != 3 {
					t.Fatalf("tools count = %d, want 3", len(tools))
				}
			},
		},
		{
			name:   "tools call",
			method: "tools/call",
			params: `{"name":"loop.correlate","arguments":{"raw_alerts":[{"alert_id":"a1","severity":"warn","resource":"pg:primary","detected_at":"2026-08-18T07:00:00Z"}],"window":"5m"}}`,
			wantResult: func(t *testing.T, body jsonRPCResponse) {
				if body.Error != nil {
					t.Fatalf("tools/call error = %+v", body.Error)
				}
				result, _ := body.Result.(map[string]any)
				if _, ok := result["content"]; !ok {
					t.Fatalf("missing MCP content: %+v", result)
				}
			},
		},
		{
			name:   "unknown method",
			method: "resources/list",
			wantResult: func(t *testing.T, body jsonRPCResponse) {
				if body.Error == nil || body.Error.Code != -32601 {
					t.Fatalf("error = %+v, want method not found", body.Error)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newJSONRPCRequest(t, test.method, test.params)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("X-Opskeeper-Version"); got != "v1" {
				t.Fatalf("version header = %q, want v1", got)
			}
			var body jsonRPCResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			test.wantResult(t, body)
		})
	}
}

func TestJSONRPC_InitializeReturnsMCPSession(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newJSONRPCRequest(t, "initialize", `{}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if sessionID := recorder.Header().Get("Mcp-Session-Id"); len(sessionID) < 32 {
		t.Fatalf("Mcp-Session-Id = %q, want a UUID-compatible session ID", sessionID)
	}
}

func TestJSONRPC_InitializedNotificationReturnsAccepted(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	request.Header.Set("X-Opskeeper-Version", "v1")
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 42, Role: "user"}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", recorder.Body.String())
	}
}

func TestJSONRPC_AgentTeamsSignedIdentityIsAuthoritative(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	claims := auth.AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-alerter", Role: "alerter",
		AllowedTools: []string{"loop.correlate"},
	}
	recorder := serveServiceJSONRPC(t, handler, claims, "", "loop.correlate")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestJSONRPC_RequiresAuthenticatedCaller(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("X-Opskeeper-Version", "v1")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if recorder.Header().Get("X-Opskeeper-Version") != "v1" {
		t.Fatal("missing X-Opskeeper-Version header")
	}
}

func TestJSONRPC_AgentTeamsServiceAuthorization(t *testing.T) {
	identityJSON := `{"tenant_id":"tenant-a","service":"agentteams","worker":"opskeeper-investigator","role":"investigator"}`
	tests := []struct {
		name        string
		claims      auth.AgentTeamsServiceClaims
		header      string
		tool        string
		wantAllowed bool
		wantReason  string
	}{
		{
			name: "allow matched investigator tool",
			claims: auth.AgentTeamsServiceClaims{
				TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
				AllowedTools: []string{"loop.correlate", "loop.investigate"},
			},
			header:      identityJSON,
			tool:        "loop.correlate",
			wantAllowed: true,
		},
		{
			name: "allow signed worker identity without duplicate header",
			claims: auth.AgentTeamsServiceClaims{
				TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
				AllowedTools: []string{"loop.correlate"},
			},
			tool:        "loop.correlate",
			wantAllowed: true,
		},
		{
			name: "reject mismatched worker identity",
			claims: auth.AgentTeamsServiceClaims{
				TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
				AllowedTools: []string{"loop.correlate"},
			},
			header:     `{"tenant_id":"tenant-a","service":"agentteams","worker":"opskeeper-verifier","role":"investigator"}`,
			tool:       "loop.correlate",
			wantReason: "mcp agentteams worker identity mismatch",
		},
		{
			name: "reject read-only worker calling restricted tool",
			claims: auth.AgentTeamsServiceClaims{
				TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
				AllowedTools: []string{"loop.correlate"},
			},
			header:     identityJSON,
			tool:       "recovery.verify",
			wantReason: "mcp agentteams role not allowed",
		},
		{
			name: "reject tool outside signed allowlist",
			claims: auth.AgentTeamsServiceClaims{
				TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
				AllowedTools: []string{"loop.investigate"},
			},
			header:     identityJSON,
			tool:       "loop.correlate",
			wantReason: "mcp agentteams tool not allowed",
		},
		{
			name: "reject malformed worker identity",
			claims: auth.AgentTeamsServiceClaims{
				TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
				AllowedTools: []string{"loop.correlate"},
			},
			header:     identityJSON + " garbage",
			tool:       "loop.correlate",
			wantReason: "mcp agentteams invalid identity",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := newJSONRPCHandler(t)
			recorder := serveServiceJSONRPC(t, handler, test.claims, test.header, test.tool)

			var body jsonRPCResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if test.wantAllowed {
				if body.Error != nil {
					t.Fatalf("authorized call error = %+v", body.Error)
				}
				return
			}
			if body.Error == nil || body.Error.Code != -32010 || !strings.Contains(body.Error.Message, test.wantReason) {
				t.Fatalf("error = %+v, want denied %q", body.Error, test.wantReason)
			}
			if recorder.Header().Get("X-Opskeeper-Version") != "v1" {
				t.Fatal("missing X-Opskeeper-Version header")
			}
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", recorder.Code)
			}
		})
	}
}

func TestJSONRPC_AgentTeamsAuthorizationUsesSignedTenant(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	recording := &recordingLoopService{}
	if err := loopHandler.SetLoopTools(recording, nil, nil); err != nil {
		t.Fatal(err)
	}
	claims := auth.AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
		AllowedTools: []string{"loop.correlate"},
	}
	identity := `{"tenant_id":"tenant-a","service":"agentteams","worker":"opskeeper-investigator","role":"investigator"}`
	recorder := serveServiceJSONRPC(t, handler, claims, identity, "loop.correlate")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recording.tenantID(); got != "tenant-a" {
		t.Fatalf("invoke tenant = %q, want tenant-a", got)
	}
}

func TestJSONRPC_QueryKnowledgeUsesSignedTenantAndAuditReceipt(t *testing.T) {
	type capturedSearch struct {
		tenantID string
		tags     []string
	}
	captured := make(chan capturedSearch, 1)
	handler, loopHandler := newJSONRPCHandler(t)
	var searcher fakeKnowledgeSearcher = func(_ context.Context, _ string, opts knowledgebiz.SearchOptions) ([]knowledgebiz.SearchHit, error) {
		captured <- capturedSearch{tenantID: opts.TenantID, tags: opts.Tags}
		return []knowledgebiz.SearchHit{{
			Doc: &model.Doc{ID: 1, Title: "DNS SOP", SourceType: "vault", Tags: opts.Tags},
		}}, nil
	}
	tool := aiopstools.NewQueryKnowledgeTool(searcher, nil)
	err := loopHandler.SetLoopTools(stubLoopService{}, []mcpclient.Tool{{
		Name:        "query_knowledge",
		Description: "Search knowledge base",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}, func(ctx context.Context, tenantID, _ string, arguments json.RawMessage) (json.RawMessage, error) {
		if receipt, ok := AuditReceiptFromContext(ctx); ok {
			receipt.SetID(889)
		}
		output, err := tool.InvokableRun(ctx, string(arguments), aiopstoolsbase.WithTenant(tenantID))
		if err != nil {
			return nil, err
		}
		return json.RawMessage(output), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := auth.AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
		AllowedTools: []string{"query_knowledge"},
	}
	recorder := serveServiceJSONRPCMethod(handler, claims, "tools/call",
		`{"name":"query_knowledge","arguments":{"query":"dns 排查","tags":["dns","dns"]}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	got := <-captured
	if got.tenantID != "tenant-a" || strings.Join(got.tags, ",") != "dns" {
		t.Fatalf("search tenant/tags = %+v, want tenant-a/[dns]", got)
	}
	if header := recorder.Header().Get("X-Opskeeper-Audit-ID"); header != "889" {
		t.Fatalf("audit header = %q, want 889", header)
	}
	var body jsonRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result, _ := body.Result.(map[string]any)
	structured, _ := result["structuredContent"].(map[string]any)
	if structured["audit_log_id"] != "889" {
		t.Fatalf("structured audit_log_id = %#v, want 889", structured["audit_log_id"])
	}
}

func (f fakeKnowledgeSearcher) Search(ctx context.Context, query string, opts knowledgebiz.SearchOptions) ([]knowledgebiz.SearchHit, error) {
	return f(ctx, query, opts)
}

func TestJSONRPC_ToolsListFiltersByWorkerIdentity(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	if err := loopHandler.SetLoopTools(stubLoopService{}, []mcpclient.Tool{{
		Name:        "query_knowledge",
		Description: "Search knowledge base",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}, nil); err != nil {
		t.Fatal(err)
	}
	claims := auth.AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
		AllowedTools: []string{"loop.investigate", "query_knowledge"},
	}
	recorder := serveServiceJSONRPCMethod(handler, claims, "tools/list", "")
	var body jsonRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result, _ := body.Result.(map[string]any)
	rawTools, _ := result["tools"].([]any)
	if len(rawTools) != 2 {
		t.Fatalf("tools = %+v, want loop.investigate and query_knowledge", rawTools)
	}
	names := make(map[string]bool, len(rawTools))
	for _, rawTool := range rawTools {
		tool, _ := rawTool.(map[string]any)
		names[tool["name"].(string)] = true
	}
	if !names["loop.investigate"] || !names["query_knowledge"] {
		t.Fatalf("tools = %+v, want loop.investigate and query_knowledge", rawTools)
	}
}

func TestJSONRPC_AuthorizationDenialEmitsAuditEvent(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	emitter := &recordingAuditEmitter{}
	loopHandler.SetAuditEmitter(emitter)
	claims := auth.AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
		AllowedTools: []string{"loop.investigate"},
	}
	recorder := serveServiceJSONRPC(t, handler, claims,
		`{"tenant_id":"tenant-a","service":"agentteams","worker":"opskeeper-verifier","role":"investigator"}`,
		"loop.investigate")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(emitter.events))
	}
	event := emitter.events[0]
	if event.Status != "denied" || event.ResourceID != "loop.investigate" {
		t.Fatalf("audit event = %+v, want denied loop.investigate", event)
	}
	payload, _ := event.Payload.(map[string]any)
	actor, _ := payload["actor"].(map[string]string)
	if actor["worker"] != "opskeeper-investigator" || actor["tenant_id"] != "tenant-a" {
		t.Fatalf("audit actor = %+v", actor)
	}
}

func TestJSONRPC_AgentTeamsProtectedToolsAndAuditDenial(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	if err := loopHandler.SetLoopTools(policyLoopService{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		tool string
		err  error
	}{
		{name: "alerter correlate", tool: "loop.correlate"},
		{name: "investigator investigate", tool: "loop.investigate"},
		{name: "verifier recovery", tool: "recovery.verify"},
		{
			name: "investigator mutating tool",
			tool: "recovery.execute",
			err:  loopbiz.ErrMCPMutatingNotAllowed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			loopHandler.authorizer = loopbiz.NewMCPAuthorizer(slog.New(slog.NewTextHandler(&logs, nil)))
			role := "investigator"
			allowed := []string{"loop.correlate", "loop.investigate"}
			switch test.tool {
			case "loop.correlate":
				role, allowed = "alerter", []string{"loop.correlate"}
			case "recovery.verify":
				role, allowed = "verifier", []string{"recovery.verify"}
			}
			claims := auth.AgentTeamsServiceClaims{
				TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-" + role, Role: role, AllowedTools: allowed,
			}
			identity := `{"tenant_id":"tenant-a","service":"agentteams","worker":"opskeeper-` + role + `","role":"` + role + `"}`
			recorder := serveServiceJSONRPC(t, handler, claims, identity, test.tool)
			var body jsonRPCResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if test.err == nil {
				if body.Error != nil {
					t.Fatalf("protected tool error = %+v", body.Error)
				}
				return
			}
			if !strings.Contains(body.Error.Message, test.err.Error()) {
				t.Fatalf("error = %+v, want %v", body.Error, test.err)
			}
			for _, field := range []string{"tenant_id=tenant-a", "service=agentteams", "tool=" + test.tool, "reason=\"" + test.err.Error() + "\""} {
				if !strings.Contains(logs.String(), field) {
					t.Fatalf("audit log %q missing %q", logs.String(), field)
				}
			}
		})
	}
}

func TestJSONRPC_ToolsListMergesBaseTools(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	if err := loopHandler.SetLoopTools(stubLoopService{}, []mcpclient.Tool{{
		Name:        "git.find_runtime_link",
		Description: "Find runtime link",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}, nil); err != nil {
		t.Fatal(err)
	}
	request := newJSONRPCRequest(t, "tools/list", "")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "git.find_runtime_link") {
		t.Fatalf("BaseTool missing from tools/list: %s", recorder.Body.String())
	}
}

func TestJSONRPC_LoopToolsPreferDecoratedBaseInvoker(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	called := make(chan string, 1)
	err := loopHandler.SetLoopTools(stubLoopService{}, nil, func(_ context.Context, tenantID, name string, _ json.RawMessage) (json.RawMessage, error) {
		called <- tenantID + ":" + name
		return json.RawMessage(`{"decorated":true}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newJSONRPCRequest(t, "tools/call", `{"name":"loop.correlate","arguments":{"raw_alerts":[],"window":"5m"}}`))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "decorated") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if got := <-called; got != "42:loop.correlate" {
		t.Fatalf("decorated invocation = %q", got)
	}
}

func TestJSONRPC_HumanToolsCallRequiresCasbin(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	loopHandler.SetAuthz(&allowCasbinTool{allow: false})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newJSONRPCRequest(t, "tools/call", `{"name":"loop.correlate","arguments":{"raw_alerts":[],"window":"5m"}}`))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), loopbiz.ErrMCPCasbinDenied.Error()) {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestJSONRPC_HumanToolsListFiltersByCasbin(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	authz := &allowCasbinTool{allow: false}
	loopHandler.SetAuthz(authz)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newJSONRPCRequest(t, "tools/list", ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if authz.action != "read" {
		t.Fatalf("Casbin action = %q, want read", authz.action)
	}
	var body jsonRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result, _ := body.Result.(map[string]any)
	rawTools, _ := result["tools"].([]any)
	if len(rawTools) != 0 {
		t.Fatalf("tools = %+v, want permission-filtered empty list", rawTools)
	}
}

func TestJSONRPC_ToolsCallReturnsAuditReceiptHeader(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	err := loopHandler.SetLoopTools(stubLoopService{}, nil, func(ctx context.Context, _, _ string, _ json.RawMessage) (json.RawMessage, error) {
		receipt, ok := AuditReceiptFromContext(ctx)
		if !ok {
			t.Fatal("audit receipt missing from decorated invoker context")
		}
		receipt.SetID(321)
		return json.RawMessage(`{"decorated":true}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newJSONRPCRequest(t, "tools/call", `{"name":"loop.correlate","arguments":{"raw_alerts":[],"window":"5m"}}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Opskeeper-Audit-ID"); got != "321" {
		t.Fatalf("X-Opskeeper-Audit-ID = %q, want 321", got)
	}
}

func TestJSONRPC_ToolsCallEmbedsAuditIDInResult(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	arguments := json.RawMessage(`{"raw_alerts":[],"window":"5m"}`)
	output := json.RawMessage(`{"correlated_groups":[],"dedup_reason":"same resource","severity":"critical"}`)
	err := loopHandler.SetLoopTools(stubLoopService{}, nil, func(ctx context.Context, _, _ string, _ json.RawMessage) (json.RawMessage, error) {
		receipt, ok := AuditReceiptFromContext(ctx)
		if !ok {
			t.Fatal("audit receipt missing from decorated invoker context")
		}
		receipt.SetID(654)
		return output, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newJSONRPCRequest(t, "tools/call", `{"name":"loop.correlate","arguments":{"raw_alerts":[],"window":"5m"}}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var body jsonRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result, _ := body.Result.(map[string]any)
	if id := result["structuredContent"].(map[string]any)["audit_log_id"]; id != "654" {
		t.Fatalf("structuredContent.audit_log_id = %v, want 654", id)
	}
	if id := result["_meta"].(map[string]any)["audit_log_id"]; id != "654" {
		t.Fatalf("_meta.audit_log_id = %v, want 654", id)
	}
	argumentsDigest, err := canonicalDigest(arguments)
	if err != nil {
		t.Fatalf("canonicalDigest(arguments): %v", err)
	}
	responseDigest, err := canonicalDigest(output)
	if err != nil {
		t.Fatalf("canonicalDigest(output): %v", err)
	}
	metaEvidence, _ := result["_meta"].(map[string]any)["opskeeper_mcp_evidence"].(map[string]any)
	structuredEvidence, _ := result["structuredContent"].(map[string]any)["opskeeper_mcp_evidence"].(map[string]any)
	if !reflect.DeepEqual(metaEvidence, structuredEvidence) {
		t.Fatalf("evidence copies disagree: _meta=%+v structuredContent=%+v", metaEvidence, structuredEvidence)
	}
	wantEvidence := map[string]any{
		"audit_log_id":     "654",
		"arguments_sha256": argumentsDigest,
		"response_sha256":  responseDigest,
		"jsonrpc_success":  true,
		"result_projection": map[string]any{
			"correlated_groups": []any{},
			"dedup_reason":      "same resource",
			"severity":          "critical",
		},
		"tool": "loop.correlate",
	}
	if !reflect.DeepEqual(metaEvidence, wantEvidence) {
		t.Fatalf("evidence = %+v, want %+v", metaEvidence, wantEvidence)
	}
}

func TestJSONRPC_HumanCasbinUsesToolClass(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	authz := &allowCasbinTool{allow: true}
	loopHandler.SetAuthz(authz)
	loopHandler.SetLoopToolMetadata(map[string]string{"loop.correlate": "write"})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newJSONRPCRequest(t, "tools/call", `{"name":"loop.correlate","arguments":{"raw_alerts":[],"window":"5m"}}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if authz.action != "write" {
		t.Fatalf("Casbin action = %q, want write", authz.action)
	}
}

func TestJSONRPC_AgentTeamsToolsCallSkipsHumanCasbin(t *testing.T) {
	handler, loopHandler := newJSONRPCHandler(t)
	loopHandler.SetAuthz(&allowCasbinTool{allow: false})
	recording := &recordingLoopService{}
	if err := loopHandler.SetLoopTools(recording, nil, nil); err != nil {
		t.Fatal(err)
	}
	claims := auth.AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-alerter", Role: "alerter",
		AllowedTools: []string{"loop.correlate"},
	}
	identity := `{"tenant_id":"tenant-a","service":"agentteams","worker":"opskeeper-alerter","role":"alerter"}`
	recorder := serveServiceJSONRPC(t, handler, claims, identity, "loop.correlate")
	if recorder.Code != http.StatusOK || recording.tenantID() != "tenant-a" {
		t.Fatalf("status = %d body = %s tenant = %q", recorder.Code, recorder.Body.String(), recording.tenantID())
	}
}

func newJSONRPCRequest(t *testing.T, method, params string) *http.Request {
	t.Helper()
	payload := map[string]any{"jsonrpc": "2.0", "id": 7, "method": method}
	if params != "" {
		payload["params"] = json.RawMessage(params)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(raw))
	request.Header.Set("X-Opskeeper-Version", "v1")
	return request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 42, Role: "user"}))
}

func serveServiceJSONRPC(t *testing.T, handler http.Handler, claims auth.AgentTeamsServiceClaims, identityHeader, tool string) *httptest.ResponseRecorder {
	t.Helper()
	arguments := map[string]any{"incident_id": "incident-1"}
	if tool == "loop.correlate" {
		arguments = map[string]any{
			"raw_alerts": []map[string]any{{
				"alert_id": "a1", "severity": "warn", "resource": "pg:primary", "detected_at": "2026-08-18T07:00:00Z",
			}},
			"window": "5m",
		}
	}
	payload := map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": arguments},
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(rawPayload))
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 42, Role: "user"}))
	signer := auth.NewSigner("test-secret", time.Minute, time.Hour)
	token, err := signer.SignAgentTeamsService(claims, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Opskeeper-Version", "v1")
	if identityHeader != "" {
		request.Header.Set("X-Opskeeper-Worker-Identity", identityHeader)
	}
	recorder := httptest.NewRecorder()
	auth.Middleware(signer)(handler).ServeHTTP(recorder, request)
	return recorder
}

func serveServiceJSONRPCMethod(handler http.Handler, claims auth.AgentTeamsServiceClaims, method, params string) *httptest.ResponseRecorder {
	payload := map[string]any{"jsonrpc": "2.0", "id": 7, "method": method}
	if params != "" {
		payload["params"] = json.RawMessage(params)
	}
	raw, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(raw))
	signer := auth.NewSigner("test-secret", time.Minute, time.Hour)
	token, err := signer.SignAgentTeamsService(claims, time.Minute)
	if err != nil {
		panic(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	identity, _ := json.Marshal(map[string]string{
		"tenant_id": claims.TenantID,
		"service":   claims.Service,
		"worker":    claims.Worker,
		"role":      claims.Role,
	})
	request.Header.Set("X-Opskeeper-Worker-Identity", string(identity))
	request.Header.Set("X-Opskeeper-Version", "v1")
	recorder := httptest.NewRecorder()
	auth.Middleware(signer)(handler).ServeHTTP(recorder, request)
	return recorder
}

func newJSONRPCHandler(t *testing.T) (http.Handler, *Handler) {
	t.Helper()
	router := chi.NewRouter()
	handler := NewHandler(nil)
	if err := handler.SetLoopTools(stubLoopService{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	handler.Register(router)
	return router, handler
}

type stubLoopService struct{}

func (stubLoopService) Tools(context.Context) []loopbiz.MCPTool {
	return (&loopbiz.MCPAdapter{}).Tools(context.Background())
}

func (s stubLoopService) Invoke(ctx context.Context, tenantID, name string, arguments json.RawMessage) (any, error) {
	return (&loopbiz.MCPAdapter{}).Invoke(ctx, tenantID, name, arguments)
}

type recordingLoopService struct {
	tenantIDs []string
}

type allowCasbinTool struct {
	allow  bool
	action string
}

func (a *allowCasbinTool) AllowAnyOrg(_ context.Context, _ uint64, _, action string) bool {
	a.action = action
	return a.allow
}

type recordingAuditEmitter struct {
	events []auditbiz.Event
}

func (e *recordingAuditEmitter) Emit(_ context.Context, event auditbiz.Event) {
	e.events = append(e.events, event)
}

type policyLoopService struct{}

func (policyLoopService) Tools(context.Context) []loopbiz.MCPTool {
	tools := (&loopbiz.MCPAdapter{}).Tools(context.Background())
	return append(tools, loopbiz.MCPTool{Name: "recovery.execute"})
}

func (policyLoopService) Invoke(_ context.Context, _, name string, _ json.RawMessage) (any, error) {
	return map[string]any{"tool": name}, nil
}

func (s *recordingLoopService) Tools(context.Context) []loopbiz.MCPTool {
	return (&loopbiz.MCPAdapter{}).Tools(context.Background())
}

func (s *recordingLoopService) Invoke(_ context.Context, tenantID, name string, _ json.RawMessage) (any, error) {
	s.tenantIDs = append(s.tenantIDs, tenantID)
	return map[string]any{"tool": name}, nil
}

func (s *recordingLoopService) tenantID() string {
	if len(s.tenantIDs) == 0 {
		return ""
	}
	return s.tenantIDs[len(s.tenantIDs)-1]
}

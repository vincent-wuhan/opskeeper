package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/mcpclient"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
)

// postMCP sends one JSON-RPC message to the endpoint with a plain user tenant
// — what the handler sees downstream of the bearer middleware. version is the
// fleet header's value; "" means the request carries none, which is what a
// stock MCP client sends.
func postMCP(t *testing.T, handler http.Handler, method, params, version string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != "" {
		payload["params"] = json.RawMessage(params)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(raw))
	if version != "" {
		request.Header.Set("X-Opskeeper-Version", version)
	}
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 42, Role: "user"}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// postMCPNotification sends a JSON-RPC notification: no id, no response
// expected.
func postMCPNotification(t *testing.T, handler http.Handler, method string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(raw))
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 42, Role: "user"}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeRPC(t *testing.T, recorder *httptest.ResponseRecorder) jsonRPCResponse {
	t.Helper()
	var body jsonRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return body
}

// TestPingIsTheEmptyReplyTheSpecDefines: ping is the spec's keepalive utility
// and either side may send it. Answering method-not-found — what this endpoint
// did before decision 108 — tells a health-checking client that a healthy
// server is broken.
func TestPingIsTheEmptyReplyTheSpecDefines(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	recorder := postMCP(t, handler, "ping", `{}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := decodeRPC(t, recorder)
	if body.Error != nil {
		t.Fatalf("ping error = %+v", body.Error)
	}
	result, ok := body.Result.(map[string]any)
	if !ok {
		t.Fatalf("ping result = %#v, want an object", body.Result)
	}
	if len(result) != 0 {
		t.Fatalf("ping result = %+v, want the empty object", result)
	}
}

// TestInitializeEchoesTheRevisionTheClientAskedFor: the whole point of the
// handshake is that the server answers with something the client asked for.
// The revision used here is the one pkg/mcpclient advertises, so this is our
// own client's request replayed.
func TestInitializeEchoesTheRevisionTheClientAskedFor(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	recorder := postMCP(t, handler, "initialize", `{"protocolVersion":"`+mcpclient.ProtocolVersion+`"}`, "")
	body := decodeRPC(t, recorder)
	if body.Error != nil {
		t.Fatalf("initialize error = %+v", body.Error)
	}
	result, _ := body.Result.(map[string]any)
	if got := result["protocolVersion"]; got != mcpclient.ProtocolVersion {
		t.Fatalf("protocolVersion = %v, want the revision the client asked for (%s)", got, mcpclient.ProtocolVersion)
	}
}

// TestInitializeAnswersItsOwnRevisionWhenTheClientNamesAnother: a revision we
// do not implement gets ours, which is the answer the spec allows when there
// is no intersection.
func TestInitializeAnswersItsOwnRevisionWhenTheClientNamesAnother(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	for _, requested := range []string{``, `{"protocolVersion":"1999-01-01"}`, `{"protocolVersion":""}`} {
		recorder := postMCP(t, handler, "initialize", requested, "")
		body := decodeRPC(t, recorder)
		if body.Error != nil {
			t.Fatalf("initialize(%s) error = %+v", requested, body.Error)
		}
		result, _ := body.Result.(map[string]any)
		if got := result["protocolVersion"]; got != mcpProtocolVersions[0] {
			t.Fatalf("initialize(%s) protocolVersion = %v, want %s", requested, got, mcpProtocolVersions[0])
		}
	}
}

// TestInitializeStatesTheBoundary: the instructions field is the one place a
// client is told what this server will not do before it calls anything. The
// two claims asserted here are the plan's: writes still queue for a human, and
// this endpoint is not a proxy to public MCP servers.
func TestInitializeStatesTheBoundary(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	body := decodeRPC(t, postMCP(t, handler, "initialize", `{}`, ""))
	result, _ := body.Result.(map[string]any)
	instructions, _ := result["instructions"].(string)
	if instructions == "" {
		t.Fatal("initialize carries no instructions, so a client is never told the boundary")
	}
	for _, want := range []string{"approval", "does not proxy public MCP servers"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions do not mention %q:\n%s", want, instructions)
		}
	}
}

// TestEveryNotificationIsAcceptedWithoutABody: notifications carry no id and
// get no response, and the prefix is what says so — not a list of the two
// names this revision happens to define, which would break the next one.
func TestEveryNotificationIsAcceptedWithoutABody(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	for _, method := range []string{"notifications/initialized", "notifications/cancelled", "notifications/something-newer"} {
		recorder := postMCPNotification(t, handler, method)
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202", method, recorder.Code)
		}
		if strings.TrimSpace(recorder.Body.String()) != "" {
			t.Fatalf("%s: body = %q, want none", method, recorder.Body.String())
		}
	}
}

// TestAStockMCPClientWithoutTheFleetVersionHeaderIsAccepted: the header is
// this fleet's own marker, not part of MCP. No third-party client sends it, so
// requiring it made the endpoint unreachable for exactly the clients it exists
// for.
func TestAStockMCPClientWithoutTheFleetVersionHeaderIsAccepted(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	for _, method := range []string{"initialize", "tools/list", "ping"} {
		recorder := postMCP(t, handler, method, `{}`, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s without the fleet header: status = %d, body = %s", method, recorder.Code, recorder.Body.String())
		}
		if body := decodeRPC(t, recorder); body.Error != nil {
			t.Fatalf("%s without the fleet header: %+v", method, body.Error)
		}
	}
}

// TestAStatedForeignVersionIsStillRefused: dropping the requirement is not
// dropping the guard. A caller that names v2 is a v2 caller and is told so,
// which is the one thing the header was ever able to say.
func TestAStatedForeignVersionIsStillRefused(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	recorder := postMCP(t, handler, "tools/list", `{}`, "v2")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	body := decodeRPC(t, recorder)
	if body.Error == nil || body.Error.Code != -32002 {
		t.Fatalf("error = %+v, want the version-mismatch error", body.Error)
	}
}

// TestToolsListPagesAndHandsBackACursor: a catalogue that grows with the
// plugin fleet cannot be trusted to fit in one frame. The page size is set
// small here because the catalogue this test can build is small; the
// mechanism is what is under test, not the number.
func TestToolsListPagesAndHandsBackACursor(t *testing.T) {
	handler, h := newJSONRPCHandler(t)
	h.toolsPageSize = 2

	first := decodeRPC(t, postMCP(t, handler, "tools/list", `{}`, ""))
	if first.Error != nil {
		t.Fatalf("tools/list: %+v", first.Error)
	}
	result, _ := first.Result.(map[string]any)
	page, _ := result["tools"].([]any)
	if len(page) != 2 {
		t.Fatalf("first page = %d tools, want 2", len(page))
	}
	next, _ := result["nextCursor"].(string)
	if next != "2" {
		t.Fatalf("nextCursor = %q, want %q", next, "2")
	}

	second := decodeRPC(t, postMCP(t, handler, "tools/list", `{"cursor":"`+next+`"}`, ""))
	if second.Error != nil {
		t.Fatalf("tools/list cursor: %+v", second.Error)
	}
	result, _ = second.Result.(map[string]any)
	page, _ = result["tools"].([]any)
	if len(page) != 1 {
		t.Fatalf("second page = %d tools, want 1", len(page))
	}
	if _, ok := result["nextCursor"]; ok {
		t.Fatalf("second page still carries a cursor: %+v", result)
	}

	// The two pages together are the catalogue, not a subset of it.
	seen := map[string]bool{}
	for _, raw := range append(append([]any{}, mustTools(t, postMCP(t, handler, "tools/list", `{}`, ""))...), page...) {
		name, _ := raw.(map[string]any)["name"].(string)
		seen[name] = true
	}
	if len(seen) != 3 {
		t.Fatalf("pages cover %d distinct tools, want the whole 3: %v", len(seen), seen)
	}
}

func mustTools(t *testing.T, recorder *httptest.ResponseRecorder) []any {
	t.Helper()
	body := decodeRPC(t, recorder)
	result, _ := body.Result.(map[string]any)
	tools, _ := result["tools"].([]any)
	return tools
}

// TestAnUnparseableCursorIsAnErrorNotAPageOneRestart: a client that lost its
// place must be told. Quietly re-showing page one would look like progress and
// silently drop whatever it had not yet listed.
func TestAnUnparseableCursorIsAnErrorNotAPageOneRestart(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	for _, params := range []string{`{"cursor":"not-a-number"}`, `{"cursor":"-1"}`} {
		body := decodeRPC(t, postMCP(t, handler, "tools/list", params, ""))
		if body.Error == nil || body.Error.Code != -32602 {
			t.Fatalf("tools/list %s: error = %+v, want invalid-params", params, body.Error)
		}
	}
}

// TestACursorPastTheEndIsAnEmptyLastPage: following the cursor to the end must
// terminate, not loop or error.
func TestACursorPastTheEndIsAnEmptyLastPage(t *testing.T) {
	handler, _ := newJSONRPCHandler(t)
	body := decodeRPC(t, postMCP(t, handler, "tools/list", `{"cursor":"99"}`, ""))
	if body.Error != nil {
		t.Fatalf("tools/list: %+v", body.Error)
	}
	result, _ := body.Result.(map[string]any)
	page, _ := result["tools"].([]any)
	if len(page) != 0 {
		t.Fatalf("page = %d tools, want none", len(page))
	}
	if _, ok := result["nextCursor"]; ok {
		t.Fatalf("an exhausted catalogue still hands back a cursor: %+v", result)
	}
}

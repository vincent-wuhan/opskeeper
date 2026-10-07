package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/mcpclient"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
)

// TestOurOwnClientCanDriveOurOwnServer is the compatibility claim stated as an
// executable fact: pkg/mcpclient, the client this repository ships, performs
// the full handshake against this server without being taught anything about
// it. Before decision 108 this failed at the first request — the endpoint
// demanded the fleet's own X-Opskeeper-Version header, which the client has no
// reason to know about — so the test is also the reason the header became
// optional rather than required.
//
// It is deliberately a real HTTP round trip rather than a recorder: the
// incompatibilities that survive unit tests are the ones in the transport and
// the handshake, and those only show up when a client speaks to a server.
func TestOurOwnClientCanDriveOurOwnServer(t *testing.T) {
	router, _ := newJSONRPCHandler(t)

	var mu sync.Mutex
	negotiated := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r.WithContext(tenantctx.With(r.Context(), tenantctx.Tenant{UserID: 42, Role: "user"})))
		for k, values := range rec.Header() {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		body := rec.Body.Bytes()
		_, _ = w.Write(body)

		var envelope struct {
			Result map[string]any `json:"result"`
		}
		if json.Unmarshal(body, &envelope) == nil && envelope.Result != nil {
			if v, ok := envelope.Result["protocolVersion"].(string); ok {
				mu.Lock()
				negotiated = v
				mu.Unlock()
			}
		}
	}))
	defer server.Close()

	ctx := context.Background()
	client := mcpclient.NewHTTP(server.URL+"/v1/mcp", nil, 10*time.Second)
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	mu.Lock()
	got := negotiated
	mu.Unlock()
	if got != mcpclient.ProtocolVersion {
		t.Fatalf("server negotiated %q, our own client asked for %q: a handshake that ends in a revision the client never named is one the client may walk away from",
			got, mcpclient.ProtocolVersion)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("tools/list returned nothing, so the surface is not reachable")
	}
	for _, tool := range tools {
		if tool.Name == "" || tool.InputSchema == nil {
			t.Fatalf("tool %q has no name or no input schema: %+v", tool.Name, tool)
		}
	}

	result, err := client.CallTool(ctx, "loop.correlate", map[string]any{
		"raw_alerts": []map[string]any{{
			"alert_id": "a1", "severity": "warn", "resource": "pg:primary", "detected_at": "2026-08-18T07:00:00Z",
		}},
		"window": "5m",
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if strings.TrimSpace(result.TextContent()) == "" {
		t.Fatal("tools/call returned no text content")
	}

	// The session id the server handed out at initialize is echoed by the
	// client on every later request; the server must not depend on it, but it
	// must not break on it either. Calling twice exercises the echo.
	if _, err := client.ListTools(ctx); err != nil {
		t.Fatalf("tools/list after a session was established: %v", err)
	}
}

//go:build e2e

// Exploration orchestration (tasks.md 1.4): testenv.Start → LoginAdmin →
// POST /api/v1/agentteams/token → connection info. Zero server changes:
// minting rides the existing admin-facing issuance route and the
// existing AgentTeams JWT verification path on /api/v1/mcp.
//
// Token discipline (design.md 裁决四): the token lives in this struct
// only — never written to a file, never logged, never put in a test
// failure message. MintToken returns shape assertions only.
package explore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/auth"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/mcpclient"
	"github.com/vincent-wuhan/opskeeper/tests/e2e/testenv"
)

// Conn is the connection info a fake explorer runs with. The token is
// deliberately NOT part of Conn's String/fmt representation: use the
// accessor only when constructing an Authorization header.
type Conn struct {
	env    *testenv.Env
	role   string
	worker string
	// tenant is the identity the token was minted FOR. The tenant
	// invariant compares audit rows against this, so a charter that
	// disagrees with the token is a mismatch the explorer caused.
	tenant string
	token  string
}

// Tenant is the tenant the token was minted for.
func (c *Conn) Tenant() string { return c.tenant }

// MintToken mints an AgentTeams service token for role through the
// existing admin issuance route. allowedTools must be a subset of the
// role's matrix row (the server enforces this too — SignAgentTeamsService
// rejects tools AgentTeamsRoleAllows does not permit).
//
// DoJSON cannot carry the X-Opskeeper-Version header the route requires,
// so this posts the request directly.
//
// MintToken binds the token to the "default" tenant, which is the only
// tenant a fresh testenv has. MintTokenForTenant is the general form; a
// charter states its own tenant and the test passes it through, so the
// tenant invariant judges the token's real binding and not a literal.
func MintToken(t *testing.T, env *testenv.Env, adminAccessToken string, role string, allowedTools []string, ttlSeconds int) (*Conn, error) {
	t.Helper()
	return MintTokenForTenant(t, env, adminAccessToken, "default", role, allowedTools, ttlSeconds)
}

// MintTokenForTenant mints an AgentTeams service token bound to tenant.
func MintTokenForTenant(t *testing.T, env *testenv.Env, adminAccessToken, tenant, role string, allowedTools []string, ttlSeconds int) (*Conn, error) {
	t.Helper()
	worker := auth.AgentTeamsWorkerForRole(role)
	body := map[string]any{
		"tenant_id":     tenant,
		"worker":        worker,
		"role":          role,
		"allowed_tools": allowedTools,
		"ttl_seconds":   ttlSeconds,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, env.BaseURL()+"/api/v1/agentteams/token", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Opskeeper-Version", "v1")
	req.Header.Set("Authorization", "Bearer "+adminAccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("explore: mint token: transport: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// The response body of a failed mint carries the server's reason,
		// not any credential; safe to surface.
		return nil, fmt.Errorf("explore: mint token: status %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("explore: mint token: decode: %w", err)
	}
	if out.Token == "" {
		return nil, fmt.Errorf("explore: mint token: empty token in response")
	}
	return &Conn{env: env, role: role, worker: worker, tenant: tenant, token: out.Token}, nil
}

// MCPClient builds an mcpclient bound to this conn's env and token.
// The Authorization header never appears in any error the client
// returns: mcpclient errors carry method, code and endpoint only.
func (c *Conn) MCPClient(timeout time.Duration) *mcpclient.Client {
	return mcpClientFor(c.env.BaseURL(), c)
}

// mcpClientFor builds a client against baseURL with conn's token. The
// split exists so the boundary test can point A's token at B's base URL.
func mcpClientFor(baseURL string, c *Conn) *mcpclient.Client {
	return mcpclient.NewHTTP(
		baseURL+"/api/v1/mcp",
		map[string]string{"Authorization": "Bearer " + c.token},
		30*time.Second,
	)
}

// Role and Worker are the charter-visible identity fields. Token shape
// assertions use HasToken / TokenLength, never the value.
func (c *Conn) Role() string     { return c.role }
func (c *Conn) Worker() string   { return c.worker }
func (c *Conn) HasToken() bool   { return c.token != "" }
func (c *Conn) TokenLength() int { return len(c.token) }
func (c *Conn) BaseURL() string  { return c.env.BaseURL() }
func (c *Conn) DSN() string      { return c.env.DSN() }

// TraceStep is one recorded exploration step (the objective trajectory).
//
// ErrText is the JSON-RPC error message verbatim (it contains no
// credentials — tool errors are business text); HTTPOK records whether
// the transport succeeded, so "200 hiding an RPC error" stays visible.
//
// ResponseDigest is the text the response carried, kept so I-5 can scan
// it for credential shapes. It is the response's own text, not a hash:
// a hash cannot be matched against a shape. A step that never got a
// response leaves it empty, and the verdict treats an empty digest on a
// successful call as unscanned rather than clean.
//
// ReachedHandler is true when the request got as far as the tool handler.
// A call refused before that (unknown tool, authorization denial) books
// no start row and owes I-1 nothing.
//
// IsCall marks a tools/call step; tools/list and initialize are steps but
// not calls, and only calls count against max_tool_calls.
type TraceStep struct {
	Tool           string
	ErrText        string
	HTTPOK         bool
	ReachedHandler bool
	IsCall         bool
	ResponseDigest string
	At             time.Time
}

// Trace records a fake explorer's steps. No Authorization header and no
// token is ever stored. What is stored is the tool name, the error text,
// the response text, the transport outcome, and time.
type Trace struct {
	Steps []TraceStep
}

func (tr *Trace) Add(step TraceStep) {
	tr.Steps = append(tr.Steps, step)
}

// RunFixedSequence drives the fixed step list (the S1 "fake explorer"):
// initialize → tools/list (before) → each call step → tools/list
// (after). Every tools/call outcome is appended to the trace. It returns
// the before/after tool-name lists alongside the trace so the verdict
// input is assembled in one place.
func (c *Conn) RunFixedSequence(ctx context.Context, t *testing.T, calls []CallSpec) (*Trace, []string, []string, error) {
	t.Helper()
	client := c.MCPClient(30 * time.Second)

	if err := client.Initialize(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("explore: initialize: %w", err)
	}
	beforeTools, err := client.ListTools(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("explore: tools/list before: %w", err)
	}
	before := toolNames(beforeTools)

	trace := &Trace{}
	trace.Add(TraceStep{Tool: "initialize", HTTPOK: true, ResponseDigest: "initialize", At: time.Now().UTC()})
	trace.Add(TraceStep{Tool: "tools/list", HTTPOK: true, ResponseDigest: strings.Join(before, " "), At: time.Now().UTC()})
	for _, spec := range calls {
		res, callErr := client.CallTool(ctx, spec.Tool, spec.Args)
		isRPC := rpcError(callErr)
		// An RPC error means the server answered the envelope: the
		// request reached the MCP handler. A transport error did not.
		reached := callErr == nil || isRPC
		step := TraceStep{
			Tool: spec.Tool, HTTPOK: callErr == nil || isRPC,
			ReachedHandler: reached, IsCall: true, At: time.Now().UTC(),
		}
		if res != nil {
			step.ResponseDigest = res.TextContent()
		}
		if callErr != nil {
			// Keep the error class and message; the client's error
			// text embeds no token or header material.
			step.ErrText = callErr.Error()
			step.ResponseDigest = step.ErrText
			if spec.ExpectErr {
				t.Logf("explore: %s rejected as expected: %s", spec.Tool, step.ErrText)
			} else {
				t.Logf("explore: %s failed: %s", spec.Tool, step.ErrText)
			}
		}
		trace.Add(step)
	}

	afterTools, err := client.ListTools(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("explore: tools/list after: %w", err)
	}
	after := toolNames(afterTools)
	trace.Add(TraceStep{Tool: "tools/list", HTTPOK: true, ResponseDigest: strings.Join(after, " "), At: time.Now().UTC()})
	return trace, before, after, nil
}

// CallSpec is one step of a fixed sequence. ExpectErr documents the
// intent of the step (approval-gate rejection) so the test's own logs
// read as evidence, but the VERDICT never consumes ExpectErr — only the
// recorded trace and audit rows.
type CallSpec struct {
	Tool      string
	Args      map[string]any
	ExpectErr bool
}

func toolNames(tools []mcpclient.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	return out
}

// rpcError reports whether err is a JSON-RPC level error (transport
// succeeded, the envelope carried "error"). mcpclient's error format for
// that path is "mcp: <method> rpc error <code>: <message>".
func rpcError(err error) bool {
	return err != nil && bytes.Contains([]byte(err.Error()), []byte("rpc error"))
}

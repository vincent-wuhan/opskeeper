package probes

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AgentTeamsControllerChecker verifies the AgentTeams Controller /api/v1/workers
// endpoint is reachable. opskeeper's plugin sync + MCP dispatch paths both
// depend on this controller — if it is unreachable, /readyz must report 503
// so K8s removes the pod from the service mesh instead of routing MCP
// calls to a manager that can only return "controller discovery failed".
//
// Behavior:
//   - controllerURL == "" → skip (return nil): legitimate for single-replica
//     dev / test deployments that do not run a Controller
//   - HTTP non-2xx → error (response body truncated to 4 KB to avoid log spam)
//   - network / timeout / DNS → error
//
// The 500ms per-check budget from RunChecks applies. We use an internal
// http.Client with 400ms timeout to leave 100ms headroom for ctx propagation
// + JSON decode, even on a busy shared transport.
func AgentTeamsControllerChecker(controllerURL, bearerToken string) Checker {
	return &agentTeamsControllerChecker{
		url:         controllerURL,
		bearerToken: bearerToken,
		httpClient:  &http.Client{Timeout: 400 * time.Millisecond},
	}
}

type agentTeamsControllerChecker struct {
	url         string
	bearerToken string
	httpClient  *http.Client
}

func (c *agentTeamsControllerChecker) Name() string { return "agentteams_controller" }

func (c *agentTeamsControllerChecker) Check(ctx context.Context) error {
	if c.url == "" {
		// Graceful degrade: not every deployment runs a Controller. K8s
		// removes /readyz gating for this dimension so dev / CI tests
		// without a Controller still get 200.
		return nil
	}
	endpoint := strings.TrimRight(c.url, "/") + "/api/v1/workers"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("agentteams_controller: build request: %w", err)
	}
	if c.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("agentteams_controller: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Read up to 4 KB so operators see WHY in /readyz response without
		// flooding logs on every poll (K8s polls /readyz every 10s by default).
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("agentteams_controller: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(buf[:n])))
	}
	return nil
}

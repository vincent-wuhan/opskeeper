package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const hostFixtureClientMaxResponseBytes = 1 << 20

// HostFixtureClient is the production HostProcessTerminator adapter. It
// exposes neither a PID field nor a shell interface; the fixture resolves the
// incident-owned process group from its own process table.
type HostFixtureClient struct {
	endpoint   string
	token      string
	httpClient *http.Client
}

type HostFixtureStatus struct {
	ManifestID   string `json:"manifest_id"`
	IncidentID   string `json:"incident_id"`
	Resource     string `json:"resource"`
	Status       string `json:"status"`
	ProcessCount int    `json:"process_count"`
}

func NewHostFixtureClient(endpoint, token string) *HostFixtureClient {
	return &HostFixtureClient{
		endpoint: strings.TrimRight(endpoint, "/"),
		token:    token,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        2,
				IdleConnTimeout:     30 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
	}
}

func (c *HostFixtureClient) Status(ctx context.Context, request HostProcessTerminationRequest) (HostFixtureStatus, error) {
	path, err := c.fixturePath(request, "")
	if err != nil {
		return HostFixtureStatus{}, err
	}
	var status HostFixtureStatus
	if err := c.do(ctx, http.MethodGet, path, nil, &status); err != nil {
		return HostFixtureStatus{}, err
	}
	return status, nil
}

func (c *HostFixtureClient) Terminate(ctx context.Context, request HostProcessTerminationRequest) (json.RawMessage, error) {
	path, err := c.fixturePath(request, "/terminate")
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	if err := c.do(ctx, http.MethodPost, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *HostFixtureClient) fixturePath(request HostProcessTerminationRequest, action string) (string, error) {
	if c == nil || c.endpoint == "" || c.token == "" {
		return "", fmt.Errorf("host fixture endpoint and token are required")
	}
	if request.IncidentID == "" || request.FixtureManifestID == "" {
		return "", fmt.Errorf("incident_id and fixture_manifest_id are required")
	}
	return fmt.Sprintf("/v1/fixtures/%s%s", url.PathEscape(request.FixtureManifestID), action), nil
}

func (c *HostFixtureClient) do(ctx context.Context, method, path string, body []byte, output any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build fixture request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Opskeeper-Version", "v1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call host fixture: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, hostFixtureClientMaxResponseBytes))
	if err != nil {
		return fmt.Errorf("read host fixture response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("host fixture returned HTTP %d", resp.StatusCode)
	}
	var response struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return fmt.Errorf("decode host fixture response: %w", err)
	}
	if response.Code != http.StatusOK || len(response.Data) == 0 {
		return fmt.Errorf("host fixture rejected request: %s", response.Message)
	}
	if err := json.Unmarshal(response.Data, output); err != nil {
		return fmt.Errorf("decode host fixture data: %w", err)
	}
	return nil
}

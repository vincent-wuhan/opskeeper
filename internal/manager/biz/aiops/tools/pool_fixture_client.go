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

const poolFixtureClientMaxResponseBytes = 1 << 20

// PoolFixtureClient is the only execution seam for resize_pool. The fixture
// owns its PostgreSQL connections and resolves the manifest server-side.
type PoolFixtureClient struct {
	endpoint   string
	token      string
	httpClient *http.Client
}

type PoolFixtureStatus struct {
	ManifestID  string `json:"manifest_id"`
	IncidentID  string `json:"incident_id"`
	Resource    string `json:"resource"`
	Status      string `json:"status"`
	FailedProbe struct {
		Status string `json:"status"`
	} `json:"failed_probe"`
}

type PoolRecoveryRequest struct {
	IncidentID     string
	PoolManifestID string
	Reason         string
}

func NewPoolFixtureClient(endpoint, token string) *PoolFixtureClient {
	return &PoolFixtureClient{
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

func (c *PoolFixtureClient) Status(ctx context.Context, request PoolRecoveryRequest) (PoolFixtureStatus, error) {
	path, err := c.fixturePath(request, "")
	if err != nil {
		return PoolFixtureStatus{}, err
	}
	var status PoolFixtureStatus
	if err := c.do(ctx, http.MethodGet, path, nil, &status); err != nil {
		return PoolFixtureStatus{}, err
	}
	return status, nil
}

func (c *PoolFixtureClient) Recover(ctx context.Context, request PoolRecoveryRequest) (json.RawMessage, error) {
	path, err := c.fixturePath(request, "/recover")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: request.Reason})
	if err != nil {
		return nil, fmt.Errorf("encode pool recovery request: %w", err)
	}
	var result json.RawMessage
	if err := c.do(ctx, http.MethodPost, path, body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *PoolFixtureClient) fixturePath(request PoolRecoveryRequest, action string) (string, error) {
	if c == nil || c.endpoint == "" || c.token == "" {
		return "", fmt.Errorf("pool fixture endpoint and token are required")
	}
	if request.IncidentID == "" || request.PoolManifestID == "" {
		return "", fmt.Errorf("incident_id and pool_manifest_id are required")
	}
	return fmt.Sprintf("/v1/pool-fixtures/%s%s", url.PathEscape(request.PoolManifestID), action), nil
}

func (c *PoolFixtureClient) do(ctx context.Context, method, path string, body []byte, output any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build pool fixture request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Opskeeper-Version", "v1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call pool fixture: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, poolFixtureClientMaxResponseBytes))
	if err != nil {
		return fmt.Errorf("read pool fixture response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("pool fixture returned HTTP %d", resp.StatusCode)
	}
	var response struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return fmt.Errorf("decode pool fixture response: %w", err)
	}
	if response.Code != http.StatusOK || len(response.Data) == 0 {
		return fmt.Errorf("pool fixture rejected request: %s", response.Message)
	}
	if err := json.Unmarshal(response.Data, output); err != nil {
		return fmt.Errorf("decode pool fixture data: %w", err)
	}
	return nil
}

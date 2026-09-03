// Higress 控制面客户端实现。
//
// AgentTeams Worker 通过 stdio MCP proxy (mcp/server.py) 调 opskeeper 时，
// opskeeper 需解析 Bearer GatewayKey → Higress consumer → 角色映射。
// 本实现调 Higress 控制面 API GET /v1/consumers?apikey=<key>。
//
// 生产通过 env 注入：
//
//	HIGRESS_CONSOLE_URL=http://higress-console:8001
//	HIGRESS_ADMIN_USER=admin
//	HIGRESS_ADMIN_PASSWORD_FILE=/var/secrets/higress-password
//
// 测试用 mockHigress（在 auth_test.go 里）。
package agentteams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/prom"
)

// HigressHTTPClient 调真实 Higress 控制面。
type HigressHTTPClient struct {
	consoleURL    string
	adminUser     string
	adminPassword string
	cookieName    string
	sessionCookie string
	httpClient    *http.Client
}

// NewHigressHTTPClient 构造。
func NewHigressHTTPClient(consoleURL, adminUser, adminPassword string) *HigressHTTPClient {
	return &HigressHTTPClient{
		consoleURL:    strings.TrimRight(consoleURL, "/"),
		adminUser:     adminUser,
		adminPassword: adminPassword,
		httpClient:    &http.Client{Timeout: 5 * time.Second},
	}
}

// NewHigressHTTPClientFromEnv 从环境变量构造。
func NewHigressHTTPClientFromEnv() *HigressHTTPClient {
	pwd := os.Getenv("HIGRESS_ADMIN_PASSWORD")
	if f := os.Getenv("HIGRESS_ADMIN_PASSWORD_FILE"); f != "" {
		if data, err := os.ReadFile(f); err == nil {
			pwd = strings.TrimSpace(string(data))
		}
	}
	return NewHigressHTTPClient(
		os.Getenv("HIGRESS_CONSOLE_URL"),
		os.Getenv("HIGRESS_ADMIN_USER"),
		pwd,
	)
}

// login 调 /session/login 拿控制台会话 cookie。失败返回 error。
func (c *HigressHTTPClient) login(ctx context.Context) error {
	if c.sessionCookie != "" {
		return nil
	}
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, c.adminUser, c.adminPassword)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.consoleURL+"/session/login", strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("higress login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("higress login http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("higress login: HTTP %d: %s", resp.StatusCode, string(buf))
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Value != "" {
			c.cookieName = cookie.Name
			c.sessionCookie = cookie.Value
			return nil
		}
	}
	// Fallback: read Set-Cookie header raw
	setCookie := resp.Header.Get("Set-Cookie")
	if setCookie != "" {
		parts := strings.Split(setCookie, ";")
		if len(parts) > 0 {
			kv := strings.SplitN(parts[0], "=", 2)
			if len(kv) == 2 {
				c.cookieName = kv[0]
				c.sessionCookie = kv[1]
				return nil
			}
		}
	}
	return fmt.Errorf("higress login: no session cookie returned")
}

// ResolveConsumer 调 /v1/consumers?apikey=<key> 解析 consumer。
//
// 返回 consumerName / apiKeyID / role（role 由 consumerName 前缀推断：
// "manager-" / "worker-" / "admin-"）。
//
// 副作用：每次调用 emit 一条 prom.IncAgentTeamsHigressResolve(result)，
// 让 opskeeper self-health 能区分 Higress 不可达 / 鉴权失败 / 找不到 consumer。
func (c *HigressHTTPClient) ResolveConsumer(ctx context.Context, apiKey string) (consumerName, apiKeyID, role string, err error) {
	if err := c.login(ctx); err != nil {
		prom.IncAgentTeamsHigressResolve("auth_failed")
		return "", "", "", err
	}

	u := c.consoleURL + "/v1/consumers?" + url.Values{"apikey": {apiKey}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		prom.IncAgentTeamsHigressResolve("network_error")
		return "", "", "", fmt.Errorf("higress resolve: %w", err)
	}
	cookieName := c.cookieName
	if cookieName == "" {
		cookieName = "session"
	}
	req.AddCookie(&http.Cookie{Name: cookieName, Value: c.sessionCookie})
	resp, err := c.httpClient.Do(req)
	if err != nil {
		prom.IncAgentTeamsHigressResolve("network_error")
		return "", "", "", fmt.Errorf("higress resolve http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		prom.IncAgentTeamsHigressResolve("not_found")
		return "", "", "", ErrHigressConsumerNotFound
	}
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(resp.Body)
		prom.IncAgentTeamsHigressResolve("auth_failed")
		return "", "", "", fmt.Errorf("higress resolve: HTTP %d: %s", resp.StatusCode, string(buf))
	}

	var body struct {
		Name     string `json:"name"`
		APIKeyID string `json:"apiKeyId"`
		Data     []struct {
			Name        string `json:"name"`
			APIKeyID    string `json:"apiKeyId"`
			Credentials []struct {
				Key    string   `json:"key"`
				Values []string `json:"values"`
			} `json:"credentials"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		prom.IncAgentTeamsHigressResolve("network_error")
		return "", "", "", fmt.Errorf("higress resolve decode: %w", err)
	}
	if body.Name == "" && len(body.Data) > 0 {
		matched := body.Data[0]
		if len(body.Data) > 1 {
			matched.Name = ""
			for _, candidate := range body.Data {
				for _, credential := range candidate.Credentials {
					if credential.Key == apiKey || sliceContains(credential.Values, apiKey) {
						matched = candidate
						break
					}
				}
				if matched.Name != "" {
					break
				}
			}
		}
		body.Name = matched.Name
		body.APIKeyID = matched.APIKeyID
	}
	if body.Name == "" {
		prom.IncAgentTeamsHigressResolve("not_found")
		return "", "", "", ErrHigressConsumerNotFound
	}

	role = inferRoleFromConsumerName(body.Name)
	prom.IncAgentTeamsHigressResolve("ok")
	return body.Name, body.APIKeyID, role, nil
}

func sliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ErrHigressConsumerNotFound Higress 没找到对应 consumer。
var ErrHigressConsumerNotFound = fmt.Errorf("higress consumer not found")

// inferRoleFromConsumerName 由 consumerName 前缀推断 role。
//
// 约定：
//
//	manager-*        → role=manager
//	worker-*         → role=worker
//	admin-*          → role=admin
//	opskeeper-<role> → role=<role>（AgentTeams canonical naming，PR #11 引入）
//	其他             → role=unknown
func inferRoleFromConsumerName(name string) string {
	switch {
	case name == "manager":
		return "manager"
	case name == "worker":
		return "worker"
	case name == "admin":
		return "admin"
	case strings.HasPrefix(name, "manager-"):
		return "manager"
	case strings.HasPrefix(name, "worker-"):
		return "worker"
	case strings.HasPrefix(name, "admin-"):
		return "admin"
	case strings.HasPrefix(name, "opskeeper-"):
		return strings.TrimPrefix(name, "opskeeper-")
	default:
		return "unknown"
	}
}

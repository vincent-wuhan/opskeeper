// Package agentteams plugin sync client — 通过 opskeeper 触发 worker 端 reload。
//
// 四种实现：
//   - WorkerHTTPClient:    真发 POST 到一组 worker qwenpaw plugin HTTP router（多副本生产）
//   - DiscoveredWorkerClient: 通过 AgentTeams Controller /api/v1/workers 自动发现 worker 端点
//   - LocalHarnessSyncClient: 单机 / 单副本部署，真发 POST 到本地 qwenpaw 控制台
//   - LoggingSyncClient:    仅写日志（显式 dev / 离线回退，不再是默认）
//
// 选型（由 cmd/opskeeper/buildPluginSyncClient 编排）：
//
//	auto mode (env OPSKEEPER_PLUGIN_SYNC_MODE=auto) 自动按可用性选择 Controller > endpoints file > 本地 harness。
//	显式 mode 仍可强制 worker-http / controller-discovery / local-harness / stub。
//
// WorkerHTTPClient 期望 worker 端 plugin 通过 qwenpaw register_http_router 暴露
// POST /api/<plugin-id>/sync 端点（对齐 AgentTeams teamharness reference 模式：
// https://github.com/agentscope-ai/AgentTeams/blob/main/plugins/teamharness/adapters/qwenpaw/plugin.py
// `_register_http` 方法）。opskeeper-teamharness 已在 plugin.py 实现同样模式
// （前缀 `/opskeeper-teamharness`，端点 `/health` + `/sync`）。
package agentteams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// PluginSyncClient 抽象"向 worker 发 reload / install 请求"的能力。
// 生产实现调 worker HTTP endpoint（WorkerHTTPClient）；测试 / 单机模式用 LoggingSyncClient。
//
// InstallPlugin 与 SyncPlugin 是两条独立通路：
//   - SyncPlugin:   POST /api/<pluginID>/sync            (已有插件的配置热重载)
//   - InstallPlugin: POST /api/<pluginID>/install-plugin  (上传 zip + qwenpaw plugin install --force)
type PluginSyncClient interface {
	SyncPlugin(ctx context.Context, pluginID string) error
	InstallPlugin(ctx context.Context, pluginID string, zipPayload []byte, filename string) error
}

// LoggingSyncClient 是 PluginSyncClient 的"啥都不做但记日志"实现。
//
// 设计动机：单机模式 / 评审演示 / 本地 dev 环境避免依赖 worker HTTP endpoint。
// 生产部署应使用 WorkerHTTPClient 替换。
type LoggingSyncClient struct {
	Log *slog.Logger
}

// SyncPlugin 记一条日志然后返回 nil（视为 sync 成功）。
func (c *LoggingSyncClient) SyncPlugin(_ context.Context, pluginID string) error {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("plugin sync (stub)", "plugin", pluginID)
	return nil
}

// InstallPlugin 记一条日志然后返回 nil（视为 install 成功）。
//
// 设计动机：与 SyncPlugin 对齐，单机模式 / 评审演示 / 本地 dev 环境
// 避免依赖 worker HTTP endpoint。生产部署应使用 WorkerHTTPClient。
func (c *LoggingSyncClient) InstallPlugin(_ context.Context, pluginID string, zipPayload []byte, filename string) error {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("plugin install (stub)",
		"plugin", pluginID,
		"filename", filename,
		"bytes", len(zipPayload),
	)
	return nil
}

// WorkerEndpoint 描述一个 worker 上的 plugin sync 端点。
type WorkerEndpoint struct {
	// WorkerName 用于日志与审计,可空
	WorkerName string
	// BaseURL 例如 http://worker-qwenpaw-headless:8088
	BaseURL string
	// PluginPath 例如 /api/opskeeper-teamharness/sync
	// （默认 <pluginID>-sync；opskeeper-teamharness 约定前缀 /opskeeper-teamharness）
	PluginPath string
	// HTTPClient 复用现有 client，可空（默认 5s timeout）
	HTTPClient *http.Client
}

// WorkerHTTPClient 是 PluginSyncClient 的生产实现：通过 HTTP 触发每个 worker
// 上 qwenpaw plugin 的 /sync 端点，把 plugin 配置重载到 in-memory agent workspace。
//
// 设计要点：
//   - 每个 WorkerEndpoint 独立 POST，错误聚合返回（一个 worker 失败不影响其他）
//   - 默认 5s timeout，可由每个 endpoint 自定义
//   - 失败 worker 名称出现在 error message，便于运维定位
//   - 不抛 panic，所有错误 slog + 返回
//
// 重试（仅 InstallPlugin 路径）：
//   - 瞬时错误（network / 5xx / 429）按 InstallMaxAttempts 重试，指数退避
//   - 4xx（除 429）不重试 — 是客户端 / 配置错误，重试不会成功
//   - SyncPlugin 不重试 — 调用频次高，5s timeout 即可，重试反而堆积
type WorkerHTTPClient struct {
	// Workers sync 目标列表（由调用方构造，从 AgentTeams Controller / K8s API / 静态配置获取）
	Workers []WorkerEndpoint
	// BearerToken 可选，所有 worker 调用统一带 Authorization: Bearer <token>
	BearerToken string
	// Log 可空
	Log *slog.Logger
	// PluginPathTemplate 默认 "/api/{pluginID}/sync"，可覆盖
	PluginPathTemplate string
	// InstallMaxAttempts install 重试总次数（含首次）。0 / 负数 = 默认 3
	InstallMaxAttempts int
	// InstallRetryBaseDelay 首次重试前等待，后续 ×2 指数退避。0 = 默认 500ms
	InstallRetryBaseDelay time.Duration
}

// NewWorkerHTTPClient 构造。
//
// endpoints: worker 端点列表
// bearerToken: 可选 bearer token（Higress GatewayKey 或 AgentTeams service token）
func NewWorkerHTTPClient(endpoints []WorkerEndpoint, bearerToken string, log *slog.Logger) *WorkerHTTPClient {
	if log == nil {
		log = slog.Default()
	}
	return &WorkerHTTPClient{
		Workers:               endpoints,
		BearerToken:           bearerToken,
		Log:                   log,
		PluginPathTemplate:    "/api/%s/sync",
		InstallMaxAttempts:    3,
		InstallRetryBaseDelay: 500 * time.Millisecond,
	}
}

// defaultInstallPathTemplate is the suffix appended to the plugin path for install calls.
// Worker opskeeper-teamharness router mounts /opskeeper-teamharness/install-plugin.
const defaultInstallPathTemplate = "/api/%s/install-plugin"

// defaultInstallTimeout 上传 zip 需更长 timeout
const defaultInstallTimeout = 60 * time.Second

// isTransientHTTPStatus 判断 HTTP 状态码是否值得重试。
// 5xx + 429 一律重试；其余 4xx（bad request / not found / conflict …）是客户端 / 配置错误，重试无意义。
func isTransientHTTPStatus(code int) bool {
	if code >= 500 {
		return true
	}
	return code == http.StatusTooManyRequests
}

// installRetryDelay 第 n 次重试（n 从 1 起）前等待的时间。
// 指数退避：base * 2^(n-1)，封顶 10s。
func installRetryDelay(base time.Duration, n int) time.Duration {
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	if n < 1 {
		n = 1
	}
	d := base
	for i := 1; i < n; i++ {
		d *= 2
		if d >= 10*time.Second {
			return 10 * time.Second
		}
	}
	return d
}

// InstallPlugin 给所有 worker 上传 zip + 触发 qwenpaw plugin install --force。
//
// 与 SyncPlugin 同样按 worker 聚合错误：任一失败不阻塞其他 worker。
// zipPayload 即 multipart file 字段的内容（worker 端期望 multipart/form-data）。
//
// 重试：对每个 worker 独立重试 transient errors（network / 5xx / 429）。
// 任一 worker 达到 InstallMaxAttempts 上限后记入 failures 列表，其他 worker 不受影响。
func (c *WorkerHTTPClient) InstallPlugin(ctx context.Context, pluginID string, zipPayload []byte, filename string) error {
	if len(c.Workers) == 0 {
		c.Log.Warn("plugin install: no workers configured", "plugin", pluginID)
		return nil
	}
	if filename == "" {
		filename = pluginID + ".zip"
	}
	maxAttempts := c.InstallMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	var failures []string
	var successes int
	for _, ep := range c.Workers {
		path := ep.PluginPath
		// install 用单独的路径模板；如 endpoint 显式给出 PluginPath 则尊重它
		if path == "" {
			path = fmt.Sprintf(defaultInstallPathTemplate, pluginID)
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		url := strings.TrimRight(ep.BaseURL, "/") + path

		body, contentType, err := buildMultipartInstallBody(filename, zipPayload)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: build multipart: %v", ep.WorkerName, err))
			continue
		}

		client := ep.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: defaultInstallTimeout}
		}
		var lastErrMsg string
		var installed bool
	retryLoop:
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if err := ctx.Err(); err != nil {
				lastErrMsg = "ctx cancelled: " + err.Error()
				break
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				// 构建失败是 deterministic error，不重试
				failures = append(failures, fmt.Sprintf("%s: build req: %v", ep.WorkerName, err))
				continue retryLoop
			}
			req.Header.Set("Content-Type", contentType)
			if c.BearerToken != "" {
				req.Header.Set("Authorization", "Bearer "+c.BearerToken)
			}
			resp, err := client.Do(req)
			if err != nil {
				lastErrMsg = err.Error()
				c.Log.Warn("plugin install transient failure (network)",
					"plugin", pluginID, "worker", ep.WorkerName,
					"attempt", attempt, "max", maxAttempts, "err", err.Error())
				if attempt < maxAttempts {
					select {
					case <-ctx.Done():
						break retryLoop
					case <-time.After(installRetryDelay(c.InstallRetryBaseDelay, attempt)):
					}
					continue
				}
				break
			}
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				c.Log.Info("plugin install ok",
					"plugin", pluginID, "worker", ep.WorkerName,
					"status", resp.StatusCode, "bytes", len(zipPayload), "attempt", attempt)
				successes++
				installed = true
				break
			}
			lastErrMsg = fmt.Sprintf("HTTP %d %s", resp.StatusCode, string(respBody))
			if !isTransientHTTPStatus(resp.StatusCode) {
				c.Log.Warn("plugin install non-retryable failure",
					"plugin", pluginID, "worker", ep.WorkerName,
					"status", resp.StatusCode, "body", string(respBody))
				break
			}
			c.Log.Warn("plugin install transient failure (status)",
				"plugin", pluginID, "worker", ep.WorkerName,
				"attempt", attempt, "max", maxAttempts,
				"status", resp.StatusCode, "body", string(respBody))
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					break retryLoop
				case <-time.After(installRetryDelay(c.InstallRetryBaseDelay, attempt)):
				}
			}
		}
		if !installed {
			if lastErrMsg == "" {
				lastErrMsg = "no attempts"
			}
			failures = append(failures, fmt.Sprintf("%s: %s (after %d attempts)", ep.WorkerName, lastErrMsg, maxAttempts))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("plugin install partial failure (%d/%d ok): %s",
			successes, len(c.Workers), strings.Join(failures, "; "))
	}
	return nil
}

// buildMultipartInstallBody 构造 multipart/form-data；filename + zip bytes 一个 part。
//
// worker 端 opskeeper-teamharness install-plugin 端点通过 fastapi UploadFile
// 接收 multipart file 字段，filename 与 server 端 file.filename 一致。
func buildMultipartInstallBody(filename string, payload []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		_ = w.Close()
		return nil, "", err
	}
	if _, err := fw.Write(payload); err != nil {
		_ = w.Close()
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// SyncPlugin 给所有 worker 发 POST <base>/api/<pluginID>/sync。
//
// 错误聚合：若任一 worker 失败，error message 含失败 worker 名称；其他 worker
// 即使成功也会被记录到日志，便于事后排查。
func (c *WorkerHTTPClient) SyncPlugin(ctx context.Context, pluginID string) error {
	if len(c.Workers) == 0 {
		c.Log.Warn("plugin sync: no workers configured", "plugin", pluginID)
		return nil
	}
	var failures []string
	var successes int
	for _, ep := range c.Workers {
		path := ep.PluginPath
		if path == "" {
			path = fmt.Sprintf(c.PluginPathTemplate, pluginID)
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		url := strings.TrimRight(ep.BaseURL, "/") + path

		client := ep.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: 5 * time.Second}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: build req: %v", ep.WorkerName, err))
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if c.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.BearerToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ep.WorkerName, err))
			c.Log.Warn("plugin sync failed", "plugin", pluginID, "worker", ep.WorkerName, "url", url, "err", err.Error())
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			failures = append(failures, fmt.Sprintf("%s: HTTP %d %s", ep.WorkerName, resp.StatusCode, string(body)))
			c.Log.Warn("plugin sync non-2xx", "plugin", pluginID, "worker", ep.WorkerName, "status", resp.StatusCode, "body", string(body))
			continue
		}
		successes++
		c.Log.Info("plugin sync ok", "plugin", pluginID, "worker", ep.WorkerName, "status", resp.StatusCode)
	}
	if len(failures) > 0 {
		return fmt.Errorf("plugin sync partial failure (%d/%d ok): %s",
			successes, len(c.Workers), strings.Join(failures, "; "))
	}
	return nil
}

// MarshalWorkerEndpoints 序列化 worker endpoints（用于从 env / config 文件加载）。
func MarshalWorkerEndpoints(eps []WorkerEndpoint) ([]byte, error) {
	return json.Marshal(eps)
}

// UnmarshalWorkerEndpoints 反序列化。
func UnmarshalWorkerEndpoints(data []byte) ([]WorkerEndpoint, error) {
	var eps []WorkerEndpoint
	if err := json.Unmarshal(data, &eps); err != nil {
		return nil, err
	}
	return eps, nil
}

// LocalHarnessSyncClient 是 PluginSyncClient 的"单机 / 单副本"生产实现：把 sync / install
// 请求转发到本机或单实例 qwenpaw 控制台（默认 http://127.0.0.1:8088）。
//
// 设计动机：
//   - 产品演示 / CI / 单机部署的典型形态 — 一个 opskeeper + 一个 worker 同进程或同 pod。
//   - 不引入对 AgentTeams Controller 的依赖（Controller 是多副本编排的元服务）。
//   - 不需要 K8s / Docker 服务发现 — 直接走 127.0.0.1。
//
// 实现要点：
//   - 内部委托给 WorkerHTTPClient（一个 WorkerEndpoint），保持错误聚合 / 重试 / multipart 上传
//     等行为与多 worker 模式一致。
//   - BaseURL 由 env OPSKEEPER_LOCAL_HARNESS_URL 覆盖；空字符串 = 真 fallback 到 LoggingSyncClient。
//   - 启动期调一次 /health（best-effort）决定是否启用，避免 sync 调用时才暴露网络错误。
//   - 若启动 healthcheck 失败，仍返回 client（不阻塞 opskeeper 启动），但每次 sync 会重新尝试 +
//     给出明确 warn。
type LocalHarnessSyncClient struct {
	// BaseURL 例如 http://127.0.0.1:8088。空字符串 = 真 fallback，等价 LoggingSyncClient。
	BaseURL string
	// BearerToken 可选；多副本环境用 Higress GatewayKey，单机通常不需要
	BearerToken string
	// PluginPath 默认 /api/opskeeper-teamharness/sync
	PluginPath string
	// InstallPath 默认 /api/opskeeper-teamharness/install-plugin
	InstallPath string
	// HealthPath 默认 /api/opskeeper-teamharness/health
	HealthPath string
	// HTTPClient 可空（默认 sync=5s / install=60s，通过 WorkerHTTPClient 内部委托）
	HTTPClient *http.Client
	// Log 可空
	Log *slog.Logger
	// SkipHealthCheck 跳过启动期 /health 探测（默认 false）。CI / 短寿命进程可设为 true。
	SkipHealthCheck bool
}

// NewLocalHarnessSyncClient 构造一个本地 harness sync 客户端。
//
// baseURL: 必填；空字符串返回 nil 并报告错误。
// bearer: 可选 bearer token（Higress GatewayKey / service token）。
// log: 可选 slog logger。
func NewLocalHarnessSyncClient(baseURL, bearer string, log *slog.Logger) (*LocalHarnessSyncClient, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("local harness sync: baseURL is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &LocalHarnessSyncClient{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		BearerToken: bearer,
		PluginPath:  "/api/opskeeper-teamharness/sync",
		InstallPath: "/api/opskeeper-teamharness/install-plugin",
		HealthPath:  "/api/opskeeper-teamharness/health",
		Log:         log,
	}, nil
}

// HealthCheck 对本地 harness 探活（GET <BaseURL><HealthPath>）。任何 2xx 视为 OK。
//
// 在 NewLocalHarnessSyncClient 之后立刻调一次；失败仅记 warn，不阻塞构造。
func (c *LocalHarnessSyncClient) HealthCheck(ctx context.Context) error {
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	url := c.BaseURL + c.HealthPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("local harness health: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("local harness health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// endpoint 返回本地 harness 唯一的 WorkerEndpoint（PluginPath 留空，由方法内模板决定）。
func (c *LocalHarnessSyncClient) endpoint() WorkerEndpoint {
	return WorkerEndpoint{
		WorkerName: "local-harness",
		BaseURL:    c.BaseURL,
		HTTPClient: c.HTTPClient,
	}
}

// SyncPlugin 转发到本地 qwenpaw 控制台（POST <BaseURL><PluginPath>）。
//
// 与 WorkerHTTPClient 一致：错误聚合、5xx 不重试（每次 sync 独立调，调用方决定是否重试）。
func (c *LocalHarnessSyncClient) SyncPlugin(ctx context.Context, pluginID string) error {
	ep := c.endpoint()
	ep.PluginPath = c.PluginPath
	if !strings.HasPrefix(ep.PluginPath, "/") {
		ep.PluginPath = "/" + ep.PluginPath
	}
	h := NewWorkerHTTPClient([]WorkerEndpoint{ep}, c.BearerToken, c.Log)
	return h.SyncPlugin(ctx, pluginID)
}

// InstallPlugin 上传 zip + 触发本地 qwenpaw 控制台 install 端点。
//
// multipart 上传由 WorkerHTTPClient.InstallPlugin 内部完成；失败重试沿用其策略。
func (c *LocalHarnessSyncClient) InstallPlugin(ctx context.Context, pluginID string, zipPayload []byte, filename string) error {
	ep := c.endpoint()
	ep.PluginPath = c.InstallPath
	if !strings.HasPrefix(ep.PluginPath, "/") {
		ep.PluginPath = "/" + ep.PluginPath
	}
	h := NewWorkerHTTPClient([]WorkerEndpoint{ep}, c.BearerToken, c.Log)
	return h.InstallPlugin(ctx, pluginID, zipPayload, filename)
}

// ProbeLocalHarness 在启动期 / 配置变更期调用：返回本地 harness 是否可达 + 健康。
//
// 返回 (ok, err)：ok=true 表示 healthcheck 通过；err 始终非 nil（即使 ok=true 也带原因）。
// 调用方按需记 log / 调整后续行为。
func ProbeLocalHarness(ctx context.Context, baseURL string, bearer string, timeout time.Duration) (bool, error) {
	if strings.TrimSpace(baseURL) == "" {
		return false, errors.New("local harness probe: empty baseURL")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	c, err := NewLocalHarnessSyncClient(baseURL, bearer, slog.Default())
	if err != nil {
		return false, err
	}
	c.HTTPClient = &http.Client{Timeout: timeout}
	if err := c.HealthCheck(ctx); err != nil {
		return false, err
	}
	return true, nil
}

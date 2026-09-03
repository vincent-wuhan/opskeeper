// Package agentteams controller worker discovery — 通过 AgentTeams Controller
// /api/v1/workers 自动推导 worker plugin sync endpoints。
//
// 解决 K8s/Docker 部署下"为 opskeeper PluginSyncClient 配置 worker URL"的运维痛点：
//   - K8s:pod IP 不固定,但 Controller 已经跟踪 — 调 /api/v1/workers 即可
//   - Docker:container 名稳定但跨 host 难发现 — Controller 抽象了这一点
//   - 任何 Controller 可达的环境都 work — Controller 自身抽象环境差异
//
// 与 WorkerHTTPClient 配合:本模块只负责 discover,SyncPlugin 仍走 WorkerHTTPClient。
package agentteams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ControllerWorker 是 AgentTeams Controller /api/v1/workers 返回的 worker 摘要。
//
// 字段对照 Controller internal/server/types.go（snake_case）。当前只用 5 个最小子集
// 推导 plugin sync endpoint；扩展字段按需添加。
type ControllerWorker struct {
	Name     string `json:"name"`
	Phase    string `json:"phase,omitempty"`
	Runtime  string `json:"runtime,omitempty"`
	Host     string `json:"host,omitempty"`     // K8s pod DNS / Docker container name
	Port     int    `json:"port,omitempty"`     // qwenpaw console port (default 8088)
	Endpoint string `json:"endpoint,omitempty"` // 已构造好的 http URL（Controller 1.4+ 提供）
}

// DiscoveryConfig 构造参数。
type DiscoveryConfig struct {
	// ControllerURL 例如 http://agentteams-controller.default.svc.cluster.local:8080
	// 默认 env: AGENTTEAMS_CONTROLLER_URL
	ControllerURL string
	// BearerToken 可选（Higress GatewayKey / Controller service token）
	BearerToken string
	// PluginPath 默认 /api/opskeeper-teamharness/sync
	// 多数 plugin 约定 <plugin-id>-sync 格式；可覆盖
	PluginPath string
	// 过滤:只返回 phase ∈ {Running, Ready, Active} 的 worker（空 = 不过滤）
	RunningPhases []string
	// 缓存 TTL，默认 5 min。0 表示每次 Discover 都打 Controller
	CacheTTL time.Duration
	// HTTPClient 可选（默认 5s timeout）
	HTTPClient *http.Client
	// Logger 可选
	Logger *slog.Logger
}

// DefaultDiscoveryConfig 从环境变量填充缺省值。
func DefaultDiscoveryConfig() DiscoveryConfig {
	return DiscoveryConfig{
		ControllerURL: os.Getenv("AGENTTEAMS_CONTROLLER_URL"),
		BearerToken:   os.Getenv("AGENTTEAMS_CONTROLLER_BEARER"),
		PluginPath:    "/api/opskeeper-teamharness/sync",
		RunningPhases: []string{"Running", "Ready", "Active"},
		CacheTTL:      5 * time.Minute,
	}
}

// ControllerWorkerDiscovery 实现 worker endpoint 自动发现 + 缓存。
//
// 线程安全：mu 保护 cached endpoints；多个并发 SyncPlugin 调用共享一次 Discover。
type ControllerWorkerDiscovery struct {
	cfg DiscoveryConfig

	mu       sync.RWMutex
	cached   []WorkerEndpoint
	cachedAt time.Time

	// fetchGroup 是 single-flight 组：cold cache 下 N 个并发 Discover() 只触发
	// 一次 controller fetch，第二个以后共享结果。等价于"dedupe by string key"的
	// golang.org/x/sync/singleflight.Group。Key 用常量字符串：所有 Discover() 共享。
	fetchGroup singleflight.Group
}

// NewControllerWorkerDiscovery 构造。
func NewControllerWorkerDiscovery(cfg DiscoveryConfig) *ControllerWorkerDiscovery {
	if cfg.PluginPath == "" {
		cfg.PluginPath = "/api/opskeeper-teamharness/sync"
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if len(cfg.RunningPhases) == 0 {
		cfg.RunningPhases = []string{"Running", "Ready", "Active"}
	}
	return &ControllerWorkerDiscovery{cfg: cfg}
}

// Discover 返回当前所有可达 worker 的 plugin sync endpoints。
//
// 行为：
//   - cache hit → 直接返回 cached
//   - cache miss 或 expired → 调 Controller /api/v1/workers
//   - 过滤 phase
//   - 推导 WorkerEndpoint.BaseURL = http(s)://<host>:<port> 或 endpoint 字段
//
// 失败时返回上一个 cached（stale-while-revalidate 模式），便于 Controller 临时
// 不可用时 opskeeper 仍能同步 plugin。
func (d *ControllerWorkerDiscovery) Discover(ctx context.Context) ([]WorkerEndpoint, error) {
	if d.cfg.ControllerURL == "" {
		return nil, fmt.Errorf("AGENTTEAMS_CONTROLLER_URL not set")
	}
	d.mu.RLock()
	if time.Since(d.cachedAt) < d.cfg.CacheTTL && len(d.cached) > 0 {
		out := make([]WorkerEndpoint, len(d.cached))
		copy(out, d.cached)
		d.mu.RUnlock()
		return out, nil
	}
	d.mu.RUnlock()

	d.mu.Lock()
	// double-check: another goroutine may have refreshed while we waited
	if time.Since(d.cachedAt) < d.cfg.CacheTTL && len(d.cached) > 0 {
		out := make([]WorkerEndpoint, len(d.cached))
		copy(out, d.cached)
		d.mu.Unlock()
		return out, nil
	}
	d.mu.Unlock()

	// single-flight: cold cache 下并发 Discover() 只发一次 controller 请求
	v, err, _ := d.fetchGroup.Do("controller-workers", func() (any, error) {
		return d.fetchFromController(ctx)
	})
	var eps []WorkerEndpoint
	if v != nil {
		eps = v.([]WorkerEndpoint)
	}
	if err != nil {
		d.mu.RLock()
		stale := make([]WorkerEndpoint, len(d.cached))
		copy(stale, d.cached)
		d.mu.RUnlock()
		if len(stale) > 0 {
			d.cfg.Logger.Warn("controller discovery failed, returning stale cache",
				"err", err.Error(), "stale_count", len(stale))
			return stale, nil
		}
		return nil, fmt.Errorf("controller discovery failed: %w", err)
	}

	d.mu.Lock()
	d.cached = eps
	d.cachedAt = time.Now()
	d.mu.Unlock()

	d.cfg.Logger.Info("controller worker discovery refreshed",
		"count", len(eps), "controller_url", d.cfg.ControllerURL)
	return eps, nil
}

// fetchFromController 调 Controller /api/v1/workers 并构造 []WorkerEndpoint。
func (d *ControllerWorkerDiscovery) fetchFromController(ctx context.Context) ([]WorkerEndpoint, error) {
	u := strings.TrimRight(d.cfg.ControllerURL, "/") + "/api/v1/workers"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if d.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.cfg.BearerToken)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := d.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("controller request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("controller returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Controller 返回结构可能是 {workers: [...]} 或裸数组；兼容两种
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var workers []ControllerWorker
	if err := json.Unmarshal(body, &workers); err != nil {
		var wrapped struct {
			Workers []ControllerWorker `json:"workers"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return nil, fmt.Errorf("decode controller response: %w (raw: %s)", err, string(body[:min(len(body), 256)]))
		}
		workers = wrapped.Workers
	}

	allowed := make(map[string]bool, len(d.cfg.RunningPhases))
	for _, p := range d.cfg.RunningPhases {
		allowed[p] = true
	}

	eps := make([]WorkerEndpoint, 0, len(workers))
	for _, w := range workers {
		if len(allowed) > 0 && !allowed[w.Phase] {
			continue
		}
		base := w.Endpoint
		if base == "" {
			base = constructWorkerURL(w)
		}
		if base == "" {
			d.cfg.Logger.Warn("worker missing host/port, skipping",
				"worker", w.Name, "phase", w.Phase)
			continue
		}
		eps = append(eps, WorkerEndpoint{
			WorkerName: w.Name,
			BaseURL:    base,
			PluginPath: d.cfg.PluginPath,
		})
	}
	return eps, nil
}

// constructWorkerURL 从 host + port 推导 http://host:port。
func constructWorkerURL(w ControllerWorker) string {
	if w.Host == "" || w.Port == 0 {
		return ""
	}
	u := &url.URL{
		Scheme: "http", // 容器内通常 http；如果 Controller 提供 scheme 字段应优先用
		Host:   fmt.Sprintf("%s:%d", w.Host, w.Port),
	}
	return u.String()
}

// DiscoveredWorkerClient 把 discovery 与 WorkerHTTPClient 组合，自动同步 plugin 到当前可达 worker。
//
// 用法：
//
//	disc := NewControllerWorkerDiscovery(...)
//	sync := NewDiscoveredWorkerClient(disc, bearerToken, log)
//	http.Handle("/v1/plugins/{id}/sync", /* handler that calls sync.SyncPlugin(ctx, id) */)
type DiscoveredWorkerClient struct {
	Discovery   *ControllerWorkerDiscovery
	BearerToken string
	Workers     []WorkerEndpoint // 可选 — 与 discovery 合并；通常留空只用 discovery
	Logger      *slog.Logger
}

// NewDiscoveredWorkerClient 构造。
func NewDiscoveredWorkerClient(d *ControllerWorkerDiscovery, bearerToken string, log *slog.Logger) *DiscoveredWorkerClient {
	if log == nil {
		log = slog.Default()
	}
	return &DiscoveredWorkerClient{
		Discovery:   d,
		BearerToken: bearerToken,
		Logger:      log,
	}
}

// SyncPlugin 调 discovery 拿当前 workers，构造一个临时 WorkerHTTPClient 触发 sync。
func (c *DiscoveredWorkerClient) SyncPlugin(ctx context.Context, pluginID string) error {
	eps, err := c.Discovery.Discover(ctx)
	if err != nil {
		return fmt.Errorf("plugin sync: discovery failed: %w", err)
	}
	if len(eps) == 0 {
		c.Logger.Warn("plugin sync: no workers discovered", "plugin", pluginID)
		return nil
	}
	hc := &WorkerHTTPClient{
		Workers:            eps,
		BearerToken:        c.BearerToken,
		Log:                c.Logger,
		PluginPathTemplate: "/api/%s/sync",
	}
	return hc.SyncPlugin(ctx, pluginID)
}

// InstallPlugin 与 SyncPlugin 同样走 discovery → WorkerHTTPClient 通路。
//
// worker 端 install 路径由 WorkerHTTPClient.InstallPlugin 内置模板
// `/api/<pluginID>/install-plugin` 解析，无需调用方传 path。
func (c *DiscoveredWorkerClient) InstallPlugin(ctx context.Context, pluginID string, zipPayload []byte, filename string) error {
	eps, err := c.Discovery.Discover(ctx)
	if err != nil {
		return fmt.Errorf("plugin install: discovery failed: %w", err)
	}
	if len(eps) == 0 {
		c.Logger.Warn("plugin install: no workers discovered", "plugin", pluginID)
		return nil
	}
	hc := &WorkerHTTPClient{
		Workers:     eps,
		BearerToken: c.BearerToken,
		Log:         c.Logger,
	}
	return hc.InstallPlugin(ctx, pluginID, zipPayload, filename)
}

// min helper (Go 1.21+ 内置;保留兜底 for 1.20)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

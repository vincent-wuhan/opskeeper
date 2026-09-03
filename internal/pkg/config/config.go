// Package config loads runtime configuration from environment variables.
//
// MVP uses plain os.Getenv + sensible defaults (no YAML/viper dep yet).
// See .env.example at the repo root for the full list of variables.
//
// Field grouping reflects the post-pivot stack: HTTP / metrics, DB (MySQL
// default, SQLite opt-in), JWT (iam), OpenAI (llm), Admin bootstrap (cloud
// only), Edge (opskeeper-edge).
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the top-level runtime configuration shared by opskeeper and
// opskeeper-edge. Fields unused by one binary are simply ignored.
type Config struct {
	HTTPAddr    string
	MetricsAddr string
	TunnelAddr  string

	// PublicURL is the canonical https URL operators use to reach this
	// manager from outside the docker network. Used to compose data
	// plane endpoints handed out to edges:
	// e.g. logs plugin pushes to PublicURL + "/loki/api/v1/push".
	// env: OPSKEEPER_PUBLIC_URL (no default — empty disables data plane
	// plugin endpoints, which then prevents edges from being able to
	// push logs/traces).
	PublicURL string

	DB             DBConfig
	JWT            JWTConfig
	OpenAI         OpenAIConfig
	LLM            LLMConfig
	Admin          AdminConfig
	Edge           EdgeConfig
	FrontierClient FrontierClientConfig
	Prom           PromConfig
	Grafana        GrafanaConfig
	Notification   NotificationConfig
	Alert          AlertConfig
	Logs           LogsConfig
	Traces         TracesConfig
	Skills         SkillsConfig
	Redis          RedisConfig
	Leader         LeaderConfig
}

// SkillsConfig wires the manager-side subprocess skill loader. The
// loader scans each directory for skill.json manifests and registers
// them as ScopeManager SubprocessSkills (see internal/skill/loader.go).
//
// Default ExternalDirs is empty unless OPSKEEPER_SKILLS_EXTERNAL_DIRS is
// set; on the deployed image the compose env block points it at
// /etc/opskeeper/skills.
type SkillsConfig struct {
	// ExternalDirs is the comma-/colon-separated list of allowlist
	// roots the loader walks at startup. Each must be an absolute
	// path; relative or non-existent entries are skipped with a log
	// line.
	// env: OPSKEEPER_SKILLS_EXTERNAL_DIRS; default empty.
	ExternalDirs []string
}

// LogsConfig wires the manager-side Loki query proxy. When
// URL is empty the Logs page returns 503 and the SPA shows a "logs
// disabled" state. Built-in deployments default URL to
// http://loki:3100 via the manager docker-compose env block.
type LogsConfig struct {
	// URL is the Loki API root the manager talks to for query_range /
	// labels / label values. env: OPSKEEPER_LOG_QUERY_URL; defaults from
	// docker-compose env block to http://loki:3100.
	URL string
}

// TracesConfig wires the manager-side Tempo query proxy. Mirrors
// LogsConfig — same role for the trace signal. When URL is empty the
// Traces page returns 503 and the SPA shows a "traces disabled" state.
// Built-in deployments default URL to http://tempo:3200 via the manager
// docker-compose env block.
type TracesConfig struct {
	// URL is the Tempo HTTP listener root the manager talks to for
	// /api/search, /api/traces/<id>, and /api/search/tag/<tag>/values.
	// env: OPSKEEPER_TRACE_QUERY_URL; defaults from docker-compose env
	// block to http://tempo:3200.
	URL string
}

// GrafanaConfig holds first-boot seed values for the Grafana integration.
// Runtime values (root URL, SA token) are read from system_settings and
// can be edited live in the UI; this struct only sets defaults applied
// when the corresponding rows are missing on first start.
type GrafanaConfig struct {
	// InternalRootURL is the URL the manager uses to reach Grafana via
	// the docker network. The compose-shipped Grafana lives at
	// http://grafana:3000/grafana (the /grafana sub-path comes from
	// GF_SERVER_SERVE_FROM_SUB_PATH=true). Operators pointing at a
	// real external Grafana override this in the UI on first run.
	// env: OPSKEEPER_GRAFANA_INTERNAL_URL; default http://grafana:3000/grafana.
	InternalRootURL string

	// BootstrapUser / BootstrapPassword are the admin credentials the
	// manager uses ONCE at first boot to auto-create the opskeeper SA +
	// token via the Grafana admin API. Only set for the embedded Grafana
	// shipped in our compose; for a customer-supplied external Grafana
	// these stay empty and bootstrap is skipped — the operator pastes
	// a manually-created token into the UI.
	// env: OPSKEEPER_GRAFANA_BOOTSTRAP_USER (default admin) and
	//      OPSKEEPER_GRAFANA_BOOTSTRAP_PASSWORD (no default — empty disables).
	BootstrapUser     string
	BootstrapPassword string

	// TLSInsecure skips cert verification when calling Grafana from the
	// manager. Symmetric with PromConfig.TLSInsecure — needed when
	// pointing at an external Grafana behind a self-signed cert.
	// env: OPSKEEPER_GRAFANA_TLS_INSECURE; default false.
	TLSInsecure bool
}

// NotificationConfig controls outbound notifications for alerts, scheduled
// tasks, and future AIOps proactive recommendations.
//
// The concrete delivery adapters live in internal/pkg/notify. Keeping only
// plain configuration here avoids coupling config loading to any transport
// client.
type NotificationConfig struct {
	// Enabled gates all outbound notification sends.
	// env: OPSKEEPER_NOTIFY_ENABLED; default true (configured channels deliver
	// out of the box; set false to mute all notifications).
	Enabled bool
	// DefaultChannels is the ordered channel name list used when a caller
	// does not specify explicit destinations.
	// env: OPSKEEPER_NOTIFY_DEFAULT_CHANNELS; comma-separated; default empty.
	DefaultChannels []string
	// Timeout is the per-channel send timeout.
	// env: OPSKEEPER_NOTIFY_TIMEOUT; default 10s.
	Timeout time.Duration

	// "log" channel was removed in 2026-05; alert_events table is the
	// authoritative audit. Webhook / IM channels remain.
	Webhook  NotifyWebhookConfig
	Slack    NotifyWebhookConfig
	Feishu   NotifyWebhookConfig
	DingTalk NotifyWebhookConfig
}

// NotifyWebhookConfig covers webhook-style channels. Slack, Feishu, and
// DingTalk use different payload shapes, but the endpoint/secret material
// they need is the same at config level.
type NotifyWebhookConfig struct {
	Enabled bool
	Name    string
	URL     string
	Secret  string
}

// AlertConfig controls built-in host monitoring alert evaluation. The
// evaluator consumes the closed-set HostMetricPoint fast path, so these
// rules work in both embedded and scrape collector modes.
type AlertConfig struct {
	// Enabled gates built-in host alert evaluation.
	// env: OPSKEEPER_ALERT_ENABLED; default true.
	Enabled bool
	// Cooldown suppresses duplicate notifications for the same edge+rule.
	// env: OPSKEEPER_ALERT_COOLDOWN; default 10m.
	Cooldown time.Duration
	// CPUPercent fires when cpu_pct >= threshold. 0 disables the rule.
	// env: OPSKEEPER_ALERT_CPU_PERCENT; default 90.
	CPUPercent float64
	// MemPercent fires when mem_pct >= threshold. 0 disables the rule.
	// env: OPSKEEPER_ALERT_MEM_PERCENT; default 90.
	MemPercent float64
	// DiskUsedPercent fires when disk_used_pct >= threshold. 0 disables the rule.
	// env: OPSKEEPER_ALERT_DISK_USED_PERCENT; default 90.
	DiskUsedPercent float64
	// Load1 fires when load1 >= threshold. 0 disables the rule.
	// env: OPSKEEPER_ALERT_LOAD1; default 0.
	Load1 float64
	// EvaluatorInterval is how often the pipeline evaluator scans edges
	// and queries Prom for `up` / remote_write health.
	// env: OPSKEEPER_ALERT_EVAL_INTERVAL; default 5m (was 30s pre-2026-05-31 —
	// too noisy on long-firing rules, an always-true rule racked up
	// ~3900 alert_events in 32h). Notification cadence is independently
	// bounded by Alert.Cooldown so stretching this doesn't multiply
	// notifications — only the storage + evaluator-CPU side.
	EvaluatorInterval time.Duration
	// EdgeOfflineThreshold is the heartbeat staleness above which an edge
	// counts as offline.
	// env: OPSKEEPER_ALERT_EDGE_OFFLINE_THRESHOLD; default 90s.
	EdgeOfflineThreshold time.Duration
	// PromIngestFailLimit is the consecutive remote_write failure count at
	// or above which the prom_ingest_fail rule fires.
	// env: OPSKEEPER_ALERT_PROM_INGEST_FAIL_LIMIT; default 5.
	PromIngestFailLimit int
	// WebhookToken authorizes the public Alertmanager ingest endpoint.
	// Empty keeps the endpoint disabled.
	WebhookToken string
}

// PromConfig points the manager at a cloud-side Prometheus instance for
// remote_write ingestion (open-set edge samples) and PromQL queries
// (driven by the AI tool registry).
//
// When Enabled is false the manager runs without Prom: the
// push_prom_samples tunnel handler is still installed but silently drops
// every batch (so edges keep working), and the query_promql AI tool is
// not registered.
type PromConfig struct {
	// Enabled gates all Prom wiring. env: OPSKEEPER_PROM_ENABLED; default false.
	Enabled bool
	// URL is the Prom server root, e.g. "http://prometheus:9090".
	// env: OPSKEEPER_PROM_URL; default "http://prometheus:9090".
	URL string
	// RemoteWriteURL, when set, is the exact remote_write endpoint. This
	// supports Prometheus-compatible TSDBs whose write path is not rooted at
	// /api/v1/write (for example Mimir/Cortex-style gateways).
	// env: OPSKEEPER_PROM_REMOTE_WRITE_URL; default empty.
	RemoteWriteURL string
	// QueryURL is the Prometheus-compatible HTTP API root used by query_promql.
	// If empty it falls back to URL.
	// env: OPSKEEPER_PROM_QUERY_URL; default empty.
	QueryURL string

	// TLSInsecure skips TLS cert verification when talking to the TSDB.
	// env: OPSKEEPER_PROM_TLS_INSECURE; default false.
	TLSInsecure bool

	// TLSCAPath is the path to a PEM file with the root CA used to verify
	// the TSDB's cert. Empty = use system roots.
	// env: OPSKEEPER_PROM_TLS_CA_FILE; default empty.
	TLSCAPath string
}

// FrontierClientConfig drives the manager-side service-end SDK that dials
// the upstream github.com/singchia/frontier broker. The broker itself is
// a separate docker container; this struct only describes how to reach it.
type FrontierClientConfig struct {
	// Addr is the frontier service-bound listen, reached over the docker
	// network, e.g. "frontier:40011".
	Addr string
	// ServiceName is the identifier reported to the frontier on connect
	// via fbsvc.OptionServiceName. Defaults to "opskeeper-manager".
	ServiceName string
	// Disabled skips the long-lived service-end dial to the frontier
	// broker entirely. Set by OPSKEEPER_FRONTIER_DISABLED=true. The e2e
	// harness uses it to bring the manager up without a real geminio
	// broker — features that require fbClient (webssh, edge reverse
	// calls) error at call site rather than failing manager startup.
	Disabled bool
}

// DBConfig selects the backend (MySQL by default, PostgreSQL/SQLite opt-in) and
// carries the parameters for whichever is active. Only the fields matching
// Dialect are consulted at Open time; the others may be empty.
type DBConfig struct {
	// Dialect selects the backend: "mysql" (default), "postgres", or "sqlite".
	// An empty string is treated as "mysql" for defensive defaults.
	Dialect string
	// DSN is the MySQL or PostgreSQL Data Source Name used when Dialect is
	// "mysql" or "postgres".
	// Example: "opskeeper:opskeeper@tcp(127.0.0.1:3306)/opskeeper?parseTime=true&charset=utf8mb4&loc=Local".
	DSN string
	// Path is the sqlite database file path used when Dialect == "sqlite".
	// The special value ":memory:" is accepted for tests.
	Path string
	// Host is the MySQL host. When non-empty the loader composes DSN
	// from the Host/Port/User/Password/DBName/SSLMode fields (which
	// take precedence over the legacy DSN field above); this is the
	// shape the HA Helm chart uses, where credentials live in
	// k8s-managed Secrets.
	// env: OPSKEEPER_DB_HOST; default empty.
	Host string
	// Port is the MySQL port. env: OPSKEEPER_DB_PORT; default 3306.
	Port int
	// User is the MySQL username. env: OPSKEEPER_DB_USER; default empty.
	User string
	// Password is the MySQL password. env: OPSKEEPER_DB_PASSWORD; default empty.
	Password string
	// DBName is the schema name. env: OPSKEEPER_DB_NAME; default empty.
	DBName string
	// SSLMode controls TLS verification on the MySQL connection.
	// Common values: "disable", "require", "verify-ca", "verify-full".
	// env: OPSKEEPER_DB_SSLMODE; default "disable".
	SSLMode string
	// Pool tunes the underlying database/sql pool. Defaults are sized
	// for the single-replica MVP; multi-replica HA deployments should
	// raise MaxOpen and shrink ConnMaxLifetime so load balancers cycle
	// connections out from under rolling upgrades.
	Pool DBPoolConfig
}

// DBPoolConfig groups the database/sql pool tuning knobs that are
// plumbed into *sql.DB.SetMax* on startup. Defaults live in Load().
type DBPoolConfig struct {
	// MaxOpen is the maximum number of open connections. <=0 keeps
	// the driver default (no limit). env: OPSKEEPER_DB_POOL_MAX_OPEN; default 25.
	MaxOpen int
	// MaxIdle is the maximum idle connections in the pool. <=0 keeps
	// the driver default. env: OPSKEEPER_DB_POOL_MAX_IDLE; default 5.
	MaxIdle int
	// ConnMaxLifetime bounds the age of any open connection; the pool
	// retires connections older than this on the next acquire.
	// env: OPSKEEPER_DB_POOL_CONN_MAX_LIFETIME; default 30m.
	ConnMaxLifetime time.Duration
}

// RedisConfig is the runtime config for the manager's Redis client
// (used by internal/pkg/redislock + internal/pkg/leader). When Addr is
// empty the manager skips Redis-dependent features (leader election,
// distributed lock) and degrades to single-replica behaviour; this
// is the historical MVP fallback.
type RedisConfig struct {
	// Addr is the host:port pair the client dials.
	// env: OPSKEEPER_REDIS_ADDR; default "127.0.0.1:6379".
	Addr string
	// Password is the AUTH secret; empty disables AUTH.
	// env: OPSKEEPER_REDIS_PASSWORD; default empty.
	Password string
	// DB is the logical database index (0..15 by default).
	// env: OPSKEEPER_REDIS_DB; default 0.
	DB int
	// Pool sizes the underlying go-redis connection pool.
	Pool RedisPoolConfig
}

// RedisPoolConfig groups the pool knobs plumbed into redis.Options.
// Defaults are sized for a low-traffic MVP; HA deployments should
// raise MaxActive and shorten DialTimeout so a wedged Redis fails
// fast under failover.
type RedisPoolConfig struct {
	// MaxActive bounds total open connections. <=0 lets go-redis use
	// its default (10 * runtime.GOMAXPROCS).
	// env: OPSKEEPER_REDIS_POOL_MAX_ACTIVE; default 50.
	MaxActive int
	// MaxIdle is the maximum number of idle connections kept in the pool.
	// env: OPSKEEPER_REDIS_POOL_MAX_IDLE; default 10.
	MaxIdle int
	// DialTimeout caps the time spent establishing a new connection.
	// env: OPSKEEPER_REDIS_POOL_DIAL_TIMEOUT; default 5s.
	DialTimeout time.Duration
}

// LeaderConfig controls the per-role leader election that ships with
// platform-base-ha. When Enabled is false the manager treats every
// role as locally-active, restoring the historical single-replica
// behaviour — useful for unit tests and the embedded compose.
type LeaderConfig struct {
	// Enabled gates leader election. When false the manager skips
	// running electors and treats every role as locally-active.
	// env: OPSKEEPER_LEADER_ENABLED; default true.
	Enabled bool
	// TTL is the lifetime of a held leader lock in Redis; the elector
	// renews at RenewInterval and treats a missed renewal as leader
	// loss. Must be > 2x RenewInterval to survive a single missed tick.
	// env: OPSKEEPER_LEADER_TTL; default 15s.
	TTL time.Duration
	// RenewInterval is how often the leader renews its lock.
	// env: OPSKEEPER_LEADER_RENEW_INTERVAL; default 5s.
	RenewInterval time.Duration
	// StartTimeout caps the time a registered worker's Start function
	// is allowed to take; the elector waits this long before
	// declaring the start failed and releasing the lock for retry.
	// env: OPSKEEPER_LEADER_START_TIMEOUT; default 30s.
	StartTimeout time.Duration
	// InstanceID overrides the auto-derived instance identity
	// (hostname-pid). Operators rarely need to set this; the helm
	// chart's downward API can do so for cross-pod observability.
	// env: OPSKEEPER_LEADER_INSTANCE_ID; default empty → auto-derive.
	InstanceID string
}

// JWTConfig holds JWT signing / expiry parameters used by the iam bounded context.
type JWTConfig struct {
	Secret     string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

// OpenAIConfig holds credentials / endpoint for the LLM client.
type OpenAIConfig struct {
	APIKey  string
	Model   string
	BaseURL string
}

// LLMProviderConfig is one configured LLM upstream beyond OpenAI. The
// SPA chat input's per-message model selector is populated from the
// configured-and-non-empty subset of these. All fields env-derived to
// keep the bootstrap path uniform with OpenAI; any provider with an
// empty APIKey is silently dropped from the catalog.
type LLMProviderConfig struct {
	APIKey  string
	Model   string   // default model
	BaseURL string   // optional base URL override
	Models  []string // closed-set of models exposed via /v1/aiops/models
}

// LLMConfig groups the multi-provider router config. OpenAI lives in
// its own top-level field for legacy reasons (see OpenAIConfig); the
// non-OpenAI providers cluster here.
type LLMConfig struct {
	Anthropic LLMProviderConfig
	Zhipu     LLMProviderConfig
	Gemini    LLMProviderConfig
	DeepSeek  LLMProviderConfig
	Kimi      LLMProviderConfig
	// Default is the provider id used when a chat-send request does not
	// specify Provider. Empty → first configured provider (deterministic
	// alphabetical ordering, see llm.NewMultiClient).
	Default string
	// DailyTokenLimit is the global per-UTC-day token ceiling enforced
	// against the BudgetChecker callback. <=0 means unlimited (the
	// historical MVP default). Single-tenant scope — when the platform
	// goes multi-tenant the limit moves into per-org settings and this
	// stays as a safety-net global cap.
	// env: OPSKEEPER_LLM_DAILY_TOKEN_LIMIT; default 0 (unlimited).
	DailyTokenLimit int
}

// AdminConfig holds bootstrap admin credentials. Used only by the cloud
// binary at startup to seed the first admin user in a self-managed deployment.
// If either field is empty, no bootstrap is attempted.
type AdminConfig struct {
	Email    string
	Password string
}

// EdgeConfig is consumed only by opskeeper-edge to dial the cloud tunnel.
type EdgeConfig struct {
	CloudAddr string
	AccessKey string
	SecretKey string

	// CollectorMode selects how the edge's periodic metric-push path
	// behaves. Defaults to "off" because the hostmetrics + procmetrics
	// plugins now expose host / per-process metrics to the manager via
	// direct Prometheus scrape — pushing duplicate samples via tunnel
	// produces noisy "opskeeper_source=embedded" extra series in panel
	// legends.
	//
	// Values:
	//	off / "" — no periodic push; on-demand RPCs still work
	//	auto — legacy: embedded (gopsutil push) + optional scraper
	//	embedded — embedded push only
	//	scrape — multi-target HTTP scraper that polls /metrics
	//
	// env: OPSKEEPER_EDGE_COLLECTOR_MODE
	CollectorMode string

	// ScrapeConfigFile is the absolute path to the yaml file describing
	// scrape targets. Only consulted when CollectorMode == "scrape".
	//
	// env: OPSKEEPER_EDGE_SCRAPE_CONFIG_FILE; default /etc/opskeeper-edge/scrape.yaml
	ScrapeConfigFile string

	// CollectorInterval is how often the embedded collector takes a
	// snapshot. Scrape mode ignores this — each target carries its own
	// per-target interval in the yaml.
	//
	// env: OPSKEEPER_EDGE_COLLECTOR_INTERVAL; default 10s
	CollectorInterval time.Duration
}

// Load reads env vars and returns a Config with defaults applied.
// It never returns a non-nil error in MVP; the signature leaves room
// for future validation (e.g. required fields).
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:    getEnv("OPSKEEPER_HTTP_ADDR", ":8080"),
		MetricsAddr: getEnv("OPSKEEPER_METRICS_ADDR", ":9100"),
		TunnelAddr:  getEnv("OPSKEEPER_TUNNEL_ADDR", ":40012"),
		PublicURL:   getEnv("OPSKEEPER_PUBLIC_URL", ""),
		Logs:        LogsConfig{URL: getEnv("OPSKEEPER_LOG_QUERY_URL", "http://loki:3100")},
		Traces:      TracesConfig{URL: getEnv("OPSKEEPER_TRACE_QUERY_URL", "http://tempo:3200")},
	}

	c.DB.Dialect = getEnv("OPSKEEPER_DB_DIALECT", "mysql")
	c.DB.DSN = getEnv(
		"OPSKEEPER_DB_DSN",
		"opskeeper:opskeeper@tcp(127.0.0.1:3306)/opskeeper?parseTime=true&charset=utf8mb4&loc=Local",
	)
	c.DB.Path = getEnv("OPSKEEPER_DB_PATH", "./data/opskeeper.db")

	c.JWT.Secret = getEnv("OPSKEEPER_JWT_SECRET", "dev-insecure-secret-change-me")
	c.JWT.AccessTTL = getEnvDuration("OPSKEEPER_JWT_ACCESS_TTL", 15*time.Minute)
	c.JWT.RefreshTTL = getEnvDuration("OPSKEEPER_JWT_REFRESH_TTL", 7*24*time.Hour)

	c.OpenAI.APIKey = getEnv("OPSKEEPER_OPENAI_API_KEY", "")
	c.OpenAI.Model = getEnv("OPSKEEPER_OPENAI_MODEL", "gpt-5.4")
	c.OpenAI.BaseURL = getEnv("OPSKEEPER_OPENAI_BASE_URL", "")

	// Multi-provider LLM config (HLD: ChatInput model selector). Each
	// provider is gated by its own API key — empty key = provider not
	// surfaced to the SPA's selector. BaseURL defaults are pre-baked at
	// the wiring site so operators can drop in just the API key.
	c.LLM.Anthropic.APIKey = getEnv("OPSKEEPER_ANTHROPIC_API_KEY", "")
	c.LLM.Anthropic.Model = getEnv("OPSKEEPER_ANTHROPIC_MODEL", "claude-sonnet-4-6")
	c.LLM.Anthropic.BaseURL = getEnv("OPSKEEPER_ANTHROPIC_BASE_URL", "")
	c.LLM.Anthropic.Models = splitProviderModels(getEnv("OPSKEEPER_ANTHROPIC_MODELS", "claude-opus-4-7,claude-sonnet-4-6,claude-haiku-4-5"))
	c.LLM.Zhipu.APIKey = getEnv("OPSKEEPER_ZHIPU_API_KEY", "")
	c.LLM.Zhipu.Model = getEnv("OPSKEEPER_ZHIPU_MODEL", "glm-4.7")
	c.LLM.Zhipu.BaseURL = getEnv("OPSKEEPER_ZHIPU_BASE_URL", "")
	c.LLM.Zhipu.Models = splitProviderModels(getEnv("OPSKEEPER_ZHIPU_MODELS", "glm-5.1,glm-5,glm-4.7,glm-4.7-flash"))
	c.LLM.Gemini.APIKey = getEnv("OPSKEEPER_GEMINI_API_KEY", "")
	c.LLM.Gemini.Model = getEnv("OPSKEEPER_GEMINI_MODEL", "gemini-2.5-pro")
	c.LLM.Gemini.BaseURL = getEnv("OPSKEEPER_GEMINI_BASE_URL", "")
	c.LLM.Gemini.Models = splitProviderModels(getEnv("OPSKEEPER_GEMINI_MODELS", "gemini-3.5-flash,gemini-2.5-pro,gemini-2.5-flash"))
	c.LLM.DeepSeek.APIKey = getEnv("OPSKEEPER_DEEPSEEK_API_KEY", "")
	c.LLM.DeepSeek.Model = getEnv("OPSKEEPER_DEEPSEEK_MODEL", "deepseek-v4-flash")
	c.LLM.DeepSeek.BaseURL = getEnv("OPSKEEPER_DEEPSEEK_BASE_URL", "")
	c.LLM.DeepSeek.Models = splitProviderModels(getEnv("OPSKEEPER_DEEPSEEK_MODELS", "deepseek-v4-pro,deepseek-v4-flash,deepseek-reasoner"))
	c.LLM.Kimi.APIKey = getEnv("OPSKEEPER_KIMI_API_KEY", "")
	c.LLM.Kimi.Model = getEnv("OPSKEEPER_KIMI_MODEL", "kimi-k2.6")
	c.LLM.Kimi.BaseURL = getEnv("OPSKEEPER_KIMI_BASE_URL", "")
	c.LLM.Kimi.Models = splitProviderModels(getEnv("OPSKEEPER_KIMI_MODELS", "kimi-k2.6,kimi-k2.5,moonshot-v1-128k"))
	c.LLM.Default = getEnv("OPSKEEPER_LLM_DEFAULT_PROVIDER", "")
	c.LLM.DailyTokenLimit = getEnvInt("OPSKEEPER_LLM_DAILY_TOKEN_LIMIT", 0)

	c.Admin.Email = getEnv("OPSKEEPER_ADMIN_EMAIL", "")
	c.Admin.Password = getEnv("OPSKEEPER_ADMIN_PASSWORD", "")

	c.Edge.CloudAddr = getEnv("OPSKEEPER_EDGE_CLOUD_ADDR", "127.0.0.1:40012")
	c.Edge.AccessKey = getEnv("OPSKEEPER_EDGE_ACCESS_KEY", "")
	c.Edge.SecretKey = getEnv("OPSKEEPER_EDGE_SECRET_KEY", "")
	c.Edge.CollectorMode = getEnv("OPSKEEPER_EDGE_COLLECTOR_MODE", "off")
	c.Edge.ScrapeConfigFile = getEnv("OPSKEEPER_EDGE_SCRAPE_CONFIG_FILE", "/etc/opskeeper-edge/scrape.yaml")
	c.Edge.CollectorInterval = getEnvDuration("OPSKEEPER_EDGE_COLLECTOR_INTERVAL", 10*time.Second)

	c.FrontierClient.Addr = getEnv("OPSKEEPER_FRONTIER_ADDR", "frontier:40011")
	c.FrontierClient.ServiceName = getEnv("OPSKEEPER_FRONTIER_SERVICE_NAME", "opskeeper-manager")
	c.FrontierClient.Disabled = getEnvBool("OPSKEEPER_FRONTIER_DISABLED", false)

	c.Prom.Enabled = getEnvBool("OPSKEEPER_PROM_ENABLED", false)
	c.Prom.URL = getEnv("OPSKEEPER_PROM_URL", "http://prometheus:9090")
	c.Prom.RemoteWriteURL = getEnv("OPSKEEPER_PROM_REMOTE_WRITE_URL", "")
	c.Prom.QueryURL = getEnv("OPSKEEPER_PROM_QUERY_URL", "")
	c.Prom.TLSInsecure = getEnvBool("OPSKEEPER_PROM_TLS_INSECURE", false)
	c.Prom.TLSCAPath = getEnv("OPSKEEPER_PROM_TLS_CA_FILE", "")
	c.Grafana.InternalRootURL = getEnv("OPSKEEPER_GRAFANA_INTERNAL_URL", "http://grafana:3000/grafana")
	c.Grafana.BootstrapUser = getEnv("OPSKEEPER_GRAFANA_BOOTSTRAP_USER", "admin")
	c.Grafana.BootstrapPassword = getEnv("OPSKEEPER_GRAFANA_BOOTSTRAP_PASSWORD", "")
	c.Grafana.TLSInsecure = getEnvBool("OPSKEEPER_GRAFANA_TLS_INSECURE", false)

	// Default ON: notifications are allowed out of the box, so any channel
	// the operator configures (UI or env) delivers without flipping a
	// master switch. Per-channel env enables (Slack/Feishu/... below) stay
	// OFF by default on purpose — seeding an enabled-but-URL-less channel
	// would clutter the UI; UI-created channels carry their own enabled flag.
	c.Notification.Enabled = getEnvBool("OPSKEEPER_NOTIFY_ENABLED", true)
	// Default channels list defaults empty — operators can pin specific
	// channel names per env if desired. The legacy "log" entry was
	// removed alongside the log channel type.
	c.Notification.DefaultChannels = getEnvCSV("OPSKEEPER_NOTIFY_DEFAULT_CHANNELS", nil)
	c.Notification.Timeout = getEnvDuration("OPSKEEPER_NOTIFY_TIMEOUT", 10*time.Second)
	c.Notification.Webhook.Enabled = getEnvBool("OPSKEEPER_NOTIFY_WEBHOOK_ENABLED", false)
	c.Notification.Webhook.Name = getEnv("OPSKEEPER_NOTIFY_WEBHOOK_NAME", "webhook")
	c.Notification.Webhook.URL = getEnv("OPSKEEPER_NOTIFY_WEBHOOK_URL", "")
	c.Notification.Webhook.Secret = getEnv("OPSKEEPER_NOTIFY_WEBHOOK_SECRET", "")
	c.Notification.Slack.Enabled = getEnvBool("OPSKEEPER_NOTIFY_SLACK_ENABLED", false)
	c.Notification.Slack.Name = getEnv("OPSKEEPER_NOTIFY_SLACK_NAME", "slack")
	c.Notification.Slack.URL = getEnv("OPSKEEPER_NOTIFY_SLACK_WEBHOOK_URL", "")
	c.Notification.Feishu.Enabled = getEnvBool("OPSKEEPER_NOTIFY_FEISHU_ENABLED", false)
	c.Notification.Feishu.Name = getEnv("OPSKEEPER_NOTIFY_FEISHU_NAME", "feishu")
	c.Notification.Feishu.URL = getEnv("OPSKEEPER_NOTIFY_FEISHU_WEBHOOK_URL", "")
	c.Notification.Feishu.Secret = getEnv("OPSKEEPER_NOTIFY_FEISHU_SECRET", "")
	c.Notification.DingTalk.Enabled = getEnvBool("OPSKEEPER_NOTIFY_DINGTALK_ENABLED", false)
	c.Notification.DingTalk.Name = getEnv("OPSKEEPER_NOTIFY_DINGTALK_NAME", "dingtalk")
	c.Notification.DingTalk.URL = getEnv("OPSKEEPER_NOTIFY_DINGTALK_WEBHOOK_URL", "")
	c.Notification.DingTalk.Secret = getEnv("OPSKEEPER_NOTIFY_DINGTALK_SECRET", "")

	c.Alert.Enabled = getEnvBool("OPSKEEPER_ALERT_ENABLED", true)
	c.Alert.Cooldown = getEnvDuration("OPSKEEPER_ALERT_COOLDOWN", 10*time.Minute)
	c.Alert.CPUPercent = getEnvFloat("OPSKEEPER_ALERT_CPU_PERCENT", 90)
	c.Alert.MemPercent = getEnvFloat("OPSKEEPER_ALERT_MEM_PERCENT", 90)
	c.Alert.DiskUsedPercent = getEnvFloat("OPSKEEPER_ALERT_DISK_USED_PERCENT", 90)
	c.Alert.Load1 = getEnvFloat("OPSKEEPER_ALERT_LOAD1", 0)
	c.Alert.EvaluatorInterval = getEnvDuration("OPSKEEPER_ALERT_EVAL_INTERVAL", 5*time.Minute)
	c.Alert.EdgeOfflineThreshold = getEnvDuration("OPSKEEPER_ALERT_EDGE_OFFLINE_THRESHOLD", 90*time.Second)
	c.Alert.PromIngestFailLimit = getEnvInt("OPSKEEPER_ALERT_PROM_INGEST_FAIL_LIMIT", 5)
	c.Alert.WebhookToken = getEnv("OPSKEEPER_ALERT_WEBHOOK_TOKEN", "")

	c.Skills.ExternalDirs = getEnvCSV("OPSKEEPER_SKILLS_EXTERNAL_DIRS", nil)

	// Redis (platform-base-ha). Empty Addr preserves the historical
	// single-replica behaviour — leader election, distributed locks,
	// and the cross-replica state registry are all gated on Addr being
	// non-empty at the wiring site.
	c.Redis.Addr = getEnv("OPSKEEPER_REDIS_ADDR", "127.0.0.1:6379")
	c.Redis.Password = getEnv("OPSKEEPER_REDIS_PASSWORD", "")
	c.Redis.DB = getEnvInt("OPSKEEPER_REDIS_DB", 0)
	c.Redis.Pool.MaxActive = getEnvInt("OPSKEEPER_REDIS_POOL_MAX_ACTIVE", 50)
	c.Redis.Pool.MaxIdle = getEnvInt("OPSKEEPER_REDIS_POOL_MAX_IDLE", 10)
	c.Redis.Pool.DialTimeout = getEnvDuration("OPSKEEPER_REDIS_POOL_DIAL_TIMEOUT", 5*time.Second)

	// Leader election (platform-base-ha). Default Enabled is true so
	// multi-replica HA deployments get leader election out of the box;
	// unit tests and the embedded compose flip this off.
	c.Leader.Enabled = getEnvBool("OPSKEEPER_LEADER_ENABLED", true)
	c.Leader.TTL = getEnvDuration("OPSKEEPER_LEADER_TTL", 15*time.Second)
	c.Leader.RenewInterval = getEnvDuration("OPSKEEPER_LEADER_RENEW_INTERVAL", 5*time.Second)
	c.Leader.StartTimeout = getEnvDuration("OPSKEEPER_LEADER_START_TIMEOUT", 30*time.Second)
	c.Leader.InstanceID = getEnv("OPSKEEPER_LEADER_INSTANCE_ID", "")

	// External MySQL composition (platform-base-ha). When the helm
	// chart wires OPSKEEPER_DB_HOST (e.g. via a Secret projected to env
	// vars) we compose the DSN from discrete fields; otherwise the
	// legacy OPSKEEPER_DB_DSN value (set just above) is preserved.
	c.DB.Host = getEnv("OPSKEEPER_DB_HOST", "")
	c.DB.Port = getEnvInt("OPSKEEPER_DB_PORT", 3306)
	c.DB.User = getEnv("OPSKEEPER_DB_USER", "")
	c.DB.Password = getEnv("OPSKEEPER_DB_PASSWORD", "")
	c.DB.DBName = getEnv("OPSKEEPER_DB_NAME", "")
	c.DB.SSLMode = getEnv("OPSKEEPER_DB_SSLMODE", "disable")
	if c.DB.Host != "" {
		c.DB.DSN = buildMySQLDSN(c.DB)
	}
	c.DB.Pool.MaxOpen = getEnvInt("OPSKEEPER_DB_POOL_MAX_OPEN", 25)
	c.DB.Pool.MaxIdle = getEnvInt("OPSKEEPER_DB_POOL_MAX_IDLE", 5)
	c.DB.Pool.ConnMaxLifetime = getEnvDuration("OPSKEEPER_DB_POOL_CONN_MAX_LIFETIME", 30*time.Minute)

	return c, nil
}

// buildMySQLDSN composes a DSN from the discrete Host/Port/User/Password/DBName/SSLMode
// fields populated by the platform-base-ha Helm chart. The shape matches the
// example embedded in the legacy OPSKEEPER_DB_DSN default so callers can swap
// seamlessly between the two env-var strategies.
func buildMySQLDSN(d DBConfig) string {
	host := d.Host
	if d.Port != 0 {
		host = net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
	}
	params := "parseTime=true&charset=utf8mb4&loc=Local"
	if d.SSLMode != "" {
		params += "&tls=" + d.SSLMode
	}
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?%s", d.User, d.Password, host, d.DBName, params)
}

// getEnvBool parses a boolean env var. Accepts the usual strconv.ParseBool
// values (1/0, t/f, true/false, TRUE/FALSE …); any other value returns def.
func getEnvBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return def
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// splitProviderModels parses a comma-separated list of model slugs into
// a trimmed, dedup'd slice. Empty input returns nil so the wiring site
// can decide whether to fall back to the default model only.
func splitProviderModels(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';'
	})
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func getEnvCSV(key string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	parts := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ';'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func getEnvInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	// Fall back to integer seconds for convenience.
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}

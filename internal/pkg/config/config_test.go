package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// Clear all OPSKEEPER_* vars so we test defaults deterministically.
	vars := []string{
		"OPSKEEPER_HTTP_ADDR", "OPSKEEPER_METRICS_ADDR", "OPSKEEPER_TUNNEL_ADDR",
		"OPSKEEPER_DB_DIALECT", "OPSKEEPER_DB_DSN", "OPSKEEPER_DB_PATH",
		"OPSKEEPER_JWT_SECRET", "OPSKEEPER_JWT_ACCESS_TTL", "OPSKEEPER_JWT_REFRESH_TTL",
		"OPSKEEPER_OPENAI_API_KEY", "OPSKEEPER_OPENAI_MODEL", "OPSKEEPER_OPENAI_BASE_URL",
		"OPSKEEPER_ADMIN_EMAIL", "OPSKEEPER_ADMIN_PASSWORD",
		"OPSKEEPER_EDGE_CLOUD_ADDR", "OPSKEEPER_EDGE_ACCESS_KEY", "OPSKEEPER_EDGE_SECRET_KEY",
		"OPSKEEPER_EDGE_COLLECTOR_MODE", "OPSKEEPER_EDGE_SCRAPE_CONFIG_FILE", "OPSKEEPER_EDGE_COLLECTOR_INTERVAL",
		"OPSKEEPER_FRONTIER_ADDR", "OPSKEEPER_FRONTIER_SERVICE_NAME",
		"OPSKEEPER_PROM_ENABLED", "OPSKEEPER_PROM_URL", "OPSKEEPER_PROM_REMOTE_WRITE_URL", "OPSKEEPER_PROM_QUERY_URL",
		"OPSKEEPER_NOTIFY_ENABLED", "OPSKEEPER_NOTIFY_DEFAULT_CHANNELS", "OPSKEEPER_NOTIFY_TIMEOUT",
		"OPSKEEPER_NOTIFY_LOG_ENABLED", "OPSKEEPER_NOTIFY_LOG_NAME",
		"OPSKEEPER_NOTIFY_WEBHOOK_ENABLED", "OPSKEEPER_NOTIFY_WEBHOOK_NAME", "OPSKEEPER_NOTIFY_WEBHOOK_URL", "OPSKEEPER_NOTIFY_WEBHOOK_SECRET",
		"OPSKEEPER_NOTIFY_SLACK_ENABLED", "OPSKEEPER_NOTIFY_SLACK_NAME", "OPSKEEPER_NOTIFY_SLACK_WEBHOOK_URL",
		"OPSKEEPER_NOTIFY_FEISHU_ENABLED", "OPSKEEPER_NOTIFY_FEISHU_NAME", "OPSKEEPER_NOTIFY_FEISHU_WEBHOOK_URL", "OPSKEEPER_NOTIFY_FEISHU_SECRET",
		"OPSKEEPER_NOTIFY_DINGTALK_ENABLED", "OPSKEEPER_NOTIFY_DINGTALK_NAME", "OPSKEEPER_NOTIFY_DINGTALK_WEBHOOK_URL", "OPSKEEPER_NOTIFY_DINGTALK_SECRET",
		"OPSKEEPER_ALERT_ENABLED", "OPSKEEPER_ALERT_COOLDOWN", "OPSKEEPER_ALERT_CPU_PERCENT", "OPSKEEPER_ALERT_MEM_PERCENT",
		"OPSKEEPER_ALERT_DISK_USED_PERCENT", "OPSKEEPER_ALERT_LOAD1",
	}
	for _, k := range vars {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr default = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.MetricsAddr != ":9100" {
		t.Errorf("MetricsAddr default = %q, want :9100", cfg.MetricsAddr)
	}
	if cfg.TunnelAddr != ":40012" {
		t.Errorf("TunnelAddr default = %q, want :40012", cfg.TunnelAddr)
	}
	if cfg.DB.Dialect != "mysql" {
		t.Errorf("DB.Dialect default = %q, want mysql", cfg.DB.Dialect)
	}
	wantDSN := "opskeeper:opskeeper@tcp(127.0.0.1:3306)/opskeeper?parseTime=true&charset=utf8mb4&loc=Local"
	if cfg.DB.DSN != wantDSN {
		t.Errorf("DB.DSN default = %q, want %q", cfg.DB.DSN, wantDSN)
	}
	if cfg.DB.Path != "./data/opskeeper.db" {
		t.Errorf("DB.Path default = %q, want ./data/opskeeper.db", cfg.DB.Path)
	}
	if cfg.JWT.AccessTTL != 15*time.Minute {
		t.Errorf("JWT.AccessTTL default = %v, want 15m", cfg.JWT.AccessTTL)
	}
	if cfg.JWT.RefreshTTL != 7*24*time.Hour {
		t.Errorf("JWT.RefreshTTL default = %v, want 168h", cfg.JWT.RefreshTTL)
	}
	if cfg.OpenAI.Model != "gpt-5.4" {
		t.Errorf("OpenAI.Model default = %q, want gpt-5.4", cfg.OpenAI.Model)
	}
	if cfg.Admin.Email != "" {
		t.Errorf("Admin.Email default = %q, want empty", cfg.Admin.Email)
	}
	if cfg.Admin.Password != "" {
		t.Errorf("Admin.Password default = %q, want empty", cfg.Admin.Password)
	}
	if cfg.FrontierClient.Addr != "frontier:40011" {
		t.Errorf("FrontierClient.Addr default = %q, want frontier:40011", cfg.FrontierClient.Addr)
	}
	if cfg.FrontierClient.ServiceName != "opskeeper-manager" {
		t.Errorf("FrontierClient.ServiceName default = %q, want opskeeper-manager", cfg.FrontierClient.ServiceName)
	}
	if cfg.Edge.CollectorMode != "off" {
		t.Errorf("Edge.CollectorMode default = %q, want off", cfg.Edge.CollectorMode)
	}
	if cfg.Edge.ScrapeConfigFile != "/etc/opskeeper-edge/scrape.yaml" {
		t.Errorf("Edge.ScrapeConfigFile default = %q, want /etc/opskeeper-edge/scrape.yaml", cfg.Edge.ScrapeConfigFile)
	}
	if cfg.Edge.CollectorInterval != 10*time.Second {
		t.Errorf("Edge.CollectorInterval default = %v, want 10s", cfg.Edge.CollectorInterval)
	}
	if cfg.Prom.Enabled {
		t.Errorf("Prom.Enabled default = true, want false")
	}
	if cfg.Prom.URL != "http://prometheus:9090" {
		t.Errorf("Prom.URL default = %q, want http://prometheus:9090", cfg.Prom.URL)
	}
	if cfg.Prom.RemoteWriteURL != "" {
		t.Errorf("Prom.RemoteWriteURL default = %q, want empty", cfg.Prom.RemoteWriteURL)
	}
	if cfg.Prom.QueryURL != "" {
		t.Errorf("Prom.QueryURL default = %q, want empty", cfg.Prom.QueryURL)
	}
	if !cfg.Notification.Enabled {
		t.Errorf("Notification.Enabled default = false, want true (notifications allowed by default; configured channels deliver)")
	}
	if cfg.Notification.Timeout != 10*time.Second {
		t.Errorf("Notification.Timeout default = %v, want 10s", cfg.Notification.Timeout)
	}
	if len(cfg.Notification.DefaultChannels) != 0 {
		t.Errorf("Notification.DefaultChannels default = %#v, want empty (log channel removed 2026-05)", cfg.Notification.DefaultChannels)
	}
	if cfg.Notification.Webhook.Enabled {
		t.Errorf("Notification.Webhook.Enabled default = true, want false")
	}
	if cfg.Notification.Slack.Enabled {
		t.Errorf("Notification.Slack.Enabled default = true, want false")
	}
	if cfg.Notification.Feishu.Enabled {
		t.Errorf("Notification.Feishu.Enabled default = true, want false")
	}
	if cfg.Notification.DingTalk.Enabled {
		t.Errorf("Notification.DingTalk.Enabled default = true, want false")
	}
	if !cfg.Alert.Enabled {
		t.Errorf("Alert.Enabled default = false, want true")
	}
	if cfg.Alert.Cooldown != 10*time.Minute {
		t.Errorf("Alert.Cooldown default = %v, want 10m", cfg.Alert.Cooldown)
	}
	if cfg.Alert.CPUPercent != 90 {
		t.Errorf("Alert.CPUPercent default = %v, want 90", cfg.Alert.CPUPercent)
	}
	if cfg.Alert.MemPercent != 90 {
		t.Errorf("Alert.MemPercent default = %v, want 90", cfg.Alert.MemPercent)
	}
	if cfg.Alert.DiskUsedPercent != 90 {
		t.Errorf("Alert.DiskUsedPercent default = %v, want 90", cfg.Alert.DiskUsedPercent)
	}
	if cfg.Alert.Load1 != 0 {
		t.Errorf("Alert.Load1 default = %v, want 0", cfg.Alert.Load1)
	}
}

// TestLoadPromOverrides exercises the new Prom env vars.
func TestLoadPromOverrides(t *testing.T) {
	t.Setenv("OPSKEEPER_PROM_ENABLED", "true")
	t.Setenv("OPSKEEPER_PROM_URL", "http://prom-staging:9090")
	t.Setenv("OPSKEEPER_PROM_REMOTE_WRITE_URL", "http://victoriametrics:8428/api/v1/write")
	t.Setenv("OPSKEEPER_PROM_QUERY_URL", "http://thanos-query:9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Prom.Enabled {
		t.Errorf("Prom.Enabled = false, want true")
	}
	if cfg.Prom.URL != "http://prom-staging:9090" {
		t.Errorf("Prom.URL = %q", cfg.Prom.URL)
	}
	if cfg.Prom.RemoteWriteURL != "http://victoriametrics:8428/api/v1/write" {
		t.Errorf("Prom.RemoteWriteURL = %q", cfg.Prom.RemoteWriteURL)
	}
	if cfg.Prom.QueryURL != "http://thanos-query:9090" {
		t.Errorf("Prom.QueryURL = %q", cfg.Prom.QueryURL)
	}
}

// TestLoadEdgeCollectorOverrides exercises the new env vars added for the
// embedded/scrape mode split.
func TestLoadEdgeCollectorOverrides(t *testing.T) {
	t.Setenv("OPSKEEPER_EDGE_COLLECTOR_MODE", "scrape")
	t.Setenv("OPSKEEPER_EDGE_SCRAPE_CONFIG_FILE", "/opt/opskeeper/scrape.yaml")
	t.Setenv("OPSKEEPER_EDGE_COLLECTOR_INTERVAL", "30s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Edge.CollectorMode != "scrape" {
		t.Errorf("Edge.CollectorMode = %q", cfg.Edge.CollectorMode)
	}
	if cfg.Edge.ScrapeConfigFile != "/opt/opskeeper/scrape.yaml" {
		t.Errorf("Edge.ScrapeConfigFile = %q", cfg.Edge.ScrapeConfigFile)
	}
	if cfg.Edge.CollectorInterval != 30*time.Second {
		t.Errorf("Edge.CollectorInterval = %v", cfg.Edge.CollectorInterval)
	}
}

func TestLoadNotificationOverrides(t *testing.T) {
	t.Setenv("OPSKEEPER_NOTIFY_ENABLED", "true")
	t.Setenv("OPSKEEPER_NOTIFY_DEFAULT_CHANNELS", "slack, feishu;webhook")
	t.Setenv("OPSKEEPER_NOTIFY_TIMEOUT", "15s")
	t.Setenv("OPSKEEPER_NOTIFY_WEBHOOK_ENABLED", "true")
	t.Setenv("OPSKEEPER_NOTIFY_WEBHOOK_NAME", "ops-webhook")
	t.Setenv("OPSKEEPER_NOTIFY_WEBHOOK_URL", "https://example.com/notify")
	t.Setenv("OPSKEEPER_NOTIFY_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("OPSKEEPER_NOTIFY_SLACK_ENABLED", "true")
	t.Setenv("OPSKEEPER_NOTIFY_SLACK_WEBHOOK_URL", "https://hooks.slack.test/services/xxx")
	t.Setenv("OPSKEEPER_NOTIFY_FEISHU_ENABLED", "true")
	t.Setenv("OPSKEEPER_NOTIFY_FEISHU_WEBHOOK_URL", "https://open.feishu.test/hook/xxx")
	t.Setenv("OPSKEEPER_NOTIFY_FEISHU_SECRET", "feishu-secret")
	t.Setenv("OPSKEEPER_NOTIFY_DINGTALK_ENABLED", "true")
	t.Setenv("OPSKEEPER_NOTIFY_DINGTALK_WEBHOOK_URL", "https://oapi.dingtalk.test/robot/send")
	t.Setenv("OPSKEEPER_NOTIFY_DINGTALK_SECRET", "dingtalk-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Notification.Enabled {
		t.Errorf("Notification.Enabled = false, want true")
	}
	wantChannels := []string{"slack", "feishu", "webhook"}
	if len(cfg.Notification.DefaultChannels) != len(wantChannels) {
		t.Fatalf("Notification.DefaultChannels = %#v, want %#v", cfg.Notification.DefaultChannels, wantChannels)
	}
	for i, want := range wantChannels {
		if cfg.Notification.DefaultChannels[i] != want {
			t.Errorf("Notification.DefaultChannels[%d] = %q, want %q", i, cfg.Notification.DefaultChannels[i], want)
		}
	}
	if cfg.Notification.Timeout != 15*time.Second {
		t.Errorf("Notification.Timeout = %v, want 15s", cfg.Notification.Timeout)
	}
	if !cfg.Notification.Webhook.Enabled || cfg.Notification.Webhook.Name != "ops-webhook" {
		t.Errorf("Notification.Webhook = %+v", cfg.Notification.Webhook)
	}
	if cfg.Notification.Webhook.URL != "https://example.com/notify" {
		t.Errorf("Notification.Webhook.URL = %q", cfg.Notification.Webhook.URL)
	}
	if cfg.Notification.Webhook.Secret != "webhook-secret" {
		t.Errorf("Notification.Webhook.Secret not loaded")
	}
	if !cfg.Notification.Slack.Enabled || cfg.Notification.Slack.URL == "" {
		t.Errorf("Notification.Slack = %+v", cfg.Notification.Slack)
	}
	if !cfg.Notification.Feishu.Enabled || cfg.Notification.Feishu.Secret != "feishu-secret" {
		t.Errorf("Notification.Feishu = %+v", cfg.Notification.Feishu)
	}
	if !cfg.Notification.DingTalk.Enabled || cfg.Notification.DingTalk.Secret != "dingtalk-secret" {
		t.Errorf("Notification.DingTalk = %+v", cfg.Notification.DingTalk)
	}
}

func TestLoadAlertOverrides(t *testing.T) {
	t.Setenv("OPSKEEPER_ALERT_ENABLED", "false")
	t.Setenv("OPSKEEPER_ALERT_COOLDOWN", "30m")
	t.Setenv("OPSKEEPER_ALERT_CPU_PERCENT", "85.5")
	t.Setenv("OPSKEEPER_ALERT_MEM_PERCENT", "88")
	t.Setenv("OPSKEEPER_ALERT_DISK_USED_PERCENT", "92")
	t.Setenv("OPSKEEPER_ALERT_LOAD1", "8")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alert.Enabled {
		t.Errorf("Alert.Enabled = true, want false")
	}
	if cfg.Alert.Cooldown != 30*time.Minute {
		t.Errorf("Alert.Cooldown = %v, want 30m", cfg.Alert.Cooldown)
	}
	if cfg.Alert.CPUPercent != 85.5 {
		t.Errorf("Alert.CPUPercent = %v, want 85.5", cfg.Alert.CPUPercent)
	}
	if cfg.Alert.MemPercent != 88 {
		t.Errorf("Alert.MemPercent = %v, want 88", cfg.Alert.MemPercent)
	}
	if cfg.Alert.DiskUsedPercent != 92 {
		t.Errorf("Alert.DiskUsedPercent = %v, want 92", cfg.Alert.DiskUsedPercent)
	}
	if cfg.Alert.Load1 != 8 {
		t.Errorf("Alert.Load1 = %v, want 8", cfg.Alert.Load1)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("OPSKEEPER_DB_DIALECT", "sqlite")
	t.Setenv("OPSKEEPER_DB_DSN", "u:p@tcp(db:3306)/opskeeper")
	t.Setenv("OPSKEEPER_DB_PATH", "/var/lib/opskeeper/db.sqlite")
	t.Setenv("OPSKEEPER_ADMIN_EMAIL", "root@example.com")
	t.Setenv("OPSKEEPER_ADMIN_PASSWORD", "s3cret")
	t.Setenv("OPSKEEPER_FRONTIER_ADDR", "frontier-staging:31011")
	t.Setenv("OPSKEEPER_FRONTIER_SERVICE_NAME", "opskeeper-manager-staging")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.Dialect != "sqlite" {
		t.Errorf("DB.Dialect = %q", cfg.DB.Dialect)
	}
	if cfg.DB.DSN != "u:p@tcp(db:3306)/opskeeper" {
		t.Errorf("DB.DSN = %q", cfg.DB.DSN)
	}
	if cfg.DB.Path != "/var/lib/opskeeper/db.sqlite" {
		t.Errorf("DB.Path = %q", cfg.DB.Path)
	}
	if cfg.Admin.Email != "root@example.com" {
		t.Errorf("Admin.Email = %q", cfg.Admin.Email)
	}
	if cfg.Admin.Password != "s3cret" {
		t.Errorf("Admin.Password = %q", cfg.Admin.Password)
	}
	if cfg.FrontierClient.Addr != "frontier-staging:31011" {
		t.Errorf("FrontierClient.Addr = %q", cfg.FrontierClient.Addr)
	}
	if cfg.FrontierClient.ServiceName != "opskeeper-manager-staging" {
		t.Errorf("FrontierClient.ServiceName = %q", cfg.FrontierClient.ServiceName)
	}
}

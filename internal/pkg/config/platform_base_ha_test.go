package config

import (
	"strings"
	"testing"
	"time"
)

// clearPlatformBaseHAVars resets every OPSKEEPER_* env var that the
// platform-base-ha config fields consume, so each test starts from a
// deterministic default state. Mirrors the pattern in TestLoadDefaults
// but kept in one helper to avoid drift as new fields are added.
func clearPlatformBaseHAVars(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OPSKEEPER_DB_HOST", "OPSKEEPER_DB_PORT", "OPSKEEPER_DB_USER", "OPSKEEPER_DB_PASSWORD",
		"OPSKEEPER_DB_NAME", "OPSKEEPER_DB_SSLMODE",
		"OPSKEEPER_DB_POOL_MAX_OPEN", "OPSKEEPER_DB_POOL_MAX_IDLE", "OPSKEEPER_DB_POOL_CONN_MAX_LIFETIME",
		"OPSKEEPER_REDIS_ADDR", "OPSKEEPER_REDIS_PASSWORD", "OPSKEEPER_REDIS_DB",
		"OPSKEEPER_REDIS_POOL_MAX_ACTIVE", "OPSKEEPER_REDIS_POOL_MAX_IDLE", "OPSKEEPER_REDIS_POOL_DIAL_TIMEOUT",
		"OPSKEEPER_LEADER_ENABLED", "OPSKEEPER_LEADER_TTL", "OPSKEEPER_LEADER_RENEW_INTERVAL",
		"OPSKEEPER_LEADER_START_TIMEOUT", "OPSKEEPER_LEADER_INSTANCE_ID",
	} {
		t.Setenv(k, "")
	}
}

func TestDatabaseFieldsDefaults(t *testing.T) {
	clearPlatformBaseHAVars(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.Host != "" {
		t.Errorf("DB.Host default = %q, want empty", cfg.DB.Host)
	}
	// Port/SSLMode have safe defaults even when Host is unset, so
	// the values are plumbed through to buildMySQLDSN the moment an
	// operator does set Host — no half-configured state.
	if cfg.DB.Port != 3306 {
		t.Errorf("DB.Port default = %d, want 3306", cfg.DB.Port)
	}
	if cfg.DB.SSLMode != "disable" {
		t.Errorf("DB.SSLMode default = %q, want disable", cfg.DB.SSLMode)
	}
	if cfg.DB.Pool.MaxOpen != 25 {
		t.Errorf("DB.Pool.MaxOpen default = %d, want 25", cfg.DB.Pool.MaxOpen)
	}
	if cfg.DB.Pool.MaxIdle != 5 {
		t.Errorf("DB.Pool.MaxIdle default = %d, want 5", cfg.DB.Pool.MaxIdle)
	}
	if cfg.DB.Pool.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("DB.Pool.ConnMaxLifetime default = %v, want 30m", cfg.DB.Pool.ConnMaxLifetime)
	}
	// Legacy DSN preserved when Host is empty
	if !strings.Contains(cfg.DB.DSN, "opskeeper:opskeeper@tcp(127.0.0.1:3306)/opskeeper") {
		t.Errorf("legacy DSN overridden when Host empty: %q", cfg.DB.DSN)
	}
}

func TestDatabaseFieldsEnvOverride(t *testing.T) {
	clearPlatformBaseHAVars(t)
	t.Setenv("OPSKEEPER_DB_HOST", "db.internal")
	t.Setenv("OPSKEEPER_DB_PORT", "3307")
	t.Setenv("OPSKEEPER_DB_USER", "opskeeper_app")
	t.Setenv("OPSKEEPER_DB_PASSWORD", "s3cret")
	t.Setenv("OPSKEEPER_DB_NAME", "opskeeper_prod")
	t.Setenv("OPSKEEPER_DB_SSLMODE", "require")
	t.Setenv("OPSKEEPER_DB_POOL_MAX_OPEN", "100")
	t.Setenv("OPSKEEPER_DB_POOL_MAX_IDLE", "20")
	t.Setenv("OPSKEEPER_DB_POOL_CONN_MAX_LIFETIME", "5m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.Host != "db.internal" {
		t.Errorf("DB.Host = %q, want db.internal", cfg.DB.Host)
	}
	if cfg.DB.Port != 3307 {
		t.Errorf("DB.Port = %d, want 3307", cfg.DB.Port)
	}
	if cfg.DB.SSLMode != "require" {
		t.Errorf("DB.SSLMode = %q, want require", cfg.DB.SSLMode)
	}
	if cfg.DB.Pool.MaxOpen != 100 {
		t.Errorf("DB.Pool.MaxOpen = %d, want 100", cfg.DB.Pool.MaxOpen)
	}
	if cfg.DB.Pool.ConnMaxLifetime != 5*time.Minute {
		t.Errorf("DB.Pool.ConnMaxLifetime = %v, want 5m", cfg.DB.Pool.ConnMaxLifetime)
	}
	// DSN must be re-composed from discrete fields, not the legacy default.
	want := "opskeeper_app:s3cret@tcp(db.internal:3307)/opskeeper_prod?parseTime=true&charset=utf8mb4&loc=Local&tls=require"
	if cfg.DB.DSN != want {
		t.Errorf("DB.DSN = %q\nwant  %q", cfg.DB.DSN, want)
	}
}

func TestDatabaseDSNSpecialSSLMode(t *testing.T) {
	clearPlatformBaseHAVars(t)
	t.Setenv("OPSKEEPER_DB_HOST", "h")
	t.Setenv("OPSKEEPER_DB_USER", "u")
	t.Setenv("OPSKEEPER_DB_PASSWORD", "p")
	t.Setenv("OPSKEEPER_DB_NAME", "d")
	t.Setenv("OPSKEEPER_DB_SSLMODE", "verify-full")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(cfg.DB.DSN, "tls=verify-full") {
		t.Errorf("DSN missing tls=verify-full: %q", cfg.DB.DSN)
	}
}

func TestRedisDefaults(t *testing.T) {
	clearPlatformBaseHAVars(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis.Addr != "127.0.0.1:6379" {
		t.Errorf("Redis.Addr default = %q, want 127.0.0.1:6379", cfg.Redis.Addr)
	}
	if cfg.Redis.Password != "" {
		t.Errorf("Redis.Password default = %q, want empty", cfg.Redis.Password)
	}
	if cfg.Redis.DB != 0 {
		t.Errorf("Redis.DB default = %d, want 0", cfg.Redis.DB)
	}
	if cfg.Redis.Pool.MaxActive != 50 {
		t.Errorf("Redis.Pool.MaxActive default = %d, want 50", cfg.Redis.Pool.MaxActive)
	}
	if cfg.Redis.Pool.MaxIdle != 10 {
		t.Errorf("Redis.Pool.MaxIdle default = %d, want 10", cfg.Redis.Pool.MaxIdle)
	}
	if cfg.Redis.Pool.DialTimeout != 5*time.Second {
		t.Errorf("Redis.Pool.DialTimeout default = %v, want 5s", cfg.Redis.Pool.DialTimeout)
	}
}

func TestRedisEnvOverride(t *testing.T) {
	clearPlatformBaseHAVars(t)
	t.Setenv("OPSKEEPER_REDIS_ADDR", "redis.prod:6379")
	t.Setenv("OPSKEEPER_REDIS_PASSWORD", "auth")
	t.Setenv("OPSKEEPER_REDIS_DB", "3")
	t.Setenv("OPSKEEPER_REDIS_POOL_MAX_ACTIVE", "200")
	t.Setenv("OPSKEEPER_REDIS_POOL_MAX_IDLE", "40")
	t.Setenv("OPSKEEPER_REDIS_POOL_DIAL_TIMEOUT", "2s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis.Addr != "redis.prod:6379" {
		t.Errorf("Redis.Addr = %q", cfg.Redis.Addr)
	}
	if cfg.Redis.Password != "auth" {
		t.Errorf("Redis.Password = %q", cfg.Redis.Password)
	}
	if cfg.Redis.DB != 3 {
		t.Errorf("Redis.DB = %d", cfg.Redis.DB)
	}
	if cfg.Redis.Pool.MaxActive != 200 {
		t.Errorf("Redis.Pool.MaxActive = %d", cfg.Redis.Pool.MaxActive)
	}
	if cfg.Redis.Pool.DialTimeout != 2*time.Second {
		t.Errorf("Redis.Pool.DialTimeout = %v", cfg.Redis.Pool.DialTimeout)
	}
}

func TestLeaderDefaults(t *testing.T) {
	clearPlatformBaseHAVars(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Leader.Enabled {
		t.Errorf("Leader.Enabled default = false, want true")
	}
	if cfg.Leader.TTL != 15*time.Second {
		t.Errorf("Leader.TTL default = %v, want 15s", cfg.Leader.TTL)
	}
	if cfg.Leader.RenewInterval != 5*time.Second {
		t.Errorf("Leader.RenewInterval default = %v, want 5s", cfg.Leader.RenewInterval)
	}
	if cfg.Leader.StartTimeout != 30*time.Second {
		t.Errorf("Leader.StartTimeout default = %v, want 30s", cfg.Leader.StartTimeout)
	}
	if cfg.Leader.InstanceID != "" {
		t.Errorf("Leader.InstanceID default = %q, want empty (auto-derive)", cfg.Leader.InstanceID)
	}
	// TTL > 2x RenewInterval invariant (per LeaderConfig docs).
	if cfg.Leader.TTL <= 2*cfg.Leader.RenewInterval {
		t.Errorf("default TTL %v should be > 2*RenewInterval %v", cfg.Leader.TTL, cfg.Leader.RenewInterval)
	}
}

func TestLeaderEnvOverride(t *testing.T) {
	clearPlatformBaseHAVars(t)
	t.Setenv("OPSKEEPER_LEADER_ENABLED", "false")
	t.Setenv("OPSKEEPER_LEADER_TTL", "30s")
	t.Setenv("OPSKEEPER_LEADER_RENEW_INTERVAL", "10s")
	t.Setenv("OPSKEEPER_LEADER_START_TIMEOUT", "1m")
	t.Setenv("OPSKEEPER_LEADER_INSTANCE_ID", "manager-0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Leader.Enabled {
		t.Errorf("Leader.Enabled = true, want false")
	}
	if cfg.Leader.TTL != 30*time.Second {
		t.Errorf("Leader.TTL = %v", cfg.Leader.TTL)
	}
	if cfg.Leader.RenewInterval != 10*time.Second {
		t.Errorf("Leader.RenewInterval = %v", cfg.Leader.RenewInterval)
	}
	if cfg.Leader.StartTimeout != time.Minute {
		t.Errorf("Leader.StartTimeout = %v", cfg.Leader.StartTimeout)
	}
	if cfg.Leader.InstanceID != "manager-0" {
		t.Errorf("Leader.InstanceID = %q", cfg.Leader.InstanceID)
	}
}

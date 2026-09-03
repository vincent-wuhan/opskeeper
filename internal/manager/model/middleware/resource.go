// Package middleware holds persistence entities for the Middleware
// Adapter feature (路径 A 阶段 2 任务 2.1-2.5)。
//
// Three tables:
//   - middleware_resources: registered middleware resources (PG/Redis/MQ/K8s/Git)
//   - middleware_resource_conn_specs: encrypted DSN storage
//   - middleware_resource_health: periodic health check results
//
// Multi-tenant: every row has tenant_id; queries MUST filter by tenant.
package middleware

import (
	"time"

	"gorm.io/plugin/soft_delete"
)

// ResourceType constants — enum for the 6 supported resource families.
const (
	ResourceTypePostgres      = "postgres"
	ResourceTypeRedis         = "redis"
	ResourceTypeRabbitMQ      = "rabbitmq"
	ResourceTypeKafka         = "kafka"
	ResourceTypeK8sCluster    = "k8s_cluster"
	ResourceTypeGitRepository = "git_repository"
)

// ResourceStatus constants — health probe result.
const (
	ResourceStatusHealthy  = "healthy"
	ResourceStatusDegraded = "degraded"
	ResourceStatusDown     = "down"
	ResourceStatusUntested = "untested"
)

// MiddlewareResource is one row of middleware_resources — a registered
// middleware resource (one PG cluster, one Redis instance, one K8s cluster, etc.).
//
// tenant_id is REQUIRED: every query MUST filter by tenant to maintain
// multi-tenant isolation. The unique index (tenant_id, type, name) prevents
// duplicate resource names within a tenant.
type MiddlewareResource struct {
	ID uint64 `gorm:"primaryKey;autoIncrement"`

	TenantID uint64 `gorm:"not null;default:0;index;uniqueIndex:idx_tenant_type_name,priority:1"`

	// Type is one of: postgres / redis / rabbitmq / kafka / k8s_cluster / git_repository.
	Type string `gorm:"size:32;not null;uniqueIndex:idx_tenant_type_name,priority:2"`

	// Name is the human-friendly identifier (e.g. "prod-pg-cluster-1").
	// Unique per (tenant, type) pair.
	Name string `gorm:"size:128;not null;uniqueIndex:idx_tenant_type_name,priority:3"`

	// LabelsJSON is a JSON-encoded map[string]string — free-form tags
	// for filtering (env=prod, region=us-east-1, owner=dba-team).
	LabelsJSON string `gorm:"type:text"`

	// ConnSpecID points to MiddlewareConnSpec.ID (FK-like, but kept loose
	// for cross-table audit). The actual DSN lives in the secrets table.
	ConnSpecID uint64 `gorm:"not null;index"`

	// Status is one of: healthy / degraded / down / untested.
	Status string `gorm:"size:16;not null;default:'untested'"`

	// LastSeen is the last successful health probe timestamp.
	LastSeen *time.Time `gorm:"index"`

	// LastError records the most recent health probe failure message
	// (sanitized — never contains credentials).
	LastError string `gorm:"type:text"`

	// ConfigJSON holds adapter-specific configuration (e.g. K8s cluster
	// namespace filter, Redis db index). JSON-encoded; opaque to this layer.
	ConfigJSON string `gorm:"type:text"`

	// CreatedBy / CreatedAt / UpdatedAt are audit fields.
	CreatedBy uint64    `gorm:"not null;default:0"`
	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`

	// DeletedAt + DeleteMarker for soft delete (see marketplace package
	// for the same pattern).
	DeletedAt    *time.Time            `gorm:"column:deleted_at;index"`
	DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:idx_tenant_type_name,priority:4"`
}

// TableName pins the table name.
func (MiddlewareResource) TableName() string { return "middleware_resources" }

// MiddlewareConnSpec stores the encrypted connection spec (DSN).
// The actual DSN plaintext NEVER lives in this table — only in the
// secrets table. This row holds metadata + pointer.
type MiddlewareConnSpec struct {
	ID uint64 `gorm:"primaryKey;autoIncrement"`

	TenantID uint64 `gorm:"not null;default:0;index"`

	// ResourceID is the owning MiddlewareResource.ID.
	ResourceID uint64 `gorm:"not null;uniqueIndex"`

	// SecretRef is the reference into the secrets table (e.g. "secret://dsn/<uuid>").
	// Runtime resolution happens in pkg/credinject.
	SecretRef string `gorm:"size:255;not null"`

	// PoolSize is the connection pool size (default 10).
	PoolSize int `gorm:"not null;default:10"`

	// TimeoutSeconds is the connection / query timeout (default 30s).
	TimeoutSeconds int `gorm:"not null;default:30"`

	// TLSMode: "disable" | "require" | "verify-ca" | "verify-full".
	TLSMode string `gorm:"size:16;not null;default:'require'"`

	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`

	DeletedAt    *time.Time            `gorm:"column:deleted_at;index"`
	DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt"`
}

// TableName pins the table name.
func (MiddlewareConnSpec) TableName() string { return "middleware_resource_conn_specs" }

// MiddlewareResourceHealth is one row of periodic health probe results.
// Kept separate from MiddlewareResource to avoid bloating the main table
// with high-frequency writes.
type MiddlewareResourceHealth struct {
	ID uint64 `gorm:"primaryKey;autoIncrement"`

	TenantID   uint64 `gorm:"not null;default:0;index"`
	ResourceID uint64 `gorm:"not null;index"`

	// Status: healthy / degraded / down.
	Status string `gorm:"size:16;not null"`

	// LatencyMs is the probe latency in milliseconds.
	LatencyMs int `gorm:"not null;default:0"`

	// ErrorMessage is the sanitized error (never contains credentials).
	ErrorMessage string `gorm:"type:text"`

	// ProbedAt is the probe timestamp.
	ProbedAt time.Time `gorm:"not null;index"`

	CreatedAt time.Time `gorm:"autoCreateTime"`
}

// TableName pins the table name.
func (MiddlewareResourceHealth) TableName() string { return "middleware_resource_health" }

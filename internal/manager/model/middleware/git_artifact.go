// git_artifact.go — 路径 A 阶段 1 任务 1.3 补充：git-artifact 数据模型
//
// Two tables:
//   - git_artifacts: CI 上报的制品元数据（commit + artifact_url + meta）
//   - runtime_symbol_links: 反向索引（pg_query / redis_cmd / k8s_image / http_route → commit + file:line）
//
// 关联 spec: openspec/changes/unified-platform-base-selection/specs/git-artifact-linker/spec.md
// 关联协议: openspec/changes/unified-platform-base-selection/protocols/git-artifact-v0.md
package middleware

import (
	"time"

	"gorm.io/plugin/soft_delete"
)

// GitArtifact is one row of git_artifacts — CI 上报的制品元数据。
//
// 一次制品上报 = 一行。commit + repo_url 唯一（不依赖 branch，因为同一 commit
// 可在不同分支上重建）。
type GitArtifact struct {
	ID uint64 `gorm:"primaryKey;autoIncrement"`

	TenantID uint64 `gorm:"not null;default:0;index"`

	// ID 是协议层暴露的字符串 ID（ga-<hash>）。冗余存储便于快速查找。
	// NOT NULL + UNIQUE per tenant。
	PublicID string `gorm:"size:64;not null;uniqueIndex:idx_tenant_public"`

	RepoURL     string `gorm:"size:512;not null;uniqueIndex:idx_repo_commit,priority:1"`
	Commit      string `gorm:"size:64;not null;uniqueIndex:idx_repo_commit,priority:2"`
	Branch      string `gorm:"size:255;not null;index"`
	ArtifactURL string `gorm:"size:1024;not null"`

	// MetaJSON: 协议层 meta 字段（build_id / pipeline / runner / env / image_tag 等）。
	MetaJSON string `gorm:"type:text"`

	// BuildAt 是协议层 build_at 字段（CI 上报时间）。
	BuildAt time.Time `gorm:"not null;index"`

	// IndexedAt 是反向索引构建完成时间。
	IndexedAt *time.Time `gorm:"index"`

	// IndexStatus: queued / running / completed / failed
	IndexStatus string `gorm:"size:16;not null;default:'queued';index"`

	// IndexError 记录索引构建失败原因（sanitized — 不含敏感数据）。
	IndexError string `gorm:"type:text"`

	// RuntimeLinksCount 是反向索引生成的运行时符号链接数。
	RuntimeLinksCount int `gorm:"not null;default:0"`

	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`

	DeletedAt    *time.Time            `gorm:"column:deleted_at;index"`
	DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:idx_tenant_public,priority:2"`
}

// TableName pins the table name.
func (GitArtifact) TableName() string { return "git_artifacts" }

// RuntimeSymbolLink is one row of runtime_symbol_links — 反向索引。
//
// 把运行时符号（pg_query 文本 / redis_cmd / k8s_image tag / http_route）
// 关联到 commit + file:line + author + commit_msg + confidence。
type RuntimeSymbolLink struct {
	ID uint64 `gorm:"primaryKey;autoIncrement"`

	TenantID uint64 `gorm:"not null;default:0;index"`

	// SymbolType: pg_query / redis_cmd / k8s_image / http_route
	SymbolType string `gorm:"size:32;not null;index"`

	// Symbol 是反查键（去空白 + 标准化后的全文）。
	// 标准化规则在 Linker 实现：PG query 去注释 + trim；Redis cmd 转大写；K8s image 转 lowercase；HTTP route 转正则模板。
	Symbol string `gorm:"size:2048;not null;index"`

	// GitArtifactID 关联到 GitArtifact.ID（同一表内）。
	GitArtifactID uint64 `gorm:"not null;index"`

	// FilePath / LineStart / LineEnd 是反查结果。
	FilePath  string `gorm:"size:512;not null"`
	LineStart int    `gorm:"not null;default:0"`
	LineEnd   int    `gorm:"not null;default:0"`

	// Author / CommitMsg 是 Git 元数据（来自 commit + git show）。
	Author    string `gorm:"size:255"`
	CommitMsg string `gorm:"type:text"`

	// Confidence 0-1，< 0.7 时 Linker 标注 needs_human_confirm。
	Confidence float64 `gorm:"not null;default:0"`

	// EvidenceJSON 是反查证据链（如 ["exact_match_in_blob", "matched_artifact_ga-abc", ...]）。
	EvidenceJSON string `gorm:"type:text"`

	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`

	DeletedAt    *time.Time            `gorm:"column:deleted_at;index"`
	DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt"`
}

// TableName pins the table name.
func (RuntimeSymbolLink) TableName() string { return "runtime_symbol_links" }

// SymbolType constants — 协议层 enum。
const (
	SymbolTypePGQuery   = "pg_query"
	SymbolTypeRedisCmd  = "redis_cmd"
	SymbolTypeK8sImage  = "k8s_image"
	SymbolTypeHTTPRoute = "http_route"
)

// IndexStatus constants
const (
	IndexStatusQueued    = "queued"
	IndexStatusRunning   = "running"
	IndexStatusCompleted = "completed"
	IndexStatusFailed    = "failed"
)

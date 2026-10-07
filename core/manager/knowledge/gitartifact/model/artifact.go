// Package model 定义 git-artifact 制品的数据模型。
//
// 路径 A 阶段 2 任务 2.10 — knowledge 反向索引接入。
//
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/git-artifact-linker/spec.md
// 关联协议：openspec/changes/unified-platform-base-selection/protocols/git-artifact-v0.md
package model

import (
	"fmt"
	"time"
)

// IndexStatus 是制品反向索引构建状态。
type IndexStatus string

const (
	IndexStatusQueued    IndexStatus = "queued"    // 入库待构建
	IndexStatusRunning   IndexStatus = "running"   // 构建中
	IndexStatusCompleted IndexStatus = "completed" // 完成
	IndexStatusFailed    IndexStatus = "failed"    // 失败（可重试）
	IndexStatusStale     IndexStatus = "stale"     // 过期（需重建）
)

// Artifact 是入库的 git 制品（与 v0 协议字段对齐）。
//
// 这是 server.go StoredArtifact 的规范化版本。StoredArtifact 保留为 alias
// 避免破坏现有测试；新增字段走 Artifact。
type Artifact struct {
	// ID 是内部主键（DB 主键 / file 行 ID）。
	ID string
	// PublicID 是对外暴露的 ID（CI 回调用，URL-safe）。
	PublicID string
	// RepoURL 是 Git 仓库地址。
	RepoURL string
	// Commit 是 40/64 字符 commit hash。
	Commit string
	// Branch 是构建时所在分支。
	Branch string
	// ArtifactURL 是制品存储地址（S3/HTTP/OCI）。
	ArtifactURL string
	// Meta 是 CI 透传的元数据（必含 build_id）。
	Meta map[string]interface{}
	// BuildAt 是 CI 构建时间。
	BuildAt time.Time
	// IndexedAt 是反向索引构建完成时间（nil = 未完成）。
	IndexedAt *time.Time
	// IndexStatus 是当前构建状态。
	IndexStatus IndexStatus
	// IndexError 是最近一次失败原因（成功时为空）。
	IndexError string
	// TenantID 是多租户隔离。
	TenantID uint64
}

// Validate 校验必填字段。
func (a *Artifact) Validate() error {
	if a.RepoURL == "" {
		return fmt.Errorf("missing repo_url")
	}
	if len(a.Commit) != 40 && len(a.Commit) != 64 {
		return fmt.Errorf("commit must be 40 or 64 chars, got %d", len(a.Commit))
	}
	if a.Branch == "" {
		return fmt.Errorf("missing branch")
	}
	if a.ArtifactURL == "" {
		return fmt.Errorf("missing artifact_url")
	}
	if a.BuildAt.IsZero() {
		return fmt.Errorf("missing build_at")
	}
	if a.Meta == nil || a.Meta["build_id"] == nil {
		return fmt.Errorf("missing required field: meta.build_id")
	}
	return nil
}

// BuildID 提取 meta.build_id（强类型访问）。
func (a *Artifact) BuildID() string {
	if a.Meta == nil {
		return ""
	}
	if v, ok := a.Meta["build_id"].(string); ok {
		return v
	}
	return fmt.Sprintf("%v", a.Meta["build_id"])
}

// GitArtifact 是 Git 制品的完整元数据（Artifact + 反向索引条目）。
//
// 用于全量重建场景：从 GitArtifact 反推出所有 ExtractedSymbol 重新注册。
type GitArtifact struct {
	Artifact
	// Symbols 是从该 commit 提取的所有运行时符号（pg_query / redis_cmd / k8s_image / http_route）。
	Symbols []ExtractedSymbol `json:"symbols"`
}

// ExtractedSymbol 是一个从源码提取的运行时符号。
//
// 与 ExtractedSymbol（api/indexer.go）共享字段，但本类型不依赖 Linker，
// 用于序列化（JSON / 跨进程传输）。
type ExtractedSymbol struct {
	Type       string                 `json:"type"`      // pg_query / redis_cmd / k8s_image / http_route
	Input      map[string]interface{} `json:"input"`     // 标准化输入字段
	FilePath   string                 `json:"file_path"` // 源码位置
	LineStart  int                    `json:"line_start"`
	LineEnd    int                    `json:"line_end"`
	CommitSHA  string                 `json:"commit_sha"`
	Confidence float64                `json:"confidence"` // 0-1
}

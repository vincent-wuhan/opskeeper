// chatdiagnose/store/gitartifact_linker.go — incident_pattern KB 的
// gitartifact linker 真实实现，包装 internal/knowledge/gitartifact/store。
//
// 设计：
//   - 复用 v1 store（gitastore.MemoryStore / JSONFileStore）
//   - 严格多租户：tenant_id 必须先 parse 成 uint64
//   - 资源匹配启发式：RepoURL 包含 resourceID，或 Meta["resource"] == resourceID
//   - ResourceType 暂作 Branch filter（v1 store 没有 resource_type 字段；Branch 已是 tenant
//     内可选的强约束维度；"pg" / "redis" 等作为 Branch 是约定）
//
// KB miss MUST NOT block the chat：所有错误 slog warn + 返回 nil, nil。

package store

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	gitamodel "github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/model"
	gitastore "github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/store"

	chatdiagnose "github.com/vincent-wuhan/opskeeper/internal/manager/biz/chatdiagnose"
)

// DBGitArtifactLinker 实现 chatdiagnose.GitArtifactLinker 接口，
// 复用 v1 gitastore.Store 做反查。
type DBGitArtifactLinker struct {
	store gitastore.Store
	log   *slog.Logger
}

// NewDBGitArtifactLinker 构造。store 不得为 nil。
func NewDBGitArtifactLinker(store gitastore.Store, log *slog.Logger) *DBGitArtifactLinker {
	if log == nil {
		log = slog.Default()
	}
	return &DBGitArtifactLinker{store: store, log: log.With(slog.String("comp", "chatdiagnose.gitartifact_linker"))}
}

// Compile-time interface satisfaction check.
var _ chatdiagnose.GitArtifactLinker = (*DBGitArtifactLinker)(nil)

// LookupByResource 反查：列出 (tenantID) 所有 artifact，匹配 (resourceType, resourceID)。
//
// 匹配策略（启发式）：
//   - resourceType 非空 → 当 Branch 过滤（约定）
//   - resourceID 非空 → RepoURL 包含 OR Meta["resource"] == resourceID
//
// 返回 Relevance 启发式：
//   - 1.0：Meta["resource"] 完全相等（精确）
//   - 0.8：RepoURL 包含（模糊）
//   - 0.5：仅 Branch 命中（弱）
func (l *DBGitArtifactLinker) LookupByResource(ctx context.Context, tenantID, resourceType, resourceID string) ([]chatdiagnose.GitArtifactHit, error) {
	if l.store == nil {
		return nil, errors.New("chatdiagnose: DBGitArtifactLinker: nil store")
	}
	if tenantID == "" {
		return nil, errors.New("chatdiagnose: LookupByResource requires tenant_id")
	}

	tenantUint, err := strconv.ParseUint(tenantID, 10, 64)
	if err != nil {
		// tenantID 不是 uint64（如 UUID 字符串）→ 视为 KB miss，不阻塞 chat
		l.log.Warn("gitartifact_linker: tenantID not uint64, skip",
			slog.String("tenant_id", tenantID),
			slog.Any("err", err))
		return nil, nil
	}

	filter := gitastore.ListFilter{
		TenantID: tenantUint,
		Branch:   resourceType, // 约定：resourceType 当 Branch 用
	}
	artifacts, err := l.store.List(ctx, filter)
	if err != nil {
		// KB miss 不阻塞 — warn + 返回空
		l.log.Warn("gitartifact_linker: store.List failed",
			slog.String("tenant_id", tenantID),
			slog.Any("err", err))
		return nil, nil
	}

	out := make([]chatdiagnose.GitArtifactHit, 0, len(artifacts))
	for _, a := range artifacts {
		if a == nil || a.Commit == "" {
			continue
		}
		rel, filePath := matchArtifact(a, resourceType, resourceID)
		if rel <= 0 {
			continue
		}
		out = append(out, chatdiagnose.GitArtifactHit{
			CommitSHA: a.Commit,
			FilePath:  filePath,
			Relevance: rel,
		})
	}
	return out, nil
}

// matchArtifact 启发式匹配；返回 (relevance, filePath)。relevance=0 表示不命中。
func matchArtifact(a *gitamodel.Artifact, resourceType, resourceID string) (float64, string) {
	if resourceID == "" {
		// resourceID 缺失 → 按 resourceType 弱匹配
		if resourceType != "" && strings.Contains(a.RepoURL, resourceType) {
			return 0.5, "/"
		}
		return 0, ""
	}
	// 精确：Meta["resource"] == resourceID
	if a.Meta != nil {
		if v, ok := a.Meta["resource"].(string); ok && v == resourceID {
			return 1.0, filePathFromMeta(a.Meta)
		}
	}
	// 模糊：RepoURL 包含 resourceID
	if strings.Contains(a.RepoURL, resourceID) {
		return 0.8, "/"
	}
	return 0, ""
}

// filePathFromMeta 从 Meta["file_path"] 取文件路径（不存在 → "/"）。
func filePathFromMeta(m map[string]interface{}) string {
	if v, ok := m["file_path"].(string); ok && v != "" {
		return v
	}
	return "/"
}

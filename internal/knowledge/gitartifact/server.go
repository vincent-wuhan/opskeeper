package gitartifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/model"
	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/store"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// StoredArtifact 是 model.Artifact 的别名（向后兼容旧代码 / 测试）。
//
// 路径 A 阶段 2 任务 2.10 起，所有新代码应使用 model.Artifact。
type StoredArtifact = model.Artifact

// Server 是 git-artifact v0 协议的 HTTP handler。
//
// 路由：
//
//	POST /api/v1/git-artifacts        — CI 上报制品
//	GET  /api/v1/git-artifacts/{id}   — 查询制品
//	POST /api/v1/runtime-link         — 反查运行时符号
//
// 存储后端（Task 2.10）：
//   - 默认 NewMemoryStore()（单进程 / 测试）
//   - 生产环境用 NewJSONFileStore(path) 或替换为 GORM + PostgreSQL
//
// 反向索引（Task 2.10）：
//   - 默认不挂 Indexer（兼容旧行为：仅标记 indexed_at 占位）
//   - 挂上 Indexer 后走真正的反向索引构建
//   - 旧测试 / 旧行为通过 WithIndexer(nil) 保留
//
// 关联协议：openspec/changes/unified-platform-base-selection/protocols/git-artifact-v0.md
type Server struct {
	registry *LinkerRegistry
	mu       sync.RWMutex
	store    store.Store
	indexer  IndexerHandle // optional; nil = 占位 buildIndex
	logger   *slog.Logger
}

// IndexerHandle 是 Indexer 的句柄（避免 api → server 循环 import）。
//
// api 包实现真正的 Indexer；Server 通过 interface 拿到回调。
// 这里用 interface 简化（api.Indexer 也满足此接口）。
type IndexerHandle interface {
	Index(ctx context.Context, publicID string) error
}

// NewServer 创建 Server（默认 MemoryStore + 无 Indexer）。
func NewServer(reg *LinkerRegistry, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		registry: reg,
		store:    store.NewMemoryStore(),
		logger:   logger,
	}
}

// WithStore 替换 store（用于接入 JSONFileStore / DB 实现）。
func (s *Server) WithStore(st store.Store) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = st
	return s
}

// WithIndexer 注入 Indexer（开启真正的反向索引构建）。
func (s *Server) WithIndexer(idx IndexerHandle) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexer = idx
	return s
}

// RegisterRoutes 注册 HTTP 路由到 mux。
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/git-artifacts", s.handleArtifacts)
	mux.HandleFunc("/api/v1/git-artifacts/", s.handleArtifactByID)
	mux.HandleFunc("/api/v1/runtime-link", s.handleRuntimeLink)
}

// Handler returns the protocol handler for embedding in another router.
// Authentication remains the caller's responsibility; cmd/opskeeper mounts it
// only inside the JWT-protected API group.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	return mux
}

// --- Handlers ---

// handleArtifacts 处理 POST /api/v1/git-artifacts
func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("X-GitArtifact-Version"); v != "" && v != "v0" {
		http.Error(w, fmt.Sprintf("unsupported protocol version: %s", v), http.StatusBadRequest)
		return
	}

	var req struct {
		Artifact struct {
			RepoURL     string                 `json:"repo_url"`
			Commit      string                 `json:"commit"`
			Branch      string                 `json:"branch"`
			ArtifactURL string                 `json:"artifact_url"`
			Meta        map[string]interface{} `json:"meta"`
			BuildAt     time.Time              `json:"build_at"`
		} `json:"artifact"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	artifact := &model.Artifact{
		RepoURL:     req.Artifact.RepoURL,
		Commit:      req.Artifact.Commit,
		Branch:      req.Artifact.Branch,
		ArtifactURL: req.Artifact.ArtifactURL,
		Meta:        req.Artifact.Meta,
		BuildAt:     req.Artifact.BuildAt,
		IndexStatus: model.IndexStatusQueued,
		TenantID:    tenantFromContext(r.Context()),
	}
	// Set ID/PublicID in lock-free way first
	artifact.ID = generatePublicID(artifact.RepoURL, artifact.Commit)
	artifact.PublicID = artifact.ID

	if err := artifact.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 重复检测：相同 public_id 视为已存在（v0 协议幂等性约束）
	if _, err := s.store.Get(r.Context(), artifact.PublicID); err == nil {
		writeError(w, http.StatusConflict, "artifact already exists: "+artifact.PublicID)
		return
	} else {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// 入库
	if err := s.store.Put(r.Context(), artifact); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 异步触发反向索引构建
	s.triggerIndex(artifact.PublicID)

	resp := map[string]interface{}{
		"code":    0,
		"message": "ok",
		"data": map[string]interface{}{
			"id":           artifact.PublicID,
			"index_status": string(model.IndexStatusQueued),
		},
	}
	w.Header().Set("X-GitArtifact-Version", "v0")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleArtifactByID 处理 GET /api/v1/git-artifacts/{id}
func (s *Server) handleArtifactByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/git-artifacts/")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	artifact, err := s.store.Get(r.Context(), id)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 多租户隔离
	tenantID := tenantFromContext(r.Context())
	if artifact.TenantID != 0 && tenantID != 0 && artifact.TenantID != tenantID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	resp := map[string]interface{}{
		"code":    0,
		"message": "ok",
		"data":    artifact,
	}
	w.Header().Set("X-GitArtifact-Version", "v0")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleRuntimeLink 处理 POST /api/v1/runtime-link
func (s *Server) handleRuntimeLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("X-GitArtifact-Version"); v != "" && v != "v0" {
		http.Error(w, fmt.Sprintf("unsupported protocol version: %s", v), http.StatusBadRequest)
		return
	}

	var req struct {
		Query struct {
			SymbolType SymbolType `json:"symbol_type"`
			Symbol     string     `json:"symbol"`
			TenantID   uint64     `json:"tenant_id"`
		} `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Query.SymbolType == "" || req.Query.Symbol == "" {
		writeError(w, http.StatusBadRequest, "missing symbol_type or symbol")
		return
	}
	contextTenantID := tenantFromContext(r.Context())
	if req.Query.TenantID != 0 && contextTenantID != 0 && req.Query.TenantID != contextTenantID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	input, err := parseSymbolInput(req.Query.SymbolType, req.Query.Symbol)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	linkCtx := r.Context()
	if contextTenantID == 0 && req.Query.TenantID != 0 {
		linkCtx = WithTenant(linkCtx, req.Query.TenantID)
	}
	result, err := s.registry.LinkByType(linkCtx, req.Query.SymbolType, input)
	if err != nil {
		if errors.Is(err, ErrUnsupportedInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if result == nil {
		resp := map[string]interface{}{
			"code":    0,
			"message": "ok",
			"data": map[string]interface{}{
				"link":   nil,
				"reason": "no_match_in_index",
			},
		}
		w.Header().Set("X-GitArtifact-Version", "v0")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	// 低置信度标注
	if result.NeedsHumanConfirm() {
		result.Flag = "needs_human_confirm"
	}

	resp := map[string]interface{}{
		"code":    0,
		"message": "ok",
		"data":    map[string]interface{}{"link": result},
	}
	w.Header().Set("X-GitArtifact-Version", "v0")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// --- helpers ---

// triggerIndex 异步触发反向索引构建。
//
// 优先调用 Indexer（Task 2.10）；若未挂 Indexer 则 fallback 到占位 buildIndex。
func (s *Server) triggerIndex(publicID string) {
	s.mu.RLock()
	idx := s.indexer
	s.mu.RUnlock()
	if idx == nil {
		// Fallback: 占位实现（仅标记 indexed_at，保留旧行为）
		s.buildIndex(publicID)
		return
	}
	go func() {
		if err := idx.Index(context.Background(), publicID); err != nil {
			s.logger.Warn("async index failed",
				slog.String("public_id", publicID),
				slog.String("err", err.Error()),
			)
		}
	}()
}

// buildIndex 旧版占位实现（向后兼容 TestServer_BuildIndex + 无 Indexer 场景）。
//
// 当前行为：从 store 取出 artifact，仅标记 indexed_at，不实际解析。
// 推荐：新代码挂上 Indexer（WithIndexer）走真正的反向索引。
func (s *Server) buildIndex(publicID string) {
	ctx := context.Background()
	a, err := s.store.Get(ctx, publicID)
	if err != nil {
		s.logger.Warn("buildIndex: get failed",
			slog.String("public_id", publicID),
			slog.String("err", err.Error()),
		)
		return
	}
	s.logger.Info("git-artifact buildIndex start (skeleton)",
		slog.String("public_id", a.PublicID),
		slog.String("repo", a.RepoURL),
		slog.String("commit", a.Commit),
	)
	now := time.Now()
	a.IndexedAt = &now
	a.IndexStatus = model.IndexStatusCompleted
	if err := s.store.Put(ctx, a); err != nil {
		s.logger.Warn("buildIndex: put failed",
			slog.String("public_id", a.PublicID),
			slog.String("err", err.Error()),
		)
	}
	s.logger.Info("git-artifact buildIndex done (skeleton)",
		slog.String("public_id", a.PublicID),
	)
}

// writeError 输出统一错误响应。
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code":    status,
		"message": msg,
	})
}

// parseSymbolInput 根据 symbol_type 把 string 解析为对应的 input struct。
func parseSymbolInput(t SymbolType, symbol string) (interface{}, error) {
	switch t {
	case SymbolTypePGQuery:
		return PGQuery{Query: symbol}, nil
	case SymbolTypeRedisCmd:
		parts := strings.SplitN(symbol, " ", 2)
		c := RedisCmd{Cmd: parts[0]}
		if len(parts) > 1 {
			c.Key = parts[1]
		}
		return c, nil
	case SymbolTypeK8sImage:
		return K8sImage{Image: symbol}, nil
	case SymbolTypeHTTPRoute:
		parts := strings.SplitN(symbol, " ", 2)
		r := HTTPRoute{}
		if len(parts) >= 1 {
			r.Method = parts[0]
		}
		if len(parts) >= 2 {
			r.Path = parts[1]
		}
		return r, nil
	default:
		return nil, fmt.Errorf("unknown symbol_type: %s", t)
	}
}

// --- ctx helper ---

type tenantCtxKey struct{}

// WithTenant 把 tenant_id 写入 ctx（API server 用）。
func WithTenant(ctx context.Context, tenantID uint64) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// tenantFromContext 从 ctx 提取 tenant_id。
func tenantFromContext(ctx context.Context) uint64 {
	if v, ok := ctx.Value(tenantCtxKey{}).(uint64); ok {
		return v
	}
	if tenant, ok := tenantctx.From(ctx); ok {
		if tenant.IsSuperuser {
			return 0
		}
		return tenant.UserID
	}
	return 0
}

// generatePublicID 生成制品 public_id。
//
// 简化：用 repo_url + commit 哈希作为唯一键。
func generatePublicID(repoURL, commit string) string {
	h := simpleHash(repoURL + "|" + commit)
	return fmt.Sprintf("ga-%s", h)
}

// simpleHash 是非加密哈希（仅用于 ID 生成）。
func simpleHash(s string) string {
	var h uint64 = 14695981039346656037 // FNV offset basis
	for _, c := range s {
		h ^= uint64(c)
		h *= 1099511628211 // FNV prime
	}
	return fmt.Sprintf("%016x", h)
}

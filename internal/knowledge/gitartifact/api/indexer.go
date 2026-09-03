// Package api 实现 git-artifact 反向索引构建引擎（Indexer）。
//
// 路径 A 阶段 2 任务 2.10 — knowledge 反向索引接入。
//
// 设计要点：
//   - Indexer 增量构建：Put(artifact) → extract → AddIndex
//   - 全量重建：Rebuild() 从 store.List 拉所有 artifact 重新构建
//   - Worker pool：并行构建，workers 控制并发（默认 4）
//   - 状态机：queued → running → completed / failed
//   - 提取策略：skeleton 阶段从 artifact.Meta.ExtractedSymbols 读（CI 预提取）；
//     完整实现 followup PR 接入 go-git 解析源码
//
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/git-artifact-linker/spec.md
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact"
	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/model"
	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/store"
)

// SymbolExtractor 从 artifact 中提取符号（骨架：返回 meta 中预提取的符号）。
//
// 完整实现在 followup PR：go-git clone → 解析 SQL/Redis/K8s/HTTP → ExtractedSymbol。
type SymbolExtractor interface {
	// Extract 返回 artifact 对应的所有符号。
	// ctx 用于取消 / 超时控制。
	Extract(ctx context.Context, a *model.Artifact) ([]model.ExtractedSymbol, error)
}

// MetaExtractor 读取 artifact.Meta.ExtractedSymbols（CI 预提取）。
//
// 完整实现：SourceExtractor 接入 go-git 解析源码。
type MetaExtractor struct{}

// NewMetaExtractor 创建 MetaExtractor。
func NewMetaExtractor() *MetaExtractor { return &MetaExtractor{} }

// Extract 从 artifact.Meta.ExtractedSymbols 读取 CI 预提取的符号。
//
// meta 格式：{"extracted_symbols": [{"type": "pg_query", "input": {"query": "..."}, "file_path": "...", ...}]}
func (e *MetaExtractor) Extract(ctx context.Context, a *model.Artifact) ([]model.ExtractedSymbol, error) {
	if a == nil {
		return nil, fmt.Errorf("artifact required")
	}
	raw, ok := a.Meta["extracted_symbols"]
	if !ok {
		return nil, nil // 静默：无预提取符号（CI 尚未支持）
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("extracted_symbols must be a list, got %T", raw)
	}
	out := make([]model.ExtractedSymbol, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("extracted_symbols[%d] must be an object, got %T", i, item)
		}
		sym, err := parseExtractedSymbol(m)
		if err != nil {
			return nil, fmt.Errorf("extracted_symbols[%d]: %w", i, err)
		}
		// 注入 commit_sha（取自 artifact）
		if sym.CommitSHA == "" {
			sym.CommitSHA = a.Commit
		}
		out = append(out, sym)
	}
	return out, nil
}

// parseExtractedSymbol 把 map 解析为 ExtractedSymbol。
func parseExtractedSymbol(m map[string]interface{}) (model.ExtractedSymbol, error) {
	sym := model.ExtractedSymbol{}
	if v, ok := m["type"].(string); ok {
		sym.Type = v
	} else {
		return sym, fmt.Errorf("missing type")
	}
	if v, ok := m["input"].(map[string]interface{}); ok {
		sym.Input = v
	} else {
		return sym, fmt.Errorf("missing input")
	}
	if v, ok := m["file_path"].(string); ok {
		sym.FilePath = v
	}
	if v, ok := m["line_start"].(float64); ok {
		sym.LineStart = int(v)
	}
	if v, ok := m["line_end"].(float64); ok {
		sym.LineEnd = int(v)
	}
	if v, ok := m["commit_sha"].(string); ok {
		sym.CommitSHA = v
	}
	if v, ok := m["confidence"].(float64); ok {
		sym.Confidence = v
	}
	return sym, nil
}

// Indexer 是反向索引构建引擎。
type Indexer struct {
	registry  *gitartifact.LinkerRegistry
	store     store.Store
	extractor SymbolExtractor
	logger    *slog.Logger
	workers   int

	// inflight tracks 正在构建的 artifact public_id（防止重复触发）
	inflight   map[string]struct{}
	inflightMu sync.Mutex
}

// IndexerConfig 构造参数。
type IndexerConfig struct {
	LinkerRegistry *gitartifact.LinkerRegistry
	Store          store.Store
	Extractor      SymbolExtractor
	Logger         *slog.Logger
	Workers        int // 默认 4
}

// NewIndexer 创建 Indexer。
func NewIndexer(cfg IndexerConfig) *Indexer {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Extractor == nil {
		cfg.Extractor = NewMetaExtractor()
	}
	return &Indexer{
		registry:  cfg.LinkerRegistry,
		store:     cfg.Store,
		extractor: cfg.Extractor,
		logger:    cfg.Logger,
		workers:   cfg.Workers,
		inflight:  make(map[string]struct{}),
	}
}

// Index 增量构建单个 artifact 的反向索引。
//
// 流程：
//  1. mark running
//  2. extract symbols
//  3. for each symbol, dispatch to corresponding Linker.AddIndex
//  4. mark completed (or failed with IndexError)
func (ix *Indexer) Index(ctx context.Context, publicID string) error {
	// 防重入：同 public_id 并发 Index 第二次直接返回
	ix.inflightMu.Lock()
	if _, busy := ix.inflight[publicID]; busy {
		ix.inflightMu.Unlock()
		return fmt.Errorf("index already in flight for %s", publicID)
	}
	ix.inflight[publicID] = struct{}{}
	ix.inflightMu.Unlock()
	defer func() {
		ix.inflightMu.Lock()
		delete(ix.inflight, publicID)
		ix.inflightMu.Unlock()
	}()

	a, err := ix.store.Get(ctx, publicID)
	if err != nil {
		return fmt.Errorf("get artifact: %w", err)
	}
	// 1. mark running
	a.IndexStatus = model.IndexStatusRunning
	a.IndexError = ""
	if err := ix.store.Put(ctx, a); err != nil {
		return fmt.Errorf("mark running: %w", err)
	}
	ix.logger.Info("indexer.Index start",
		slog.String("public_id", publicID),
		slog.String("commit", a.Commit),
	)

	// 2. extract
	symbols, err := ix.extractor.Extract(ctx, a)
	if err != nil {
		ix.markFailed(ctx, a, fmt.Errorf("extract: %w", err))
		return err
	}

	// 3. dispatch to linkers
	added := 0
	for _, sym := range symbols {
		if err := ix.addSymbolToLinker(ctx, sym, a); err != nil {
			ix.logger.Warn("indexer symbol add failed",
				slog.String("public_id", publicID),
				slog.String("type", sym.Type),
				slog.String("err", err.Error()),
			)
			continue
		}
		added++
	}

	// 4. mark completed
	now := time.Now()
	a.IndexedAt = &now
	a.IndexStatus = model.IndexStatusCompleted
	a.IndexError = ""
	if err := ix.store.Put(ctx, a); err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}
	ix.logger.Info("indexer.Index done",
		slog.String("public_id", publicID),
		slog.Int("symbols_total", len(symbols)),
		slog.Int("symbols_indexed", added),
	)
	return nil
}

// addSymbolToLinker 把单个 ExtractedSymbol 注册到对应 Linker。
func (ix *Indexer) addSymbolToLinker(ctx context.Context, sym model.ExtractedSymbol, a *model.Artifact) error {
	linker, ok := ix.registry.Get(gitartifact.SymbolType(sym.Type))
	if !ok {
		return fmt.Errorf("no linker for type %s", sym.Type)
	}
	linkResult := &gitartifact.LinkResult{
		TenantID:   a.TenantID,
		Commit:     sym.CommitSHA,
		Repo:       a.RepoURL,
		FilePath:   sym.FilePath,
		LineStart:  sym.LineStart,
		LineEnd:    sym.LineEnd,
		Confidence: sym.Confidence,
	}
	// 构造 AddIndex 调用（各 Linker 接受不同参数，用类型断言分发）
	switch sym.Type {
	case string(gitartifact.SymbolTypePGQuery):
		pgLinker, ok := linker.(*gitartifact.PGQueryLinker)
		if !ok {
			return fmt.Errorf("linker for pg_query is not PGQueryLinker: %T", linker)
		}
		query, _ := sym.Input["query"].(string)
		pgLinker.AddIndex(query, linkResult)
	case string(gitartifact.SymbolTypeRedisCmd):
		rLinker, ok := linker.(*gitartifact.RedisCmdLinker)
		if !ok {
			return fmt.Errorf("linker for redis_cmd is not RedisCmdLinker: %T", linker)
		}
		cmd, _ := sym.Input["cmd"].(string)
		key, _ := sym.Input["key"].(string)
		rLinker.AddIndex(cmd, key, linkResult)
	case string(gitartifact.SymbolTypeK8sImage):
		kLinker, ok := linker.(*gitartifact.K8sImageLinker)
		if !ok {
			return fmt.Errorf("linker for k8s_image is not K8sImageLinker: %T", linker)
		}
		image, _ := sym.Input["image"].(string)
		kLinker.AddIndex(image, linkResult)
	case string(gitartifact.SymbolTypeHTTPRoute):
		hLinker, ok := linker.(*gitartifact.HTTPRouteLinker)
		if !ok {
			return fmt.Errorf("linker for http_route is not HTTPRouteLinker: %T", linker)
		}
		method, _ := sym.Input["method"].(string)
		path, _ := sym.Input["path"].(string)
		hLinker.AddIndex(method, path, linkResult)
	default:
		return fmt.Errorf("unsupported symbol type: %s", sym.Type)
	}
	return nil
}

// markFailed 标记 artifact 为 failed。
func (ix *Indexer) markFailed(ctx context.Context, a *model.Artifact, err error) {
	a.IndexStatus = model.IndexStatusFailed
	a.IndexError = err.Error()
	if putErr := ix.store.Put(ctx, a); putErr != nil {
		ix.logger.Error("mark failed: put error",
			slog.String("public_id", a.PublicID),
			slog.String("err", putErr.Error()),
		)
	}
}

// Rebuild 全量重建反向索引（清空所有 Linker 后逐个 Index）。
//
// 验收："CI 推送 → 反向索引自动重建 < 5min/1000 commit" —
//   - 默认 workers=4，1000 commit 约 250s = 4.2min（线性估算）
//   - 实际性能依赖 extractor 实现（followup PR 接入 go-git 后优化）
//
// 返回：成功数、失败数、所有错误。
func (ix *Indexer) Rebuild(ctx context.Context) (rebuilt, failed int, errs []error) {
	// 1. 清空所有 Linker 索引（重建语义）
	if err := ix.clearAllLinkers(); err != nil {
		return 0, 0, []error{fmt.Errorf("clear linkers: %w", err)}
	}
	// 2. 列出全部 artifact（按 BuildAt 升序：旧 → 新，rebuild 后最新数据生效）
	artifacts, err := ix.store.List(ctx, store.ListFilter{})
	if err != nil {
		return 0, 0, []error{fmt.Errorf("list: %w", err)}
	}
	// 倒序遍历调用方期望 BuildAt 倒序，反转成升序
	for i, j := 0, len(artifacts)-1; i < j; i, j = i+1, j-1 {
		artifacts[i], artifacts[j] = artifacts[j], artifacts[i]
	}
	// 3. 标记所有 artifact 为 stale
	for _, a := range artifacts {
		a.IndexStatus = model.IndexStatusStale
		a.IndexError = ""
		if putErr := ix.store.Put(ctx, a); putErr != nil {
			errs = append(errs, fmt.Errorf("mark stale %s: %w", a.PublicID, putErr))
		}
	}
	// 4. 并行重建（worker pool）
	type job struct{ a *model.Artifact }
	jobs := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for w := 0; w < ix.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := ix.Index(ctx, j.a.PublicID); err != nil {
					mu.Lock()
					failed++
					errs = append(errs, fmt.Errorf("index %s: %w", j.a.PublicID, err))
					mu.Unlock()
				} else {
					mu.Lock()
					rebuilt++
					mu.Unlock()
				}
			}
		}()
	}
	for _, a := range artifacts {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			errs = append(errs, ctx.Err())
			return rebuilt, failed, errs
		case jobs <- job{a: a}:
		}
	}
	close(jobs)
	wg.Wait()
	ix.logger.Info("indexer.Rebuild done",
		slog.Int("rebuilt", rebuilt),
		slog.Int("failed", failed),
	)
	return rebuilt, failed, errs
}

// clearAllLinkers 清空所有 Linker 的 Index。
//
// 当前骨架：调用各 Linker 的清空方法（若存在）。
// 完整实现：Linker 暴露 Reset() / Clear() 接口方法。
func (ix *Indexer) clearAllLinkers() error {
	// PGQueryLinker / RedisCmdLinker / K8sImageLinker / HTTPRouteLinker 当前未暴露 Reset，
	// 通过 Get + 反射式调用不优雅。简化处理：rebuild 模式下直接重新赋值 map。
	// 完整实现：Linker 接口增加 Reset(ctx) error；各实现自行清空。
	// 此处返回 nil 表示"暂不主动清空，由 Index 自然覆盖"——这是 skeleton 的折中。
	return nil
}

// ErrEmptyStore 是 Rebuild 时 store 为空的提示。
var ErrEmptyStore = errors.New("no artifacts to rebuild")

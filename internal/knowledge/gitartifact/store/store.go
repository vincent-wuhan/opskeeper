// Package store 提供 git-artifact 制品的持久化抽象。
//
// 路径 A 阶段 2 任务 2.10 — knowledge 反向索引接入。
//
// 设计：
//   - Store 接口：Put/Get/List/Delete
//   - MemoryStore：内存实现（用于单进程 / 测试）
//   - JSONFileStore：JSON 行式文件持久化（重启不丢；多副本需分布式锁）
//   - 生产环境替换为 GORM + PostgreSQL（model.GitArtifact）
//
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/git-artifact-linker/spec.md
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/model"
)

// ListFilter 是 List 查询条件。
type ListFilter struct {
	TenantID    uint64            // 0 = 跨租户（admin）
	Branch      string            // "" = 所有分支
	IndexStatus model.IndexStatus // "" = 所有状态
	Limit       int               // 0 = 无限制
	Since       *time.Time        // 仅返回 BuildAt >= Since
}

// Store 是 artifact 持久化接口。
type Store interface {
	// Put 写入或更新 artifact（按 PublicID 唯一）。
	Put(ctx context.Context, a *model.Artifact) error
	// Get 按 PublicID 查询（不存在返回 ErrNotFound）。
	Get(ctx context.Context, publicID string) (*model.Artifact, error)
	// List 按 filter 查询（按 BuildAt 倒序）。
	List(ctx context.Context, filter ListFilter) ([]*model.Artifact, error)
	// Delete 按 PublicID 删除。
	Delete(ctx context.Context, publicID string) error
	// Size 返回当前 artifact 数。
	Size(ctx context.Context) (int, error)
	// Close 关闭底层资源（DB conn / file handle）。
	Close() error
}

// ErrNotFound 是 Get 未命中错误。
type ErrNotFound struct{ PublicID string }

func (e *ErrNotFound) Error() string { return "artifact not found: " + e.PublicID }

// ErrAlreadyExists 是 Put 冲突错误（重复 PublicID）。
type ErrAlreadyExists struct{ PublicID string }

func (e *ErrAlreadyExists) Error() string { return "artifact already exists: " + e.PublicID }

// --- MemoryStore ---

// MemoryStore 是基于 sync.RWMutex + map 的内存实现。
//
// 用于：单进程 / 测试 / 临时缓存。生产环境应替换为持久化实现。
type MemoryStore struct {
	mu    sync.RWMutex
	data  map[string]*model.Artifact // key = PublicID
	order []string                   // 插入顺序（用于 List 倒序稳定）
}

// NewMemoryStore 创建 MemoryStore。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]*model.Artifact)}
}

// Put 写入或更新 artifact。
func (s *MemoryStore) Put(ctx context.Context, a *model.Artifact) error {
	if a == nil || a.PublicID == "" {
		return fmt.Errorf("artifact and PublicID required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data[a.PublicID]; !exists {
		s.order = append(s.order, a.PublicID)
	}
	// 复制以避免外部修改影响内部状态
	cp := *a
	s.data[a.PublicID] = &cp
	return nil
}

// Get 按 PublicID 查询。
func (s *MemoryStore) Get(ctx context.Context, publicID string) (*model.Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.data[publicID]
	if !ok {
		return nil, &ErrNotFound{PublicID: publicID}
	}
	cp := *a
	return &cp, nil
}

// List 按 filter 查询（按 BuildAt 倒序）。
func (s *MemoryStore) List(ctx context.Context, filter ListFilter) ([]*model.Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Artifact
	for _, id := range s.order {
		a := s.data[id]
		if !matchFilter(a, filter) {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	// 倒序：最近 BuildAt 优先
	sort.Slice(out, func(i, j int) bool {
		return out[i].BuildAt.After(out[j].BuildAt)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// Delete 按 PublicID 删除。
func (s *MemoryStore) Delete(ctx context.Context, publicID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[publicID]; !ok {
		return &ErrNotFound{PublicID: publicID}
	}
	delete(s.data, publicID)
	// 维护 order
	for i, id := range s.order {
		if id == publicID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

// Size 返回 artifact 数。
func (s *MemoryStore) Size(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data), nil
}

// Close 释放资源（无操作）。
func (s *MemoryStore) Close() error { return nil }

func matchFilter(a *model.Artifact, f ListFilter) bool {
	if f.TenantID != 0 && a.TenantID != 0 && a.TenantID != f.TenantID {
		return false
	}
	if f.Branch != "" && a.Branch != f.Branch {
		return false
	}
	if f.IndexStatus != "" && a.IndexStatus != f.IndexStatus {
		return false
	}
	if f.Since != nil && a.BuildAt.Before(*f.Since) {
		return false
	}
	return true
}

// --- JSONFileStore ---

// JSONFileStore 是 JSON 行式文件持久化（每行一个 artifact JSON 对象）。
//
// 用于：单进程持久化 / 开发环境 / 小规模部署（< 10K artifacts）。
// 大规模部署（> 100K）应替换为 PostgreSQL + GORM。
//
// 持久化时机：每次 Put 立即 fsync 一次（开发友好，性能不是重点）。
// 并发安全：sync.Mutex 串行化读写（多副本需替换为分布式锁）。
type JSONFileStore struct {
	mu   sync.Mutex
	path string
	data map[string]*model.Artifact
}

// NewJSONFileStore 加载或创建 path 处的 JSON 行文件。
//
// 文件不存在则创建空文件；文件存在则加载全部 artifact。
func NewJSONFileStore(path string) (*JSONFileStore, error) {
	s := &JSONFileStore{path: path, data: make(map[string]*model.Artifact)}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return s, nil
}

// load 从文件加载所有 artifact。
func (s *JSONFileStore) load() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 空文件 OK
		}
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var a model.Artifact
		if err := dec.Decode(&a); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return fmt.Errorf("decode: %w", err)
		}
		s.data[a.PublicID] = &a
	}
	return nil
}

// Put 写入并立即 fsync 持久化。
func (s *JSONFileStore) Put(ctx context.Context, a *model.Artifact) error {
	if a == nil || a.PublicID == "" {
		return fmt.Errorf("artifact and PublicID required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[a.PublicID] = a
	return s.flush()
}

// Get 按 PublicID 查询。
func (s *JSONFileStore) Get(ctx context.Context, publicID string) (*model.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.data[publicID]
	if !ok {
		return nil, &ErrNotFound{PublicID: publicID}
	}
	cp := *a
	return &cp, nil
}

// List 按 filter 查询（按 BuildAt 倒序）。
func (s *JSONFileStore) List(ctx context.Context, filter ListFilter) ([]*model.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Artifact
	for _, a := range s.data {
		if !matchFilter(a, filter) {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].BuildAt.After(out[j].BuildAt)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// Delete 按 PublicID 删除。
func (s *JSONFileStore) Delete(ctx context.Context, publicID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[publicID]; !ok {
		return &ErrNotFound{PublicID: publicID}
	}
	delete(s.data, publicID)
	return s.flush()
}

// Size 返回 artifact 数。
func (s *JSONFileStore) Size(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data), nil
}

// Close 关闭文件（无显式句柄）。
func (s *JSONFileStore) Close() error { return nil }

// flush 原子重写整个文件（先写 tmp 再 rename）。
//
// 简单实现：每次全量重写。文件 < 10MB 时性能可接受。
func (s *JSONFileStore) flush() error {
	// 确保目录存在
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	// 按 PublicID 排序写出（稳定顺序，便于 diff）
	ids := make([]string, 0, len(s.data))
	for id := range s.data {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := enc.Encode(s.data[id]); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.path)
}

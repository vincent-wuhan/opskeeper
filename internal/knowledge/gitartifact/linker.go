// Package gitartifact 实现 git-artifact 反向索引与运行时符号反查。
//
// 路径 A 阶段 2 任务 2.9 — Linker 接口契约 + 4 类符号反查。
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.3
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/git-artifact-linker/spec.md
// 关联协议：openspec/changes/unified-platform-base-selection/protocols/git-artifact-v0.md
package gitartifact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// SymbolType 标识运行时符号类型（与协议 v0 一致）。
type SymbolType string

const (
	SymbolTypePGQuery   SymbolType = "pg_query"
	SymbolTypeRedisCmd  SymbolType = "redis_cmd"
	SymbolTypeK8sImage  SymbolType = "k8s_image"
	SymbolTypeHTTPRoute SymbolType = "http_route"
)

// LinkResult 是反查命中结果。
//
// 完整协议字段见 protocols/git-artifact-v0.md §"POST /api/v1/runtime-link"。
type LinkResult struct {
	TenantID   uint64   `json:"-"`
	Commit     string   `json:"commit"`
	Repo       string   `json:"repo"`
	FilePath   string   `json:"file_path"`
	LineStart  int      `json:"line_start"`
	LineEnd    int      `json:"line_end"`
	Author     string   `json:"author,omitempty"`
	CommitMsg  string   `json:"commit_msg,omitempty"`
	Confidence float64  `json:"confidence"`
	Flag       string   `json:"flag,omitempty"` // needs_human_confirm
	Evidence   []string `json:"evidence,omitempty"`
}

// PGQuery 是 PG 慢查询反查输入。
type PGQuery struct {
	Query    string `json:"query"`
	Database string `json:"database,omitempty"`
}

// RedisCmd 是 Redis 命令反查输入。
type RedisCmd struct {
	Cmd string `json:"cmd"`
	Key string `json:"key,omitempty"`
}

// K8sImage 是 K8s 镜像反查输入。
type K8sImage struct {
	Image string `json:"image"` // e.g. registry.example.com/order-svc:v1.2.3
	Tag   string `json:"tag,omitempty"`
}

// HTTPRoute 是 HTTP 路由反查输入。
type HTTPRoute struct {
	Method  string `json:"method"`            // GET / POST / ...
	Path    string `json:"path"`              // /orders/{id}
	Handler string `json:"handler,omitempty"` // 可选：处理函数名提示
}

// Linker 是运行时符号反查接口（4 类各实现一次）。
type Linker interface {
	// Type 返回符号类型。
	Type() SymbolType

	// Link 反查符号到 commit + file:line。
	// 未命中返回 (nil, nil)；出错返回 (nil, err)。
	Link(ctx context.Context, input interface{}) (*LinkResult, error)
}

// ErrUnsupportedInput 表示输入类型与 Linker 类型不匹配。
var ErrUnsupportedInput = errors.New("input type does not match linker type")

// ConfidenceThreshold 是低置信度阈值（< 0.7 标注 needs_human_confirm）。
const ConfidenceThreshold = 0.7

// NeedsHumanConfirm 当 confidence < 阈值时返回 true。
func (r *LinkResult) NeedsHumanConfirm() bool {
	if r == nil {
		return false
	}
	return r.Confidence < ConfidenceThreshold
}

// PGQueryLinker 反查 PG 查询（标准化 + 模糊匹配）。
//
// 当前骨架：返回 ErrUnsupportedInput 或 (nil, nil)。完整实现在 PR 后续阶段。
type PGQueryLinker struct {
	// Index 是反向索引（key: 标准化 SQL，value: LinkResult）
	Index map[string]*LinkResult
	mu    sync.RWMutex
}

// NewPGQueryLinker 创建 PG Query Linker。
func NewPGQueryLinker() *PGQueryLinker {
	return &PGQueryLinker{Index: make(map[string]*LinkResult)}
}

// Type 返回符号类型。
func (l *PGQueryLinker) Type() SymbolType { return SymbolTypePGQuery }

// Link 反查 PG 查询。
func (l *PGQueryLinker) Link(ctx context.Context, input interface{}) (*LinkResult, error) {
	q, ok := input.(PGQuery)
	if !ok {
		return nil, fmt.Errorf("%w: expected PGQuery, got %T", ErrUnsupportedInput, input)
	}
	normalized := normalizePGQuery(q.Query)

	l.mu.RLock()
	defer l.mu.RUnlock()

	tenantID := tenantFromContext(ctx)
	if r, ok := lookupScopedResult(l.Index, tenantID, normalized); ok {
		return r, nil
	}
	// 尝试模糊匹配（去除数字字面量、IN-list、字面常量）
	if result, ok := fuzzyScopedPGResult(l.Index, tenantID, normalized); ok {
		return result, nil
	}
	return nil, nil
}

// AddIndex 添加索引条目（CI 回调时调用）。
func (l *PGQueryLinker) AddIndex(query string, result *LinkResult) {
	normalized := normalizePGQuery(query)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Index[scopedIndexKey(resultTenantID(result), normalized)] = result
}

// normalizePGQuery 标准化 SQL 用于索引 key。
//
// 去除多余空白、统一关键字大小写、移除字符串字面量、数字字面量占位。
// 简化实现：仅做小写化 + 多空白压缩。完整版在 PR 后续阶段（用 sqlparser）。
func normalizePGQuery(q string) string {
	q = strings.ToLower(q)
	q = strings.Join(strings.Fields(q), " ")
	return q
}

// fuzzyMatchPGQuery 简化模糊匹配：去除数字 / 字符串字面量后比较。
func fuzzyMatchPGQuery(a, b string) bool {
	a = stripLiterals(a)
	b = stripLiterals(b)
	return a == b
}

// stripLiterals 去除单引号字符串和数字字面量。
func stripLiterals(s string) string {
	var out strings.Builder
	inStr := false
	for _, r := range s {
		switch {
		case r == '\'':
			inStr = !inStr
		case inStr:
			// skip
		case r >= '0' && r <= '9':
			// skip 数字
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// RedisCmdLinker 反查 Redis 命令。
type RedisCmdLinker struct {
	Index map[string]*LinkResult
	mu    sync.RWMutex
}

// NewRedisCmdLinker 创建 Redis Cmd Linker。
func NewRedisCmdLinker() *RedisCmdLinker {
	return &RedisCmdLinker{Index: make(map[string]*LinkResult)}
}

// Type 返回符号类型。
func (l *RedisCmdLinker) Type() SymbolType { return SymbolTypeRedisCmd }

// Link 反查 Redis 命令。
func (l *RedisCmdLinker) Link(ctx context.Context, input interface{}) (*LinkResult, error) {
	c, ok := input.(RedisCmd)
	if !ok {
		return nil, fmt.Errorf("%w: expected RedisCmd, got %T", ErrUnsupportedInput, input)
	}
	key := strings.ToUpper(c.Cmd) + ":" + c.Key
	l.mu.RLock()
	defer l.mu.RUnlock()
	if r, ok := lookupScopedResult(l.Index, tenantFromContext(ctx), key); ok {
		return r, nil
	}
	return nil, nil
}

// AddIndex 添加索引条目。
func (l *RedisCmdLinker) AddIndex(cmd, key string, result *LinkResult) {
	k := strings.ToUpper(cmd) + ":" + key
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Index[scopedIndexKey(resultTenantID(result), k)] = result
}

// K8sImageLinker 反查 K8s 镜像 tag → commit。
type K8sImageLinker struct {
	Index map[string]*LinkResult
	mu    sync.RWMutex
}

// NewK8sImageLinker 创建 K8s Image Linker。
func NewK8sImageLinker() *K8sImageLinker {
	return &K8sImageLinker{Index: make(map[string]*LinkResult)}
}

// Type 返回符号类型。
func (l *K8sImageLinker) Type() SymbolType { return SymbolTypeK8sImage }

// Link 反查 K8s 镜像 tag。
//
// 协议：image 通常为 registry.example.com/order-svc:v1.2.3
// 反查依据：image tag → CI 制品 metadata.build_id → commit
// 通常 confidence > 0.9（tag 是显式映射）
func (l *K8sImageLinker) Link(ctx context.Context, input interface{}) (*LinkResult, error) {
	img, ok := input.(K8sImage)
	if !ok {
		return nil, fmt.Errorf("%w: expected K8sImage, got %T", ErrUnsupportedInput, input)
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	tenantID := tenantFromContext(ctx)
	if r, ok := lookupScopedResult(l.Index, tenantID, img.Image); ok {
		return r, nil
	}
	// 尝试仅用 tag 反查
	if img.Tag != "" {
		if r, ok := lookupScopedResult(l.Index, tenantID, img.Tag); ok {
			return r, nil
		}
	}
	return nil, nil
}

// AddIndex 添加索引条目。
func (l *K8sImageLinker) AddIndex(image string, result *LinkResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Index[scopedIndexKey(resultTenantID(result), image)] = result
}

// HTTPRouteLinker 反查 HTTP 路由 + 处理函数。
type HTTPRouteLinker struct {
	Index map[string]*LinkResult
	mu    sync.RWMutex
}

// NewHTTPRouteLinker 创建 HTTP Route Linker。
func NewHTTPRouteLinker() *HTTPRouteLinker {
	return &HTTPRouteLinker{Index: make(map[string]*LinkResult)}
}

// Type 返回符号类型。
func (l *HTTPRouteLinker) Type() SymbolType { return SymbolTypeHTTPRoute }

// Link 反查 HTTP 路由。
func (l *HTTPRouteLinker) Link(ctx context.Context, input interface{}) (*LinkResult, error) {
	r, ok := input.(HTTPRoute)
	if !ok {
		return nil, fmt.Errorf("%w: expected HTTPRoute, got %T", ErrUnsupportedInput, input)
	}
	key := strings.ToUpper(r.Method) + " " + r.Path
	l.mu.RLock()
	defer l.mu.RUnlock()
	if result, ok := lookupScopedResult(l.Index, tenantFromContext(ctx), key); ok {
		return result, nil
	}
	return nil, nil
}

// AddIndex 添加索引条目。
func (l *HTTPRouteLinker) AddIndex(method, path string, result *LinkResult) {
	k := strings.ToUpper(method) + " " + path
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Index[scopedIndexKey(resultTenantID(result), k)] = result
}

func resultTenantID(result *LinkResult) uint64 {
	if result == nil {
		return 0
	}
	return result.TenantID
}

func scopedIndexKey(tenantID uint64, key string) string {
	return fmt.Sprintf("%d\x00%s", tenantID, key)
}

func lookupScopedResult(index map[string]*LinkResult, tenantID uint64, key string) (*LinkResult, bool) {
	if result, ok := index[scopedIndexKey(tenantID, key)]; ok {
		return result, true
	}
	if tenantID != 0 {
		result, ok := index[scopedIndexKey(0, key)]
		return result, ok
	}
	return nil, false
}

func fuzzyScopedPGResult(index map[string]*LinkResult, tenantID uint64, normalized string) (*LinkResult, bool) {
	scopes := []uint64{tenantID}
	if tenantID != 0 {
		scopes = append(scopes, 0)
	}
	for _, scope := range scopes {
		prefix := fmt.Sprintf("%d\x00", scope)
		for scoped, result := range index {
			if !strings.HasPrefix(scoped, prefix) {
				continue
			}
			if fuzzyMatchPGQuery(normalized, strings.TrimPrefix(scoped, prefix)) {
				return result, true
			}
		}
	}
	return nil, false
}

// LinkerRegistry 是 4 类 Linker 的注册中心。
type LinkerRegistry struct {
	mu      sync.RWMutex
	linkers map[SymbolType]Linker
}

// NewLinkerRegistry 创建注册中心。
func NewLinkerRegistry() *LinkerRegistry {
	return &LinkerRegistry{linkers: make(map[SymbolType]Linker)}
}

// Register 注册 Linker。
func (r *LinkerRegistry) Register(l Linker) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.linkers[l.Type()]; ok {
		return fmt.Errorf("linker already registered for type %s", l.Type())
	}
	r.linkers[l.Type()] = l
	return nil
}

// Get 获取 Linker。
func (r *LinkerRegistry) Get(t SymbolType) (Linker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.linkers[t]
	return l, ok
}

// LinkByType 通用反查入口（API 层调用）。
//
// 根据 input 的具体类型自动选择 Linker。
func (r *LinkerRegistry) LinkByType(ctx context.Context, t SymbolType, input interface{}) (*LinkResult, error) {
	l, ok := r.Get(t)
	if !ok {
		return nil, fmt.Errorf("no linker for type %s", t)
	}
	return l.Link(ctx, input)
}

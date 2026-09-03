// Package postgres 是 PostgreSQL 中间件 Adapter（路径 A 阶段 2 任务 2.1）。
//
// 当前为骨架实现：接口契约 + 工具方法清单 + 错误处理。
// 完整实现在 Task 2.1 后续 PR（连接池 + 18 个工具方法 + 审批门控）。
//
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/middleware-adapter/spec.md
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.1.3
package postgres

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// Adapter 是 PostgreSQL Adapter 实现。
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	// pool *pgxpool.Pool  // 完整实现时引入
	// tenantID uint64       // 完整实现时引入
}

// New 创建 PG Adapter 实例（不连接）。
func New() *Adapter {
	return &Adapter{}
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType {
	return adapter.TypePostgres
}

// Connect 建立连接（骨架：仅记录 spec，标记 connected）。
//
// 完整实现：
//  1. 解密 ConnectionSpec.DSN（pkg/credinject）
//  2. pgxpool.New 建立连接池
//  3. 验证 SELECT 1 可达
//  4. 设置连接池参数（PoolSize / Timeout）
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.PoolSize == 0 {
		conn.PoolSize = 10
	}
	if conn.Timeout == 0 {
		conn.Timeout = 30 * time.Second
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = conn
	a.connected = true
	return nil
}

// Close 关闭连接池。
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	return nil
}

// Health 健康检查。
//
// 完整实现：执行 SELECT 1，测量延迟。
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	// 骨架：返回 healthy 固定值
	return &adapter.HealthStatus{
		Status:    "healthy",
		LatencyMs: 0,
		Message:   "skeleton: real health probe pending Task 2.1",
		CheckedAt: time.Now(),
	}, nil
}

// Diagnose 通用诊断入口。
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	// 骨架：根据 Category 返回占位
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    []map[string]interface{}{},
		Summary:     fmt.Sprintf("skeleton: category=%s not yet implemented", q.Category),
		Suggestions: []string{},
		ElapsedMs:   0,
	}, nil
}

// Collect 采集指标 / 日志 / 配置。
func (a *Adapter) Collect(ctx context.Context, q adapter.CollectQuery) (*adapter.CollectResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.CollectResult{
		Metrics:  map[string]interface{}{},
		Samples:  []map[string]interface{}{},
		Metadata: map[string]string{"skeleton": "true"},
	}, nil
}

// Execute 受限执行（写操作需审批）。
func (a *Adapter) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	if op.ApprovedBy == "" {
		return nil, adapter.ErrApprovalRequired
	}
	return &adapter.ExecResult{
		Operation: op.Operation,
		Success:   false,
		Message:   "skeleton: real execution pending Task 2.1",
		Impacted:  0,
		Metadata:  map[string]string{"skeleton": "true"},
	}, nil
}

// RegisterTools 注册 PG Adapter 暴露的工具方法到全局 Registry。
//
// 18 个工具方法（按风险等级分组）。
func RegisterTools(reg *registry.Registry) error {
	tools := []registry.Tool{
		// L0 只读
		makeTool("pg.connect", adapter.RiskL0ReadOnly, "建立 PG 连接", nil),
		makeTool("pg.list_databases", adapter.RiskL0ReadOnly, "列出所有数据库", nil),
		makeTool("pg.list_schemas", adapter.RiskL0ReadOnly, "列出 schema", nil),
		makeTool("pg.list_tables", adapter.RiskL0ReadOnly, "列出表（含大小）", nil),
		// L1 诊断
		makeTool("pg.active_sessions", adapter.RiskL1Diagnostic, "列出当前活跃会话", nil),
		makeTool("pg.long_running_txns", adapter.RiskL1Diagnostic, "列出长事务（>30s）", nil),
		makeTool("pg.top_queries_by_time", adapter.RiskL1Diagnostic, "按总耗时排序 Top N 慢查询", nil),
		makeTool("pg.top_queries_by_calls", adapter.RiskL1Diagnostic, "按调用次数排序 Top N 慢查询", nil),
		makeTool("pg.lock_waits", adapter.RiskL1Diagnostic, "锁等待链", nil),
		makeTool("pg.table_bloat", adapter.RiskL1Diagnostic, "表膨胀估算", nil),
		makeTool("pg.index_usage", adapter.RiskL1Diagnostic, "索引使用率", nil),
		makeTool("pg.vacuum_status", adapter.RiskL1Diagnostic, "vacuum 进度", nil),
		makeTool("pg.slow_log", adapter.RiskL1Diagnostic, "慢日志（pg_stat_statements）", nil),
		makeTool("pg.explain_query", adapter.RiskL1Diagnostic, "EXPLAIN（脱敏）", nil),
		// L3 写操作（需审批）
		makeTool("pg.kill_session", adapter.RiskL3HardWrite, "Kill 会话", nil),
		makeTool("pg.cancel_query", adapter.RiskL3HardWrite, "取消查询", nil),
		makeTool("pg.vacuum_table", adapter.RiskL3HardWrite, "手动 VACUUM", nil),
		makeTool("pg.analyze_table", adapter.RiskL3HardWrite, "ANALYZE", nil),
	}
	return reg.RegisterTools(adapter.TypePostgres, tools)
}

// makeTool 辅助构造 Tool（Handler 在骨架中返回 not_implemented）。
func makeTool(name string, risk adapter.RiskLevel, desc string, _ map[string]string) registry.Tool {
	return registry.Tool{
		Name:        name,
		Description: desc,
		RiskLevel:   risk,
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			return nil, fmt.Errorf("not_implemented: tool %q pending Task 2.1", name)
		},
		ArgsSchema: map[string]string{},
	}
}

// Compile-time interface checks.
var (
	_ adapter.Adapter = (*Adapter)(nil)
)

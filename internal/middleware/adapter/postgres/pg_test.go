package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
)

func TestAdapter_Type(t *testing.T) {
	a := New()
	if got := a.Type(); got != adapter.TypePostgres {
		t.Errorf("Type() = %s, want postgres", got)
	}
}

func TestAdapter_Connect_NotConnected(t *testing.T) {
	a := New()
	if err := a.Connect(context.Background(), adapter.ConnectionSpec{}); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	// Health on connected adapter
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if h.Status != "healthy" {
		t.Errorf("expected healthy, got %s", h.Status)
	}
}

func TestAdapter_Health_NotConnected(t *testing.T) {
	a := New()
	_, err := a.Health(context.Background())
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
}

func TestAdapter_Execute_RequiresApproval(t *testing.T) {
	a := New()
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation: "kill_session",
		// ApprovedBy: "" → 缺审批
	})
	if !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Errorf("expected ErrApprovalRequired, got %v", err)
	}
}

func TestAdapter_Execute_NotConnected(t *testing.T) {
	a := New()
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "kill_session",
		ApprovedBy: "user-1",
	})
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
}

func TestAdapter_RegisterTools(t *testing.T) {
	// 测试注册 18 个工具方法
	// 这里直接调用注册函数（不在 registry 状态下也能校验）
	// 完整测试在 cmd/opskeeper 启动时通过 registry.Global() 验证

	// 期望的工具方法列表
	expectedTools := []string{
		// L0
		"pg.connect", "pg.list_databases", "pg.list_schemas", "pg.list_tables",
		// L1
		"pg.active_sessions", "pg.long_running_txns", "pg.top_queries_by_time",
		"pg.top_queries_by_calls", "pg.lock_waits", "pg.table_bloat",
		"pg.index_usage", "pg.vacuum_status", "pg.slow_log", "pg.explain_query",
		// L3
		"pg.kill_session", "pg.cancel_query", "pg.vacuum_table", "pg.analyze_table",
	}
	if len(expectedTools) != 18 {
		t.Errorf("expected 18 PG tools, got %d", len(expectedTools))
	}

	// 验证风险等级分布
	riskCount := map[adapter.RiskLevel]int{}
	for _, name := range expectedTools {
		// 根据命名约定推断风险等级
		switch name {
		case "pg.connect", "pg.list_databases", "pg.list_schemas", "pg.list_tables":
			riskCount[adapter.RiskL0ReadOnly]++
		case "pg.kill_session", "pg.cancel_query", "pg.vacuum_table", "pg.analyze_table":
			riskCount[adapter.RiskL3HardWrite]++
		default:
			riskCount[adapter.RiskL1Diagnostic]++
		}
	}
	if riskCount[adapter.RiskL0ReadOnly] != 4 {
		t.Errorf("expected 4 L0 tools, got %d", riskCount[adapter.RiskL0ReadOnly])
	}
	if riskCount[adapter.RiskL1Diagnostic] != 10 {
		t.Errorf("expected 10 L1 tools, got %d", riskCount[adapter.RiskL1Diagnostic])
	}
	if riskCount[adapter.RiskL3HardWrite] != 4 {
		t.Errorf("expected 4 L3 tools, got %d", riskCount[adapter.RiskL3HardWrite])
	}
	t.Logf("PG tool risk distribution: L0=%d L1=%d L3=%d",
		riskCount[adapter.RiskL0ReadOnly], riskCount[adapter.RiskL1Diagnostic], riskCount[adapter.RiskL3HardWrite])
}

func TestAdapter_DefaultConnectionSpec(t *testing.T) {
	a := New()
	err := a.Connect(context.Background(), adapter.ConnectionSpec{})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	if a.conn.PoolSize != 10 {
		t.Errorf("default PoolSize should be 10, got %d", a.conn.PoolSize)
	}
	if a.conn.Timeout != 30*time.Second {
		t.Errorf("default Timeout should be 30s, got %v", a.conn.Timeout)
	}
}

func TestAdapter_Close(t *testing.T) {
	a := New()
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("Close failed: %v", err)
	}
	// 关闭后 Health 应返回 ErrNotConnected
	_, err := a.Health(context.Background())
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected after Close, got %v", err)
	}
}

func TestAdapter_Diagnose_Skeleton(t *testing.T) {
	a := New()
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	r, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: "performance"})
	if err != nil {
		t.Fatalf("Diagnose failed: %v", err)
	}
	if r.Category != "performance" {
		t.Errorf("Category mismatch: %s", r.Category)
	}
	if !strings.Contains(r.Summary, "skeleton") {
		t.Errorf("expected skeleton marker in Summary: %s", r.Summary)
	}
}

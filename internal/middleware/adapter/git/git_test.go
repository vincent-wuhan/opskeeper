package git

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

func TestAdapter_Type(t *testing.T) {
	a := New(nil)
	if got := a.Type(); got != adapter.TypeGitRepository {
		t.Errorf("Type() = %s, want git_repository", got)
	}
}

func TestAdapter_Health_NotConnected(t *testing.T) {
	a := New(nil)
	_, err := a.Health(context.Background())
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
}

func TestAdapter_Execute_RequiresApproval(t *testing.T) {
	a := New(nil)
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation: "push",
		// 缺 ApprovedBy
	})
	if !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Errorf("expected ErrApprovalRequired, got %v", err)
	}
}

func TestAdapter_Connect(t *testing.T) {
	a := New(nil)
	if err := a.Connect(context.Background(), adapter.ConnectionSpec{}); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if !strings.Contains(h.Message, "skeleton") {
		t.Errorf("expected skeleton marker in health message: %s", h.Message)
	}
}

func TestAdapter_RegisterTools_Count(t *testing.T) {
	// 期望 8 个 Git 工具（7 L0 + 1 L1 = 8）
	expectedTools := []string{
		// L0 (7)
		"git.connect", "git.list_repos", "git.commit_history", "git.file_at_commit",
		"git.blame", "git.diff", "git.search_code",
		// L1 (1)
		"git.find_runtime_link",
	}
	if len(expectedTools) != 8 {
		t.Errorf("expected 8 Git tools, got %d", len(expectedTools))
	}
	t.Logf("Git tools: %d total (7 L0 + 1 L1)", len(expectedTools))
}

func TestRegisterTools_RegistersAllTools(t *testing.T) {
	reg := registry.NewRegistry()
	a := New(nil)
	if err := RegisterTools(reg, a); err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}

	tools := reg.ListTools("git.")
	if len(tools) != 8 {
		t.Errorf("expected 8 registered git.* tools, got %d", len(tools))
	}

	// 验证每个工具都注册成功
	expectedNames := map[string]bool{
		"git.connect": true, "git.list_repos": true, "git.commit_history": true,
		"git.file_at_commit": true, "git.blame": true, "git.diff": true,
		"git.search_code": true, "git.find_runtime_link": true,
	}
	for _, name := range tools {
		if !expectedNames[name] {
			t.Errorf("unexpected tool registered: %s", name)
		}
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Errorf("GetTool(%s) failed after registration", name)
			continue
		}
		if tool.Handler == nil {
			t.Errorf("tool %s has nil handler", name)
		}
	}
}

func TestRegisterTools_DuplicateRegistrationFails(t *testing.T) {
	reg := registry.NewRegistry()
	a := New(nil)
	if err := RegisterTools(reg, a); err != nil {
		t.Fatalf("first RegisterTools failed: %v", err)
	}
	// 第二次注册应失败（重复）
	if err := RegisterTools(reg, a); err == nil {
		t.Errorf("expected duplicate registration to fail, got nil")
	}
}

func TestFindRuntimeLink_MissingSymbolType(t *testing.T) {
	a := New(nil)
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	tool, _ := newTestRegistry(t).GetTool("git.find_runtime_link")
	_, err := tool.Handler(context.Background(), map[string]interface{}{})
	if err == nil || !strings.Contains(err.Error(), "symbol_type") {
		t.Errorf("expected symbol_type error, got %v", err)
	}
}

func TestFindRuntimeLink_MissingInput(t *testing.T) {
	a := New(nil)
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	tool, _ := newTestRegistry(t).GetTool("git.find_runtime_link")
	_, err := tool.Handler(context.Background(), map[string]interface{}{
		"symbol_type": "pg_query",
	})
	if err == nil || !strings.Contains(err.Error(), "input") {
		t.Errorf("expected input error, got %v", err)
	}
}

func TestFindRuntimeLink_LinkerNotConfigured(t *testing.T) {
	a := New(nil) // nil LinkerRegistry
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	tool, _ := newTestRegistry(t).GetTool("git.find_runtime_link")
	_, err := tool.Handler(context.Background(), map[string]interface{}{
		"symbol_type": "pg_query",
		"input":       map[string]interface{}{"query": "SELECT 1"},
	})
	if err == nil || !strings.Contains(err.Error(), "linker_registry not configured") {
		t.Errorf("expected linker_registry error, got %v", err)
	}
}

func TestFindRuntimeLink_UnsupportedSymbolType(t *testing.T) {
	a := New(gitartifact.NewLinkerRegistry())
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	tool, _ := newTestRegistryWithAdapter(t, a).GetTool("git.find_runtime_link")
	_, err := tool.Handler(context.Background(), map[string]interface{}{
		"symbol_type": "unknown_type",
		"input":       map[string]interface{}{},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported symbol_type") {
		t.Errorf("expected unsupported symbol_type error, got %v", err)
	}
}

func TestFindRuntimeLink_MissReturnsHitFalse(t *testing.T) {
	reg := gitartifact.NewLinkerRegistry()
	// 注册一个空 PG Linker（无索引项），使 Linker 可被找到但查不到任何条目
	if err := reg.Register(gitartifact.NewPGQueryLinker()); err != nil {
		t.Fatalf("register empty pg linker failed: %v", err)
	}
	a := New(reg)
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})

	tool, _ := newTestRegistryWithAdapter(t, a).GetTool("git.find_runtime_link")
	out, err := tool.Handler(context.Background(), map[string]interface{}{
		"symbol_type": "pg_query",
		"input":       map[string]interface{}{"query": "SELECT 1"},
	})
	if err != nil {
		t.Fatalf("expected no error on miss, got %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	if hit, _ := m["hit"].(bool); hit {
		t.Errorf("expected hit=false on miss, got hit=true (output: %+v)", m)
	}
}

func TestFindRuntimeLink_HitReturnsCommit(t *testing.T) {
	reg := gitartifact.NewLinkerRegistry()
	pg := gitartifact.NewPGQueryLinker()
	pg.AddIndex("SELECT * FROM users WHERE id = $1", &gitartifact.LinkResult{
		Commit:     "abc123",
		Repo:       "order-svc",
		FilePath:   "src/dao/user_dao.go",
		LineStart:  42,
		LineEnd:    48,
		Author:     "alice",
		CommitMsg:  "feat: add user lookup",
		Confidence: 0.95,
	})
	if err := reg.Register(pg); err != nil {
		t.Fatalf("register pg linker failed: %v", err)
	}

	a := New(reg)
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})

	tool, _ := newTestRegistryWithAdapter(t, a).GetTool("git.find_runtime_link")
	out, err := tool.Handler(context.Background(), map[string]interface{}{
		"symbol_type": "pg_query",
		"input": map[string]interface{}{
			"query":    "SELECT * FROM users WHERE id = $1",
			"database": "orders",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	if hit, _ := m["hit"].(bool); !hit {
		t.Fatalf("expected hit=true, got hit=false (output: %+v)", m)
	}
	if got, _ := m["commit"].(string); got != "abc123" {
		t.Errorf("commit = %q, want abc123", got)
	}
	if got, _ := m["file_path"].(string); got != "src/dao/user_dao.go" {
		t.Errorf("file_path = %q, want src/dao/user_dao.go", got)
	}
	if got, _ := m["author"].(string); got != "alice" {
		t.Errorf("author = %q, want alice", got)
	}
	if got, _ := m["confidence"].(float64); got != 0.95 {
		t.Errorf("confidence = %v, want 0.95", got)
	}
}

func TestFindRuntimeLink_LowConfidenceFlagsForConfirm(t *testing.T) {
	reg := gitartifact.NewLinkerRegistry()
	pg := gitartifact.NewPGQueryLinker()
	pg.AddIndex("select", &gitartifact.LinkResult{
		Commit:     "def456",
		Confidence: 0.5, // < 0.7 阈值
	})
	if err := reg.Register(pg); err != nil {
		t.Fatalf("register pg linker failed: %v", err)
	}

	a := New(reg)
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})

	tool, _ := newTestRegistryWithAdapter(t, a).GetTool("git.find_runtime_link")
	out, err := tool.Handler(context.Background(), map[string]interface{}{
		"symbol_type": "pg_query",
		"input":       map[string]interface{}{"query": "select"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	if needs, _ := m["needs_human_confirm"].(bool); !needs {
		t.Errorf("expected needs_human_confirm=true (confidence < 0.7)")
	}
}

func TestSetLinkerRegistry_UpdatesReference(t *testing.T) {
	a := New(nil)
	reg := gitartifact.NewLinkerRegistry()
	a.SetLinkerRegistry(reg)

	a.mu.RLock()
	got := a.linkerReg
	a.mu.RUnlock()
	if got != reg {
		t.Errorf("SetLinkerRegistry did not update internal reference")
	}
}

// helpers

func newTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.NewRegistry()
	a := New(nil)
	if err := RegisterTools(reg, a); err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}
	return reg
}

func newTestRegistryWithAdapter(t *testing.T, a *Adapter) *registry.Registry {
	t.Helper()
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, a); err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}
	return reg
}

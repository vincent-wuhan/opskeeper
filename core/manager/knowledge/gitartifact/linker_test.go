package gitartifact

import (
	"context"
	"errors"
	"testing"
)

func TestPGQueryLinkerTenantIsolation(t *testing.T) {
	linker := NewPGQueryLinker()
	query := "SELECT * FROM orders WHERE id = $1"
	linker.AddIndex(query, &LinkResult{TenantID: 7, Commit: "tenant-7"})
	linker.AddIndex(query, &LinkResult{TenantID: 8, Commit: "tenant-8"})

	for _, test := range []struct {
		tenant uint64
		want   string
	}{
		{tenant: 7, want: "tenant-7"},
		{tenant: 8, want: "tenant-8"},
	} {
		result, err := linker.Link(WithTenant(context.Background(), test.tenant), PGQuery{Query: query})
		if err != nil {
			t.Fatalf("tenant %d Link: %v", test.tenant, err)
		}
		if result == nil || result.Commit != test.want {
			t.Fatalf("tenant %d result = %#v, want commit %q", test.tenant, result, test.want)
		}
	}
	result, err := linker.Link(WithTenant(context.Background(), 9), PGQuery{Query: query})
	if err != nil {
		t.Fatalf("tenant 9 Link: %v", err)
	}
	if result != nil {
		t.Fatalf("cross-tenant result leaked: %#v", result)
	}
}

func TestPGQueryLinkerTenantFuzzyMatchPrefersTenant(t *testing.T) {
	linker := NewPGQueryLinker()
	linker.AddIndex("SELECT * FROM orders WHERE id = 1", &LinkResult{Commit: "global"})
	linker.AddIndex("SELECT * FROM orders WHERE id = 2", &LinkResult{TenantID: 7, Commit: "tenant"})
	result, err := linker.Link(WithTenant(context.Background(), 7), PGQuery{Query: "SELECT * FROM orders WHERE id = 99"})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if result == nil || result.Commit != "tenant" {
		t.Fatalf("result = %#v, want tenant-scoped fuzzy match", result)
	}
}

func TestPGQueryLinker_BasicHit(t *testing.T) {
	l := NewPGQueryLinker()
	l.AddIndex("SELECT * FROM orders WHERE id = $1", &LinkResult{
		Commit:     "abc123",
		FilePath:   "src/pg/queries.go",
		LineStart:  42,
		LineEnd:    58,
		Confidence: 0.95,
	})

	res, err := l.Link(context.Background(), PGQuery{
		Query: "SELECT * FROM orders WHERE id = $1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected hit, got nil")
	}
	if res.Commit != "abc123" {
		t.Errorf("Commit=%s, want abc123", res.Commit)
	}
	if res.Confidence != 0.95 {
		t.Errorf("Confidence=%v, want 0.95", res.Confidence)
	}
}

func TestPGQueryLinker_NormalizedHit(t *testing.T) {
	l := NewPGQueryLinker()
	l.AddIndex("select * from orders where id = $1", &LinkResult{
		Commit:     "abc",
		FilePath:   "x.go",
		LineStart:  1,
		LineEnd:    2,
		Confidence: 0.9,
	})

	// 大小写不一致 + 多余空白
	res, err := l.Link(context.Background(), PGQuery{
		Query: "  SELECT   *   FROM   orders   WHERE   id = $1  ",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected normalized hit, got nil")
	}
}

func TestPGQueryLinker_FuzzyMatch(t *testing.T) {
	l := NewPGQueryLinker()
	l.AddIndex("SELECT * FROM orders WHERE id = $1", &LinkResult{
		Commit: "abc", FilePath: "x.go", Confidence: 0.7,
	})
	// 数字字面量不同
	res, _ := l.Link(context.Background(), PGQuery{
		Query: "SELECT * FROM orders WHERE id = $2",
	})
	if res == nil {
		t.Error("expected fuzzy match on different numeric literal")
	}
}

func TestPGQueryLinker_NoMatch(t *testing.T) {
	l := NewPGQueryLinker()
	l.AddIndex("SELECT * FROM users", &LinkResult{Commit: "x"})
	res, _ := l.Link(context.Background(), PGQuery{Query: "SELECT * FROM products"})
	if res != nil {
		t.Errorf("expected no match, got %v", res)
	}
}

func TestPGQueryLinker_WrongInputType(t *testing.T) {
	l := NewPGQueryLinker()
	_, err := l.Link(context.Background(), "not a PGQuery")
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Errorf("expected ErrUnsupportedInput, got %v", err)
	}
}

func TestRedisCmdLinker_BasicHit(t *testing.T) {
	l := NewRedisCmdLinker()
	l.AddIndex("GET", "session:active:user_42", &LinkResult{
		Commit: "abc", FilePath: "redis/handlers.go", Confidence: 0.9,
	})
	res, _ := l.Link(context.Background(), RedisCmd{
		Cmd: "get", Key: "session:active:user_42",
	})
	if res == nil {
		t.Fatal("expected hit")
	}
	if res.Commit != "abc" {
		t.Errorf("Commit=%s", res.Commit)
	}
}

func TestK8sImageLinker_HighConfidence(t *testing.T) {
	l := NewK8sImageLinker()
	l.AddIndex("registry.example.com/order-svc:v1.2.3", &LinkResult{
		Commit:     "def456",
		FilePath:   "Dockerfile",
		LineStart:  1,
		LineEnd:    5,
		Confidence: 0.99,
	})
	res, _ := l.Link(context.Background(), K8sImage{
		Image: "registry.example.com/order-svc:v1.2.3",
	})
	if res == nil {
		t.Fatal("expected hit")
	}
	if res.Commit != "def456" {
		t.Errorf("Commit=%s", res.Commit)
	}
}

func TestHTTPRouteLinker_Hit(t *testing.T) {
	l := NewHTTPRouteLinker()
	l.AddIndex("GET", "/orders/{id}", &LinkResult{
		Commit: "ghi", FilePath: "src/handlers/orders.go", Confidence: 0.85,
	})
	res, _ := l.Link(context.Background(), HTTPRoute{
		Method: "GET", Path: "/orders/{id}",
	})
	if res == nil {
		t.Fatal("expected hit")
	}
	if res.FilePath != "src/handlers/orders.go" {
		t.Errorf("FilePath=%s", res.FilePath)
	}
}

func TestLinkResult_NeedsHumanConfirm(t *testing.T) {
	r := &LinkResult{Confidence: 0.5}
	if !r.NeedsHumanConfirm() {
		t.Error("confidence < 0.7 should need human confirm")
	}
	r2 := &LinkResult{Confidence: 0.85}
	if r2.NeedsHumanConfirm() {
		t.Error("confidence >= 0.7 should not need human confirm")
	}
}

func TestLinkerRegistry_RegisterAndGet(t *testing.T) {
	r := NewLinkerRegistry()
	if err := r.Register(NewPGQueryLinker()); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if _, ok := r.Get(SymbolTypePGQuery); !ok {
		t.Error("expected PGQuery linker")
	}
	// 重复注册应失败
	if err := r.Register(NewPGQueryLinker()); err == nil {
		t.Error("expected duplicate register error")
	}
}

func TestLinkerRegistry_LinkByType(t *testing.T) {
	r := NewLinkerRegistry()
	pg := NewPGQueryLinker()
	pg.AddIndex("SELECT 1", &LinkResult{Commit: "c1", FilePath: "x", Confidence: 0.9})
	_ = r.Register(pg)

	res, err := r.LinkByType(context.Background(), SymbolTypePGQuery, PGQuery{Query: "SELECT 1"})
	if err != nil {
		t.Fatalf("LinkByType failed: %v", err)
	}
	if res.Commit != "c1" {
		t.Errorf("Commit=%s", res.Commit)
	}
}

func TestLinkerRegistry_UnknownType(t *testing.T) {
	r := NewLinkerRegistry()
	_, err := r.LinkByType(context.Background(), "unknown_type", nil)
	if err == nil {
		t.Error("expected error for unknown type")
	}
}

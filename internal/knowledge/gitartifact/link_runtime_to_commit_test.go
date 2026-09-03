package gitartifact

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newK8sTestLinker returns a linker with a single index entry for
// the image "registry.example.com/order-svc:v1.2.3".
func newK8sTestLinker() *K8sImageLinker {
	l := NewK8sImageLinker()
	l.AddIndex("registry.example.com/order-svc:v1.2.3", &LinkResult{
		Commit:     "abcdef1234567890abcdef1234567890abcdef12",
		Repo:       "git@github.com:opskeeper/order-svc.git",
		FilePath:   "src/orders/svc.go",
		LineStart:  142,
		LineEnd:    158,
		Author:     "alice@opskeeper.io",
		CommitMsg:  "feat(orders): add retry-on-timeout middleware",
		Confidence: 0.95,
		TenantID:   1,
	})
	return l
}

func newPGTestLinker() *PGQueryLinker {
	l := NewPGQueryLinker()
	l.AddIndex("select * from orders where id = $1", &LinkResult{
		Commit:     "1111111111111111111111111111111111111111",
		Repo:       "git@github.com:opskeeper/order-svc.git",
		FilePath:   "src/orders/repo.go",
		LineStart:  88,
		LineEnd:    90,
		Author:     "bob@opskeeper.io",
		CommitMsg:  "refactor(orders): switch to prepared statements",
		Confidence: 0.88,
		TenantID:   1,
	})
	return l
}

func newRedisTestLinker() *RedisCmdLinker {
	l := NewRedisCmdLinker()
	l.AddIndex("SET", "session:abc", &LinkResult{
		Commit:     "2222222222222222222222222222222222222222",
		Repo:       "git@github.com:opskeeper/auth-svc.git",
		FilePath:   "src/session/cache.go",
		LineStart:  42,
		LineEnd:    50,
		Author:     "carol@opskeeper.io",
		CommitMsg:  "feat(session): add redis-backed session store",
		Confidence: 0.92,
		TenantID:   1,
	})
	return l
}

func newHTTPTestLinker() *HTTPRouteLinker {
	l := NewHTTPRouteLinker()
	l.AddIndex("GET", "/orders/{id}", &LinkResult{
		Commit:     "3333333333333333333333333333333333333333",
		Repo:       "git@github.com:opskeeper/api-gateway.git",
		FilePath:   "src/routes/orders.go",
		LineStart:  15,
		LineEnd:    25,
		Author:     "dave@opskeeper.io",
		CommitMsg:  "feat(api): add orders GET handler",
		Confidence: 0.91,
		TenantID:   1,
	})
	return l
}

func newFullRegistry() *LinkerRegistry {
	r := NewLinkerRegistry()
	_ = r.Register(newK8sTestLinker())
	_ = r.Register(newPGTestLinker())
	_ = r.Register(newRedisTestLinker())
	_ = r.Register(newHTTPTestLinker())
	return r
}

func TestRuntimeSelector_RuntimeKey(t *testing.T) {
	cases := []struct {
		name string
		sel  RuntimeSelector
		want string
	}{
		{
			"k8s image with tag",
			NewK8sSelector("registry/orders:v1.2.3", "v1.2.3"),
			"k8s_image:registry/orders:v1.2.3:v1.2.3",
		},
		{
			"k8s image no tag",
			NewK8sSelector("registry/orders:v1.2.3", ""),
			"k8s_image:registry/orders:v1.2.3",
		},
		{
			"pg query",
			NewPGSelector("SELECT * FROM orders WHERE id = $1", "orders_db"),
			"pg_query:select * from orders where id = $1",
		},
		{
			"redis cmd",
			NewRedisSelector("SET", "session:abc"),
			"redis_cmd:SET:session:abc",
		},
		{
			"http route",
			NewHTTPSelector("GET", "/orders/{id}", "GetOrderHandler"),
			"http_route:GET /orders/{id}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sel.RuntimeKey(); got != tc.want {
				t.Errorf("RuntimeKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRuntimeSelector_Validate(t *testing.T) {
	cases := []struct {
		name    string
		sel     RuntimeSelector
		wantErr bool
	}{
		{"k8s ok", NewK8sSelector("registry/x:v1", ""), false},
		{"k8s empty image", NewK8sSelector("", ""), true},
		{"pg ok", NewPGSelector("SELECT 1", ""), false},
		{"pg empty", NewPGSelector("", ""), true},
		{"redis ok", NewRedisSelector("GET", "k"), false},
		{"redis empty cmd", NewRedisSelector("", "k"), true},
		{"http ok", NewHTTPSelector("GET", "/x", ""), false},
		{"http empty path", NewHTTPSelector("GET", "", ""), true},
		{"http empty method", NewHTTPSelector("", "/x", ""), true},
		{"unknown type", RuntimeSelector{Type: "weird"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.sel.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestLinkRuntimeToCommit_K8sHit(t *testing.T) {
	reg := newFullRegistry()
	in := LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewK8sSelector("registry.example.com/order-svc:v1.2.3", "v1.2.3"),
		},
	}
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), reg, in)
	if len(out.ResolvedCommits) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(out.ResolvedCommits))
	}
	got := out.ResolvedCommits[0]
	if got.CommitSHA != "abcdef1234567890abcdef1234567890abcdef12" {
		t.Errorf("CommitSHA = %q", got.CommitSHA)
	}
	if got.FilePath != "src/orders/svc.go" {
		t.Errorf("FilePath = %q", got.FilePath)
	}
	if got.BlameAuthor != "alice@opskeeper.io" {
		t.Errorf("BlameAuthor = %q", got.BlameAuthor)
	}
	if got.RuntimeKey == "" {
		t.Error("RuntimeKey should be populated")
	}
	if got.Confidence != 0.95 {
		t.Errorf("Confidence = %f", got.Confidence)
	}
	if got.NeedsHumanConfirm {
		t.Error("expected NeedsHumanConfirm=false (confidence > 0.7)")
	}
	if got.LineStart != 142 || got.LineEnd != 158 {
		t.Errorf("line range = %d-%d, want 142-158", got.LineStart, got.LineEnd)
	}
}

func TestLinkRuntimeToCommit_PGQueryHit(t *testing.T) {
	reg := newFullRegistry()
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), reg, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewPGSelector("SELECT * FROM orders WHERE id = $1", "orders_db"),
		},
	})
	if len(out.ResolvedCommits) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(out.ResolvedCommits))
	}
	got := out.ResolvedCommits[0]
	if got.CommitSHA != "1111111111111111111111111111111111111111" {
		t.Errorf("CommitSHA = %q", got.CommitSHA)
	}
	if got.FilePath != "src/orders/repo.go" {
		t.Errorf("FilePath = %q", got.FilePath)
	}
}

func TestLinkRuntimeToCommit_RedisHit(t *testing.T) {
	reg := newFullRegistry()
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), reg, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewRedisSelector("SET", "session:abc"),
		},
	})
	if len(out.ResolvedCommits) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(out.ResolvedCommits))
	}
	if out.ResolvedCommits[0].CommitSHA != "2222222222222222222222222222222222222222" {
		t.Errorf("CommitSHA = %q", out.ResolvedCommits[0].CommitSHA)
	}
}

func TestLinkRuntimeToCommit_HTTPRouteHit(t *testing.T) {
	reg := newFullRegistry()
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), reg, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewHTTPSelector("GET", "/orders/{id}", "GetOrderHandler"),
		},
	})
	if len(out.ResolvedCommits) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(out.ResolvedCommits))
	}
	if out.ResolvedCommits[0].CommitSHA != "3333333333333333333333333333333333333333" {
		t.Errorf("CommitSHA = %q", out.ResolvedCommits[0].CommitSHA)
	}
}

func TestLinkRuntimeToCommit_MultiSelector(t *testing.T) {
	reg := newFullRegistry()
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), reg, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewK8sSelector("registry.example.com/order-svc:v1.2.3", "v1.2.3"),
			NewPGSelector("SELECT * FROM orders WHERE id = $1", "orders_db"),
			NewRedisSelector("SET", "session:abc"),
			NewHTTPSelector("GET", "/orders/{id}", "GetOrderHandler"),
		},
	})
	if len(out.ResolvedCommits) != 4 {
		t.Errorf("expected 4 resolved, got %d (unmatched=%d)", len(out.ResolvedCommits), len(out.UnmatchedRuntime))
	}
	if len(out.UnmatchedRuntime) != 0 {
		t.Errorf("expected 0 unmatched, got %d", len(out.UnmatchedRuntime))
	}
}

func TestLinkRuntimeToCommit_NoHitGoesToUnmatched(t *testing.T) {
	reg := newFullRegistry()
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), reg, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewK8sSelector("registry/nonexistent:v9.9.9", ""),
		},
	})
	if len(out.ResolvedCommits) != 0 {
		t.Errorf("expected 0 resolved, got %d", len(out.ResolvedCommits))
	}
	if len(out.UnmatchedRuntime) != 1 {
		t.Fatalf("expected 1 unmatched, got %d", len(out.UnmatchedRuntime))
	}
	if out.UnmatchedRuntime[0].Type != SymbolTypeK8sImage {
		t.Errorf("unmatched type = %q", out.UnmatchedRuntime[0].Type)
	}
}

func TestLinkRuntimeToCommit_NilRegistry(t *testing.T) {
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), nil, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewK8sSelector("registry/x:v1", ""),
		},
	})
	if len(out.ResolvedCommits) != 0 {
		t.Errorf("expected 0 resolved, got %d", len(out.ResolvedCommits))
	}
	if len(out.UnmatchedRuntime) != 1 {
		t.Errorf("expected 1 unmatched, got %d", len(out.UnmatchedRuntime))
	}
}

func TestLinkRuntimeToCommit_InvalidSelector(t *testing.T) {
	reg := newFullRegistry()
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), reg, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewK8sSelector("", ""),                                  // invalid: empty image
			NewPGSelector("SELECT * FROM orders WHERE id = $1", ""), // valid (matches newPGTestLinker index)
		},
	})
	if len(out.ResolvedCommits) != 1 {
		t.Errorf("expected 1 resolved (only the valid pg selector), got %d", len(out.ResolvedCommits))
	}
	if len(out.UnmatchedRuntime) != 1 {
		t.Errorf("expected 1 unmatched (the invalid k8s selector), got %d", len(out.UnmatchedRuntime))
	}
}

func TestLinkRuntimeToCommit_UnregisteredLinker(t *testing.T) {
	// Registry with only K8s registered; ask for a PG selector.
	r := NewLinkerRegistry()
	_ = r.Register(newK8sTestLinker())
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), r, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewPGSelector("SELECT 1", ""),
		},
	})
	if len(out.ResolvedCommits) != 0 {
		t.Errorf("expected 0 resolved, got %d", len(out.ResolvedCommits))
	}
	if len(out.UnmatchedRuntime) != 1 {
		t.Errorf("expected 1 unmatched, got %d", len(out.UnmatchedRuntime))
	}
}

func TestLinkRuntimeToCommit_TenantScopedMiss(t *testing.T) {
	// Linker is populated for tenant 1. Tenant 2 query should miss
	// (lookupScopedResult falls back to tenant 0, but our entries
	// are tenant 1 only — so a tenant-2 query misses).
	r := newFullRegistry()
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 2), r, LinkRuntimeToCommitInput{
		TenantID: 2,
		Selectors: []RuntimeSelector{
			NewK8sSelector("registry.example.com/order-svc:v1.2.3", "v1.2.3"),
		},
	})
	if len(out.ResolvedCommits) != 0 {
		t.Errorf("expected tenant-2 miss to yield 0 resolved, got %d", len(out.ResolvedCommits))
	}
}

func TestLinkRuntimeToCommit_TenantZeroFallback(t *testing.T) {
	// Add a tenant-0 (global) entry; tenant 7 should still find it.
	r := NewLinkerRegistry()
	l := NewK8sImageLinker()
	l.AddIndex("registry/global:v1", &LinkResult{
		Commit:     "9999999999999999999999999999999999999999",
		FilePath:   "src/global/x.go",
		Author:     "global@opskeeper.io",
		Confidence: 0.8,
		TenantID:   0, // global
	})
	_ = r.Register(l)
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), r, LinkRuntimeToCommitInput{
		TenantID: 7,
		Selectors: []RuntimeSelector{
			NewK8sSelector("registry/global:v1", ""),
		},
	})
	if len(out.ResolvedCommits) != 1 {
		t.Fatalf("expected tenant-7 to fall back to tenant-0 entry, got %d", len(out.ResolvedCommits))
	}
	if out.ResolvedCommits[0].CommitSHA != "9999999999999999999999999999999999999999" {
		t.Errorf("CommitSHA = %q", out.ResolvedCommits[0].CommitSHA)
	}
}

func TestLinkRuntimeToCommit_EmptyInput(t *testing.T) {
	reg := newFullRegistry()
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), reg, LinkRuntimeToCommitInput{TenantID: 1})
	if len(out.ResolvedCommits) != 0 || len(out.UnmatchedRuntime) != 0 {
		t.Errorf("empty input should yield empty result, got %+v", out)
	}
}

func TestLinkRuntimeToCommit_NeedsHumanConfirm(t *testing.T) {
	// Confidence below 0.7 should set NeedsHumanConfirm.
	r := NewLinkerRegistry()
	l := NewK8sImageLinker()
	l.AddIndex("registry/uncertain:v1", &LinkResult{
		Commit:     "5555555555555555555555555555555555555555",
		FilePath:   "src/x.go",
		Author:     "eve@opskeeper.io",
		Confidence: 0.5, // < 0.7 threshold
		TenantID:   1,
	})
	_ = r.Register(l)
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), r, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewK8sSelector("registry/uncertain:v1", ""),
		},
	})
	if len(out.ResolvedCommits) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(out.ResolvedCommits))
	}
	if !out.ResolvedCommits[0].NeedsHumanConfirm {
		t.Error("expected NeedsHumanConfirm=true for confidence=0.5")
	}
}

func TestLinkRuntimeToCommit_OrderPreserved(t *testing.T) {
	reg := newFullRegistry()
	out := LinkRuntimeToCommit(WithTenant(context.Background(), 1), reg, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewK8sSelector("nonexistent-1:v1", ""),                            // unmatched
			NewK8sSelector("registry.example.com/order-svc:v1.2.3", "v1.2.3"), // hit
			NewK8sSelector("nonexistent-2:v1", ""),                            // unmatched
		},
	})
	if len(out.ResolvedCommits) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(out.ResolvedCommits))
	}
	if len(out.UnmatchedRuntime) != 2 {
		t.Fatalf("expected 2 unmatched, got %d", len(out.UnmatchedRuntime))
	}
	// Resolved hit should be the middle input.
	if !strings.Contains(out.ResolvedCommits[0].RuntimeKey, "order-svc") {
		t.Errorf("resolved key = %q, want order-svc", out.ResolvedCommits[0].RuntimeKey)
	}
}

func TestLinkRuntimeToCommit_RespectsCtxCancel(t *testing.T) {
	// Just ensure ctx plumbing doesn't crash; we don't have a slow
	// linker here so the function returns immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reg := newFullRegistry()
	_ = LinkRuntimeToCommit(ctx, reg, LinkRuntimeToCommitInput{
		TenantID: 1,
		Selectors: []RuntimeSelector{
			NewK8sSelector("registry.example.com/order-svc:v1.2.3", "v1.2.3"),
		},
	})
	// Test passes as long as the call doesn't panic with a canceled ctx.
	_ = time.Now() // silence unused import
}

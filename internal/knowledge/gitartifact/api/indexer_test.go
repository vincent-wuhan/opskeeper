package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact"
	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/model"
	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/store"
)

func sampleArtifact(id string) *model.Artifact {
	return &model.Artifact{
		ID:          id,
		PublicID:    id,
		RepoURL:     "https://github.com/x/y",
		Commit:      strings.Repeat("a", 40),
		Branch:      "main",
		ArtifactURL: "s3://x/y",
		Meta:        map[string]interface{}{"build_id": "b-" + id},
		BuildAt:     time.Now(),
		IndexStatus: model.IndexStatusQueued,
	}
}

func setupIndexer(t *testing.T, symbols []model.ExtractedSymbol) (*Indexer, *gitartifact.LinkerRegistry, store.Store) {
	t.Helper()
	reg := gitartifact.NewLinkerRegistry()
	_ = reg.Register(gitartifact.NewPGQueryLinker())
	_ = reg.Register(gitartifact.NewRedisCmdLinker())
	_ = reg.Register(gitartifact.NewK8sImageLinker())
	_ = reg.Register(gitartifact.NewHTTPRouteLinker())
	st := store.NewMemoryStore()
	ext := &mockExtractor{symbols: symbols}
	ix := NewIndexer(IndexerConfig{
		LinkerRegistry: reg,
		Store:          st,
		Extractor:      ext,
		Workers:        2,
	})
	return ix, reg, st
}

type mockExtractor struct {
	symbols []model.ExtractedSymbol
	calls   int
	mu      sync.Mutex
}

func (m *mockExtractor) Extract(ctx context.Context, a *model.Artifact) ([]model.ExtractedSymbol, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.symbols, nil
}

func TestMetaExtractor_NoSymbols(t *testing.T) {
	e := NewMetaExtractor()
	a := sampleArtifact("ga-1")
	syms, err := e.Extract(context.Background(), a)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(syms) != 0 {
		t.Errorf("len = %d, want 0", len(syms))
	}
}

func TestMetaExtractor_ReadsSymbols(t *testing.T) {
	e := NewMetaExtractor()
	a := sampleArtifact("ga-1")
	a.Meta["extracted_symbols"] = []interface{}{
		map[string]interface{}{
			"type":       "pg_query",
			"input":      map[string]interface{}{"query": "SELECT 1"},
			"file_path":  "src/x.go",
			"line_start": float64(10),
			"line_end":   float64(12),
			"confidence": float64(0.9),
		},
	}
	syms, err := e.Extract(context.Background(), a)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(syms) != 1 {
		t.Fatalf("len = %d, want 1", len(syms))
	}
	if syms[0].Type != "pg_query" {
		t.Errorf("Type = %q", syms[0].Type)
	}
	if syms[0].FilePath != "src/x.go" {
		t.Errorf("FilePath = %q", syms[0].FilePath)
	}
	if syms[0].LineStart != 10 {
		t.Errorf("LineStart = %d, want 10", syms[0].LineStart)
	}
}

func TestMetaExtractor_InvalidFormat(t *testing.T) {
	e := NewMetaExtractor()
	a := sampleArtifact("ga-1")
	a.Meta["extracted_symbols"] = "not a list"
	_, err := e.Extract(context.Background(), a)
	if err == nil {
		t.Error("expected error on non-list")
	}
}

func TestMetaExtractor_AutoInjectsCommit(t *testing.T) {
	e := NewMetaExtractor()
	a := sampleArtifact("ga-1")
	a.Meta["extracted_symbols"] = []interface{}{
		map[string]interface{}{
			"type":  "k8s_image",
			"input": map[string]interface{}{"image": "nginx:1.0"},
		},
	}
	syms, _ := e.Extract(context.Background(), a)
	if syms[0].CommitSHA != a.Commit {
		t.Errorf("CommitSHA = %q, want %q (auto-injected)", syms[0].CommitSHA, a.Commit)
	}
}

func TestIndexer_Index_PGQuery(t *testing.T) {
	ix, reg, st := setupIndexer(t, []model.ExtractedSymbol{
		{Type: "pg_query", Input: map[string]interface{}{"query": "SELECT 1"},
			FilePath: "x.go", LineStart: 1, LineEnd: 2, Confidence: 0.95},
	})
	a := sampleArtifact("ga-1")
	st.Put(context.Background(), a)
	if err := ix.Index(context.Background(), "ga-1"); err != nil {
		t.Fatalf("Index: %v", err)
	}
	// 验证 linker 收到
	linker, _ := reg.Get(gitartifact.SymbolTypePGQuery)
	pg := linker.(*gitartifact.PGQueryLinker)
	if len(pg.Index) != 1 {
		t.Errorf("PG index size = %d, want 1", len(pg.Index))
	}
	// 验证 status 更新
	got, _ := st.Get(context.Background(), "ga-1")
	if got.IndexStatus != model.IndexStatusCompleted {
		t.Errorf("IndexStatus = %q, want completed", got.IndexStatus)
	}
}

func TestIndexer_Index_AllSymbolTypes(t *testing.T) {
	ix, reg, st := setupIndexer(t, []model.ExtractedSymbol{
		{Type: "pg_query", Input: map[string]interface{}{"query": "q"}},
		{Type: "redis_cmd", Input: map[string]interface{}{"cmd": "GET", "key": "k"}},
		{Type: "k8s_image", Input: map[string]interface{}{"image": "nginx:1"}},
		{Type: "http_route", Input: map[string]interface{}{"method": "GET", "path": "/x"}},
	})
	st.Put(context.Background(), sampleArtifact("ga-1"))
	if err := ix.Index(context.Background(), "ga-1"); err != nil {
		t.Fatalf("Index: %v", err)
	}
	// 4 个 linker 都有 index
	for _, t2 := range []gitartifact.SymbolType{
		gitartifact.SymbolTypePGQuery, gitartifact.SymbolTypeRedisCmd,
		gitartifact.SymbolTypeK8sImage, gitartifact.SymbolTypeHTTPRoute,
	} {
		linker, _ := reg.Get(t2)
		// 通过类型断言检查 Index 字段非空
		switch l := linker.(type) {
		case *gitartifact.PGQueryLinker:
			if len(l.Index) != 1 {
				t.Errorf("PG index size = %d", len(l.Index))
			}
		case *gitartifact.RedisCmdLinker:
			if len(l.Index) != 1 {
				t.Errorf("Redis index size = %d", len(l.Index))
			}
		case *gitartifact.K8sImageLinker:
			if len(l.Index) != 1 {
				t.Errorf("K8s index size = %d", len(l.Index))
			}
		case *gitartifact.HTTPRouteLinker:
			if len(l.Index) != 1 {
				t.Errorf("HTTP index size = %d", len(l.Index))
			}
		}
	}
}

func TestIndexer_Index_UnknownSymbolType_Skips(t *testing.T) {
	ix, _, st := setupIndexer(t, []model.ExtractedSymbol{
		{Type: "pg_query", Input: map[string]interface{}{"query": "q"}},
		{Type: "unknown_type", Input: map[string]interface{}{}},
	})
	st.Put(context.Background(), sampleArtifact("ga-1"))
	if err := ix.Index(context.Background(), "ga-1"); err != nil {
		t.Fatalf("Index should not fail on unknown type: %v", err)
	}
}

func TestIndexer_Index_NotFound(t *testing.T) {
	ix, _, _ := setupIndexer(t, nil)
	if err := ix.Index(context.Background(), "missing"); err == nil {
		t.Error("expected error for missing artifact")
	}
}

func TestIndexer_Rebuild(t *testing.T) {
	ix, _, st := setupIndexer(t, []model.ExtractedSymbol{
		{Type: "pg_query", Input: map[string]interface{}{"query": "q"}},
	})
	for i := 0; i < 3; i++ {
		a := sampleArtifact("ga-" + string(rune('a'+i)))
		a.BuildAt = time.Now().Add(time.Duration(i) * time.Second)
		st.Put(context.Background(), a)
	}
	rebuilt, failed, errs := ix.Rebuild(context.Background())
	if failed != 0 || len(errs) != 0 {
		t.Errorf("Rebuild errors: failed=%d errs=%v", failed, errs)
	}
	if rebuilt != 3 {
		t.Errorf("rebuilt = %d, want 3", rebuilt)
	}
	// 所有 artifact 状态应是 completed
	list, _ := st.List(context.Background(), store.ListFilter{})
	for _, a := range list {
		if a.IndexStatus != model.IndexStatusCompleted {
			t.Errorf("%s status = %q, want completed", a.PublicID, a.IndexStatus)
		}
	}
}

func TestIndexer_Rebuild_EmptyStore(t *testing.T) {
	ix, _, _ := setupIndexer(t, nil)
	rebuilt, failed, errs := ix.Rebuild(context.Background())
	if rebuilt != 0 || failed != 0 {
		t.Errorf("rebuilt=%d failed=%d, want both 0", rebuilt, failed)
	}
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none", errs)
	}
}

func TestIndexer_Config_Defaults(t *testing.T) {
	reg := gitartifact.NewLinkerRegistry()
	st := store.NewMemoryStore()
	ix := NewIndexer(IndexerConfig{LinkerRegistry: reg, Store: st})
	if ix.workers != 4 {
		t.Errorf("workers = %d, want 4 (default)", ix.workers)
	}
	if ix.extractor == nil {
		t.Error("extractor should default to MetaExtractor")
	}
}

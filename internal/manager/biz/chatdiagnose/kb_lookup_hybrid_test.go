package chatdiagnose

import (
	"context"
	"errors"
	"sync"
	"testing"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

type hybridPatternRepo struct {
	mu              sync.Mutex
	patterns        map[string][]chatdiagnosemodel.IncidentPattern
	vectorResults   map[string][]chatdiagnosemodel.IncidentPattern
	searchTenants   []string
	candidateLimits []int
	candidateErr    error
}

func (r *hybridPatternRepo) FindSimilar(_ context.Context, tenantID string, _ []float64, _ int) ([]chatdiagnosemodel.IncidentPattern, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.vectorResults[tenantID], nil
}

func (r *hybridPatternRepo) SearchCandidates(_ context.Context, tenantID string, _ []string, limit int) ([]chatdiagnosemodel.IncidentPattern, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.searchTenants = append(r.searchTenants, tenantID)
	r.candidateLimits = append(r.candidateLimits, limit)
	if r.candidateErr != nil {
		return nil, r.candidateErr
	}
	return r.patterns[tenantID], nil
}

func (r *hybridPatternRepo) IncHitCount(_ context.Context, _ int64) error {
	return nil
}

func (r *hybridPatternRepo) Save(_ context.Context, _ *chatdiagnosemodel.IncidentPattern) error {
	return nil
}

func (r *hybridPatternRepo) observedSearchTenant() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.searchTenants) == 0 {
		return ""
	}
	return r.searchTenants[len(r.searchTenants)-1]
}

func (r *hybridPatternRepo) observedCandidateLimit() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.candidateLimits) == 0 {
		return 0
	}
	return r.candidateLimits[len(r.candidateLimits)-1]
}

type vectorOnlyRepo struct {
	patterns []chatdiagnosemodel.IncidentPattern
}

func (r *vectorOnlyRepo) FindSimilar(_ context.Context, _ string, _ []float64, _ int) ([]chatdiagnosemodel.IncidentPattern, error) {
	return r.patterns, nil
}

func (r *vectorOnlyRepo) IncHitCount(_ context.Context, _ int64) error {
	return nil
}

func (r *vectorOnlyRepo) Save(_ context.Context, _ *chatdiagnosemodel.IncidentPattern) error {
	return nil
}

type staticEmbedder struct{}

func (staticEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	return []float64{1}, nil
}

func hybridPattern(id int64, tenantID, symptom, signature string) chatdiagnosemodel.IncidentPattern {
	return chatdiagnosemodel.IncidentPattern{
		ID:              id,
		TenantID:        tenantID,
		ResourceType:    "pg",
		Symptom:         symptom,
		RootCauseObject: symptom,
		Signature:       signature,
		Relevance:       0.9,
	}
}

func TestKBLookup_VectorOnly_WhenCandidateSearcherUnavailable(t *testing.T) {
	repo := &vectorOnlyRepo{
		patterns: []chatdiagnosemodel.IncidentPattern{
			hybridPattern(1, "tenant-a", "latency", "postgres primary latency"),
			hybridPattern(2, "tenant-a", "restart", "postgres replica restart"),
		},
	}
	repo.patterns[0].Relevance = 0.95
	repo.patterns[1].Relevance = 0.9
	lookup := NewKBLookup(repo, nil, staticEmbedder{})

	hits, err := lookup.Lookup(context.Background(), KBLookupRequest{
		TenantID:  "tenant-a",
		Signature: "postgres primary latency",
		TopK:      2,
	})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 vector-only hits, got %d", len(hits))
	}
	if hits[0].PatternID != 1 || hits[1].PatternID != 2 {
		t.Fatalf("expected vector ordering [1 2], got [%d %d]", hits[0].PatternID, hits[1].PatternID)
	}
}

func TestKBLookup_LexicalOnly_WhenEmbedderUnavailable(t *testing.T) {
	repo := &hybridPatternRepo{
		patterns: map[string][]chatdiagnosemodel.IncidentPattern{
			"tenant-a": {
				hybridPattern(1, "tenant-a", "long transaction", "postgres long running transaction locks"),
				hybridPattern(2, "tenant-a", "memory", "redis eviction memory pressure"),
				hybridPattern(3, "tenant-a", "short transaction", "postgres short transaction"),
			},
			"tenant-b": {
				hybridPattern(4, "tenant-b", "long transaction", "postgres long transaction"),
			},
		},
	}
	lookup := NewKBLookup(repo, nil, nil)

	hits, err := lookup.Lookup(context.Background(), KBLookupRequest{
		TenantID:  "tenant-a",
		Signature: "postgres long transaction locks",
		TopK:      2,
		Threshold: 0.5,
	})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 lexical-only hits, got %d", len(hits))
	}
	if hits[0].PatternID != 1 {
		t.Fatalf("expected BM25 pattern 1 first, got %d", hits[0].PatternID)
	}
	if repo.observedSearchTenant() != "tenant-a" {
		t.Fatalf("expected tenant-a candidate query, got %q", repo.observedSearchTenant())
	}
	if repo.observedCandidateLimit() != 100 {
		t.Fatalf("expected candidate limit 100, got %d", repo.observedCandidateLimit())
	}
	for _, hit := range hits {
		if hit.PatternID == 4 {
			t.Fatal("cross-tenant pattern leaked into lexical result")
		}
	}
}

func TestKBLookup_Hybrid_FusesDeduplicatesAppliesTopKAndThreshold(t *testing.T) {
	shared := hybridPattern(2, "tenant-a", "connection pool", "postgres connection pool exhausted")
	repo := &hybridPatternRepo{
		vectorResults: map[string][]chatdiagnosemodel.IncidentPattern{
			"tenant-a": {
				hybridPattern(1, "tenant-a", "primary latency", "postgres primary latency"),
				shared,
			},
		},
		patterns: map[string][]chatdiagnosemodel.IncidentPattern{
			"tenant-a": {
				shared,
				hybridPattern(3, "tenant-a", "replica lag", "postgres replica lag"),
			},
		},
	}
	repo.vectorResults["tenant-a"][0].Relevance = 0.8
	lookup := NewKBLookup(repo, nil, staticEmbedder{})

	hits, err := lookup.Lookup(context.Background(), KBLookupRequest{
		TenantID:  "tenant-a",
		Signature: "postgres connection pool exhausted",
		TopK:      2,
		Threshold: 0.5,
	})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 fused hits, got %d", len(hits))
	}
	if hits[0].PatternID != 2 || hits[1].PatternID != 1 {
		t.Fatalf("expected fused ordering [2 1], got [%d %d]", hits[0].PatternID, hits[1].PatternID)
	}
	if hits[0].Similarity != 1 {
		t.Fatalf("expected duplicate pattern to normalize to 1, got %f", hits[0].Similarity)
	}

	hits, err = lookup.Lookup(context.Background(), KBLookupRequest{
		TenantID:  "tenant-a",
		Signature: "postgres connection pool exhausted",
		TopK:      1,
		Threshold: 0.5,
	})
	if err != nil {
		t.Fatalf("topK Lookup returned error: %v", err)
	}
	if len(hits) != 1 || hits[0].PatternID != 2 {
		t.Fatalf("expected topK to return pattern 2, got %+v", hits)
	}

	hits, err = lookup.Lookup(context.Background(), KBLookupRequest{
		TenantID:  "tenant-a",
		Signature: "postgres connection pool exhausted",
		TopK:      2,
		Threshold: 0.9,
	})
	if err != nil {
		t.Fatalf("threshold Lookup returned error: %v", err)
	}
	if len(hits) != 1 || hits[0].PatternID != 2 {
		t.Fatalf("expected threshold to retain only pattern 2, got %+v", hits)
	}
}

func TestKBLookup_RRFOrderIsNotOverriddenBySourceRelevance(t *testing.T) {
	shared := hybridPattern(2, "tenant-a", "connection pool", "postgres pool exhausted")
	shared.Relevance = 0.667
	highVectorOnly := hybridPattern(1, "tenant-a", "primary latency", "postgres primary latency")
	highVectorOnly.Relevance = 0.99
	repo := &hybridPatternRepo{
		vectorResults: map[string][]chatdiagnosemodel.IncidentPattern{
			"tenant-a": {highVectorOnly, shared},
		},
		patterns: map[string][]chatdiagnosemodel.IncidentPattern{
			"tenant-a": {shared},
		},
	}
	lookup := NewKBLookup(repo, nil, staticEmbedder{})

	hits, err := lookup.Lookup(context.Background(), KBLookupRequest{
		TenantID:  "tenant-a",
		Signature: "postgres pool maintenance",
		TopK:      2,
		Threshold: 0.5,
	})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 fused hits, got %d", len(hits))
	}
	if hits[0].PatternID != 2 || hits[1].PatternID != 1 {
		t.Fatalf("expected dual-source pattern before higher-relevance vector-only hit, got [%d %d]", hits[0].PatternID, hits[1].PatternID)
	}
	if hits[0].Similarity >= hits[1].Similarity {
		t.Fatalf("expected Similarity to remain source relevance: %f, %f", hits[0].Similarity, hits[1].Similarity)
	}
}

func TestKBLookup_CandidateFailure_FallsBackToVector(t *testing.T) {
	repo := &hybridPatternRepo{
		vectorResults: map[string][]chatdiagnosemodel.IncidentPattern{
			"tenant-a": {hybridPattern(7, "tenant-a", "latency", "postgres latency")},
		},
		candidateErr: errors.New("candidate store unavailable"),
	}
	lookup := NewKBLookup(repo, nil, staticEmbedder{})

	hits, err := lookup.Lookup(context.Background(), KBLookupRequest{
		TenantID:  "tenant-a",
		Signature: "postgres latency",
	})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if len(hits) != 1 || hits[0].PatternID != 7 {
		t.Fatalf("expected vector fallback hit 7, got %+v", hits)
	}
}

func TestRankBM25_RanksRelevantDocuments(t *testing.T) {
	patterns := []chatdiagnosemodel.IncidentPattern{
		hybridPattern(1, "tenant-a", "memory pressure", "redis memory pressure eviction"),
		hybridPattern(2, "tenant-a", "long transaction", "postgres long transaction lock timeout"),
		hybridPattern(3, "tenant-a", "connection", "postgres connection pool exhausted"),
	}

	got := rankBM25("postgres long transaction timeout", patterns, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 BM25 hits, got %d", len(got))
	}
	if got[0].ID != 2 {
		t.Fatalf("expected pattern 2 first, got %d", got[0].ID)
	}
	if got[1].ID != 3 {
		t.Fatalf("expected pattern 3 second, got %d", got[1].ID)
	}
}

func TestRankBM25_ChineseUsesCharacterBigrams(t *testing.T) {
	got := tokenizeBM25("数据库连接超时！")
	want := []string{"数据", "据库", "库连", "连接", "接超", "超时"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}

	patterns := []chatdiagnosemodel.IncidentPattern{
		hybridPattern(1, "tenant-a", "数据库缓慢", "数据库磁盘空间不足"),
		hybridPattern(2, "tenant-a", "数据库连接超时", "数据库连接池耗尽导致连接超时"),
	}
	ranked := rankBM25("数据库连接超时", patterns, 1)
	if len(ranked) != 1 || ranked[0].ID != 2 {
		t.Fatalf("expected Chinese pattern 2, got %+v", ranked)
	}
	if ranked[0].Relevance != 1 {
		t.Fatalf("expected full Chinese token coverage, got %f", ranked[0].Relevance)
	}
}

func TestKBLookup_RejectsLowLexicalRelevance(t *testing.T) {
	repo := &hybridPatternRepo{
		patterns: map[string][]chatdiagnosemodel.IncidentPattern{
			"tenant-a": {
				hybridPattern(1, "tenant-a", "postgres", "postgres primary latency"),
			},
		},
	}
	lookup := NewKBLookup(repo, nil, nil)

	hits, err := lookup.Lookup(context.Background(), KBLookupRequest{
		TenantID:  "tenant-a",
		Signature: "postgres connection timeout",
		TopK:      2,
		Threshold: 0.9,
	})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected low-relevance hit to be rejected, got %+v", hits)
	}

	hits, err = lookup.Lookup(context.Background(), KBLookupRequest{
		TenantID:  "tenant-a",
		Signature: "postgres connection timeout",
		TopK:      2,
		Threshold: 0.3,
	})
	if err != nil {
		t.Fatalf("thresholded Lookup returned error: %v", err)
	}
	if len(hits) != 1 || hits[0].Similarity != 1.0/3.0 {
		t.Fatalf("expected bounded lexical relevance 1/3, got %+v", hits)
	}
}

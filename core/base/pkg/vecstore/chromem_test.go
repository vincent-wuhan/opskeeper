package vecstore

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/qdrantx"
)

const testCollection = "opskeeper_knowledge"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStore returns an in-memory store with the collection already open,
// which is the state every call site is in by the time it does real work.
func newTestStore(t *testing.T) *Chromem {
	t.Helper()
	c, err := NewChromem("", testLogger())
	if err != nil {
		t.Fatalf("NewChromem: %v", err)
	}
	if err := c.EnsureCollection(context.Background(), testCollection, 2); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	return c
}

func TestSearchRanksByCosine(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)

	// Orthogonal-ish vectors in 2-D so the ordering is unambiguous: id 1
	// points at the query, id 2 partly at it, id 3 away from it.
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{
		{ID: 1, Vector: []float32{1, 0}, Payload: map[string]any{"title": "along"}},
		{ID: 2, Vector: []float32{0.6, 0.8}, Payload: map[string]any{"title": "near"}},
		{ID: 3, Vector: []float32{-1, 0}, Payload: map[string]any{"title": "against"}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := c.Search(ctx, testCollection, []float32{1, 0}, qdrantx.SearchOpts{Limit: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	for i, want := range []uint64{1, 2, 3} {
		if hits[i].ID != want {
			t.Fatalf("hit %d is point %d, want %d (scores %v)", i, hits[i].ID, want, scores(hits))
		}
	}
	if hits[0].Score <= hits[1].Score || hits[1].Score <= hits[2].Score {
		t.Fatalf("scores are not descending: %v", scores(hits))
	}
	// The query is identical to point 1's vector, so cosine is 1 — the same
	// value qdrant would have returned, which is why no conversion happens.
	if hits[0].Score < 0.99 {
		t.Fatalf("identical vector scored %v, want ~1", hits[0].Score)
	}
	if got := hits[0].Payload["title"]; got != "along" {
		t.Fatalf("top hit payload title = %v, want \"along\"", got)
	}
}

func TestSearchLimitTruncates(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)

	points := make([]qdrantx.Point, 0, 5)
	for i := 1; i <= 5; i++ {
		// Similarity falls off as the second component grows, so ids 1 and 2
		// are the top two.
		points = append(points, qdrantx.Point{ID: uint64(i), Vector: []float32{1, float32(i - 1)}})
	}
	if err := c.Upsert(ctx, testCollection, points); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := c.Search(ctx, testCollection, []float32{1, 0}, qdrantx.SearchOpts{Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 || hits[0].ID != 1 || hits[1].ID != 2 {
		t.Fatalf("top 2 = %v, want points 1 and 2", ids(hits))
	}
}

func TestSearchAppliesMustMatch(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)

	// The unfiltered winner is deliberately in another tenant, so a backend
	// that filtered only after ignoring the predicate would return it.
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{
		{ID: 1, Vector: []float32{1, 0}, Payload: map[string]any{"tenant_id": "other"}},
		{ID: 2, Vector: []float32{0.9, 0.1}, Payload: map[string]any{"tenant_id": "t1"}},
		{ID: 3, Vector: []float32{0.8, 0.2}, Payload: map[string]any{"tenant_id": "t1"}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := c.Search(ctx, testCollection, []float32{1, 0}, qdrantx.SearchOpts{
		Limit:     10,
		MustMatch: map[string]any{"tenant_id": "t1"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits %v, want 2", len(hits), ids(hits))
	}
	for _, h := range hits {
		if h.Payload["tenant_id"] != "t1" {
			t.Fatalf("hit %d leaked tenant %v", h.ID, h.Payload["tenant_id"])
		}
	}
}

func TestSearchOnEmptyCollection(t *testing.T) {
	c := newTestStore(t)
	hits, err := c.Search(context.Background(), testCollection, []float32{1, 0}, qdrantx.SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("Search on empty collection: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("got %d hits, want none", len(hits))
	}
}

func TestSearchOnUnknownCollectionErrors(t *testing.T) {
	c := newTestStore(t)
	if _, err := c.Search(context.Background(), "nope", []float32{1, 0}, qdrantx.SearchOpts{Limit: 5}); err == nil {
		t.Fatal("expected an error searching a collection that was never ensured")
	}
}

func TestUpsertReplacesByID(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)

	first := []qdrantx.Point{{ID: 1, Vector: []float32{1, 0}, Payload: map[string]any{"title": "old"}}}
	second := []qdrantx.Point{{ID: 1, Vector: []float32{0, 1}, Payload: map[string]any{"title": "new"}}}
	if err := c.Upsert(ctx, testCollection, first); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := c.Upsert(ctx, testCollection, second); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := c.GetPoints(ctx, testCollection, []uint64{1})
	if err != nil {
		t.Fatalf("GetPoints: %v", err)
	}
	if len(got) != 1 || got[0].Payload["title"] != "new" {
		t.Fatalf("payload after replace = %v, want title \"new\"", got)
	}
	// The replacement moved the vector too, so a query along the old axis
	// no longer finds it.
	hits, err := c.Search(ctx, testCollection, []float32{1, 0}, qdrantx.SearchOpts{Limit: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Score > 0.1 {
		t.Fatalf("replaced vector still scores %v along the old axis", hits)
	}
}

func TestUpsertRejectsPayloadOnlyPoint(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	// qdrant would accept this; chromem has no document without a vector.
	err := c.Upsert(ctx, testCollection, []qdrantx.Point{{ID: 1, Payload: map[string]any{"a": 1}}})
	if err == nil {
		t.Fatal("expected an error for a point with no vector")
	}
	got, err := c.GetPoints(ctx, testCollection, []uint64{1})
	if err != nil {
		t.Fatalf("GetPoints: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the rejected point was stored anyway: %v", got)
	}
}

// TestPayloadsReadBackAsJSONTypes pins the port's payload contract. Callers
// type-assert against the shapes the HTTP qdrant client produces by decoding
// JSON, so the embedded backend must hand those back too — not the Go types
// the caller happened to pass in. Getting this wrong is silent: the
// assertion yields a zero value and the doc reads as having no tenant.
func TestPayloadsReadBackAsJSONTypes(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)

	original := map[string]any{
		"tenant_scopes": []string{"tenant:7"},
		"tags":          []string{"a", "b"},
		"doc_id":        uint64(42),
		"title":         "t",
	}
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{{ID: 1, Vector: []float32{1, 0}, Payload: original}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := c.GetPoints(ctx, testCollection, []uint64{1})
	if err != nil {
		t.Fatalf("GetPoints: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1", len(got))
	}
	p := got[0].Payload
	if _, ok := p["tenant_scopes"].([]any); !ok {
		t.Fatalf("tenant_scopes is %T, want []any", p["tenant_scopes"])
	}
	if _, ok := p["tags"].([]any); !ok {
		t.Fatalf("tags is %T, want []any", p["tags"])
	}
	if _, ok := p["doc_id"].(float64); !ok {
		t.Fatalf("doc_id is %T, want float64", p["doc_id"])
	}
	if p["title"] != "t" {
		t.Fatalf("title = %v, want \"t\"", p["title"])
	}
	// The caller's own map must be untouched by any of this.
	if scopes, ok := original["tenant_scopes"].([]string); !ok || len(scopes) != 1 || scopes[0] != "tenant:7" {
		t.Fatalf("the caller's payload was mutated: %#v", original)
	}
}

func TestSetPayloadByFilterKeepsJSONTypes(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{{
		ID:      1,
		Vector:  []float32{1, 0},
		Payload: map[string]any{"tenant_scopes": []string{"tenant:7"}},
	}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// A merged-in []string would reintroduce the wrong shape if the merge
	// skipped normalization.
	if err := c.SetPayloadByFilter(ctx, testCollection,
		map[string]any{"tags": []string{"new"}},
		map[string]any{"tenant_scopes": []string{"tenant:7"}},
	); err != nil {
		t.Fatalf("SetPayloadByFilter: %v", err)
	}
	got, err := c.GetPoints(ctx, testCollection, []uint64{1})
	if err != nil {
		t.Fatalf("GetPoints: %v", err)
	}
	if _, ok := got[0].Payload["tags"].([]any); !ok {
		t.Fatalf("tags is %T after merge, want []any", got[0].Payload["tags"])
	}
	if _, ok := got[0].Payload["tenant_scopes"].([]any); !ok {
		t.Fatalf("tenant_scopes is %T after merge, want []any", got[0].Payload["tenant_scopes"])
	}
}

func TestGetPointsPreservesOrderAndSkipsMissing(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{
		{ID: 10, Vector: []float32{1, 0}, Payload: map[string]any{"n": 10}},
		{ID: 20, Vector: []float32{0, 1}, Payload: map[string]any{"n": 20}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := c.GetPoints(ctx, testCollection, []uint64{20, 99, 10})
	if err != nil {
		t.Fatalf("GetPoints: %v", err)
	}
	if len(got) != 2 || got[0].ID != 20 || got[1].ID != 10 {
		t.Fatalf("got ids %v, want [20 10]", ids(got))
	}
}

func TestDeleteByID(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{
		{ID: 1, Vector: []float32{1, 0}},
		{ID: 2, Vector: []float32{0, 1}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := c.DeleteByID(ctx, testCollection, 1); err != nil {
		t.Fatalf("DeleteByID: %v", err)
	}
	hits, err := c.Search(ctx, testCollection, []float32{1, 0}, qdrantx.SearchOpts{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != 2 {
		t.Fatalf("after delete got %v, want only point 2", ids(hits))
	}
}

func TestDeleteByFilter(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{
		{ID: 1, Vector: []float32{1, 0}, Payload: map[string]any{"doc_id": "a"}},
		{ID: 2, Vector: []float32{0.9, 0.1}, Payload: map[string]any{"doc_id": "b"}},
		{ID: 3, Vector: []float32{0.8, 0.2}, Payload: map[string]any{"doc_id": "c"}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := c.DeleteByFilter(ctx, testCollection, map[string]any{"doc_id": "b"}); err != nil {
		t.Fatalf("DeleteByFilter: %v", err)
	}
	got, err := c.GetPoints(ctx, testCollection, []uint64{1, 2, 3})
	if err != nil {
		t.Fatalf("GetPoints: %v", err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Fatalf("after delete got %v, want points 1 and 3", ids(got))
	}
	// chromem is the source of truth, so the vector must be gone from it too
	// — not just from the in-memory index.
	hits, err := c.Search(ctx, testCollection, []float32{1, 0}, qdrantx.SearchOpts{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("search still returns %d points, want 2", len(hits))
	}
}

func TestDeleteByFilterRefusesEmptyPredicate(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{{ID: 1, Vector: []float32{1, 0}}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := c.DeleteByFilter(ctx, testCollection, nil); err == nil {
		t.Fatal("expected an error for an empty predicate; it matches everything")
	}
	got, err := c.GetPoints(ctx, testCollection, []uint64{1})
	if err != nil {
		t.Fatalf("GetPoints: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("the point was deleted despite the refused call")
	}
}

func TestScrollPaginates(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	points := make([]qdrantx.Point, 0, 5)
	for i := 1; i <= 5; i++ {
		points = append(points, qdrantx.Point{ID: uint64(i), Vector: []float32{1, 0}, Payload: map[string]any{"n": i}})
	}
	if err := c.Upsert(ctx, testCollection, points); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	var seen []uint64
	var cursor *uint64
	for page := 0; page < 10; page++ {
		res, err := c.Scroll(ctx, testCollection, qdrantx.ScrollOpts{Limit: 2, Offset: cursor})
		if err != nil {
			t.Fatalf("Scroll: %v", err)
		}
		for _, p := range res.Points {
			seen = append(seen, p.ID)
		}
		if res.NextOffset == nil {
			break
		}
		cursor = res.NextOffset
	}

	want := []uint64{1, 2, 3, 4, 5}
	if len(seen) != len(want) {
		t.Fatalf("paged over %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paged over %v, want %v", seen, want)
		}
	}
}

func TestScrollFiltersByPredicate(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{
		{ID: 1, Vector: []float32{1, 0}, Payload: map[string]any{"tenant_id": "t1"}},
		{ID: 2, Vector: []float32{1, 0}, Payload: map[string]any{"tenant_id": "t2"}},
		{ID: 3, Vector: []float32{1, 0}, Payload: map[string]any{"tenant_id": "t1"}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	res, err := c.Scroll(ctx, testCollection, qdrantx.ScrollOpts{
		Limit:     10,
		MustMatch: map[string]any{"tenant_id": "t1"},
	})
	if err != nil {
		t.Fatalf("Scroll: %v", err)
	}
	if got := ids(res.Points); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("scrolled %v, want points 1 and 3", got)
	}
	// The predicate is applied before paging, so a page that returned
	// every match must not hand back a cursor.
	if res.NextOffset != nil {
		t.Fatalf("NextOffset = %v although both matches fit in one page", *res.NextOffset)
	}
}

func TestSetPayloadByFilter(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	caller := map[string]any{"tenant_id": "t1", "title": "before"}
	if err := c.Upsert(ctx, testCollection, []qdrantx.Point{
		{ID: 1, Vector: []float32{1, 0}, Payload: caller},
		{ID: 2, Vector: []float32{0, 1}, Payload: map[string]any{"tenant_id": "t2", "title": "untouched"}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := c.SetPayloadByFilter(ctx, testCollection,
		map[string]any{"title": "after"},
		map[string]any{"tenant_id": "t1"},
	); err != nil {
		t.Fatalf("SetPayloadByFilter: %v", err)
	}

	// The merge must not write through to the caller's map.
	if caller["title"] != "before" {
		t.Fatalf("the caller's payload was mutated to %v", caller["title"])
	}

	hits, err := c.Search(ctx, testCollection, []float32{1, 0}, qdrantx.SearchOpts{Limit: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Payload["title"] != "after" {
		t.Fatalf("search saw %v, want the merged title — chromem metadata was not rewritten", hits)
	}

	res, err := c.Scroll(ctx, testCollection, qdrantx.ScrollOpts{Limit: 10})
	if err != nil {
		t.Fatalf("Scroll: %v", err)
	}
	for _, p := range res.Points {
		if p.ID == 2 && p.Payload["title"] != "untouched" {
			t.Fatalf("point 2 was rewritten: %v", p.Payload)
		}
	}
}

func TestSetPayloadByFilterRequiresBothArguments(t *testing.T) {
	ctx := context.Background()
	c := newTestStore(t)
	if err := c.SetPayloadByFilter(ctx, testCollection, nil, map[string]any{"tenant_id": "t1"}); err == nil {
		t.Fatal("expected an error with no payload")
	}
	if err := c.SetPayloadByFilter(ctx, testCollection, map[string]any{"a": 1}, nil); err == nil {
		t.Fatal("expected an error with no match clause")
	}
}

// TestRebuildsIndexFromDisk is the restart path: chromem is the only durable
// store, so a fresh process must recover its payload index from it and still
// filter and paginate correctly.
func TestRebuildsIndexFromDisk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first, err := NewChromem(dir, testLogger())
	if err != nil {
		t.Fatalf("NewChromem: %v", err)
	}
	if err := first.EnsureCollection(ctx, testCollection, 2); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if err := first.Upsert(ctx, testCollection, []qdrantx.Point{
		{ID: 1, Vector: []float32{1, 0}, Payload: map[string]any{"tenant_id": "t1", "title": "one"}},
		{ID: 2, Vector: []float32{0, 1}, Payload: map[string]any{"tenant_id": "t2", "title": "two"}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	second, err := NewChromem(dir, testLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := second.EnsureCollection(ctx, testCollection, 2); err != nil {
		t.Fatalf("EnsureCollection on reopen: %v", err)
	}

	res, err := second.Scroll(ctx, testCollection, qdrantx.ScrollOpts{Limit: 10})
	if err != nil {
		t.Fatalf("Scroll after reopen: %v", err)
	}
	if got := ids(res.Points); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("after reopen scrolled %v, want points 1 and 2", got)
	}
	if got := res.Points[0].Payload["title"]; got != "one" {
		t.Fatalf("payload after reopen = %v, want \"one\"", got)
	}

	hits, err := second.Search(ctx, testCollection, []float32{1, 0}, qdrantx.SearchOpts{
		Limit:     10,
		MustMatch: map[string]any{"tenant_id": "t2"},
	})
	if err != nil {
		t.Fatalf("Search after reopen: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != 2 {
		t.Fatalf("filtered search after reopen returned %v, want point 2", ids(hits))
	}
}

func scores(hits []qdrantx.SearchHit) []float64 {
	out := make([]float64, len(hits))
	for i, h := range hits {
		out[i] = h.Score
	}
	return out
}

func ids(hits []qdrantx.SearchHit) []uint64 {
	out := make([]uint64, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

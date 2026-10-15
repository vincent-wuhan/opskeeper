// Package vecstore hosts OpsKeeper's embeddable vector backends: engines
// that need no external service, so a fresh checkout can run the knowledge
// base from a single binary. They implement qdrantx.Store — the same port
// the HTTP qdrant client serves — so callers hold one interface and never
// branch on which backend is wired.
package vecstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"

	"github.com/philippgille/chromem-go"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/qdrantx"
)

const (
	// metaPayload carries the caller's whole payload as JSON. chromem's own
	// metadata is map[string]string and its filter is equality-only, so the
	// payload rides along whole rather than being shredded into fields.
	metaPayload = "_payload"

	// metaAll tags every document, which is how the boot-time rebuild finds
	// all of them again in one query.
	metaAll   = "_all"
	metaAllOn = "1"

	// A filtered search cannot narrow candidates server-side, so it pulls
	// limit*filteredSearchFactor points and applies the predicate here.
	// Both the factor and the cap are a deliberate ceiling: a selective
	// filter over a large collection can return fewer than limit hits, and
	// an embedded single-node store is sized so this stays cheap.
	filteredSearchFactor = 20
	filteredSearchCap    = 2000
)

// Chromem is the embedded vector backend. It keeps two things in step:
//
//   - chromem itself, the vector engine, holding every embedding plus the
//     payload JSON in metadata. It is the source of truth and is what
//     survives a restart.
//   - idx, an in-memory payload index rebuilt from chromem when a
//     collection is first ensured. It exists because Scroll, GetPoints and
//     the delete/set-payload operations need predicate matching and ID
//     lookup, and chromem offers neither without a scan.
//
// Writes go to both; chromem is only ever read back to rebuild idx, never
// kept in sync by hand.
type Chromem struct {
	db  *chromem.DB
	log *slog.Logger

	mu    sync.RWMutex
	idx   map[string]map[uint64]map[string]any // collection -> point ID -> payload
	built map[string]struct{}
}

// NewChromem opens the embedded engine. persistDir is the directory a
// persistent store lives in; "" or ":memory:" keeps everything in RAM.
func NewChromem(persistDir string, log *slog.Logger) (*Chromem, error) {
	if log == nil {
		log = slog.Default()
	}
	var (
		db  *chromem.DB
		err error
	)
	switch persistDir {
	case "", ":memory:":
		db = chromem.NewDB()
	default:
		db, err = chromem.NewPersistentDB(persistDir, true)
		if err != nil {
			return nil, fmt.Errorf("vecstore: open chromem store at %q: %w", persistDir, err)
		}
	}
	return &Chromem{
		db:    db,
		log:   log,
		idx:   make(map[string]map[uint64]map[string]any),
		built: make(map[string]struct{}),
	}, nil
}

// noEmbed stands in for chromem's embedding function. OpsKeeper always
// supplies precomputed vectors, so this should never run; returning an
// error beats a nil func panicking on an accidental text query.
func noEmbed(context.Context, string) ([]float32, error) {
	return nil, errors.New("vecstore: chromem collection has no embedding function; OpsKeeper supplies precomputed vectors")
}

// EnsureCollection opens the collection and, the first time, reads every
// document back to rebuild the payload index.
func (c *Chromem) EnsureCollection(ctx context.Context, name string, dim int) error {
	col, err := c.db.GetOrCreateCollection(name, nil, noEmbed)
	if err != nil {
		return fmt.Errorf("vecstore: open collection %s: %w", name, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.built[name]; ok {
		return nil
	}
	idx, err := c.readBack(ctx, col, name, dim)
	if err != nil {
		return err
	}
	c.idx[name] = idx
	c.built[name] = struct{}{}
	c.log.Info("vecstore: collection ready", "collection", name, "points", len(idx), "dim", dim)
	return nil
}

// readBack repopulates the payload index from chromem. chromem has no
// list-all operation and no metadata query beyond string equality, so this
// walks the collection by asking for every point against a unit probe
// vector; the similarity scores that come back are meaningless here and
// get discarded. It runs once per collection per process, at boot.
func (c *Chromem) readBack(ctx context.Context, col *chromem.Collection, name string, dim int) (map[uint64]map[string]any, error) {
	idx := make(map[uint64]map[string]any)
	count := col.Count()
	if count == 0 {
		return idx, nil
	}
	if dim <= 0 {
		return nil, fmt.Errorf("vecstore: collection %s holds %d points but dim is %d; refusing to rebuild the index", name, count, dim)
	}
	// A unit probe, so chromem neither rescales it nor divides by zero.
	probe := make([]float32, dim)
	probe[0] = 1

	results, err := col.QueryEmbedding(ctx, probe, count, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("vecstore: read back collection %s: %w", name, err)
	}
	for _, r := range results {
		id, payload, err := decode(r)
		if err != nil {
			return nil, fmt.Errorf("vecstore: collection %s: %w", name, err)
		}
		idx[id] = payload
	}
	return idx, nil
}

// EnsurePayloadIndex is a no-op: chromem has no payload indexes, and
// filtering happens in Go. The declaration is on the port because the
// knowledge usecase issues these at boot and expects them to succeed.
func (c *Chromem) EnsurePayloadIndex(context.Context, string, string, string) error {
	return nil
}

// Upsert writes points, replacing any with the same ID.
func (c *Chromem) Upsert(ctx context.Context, collection string, points []qdrantx.Point) error {
	if len(points) == 0 {
		return nil
	}
	col, err := c.collection(collection)
	if err != nil {
		return err
	}

	ids := make([]string, len(points))
	embeddings := make([][]float32, len(points))
	metadatas := make([]map[string]string, len(points))
	normalized := make([]map[string]any, len(points))
	for i, p := range points {
		// qdrant stores a point carrying only a payload; chromem has no such
		// document, and letting it through fails deep inside Add with a
		// message about content. Every OpsKeeper upsert is vectorized, so
		// say so here rather than there.
		if len(p.Vector) == 0 {
			return fmt.Errorf("vecstore: point %d has no vector; the embedded store cannot hold a payload-only point", p.ID)
		}
		blob, norm, err := encodePayload(p.Payload)
		if err != nil {
			return fmt.Errorf("vecstore: encode payload for point %d: %w", p.ID, err)
		}
		ids[i] = encodeID(p.ID)
		embeddings[i] = p.Vector
		metadatas[i] = metadata(blob)
		normalized[i] = norm
	}
	if err := col.Add(ctx, ids, embeddings, metadatas, nil); err != nil {
		return fmt.Errorf("vecstore: upsert into %s: %w", collection, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.index(collection)
	for i, p := range points {
		idx[p.ID] = normalized[i]
	}
	return nil
}

// Search ranks by cosine similarity. chromem scores on normalized vectors,
// so its Similarity is the cosine of the same vectors qdrant would have
// scored and needs no conversion to become a SearchHit.Score.
func (c *Chromem) Search(ctx context.Context, collection string, vector []float32, opts qdrantx.SearchOpts) ([]qdrantx.SearchHit, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(vector) == 0 {
		return nil, errors.New("vecstore: search requires a query vector")
	}
	col, err := c.collection(collection)
	if err != nil {
		return nil, err
	}
	count := col.Count()
	if count == 0 {
		return nil, nil
	}

	n := limit
	if len(opts.MustMatch) > 0 {
		n = limit * filteredSearchFactor
		if n > filteredSearchCap {
			n = filteredSearchCap
		}
	}
	if n > count {
		n = count
	}

	results, err := col.QueryEmbedding(ctx, vector, n, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("vecstore: search in %s: %w", collection, err)
	}

	// chromem returns most-similar first, so truncating preserves the top-k.
	hits := make([]qdrantx.SearchHit, 0, len(results))
	for _, r := range results {
		id, payload, err := decode(r)
		if err != nil {
			return nil, fmt.Errorf("vecstore: collection %s: %w", collection, err)
		}
		if len(opts.MustMatch) > 0 && !qdrantx.MatchPayload(payload, opts.MustMatch) {
			continue
		}
		hits = append(hits, qdrantx.SearchHit{ID: id, Score: float64(r.Similarity), Payload: payload})
		if len(hits) == limit {
			break
		}
	}
	return hits, nil
}

// Scroll lists points by predicate. The offset is an opaque cursor to
// callers; here it is the last ID of the previous page, so paging is stable
// as long as no point below it is inserted mid-scroll.
func (c *Chromem) Scroll(_ context.Context, collection string, opts qdrantx.ScrollOpts) (*qdrantx.ScrollResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	idx := c.idx[collection]
	ids := make([]uint64, 0, len(idx))
	for id, payload := range idx {
		if opts.Offset != nil && id <= *opts.Offset {
			continue
		}
		if opts.MustMatch != nil && !qdrantx.MatchPayload(payload, opts.MustMatch) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	points := make([]qdrantx.SearchHit, 0, limit)
	for _, id := range ids {
		if len(points) == limit {
			break
		}
		points = append(points, qdrantx.SearchHit{ID: id, Payload: idx[id]})
	}

	res := &qdrantx.ScrollResult{Points: points}
	if len(points) > 0 && len(points) < len(ids) {
		next := points[len(points)-1].ID
		res.NextOffset = &next
	}
	return res, nil
}

// GetPoints fetches payloads by ID, in the order asked. Missing IDs are
// skipped, as qdrant does.
func (c *Chromem) GetPoints(_ context.Context, collection string, ids []uint64) ([]qdrantx.SearchHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	idx := c.idx[collection]
	hits := make([]qdrantx.SearchHit, 0, len(ids))
	for _, id := range ids {
		if payload, ok := idx[id]; ok {
			hits = append(hits, qdrantx.SearchHit{ID: id, Payload: payload})
		}
	}
	return hits, nil
}

// DeleteByFilter drops every point matching the predicate.
func (c *Chromem) DeleteByFilter(ctx context.Context, collection string, must map[string]any) error {
	// An empty predicate matches everything, and silently emptying a
	// collection is never what a caller meant. qdrant refuses this too.
	if len(must) == 0 {
		return errors.New("vecstore: DeleteByFilter requires at least one match clause (refusing to delete all)")
	}
	col, err := c.collection(collection)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.idx[collection]

	var ids []string
	for id, payload := range idx {
		if qdrantx.MatchPayload(payload, must) {
			ids = append(ids, encodeID(id))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if err := col.Delete(ctx, nil, nil, ids...); err != nil {
		return fmt.Errorf("vecstore: delete from %s: %w", collection, err)
	}
	for _, id := range ids {
		if parsed, err := strconv.ParseUint(id, 10, 64); err == nil {
			delete(idx, parsed)
		}
	}
	return nil
}

// DeleteByID drops one point.
func (c *Chromem) DeleteByID(ctx context.Context, collection string, id uint64) error {
	col, err := c.collection(collection)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := col.Delete(ctx, nil, nil, encodeID(id)); err != nil {
		return fmt.Errorf("vecstore: delete point %d from %s: %w", id, collection, err)
	}
	delete(c.idx[collection], id)
	return nil
}

// SetPayloadByFilter merges payload into every matching point without
// re-embedding them.
func (c *Chromem) SetPayloadByFilter(ctx context.Context, collection string, payload, must map[string]any) error {
	if len(payload) == 0 {
		return errors.New("vecstore: SetPayloadByFilter requires payload")
	}
	if len(must) == 0 {
		return errors.New("vecstore: SetPayloadByFilter requires at least one match clause")
	}
	col, err := c.collection(collection)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.index(collection)

	var touched []uint64
	for id, have := range idx {
		if !qdrantx.MatchPayload(have, must) {
			continue
		}
		// Copy rather than merge in place: the indexed payload may still
		// be shared with a caller, and SetPayloadByFilter must not mutate it.
		merged := make(map[string]any, len(have)+len(payload))
		for k, v := range have {
			merged[k] = v
		}
		for k, v := range payload {
			merged[k] = v
		}
		// Re-normalize: the merge may have introduced caller-typed values,
		// and the index must stay in the shape reads expect.
		_, norm, err := encodePayload(merged)
		if err != nil {
			return fmt.Errorf("vecstore: encode merged payload for point %d: %w", id, err)
		}
		idx[id] = norm
		touched = append(touched, id)
	}
	if len(touched) == 0 {
		return nil
	}

	// chromem has no partial update, so rewrite each affected document to
	// keep its stored metadata equal to the index.
	for _, id := range touched {
		doc, err := col.GetByID(ctx, encodeID(id))
		if err != nil {
			return fmt.Errorf("vecstore: read point %d from %s: %w", id, collection, err)
		}
		blob, err := json.Marshal(idx[id])
		if err != nil {
			return fmt.Errorf("vecstore: encode payload for point %d: %w", id, err)
		}
		err = col.Add(ctx,
			[]string{doc.ID},
			[][]float32{doc.Embedding},
			[]map[string]string{metadata(blob)},
			nil,
		)
		if err != nil {
			return fmt.Errorf("vecstore: rewrite point %d in %s: %w", id, collection, err)
		}
	}
	return nil
}

// collection resolves an already-created collection.
func (c *Chromem) collection(name string) (*chromem.Collection, error) {
	col := c.db.GetCollection(name, noEmbed)
	if col == nil {
		return nil, fmt.Errorf("vecstore: collection %q is not open (call EnsureCollection first)", name)
	}
	return col, nil
}

// index returns the mutable payload index for a collection, creating it on
// demand. Callers must hold c.mu.
func (c *Chromem) index(name string) map[uint64]map[string]any {
	idx, ok := c.idx[name]
	if !ok {
		idx = make(map[uint64]map[string]any)
		c.idx[name] = idx
	}
	return idx
}

// decode turns a chromem result back into an OpsKeeper point.
func decode(r chromem.Result) (uint64, map[string]any, error) {
	id, err := strconv.ParseUint(r.ID, 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("point id %q is not numeric: %w", r.ID, err)
	}
	blob, ok := r.Metadata[metaPayload]
	if !ok {
		return 0, nil, fmt.Errorf("point %d carries no %s metadata", id, metaPayload)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(blob), &payload); err != nil {
		return 0, nil, fmt.Errorf("decode payload of point %d: %w", id, err)
	}
	return id, payload, nil
}

func metadata(payloadJSON []byte) map[string]string {
	return map[string]string{
		metaAll:     metaAllOn,
		metaPayload: string(payloadJSON),
	}
}

// encodePayload renders a caller's payload to the JSON chromem stores, and
// back into a map in the shape that JSON produces.
//
// The second return value is the point. Callers read payloads back through
// type assertions — `p["tenant_scopes"].([]any)` and friends — that only
// hold because the HTTP qdrant client decodes JSON. Handing back the Go
// types the caller passed in would leave every array as []string and every
// number as int64, and the assertions would silently yield zero values: a
// doc would read as having no tenant scopes and become invisible. The index
// therefore holds the normalized copy, not the caller's map.
func encodePayload(payload map[string]any) ([]byte, map[string]any, error) {
	blob, err := json.Marshal(payloadOrEmpty(payload))
	if err != nil {
		return nil, nil, err
	}
	var norm map[string]any
	if err := json.Unmarshal(blob, &norm); err != nil {
		return nil, nil, err
	}
	return blob, norm, nil
}

func encodeID(id uint64) string { return strconv.FormatUint(id, 10) }

func payloadOrEmpty(p map[string]any) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	return p
}

// Compile-time proof that the embedded backend serves the same port as the
// qdrant HTTP client, including the optional payload migration.
var (
	_ qdrantx.Store                  = (*Chromem)(nil)
	_ interface {
		SetPayloadByFilter(context.Context, string, map[string]any, map[string]any) error
	} = (*Chromem)(nil)
)
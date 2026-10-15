package qdrantx

import "context"

// Store is the vector-store port: the narrow surface the knowledge and
// chatdiagnose layers need from a backend. Two implementations exist and
// callers never branch on which one they hold — *Client for a qdrant
// reached over HTTP, and vecstore.Chromem for the embedded engine. The
// methods, the DTOs and the filter semantics are identical for both.
type Store interface {
	EnsureCollection(ctx context.Context, name string, dim int) error
	EnsurePayloadIndex(ctx context.Context, collection, field, schema string) error
	Upsert(ctx context.Context, collection string, points []Point) error
	DeleteByFilter(ctx context.Context, collection string, mustMatch map[string]any) error
	DeleteByID(ctx context.Context, collection string, id uint64) error
	GetPoints(ctx context.Context, collection string, ids []uint64) ([]SearchHit, error)
	Search(ctx context.Context, collection string, vector []float32, opts SearchOpts) ([]SearchHit, error)
	Scroll(ctx context.Context, collection string, opts ScrollOpts) (*ScrollResult, error)
}

// The HTTP client is the port's reference implementation; keep it honest.
var _ Store = (*Client)(nil)
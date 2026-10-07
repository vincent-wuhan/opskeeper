package host

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// fakes_test.go is the test double the host cluster shares internally.
//
// It is a copy, not a move: the same fake still lives in the tools package
// for the tests that stayed behind. A test double is not a contract
// between two packages, it is scaffolding inside one, and two copies
// cannot drift into anything that matters because neither is reachable
// from production code.
type fakeCaller struct {
	mu       sync.Mutex
	lastID   uint64
	lastName string
	lastBody []byte
	respBody []byte
	respErr  error
}

func (f *fakeCaller) Call(_ context.Context, edgeID uint64, method string, body []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID = edgeID
	f.lastName = method
	f.lastBody = append([]byte(nil), body...)
	if f.respErr != nil {
		return nil, f.respErr
	}
	return f.respBody, nil
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// fakeEdgeRepo is an in-memory edge.Repo. The tools package keeps a
// copy; the host cluster needs its own because a test double is
// scaffolding inside a package, not a contract between them.
type fakeEdgeRepo struct {
	byID   map[uint64]*edgemodel.Edge
	byName map[string]*edgemodel.Edge
}

func newFakeEdgeRepo(edges ...*edgemodel.Edge) *fakeEdgeRepo {
	r := &fakeEdgeRepo{
		byID:   map[uint64]*edgemodel.Edge{},
		byName: map[string]*edgemodel.Edge{},
	}
	for _, e := range edges {
		r.byID[e.ID] = e
		r.byName[e.Name] = e
	}
	return r
}

func (r *fakeEdgeRepo) Create(_ context.Context, _ *edgemodel.Edge) error { return nil }
func (r *fakeEdgeRepo) GetByID(_ context.Context, id uint64) (*edgemodel.Edge, error) {
	if e, ok := r.byID[id]; ok {
		return e, nil
	}
	return nil, errs.ErrNotFound
}
func (r *fakeEdgeRepo) GetByAccessKey(_ context.Context, _ string) (*edgemodel.Edge, error) {
	return nil, errs.ErrNotFound
}
func (r *fakeEdgeRepo) GetByName(_ context.Context, name string) (*edgemodel.Edge, error) {
	if e, ok := r.byName[name]; ok {
		return e, nil
	}
	return nil, errs.ErrNotFound
}
func (r *fakeEdgeRepo) List(_ context.Context, _ edgebiz.ListFilter) ([]*edgemodel.Edge, error) {
	out := make([]*edgemodel.Edge, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, e)
	}
	return out, nil
}
func (r *fakeEdgeRepo) UpdateSecretHash(_ context.Context, _ uint64, _ string) error { return nil }
func (r *fakeEdgeRepo) UpdateStatus(_ context.Context, _ uint64, _ string, _ time.Time) error {
	return nil
}
func (r *fakeEdgeRepo) UpdateRoles(_ context.Context, _ uint64, _ uint8) error      { return nil }
func (r *fakeEdgeRepo) UpdateName(_ context.Context, _ uint64, _ string) error      { return nil }
func (r *fakeEdgeRepo) SetDeviceID(_ context.Context, _ uint64, _ uint64) error     { return nil }
func (r *fakeEdgeRepo) SetAgentVersion(_ context.Context, _ uint64, _ string) error { return nil }
func (r *fakeEdgeRepo) SetPigVersion(_ context.Context, _ uint64, _ string) error   { return nil }
func (r *fakeEdgeRepo) Delete(_ context.Context, _ uint64) error                    { return nil }
func (r *fakeEdgeRepo) Count(_ context.Context) (int64, error)                      { return int64(len(r.byID)), nil }

package topology

// fakes_test.go is the scaffolding the topology tests share internally.
//
// Both doubles exist because the tools package needs the same two, and a
// test double is not a contract between packages — it is a way of not
// standing up a real Prometheus and a real edge repo. Two copies cannot
// drift into anything that matters because neither is reachable from
// production code.

import (
	"context"
	"sync"
	"time"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
)

type fakePromQuerier struct {
	mu       sync.Mutex
	gotExpr  string
	gotStep  time.Duration
	gotStart time.Time
	gotEnd   time.Time
	resp     *promquery.InstantResult
	err      error
}

func (f *fakePromQuerier) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) (*promquery.InstantResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotExpr = expr
	f.gotStart = start
	f.gotEnd = end
	f.gotStep = step
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// Query is the instant-form sibling of QueryRange. The PromQuerier
// interface grew it so correlate_incident.go can probe a single point
// in time; the smoke tests in this package don't exercise it, so the
// stub mirrors QueryRange's response/error behavior without recording
// timing.
func (f *fakePromQuerier) Query(_ context.Context, expr string, _ time.Time) (*promquery.InstantResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotExpr = expr
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// fakeEdgeRepo is an in-memory edge.Repo. The tools package keeps a
// copy; a test double is scaffolding inside a package, not a contract
// between two of them.
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

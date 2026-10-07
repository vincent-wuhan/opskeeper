package database

// fakes_test.go — the in-memory edge repo this cluster's tests need.
//
// A copy of the tools package's fakeEdgeRepo of the same name. Test
// doubles are not a contract between packages: sharing one would let a
// test here break because somebody widened it over there to satisfy an
// unrelated test, and the failure would point at this file rather than
// at the edit that caused it.

import (
	"context"
	"time"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// fakeEdgeRepo is an in-memory edge.Repo for the database tools' tests.
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

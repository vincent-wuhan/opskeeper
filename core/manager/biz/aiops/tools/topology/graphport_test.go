package topology_test

import (
	"context"
	"testing"

	aiopstopology "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
	topologybiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/topology"
	topologymodel "github.com/vincent-wuhan/opskeeper/core/manager/model/topology"
)

// testGraphAdapter is the test-side twin of the production adapter in
// cmd/opskeeper/aiops_topology_wiring.go. It exists because core/manager
// packages cannot import package main, and because a test that went
// through the real adapter would need the whole wiring root to be
// constructible. The two are kept honest separately: the conversion
// functions below are pinned field by field by
// TestTheTestGraphAdapterCarriesEveryFilterField, and the production pair
// is pinned by its own test in cmd/opskeeper.
type testGraphAdapter struct {
	uc *topologybiz.Usecase
}

var _ aiopstopology.Graph = (*testGraphAdapter)(nil)

func (a *testGraphAdapter) GetNode(ctx context.Context, id uint64) (*aiopstopology.Node, error) {
	n, err := a.uc.GetNode(ctx, id)
	if err != nil {
		return nil, err
	}
	return nodeProjection(n), nil
}

func (a *testGraphAdapter) ListNodes(ctx context.Context, f aiopstopology.NodeListFilter) ([]*aiopstopology.Node, int64, error) {
	nodes, total, err := a.uc.ListNodes(ctx, topologybiz.NodeListFilter{Type: f.Type, Q: f.Q, Limit: f.Limit})
	if err != nil {
		return nil, 0, err
	}
	out := make([]*aiopstopology.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeProjection(n))
	}
	return out, total, nil
}

func (a *testGraphAdapter) ListRelations(ctx context.Context, f aiopstopology.RelationListFilter) ([]*aiopstopology.Relation, int64, error) {
	rels, total, err := a.uc.ListRelations(ctx, topologybiz.RelationListFilter{Limit: f.Limit})
	if err != nil {
		return nil, 0, err
	}
	out := make([]*aiopstopology.Relation, 0, len(rels))
	for _, r := range rels {
		out = append(out, &aiopstopology.Relation{SrcID: r.SrcID, DstID: r.DstID, Type: r.Type})
	}
	return out, total, nil
}

func (a *testGraphAdapter) ListRelationTypes(ctx context.Context) ([]*aiopstopology.RelationType, error) {
	rts, err := a.uc.ListRelationTypes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*aiopstopology.RelationType, 0, len(rts))
	for _, rt := range rts {
		out = append(out, &aiopstopology.RelationType{
			Name:              rt.Name,
			SemanticsTag:      rt.SemanticsTag,
			PropagatesFailure: rt.PropagatesFailure,
		})
	}
	return out, nil
}

func nodeProjection(n *topologymodel.Node) *aiopstopology.Node {
	if n == nil {
		return nil
	}
	return &aiopstopology.Node{ID: n.ID, Type: n.Type, Name: n.Name}
}

// A filter field that is dropped by the conversion cannot be caught by the
// tool tests: both tools only ever set one field, and a default-valued
// field filters nothing out. So this test drives the adapter through the
// real usecase with values that are non-zero AND mutually distinguishable,
// and asserts the rows that come back. Type/Q are given different literal
// values on purpose — if a conversion crossed the two fields, the filter
// would match nothing and this would fail rather than silently pass.
func TestTheTestGraphAdapterCarriesEveryFilterField(t *testing.T) {
	uc := newTopologyUC(t)
	ctx := context.Background()
	g := &testGraphAdapter{uc: uc}

	if _, err := uc.CreateNode(ctx, "service", "order-api", ""); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := uc.RegisterRelationType(ctx, topologymodel.RelationType{
		Name: "zz_test_edge", Direction: string(topologymodel.DirectionSrcToDst),
		SemanticsTag: string(topologymodel.SemanticsHardDep), PropagatesFailure: true,
	}); err != nil {
		t.Fatalf("register relation type: %v", err)
	}
	from, err := uc.CreateNode(ctx, "app", "checkout", "")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	to, err := uc.CreateNode(ctx, "service", "db", "")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	if _, err := uc.CreateRelation(ctx, from.ID, to.ID, "zz_test_edge", ""); err != nil {
		t.Fatalf("create relation: %v", err)
	}
	if _, err := uc.CreateRelation(ctx, to.ID, from.ID, "zz_test_edge", ""); err != nil {
		t.Fatalf("create reverse relation: %v", err)
	}

	// Limit reaches the query: asking for 1 of the 3 seeded nodes must
	// return 1 row with total 3. A dropped Limit would return 3 rows.
	one, total, err := g.ListNodes(ctx, aiopstopology.NodeListFilter{Limit: 1})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(one) != 1 || total != 3 {
		t.Fatalf("Limit not carried: got %d rows / total %d, want 1 / 3", len(one), total)
	}

	// Q reaches the query, and it is not the Type field doing the work:
	// "checkout" is an app name, "service" is a type. Swap them and the
	// result must go empty, which is what pins the two to their own field.
	qHits, _, err := g.ListNodes(ctx, aiopstopology.NodeListFilter{Q: "checkout", Type: "service"})
	if err != nil {
		t.Fatalf("ListNodes by Q: %v", err)
	}
	if len(qHits) != 0 {
		t.Fatalf("Q and Type look crossed: got %d hits for Q=checkout Type=service, want 0", len(qHits))
	}
	qHits, _, err = g.ListNodes(ctx, aiopstopology.NodeListFilter{Q: "checkout", Type: "app"})
	if err != nil {
		t.Fatalf("ListNodes by Q: %v", err)
	}
	if len(qHits) != 1 || qHits[0].Name != "checkout" {
		t.Fatalf("Q not carried: got %+v, want the one node named checkout", qHits)
	}

	// Same story for relations.
	rels, _, err := g.ListRelations(ctx, aiopstopology.RelationListFilter{Limit: 1})
	if err != nil {
		t.Fatalf("ListRelations: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("relation Limit not carried: got %d rows, want 1", len(rels))
	}

	// The two pass-through methods are here so a signature change on the
	// port shows up as a compile error rather than as a tool that quietly
	// stops finding anything.
	if _, err := g.GetNode(ctx, from.ID); err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	rts, err := g.ListRelationTypes(ctx)
	if err != nil {
		t.Fatalf("ListRelationTypes: %v", err)
	}
	// Migrate seeds the built-in relation types, so this asserts the row
	// is among them rather than that it is the only one.
	found := false
	for _, rt := range rts {
		if rt.Name == "zz_test_edge" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListRelationTypes: zz_test_edge missing from %d rows", len(rts))
	}
}

package main

import (
	"context"

	aiopstopology "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
	topologybiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/topology"
	topologymodel "github.com/vincent-wuhan/opskeeper/core/manager/model/topology"
)

// topologyGraphAdapter hands the aiops topology tools a read-only view of
// the topology domain without giving them a compile-time dependency on it
// (decision 276). It lives at the wiring root for the same reason the
// federation adapter does: it is the one place allowed to know about both
// sides.
//
// Every translation in this file is a named function rather than an inline
// struct literal, and each is pinned field by field by
// aiops_topology_wiring_test.go. A conversion is exactly the place where a
// mistyped field compiles fine and returns nothing at runtime, and the
// tools cannot catch it themselves: they set one filter field at a time, so
// a dropped field still produces plausible-looking JSON.
type topologyGraphAdapter struct {
	uc *topologybiz.Usecase
}

var _ aiopstopology.Graph = (*topologyGraphAdapter)(nil)

func (a *topologyGraphAdapter) GetNode(ctx context.Context, id uint64) (*aiopstopology.Node, error) {
	n, err := a.uc.GetNode(ctx, id)
	if err != nil {
		return nil, err
	}
	return nodeFrom(n), nil
}

func (a *topologyGraphAdapter) ListNodes(ctx context.Context, f aiopstopology.NodeListFilter) ([]*aiopstopology.Node, int64, error) {
	nodes, total, err := a.uc.ListNodes(ctx, nodeFilterTo(f))
	if err != nil {
		return nil, 0, err
	}
	out := make([]*aiopstopology.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeFrom(n))
	}
	return out, total, nil
}

func (a *topologyGraphAdapter) ListRelations(ctx context.Context, f aiopstopology.RelationListFilter) ([]*aiopstopology.Relation, int64, error) {
	rels, total, err := a.uc.ListRelations(ctx, relationFilterTo(f))
	if err != nil {
		return nil, 0, err
	}
	out := make([]*aiopstopology.Relation, 0, len(rels))
	for _, r := range rels {
		out = append(out, relationFrom(r))
	}
	return out, total, nil
}

func (a *topologyGraphAdapter) ListRelationTypes(ctx context.Context) ([]*aiopstopology.RelationType, error) {
	rts, err := a.uc.ListRelationTypes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*aiopstopology.RelationType, 0, len(rts))
	for _, rt := range rts {
		out = append(out, relationTypeFrom(rt))
	}
	return out, nil
}

func nodeFilterTo(f aiopstopology.NodeListFilter) topologybiz.NodeListFilter {
	return topologybiz.NodeListFilter{
		Type:  f.Type,
		Q:     f.Q,
		Limit: f.Limit,
	}
}

func relationFilterTo(f aiopstopology.RelationListFilter) topologybiz.RelationListFilter {
	return topologybiz.RelationListFilter{
		Limit: f.Limit,
	}
}

// nodeFrom projects one row. nil in, nil out — the tools check the error
// first and a nil row behind a nil error would be a nil dereference in the
// BFS rather than a reported failure.
func nodeFrom(n *topologymodel.Node) *aiopstopology.Node {
	if n == nil {
		return nil
	}
	return &aiopstopology.Node{ID: n.ID, Type: n.Type, Name: n.Name}
}

// relationFrom projects one edge. Direction is deliberately absent: the
// BFS works out which way it is walking from which endpoint matched, so
// projecting the row's own direction field would be a second answer to a
// question the tool already answers.
func relationFrom(r *topologymodel.Relation) *aiopstopology.Relation {
	if r == nil {
		return nil
	}
	return &aiopstopology.Relation{SrcID: r.SrcID, DstID: r.DstID, Type: r.Type}
}

// relationTypeFrom projects one edge-type row. PropagatesFailure is the
// field the blast-radius walk filters on, so a dropped assignment there
// would not break the tool — it would quietly widen the blast radius to
// every edge, which is why it is pinned individually below.
func relationTypeFrom(rt *topologymodel.RelationType) *aiopstopology.RelationType {
	if rt == nil {
		return nil
	}
	return &aiopstopology.RelationType{
		Name:              rt.Name,
		SemanticsTag:      rt.SemanticsTag,
		PropagatesFailure: rt.PropagatesFailure,
	}
}

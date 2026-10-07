package topology

import "context"

// Graph is the read-only slice of the business-topology domain that the
// two topology tools in this package actually consume. It is declared here
// rather than imported from biz/topology or model/topology so the aiops
// domain keeps no compile-time dependency on the topology domain at all
// (decision 276) — the rows come back as the projections below, not as the
// topology domain's own structs.
//
// The concrete *topology.Usecase cannot satisfy this interface as-is: its
// two list methods take biz/topology's own filter types, and Go does not
// convert between two identical struct types with different names. The
// wiring root therefore supplies an adapter; see
// cmd/opskeeper/aiops_topology_wiring.go.
//
// Each projection carries only the fields a tool reads. That is a
// deliberate limit rather than a copy that will rot: a field added here
// without a reader is dead weight, and one missing shows up as a compile
// error in the tool that wanted it.
type Graph interface {
	GetNode(ctx context.Context, id uint64) (*Node, error)
	ListNodes(ctx context.Context, f NodeListFilter) ([]*Node, int64, error)
	ListRelations(ctx context.Context, f RelationListFilter) ([]*Relation, int64, error)
	ListRelationTypes(ctx context.Context) ([]*RelationType, error)
}

// Node is the node detail both tools put in their JSON output: the id they
// address it by, plus the type/name pair a human reads. PropsJSON is not
// projected — no tool surfaces it, and carrying a whole JSON blob across a
// domain boundary for nobody to read is how that blob becomes a second
// source of truth.
type Node struct {
	ID   uint64
	Type string
	Name string
}

// Relation is one directed edge, as the BFS sees it: which two nodes it
// joins and what type it carries.
type Relation struct {
	SrcID uint64
	DstID uint64
	Type  string
}

// RelationType is the AIOps semantics attached to an edge name. Direction
// is not projected: the BFS derives the direction it walks from which
// endpoint matched, not from this row.
type RelationType struct {
	Name              string
	SemanticsTag      string
	PropagatesFailure bool
}

// NodeListFilter narrows a node listing. Type and Q are optional; Q is the
// substring match the find tool relies on.
type NodeListFilter struct {
	Type  string
	Q     string
	Limit int
}

// RelationListFilter narrows a relation listing. expand_topology only ever
// asks for "everything, bounded", so it carries the bound and nothing else.
type RelationListFilter struct {
	Limit int
}

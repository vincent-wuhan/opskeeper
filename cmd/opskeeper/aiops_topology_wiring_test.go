package main

import (
	"testing"

	aiopstopology "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
	topologymodel "github.com/vincent-wuhan/opskeeper/core/manager/model/topology"
)

// The five translations in aiops_topology_wiring.go are the only place
// where the aiops port's shape is translated into the topology domain's.
// A field mistyped or dropped here compiles fine and returns nothing at
// runtime, and the tool tests cannot catch it: they set one filter field
// at a time, so a dropped field still produces plausible-looking JSON.
// Hence one assertion per field, with values that are distinct so a
// crossed assignment is visible rather than plausible.
func TestTheProductionTopologyFilterConversionsCarryEveryField(t *testing.T) {
	got := nodeFilterTo(aiopstopology.NodeListFilter{Type: "service", Q: "order-api", Limit: 7})
	if got.Type != "service" {
		t.Errorf("Type: got %q, want %q", got.Type, "service")
	}
	if got.Q != "order-api" {
		t.Errorf("Q: got %q, want %q", got.Q, "order-api")
	}
	if got.Limit != 7 {
		t.Errorf("Limit: got %d, want 7", got.Limit)
	}
	// The topology filter has an Offset the port deliberately does not
	// carry. Assert it stays zero rather than inheriting anything, so
	// adding it to the port later is a visible decision.
	if got.Offset != 0 {
		t.Errorf("Offset: got %d, want 0 (the port does not carry it)", got.Offset)
	}

	relGot := relationFilterTo(aiopstopology.RelationListFilter{Limit: 9})
	if relGot.Limit != 9 {
		t.Errorf("relation Limit: got %d, want 9", relGot.Limit)
	}
	// Same as Offset above, for every endpoint field the port omits.
	if relGot.SrcID != 0 || relGot.DstID != 0 || relGot.SrcOrDstID != 0 || relGot.Type != "" {
		t.Errorf("relation filter: got %+v, want only Limit set", relGot)
	}
}

func TestTheProductionRowProjectionsCarryEveryField(t *testing.T) {
	n := nodeFrom(&topologymodel.Node{ID: 11, Type: "service", Name: "order-api", PropsJSON: `{"x":1}`})
	if n == nil {
		t.Fatal("nodeFrom: got nil")
	}
	if n.ID != 11 {
		t.Errorf("Node.ID: got %d, want 11", n.ID)
	}
	if n.Type != "service" {
		t.Errorf("Node.Type: got %q, want %q", n.Type, "service")
	}
	if n.Name != "order-api" {
		t.Errorf("Node.Name: got %q, want %q", n.Name, "order-api")
	}

	r := relationFrom(&topologymodel.Relation{ID: 3, SrcID: 11, DstID: 12, Type: "depends_on"})
	if r == nil {
		t.Fatal("relationFrom: got nil")
	}
	// SrcID and DstID get different values on purpose: swapping them is
	// the single most damaging mistake available in this conversion, and
	// it would produce a graph that still looks connected.
	if r.SrcID != 11 || r.DstID != 12 {
		t.Errorf("Relation endpoints: got src=%d dst=%d, want src=11 dst=12", r.SrcID, r.DstID)
	}
	if r.Type != "depends_on" {
		t.Errorf("Relation.Type: got %q, want %q", r.Type, "depends_on")
	}

	// PropagatesFailure false with every other field populated: the
	// default-value case is the one that hides a dropped assignment, so
	// both polarities are checked below.
	rt := relationTypeFrom(&topologymodel.RelationType{
		Name:         "depends_on",
		SemanticsTag: "hard_dep",
		Direction:    "dst_to_src",
	})
	if rt == nil {
		t.Fatal("relationTypeFrom: got nil")
	}
	if rt.Name != "depends_on" {
		t.Errorf("RelationType.Name: got %q, want %q", rt.Name, "depends_on")
	}
	if rt.SemanticsTag != "hard_dep" {
		t.Errorf("RelationType.SemanticsTag: got %q, want %q", rt.SemanticsTag, "hard_dep")
	}
	if rt.PropagatesFailure {
		t.Error("RelationType.PropagatesFailure: got true, want false (the row does not set it)")
	}
	yes := relationTypeFrom(&topologymodel.RelationType{Name: "routes_to", PropagatesFailure: true})
	if yes == nil || !yes.PropagatesFailure {
		t.Error("RelationType.PropagatesFailure: got false, want true — a dropped assignment here widens the blast radius instead of erroring")
	}
}

// A nil row behind a nil error would be dereferenced by the BFS. The
// projections turn it into a nil the caller can see.
func TestTheProductionRowProjectionsPassNilThrough(t *testing.T) {
	if nodeFrom(nil) != nil {
		t.Error("nodeFrom(nil): want nil")
	}
	if relationFrom(nil) != nil {
		t.Error("relationFrom(nil): want nil")
	}
	if relationTypeFrom(nil) != nil {
		t.Error("relationTypeFrom(nil): want nil")
	}
}

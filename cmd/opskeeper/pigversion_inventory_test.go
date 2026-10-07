package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	managerbizedge "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	managersvcedge "github.com/vincent-wuhan/opskeeper/core/manager/service/edge"
)

// The compatibility matrix's only input.
//
// Everything above this line is plumbing that has been tested at its own
// layer: the node puts a version on the heartbeat, the usecase stores it,
// the store persists it. This file is the one place where all of that is
// reduced to the two strings the matrix actually compares, which makes it
// the only place a mismatch between the two axes can survive to the
// operator — a node whose PiG build is read from the wrong column, or not
// read at all, produces a matrix that looks completely healthy and is
// wrong in the direction that clears a release.

// listOnlyEdgeRepo answers List and refuses everything else.
//
// Refusing beats stubbing on purpose. A stub that returns zero values for
// an unimplemented method would let this test keep passing after someone
// rerouted NodeVersions through some other call, which is precisely the
// change it exists to catch.
type listOnlyEdgeRepo struct {
	edges []*edgemodel.Edge
}

func (r listOnlyEdgeRepo) List(context.Context, managerbizedge.ListFilter) ([]*edgemodel.Edge, error) {
	return r.edges, nil
}

func (listOnlyEdgeRepo) Create(context.Context, *edgemodel.Edge) error {
	panic("pigversion inventory test: Create is not on this path")
}

func (listOnlyEdgeRepo) GetByID(context.Context, uint64) (*edgemodel.Edge, error) {
	panic("pigversion inventory test: GetByID is not on this path")
}

func (listOnlyEdgeRepo) GetByAccessKey(context.Context, string) (*edgemodel.Edge, error) {
	panic("pigversion inventory test: GetByAccessKey is not on this path")
}

func (listOnlyEdgeRepo) GetByName(context.Context, string) (*edgemodel.Edge, error) {
	panic("pigversion inventory test: GetByName is not on this path")
}

func (listOnlyEdgeRepo) UpdateSecretHash(context.Context, uint64, string) error {
	panic("pigversion inventory test: UpdateSecretHash is not on this path")
}

func (listOnlyEdgeRepo) UpdateStatus(context.Context, uint64, string, time.Time) error {
	panic("pigversion inventory test: UpdateStatus is not on this path")
}

func (listOnlyEdgeRepo) UpdateName(context.Context, uint64, string) error {
	panic("pigversion inventory test: UpdateName is not on this path")
}

func (listOnlyEdgeRepo) SetDeviceID(context.Context, uint64, uint64) error {
	panic("pigversion inventory test: SetDeviceID is not on this path")
}

func (listOnlyEdgeRepo) SetAgentVersion(context.Context, uint64, string) error {
	panic("pigversion inventory test: SetAgentVersion is not on this path")
}

func (listOnlyEdgeRepo) SetPigVersion(context.Context, uint64, string) error {
	panic("pigversion inventory test: SetPigVersion is not on this path")
}

func (listOnlyEdgeRepo) Delete(context.Context, uint64) error {
	panic("pigversion inventory test: Delete is not on this path")
}

func (listOnlyEdgeRepo) Count(context.Context) (int64, error) {
	panic("pigversion inventory test: Count is not on this path")
}

func newPigVersionInventory(t *testing.T, edges ...*edgemodel.Edge) *edgeVersionInventory {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	uc := managerbizedge.NewUsecase(listOnlyEdgeRepo{edges: edges}, nil, nil, discard)
	return &edgeVersionInventory{svc: managersvcedge.New(uc, nil, discard)}
}

// Both axes reach the matrix, from their own columns, independently.
//
// The fleet below is the shape that actually occurs: a node current on both
// axes, a node current on the edge but behind on PiG, and a node that
// reports nothing for the PiG axis at all. Asserting the third case
// matters more than it looks — it is the one where a "helpful" fallback
// (defaulting to the edge version, or to a known-good constant) would
// turn an honest "cannot tell" into a fleet-wide all-clear.
func TestTheCompatibilityInventoryReadsBothVersionAxesOffTheEdgeRow(t *testing.T) {
	inv := newPigVersionInventory(t,
		&edgemodel.Edge{ID: 1, Name: "web-1", AgentVersion: "0.8.0", PigVersion: "0.3.0"},
		&edgemodel.Edge{ID: 2, Name: "web-2", AgentVersion: "0.8.0", PigVersion: "0.2.9"},
		&edgemodel.Edge{ID: 3, Name: "silent", AgentVersion: "0.8.0", PigVersion: ""},
	)

	got, err := inv.NodeVersions(context.Background())
	if err != nil {
		t.Fatalf("NodeVersions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d nodes, want 3", len(got))
	}

	want := []struct {
		nodeID uint64
		name   string
		edge   string
		pig    string
	}{
		{1, "web-1", "0.8.0", "0.3.0"},
		{2, "web-2", "0.8.0", "0.2.9"},
		{3, "silent", "0.8.0", ""},
	}
	for i, w := range want {
		g := got[i]
		if g.NodeID != w.nodeID {
			t.Errorf("node %d: NodeID = %d, want %d", i, g.NodeID, w.nodeID)
		}
		if g.Name != w.name {
			t.Errorf("node %d: Name = %q, want %q", i, g.Name, w.name)
		}
		if g.EdgeVersion != w.edge {
			t.Errorf("node %d: EdgeVersion = %q, want %q", i, g.EdgeVersion, w.edge)
		}
		if g.PigVersion != w.pig {
			t.Errorf("node %d: PigVersion = %q, want %q", i, g.PigVersion, w.pig)
		}
	}
}

// A node that never reported a PiG version must reach the matrix as an
// empty string, not as a substituted one.
//
// This is the assertion that keeps the "just default it" shortcut out of
// the code. pluginmanifest.CheckVersions reads an empty axis as "this node
// cannot tell" and refuses the package on it, and that refusal is the only
// thing standing between an unknown agent build and a package whose
// extensions were written against a newer one. Any value invented here —
// the edge version, the fleet's newest, a constant — would clear every node
// at once, silently, on the single comparison that decides whether an agent
// can load a package at all.
func TestANodeWithNoPigVersionReachesTheMatrixAsUnknownRatherThanSubstituted(t *testing.T) {
	inv := newPigVersionInventory(t,
		&edgemodel.Edge{ID: 7, Name: "quiet", AgentVersion: "0.8.0", PigVersion: ""},
	)

	got, err := inv.NodeVersions(context.Background())
	if err != nil {
		t.Fatalf("NodeVersions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d nodes, want 1", len(got))
	}
	if got[0].PigVersion != "" {
		t.Errorf("PigVersion = %q for a node that never reported one; "+
			"an invented value here becomes a fleet-wide all-clear", got[0].PigVersion)
	}
	// And the edge axis is genuinely still being read, so the empty above
	// is the node's own silence rather than a broken read.
	if got[0].EdgeVersion != "0.8.0" {
		t.Errorf("EdgeVersion = %q, want 0.8.0; if this is empty the row was not read at all", got[0].EdgeVersion)
	}
}

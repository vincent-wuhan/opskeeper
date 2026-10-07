package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The compatibility matrix is the control plane's answer to "which of my
// nodes can take this package", asked before a release instead of during
// one.
//
// What these tests are actually protecting is a single property: the
// matrix must not be a second opinion. It is a projection of
// pluginmanifest.CheckVersions, which is the same function the node runs
// when it refuses. A matrix that computed its own answer would be more
// dangerous than having none, because it would be consulted first and
// believed.

type fakeVersions struct {
	nodes []NodeVersions
	err   error
}

func (f fakeVersions) NodeVersions(context.Context) ([]NodeVersions, error) {
	return f.nodes, f.err
}

type stubFleet struct{ ids []uint64 }

func (f stubFleet) EdgeIDs(context.Context) ([]uint64, error) { return f.ids, nil }

// noNode is a Node that refuses everything. Compatibility never
// dispatches, so its behaviour here is only a witness that the test is not
// quietly exercising a release path.
type noNode struct{}

func (noNode) Install(context.Context, uint64, ports.PluginSpec) Outcome {
	return Outcome{Status: "refused", Reason: "not used"}
}
func (noNode) Remove(context.Context, uint64, string, string) Outcome {
	return Outcome{Status: "refused", Reason: "not used"}
}
func (noNode) Restore(context.Context, uint64, string, string) Outcome {
	return Outcome{Status: "refused", Reason: "not used"}
}

func managerWith(t *testing.T, v Versions) *Manager {
	t.Helper()
	return NewManager(stubFleet{}, noNode{}, nil).WithVersions(v)
}

func TestAMatrixRefusesEveryNodeThePackageIsTooNewFor(t *testing.T) {
	// A mixed fleet, which is the only kind worth having this for. Three
	// nodes, one requirement, and the split an operator would otherwise
	// discover over three failed canary waves.
	m := managerWith(t, fakeVersions{nodes: []NodeVersions{
		{NodeID: 1, Name: "web-1", EdgeVersion: "0.8.0", PigVersion: "0.3.0"},
		{NodeID: 2, Name: "web-2", EdgeVersion: "0.7.2", PigVersion: "0.3.0"},
		{NodeID: 3, Name: "db-1", EdgeVersion: "0.8.0", PigVersion: "0.2.9"},
	}})
	matrix, err := m.Compatibility(context.Background(), Requirement{
		Plugin: "opskeeper-sre-repair", Version: "0.1.0",
		MinEdgeVersion: "0.8.0", MinPigVersion: "0.3.0",
	})
	if err != nil {
		t.Fatalf("Compatibility: %v", err)
	}

	if len(matrix.Hostable) != 1 || matrix.Hostable[0].NodeID != 1 {
		t.Errorf("hostable = %+v, want node 1 alone", matrix.Hostable)
	}
	if len(matrix.Refused) != 2 {
		t.Fatalf("refused = %+v, want two nodes", matrix.Refused)
	}

	// The two refusals must name different components, because the fixes
	// are different. A matrix that said "too old" for both would send an
	// operator to upgrade the edge on a node whose agent is the problem.
	byID := map[uint64]Verdict{}
	for _, v := range matrix.Refused {
		byID[v.NodeID] = v
	}
	if got := byID[2].Step; got != "version" {
		t.Errorf("node 2 refused at step %q, want version — its edge build is the problem", got)
	}
	if !strings.Contains(byID[2].Reason, "0.7.2") || !strings.Contains(byID[2].Reason, "0.8.0") {
		t.Errorf("node 2's reason %q does not name both versions", byID[2].Reason)
	}
	if got := byID[3].Step; got != "agent_version" {
		t.Errorf("node 3 refused at step %q, want agent_version — its PiG build is the problem", got)
	}
	if !strings.Contains(byID[3].Reason, "0.2.9") {
		t.Errorf("node 3's reason %q does not name the agent build that is too old", byID[3].Reason)
	}
}

func TestANodeThatCannotStateAVersionIsRefusedRatherThanCountedHostable(t *testing.T) {
	// The failure this endpoint most easily has, and the most expensive.
	//
	// A node that has never reported a version is not a node that is
	// known-good; it is a node whose state is unknown. Counting it
	// hostable puts it in the first canary wave, where it fails — and
	// fails with a refusal about a version, during a release, to whoever
	// is watching. So unknown is refused, and the reason says which
	// component is missing rather than implying the node is too old.
	m := managerWith(t, fakeVersions{nodes: []NodeVersions{
		{NodeID: 1, Name: "known", EdgeVersion: "0.8.0", PigVersion: "0.3.0"},
		{NodeID: 2, Name: "silent", EdgeVersion: "", PigVersion: ""},
		{NodeID: 3, Name: "half", EdgeVersion: "0.8.0", PigVersion: ""},
	}})
	matrix, err := m.Compatibility(context.Background(), Requirement{
		Plugin: "p", Version: "1.0.0", MinEdgeVersion: "0.8.0", MinPigVersion: "0.3.0",
	})
	if err != nil {
		t.Fatalf("Compatibility: %v", err)
	}
	if len(matrix.Hostable) != 1 || matrix.Hostable[0].NodeID != 1 {
		t.Errorf("hostable = %+v, want only the node that stated both versions", matrix.Hostable)
	}
	if len(matrix.Refused) != 2 {
		t.Fatalf("refused = %+v, want nodes 2 and 3", matrix.Refused)
	}
	for _, v := range matrix.Refused {
		if v.Reason == "" {
			t.Errorf("node %d is refused with no reason", v.NodeID)
		}
		if v.EdgeVersion == "" && strings.Contains(v.Reason, "runs edge") {
			t.Errorf("node %d is told it 'runs edge' something when it stated no edge version: %q",
				v.NodeID, v.Reason)
		}
	}
}

func TestAPackageThatAsksForNothingIsHostableEverywhere(t *testing.T) {
	// An empty requirement is a yes, and it has to be. Refusing every
	// package that omits an optional field would make min_edge_version
	// mandatory by accident, and the first package written without it
	// would be un-installable fleet-wide.
	m := managerWith(t, fakeVersions{nodes: []NodeVersions{
		{NodeID: 1, EdgeVersion: "0.1.0", PigVersion: "0.1.0"},
		{NodeID: 2, EdgeVersion: "", PigVersion: ""},
	}})
	matrix, err := m.Compatibility(context.Background(), Requirement{Plugin: "p", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Compatibility: %v", err)
	}
	if len(matrix.Hostable) != 2 || len(matrix.Refused) != 0 {
		t.Errorf("a package with no version requirement got hostable=%d refused=%d, want 2/0",
			len(matrix.Hostable), len(matrix.Refused))
	}
}

func TestTheMatrixIsOrderedSoTwoOperatorsReadTheSameRows(t *testing.T) {
	// It is rendered next to a fleet listing that is sorted by node id, and
	// the person comparing two releases has to be looking at the same rows
	// in the same order. Input order is deliberately reversed here.
	m := managerWith(t, fakeVersions{nodes: []NodeVersions{
		{NodeID: 9, EdgeVersion: "0.8.0", PigVersion: "0.3.0"},
		{NodeID: 2, EdgeVersion: "0.8.0", PigVersion: "0.3.0"},
		{NodeID: 7, EdgeVersion: "0.8.0", PigVersion: "0.3.0"},
		{NodeID: 4, EdgeVersion: "0.1.0", PigVersion: "0.1.0"},
		{NodeID: 1, EdgeVersion: "0.8.0", PigVersion: "0.3.0"},
	}})
	matrix, err := m.Compatibility(context.Background(), Requirement{
		Plugin: "p", Version: "1.0.0", MinEdgeVersion: "0.8.0", MinPigVersion: "0.3.0",
	})
	if err != nil {
		t.Fatalf("Compatibility: %v", err)
	}
	var hostable, refused []uint64
	for _, v := range matrix.Hostable {
		hostable = append(hostable, v.NodeID)
	}
	for _, v := range matrix.Refused {
		refused = append(refused, v.NodeID)
	}
	if got := len(hostable); got != 4 {
		t.Errorf("hostable = %v, want four nodes", hostable)
	}
	for i := 1; i < len(hostable); i++ {
		if hostable[i-1] > hostable[i] {
			t.Errorf("hostable is not sorted by node id: %v", hostable)
			break
		}
	}
	if len(refused) != 1 || refused[0] != 4 {
		t.Errorf("refused = %v, want node 4 alone", refused)
	}
}

func TestARepeatedQueryReturnsTheSameAnswer(t *testing.T) {
	// The console polls this. A matrix whose row order changed between two
	// polls of an unchanged fleet would make the operator think a node
	// had changed state.
	m := managerWith(t, fakeVersions{nodes: []NodeVersions{
		{NodeID: 3, EdgeVersion: "0.8.0"}, {NodeID: 1, EdgeVersion: "0.1.0"},
		{NodeID: 2, EdgeVersion: "0.7.0"},
	}})
	req := Requirement{Plugin: "p", Version: "1.0.0", MinEdgeVersion: "0.8.0"}
	first, err := m.Compatibility(context.Background(), req)
	if err != nil {
		t.Fatalf("Compatibility: %v", err)
	}
	for i := 0; i < 8; i++ {
		again, err := m.Compatibility(context.Background(), req)
		if err != nil {
			t.Fatalf("Compatibility: %v", err)
		}
		for j := range first.Refused {
			if again.Refused[j].NodeID != first.Refused[j].NodeID {
				t.Fatalf("row %d moved between polls: %d then %d", j, first.Refused[j].NodeID, again.Refused[j].NodeID)
			}
		}
	}
}

func TestAManagerWithNoSnapshotRefusesRatherThanReportingAllClear(t *testing.T) {
	// The worst possible failure for this endpoint is a manager that has
	// not been wired and answers "every node can host this". It is the
	// check that exists to stop a bad release, and a silent success is
	// worse than a missing feature.
	m := NewManager(stubFleet{}, noNode{}, nil)
	if _, err := m.Compatibility(context.Background(), Requirement{Plugin: "p"}); !errors.Is(err, ErrNoVersionSnapshot) {
		t.Errorf("err = %v, want ErrNoVersionSnapshot", err)
	}
}

func TestAFailingSnapshotIsAnErrorNotAnEmptyFleet(t *testing.T) {
	// "No nodes" and "could not read the fleet" are different answers and
	// must not collapse into the same empty matrix. An operator who is
	// told the fleet is empty will start a release to nowhere.
	boom := errors.New("edge repo unavailable")
	m := managerWith(t, fakeVersions{err: boom})
	if _, err := m.Compatibility(context.Background(), Requirement{Plugin: "p"}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the snapshot's own error", err)
	}
}

func TestTheEmptyFleetIsAnAnswerRatherThanAFailure(t *testing.T) {
	// The one case where an empty result is the truth. A control plane
	// that has not onboarded a node yet should say so plainly, and the
	// caller can tell the difference from the two cases above because
	// those return errors.
	m := managerWith(t, fakeVersions{})
	matrix, err := m.Compatibility(context.Background(), Requirement{Plugin: "p", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Compatibility: %v", err)
	}
	if len(matrix.Hostable) != 0 || len(matrix.Refused) != 0 {
		t.Errorf("an empty fleet produced %+v", matrix)
	}
	if matrix.Plugin != "p" || matrix.Version != "1.0.0" {
		t.Errorf("the matrix lost the requirement it answers: %+v", matrix.Requirement)
	}
}

func TestACompatibilityQueryNeedsAPackageName(t *testing.T) {
	// A matrix with no subject cannot be read, and returning an empty one
	// for it would let a console render a plausible-looking empty page for
	// a request it got wrong.
	m := managerWith(t, fakeVersions{})
	if _, err := m.Compatibility(context.Background(), Requirement{}); err == nil {
		t.Error("a query with no package name was answered")
	}
}

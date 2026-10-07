package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The compatibility matrix, from the control plane's side.
//
// A node decides for itself whether it can host a package — that is
// pluginmanifest.Review, it runs on the node, and it is the only place the
// decision is authoritative. The design note on the version checks says so
// outright: the manager may ask for a package, but only the node can say
// whether it can run one.
//
// What the manager could not do was answer the question *before* asking.
// An operator who starts a rolling release against a fleet with three old
// nodes in it watches three waves fail, and learns from the failure log
// what the console could have told them in one call: those three are on
// 0.7.2, this package needs 0.8.0, here is the list.
//
// So this is a projection, not a second decision. Every verdict below comes
// from pluginmanifest.CheckVersions, which is the same function Review
// calls, in the same order, with the same wording. Divergence is not
// prevented by a test here — it is prevented by there being nowhere for a
// second implementation to live.

// NodeVersions is what the control plane knows about one node's ability to
// host a package.
//
// Both versions are the node's own reports. EdgeVersion is the build the
// edge agent states about itself; PigVersion is the agent build its
// supervisor last observed. They are separate fields because they are
// separate components on separate upgrade cadences, and a node can be
// current on one and stale on the other — which is exactly the case a
// single "version" column would hide.
type NodeVersions struct {
	NodeID uint64
	// Name is for the operator's benefit only. Nothing branches on it.
	Name        string
	EdgeVersion string
	PigVersion  string
}

// Requirement is what one package asks a node to be.
type Requirement struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	// MinEdgeVersion and MinPigVersion are the manifest's install policy.
	// Empty means the package expressed no requirement, which is a yes —
	// refusing every package that omits an optional field would make the
	// field mandatory by accident.
	MinEdgeVersion string `json:"min_edge_version,omitempty"`
	MinPigVersion  string `json:"min_pig_version,omitempty"`
}

// RequirementOf reads a package's requirement out of its manifest.
//
// It is a function rather than a field assignment so that a caller cannot
// accidentally pair a package's name with a different package's
// requirements, which is the one way this projection could report a
// confidently wrong answer.
func RequirementOf(plugin, version string, m domain.PluginManifest) Requirement {
	return Requirement{
		Plugin:         plugin,
		Version:        version,
		MinEdgeVersion: m.Spec.Install.MinEdgeVersion,
		MinPigVersion:  m.Spec.Install.MinPigVersion,
	}
}

// Verdict is one node's answer.
type Verdict struct {
	NodeID      uint64 `json:"node_id"`
	Name        string `json:"name,omitempty"`
	EdgeVersion string `json:"edge_version,omitempty"`
	PigVersion  string `json:"pig_version,omitempty"`
	// Hostable is the answer. There is no third state: a node that has
	// not reported a version it can be compared against is refused, and
	// the reason says so. Counting it as hostable would put it in the
	// first canary wave and fail there.
	Hostable bool `json:"hostable"`
	// Step is which axis refused — the same value Review would report, so
	// the two can be compared by an operator without a glossary.
	Step string `json:"step,omitempty"`
	// Reason is the sentence the node itself would refuse with. It is
	// copied rather than rewritten so that an operator who is told "this
	// node runs edge 0.7.2" here and "this node runs edge 0.7.2" on the
	// node is reading one message, not two that happen to agree today.
	Reason string `json:"reason,omitempty"`
}

// Matrix is the fleet's answer for one package.
//
// Hostable and Refused partition the fleet rather than sitting in one
// ordered list with a flag, because the two are answered to different
// questions: how many nodes can take this, and what do I have to fix
// first. An operator starting a release wants both numbers, and a single
// list makes the second one a count.
type Matrix struct {
	Requirement
	Hostable []Verdict `json:"hostable"`
	Refused  []Verdict `json:"refused"`
}

// Evaluate produces the fleet's matrix for one requirement.
//
// Sorting is by node id, and it is not cosmetic: this is rendered next to
// a fleet listing that is also sorted by id, and two operators comparing
// the same release must be looking at the same rows in the same order.
func Evaluate(req Requirement, nodes []NodeVersions) Matrix {
	out := Matrix{Requirement: req}
	for _, n := range nodes {
		v := Verdict{
			NodeID:      n.NodeID,
			Name:        n.Name,
			EdgeVersion: n.EdgeVersion,
			PigVersion:  n.PigVersion,
		}
		ok, step, reason := pluginmanifest.CheckVersions(
			req.MinEdgeVersion, req.MinPigVersion, n.EdgeVersion, n.PigVersion,
		)
		v.Hostable, v.Step, v.Reason = ok, step, reason
		if ok {
			out.Hostable = append(out.Hostable, v)
		} else {
			out.Refused = append(out.Refused, v)
		}
	}
	sort.Slice(out.Hostable, func(i, j int) bool { return out.Hostable[i].NodeID < out.Hostable[j].NodeID })
	sort.Slice(out.Refused, func(i, j int) bool { return out.Refused[i].NodeID < out.Refused[j].NodeID })
	return out
}

// Versions is the control plane's version snapshot for the fleet.
//
// It is a port for the same reason Fleet is: a compatibility matrix is
// exactly the thing worth testing, and a test that has to stand up an edge
// inventory to assert "three nodes are too old" is a test nobody writes.
type Versions interface {
	NodeVersions(ctx context.Context) ([]NodeVersions, error)
}

// ErrNoVersionSnapshot is returned by Compatibility on a manager that was
// never given a Versions port.
//
// It is an error rather than an empty matrix because the two are very
// different things to an operator. An empty matrix says the fleet has no
// nodes; a missing snapshot says this control plane cannot answer, and
// answering "all clear" from a manager that simply has not been wired
// would be the worst possible failure for this endpoint — it is the one
// that exists to stop a release, and it would wave it through.
var ErrNoVersionSnapshot = errors.New("plugin: no fleet version snapshot is wired; cannot compute the compatibility matrix")

// WithVersions attaches the fleet's version snapshot.
//
// It is a separate call rather than a NewManager argument because the
// snapshot reaches the manager through a different system from the node
// dispatcher, and there are wirings — notably the ones in tests — that have
// one and not the other. A nil Versions is tolerated at construction and
// refused at use, which is the same bargain the HTTP handler makes.
func (m *Manager) WithVersions(v Versions) *Manager {
	if m != nil {
		m.versions = v
	}
	return m
}

// Compatibility answers, for one package, which nodes can host it and
// which cannot.
//
// The empty fleet is a real answer and is returned as one. A fleet with no
// nodes cannot be rolled out to, and a caller asking is entitled to be told
// so without it looking like a failure of the snapshot.
func (m *Manager) Compatibility(ctx context.Context, req Requirement) (Matrix, error) {
	if m == nil || m.versions == nil {
		return Matrix{}, ErrNoVersionSnapshot
	}
	if req.Plugin == "" {
		return Matrix{}, fmt.Errorf("plugin: a compatibility query needs a package name")
	}
	nodes, err := m.versions.NodeVersions(ctx)
	if err != nil {
		return Matrix{}, fmt.Errorf("plugin: read the fleet version snapshot: %w", err)
	}
	return Evaluate(req, nodes), nil
}

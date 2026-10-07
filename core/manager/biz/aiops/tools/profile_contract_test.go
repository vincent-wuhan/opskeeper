package tools

import (
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The control-plane half of the node's read-only toolset.
//
// The node's agent offers get_topology, expand_topology, find_topology_node,
// find_outlier_edges and query_alert_rules alongside the host probes. Those
// five are not implemented on the node — they read the manager's graph and
// its rule table, and the broker reaches them by asking the control plane.
// The names therefore have to be the same strings on both sides, and the
// assertion lives here because these constants are the ones that would
// drift: a rename here silently breaks a tool the node still offers.

// profileRoot locates the shipped package from this test's own directory,
// so the check does not depend on the working directory.
func profileRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..")
	return filepath.Join(root, "plugins", "pig-ops", "opskeeper-sre-readonly")
}

func TestEveryControlPlaneToolTheNodeOffersIsRegisteredHere(t *testing.T) {
	p, err := pluginmanifest.Load(profileRoot(t))
	if err != nil {
		t.Fatalf("load the read-only profile: %v", err)
	}
	declared := make(map[string]bool, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		declared[tool.Name] = true
	}

	for _, name := range []string{
		ToolNameGetTopology,
		topology.ToolNameExpandTopology,
		topology.ToolNameFindTopologyNode,
		topology.ToolNameFindOutlierEdges,
		alerting.ToolNameQueryAlertRules,
	} {
		if !declared[name] {
			t.Errorf("%q is served by this registry but the node's read-only profile does not "+
				"declare it, so the broker will refuse every call to it", name)
		}
	}
}

func TestTheControlPlaneToolsTheNodeOffersAreAllReadOnly(t *testing.T) {
	// The node's package is L1 and claims every tool in it needs no human.
	// A control-plane tool that is not read breaks that claim from the far
	// side, where the node's own manifest cannot see it.
	p, err := pluginmanifest.Load(profileRoot(t))
	if err != nil {
		t.Fatalf("load the read-only profile: %v", err)
	}
	classes := make(map[string]domain.ToolClass, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		classes[tool.Name] = tool.Class
	}
	for _, name := range []string{
		ToolNameGetTopology,
		topology.ToolNameExpandTopology,
		topology.ToolNameFindTopologyNode,
		topology.ToolNameFindOutlierEdges,
		alerting.ToolNameQueryAlertRules,
	} {
		if got, ok := classes[name]; !ok {
			t.Errorf("%q is not declared by the node's profile", name)
		} else if got != domain.ClassRead {
			t.Errorf("%q is %q in the node's profile; the read-only package admits only read", name, got)
		}
	}
}

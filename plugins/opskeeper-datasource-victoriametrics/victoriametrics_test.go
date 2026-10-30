package opskeeperdatasourcevictoriametrics

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/sdk"

	"github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics/internal/contributor"
)

// The manifest on disk and the manifest compiled into the binary are two things
// that drift, and the drift is invisible until an operator edits the YAML and
// wonders why nothing changed. Comparing the bytes is the only check that names
// it.
func TestEmbeddedManifestIsTheFileOnDisk(t *testing.T) {
	onDisk, err := os.ReadFile("pig-ops.yaml")
	if err != nil {
		t.Fatalf("read pig-ops.yaml: %v", err)
	}
	if !bytes.Equal(onDisk, pigOpsYAML) {
		t.Errorf("the embedded manifest (%d bytes) is not pig-ops.yaml (%d bytes); "+
			"a package that ships two manifests ships two truths",
			len(pigOpsYAML), len(onDisk))
	}
}

func TestManifestDecodesAndValidates(t *testing.T) {
	p, err := Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if got := p.Name(); got != PluginName {
		t.Errorf("manifest names %q, want %q", got, PluginName)
	}
	if err := sdk.Validate(p.Manifest); err != nil {
		t.Errorf("sdk.Validate: %v", err)
	}
}

// The manifest declares exactly one scope and one capability. A third-party
// package that asked for more would be asking for reach it has no use for, and
// the ask is the reviewable act — so widening it has to change a number this
// test can see.
func TestManifestDeclaresOnlyTheReadScope(t *testing.T) {
	p, err := Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	scopes := p.Manifest.Spec.RequiredScopes
	if len(scopes) != 1 || string(scopes[0]) != "observability.read" {
		t.Errorf("required_scopes = %v, want exactly [observability.read]", scopes)
	}
	if p.HighestCapability().String() != "read" {
		t.Errorf("highest capability = %s, want read", p.HighestCapability())
	}
}

// Admission here runs against a control-plane-shaped policy written out rather
// than against core/manager's ManagerPolicy(): importing the control plane from
// a third-party package is the boundary this module exists to hold. The
// authoritative "admits under ManagerPolicy()" assertion lives in the root
// module, where core/manager is legitimately importable. This one only has to
// show the manifest is admissible under a host that grants the scopes a
// datasource is granted.
func TestManifestAdmitsUnderAControlPlanePolicy(t *testing.T) {
	p, err := Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	host := kernel.NewHost(kernel.NewRegistry(), controlPlanePolicy(), quietLog())
	if err := host.Admit(p); err != nil {
		t.Fatalf("Admit under a control-plane policy: %v", err)
	}
}

// controlPlanePolicy mirrors the shape of the manager's datasource policy:
// L2 ceiling, single-namespace blast radius, and the read scopes a datasource
// is granted. It is a restatement, not the policy — see the note on the test
// above.
func controlPlanePolicy() kernel.Policy {
	return kernel.Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL2,
		MaxBlastRadius: domain.RadiusSingleNS,
		GrantedScopes: domain.Scopes{
			domain.ScopeMetricsRO,
			domain.ScopeDBRead,
			domain.ScopeAlertRO,
			domain.ScopeTopologyRO,
		},
	}
}

// quietLog keeps the host from writing to the test log. A nil logger would
// panic on the admission path, so it is a discarding one rather than nothing.
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestContributionRegistersUnderThePromSignal(t *testing.T) {
	c := contributor.VictoriaMetrics{}
	if c.PluginName() != PluginName {
		t.Errorf("PluginName = %q, want %q", c.PluginName(), PluginName)
	}
	if contributor.SignalProm != "prom" {
		t.Errorf("SignalProm = %q, want prom — the signal is reused, not added",
			contributor.SignalProm)
	}
}

// A second package wanting the same signal is refused by the registry rather
// than overwriting the first. This is the fail-closed property the whole
// single-activation story rests on, so it is asserted here rather than assumed:
// a manager that quietly kept the first registration would look healthy and
// be querying the wrong backend.
func TestASecondPackageOnTheSameSignalIsRefused(t *testing.T) {
	p, err := Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	host := kernel.NewHost(kernel.NewRegistry(), controlPlanePolicy(), quietLog())

	ctx := t.Context()
	if err := host.Activate(ctx, p, contributor.VictoriaMetrics{}); err != nil {
		t.Fatalf("first activation: %v", err)
	}

	// A different package, same signal: this is what a configuration mistake
	// would produce if the assembly layer activated both backends.
	other := p
	other.Manifest.Metadata.Name = "opskeeper-datasource-prom"

	err = host.Activate(ctx, other, stubContribution{plugin: other.Name()})
	if err == nil {
		t.Fatal("the second activation succeeded; a signal can only have one backend")
	}
	if !errors.Is(err, kernel.ErrDuplicate) {
		t.Errorf("err = %v, want it to wrap kernel.ErrDuplicate", err)
	}
}

// stubContribution registers the `prom` signal under a name of its own, which
// is exactly what the first-party package does.
type stubContribution struct{ plugin string }

func (s stubContribution) PluginName() string { return s.plugin }

func (s stubContribution) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindDatasource, contributor.SignalProm, nil)
}

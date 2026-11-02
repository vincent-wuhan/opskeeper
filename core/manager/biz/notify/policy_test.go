package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/datasource"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/pluginhost"
)

// TestNotifierPolicyIsManagerPolicyPlusExactlyTwoDeltas pins the derivation
// of the notifier admission policy.
//
// The count is the point. A policy that quietly grew a third delta would
// widen the control plane's ceiling for reasons nobody re-reads, and the
// two deltas that exist are the two a reviewer has to be able to see:
//
//   - the notifier extension point's ceiling, raised to L3 because a
//     notification writes into somebody else's system;
//   - the alert.write scope, without which every L3 notifier manifest is
//     refused at the door.
//
// Everything else is the datasource policy, unchanged.
func TestNotifierPolicyIsManagerPolicyPlusExactlyTwoDeltas(t *testing.T) {
	base := datasource.ManagerPolicy()
	got := Policy()

	if got.Target != base.Target {
		t.Errorf("Target = %v, want %v", got.Target, base.Target)
	}
	if got.MaxSafetyLevel != base.MaxSafetyLevel {
		t.Errorf("MaxSafetyLevel = %v, want %v", got.MaxSafetyLevel, base.MaxSafetyLevel)
	}
	if got.MaxBlastRadius != base.MaxBlastRadius {
		t.Errorf("MaxBlastRadius = %v, want %v", got.MaxBlastRadius, base.MaxBlastRadius)
	}

	// Delta one: the notifier ceiling.
	if len(got.KindCeilings) != 1 {
		t.Fatalf("KindCeilings has %d entries, want exactly 1: %v", len(got.KindCeilings), got.KindCeilings)
	}
	level, ok := got.KindCeilings[kernel.KindNotifier]
	if !ok {
		t.Fatalf("KindCeilings does not name %q: %v", kernel.KindNotifier, got.KindCeilings)
	}
	if level != domain.SafetyL3 {
		t.Errorf("notifier ceiling = %v, want %v", level, domain.SafetyL3)
	}
	if got.EffectiveSafetyCeiling(kernel.KindNotifier) != domain.SafetyL3 {
		t.Errorf("effective notifier ceiling = %v, want %v", got.EffectiveSafetyCeiling(kernel.KindNotifier), domain.SafetyL3)
	}

	// Every other kind keeps the global ceiling.
	for _, kind := range []kernel.Kind{kernel.KindTool, kernel.KindAdapter, kernel.KindDatasource, kernel.KindCollector, kernel.KindProvider} {
		if got.EffectiveSafetyCeiling(kind) != base.MaxSafetyLevel {
			t.Errorf("effective ceiling of %s = %v, want %v", kind, got.EffectiveSafetyCeiling(kind), base.MaxSafetyLevel)
		}
	}

	// Delta two: the alert.write scope, and nothing else.
	if len(got.GrantedScopes) != len(base.GrantedScopes)+1 {
		t.Fatalf("GrantedScopes has %d entries, want %d: %v", len(got.GrantedScopes), len(base.GrantedScopes)+1, got.GrantedScopes)
	}
	if !got.GrantedScopes.Has(domain.ScopeAlertWrite) {
		t.Errorf("GrantedScopes does not carry %q: %v", domain.ScopeAlertWrite, got.GrantedScopes)
	}
	for _, scope := range base.GrantedScopes {
		if !got.GrantedScopes.Has(scope) {
			t.Errorf("GrantedScopes dropped %q: %v", scope, got.GrantedScopes)
		}
	}
	if got.GrantedScopes.Has(domain.ScopeAlertWrite) != true {
		t.Error("the notifier policy must grant alert.write")
	}

	// The datasource policy is untouched: the derivation copies it, and a
	// policy that was mutated in place would change every other host's
	// ceiling as a side effect of this one.
	if len(base.KindCeilings) != 0 {
		t.Errorf("the datasource policy carries %d per-kind ceilings, want 0", len(base.KindCeilings))
	}
	if len(base.GrantedScopes) != 4 || base.GrantedScopes.Has(domain.ScopeAlertWrite) {
		t.Errorf("the datasource policy's scopes changed: %v", base.GrantedScopes)
	}
}

// TestAnL3PackageIsStillRefusedInOtherKindsUnderTheNotifierPolicy pins the
// second half of the policy's story.
//
// Raising the notifier ceiling to L3 raises the package-level gate with it,
// because the package gate reads the widest ceiling the host allows. An L3
// package therefore gets past the door — and is then refused per kind,
// because the notifier is the only extension point this host opens to L3.
//
// The refusal is per package, not per host: the package is reported as
// refused and the other packages on the host are unaffected.
func TestAnL3PackageIsStillRefusedInOtherKindsUnderTheNotifierPolicy(t *testing.T) {
	base := t.TempDir()
	writePackage(t, base, "opskeeper-l3-tool", l3ToolManifestYAML("opskeeper-l3-tool"))

	h := pluginhost.New(base, Policy(), nil)
	h.RegisterActivator("opskeeper-l3-tool", func() kernel.Contribution {
		return l3ToolContribution{plugin: "opskeeper-l3-tool"}
	})

	rep, err := h.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rep.Activated) != 0 {
		t.Errorf("Activated = %v, want none: an L3 package must not activate on this host", rep.Activated)
	}
	reason, ok := rep.Refused["opskeeper-l3-tool"]
	if !ok {
		t.Fatalf("the L3 package is not in the refusal report: %+v", rep)
	}
	if !strings.Contains(reason, `kind "tool"`) {
		t.Errorf("refusal reason %q does not name the kind that refused it", reason)
	}
	if !strings.Contains(reason, "L2") {
		t.Errorf("refusal reason %q does not name the ceiling that applied", reason)
	}

	// Whole-package rollback: the contribution registered into the notifier
	// kind before the tool kind refused it, and nothing it wrote survived.
	if n := h.Registry().Len(); n != 0 {
		t.Errorf("the registry holds %d entries after the refusal, want 0: a refused package must leave nothing behind", n)
	}
}

// l3ToolContribution registers into the notifier kind first and into the
// tool kind second, so the refusal happens after the package has already
// written something — which is what makes the rollback observable.
type l3ToolContribution struct {
	plugin string
}

func (c l3ToolContribution) PluginName() string { return c.plugin }

func (c l3ToolContribution) Register(r kernel.Registrar) error {
	if err := r.Register(kernel.KindNotifier, "l3-tool-probe", struct{}{}); err != nil {
		return err
	}
	return r.Register(kernel.KindTool, "l3-tool-probe", struct{}{})
}

// writePackage writes a package directory with a pig-ops.yaml into base.
func writePackage(t *testing.T, base, name, manifest string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pig-ops.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// l3ToolManifestYAML is the manifest of an L3 manager-targeted package. It
// asks for the scopes the notifier policy grants, so nothing but the
// per-kind ceiling can refuse it.
func l3ToolManifestYAML(name string) string {
	return "apiVersion: opskeeper.io/v1\n" +
		"kind: Plugin\n" +
		"metadata:\n  name: " + name + "\n  version: 1.0.0\n  vendor: thirdparty\n" +
		"spec:\n" +
		"  targets: [manager]\n" +
		"  safety_level: L3\n" +
		"  capabilities: [destructive]\n" +
		"  required_scopes: [alert.read, alert.write]\n" +
		"  audit: {emits: true, mutates: false}\n" +
		"  approval: {required: false}\n" +
		"  install: {strategy: pin}\n"
}

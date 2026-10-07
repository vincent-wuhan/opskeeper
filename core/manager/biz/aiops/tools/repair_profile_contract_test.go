package tools

import (
	"context"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/configchange"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/recovery"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The node half of the repair package.
//
// A node's agent reaches these four tools by asking: it holds no
// implementation of them, and the manager's registry is the only thing
// that runs them. That makes the manifest on the node and the Info() on
// this side two descriptions of the same tool, kept in different
// repositories by people who may never meet.
//
// They have to agree on the name — a mismatch and the broker refuses the
// call — and, more importantly, on the class. The class is what decides
// whether an operator is asked before a live system is changed. If the
// node declares apply_config_change as read because that is what a YAML
// file said, the model calls it believing no human is involved, and the
// refusal arrives as a surprise rather than as the agreed process.
//
// So the class is read from the tool itself here rather than restated.
// The tools below are constructed with nil collaborators on purpose:
// Info() reads only the tool's own fields, and a nil dependency means
// this test cannot be broken by a constructor that grows one.

// repairProfileRoot locates the shipped repair package from this test's
// own directory, so the check does not depend on the working directory.
func repairProfileRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..")
	return filepath.Join(root, "plugins", "pig-ops", "opskeeper-sre-repair")
}

// controlPlaneRepairTools are the repair package's tools this side owns.
//
// Built here rather than looked up from a registry, because a registry is
// assembled from a dozen optional collaborators and a test that needs all
// of them wired to learn a tool's class is a test nobody will keep
// working. Info() is the same method the registry calls.
func controlPlaneRepairTools(t *testing.T) map[string]domain.ToolClass {
	t.Helper()
	ctx := context.Background()
	out := map[string]domain.ToolClass{}

	for _, tool := range []struct {
		name string
		info func() (class domain.ToolClass, err error)
	}{
		{
			configchange.ToolNameDraftConfigChange,
			func() (domain.ToolClass, error) {
				info, err := configchange.NewDraftConfigChangeTool(nil, nil).Info(ctx)
				if err != nil {
					return domain.ClassUnknown, err
				}
				return declaredClassToToolClass(info.Class), nil
			},
		},
		{
			configchange.ToolNameApplyConfigChange,
			func() (domain.ToolClass, error) {
				info, err := configchange.NewApplyConfigChangeTool(nil, nil).Info(ctx)
				if err != nil {
					return domain.ClassUnknown, err
				}
				return declaredClassToToolClass(info.Class), nil
			},
		},
		{
			recovery.ToolNameRecoveryExecute,
			func() (domain.ToolClass, error) {
				info, err := recovery.NewRecoveryExecuteTool(nil, nil, nil, nil).Info(ctx)
				if err != nil {
					return domain.ClassUnknown, err
				}
				return declaredClassToToolClass(info.Class), nil
			},
		},
		{
			recovery.ToolNameVerifyRecovery,
			func() (domain.ToolClass, error) {
				info, err := recovery.NewVerifyRecoveryTool(nil, nil, nil, recovery.VerifyRecoveryConfig{}).Info(ctx)
				if err != nil {
					return domain.ClassUnknown, err
				}
				return declaredClassToToolClass(info.Class), nil
			},
		},
	} {
		class, err := tool.info()
		if err != nil {
			t.Fatalf("%s: Info: %v", tool.name, err)
		}
		out[tool.name] = class
	}
	return out
}

// declaredClassToToolClass translates a BaseTool's coarse Class hint into
// the governance vocabulary a manifest is written in.
//
// The two are not the same scale: "read" and "write" are the only values
// the BaseTool layer uses, and they map onto read and write exactly. There
// is no hint that means destructive, so nothing here can produce one —
// which is itself worth stating, because it means a control-plane tool
// cannot be L3 by accident.
func declaredClassToToolClass(class string) domain.ToolClass {
	if class == "write" {
		return domain.ClassWrite
	}
	return domain.ClassRead
}

func TestEveryControlPlaneToolTheRepairPackageOffersIsRegisteredHere(t *testing.T) {
	p, err := pluginmanifest.Load(repairProfileRoot(t))
	if err != nil {
		t.Fatalf("load the repair profile: %v", err)
	}
	declared := make(map[string]bool, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		declared[tool.Name] = true
	}

	checked := 0
	for name := range controlPlaneRepairTools(t) {
		checked++
		if !declared[name] {
			t.Errorf("%q is served by this registry but the node's repair profile does not "+
				"declare it, so the broker will refuse every call to it", name)
		}
	}
	if checked == 0 {
		t.Error("no control-plane tool was checked, so this test is vacuous")
	}
}

// TestTheRepairPackageAndThisRegistryAgreeOnEveryClass is the assertion
// that keeps the node's promise true.
//
// The node cannot derive a class for these four from its own skill
// registry — it has no executor for them, so the manifest's declaration is
// the only class the gate ever sees. That makes the YAML the sole source
// of a fact that decides whether a human is asked before a live system
// changes, and it is checked here against the tool that actually runs.
func TestTheRepairPackageAndThisRegistryAgreeOnEveryClass(t *testing.T) {
	p, err := pluginmanifest.Load(repairProfileRoot(t))
	if err != nil {
		t.Fatalf("load the repair profile: %v", err)
	}
	declared := make(map[string]domain.ToolClass, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		declared[tool.Name] = tool.Class
	}

	for name, actual := range controlPlaneRepairTools(t) {
		got, ok := declared[name]
		if !ok {
			// The other direction is covered above; repeating it here
			// would only add noise to an unrelated failure.
			continue
		}
		if got != actual {
			t.Errorf("%q is declared %q on the node and is %q here; the node's declaration is the "+
				"only class its gate can see for this tool, so a disagreement decides wrongly "+
				"whether an operator is asked before a live system changes",
				name, got, actual)
		}
	}
}

// TestTheRepairPackageCarriesNoControlPlaneToolThisSideCallsSafe pins the
// half of the above that has teeth.
//
// Every one of these four is a tool the control plane runs. A node that
// offered a mutating one as read would be telling the model — and the
// operator reading the transcript — that no approval is involved, which is
// the one thing the manifest must never get wrong.
func TestTheRepairPackageCarriesNoControlPlaneToolThisSideCallsSafe(t *testing.T) {
	p, err := pluginmanifest.Load(repairProfileRoot(t))
	if err != nil {
		t.Fatalf("load the repair profile: %v", err)
	}
	classes := make(map[string]domain.ToolClass, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		classes[tool.Name] = tool.Class
	}

	for name, actual := range controlPlaneRepairTools(t) {
		if actual != domain.ClassWrite {
			continue
		}
		if got := classes[name]; got == domain.ClassRead {
			t.Errorf("%q runs on the control plane and is mutating there, but the node's repair "+
				"profile declares it read; the gate would let it run with no approval", name)
		}
	}
}

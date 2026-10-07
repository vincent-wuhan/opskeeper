package policygate

import (
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
)

// The registry is the host's own record of what it admitted, and the broker
// reads a tool's ceiling from it rather than from the manifest. That only
// works if the two are the same object, which is what these tests pin: a
// ceiling the broker cannot see is a ceiling nobody enforces.

func manifestWith(tools ...domain.ToolDecl) domain.PluginManifest {
	return domain.PluginManifest{
		Metadata: domain.PluginMeta{Name: "opskeeper-sre-readonly", Version: "0.1.0"},
		Spec:     domain.PluginSpec{Targets: domain.Targets{"edge"}, Tools: tools},
	}
}

func TestTheDeclaredCeilingsSurviveIntoTheHostRegistry(t *testing.T) {
	reg, err := RegistryFromManifests([]domain.PluginManifest{manifestWith(
		domain.ToolDecl{Name: "host_grep_file", Class: domain.ClassRead,
			Limits: domain.ToolLimits{OutputBytes: 262144, TimeoutSeconds: 120}},
		domain.ToolDecl{Name: "host_probe_dns", Class: domain.ClassRead},
	)})
	if err != nil {
		t.Fatalf("RegistryFromManifests: %v", err)
	}

	bounded, ok := reg.Lookup("host_grep_file")
	if !ok {
		t.Fatal("the declared tool is not in the registry")
	}
	if bounded.Budget() != 262144 {
		t.Errorf("Budget = %d, want the declared 262144", bounded.Budget())
	}
	if bounded.Timeout() != 120*time.Second {
		t.Errorf("Timeout = %s, want the declared 120s", bounded.Timeout())
	}

	// A tool that declares nothing is not unbounded: the default is applied
	// at lookup, so "the package said nothing" and "nobody applied a
	// default" cannot both be true of the same binding.
	bare, ok := reg.Lookup("host_probe_dns")
	if !ok {
		t.Fatal("the undeclared-limit tool is not in the registry")
	}
	if bare.Budget() != skill.DefaultMaxOutputBytes {
		t.Errorf("an undeclared tool got %d, want the %d byte host default",
			bare.Budget(), skill.DefaultMaxOutputBytes)
	}
	// Zero means "the broker's global ceiling", not a zero-second budget.
	// The broker reads that as an absence, so the distinction has to survive
	// the trip through this type.
	if bare.Timeout() != 0 {
		t.Errorf("Timeout = %s, want 0 to mean the broker's own ceiling", bare.Timeout())
	}
}

// A package cannot make its own tool cheap by declaring a small ceiling, and
// it cannot widen the host's default upward by declaring nothing useful: the
// zero case is the default, not infinity. Both directions are the same
// property and both are worth a test, because a sign error in either is a
// silent hole.
func TestAZeroLimitIsTheHostDefaultAndNotInfinity(t *testing.T) {
	for _, declared := range []int64{0, -1, 1} {
		binding := ToolBinding{Limits: domain.ToolLimits{OutputBytes: declared}}
		got := binding.Budget()
		switch declared {
		case 1:
			if got != 1 {
				t.Errorf("declared %d, Budget = %d; a positive declaration is the tool's own", declared, got)
			}
		default:
			if got != skill.DefaultMaxOutputBytes {
				t.Errorf("declared %d, Budget = %d; want the host default", declared, got)
			}
		}
	}
}

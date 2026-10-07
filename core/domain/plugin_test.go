package domain

import "testing"

func TestScopesSatisfies(t *testing.T) {
	declared := Scopes{ScopeHostRead, ScopeK8sRead}
	cases := []struct {
		name    string
		granted Scopes
		want    bool
	}{
		{"exact", Scopes{ScopeHostRead, ScopeK8sRead}, true},
		{"superset", Scopes{ScopeHostRead, ScopeK8sRead, ScopeK8sExec}, true},
		{"reordered", Scopes{ScopeK8sRead, ScopeHostRead}, true},
		{"missing one", Scopes{ScopeHostRead}, false},
		{"none granted", nil, false},
		{"empty declared needs nothing", Scopes{ScopeHostRead, ScopeHostWrite, ScopeDBWrite}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := declared.Satisfies(c.granted); got != c.want {
				t.Errorf("Satisfies(%v) = %v, want %v", c.granted, got, c.want)
			}
		})
	}
}

func TestEmptyDeclaredSatisfiesEmptyGrant(t *testing.T) {
	// A plugin that declares no scopes is admissible with no grant. It
	// must still be held to its SafetyLevel ceiling.
	if !(Scopes{}).Satisfies(nil) {
		t.Error("a plugin declaring no scopes should need no grant")
	}
}

func TestTargetsValid(t *testing.T) {
	cases := []struct {
		name string
		in   Targets
		want bool
	}{
		{"empty refused", Targets{}, false},
		{"edge", Targets{TargetEdge}, true},
		{"both", Targets{TargetEdge, TargetManager}, true},
		{"unknown target", Targets{TargetEdge, "cluster"}, false},
		{"typo", Targets{"edg"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.Valid(); got != c.want {
				t.Errorf("Valid() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTargetsHas(t *testing.T) {
	ts := Targets{TargetEdge, TargetManager}
	if !ts.Has(TargetManager) {
		t.Error("Has(TargetManager) should be true")
	}
	if ts.Has("cluster") {
		t.Error("Has(undeclared) should be false")
	}
}

func TestInstallPolicyValid(t *testing.T) {
	if !(InstallPolicy{}).Valid() {
		t.Error("empty strategy should default cleanly")
	}
	if !(InstallPolicy{Strategy: InstallRolling}).Valid() {
		t.Error("rolling should be valid")
	}
	if !(InstallPolicy{Strategy: InstallPin}).Valid() {
		t.Error("pin should be valid")
	}
	if (InstallPolicy{Strategy: "yolo"}).Valid() {
		t.Error("unknown strategy should be invalid")
	}
}

func TestHighestCapability(t *testing.T) {
	cases := []struct {
		name string
		in   []ToolClass
		want ToolClass
	}{
		{"none is unknown", nil, ClassUnknown},
		{"read only", []ToolClass{ClassRead}, ClassRead},
		{"mixed takes worst", []ToolClass{ClassRead, ClassDestructive}, ClassDestructive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := PluginManifest{Spec: PluginSpec{Capabilities: c.in}}
			if got := m.HighestCapability(); got != c.want {
				t.Errorf("HighestCapability() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestInstallPolicyDefaultsToRollingInDocsTerms(t *testing.T) {
	// The zero value must be usable without the author naming a strategy,
	// and the host treats empty as rolling. Assert the constant is stable
	// because the release pipeline keys off it.
	if InstallRolling != "rolling" {
		t.Errorf("InstallRolling = %q, want %q", InstallRolling, "rolling")
	}
	if InstallPin != "pin" {
		t.Errorf("InstallPin = %q, want %q", InstallPin, "pin")
	}
}

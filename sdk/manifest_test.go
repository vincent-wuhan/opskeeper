package sdk

import (
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

const goodPlugin = `
apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: diagnose-postgres
  version: 0.1.0
  vendor: opskeeper
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  required_scopes: [db.read]
  audit:
    emits: true
    mutates: false
  approval:
    required: false
  install:
    strategy: rolling
    min_edge_version: 0.7.0
`

func TestDecodeAcceptsWellFormedPlugin(t *testing.T) {
	m, err := Decode([]byte(goodPlugin))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if m.Metadata.Name != "diagnose-postgres" {
		t.Errorf("name = %q", m.Metadata.Name)
	}
	if m.Spec.SafetyLevel != domain.SafetyL1 {
		t.Errorf("safety_level = %q", m.Spec.SafetyLevel)
	}
	if !m.Spec.Targets.Has(domain.TargetEdge) {
		t.Error("edge target should be present")
	}
	if m.HighestCapability() != domain.ClassRead {
		t.Errorf("capability = %q", m.HighestCapability())
	}
}

func TestDecodeRejectsWrongAPIVersion(t *testing.T) {
	_, err := Decode([]byte(strings.Replace(goodPlugin, "opskeeper.io/v1", "opskeeper.io/v2", 1)))
	assertField(t, err, "apiVersion")
}

func TestDecodeRejectsWrongKind(t *testing.T) {
	_, err := Decode([]byte(strings.Replace(goodPlugin, "kind: Plugin", "kind: Deployment", 1)))
	assertField(t, err, "kind")
}

func TestDecodeRejectsMissingName(t *testing.T) {
	_, err := Decode([]byte(strings.Replace(goodPlugin, "  name: diagnose-postgres\n", "", 1)))
	assertField(t, err, "metadata.name")
}

func TestDecodeRejectsMissingVersion(t *testing.T) {
	_, err := Decode([]byte(strings.Replace(goodPlugin, "  version: 0.1.0\n", "", 1)))
	assertField(t, err, "metadata.version")
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	// A misspelled key must not silently become "declares no scopes",
	// which the host reads as "needs no grant".
	bad := strings.Replace(goodPlugin, "required_scopes:", "required_scope:", 1)
	_, err := Decode([]byte(bad))
	if err == nil {
		t.Fatal("a misspelled key must be an error, not a silently ignored setting")
	}
	if !strings.Contains(err.Error(), "required_scope") {
		t.Errorf("error should name the offending key, got %v", err)
	}
}

func TestDecodeRejectsEmptyFile(t *testing.T) {
	_, err := Decode(nil)
	if err == nil {
		t.Fatal("an empty manifest must be refused")
	}
}

func TestDecodeRejectsNoTargets(t *testing.T) {
	bad := strings.Replace(goodPlugin, "  targets: [edge]\n", "", 1)
	_, err := Decode([]byte(bad))
	assertField(t, err, "spec.targets")
}

func TestDecodeRejectsUnknownTarget(t *testing.T) {
	bad := strings.Replace(goodPlugin, "targets: [edge]", "targets: [edge, galaxy]", 1)
	_, err := Decode([]byte(bad))
	assertField(t, err, "spec.targets")
}

func TestDecodeRejectsUnknownSafetyLevel(t *testing.T) {
	bad := strings.Replace(goodPlugin, "safety_level: L1", "safety_level: L9", 1)
	_, err := Decode([]byte(bad))
	assertField(t, err, "spec.safety_level")
}

func TestDecodeRefusesAuditMutation(t *testing.T) {
	// Audit-ledger writes are host-only. A plugin asking for them is
	// refused outright, not downgraded.
	bad := strings.Replace(goodPlugin, "mutates: false", "mutates: true", 1)
	_, err := Decode([]byte(bad))
	assertField(t, err, "spec.audit.mutates")
}

func TestDecodeRefusesCapabilityBeyondDeclaredLevel(t *testing.T) {
	// L1 permits read only. A destructive capability under an L1 label is
	// a manifest that lies about itself, and is refused rather than
	// clamped.
	bad := strings.Replace(goodPlugin, "capabilities: [read]", "capabilities: [read, destructive]", 1)
	_, err := Decode([]byte(bad))
	assertField(t, err, "spec.capabilities")
}

func TestDecodeAllowsWriteUnderL2(t *testing.T) {
	ok := strings.NewReplacer(
		"safety_level: L1", "safety_level: L2",
		"capabilities: [read]", "capabilities: [read, write]",
		"required: false", "required: true",
	).Replace(goodPlugin)
	if _, err := Decode([]byte(ok)); err != nil {
		t.Errorf("write under L2 with approval should be accepted, got %v", err)
	}
}

func TestDecodeAllowsDestructiveUnderL3(t *testing.T) {
	ok := strings.NewReplacer(
		"safety_level: L1", "safety_level: L3",
		"capabilities: [read]", "capabilities: [read, destructive]",
		"required: false", "required: true",
	).Replace(goodPlugin)
	if _, err := Decode([]byte(ok)); err != nil {
		t.Errorf("destructive under L3 with approval should be accepted, got %v", err)
	}
}

func TestDecodeRefusesBlastRadiusOnReadOnlyPlugin(t *testing.T) {
	// A read-only plugin carrying a mutation radius is misreading the
	// field; accepting it would put a radius in the record for a package
	// that cannot mutate.
	bad := strings.NewReplacer(
		"required: false", "required: false\n    max_blast_radius: pod",
	).Replace(goodPlugin)
	_, err := Decode([]byte(bad))
	assertField(t, err, "spec.approval.max_blast_radius")
}

func TestDecodeRejectsUnknownInstallStrategy(t *testing.T) {
	bad := strings.Replace(goodPlugin, "strategy: rolling", "strategy: whenever", 1)
	_, err := Decode([]byte(bad))
	assertField(t, err, "spec.install.strategy")
}

func TestAdmitRefusesMissingScope(t *testing.T) {
	m, err := Decode([]byte(goodPlugin))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	err = Admit(m, Admission{
		GrantedScopes:  domain.Scopes{domain.ScopeHostRead},
		MaxSafetyLevel: domain.SafetyL3,
		MaxBlastRadius: domain.RadiusCluster,
	})
	if err == nil {
		t.Fatal("a missing scope must be refused")
	}
	if !strings.Contains(err.Error(), "db.read") {
		t.Errorf("error should name the missing scope, got %v", err)
	}
}

func TestAdmitRefusesWhenGrantIsASubsetOfRequired(t *testing.T) {
	// Regression: Satisfies asks "does the argument cover every scope the
	// receiver declares". Calling it as granted.Satisfies(required)
	// inverts the check into "is every granted scope also required", which
	// would admit this plugin even though k8s.exec was never granted.
	m := domain.PluginManifest{
		APIVersion: domain.PluginAPIVersion, Kind: domain.PluginKind,
		Metadata: domain.PluginMeta{Name: "x", Version: "1"},
		Spec: domain.PluginSpec{
			Targets:        domain.Targets{domain.TargetEdge},
			SafetyLevel:    domain.SafetyL1,
			RequiredScopes: domain.Scopes{domain.ScopeHostRead, domain.ScopeK8sExec},
		},
	}
	err := Admit(m, Admission{
		GrantedScopes:  domain.Scopes{domain.ScopeHostRead},
		MaxSafetyLevel: domain.SafetyL3,
		MaxBlastRadius: domain.RadiusCluster,
	})
	if err == nil {
		t.Fatal("a grant covering only some required scopes must be refused")
	}
	if !strings.Contains(err.Error(), "k8s.exec") {
		t.Errorf("error should name the missing scope, got %v", err)
	}
}

func TestAdmitAcceptsExactGrant(t *testing.T) {
	m, _ := Decode([]byte(goodPlugin))
	err := Admit(m, Admission{
		GrantedScopes:  domain.Scopes{domain.ScopeDBRead, domain.ScopeHostRead},
		MaxSafetyLevel: domain.SafetyL3,
		MaxBlastRadius: domain.RadiusCluster,
	})
	if err != nil {
		t.Errorf("a superset grant should be accepted, got %v", err)
	}
}

func TestAdmitRefusesPluginAboveNodeCeiling(t *testing.T) {
	// A node that cannot host an L3 plugin must not be handed one, even
	// if every scope is granted.
	m := domain.PluginManifest{
		APIVersion: domain.PluginAPIVersion, Kind: domain.PluginKind,
		Metadata: domain.PluginMeta{Name: "x", Version: "1"},
		Spec:     domain.PluginSpec{Targets: domain.Targets{domain.TargetEdge}, SafetyLevel: domain.SafetyL3},
	}
	err := Admit(m, Admission{
		GrantedScopes:  domain.Scopes{domain.ScopeK8sExec},
		MaxSafetyLevel: domain.SafetyL1,
		MaxBlastRadius: domain.RadiusCluster,
	})
	if err == nil {
		t.Fatal("a plugin above the node ceiling must be refused")
	}
}

func TestAdmitZeroValueRefusesEverything(t *testing.T) {
	// A host that forgot to populate Admission must deny, not allow.
	m := domain.PluginManifest{
		APIVersion: domain.PluginAPIVersion, Kind: domain.PluginKind,
		Metadata: domain.PluginMeta{Name: "x", Version: "1"},
		Spec:     domain.PluginSpec{Targets: domain.Targets{domain.TargetEdge}, SafetyLevel: domain.SafetyL0},
	}
	if err := Admit(m, Admission{}); err == nil {
		t.Error("a zero-value Admission must refuse")
	}
}

func TestAdmitRefusesOverWideBlastRadius(t *testing.T) {
	m := domain.PluginManifest{
		APIVersion: domain.PluginAPIVersion, Kind: domain.PluginKind,
		Metadata: domain.PluginMeta{Name: "x", Version: "1"},
		Spec: domain.PluginSpec{
			Targets:     domain.Targets{domain.TargetEdge},
			SafetyLevel: domain.SafetyL3,
			Approval:    domain.ApprovalPolicy{Required: true, MaxBlastRadius: domain.RadiusCluster},
		},
	}
	err := Admit(m, Admission{
		GrantedScopes:  nil,
		MaxSafetyLevel: domain.SafetyL3,
		MaxBlastRadius: domain.RadiusPod,
	})
	if err == nil {
		t.Fatal("a plugin wider than the node's radius ceiling must be refused")
	}
}

func TestLoadErrorIsUnwrappable(t *testing.T) {
	sentinel := errors.New("boom")
	le := &LoadError{Field: "spec.x", Reason: "bad", Err: sentinel}
	if !errors.Is(le, sentinel) {
		t.Error("LoadError should unwrap to its cause")
	}
	if !strings.Contains(le.Error(), "spec.x") {
		t.Errorf("Error() should name the field, got %q", le.Error())
	}
}

func assertField(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error for field %s", want)
	}
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("expected a *LoadError, got %T", err)
	}
	if le.Field != want {
		t.Errorf("Field = %q, want %q (err: %v)", le.Field, want, err)
	}
}

// The limits travel in the manifest because the manifest is the reviewed
// artefact: a ceiling in code is invisible to whoever reads the package, and
// a ceiling in the manifest that the host ignores is worse than neither.
func TestDecodeCarriesTheDeclaredLimits(t *testing.T) {
	withLimits := strings.Replace(goodPlugin,
		"  install:\n",
		"  tools:\n"+
			"    - {name: host_grep_file, class: read, limits: {output_bytes: 262144, timeout_seconds: 120}}\n"+
			"  install:\n", 1)

	m, err := Decode([]byte(withLimits))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(m.Spec.Tools) != 1 {
		t.Fatalf("decoded %d tools, want 1", len(m.Spec.Tools))
	}
	if got := m.Spec.Tools[0].Limits; got.OutputBytes != 262144 || got.TimeoutSeconds != 120 {
		t.Errorf("limits decoded as %+v, want output_bytes=262144 timeout_seconds=120", got)
	}
}

// A negative ceiling is refused at load, which is the only moment before the
// node is running that a package can be told its manifest is wrong.
func TestDecodeRefusesANegativeLimit(t *testing.T) {
	for _, field := range []string{"output_bytes: -1", "timeout_seconds: -5"} {
		withLimits := strings.Replace(goodPlugin,
			"  install:\n",
			"  tools:\n    - {name: host_grep_file, class: read, limits: {"+field+"}}\n  install:\n", 1)

		_, err := Decode([]byte(withLimits))
		if err == nil {
			t.Errorf("%s was accepted; a limit is never negative", field)
			continue
		}
		if !strings.Contains(err.Error(), "never negative") {
			t.Errorf("%s: the refusal does not say what is wrong: %v", field, err)
		}
	}
}

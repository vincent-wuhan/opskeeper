package sdk

import (
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

func testManifest(tools ...domain.ToolDecl) domain.PluginManifest {
	return domain.PluginManifest{
		APIVersion: domain.PluginAPIVersion,
		Kind:       domain.PluginKind,
		Metadata:   domain.PluginMeta{Name: "x", Version: "1"},
		Spec: domain.PluginSpec{
			Targets:     domain.Targets{domain.TargetEdge},
			SafetyLevel: domain.SafetyL1,
			Capabilities: []domain.ToolClass{
				domain.ClassRead,
			},
			Tools: tools,
		},
	}
}

func TestRegistryCheckAcceptsAMatchingPair(t *testing.T) {
	r := NewRegistry("x")
	r.MustRegister("a", domain.ClassRead)
	r.MustRegister("b", domain.ClassRead)
	m := testManifest(
		domain.ToolDecl{Name: "a", Class: domain.ClassRead},
		domain.ToolDecl{Name: "b", Class: domain.ClassRead},
	)
	if err := r.Check(m); err != nil {
		t.Fatalf("an exact match must pass, got %v", err)
	}
}

// TestAnUndeclaredToolIsReported is the failure that looks like a broken
// tool at run time: the model is offered a tool the gate refuses.
func TestAnUndeclaredToolIsReported(t *testing.T) {
	r := NewRegistry("x")
	r.MustRegister("a", domain.ClassRead)
	r.MustRegister("surprise", domain.ClassRead)
	err := r.Check(testManifest(domain.ToolDecl{Name: "a", Class: domain.ClassRead}))
	if err == nil {
		t.Fatal("an undeclared tool must be reported")
	}
	if !strings.Contains(err.Error(), "surprise") {
		t.Errorf("the message should name the tool, got %q", err)
	}
	if !strings.Contains(err.Error(), "refuse every call") {
		t.Errorf("the message should say what happens at run time, got %q", err)
	}
}

// TestAnUnregisteredDeclaredToolIsReported is the other direction: the
// review approved a capability that no code path can reach.
func TestAnUnregisteredDeclaredToolIsReported(t *testing.T) {
	r := NewRegistry("x")
	r.MustRegister("a", domain.ClassRead)
	err := r.Check(testManifest(
		domain.ToolDecl{Name: "a", Class: domain.ClassRead},
		domain.ToolDecl{Name: "ghost", Class: domain.ClassRead},
	))
	if err == nil {
		t.Fatal("a declared but unregistered tool must be reported")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("the message should name the tool, got %q", err)
	}
	if !strings.Contains(err.Error(), "cannot be called") {
		t.Errorf("the message should say the capability does not exist, got %q", err)
	}
}

// TestAClassDisagreementIsReported is the one with teeth: the manifest's
// class is what the gate enforces, so a disagreement decides whether a
// human is asked.
func TestAClassDisagreementIsReported(t *testing.T) {
	r := NewRegistry("x")
	r.MustRegister("dangerous", domain.ClassRead)
	err := r.Check(testManifest(domain.ToolDecl{Name: "dangerous", Class: domain.ClassDestructive}))
	if err == nil {
		t.Fatal("a class disagreement must be reported")
	}
	if !strings.Contains(err.Error(), "destructive") || !strings.Contains(err.Error(), "read") {
		t.Errorf("the message should name both classes, got %q", err)
	}
}

func TestCheckReportsEveryFailureAtOnce(t *testing.T) {
	r := NewRegistry("x")
	r.MustRegister("undeclared", domain.ClassRead)
	err := r.Check(testManifest(
		domain.ToolDecl{Name: "ghost", Class: domain.ClassRead},
		domain.ToolDecl{Name: "undeclared", Class: domain.ClassWrite},
	))
	if err == nil {
		t.Fatal("expected failures")
	}
	msg := err.Error()
	for _, want := range []string{"undeclared", "ghost"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report should name %s even alongside the other failure; got:\n%s", want, msg)
		}
	}
}

// TestAnUnclassifiedToolIsRefused pins that the SDK does not default the
// zero value to safe. The host's Classify treats ClassUnknown as
// destructive; a registry that promoted it to read would make the author's
// own check disagree with the node's.
func TestAnUnclassifiedToolIsRefused(t *testing.T) {
	r := NewRegistry("x")
	err := r.Register("mystery", domain.ClassUnknown)
	if err == nil {
		t.Fatal("an unclassified tool must be refused rather than defaulted")
	}
	if !strings.Contains(err.Error(), "read, write or destructive") {
		t.Errorf("the message should say what to do, got %q", err)
	}
}

func TestADuplicateRegistrationIsRefused(t *testing.T) {
	r := NewRegistry("x")
	r.MustRegister("a", domain.ClassRead)
	if err := r.Register("a", domain.ClassRead); err == nil {
		t.Fatal("a duplicate must be refused: two classes for one name is a silent choice")
	}
}

func TestDeclaredManifestIsSortedAndComplete(t *testing.T) {
	r := NewRegistry("x")
	r.MustRegister("z", domain.ClassWrite)
	r.MustRegister("a", domain.ClassRead)
	m := r.DeclaredManifest(testManifest())
	got := m.Spec.Tools
	if len(got) != 2 {
		t.Fatalf("got %d declarations, want 2", len(got))
	}
	if got[0].Name != "a" || got[1].Name != "z" {
		t.Errorf("declarations must be sorted for review, got %+v", got)
	}
	// The generator must leave the operator's judgement alone.
	if len(m.Spec.Capabilities) != 1 {
		t.Errorf("DeclaredManifest invented capabilities: %+v", m.Spec.Capabilities)
	}
}

func TestHighestClassTakesTheWorst(t *testing.T) {
	r := NewRegistry("x")
	r.MustRegister("a", domain.ClassRead)
	r.MustRegister("b", domain.ClassDestructive)
	if got := r.HighestClass(); got != domain.ClassDestructive {
		t.Errorf("HighestClass = %q, want destructive", got)
	}
}

func TestAnEmptyRegistryReportsEveryDeclaredTool(t *testing.T) {
	r := NewRegistry("x")
	err := r.Check(testManifest(domain.ToolDecl{Name: "a", Class: domain.ClassRead}))
	if err == nil {
		t.Fatal("a package that registers nothing must not pass a manifest that declares tools")
	}
	if !strings.Contains(err.Error(), "a") {
		t.Errorf("the message should name the tool, got %q", err)
	}
}

// A ceiling that exists in only one of the two files is the failure this
// check exists to prevent. The manifest is what the host enforces and the
// code is what runs, so a tool written against a 64 MiB reply and declared
// at 1 MiB produces truncation notices nobody wrote code for — and a tool
// declared at 64 MiB with an implementation that cannot produce that much
// is a promise about a tool that does not exist.
func TestALimitDisagreementIsReported(t *testing.T) {
	r := NewRegistry("x")
	if err := r.RegisterWithLimits("host_grep_file", domain.ClassRead,
		domain.ToolLimits{OutputBytes: 1 << 20, TimeoutSeconds: 120}); err != nil {
		t.Fatalf("RegisterWithLimits: %v", err)
	}

	matching := testManifest(domain.ToolDecl{
		Name: "host_grep_file", Class: domain.ClassRead,
		Limits: domain.ToolLimits{OutputBytes: 1 << 20, TimeoutSeconds: 120},
	})
	if err := r.Check(matching); err != nil {
		t.Fatalf("matching limits must pass, got %v", err)
	}

	for _, tc := range []struct {
		name     string
		declared domain.ToolLimits
	}{
		{"output ceiling", domain.ToolLimits{OutputBytes: 4 << 20, TimeoutSeconds: 120}},
		{"wall clock", domain.ToolLimits{OutputBytes: 1 << 20, TimeoutSeconds: 30}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManifest(domain.ToolDecl{
				Name: "host_grep_file", Class: domain.ClassRead, Limits: tc.declared,
			})
			err := r.Check(m)
			if err == nil {
				t.Fatal("a ceiling that exists in only one file was accepted")
			}
			if !strings.Contains(err.Error(), "the host enforces") {
				t.Errorf("the report does not say which side wins: %v", err)
			}
		})
	}
}

// Registering is the author's first declaration, and a negative ceiling there
// is caught before the manifest is ever written. Letting it through would
// mean the error surfaces in a file the author generates from this one.
func TestANegativeCeilingIsRefusedAtRegistration(t *testing.T) {
	r := NewRegistry("x")
	err := r.RegisterWithLimits("host_grep_file", domain.ClassRead,
		domain.ToolLimits{OutputBytes: -1})
	if err == nil {
		t.Fatal("a negative output ceiling was registered")
	}
	if !strings.Contains(err.Error(), "never negative") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
	if r.Len() != 0 {
		t.Error("the refused tool was added to the inventory anyway")
	}
}

// DeclaredManifest is the generator half: the limits an author registered
// have to survive into the YAML they paste, or the pair Check compares is
// not the pair they registered.
func TestDeclaredManifestCarriesTheRegisteredCeilings(t *testing.T) {
	r := NewRegistry("x")
	r.MustRegister("a", domain.ClassRead)
	if err := r.RegisterWithLimits("b", domain.ClassRead,
		domain.ToolLimits{OutputBytes: 262144, TimeoutSeconds: 90}); err != nil {
		t.Fatalf("RegisterWithLimits: %v", err)
	}

	generated := r.DeclaredManifest(testManifest())
	if err := r.Check(generated); err != nil {
		t.Fatalf("a manifest generated from the registry must agree with it: %v", err)
	}
	for _, tool := range generated.Spec.Tools {
		if tool.Name != "b" {
			continue
		}
		if tool.Limits.OutputBytes != 262144 || tool.Limits.TimeoutSeconds != 90 {
			t.Errorf("the generated declaration carries %+v, want the registered ceilings", tool.Limits)
		}
		return
	}
	t.Fatal("the generated manifest does not declare b at all")
}

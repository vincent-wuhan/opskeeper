package pluginmanifest

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// The middleware profile is the third read package and the one with the
// widest surface: fifty-odd tools across six systems, each name reaching a
// live database, a cluster or a broker.
//
// That width is exactly why the assertions here exist, and why they are
// mostly about agreement rather than about the tools working. The manager's
// adapter registry is the authority on whether a tool functions;
// core/manager/middleware/toolset's TestToolsetMatchesTheAdapters is what
// keeps the generated schemas honest. What is asserted here is narrower:
// that what this manifest permits is what the package actually offers, and
// that a package reaching into production data systems was not quietly
// promoted past the level that reach implies.
//
// The middle name in the pair — the observability package in
// observability_profile_test.go — makes the same promises about a narrower
// surface. The pairing is the point. A read of an already-deployed
// database is a different permission from a read of the fleet's own
// recordings, and a boundary that erodes between the two destroys exactly
// one of them without anybody noticing.

func middlewareRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "plugins", "pig-ops", middlewareProfile)
}

func loadMiddlewareProfile(t *testing.T) Plugin {
	t.Helper()
	p, err := Load(middlewareRoot(t))
	if err != nil {
		t.Fatalf("Load %s: %v", middlewareProfile, err)
	}
	return p
}

// TestTheMiddlewareProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel is
// the two-way agreement between the allow-list and the menu.
//
// This test is the one the package's own manifest names, and it did not
// exist until now — the manifest claimed a guard that was not installed,
// which is the failure mode a comment is least able to report. It is also
// the guard that matters most here, because the middleware package is the
// largest in the fleet: a name in the toolset but not the manifest is
// visible to the model and refused by the gate on every single call, so the
// model keeps trying an answer that is structurally impossible, and a name
// in the manifest but not the toolset is permitted and silently
// unreachable.
//
// Neither is an error anywhere. Both are found during an incident, by an
// operator, as a tool that "does nothing".
func TestTheMiddlewareProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel(t *testing.T) {
	p := loadMiddlewareProfile(t)
	declared := make(map[string]bool, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		declared[tool.Name] = true
	}

	shipped := readShippedToolNamesFor(t, middlewareRoot(t), middlewareProfile)
	if len(shipped) == 0 {
		t.Fatal("no tool names could be read out of the shipped toolset")
	}
	for name := range shipped {
		if !declared[name] {
			t.Errorf("the toolset offers %q but the manifest does not declare it, so the gate refuses every call to it", name)
		}
	}
	for name := range declared {
		if !shipped[name] {
			t.Errorf("the manifest declares %q but the toolset does not offer it, so it is permitted and unreachable", name)
		}
	}
}

func TestTheMiddlewareProfileIsL1AndNeedsNoApproval(t *testing.T) {
	// Every tool here is a read whose success state changes nothing. A
	// package that opens a connection to production and can be promoted to
	// L2 by a later edit is the shape of mistake this level exists to
	// catch, and it is caught by asserting the ceiling rather than reading
	// it back from the file the change would have edited.
	p := loadMiddlewareProfile(t)
	if got := p.Manifest.Spec.SafetyLevel; got != domain.SafetyL1 {
		t.Errorf("safety_level = %q, want L1", got)
	}
	if p.Manifest.Spec.Approval.Required {
		t.Error("approval.required is set on a package whose tools are all reads; " +
			"a read must never enter the approval queue, and asking puts every " +
			"middleware question behind a human for no safety gain")
	}
	if r := p.Manifest.Spec.Approval.MaxBlastRadius; r != "" {
		t.Errorf("approval.max_blast_radius = %q, want empty: a package that cannot mutate has no blast", r)
	}
	// Admit is the same refusal the host performs, run here so a manifest
	// that would be rejected on a real node fails in this repository.
	if err := sdk.Admit(p.Manifest, sdk.Admission{
		GrantedScopes:  p.Manifest.Spec.RequiredScopes,
		MaxSafetyLevel: domain.SafetyL1,
	}); err != nil {
		t.Errorf("the middleware package does not install on a read-only node: %v", err)
	}
}

func TestEveryToolInTheMiddlewareProfileIsReadOnly(t *testing.T) {
	// The declared class is a claim; the host re-derives the real one from
	// the adapter that implements the tool and refuses anything worse. That
	// is what makes understating a class safe. It does not make a
	// mislabelled write safe in the other direction, which is why this
	// asserts the manifest rather than trusting the re-derivation: the file
	// is what a reviewer reads, and a reviewer reading "read" on a tool
	// that evicts pods has been told something false.
	p := loadMiddlewareProfile(t)
	for _, tool := range p.Manifest.Spec.Tools {
		if tool.Class != domain.ClassRead {
			t.Errorf("tool %q is declared %q; this package is the read surface over production "+
				"data systems and a write here belongs in the repair package, where it is gated",
				tool.Name, tool.Class)
		}
	}
	if got := p.HighestCapability(); got != domain.ClassRead {
		t.Errorf("highest capability = %q, want read", got)
	}
	for _, c := range p.Manifest.Spec.Capabilities {
		if c != domain.ClassRead {
			t.Errorf("spec.capabilities contains %q; this package declares only read", c)
		}
	}
}

func TestTheMiddlewareProfileAsksForReadScopesAndNothingWider(t *testing.T) {
	// A scope is how a package is handed a credential. db.read, k8s.read
	// and mq.read are the three this package's tools need and no more; a
	// write scope here would be a credential nothing in the package can
	// use, granted anyway, and a credential in a manifest is one somebody
	// has to approve without being able to see the call it enables.
	p := loadMiddlewareProfile(t)
	want := map[string]bool{"db.read": true, "k8s.read": true, "mq.read": true}
	for _, s := range p.Manifest.Spec.RequiredScopes {
		if !want[string(s)] {
			t.Errorf("required scope %q is wider than this package's tools need (%v)", s, want)
			continue
		}
		delete(want, string(s))
	}
	if len(want) > 0 {
		t.Errorf("the package needs scopes it does not declare: %v", want)
	}
}

func TestTheMiddlewareProfileNeverClaimsToWriteTheAuditLedger(t *testing.T) {
	// The ledger is a host-only hash chain. A package that could write it
	// could remove its own footprints, so this is refused at load rather
	// than downgraded — and asserting it here means the refusal is
	// understood rather than merely present.
	p := loadMiddlewareProfile(t)
	if p.Manifest.Spec.Audit.Mutates {
		t.Error("audit.mutates is true: ledger writes are host-only and a package asking for them is refused at load")
	}
	if !p.Manifest.Spec.Audit.Emits {
		t.Error("audit.emits is false; the package runs production reads and must report them")
	}
}

func TestTheMiddlewareProfileTargetsTheEdgeAndNotTheControlPlane(t *testing.T) {
	// A package that reaches a production database runs where the
	// connection is and under the node's policy. Allowing it on the control
	// plane would put a DSN-holding extension next to the audit ledger and
	// the approval queue, which is the one place in the system that must
	// not be reachable from a plugin.
	p := loadMiddlewareProfile(t)
	if !p.RunsOn(domain.TargetEdge) {
		t.Error("the package does not target edge, so a node's agent cannot use it")
	}
	if p.RunsOn(domain.TargetManager) {
		t.Error("the package targets manager: a DSN-holding extension must not be installable next to the audit ledger")
	}
}

func TestTheMiddlewareProfilesToolListIsSortedAndUnique(t *testing.T) {
	// Fifty-three names is past the length where a person can check the
	// list by reading it. Sorted order is what turns a review of this file
	// into a diff rather than a reading, and uniqueness is what keeps a
	// duplicated name from quietly widening a class aggregate.
	p := loadMiddlewareProfile(t)
	names := make([]string, 0, len(p.Manifest.Spec.Tools))
	for _, tool := range p.Manifest.Spec.Tools {
		names = append(names, tool.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("spec.tools is not sorted, so a review of this file is a reading rather than a diff:\n  %s",
			strings.Join(names, "\n  "))
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			t.Errorf("tool %q is declared twice", n)
		}
		seen[n] = true
	}
}

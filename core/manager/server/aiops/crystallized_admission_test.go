// crystallized_admission_test.go — the last hop of the crystallisation path.
//
// The chain is: the loop verifies a fix cleanly N times → the ledger promotes
// the pattern → the review surface renders a draft → an operator writes it
// for review. The step after that is admission, and it is the one place the
// whole path can be silently broken: a draft that a human sends for review
// but that the control plane's own loader refuses is a dead end that reads as
// progress.
//
// So this test does not check that DraftFor produced *a* manifest. It writes
// the draft exactly the way the promote route does, then hands the directory
// to pluginmanifest.LoadAll — the same call the plugin coverage eval and the
// import route use to admit a package — and requires it to come back. If the
// crystalliser ever emits something that fails admission, this fails here
// rather than at install time on a node.
package aiops

import (
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

func TestPromotedDraftIsAdmissibleByTheControlPlaneLoader(t *testing.T) {
	t.Parallel()
	ledger := promotedLedger(t)
	root := t.TempDir()

	// Write every promoted pattern the way the promote route does; a review
	// root holds drafts, not a single file.
	for _, run := range ledger.Promoted() {
		draft, err := ledger.DraftFor(run)
		if err != nil {
			t.Fatalf("DraftFor: %v", err)
		}
		if _, err := draft.Write(root); err != nil {
			t.Fatalf("write draft %s: %v", draft.Name(), err)
		}
	}

	plugins, err := pluginmanifest.LoadAll(root)
	if err != nil {
		t.Fatalf("the control plane's loader refused the promoted draft: %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("admitted %d packages, want 1", len(plugins))
	}
	p := plugins[0]
	if p.Manifest.Spec.Autonomy.Actions == nil || len(p.Manifest.Spec.Autonomy.Actions) == 0 {
		t.Fatalf("admitted package carries no autonomy action: %+v", p.Manifest.Spec)
	}
	got := p.Manifest.Spec.Autonomy.Actions[0].Argv
	want := []string{"systemctl", "restart", "orders-api"}
	if len(got) != len(want) {
		t.Fatalf("admitted argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("admitted argv = %v, want %v", got, want)
		}
	}
	// The declaration must not be a read: an autonomy action may not drive
	// one, and admission is where that is enforced.
	if p.Manifest.Spec.SafetyLevel == "" {
		t.Fatalf("admitted package has no safety level")
	}
}

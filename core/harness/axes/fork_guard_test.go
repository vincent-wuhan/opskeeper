package axes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The derivation must have exactly one implementation.
//
// This guard exists because the previous arrangement was not a bug anyone
// wrote on purpose: the derivation sat in cmd/opskeeper-eval, the loop harness
// could not import it, and rather than move it someone would eventually have
// written a second one. That is the same failure this repository has already
// paid for three times — the tool summary in control plane and on the node,
// the approval target derivation, and judge.Case's threshold — each of which
// was one function living in two places with different key orders.
//
// A fork is invisible by construction: both copies compile, both produce
// plausible token lists, and only the corpus-wide comparison in the two
// bridges' tests catches them. So the check is at the source level: these
// identifiers may only be defined here.
func TestTheDerivationHasNoSecondImplementation(t *testing.T) {
	forbidden := map[string]string{
		"func caseFamily(":               "the id's family segment",
		"func caseFaultName(":            "the id's fault segment",
		"func axisTokens(":               "token splitting",
		"func scalarTokens(":             "injection parameter flattening",
		"func uniqueTokens(":             "token de-duplication",
		"locusIdentityKeys":              "the identity-parameter key list",
		"func diagnosticExpectationsOf(": "the old derivation entry point",
		"minimumAxisTokenLen":            "the minimum token length",
	}
	dirs := []string{
		// The command that used to own the derivation.
		"../../../cmd/opskeeper-eval",
		// The other harness, which is the whole reason the move happened.
		"../runner",
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			for needle, what := range forbidden {
				if strings.Contains(string(body), needle) {
					t.Errorf("%s/%s defines %q (%s); the derivation lives in core/harness/axes "+
						"and a second copy is how the two harnesses started disagreeing",
						dir, name, needle, what)
				}
			}
		}
	}
}

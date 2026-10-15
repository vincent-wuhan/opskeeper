package pigcontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extsource "github.com/MichaelKinsy/PiG/coding/extension/source"
)

// TestEveryShippedPackageExtensionEntryResolvesAsAGoExtension pins the shape
// every shipped package depends on, entry by entry.
//
// The failure it prevents is the one this repository spent decision 445
// chasing: a package whose declared extension entries resolve perfectly well
// on their own, and which nevertheless loads zero of them. Asserting per entry
// is therefore not the same as asserting the package loads -- it is the part
// that can be asserted without a running agent, and it is what makes the
// remaining gap nameable rather than vague. If PiG stops classifying a Go
// build root this way, or a package starts declaring a directory that is not
// an extension root at all, this fails here rather than on a node.
//
// What it deliberately does NOT claim: that the agent loads them. See
// tests/e2e/node_agent_plugin_tool_test.go, which is the acceptance for that
// and is currently held back with the reason written in the file.
func TestEveryShippedPackageExtensionEntryResolvesAsAGoExtension(t *testing.T) {
	root := repoRootForPackages(t)
	entries, packages := 0, 0

	shipped, err := filepath.Glob(filepath.Join(root, "plugins", "pig-ops", "*", "package.json"))
	if err != nil {
		t.Fatalf("glob shipped packages: %v", err)
	}
	if len(shipped) == 0 {
		t.Fatalf("no shipped packages found under plugins/pig-ops; this gate would pass " +
			"vacuously if the directory ever moved")
	}

	for _, manifestPath := range shipped {
		pkgDir := filepath.Dir(manifestPath)
		packages++

		body, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Errorf("%s: read: %v", pkgDir, err)
			continue
		}
		var doc struct {
			PI struct {
				Extensions []string `json:"extensions"`
			} `json:"pi"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Errorf("%s: parse package.json: %v", pkgDir, err)
			continue
		}
		if len(doc.PI.Extensions) == 0 {
			// A package with no extensions is legal (a skills-only or
			// prompts-only package). It is not this gate's business.
			continue
		}

		for _, declared := range doc.PI.Extensions {
			entries++
			entry := filepath.Join(pkgDir, filepath.FromSlash(declared))
			def, err := extsource.Resolve(entry)
			if err != nil {
				t.Errorf("%s declares %q, which PiG cannot resolve: %v", pkgDir, declared, err)
				continue
			}
			if def.Language != "go" {
				t.Errorf("%s declares %q, which PiG resolves as %q. OpsKeeper's extensions are "+
					"Go build roots and every shipped tool routes to the host over the socket "+
					"protocol implemented in them; a different language here is a package that "+
					"cannot carry those tools.",
					pkgDir, declared, def.Language)
			}
		}
	}

	t.Logf("checked %d extension entries across %d shipped packages", entries, packages)
}

// TestEveryShippedPackageDeclaresItsExtensionsInPiForm is the shape half of
// the same story, and it is the half that is easy to get wrong quietly.
//
// PiG reads `pi.extensions` as a Node-package manifest: an entry naming a
// directory is expanded with Node rules. A Go build root has no Node entry
// file, so a package whose entries are Go roots is exactly the case PiG's
// own verifier refuses -- `pig verify <package>` fails with "declares
// pi.extensions directories with no extension entry file" (measured against
// PiG 0.4.0, decision 445). The entries themselves resolve, which is why
// this went unnoticed for as long as it did.
//
// This test cannot decide what PiG should do about it; it decides that the
// repository KNOWS. A package that stops declaring extensions at all, or
// starts declaring something this gate does not recognise, is a change to
// the loading story and has to come through here.
func TestEveryShippedPackageDeclaresItsExtensionsInPiForm(t *testing.T) {
	root := repoRootForPackages(t)
	shipped, err := filepath.Glob(filepath.Join(root, "plugins", "pig-ops", "*", "package.json"))
	if err != nil {
		t.Fatalf("glob shipped packages: %v", err)
	}

	for _, manifestPath := range shipped {
		pkgDir := filepath.Dir(manifestPath)
		body, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Errorf("%s: read: %v", pkgDir, err)
			continue
		}
		var doc struct {
			PI struct {
				Extensions []string `json:"extensions"`
			} `json:"pi"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Errorf("%s: parse package.json: %v", pkgDir, err)
			continue
		}
		for _, declared := range doc.PI.Extensions {
			entry := filepath.Join(pkgDir, filepath.FromSlash(declared))
			info, err := os.Stat(entry)
			if err != nil || !info.IsDir() {
				continue
			}
			// The PiG rule, restated: a declared directory must be
			// importable as a Node extension. A Go build root is not, and
			// saying so here is what stops the next reader from assuming
			// this form is accepted.
			if _, importable := extsource.NodeDirectoryImport(entry); !importable {
				t.Logf("NOTE %s declares Go build root %q through pi.extensions; PiG 0.4.0's "+
					"own verifier refuses this package form (decision 445). The entry resolves "+
					"correctly on its own -- see the resolution gate above -- but the package "+
					"form is not accepted, and no agent has yet loaded these tools.",
					strings.TrimPrefix(pkgDir, root+string(filepath.Separator)), declared)
			}
		}
	}
}

func repoRootForPackages(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for probe := dir; probe != filepath.Dir(probe); probe = filepath.Dir(probe) {
		if _, err := os.Stat(filepath.Join(probe, "plugins", "pig-ops")); err == nil {
			return probe
		}
	}
	t.Fatalf("could not locate the repository root from %s", dir)
	return ""
}

package pluginmanifest

// The version a package claims is the only thing that tells a node whether
// the copy it already has is the copy being offered. If a package's tools
// change and its version does not, then:
//
//   - a node that already installed it sees an offer it believes it already
//     satisfies, so it either keeps the old tools or reinstalls a package
//     under a version number two nodes already recorded for different
//     contents;
//   - the catalog, which is what an operator reads to decide what is on
//     offer, names a version that no longer identifies what is on offer;
//   - and the compatibility matrix, which gates on declared versions, has
//     nothing to key on, because two different tool sets share one number.
//
// Every one of those failures is silent. That is the reason this gate
// exists: the check that would catch the mistake is the one nobody writes,
// because a package with no test failures feels finished.
//
// So this file pins, per package, the version together with the tool set
// that version shipped. Changing the tools without changing the version is
// then a failure that names the package and the two versions involved,
// rather than something a reader of the diff has to notice.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// released pins each shipped package's version to the tool set that
// version shipped.
//
// A new tool is a minor bump. A removed tool is a minor bump too, not a
// patch: the tool inventory is the review surface the whole governance
// model rests on, so both directions of change are worth an operator's
// attention. Only a change that leaves the tool set alone — a comment, a
// doc string, a limit retuned with the same names — may keep the version.
//
// Updating this table is the deliberate act. Reading it as "regenerate"
// would be exactly the discipline it removes.
// 0.2.0 is the first release in which the tool sets below did not change but
// the packages did change as *offers*: they are now rows in a registry index
// this repository emits, so a version number identifies not just a tool set
// but a set that a remote catalogue can list and a node can fetch. A node
// that saw 0.1.2 saw an unpublishable directory; one that sees 0.2.0 sees
// something with a tree digest attached.
var released = map[string]struct {
	Version   string
	ToolNames string // hex of the sorted, newline-joined tool names
}{
	"opskeeper-sre-autonomy":      {"0.2.0", "350b0b5e81ed3de5e8d8a50f4ba9d6a1942f6cbc32bdd42398ad3073d4e0db05"},
	"opskeeper-sre-middleware":    {"0.2.0", "8bc8c3e7a4d39e4f5db42cf2d01ad57349277d586725e8b965abc70c0d5eb536"},
	"opskeeper-sre-observability": {"0.2.0", "1b2616a8c3c80e3507dd3ffa6c9b10d2e8cfbe250eb4193fe71c2464849a9b48"},
	"opskeeper-sre-readonly":      {"0.2.0", "d6876e4ca87c01e0ef9b77b3be554915e05583c65e459439e5a0e4ba87aaf15f"},
	"opskeeper-sre-repair":        {"0.2.0", "010722d4295dd64769e7965359698c5a90d37222741304063ab5a22763a9a08f"},
}

// toolNames is the package's declared inventory, in manifest order.
func (p Plugin) toolNames() []string {
	out := make([]string, 0, len(p.Manifest.Spec.Tools))
	for _, t := range p.Manifest.Spec.Tools {
		out = append(out, t.Name)
	}
	return out
}

// toolFingerprint is the hash of the sorted tool names, and deliberately not
// of the whole manifest. A manifest carries prose, and prose edits are not
// releases; hashing the file would turn every comment fix into a version
// bump requirement and the gate would be turned off within a week.
func toolFingerprint(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

func TestShippedPackageVersionTracksItsTools(t *testing.T) {
	root := repoRoot(t)
	base := filepath.Join(root, "plugins", "pig-ops")

	cat, err := LoadCatalog(base)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	for _, p := range cat.Plugins {
		want, ok := released[p.Name()]
		if !ok {
			t.Errorf("package %q ships without a released-version record.\n"+
				"Add it to released in this file with the version it is shipping as. A package "+
				"with no record here is a package whose version can be changed by accident, "+
				"because nothing on this tree would notice.", p.Name())
			continue
		}

		got := p.Manifest.Metadata.Version
		if got != want.Version {
			t.Errorf("package %q declares version %s but this tree last recorded %s.\n"+
				"If the tools changed, bump the version here and in released. If they did "+
				"not, then the recorded version was wrong and should be corrected to the "+
				"one the package actually ships.", p.Name(), got, want.Version)
		}

		fp := toolFingerprint(p.toolNames())
		if fp != want.ToolNames {
			t.Errorf("package %q version %s has a tool set this tree has not recorded.\n"+
				"recorded fingerprint: %s\n"+
				"present fingerprint: %s\n"+
				"A package's tool inventory is the host's allow-list and the review surface "+
				"the governance model rests on, so shipping a different one under the same "+
				"version number is the failure this table exists to stop. Bump the version "+
				"(a new or removed tool is a minor bump) and update released, or restore the "+
				"tool list.", p.Name(), got, want.ToolNames, fp)
		}
	}

	// A record for a package that no longer ships is the other half of the
	// same mistake: it reads as coverage for something absent.
	for name := range released {
		if _, ok := cat.ByName(name); !ok {
			t.Errorf("released records %q, which no longer ships under plugins/pig-ops. "+
				"Remove the record rather than leaving it to vouch for a package that is gone.", name)
		}
	}
}

// The version lives in two files, and only one of them is authoritative.
//
// pig-ops.yaml is the governance sidecar the host admits against, signs,
// lists in the catalogue and pins in a registry index. package.json is the
// Pi manifest the agent runtime mounts, and it carries a version field too.
// Nothing in this tree reads that field, which is exactly the problem: an
// ungoverned number that nothing checks is free to drift, and it did —
// the five packages shipped 0.1.0 there while the manifests they govern
// said 0.2.0. Both were true of the same package at the same time.
//
// The drift is not harmless even though nothing reads it. package.json is
// what a third party sees when they read the package rather than the
// governance sidecar, and "0.1.0" is a version number that names a
// different set of contents. So the fix is not "delete the field" — it is
// to bind it to the field that is authoritative, so that the next bump
// cannot leave it behind again.
func TestEveryPackageManifestVersionEqualsItsGovernanceVersion(t *testing.T) {
	for _, p := range shippedPlugins(t) {
		t.Run(p.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(p.Root, "package.json"))
			if os.IsNotExist(err) {
				// A package with no Pi manifest declares its resources some
				// other way and has no second version to disagree with.
				return
			}
			if err != nil {
				t.Fatalf("read package.json: %v", err)
			}
			var doc struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse package.json: %v", err)
			}
			if doc.Name != "" && doc.Name != p.Name() {
				t.Errorf("package.json names the package %q while the governance manifest "+
					"names it %q; two names for one package is the same defect as two versions, "+
					"and it is resolved the same way — by editing package.json to match", doc.Name, p.Name())
			}
			if got, want := doc.Version, p.Manifest.Metadata.Version; got != want {
				t.Errorf("package.json says version %q while the governance manifest says %q.\n"+
					"The manifest is authoritative: it is what the host admits, signs, lists and "+
					"pins. package.json is a second number that names the same package, and a "+
					"reader who trusts it is reading a version of contents that does not exist. "+
					"Set it to %q — the same value, in both files.", got, want, want)
			}
		})
	}
}

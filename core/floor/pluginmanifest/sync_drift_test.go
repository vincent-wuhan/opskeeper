package pluginmanifest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The generated copies under plugins/pig-ops/ are produced by
// scripts/sync-pig-ops.sh. A script that can be forgotten is not a drift
// check, so the properties it is supposed to preserve are asserted here
// and run in CI.
//
// The specific per-package tests in profile_test.go stay, because they
// say things about a particular package's promise. These are the
// mechanical ones: every package, every extension, no exceptions.
//
// The one wrinkle every assertion here has to respect is that the
// packaged copy is not byte-identical to the canonical file. The node
// builds these from source and has no access to this repository, so the
// wire vocabulary travels inside each extension as a local ./wire package
// and the one import naming it is rewritten. Everything else must match
// exactly, so the checks below apply the same single substitution the
// script does and compare the result. A second substitution, or any edit
// at all, still fails — the rewrite is not a licence to diverge.

const syncScript = "scripts/sync-pig-ops.sh"

// coreWireImport is the canonical import of the broker vocabulary, the
// only path the generated copies rewrite.
const coreWireImport = "github.com/vincent-wuhan/opskeeper/core/wire"

// unpublishedCoreModule is the module a packaged extension must never
// require. There is no such release: the repository has never tagged
// core/vX.Y.Z, so "v0.0.0" is a placeholder that only ever resolved
// through a local replace. A node has no such replace, so a require on
// it fails the extension's very first build — after the package installs,
// after the agent starts, after the conversation works. The agent would
// be live and the model would have no tools, and nothing in an end-to-end
// conversation test would notice.
const unpublishedCoreModule = "github.com/vincent-wuhan/opskeeper/core"

// publishedSDK is the one dependency a packaged extension is allowed to
// keep. PiG stages it from its own tree at build time, so it resolves on
// a node with no checkout of anything.
const publishedSDK = "github.com/MichaelKinsy/PiG/extensions/sdk"

// packagesDir is every shippable package.
func packagesDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "plugins", "pig-ops")
}

// packagedModule is the import path a package's copy of an extension is
// built under. It mirrors what sync-pig-ops.sh writes into the go.mod.
func packagedModule(pkg, ext string) string {
	return "github.com/vincent-wuhan/opskeeper/plugins/pig-ops/" + pkg + "/extensions/" + ext
}

// asPackaged applies the one rewrite the generated copy is allowed to
// differ by. Quoting the import matters: an unquoted replacement would
// also catch the path inside a comment, and a comment that names the
// canonical import is documentation, not a build input.
func asPackaged(src, pkg, ext string) string {
	return strings.ReplaceAll(
		src,
		`"`+coreWireImport+`"`,
		`"`+packagedModule(pkg, ext)+`/wire"`,
	)
}

// walkExtensions visits every package and every extension it ships, and
// fails the test if it finds nothing. A drift check that walks an empty
// tree passes forever, which is the failure mode most worth designing
// out.
func walkExtensions(t *testing.T, visit func(pkg, ext, canonical, packaged string)) int {
	t.Helper()
	packages, err := os.ReadDir(packagesDir(t))
	if err != nil {
		t.Fatalf("read the packages directory: %v", err)
	}
	if len(packages) == 0 {
		t.Fatal("no packages found; the walk below would be vacuous")
	}

	visited := 0
	for _, pkg := range packages {
		if !pkg.IsDir() {
			continue
		}
		extsDir := filepath.Join(packagesDir(t), pkg.Name(), "extensions")
		exts, err := os.ReadDir(extsDir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("%s: read extensions: %v", pkg.Name(), err)
		}
		for _, ext := range exts {
			if !ext.IsDir() {
				continue
			}
			visited++
			visit(pkg.Name(), ext.Name(),
				filepath.Join(repoRoot(t), "core", "pig", "extensions", ext.Name()),
				filepath.Join(extsDir, ext.Name()))
		}
	}
	return visited
}

// goBinary finds the toolchain the test builds with. It is looked up rather
// than assumed so that a failure to find it reads as "no Go here" instead of
// a compile error somewhere inside the assertion.
func goBinary() (string, error) {
	return exec.LookPath("go")
}

// TestEveryPackagedExtensionMatchesItsCanonicalSource is the drift check.
//
// The node builds these from source and has no way to reach the reviewed
// original, so a copy that has drifted is not a stale file — it is a node
// running code nobody looked at. This walks every package rather than
// naming them, so a new package is covered the moment it is added and a
// renamed extension is caught rather than silently skipped.
func TestEveryPackagedExtensionMatchesItsCanonicalSource(t *testing.T) {
	checked := walkExtensions(t, func(pkg, ext, canonical, packaged string) {
		entries, err := os.ReadDir(canonical)
		if err != nil {
			t.Errorf("%s ships extensions/%s but there is no canonical source at %s; "+
				"a package may only ship extensions that exist under core/pig/extensions",
				pkg, ext, canonical)
			return
		}

		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			want, err := os.ReadFile(filepath.Join(canonical, name))
			if err != nil {
				t.Errorf("%s/%s: read canonical %s: %v", pkg, ext, name, err)
				return
			}
			got, err := os.ReadFile(filepath.Join(packaged, name))
			if err != nil {
				t.Errorf("%s/%s: read packaged %s: %v; run %s", pkg, ext, name, err, syncScript)
				return
			}
			// Only the import rewrite may differ. Everything else — the
			// framing, the line bound, the refusal to resend an unknown
			// outcome — is compared as written.
			if string(got) != asPackaged(string(want), pkg, ext) {
				t.Errorf("%s/%s: the packaged %s has drifted from core/pig/extensions/%s/%s; run %s",
					pkg, ext, name, ext, name, syncScript)
			}
		}
	})
	if checked == 0 {
		t.Error("no packaged extension was checked, so this drift test is vacuous")
	}
}

// TestEveryPackagedExtensionCarriesTheWireVocabulary asserts that the copy
// of core/wire a package needs to build is actually inside it.
//
// This is the difference between a node that runs the reviewed protocol
// and one that fails its first build. The wire package is three files of
// DTOs with no dependencies outside the standard library, which is
// exactly why it can travel inside an extension instead of being
// required from a module no node can resolve. The test exists because
// that argument is only true while the copy is actually being made: delete
// it, or add a dependency to core/wire, and the reason evaporates without
// anything else noticing.
func TestEveryPackagedExtensionCarriesTheWireVocabulary(t *testing.T) {
	wireRoot := filepath.Join(repoRoot(t), "core", "wire")
	canonicalWire, err := os.ReadDir(wireRoot)
	if err != nil {
		t.Fatalf("read core/wire: %v", err)
	}
	wanted := 0
	for _, e := range canonicalWire {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		wanted++
	}
	if wanted == 0 {
		t.Fatal("core/wire has no sources, so this test would be vacuous")
	}

	checked := walkExtensions(t, func(pkg, ext, canonical, packaged string) {
		packagedWire := filepath.Join(packaged, "wire")
		entries, err := os.ReadDir(packagedWire)
		if err != nil {
			t.Errorf("%s/%s: the extension ships no ./wire package, so the node's build cannot "+
				"resolve %s; run %s", pkg, ext, coreWireImport, syncScript)
			return
		}

		// A stale file here would be worse than a missing one: it would
		// compile and then describe a protocol the host no longer speaks.
		if len(entries) != wanted {
			t.Errorf("%s/%s/wire holds %d files but core/wire holds %d; a generated copy that "+
				"outlived its source is a protocol disagreement waiting to happen; run %s",
				pkg, ext, len(entries), wanted, syncScript)
		}

		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") {
				continue
			}
			want, err := os.ReadFile(filepath.Join(wireRoot, name))
			if err != nil {
				t.Errorf("%s/%s/wire: read canonical %s: %v", pkg, ext, name, err)
				continue
			}
			got, err := os.ReadFile(filepath.Join(packagedWire, name))
			if err != nil {
				t.Errorf("%s/%s/wire: read packaged %s: %v; run %s", pkg, ext, name, err, syncScript)
				continue
			}
			if string(got) != asPackaged(string(want), pkg, ext) {
				t.Errorf("%s/%s/wire: the packaged %s has drifted from core/wire/%s; run %s",
					pkg, ext, name, name, syncScript)
			}
		}
	})
	if checked == 0 {
		t.Error("no packaged extension was checked, so this test is vacuous")
	}
}

// TestEveryPackagedGoModResolvesOnANode is the constraint that makes the
// copies work at all.
//
// A node has no checkout of this repository and no checkout of a
// pre-stable PiG. A replace directive that resolves on a developer
// machine points at a path that does not exist on the node. A require on
// a version of the OpsKeeper module that was never published does not
// resolve anywhere at all. Both fail the same way and at the same moment:
// the package installs, the agent starts, and the extension's first build
// dies.
func TestEveryPackagedGoModResolvesOnANode(t *testing.T) {
	checked := walkExtensions(t, func(pkg, ext, canonical, packaged string) {
		data, err := os.ReadFile(filepath.Join(packaged, "go.mod"))
		if err != nil {
			t.Errorf("%s/%s: read go.mod: %v; run %s", pkg, ext, err, syncScript)
			return
		}
		text := string(data)

		for i, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "replace") || strings.Contains(line, "=>") {
				t.Errorf("%s/%s/go.mod line %d carries a replace directive (%q); a node cannot resolve it",
					pkg, ext, i+1, trimmed)
			}
			if strings.HasPrefix(trimmed, "require") || strings.HasPrefix(trimmed, unpublishedCoreModule) {
				if strings.Contains(trimmed, unpublishedCoreModule) {
					t.Errorf("%s/%s/go.mod line %d requires %s; no such release exists, so the "+
						"node's build fails on the first attempt; run %s",
						pkg, ext, i+1, unpublishedCoreModule, syncScript)
				}
			}
		}

		if !strings.Contains(text, "module "+packagedModule(pkg, ext)) {
			t.Errorf("%s/%s/go.mod does not declare the packaged module path; run %s", pkg, ext, syncScript)
		}
		// The expected version is the canonical module's, not a literal here.
		// This assertion used to name v0.3.0 while the host had moved to v0.4.0,
		// and the generator wrote the same literal -- so the two agreed with
		// each other and with neither. A check that pins a version in a second
		// place keeps that version alive after it stops being true.
		want := canonicalSDKVersion(t, canonical)
		if !strings.Contains(text, publishedSDK+" "+want) {
			t.Errorf("%s/%s/go.mod does not require %s %s (the version %s requires); PiG stages that "+
				"SDK from its own tree, so it is the one dependency a node can actually resolve; run %s",
				pkg, ext, publishedSDK, want, canonical, syncScript)
		}

		// The go.sum is what lets the SDK resolve before PiG's staging
		// takes over. It must exist, and it must name nothing else: a
		// checksum for the OpsKeeper module would be a checksum for a
		// module this package deliberately stopped requiring.
		sum, err := os.ReadFile(filepath.Join(packaged, "go.sum"))
		if err != nil {
			t.Errorf("%s/%s: read go.sum: %v; run %s", pkg, ext, err, syncScript)
			return
		}
		lines := 0
		for _, line := range strings.Split(strings.TrimSpace(string(sum)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			lines++
			if !strings.HasPrefix(line, publishedSDK+" ") {
				t.Errorf("%s/%s/go.sum names %q; the only module this package resolves is %s; run %s",
					pkg, ext, line, publishedSDK, syncScript)
			}
		}
		if lines == 0 {
			t.Errorf("%s/%s/go.sum is empty, so the SDK cannot resolve; run %s", pkg, ext, syncScript)
		}
	})
	if checked == 0 {
		t.Error("no packaged go.mod was checked, so this test is vacuous")
	}
}

// TestEveryPackagedExtensionBuildsTheWayTheNodeBuildsIt turns the drift
// checks above from an inspection into a proof.
//
// Everything else in this file compares files. This one runs the
// compiler, with the two environment settings the agent runtime sets
// (builder.go: GOWORK=off, CGO_ENABLED=0), over every packaged
// extension. It is the only assertion here that would have caught the
// original defect, because the original defect was not a drift — the
// packaged go.mod was exactly what the script produced. It was a
// module that resolved on a developer machine and nowhere else, and no
// amount of reading a file distinguishes that from one that resolves
// everywhere.
func TestEveryPackagedExtensionBuildsTheWayTheNodeBuildsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds every packaged extension from source; skipped under -short")
	}
	goTool, err := goBinary()
	if err != nil {
		t.Fatalf("locate the go toolchain: %v", err)
	}

	checked := walkExtensions(t, func(pkg, ext, canonical, packaged string) {
		out := filepath.Join(t.TempDir(), "extension")
		build := exec.Command(goTool, "build",
			"-buildvcs=false", "-trimpath", "-ldflags", "-s -w", "-o", out, ".")
		build.Dir = packaged
		// The agent's build environment, verbatim. GOWORK=off is the one
		// that matters: a workspace would resolve the OpsKeeper module
		// from this checkout and hide the fact that a node cannot.
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
		if combined, err := build.CombinedOutput(); err != nil {
			t.Errorf("%s/%s: the packaged extension does not build the way the node builds it: %v\n%s",
				pkg, ext, err, combined)
		}
	})
	if checked == 0 {
		t.Error("no packaged extension was built, so this test is vacuous")
	}
}

// toolsets are the extensions that each carry their own copy of the broker
// client. Every directory under core/pig/extensions that ships one is
// discovered rather than named: the test below walks this list, so adding a
// fourth package without adding it here fails as a vacuous check rather
// than passing silently.
// toolsets is every toolset that carries a copy of the broker client,
// read from the directory rather than written down here.
//
// The list used to be a literal, and a literal is a fourth thing to keep
// in step: a new toolset that shipped its own client — which every one of
// them has to, because each package builds standalone on a node — joined
// the family without joining the check, so the client that could resend a
// call whose reply was lost would have been a new file nobody compared
// against anything. The guard was real and it covered three of six.
//
// Deriving it means the only way to not be checked is not to ship a
// client, and a toolset with no client is itself wrong.
func discoverToolsets(t *testing.T) []string {
	t.Helper()
	root := filepath.Join(repoRoot(t), "core", "pig", "extensions")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		client := filepath.Join(root, e.Name(), "client.go")
		body, err := os.ReadFile(client)
		if err != nil {
			// No client at all: a toolset that ships no broker client has
			// no copy to keep in step. (Nothing ships that way today, and
			// if something did, the build check beside this one would say
			// so far more clearly than an absence here would.)
			continue
		}
		// A client.go is not automatically a *broker* client. The gate
		// extension has one too, and it speaks the policy-gate protocol —
		// a different wire type, on a different socket, answering a
		// different question. Including it in the comparison would be
		// comparing two protocols and calling the difference a bug, and
		// the first person to hit that would "fix" it by copying the
		// wrong file.
		//
		// So the rule is the protocol itself: a copy of the tool-broker
		// client is one that names ToolRequest. A new toolset that
		// implemented the broker without that type would be caught by the
		// build check, not here.
		if !bytes.Contains(body, []byte("ToolRequest")) {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// TestEveryToolsetsBrokerClientIsTheSameFile asserts the one property that
// genuinely spans packages.
//
// Each toolset carries its own copy of the broker client, because each
// package has to build standalone on a node. N copies of a protocol are N
// things that can disagree, and the disagreement that matters is the unsafe
// one: a repair client that resent a call whose reply was lost would
// restart a service twice.
//
// So the copies must be the same file, modulo the package clause they
// belong to. Everything else — the framing, the line bound, the refusal to
// resend an unknown outcome — is asserted equal here rather than left to
// several sets of tests that happen to pass today.
func TestEveryToolsetsBrokerClientIsTheSameFile(t *testing.T) {
	toolsets := discoverToolsets(t)
	if len(toolsets) < 2 {
		t.Fatal("the list is too short for the comparison below to mean anything")
	}
	// Sorted, so the "and" in a failure message is stable across runs and
	// a reviewer can see at a glance which two copies diverged.
	sort.Strings(toolsets)

	want, err := os.ReadFile(filepath.Join(repoRoot(t), "core", "pig", "extensions", toolsets[0], "client.go"))
	if err != nil {
		t.Fatalf("read the %s client: %v", toolsets[0], err)
	}
	for _, other := range toolsets[1:] {
		got, err := os.ReadFile(filepath.Join(repoRoot(t), "core", "pig", "extensions", other, "client.go"))
		if err != nil {
			t.Errorf("read the %s client: %v", other, err)
			continue
		}
		if stripPackageClause(string(want)) != stripPackageClause(string(got)) {
			t.Errorf("the %s and %s broker clients have diverged; they are the same protocol and a "+
				"divergence in the %s copy is a second action taken on a live system",
				toolsets[0], other, other)
		}
	}
}

// stripPackageClause removes the leading `package X` line so two clients
// in different packages can be compared. It deliberately does not
// normalise anything else: a whitespace change, a reordered field, a
// changed constant is a difference worth failing on.
func stripPackageClause(src string) string {
	if i := strings.IndexByte(src, '\n'); i >= 0 {
		return src[i+1:]
	}
	return src
}

// TestEveryPackageResourceEntryExistsOnDisk is the guard against the
// failure mode where a package declares a resource it does not ship.
//
// The whole file-header argument above applies here, and it applies
// hardest to this one: PiG treats a package.json's resource block as
// *authoritative* — declaring one class suppresses convention discovery
// for it — so a declared path that is not on disk is not a missing file,
// it is a package the agent refuses to load. A node then boots with a
// package set that the manager believes is installed, the model has no
// tools, and every conversation test in the repository still passes
// because none of them loads a package with extensions.
//
// It is found the hard way, which is the only reason it is here. The
// self-heal package shipped a package.json naming opskeeper-gate, and
// the directory that would have held it did not exist — sync-pig-ops.sh
// copies *into* directories that are already there, so it wrote nine
// extensions and said nothing about the tenth. `pig status` on a real
// binary answered with healthy:false and the missing path, in one line,
// after every unit test in this repository had passed.
func TestEveryPackageResourceEntryExistsOnDisk(t *testing.T) {
	for _, pkg := range shippedPlugins(t) {
		t.Run(pkg.Name(), func(t *testing.T) {
			root := pkg.Root
			raw, err := os.ReadFile(filepath.Join(root, "package.json"))
			if os.IsNotExist(err) {
				// A package with no manifest discovers by convention, which
				// is the safe direction: a directory that is there is a
				// resource that loads.
				return
			}
			if err != nil {
				t.Fatalf("read the package manifest: %v", err)
			}
			var manifest struct {
				Pi  map[string][]string `json:"pi"`
				Pig map[string][]string `json:"pig"`
			}
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatalf("parse the package manifest: %v", err)
			}
			declared := manifest.Pi
			if declared == nil {
				declared = manifest.Pig
			}
			for class, paths := range declared {
				for _, p := range paths {
					full := filepath.Join(root, filepath.FromSlash(p))
					if _, err := os.Stat(full); err != nil {
						t.Errorf("package.json declares %s %q, which is not on disk: the agent "+
							"refuses a package whose declared resource is missing, so this node "+
							"would boot with the package set the manager thinks it installed and a "+
							"model that has no tools", class, p)
					}
				}
			}
		})
	}
}

// canonicalSDKVersion reads the PiG extension SDK version the canonical
// extension module requires. Both the generator and this test read it there,
// so the packaged copy cannot drift to a version the host never asked for.
func canonicalSDKVersion(t *testing.T, canonical string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(canonical, "go.mod"))
	if err != nil {
		t.Fatalf("%s: %v", canonical, err)
	}
	re := regexp.MustCompile(`github\.com/MichaelKinsy/PiG/extensions/sdk\s+(v\S+)`)
	found := re.FindSubmatch(raw)
	if found == nil {
		t.Fatalf("%s/go.mod no longer requires the PiG extension SDK; update %s rather than guessing a version",
			canonical, syncScript)
	}
	return string(found[1])
}

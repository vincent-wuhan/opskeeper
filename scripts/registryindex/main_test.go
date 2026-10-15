package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The producer and the reader are two files in two modules that only meet at
// a JSON document. Nothing in the type system says they agree, so the tests
// here are the only thing that does: each one takes what the producer emits
// and hands it to the reader the host actually uses.

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

const shippedRoot = "plugins/pig-ops"

// The round trip, on the real tree. If the producer emits a document its own
// reader refuses, then publishing one would break every consumer at the
// moment it was first used — which is the worst possible time, because the
// artifact looks fine in whatever served it.
func TestTheShippedPackagesProduceAnIndexTheReaderAccepts(t *testing.T) {
	idx, err := build("opskeeper-official", "https://registry.invalid/ops",
		filepath.Join(repoRoot(t), shippedRoot))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pluginmanifest.ParseIndex(data); err != nil {
		t.Fatalf("the producer emitted a document the host's reader refuses: %v", err)
	}
}

// Every row's version must be the manifest's own, read back from disk rather
// than compared against the same string literal this test would otherwise
// use. Comparing index to test would only prove the index equals itself.
func TestEachRowAgreesWithTheManifestOnDisk(t *testing.T) {
	root := filepath.Join(repoRoot(t), shippedRoot)

	idx, err := build("opskeeper-official", "", root)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(idx.Items) != 5 {
		t.Fatalf("got %d packages, want the 5 this tree ships", len(idx.Items))
	}

	for _, item := range idx.Items {
		raw, err := os.ReadFile(filepath.Join(root, item.Name, manifestFile))
		if err != nil {
			t.Fatalf("%s: %v", item.Name, err)
		}
		m, err := decodeStrict(raw)
		if err != nil {
			t.Fatalf("%s: %v", item.Name, err)
		}
		if item.Version != m.Metadata.Version {
			t.Errorf("%s: index says %s, the manifest says %s", item.Name, item.Version, m.Metadata.Version)
		}

		digest, err := pluginmanifest.TreeDigest(filepath.Join(root, item.Name))
		if err != nil {
			t.Fatalf("%s: %v", item.Name, err)
		}
		if item.SHA256 != digest {
			t.Errorf("%s: index digest %s does not match the tree's %s", item.Name, item.SHA256, digest)
		}
		if item.ManifestYAML != string(raw) {
			t.Errorf("%s: the index carries a different manifest than the one on disk; "+
				"the bytes must be the node's, not a projection of them", item.Name)
		}
	}
}

// A published artefact has to be stable, or every consumer sees a change that
// did not happen and learns to ignore the digest.
func TestTwoBuildsOverAnUnchangedTreeAreByteIdentical(t *testing.T) {
	root := filepath.Join(repoRoot(t), shippedRoot)

	first, err := json.Marshal(mustBuild(t, root))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(mustBuild(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("two builds over the same tree produced different bytes")
	}
}

// A package directory whose name disagrees with its manifest is a typo in one
// of the two. Publishing whichever this script happened to read would put a
// catalogue row on a name no node will ever ask for, and the row would be
// indistinguishable from a good one.
func TestADirectoryNamedDifferentlyFromItsManifestIsRefused(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "typo-in-the-directory-name")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := strings.Replace(validManifest, "name: acme", "name: acme-real", 1)
	if err := os.WriteFile(filepath.Join(pkg, manifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := build("r", "", root)
	if err == nil {
		t.Fatal("published a package under a directory name its manifest contradicts")
	}
	if !strings.Contains(err.Error(), "acme-real") {
		t.Errorf("error %q does not name both sides of the disagreement", err)
	}
}

// An empty catalogue is a fact about a registry, and "we found nothing" is
// not one. Emitting it would overwrite a real index with an empty document.
func TestAnEmptyPackagesRootIsRefused(t *testing.T) {
	if _, err := build("r", "", t.TempDir()); err == nil {
		t.Fatal("emitted an index claiming an empty catalogue is a fact about the registry")
	}
}

func mustBuild(t *testing.T, root string) pluginmanifest.Index {
	t.Helper()
	idx, err := build("opskeeper-official", "", root)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return idx
}

const validManifest = `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: acme
  version: 1.0.0
  vendor: acme
  homepage: https://example.invalid/acme
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  required_scopes: [host.read]
  tools:
    - { name: acme_probe, class: read }
`

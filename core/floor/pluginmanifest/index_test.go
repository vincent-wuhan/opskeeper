package pluginmanifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// A registry index is the one document in this system that arrives from
// outside and is read by an operator rather than by a node. So the checks
// below are not about parsing: json.Unmarshal would parse all of these.
// They are about refusing to show a catalogue that disagrees with what a node
// would install, because the operator is the only reader who cannot check.

const goodManifest = `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: opskeeper-sre-readonly
  version: 0.2.0
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  tools:
    - { name: get_host_load, class: read }
  required_scopes: [host.read]
`

// doc builds an index document around one raw manifest.
func doc(name, version, manifest string) []byte {
	b, _ := json.Marshal(Index{
		APIVersion: IndexAPIVersion,
		Kind:       IndexKind,
		Registry:   "test-registry",
		Items:      []IndexItem{{Name: name, Version: version, ManifestYAML: manifest}},
	})
	return b
}

func TestParseIndexAcceptsAConsistentDocument(t *testing.T) {
	idx, err := ParseIndex(doc("opskeeper-sre-readonly", "0.2.0", goodManifest))
	if err != nil {
		t.Fatalf("a consistent index was refused: %v", err)
	}
	plugins := idx.Plugins()
	if len(plugins) != 1 {
		t.Fatalf("got %d plugins, want 1", len(plugins))
	}
	if got := plugins[0].Manifest.Metadata.Name; got != "opskeeper-sre-readonly" {
		t.Errorf("name = %q", got)
	}
	// A row that has not been fetched has no directory. Leaving Root empty
	// is the honest value; pointing it at a URL would invite a reader to
	// treat the URL as a path it can stat.
	if plugins[0].Root != "" {
		t.Errorf("Root = %q, want empty: nothing has been fetched", plugins[0].Root)
	}
	if plugins[0].Origin != OriginRegistry {
		t.Errorf("Origin = %q, want %q", plugins[0].Origin, OriginRegistry)
	}
}

func TestParseIndexRefusesWhatItCannotReconcile(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "wrong kind",
			data: []byte(`{"apiVersion":"opskeeper.io/v1","kind":"SomethingElse","registry":"r","items":[]}`),
			want: "kind",
		},
		{
			name: "wrong apiVersion",
			data: []byte(`{"apiVersion":"opskeeper.io/v2","kind":"PluginIndex","registry":"r","items":[]}`),
			want: "apiVersion",
		},
		{
			// A key this build does not know is a typo in somebody's
			// generator. Silently dropping it is how an index ends up
			// omitting a digest while looking complete.
			name: "unknown field",
			data: []byte(`{"apiVersion":"opskeeper.io/v1","kind":"PluginIndex","registry":"r","items":[],"refresh":"5m"}`),
			want: "not valid",
		},
		{
			name: "the listing contradicts the manifest about the name",
			data: doc("opskeeper-sre-repair", "0.2.0", goodManifest),
			want: "its manifest declares",
		},
		{
			name: "the listing contradicts the manifest about the version",
			data: doc("opskeeper-sre-readonly", "0.1.4", goodManifest),
			want: "declares",
		},
		{
			name: "the manifest is not one a node would install",
			data: doc("opskeeper-sre-readonly", "0.2.0", "apiVersion: opskeeper.io/v1\nkind: Plugin\nmetadata:\n  name: opskeeper-sre-readonly\n"),
			want: "not admissible",
		},
		{
			name: "a key the manifest schema does not know",
			data: doc("opskeeper-sre-readonly", "0.2.0", strings.Replace(goodManifest,
				"required_scopes: [host.read]", "required_scope: [host.read]", 1)),
			want: "not admissible",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseIndex(tc.data)
			if err == nil {
				t.Fatalf("accepted a document it should have refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q, so the reason is not in the message "+
					"an operator reads", err, tc.want)
			}
		})
	}
}

// An index is a public document. A registry whose label is unfamiliar is
// still worth showing — it becomes un-installable at the allowlist, which is
// a decision someone makes with their eyes open, rather than invisible, which
// is not a decision at all.
func TestParseIndexDoesNotJudgeTheRegistryLabel(t *testing.T) {
	data := []byte(`{"apiVersion":"opskeeper.io/v1","kind":"PluginIndex","registry":"a-brand-new-name","items":[]}`)
	if _, err := ParseIndex(data); err != nil {
		t.Fatalf("an unfamiliar registry name made the document unreadable: %v", err)
	}
}

// The precedence rule has to hold for a remote row, not only for directories,
// and the loser has to be recorded — otherwise an operator sees one row and
// has no way to know a second, higher-priority copy exists.
func TestARemoteRowLosesToEveryLocalRoot(t *testing.T) {
	remote := []Plugin{{
		Origin:   OriginRegistry,
		Manifest: mustDecode(t, goodManifest),
	}}

	for _, label := range []string{OriginTenant, OriginSystem, OriginBuiltin} {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			writePigOps(t, dir, goodManifest)

			cat, err := LoadCatalogSources(remote, Root{Path: dir, Label: label})
			if err != nil {
				t.Fatalf("LoadCatalogSources: %v", err)
			}
			entries := cat.Entries()
			if len(entries) != 1 {
				t.Fatalf("got %d entries, want 1", len(entries))
			}
			if entries[0].Origin != label {
				t.Errorf("origin = %q, want the local %q: a remote claim about a name must not "+
					"displace the copy this control plane holds", entries[0].Origin, label)
			}
			if !entries[0].Shadowed {
				t.Error("Shadowed = false; the operator cannot tell that a remote row was dropped")
			}
		})
	}
}

// The reverse direction is the reason the rank table has a registry row at
// all: a package nobody has installed yet is invisible otherwise, which is the
// narrower answer the single-root index already gave once (decision 459).
func TestARemoteRowAppearsWhenNothingLocalClaimsTheName(t *testing.T) {
	cat, err := LoadCatalogSources([]Plugin{{
		Origin:   OriginRegistry,
		Manifest: mustDecode(t, goodManifest),
	}})
	if err != nil {
		t.Fatalf("LoadCatalogSources: %v", err)
	}
	entries := cat.Entries()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: a package in the registry that nobody has installed "+
			"yet is exactly what the catalogue is for", len(entries))
	}
	if entries[0].Version != "0.2.0" {
		t.Errorf("version = %q; the row must carry the version the manifest declares", entries[0].Version)
	}
}

// mustDecode is the test-side door into the one parser the index uses, so a
// fixture that stops being admissible fails here with the schema's own
// complaint rather than as a mysterious ParseIndex error three tests later.
func mustDecode(t *testing.T, raw string) domain.PluginManifest {
	t.Helper()
	m, err := sdk.Decode([]byte(raw))
	if err != nil {
		t.Fatalf("the fixture manifest is not admissible, which no test below can distinguish "+
			"from a product failure: %v", err)
	}
	return m
}

// writePigOps drops a manifest into a one-package root.
//
// The subdirectory is not decoration: a Root holds packages, not manifests,
// so writing pig-ops.yaml at the root itself would produce a root the loader
// correctly reads as empty — and the test would then be asserting that an
// absent package loses, which is true and proves nothing.
func writePigOps(t *testing.T, root, raw string) {
	t.Helper()
	pkg := filepath.Join(root, "acme")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "pig-ops.yaml"), []byte(raw), 0o644); err != nil {
		t.Fatalf("writing the fixture manifest: %v", err)
	}
}

// Two local roots carrying the same package, in the wrong order on purpose.
//
// This is the regression for the defect the remote-row test above uncovered:
// the origin was stamped only for the first row to claim a name, so every
// later contender was ranked under a label that was not in the rank table at
// all. An absent key scores zero, which is the *best* rank there is, so the
// root read second won over the tenant's — and the winning row then reported
// an empty origin, so the operator could not even see which root it came
// from.
//
// The fixture therefore lists system first and tenant second, because that is
// the arrangement that hides the bug: a test that puts the tenant root first
// passes under both the old and the new code, which is the shape of test that
// would have let this ship.
func TestPrecedenceHoldsRegardlessOfRootOrder(t *testing.T) {
	system := t.TempDir()
	tenant := t.TempDir()
	writePigOps(t, system, goodManifest)
	writePigOps(t, tenant, goodManifest)

	cat, err := LoadCatalogSources(nil,
		Root{Path: system, Label: OriginSystem},
		Root{Path: tenant, Label: OriginTenant},
	)
	if err != nil {
		t.Fatalf("LoadCatalogSources: %v", err)
	}
	entries := cat.Entries()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Origin != OriginTenant {
		t.Errorf("origin = %q, want %q: a tenant's own copy is the one that runs",
			entries[0].Origin, OriginTenant)
	}
	if !entries[0].Shadowed {
		t.Error("Shadowed = false; a shadowed row that does not say so is a row an operator " +
			"cannot reason about")
	}
}

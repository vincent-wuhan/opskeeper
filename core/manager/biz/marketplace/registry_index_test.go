package marketplace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// The remote index has one job and one way to fail it: the catalogue page must
// keep answering while a registry is unreachable, and must gain a row when one
// answers. Both directions are below, because a reader that only has the
// first would pass a deployment whose registry is permanently down.

const registryManifest = `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: opskeeper-sre-readonly
  version: 0.2.0
  vendor: opskeeper
  homepage: https://example.invalid/readonly
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  required_scopes: [host.read]
  tools:
    - { name: get_host_load, class: read }
`

// serveIndex stands up a registry whose index body is supplied by the caller,
// so one test can cover a good document and a broken one.
func serveIndex(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// indexJSON renders a valid one-item index.
func indexJSON(t *testing.T, name, version, manifest string) string {
	t.Helper()
	b, err := json.Marshal(pluginmanifest.Index{
		APIVersion: pluginmanifest.IndexAPIVersion,
		Kind:       pluginmanifest.IndexKind,
		Registry:   "opskeeper-official",
		Items: []pluginmanifest.IndexItem{
			{Name: name, Version: version, ManifestYAML: manifest},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCatalogIncludesRowsAConfiguredRegistryOffers(t *testing.T) {
	url := serveIndex(t, http.StatusOK, indexJSON(t, "opskeeper-sre-readonly", "0.2.0", registryManifest))

	uc, _, _, _, _, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: url}}

	entries, err := uc.Catalog(context.Background(), Caller{UserID: 1})
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Name != "opskeeper-sre-readonly" {
		t.Errorf("name = %q", entries[0].Name)
	}
	if entries[0].Version != "0.2.0" {
		t.Errorf("version = %q, want the manifest's own 0.2.0", entries[0].Version)
	}
	if entries[0].Origin != pluginmanifest.OriginRegistry {
		t.Errorf("origin = %q, want %q: the operator has to be able to tell an offer from an "+
			"installed copy", entries[0].Origin, pluginmanifest.OriginRegistry)
	}
}

// One third party's outage must not take the local rows with it. Before this
// existed there was nothing to regress against, because there was no remote
// half; the point of the assertion is the *combination* — a dead registry and
// a package already on disk, where the tempting implementation is to fail the
// whole listing.
func TestCatalogSurvivesAConfiguredRegistryBeingDown(t *testing.T) {
	dead := serveIndex(t, http.StatusInternalServerError, "upstream is on fire")
	broken := serveIndex(t, http.StatusOK, `{"apiVersion":"opskeeper.io/v1","kind":"NotAnIndex"}`)

	uc, _, _, _, systemRoot, _ := newTestUC(t)
	writePigOpsPackage(t, filepath.Join(systemRoot, "acme"), "acme", "1.0.0")
	uc.cfg.RegistryIndexes = []RegistryIndex{
		{Name: "broken-doc", URL: broken},
		{Name: "down", URL: dead},
	}

	entries, err := uc.Catalog(context.Background(), Caller{UserID: 1})
	if err != nil {
		t.Fatalf("a dead registry failed the listing: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want the one local package: %+v", len(entries), entries)
	}
	if entries[0].Name != "acme" {
		t.Errorf("name = %q, want acme", entries[0].Name)
	}
}

// The same name from two places is the case the rank table exists for, and it
// is the case an operator will hit the first time a package is both staged and
// published. The local copy is the one that would install.
func TestAnInstalledCopyOutranksWhatTheRegistryOffersForTheSameName(t *testing.T) {
	// The registry's copy is genuinely a different version, so the test is
	// about precedence rather than about a tie. Its own listing and its own
	// manifest agree, because ParseIndex refuses a document where they do not
	// and the refusal would make this test pass for the wrong reason.
	newer := strings.Replace(acmeManifest, "version: 1.0.0", "version: 9.9.9", 1)
	url := serveIndex(t, http.StatusOK, indexJSON(t, "acme", "9.9.9", newer))

	uc, _, _, _, systemRoot, _ := newTestUC(t)
	writePigOpsPackage(t, filepath.Join(systemRoot, "acme"), "acme", "1.0.0")
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: url}}

	entries, err := uc.Catalog(context.Background(), Caller{UserID: 1})
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Version != "1.0.0" {
		t.Errorf("version = %q, want the installed 1.0.0: the catalogue answers what this "+
			"control plane will install, not what somebody else publishes", entries[0].Version)
	}
	if !entries[0].Shadowed {
		t.Error("Shadowed = false; the operator cannot tell a registry row was dropped")
	}
}

// A registry's URL is what makes "where can I install from" answerable. It was
// empty before, with a comment promising a later PR; this pins the promise.
func TestRegistriesCarryTheConfiguredURL(t *testing.T) {
	uc, _, _, _, _, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{
		Name: "opskeeper-official",
		URL:  "https://registry.internal/ops/index.json",
	}}

	got := uc.Registries(context.Background(), Caller{UserID: 1})
	var found bool
	for _, e := range got.Items {
		if e.Name == "opskeeper-official" {
			found = true
			if e.URL != "https://registry.internal/ops/index.json" {
				t.Errorf("url = %q, want the configured index url", e.URL)
			}
		}
	}
	if !found {
		t.Fatalf("the allowlisted source is missing from the listing: %+v", got.Items)
	}
}

// The end-to-end shape: a manifest this build would refuse to install must not
// be laundered into a catalogue row by a registry. The reader and the node
// share sdk.Decode precisely so this cannot drift — this test is what would
// notice if they did.
func TestCatalogRefusesToListAPackageTheHostWouldRefuse(t *testing.T) {
	// required_scopes misspelt: the host reads this as "needs no grant".
	typo := strings.Replace(registryManifest, "required_scopes:", "required_scope:", 1)
	url := serveIndex(t, http.StatusOK, indexJSON(t, "opskeeper-sre-readonly", "0.2.0", typo))

	uc, _, _, _, _, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: url}}

	entries, err := uc.Catalog(context.Background(), Caller{UserID: 1})
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0: a package the host refuses to install has no "+
			"business being offered", len(entries))
	}
}

const acmeManifest = `apiVersion: opskeeper.io/v1
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

// writePigOpsPackage stages one installed package under a skills root.
func writePigOpsPackage(t *testing.T, dir, name, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(acmeManifest, "version: 1.0.0", "version: "+version, 1)
	body = strings.Replace(body, "name: acme", "name: "+name, 1)
	if err := os.WriteFile(filepath.Join(dir, "pig-ops.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Keep the sdk import honest: the manifest fixture above must stay one the
// host admits, or every "refused" assertion in this file would pass for the
// wrong reason.
func TestTheRegistryFixtureIsOneTheHostAdmits(t *testing.T) {
	if _, err := sdk.Decode([]byte(registryManifest)); err != nil {
		t.Fatalf("the good-path fixture is not admissible: %v", err)
	}
}

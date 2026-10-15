package marketplace

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// A registry install is the one path where the address and the digest both
// come from somewhere other than the caller. Every test below is about what
// happens when that somewhere else is wrong, because the caller in this path
// has no way to check it themselves — which is the whole reason the host
// checks.

const registryPack = "opskeeper-sre-readonly"

// packTarball tars a directory the way a registry would publish it: the
// package's own files at the archive root, no wrapper directory.
func packTarball(t *testing.T, dir string) []byte {
	t.Helper()
	var buf strings.Builder
	gz := gzip.NewWriter(&stringWriter{&buf})
	tw := tar.NewWriter(gz)

	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     filepath.ToSlash(rel),
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		t.Fatalf("tarring the pack: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(buf.String())
}

// stringWriter exists so the tarball builder reads as one expression; bytes.Buffer
// would do, and this says which of the two the code means.
type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

// registry serves an index and one pack tarball, and lets a test corrupt
// either side independently.
type registry struct {
	indexURL string
	tarURL   string
}

func serveRegistry(t *testing.T, packDir, manifestYAML, version, digest string) registry {
	t.Helper()
	body := packTarball(t, packDir)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, _ *http.Request) {
		idx := pluginmanifest.Index{
			APIVersion: pluginmanifest.IndexAPIVersion,
			Kind:       pluginmanifest.IndexKind,
			Registry:   "opskeeper-official",
			Items: []pluginmanifest.IndexItem{{
				Name:         registryPack,
				Version:      version,
				URL:          srv.URL + "/" + registryPack + ".tgz",
				SHA256:       digest,
				ManifestYAML: manifestYAML,
			}},
		}
		_ = json.NewEncoder(w).Encode(idx)
	})
	mux.HandleFunc("/"+registryPack+".tgz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(body)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return registry{indexURL: srv.URL + "/index.json", tarURL: srv.URL + "/" + registryPack + ".tgz"}
}

// stageAndPublish writes a test pack plus the pig-ops manifest, and returns
// the directory and its tree digest — the digest is computed by the same
// function the install path verifies with, so the happy-path fixture is not
// asserting against a second implementation of the digest.
func stageAndPublish(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	writeTestPack(t, root, registryPack)
	if err := os.WriteFile(filepath.Join(root, registryPack, "pig-ops.yaml"),
		[]byte(registryManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := pluginmanifest.TreeDigest(filepath.Join(root, registryPack))
	if err != nil {
		t.Fatalf("hashing the fixture pack: %v", err)
	}
	return filepath.Join(root, registryPack), digest
}

func registryCaller() Caller { return Caller{UserID: 1, Role: "admin"} }

func registrySource(version string) Source {
	return Source{Type: SourceTypeRegistry, Registry: "opskeeper-official", PackID: registryPack, Version: version}
}

// The path that has to work. Everything else is a refusal.
func TestARegistryInstallFetchesWhatTheIndexNames(t *testing.T) {
	packDir, digest := stageAndPublish(t)
	reg := serveRegistry(t, packDir, registryManifest, "0.2.0", digest)

	uc, repo, _, _, systemRoot, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: reg.indexURL}}

	res, err := uc.Install(context.Background(), registryCaller(), registrySource("0.2.0"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Pack.PackID != registryPack {
		t.Errorf("pack_id = %q", res.Pack.PackID)
	}
	// SourceLabel is the registry name, not the type: a registry install and
	// a local one that happen to deliver the same bytes are different facts
	// in the install ledger.
	if res.Pack.Source != "opskeeper-official" {
		t.Errorf("source = %q, want the registry label", res.Pack.Source)
	}
	if _, err := os.Stat(filepath.Join(systemRoot, registryPack)); err != nil {
		t.Errorf("the pack is not on disk under the system root: %v", err)
	}
	if _, err := repo.GetByPackID(context.Background(), 0, registryPack); err != nil {
		t.Errorf("the install was not recorded: %v", err)
	}
}

// The substitution the digest exists for. The archive served is a real pack;
// the index lies about its digest.
func TestAPackageThatDoesNotMatchTheRowIsRefusedAndNotInstalled(t *testing.T) {
	packDir, _ := stageAndPublish(t)
	// The row's digest is wrong; the package served is the real one. That is
	// the situation "the registry republished under the same version"
	// produces, and it is why the digest is checked against the index rather
	// than against whatever the package claims about itself.
	reg := serveRegistry(t, packDir, registryManifest, "0.2.0", strings.Repeat("0", 64))

	uc, _, _, _, systemRoot, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: reg.indexURL}}

	_, err := uc.Install(context.Background(), registryCaller(), registrySource("0.2.0"))
	if err == nil {
		t.Fatal("installed a package whose digest did not match its row")
	}
	if !strings.Contains(err.Error(), "does not match the digest") {
		t.Errorf("error %q does not say what failed", err)
	}
	// Both digests are named, because "it did not match" is not actionable.
	if !strings.Contains(err.Error(), strings.Repeat("0", 64)) {
		t.Error("the refusal does not quote the digest the index claimed")
	}
	assertNotInstalled(t, systemRoot, registryPack)
}

// A registry can pass the digest check and still have shipped a manifest
// nobody validated. This is the case a digest alone does not catch.
func TestARowWhoseManifestIsNotInThePackageIsRefused(t *testing.T) {
	packDir, digest := stageAndPublish(t)
	// The row carries a manifest that is admissible, agrees with its own
	// name and version, and is not the one in the package. A comment line is
	// the smallest such difference and it is the honest one: the listing and
	// the artefact describe the same plugin in two slightly different words,
	// and nothing in the format says they are the same bytes.
	other := registryManifest + "# a line the package does not carry\n"
	reg := serveRegistry(t, packDir, other, "0.2.0", digest)

	uc, _, _, _, systemRoot, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: reg.indexURL}}

	_, err := uc.Install(context.Background(), registryCaller(), registrySource("0.2.0"))
	if err == nil {
		t.Fatal("installed a package whose manifest is not the one the index validated")
	}
	if !strings.Contains(err.Error(), "not the one in the package") {
		t.Errorf("error %q does not say what failed", err)
	}
	assertNotInstalled(t, systemRoot, registryPack)
}

// "Install the latest" is what a human wants and what a node must not do: a
// floating version leaves the node holding bytes nobody compared a digest
// against at review time.
func TestARegistryInstallMustNameItsVersion(t *testing.T) {
	packDir, digest := stageAndPublish(t)
	reg := serveRegistry(t, packDir, registryManifest, "0.2.0", digest)

	uc, _, _, _, systemRoot, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: reg.indexURL}}

	_, err := uc.Install(context.Background(), registryCaller(), registrySource(""))
	if err == nil {
		t.Fatal("accepted an unpinned registry install")
	}
	if !strings.Contains(err.Error(), "must name the version") {
		t.Errorf("error %q does not explain why", err)
	}
	assertNotInstalled(t, systemRoot, registryPack)
}

func TestARegistryInstallOfAVersionTheIndexDoesNotOfferIsRefused(t *testing.T) {
	packDir, digest := stageAndPublish(t)
	reg := serveRegistry(t, packDir, registryManifest, "0.2.0", digest)

	uc, _, _, _, systemRoot, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: reg.indexURL}}

	_, err := uc.Install(context.Background(), registryCaller(), registrySource("0.1.4"))
	if err == nil {
		t.Fatal("installed a version the index does not offer")
	}
	if !strings.Contains(err.Error(), "offers") {
		t.Errorf("error %q does not say which version is on offer", err)
	}
	assertNotInstalled(t, systemRoot, registryPack)
}

// A row with no digest is a row that says "trust me", and this path exists
// because it does not.
func TestARowWithoutADigestCannotBeInstalledFrom(t *testing.T) {
	packDir, _ := stageAndPublish(t)
	reg := serveRegistry(t, packDir, registryManifest, "0.2.0", "")

	uc, _, _, _, systemRoot, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: reg.indexURL}}

	_, err := uc.Install(context.Background(), registryCaller(), registrySource("0.2.0"))
	if err == nil {
		t.Fatal("installed from a row that carries no digest")
	}
	if !strings.Contains(err.Error(), "no digest") {
		t.Errorf("error %q does not say what was missing", err)
	}
	assertNotInstalled(t, systemRoot, registryPack)
}

// A registry label the operator never configured is not a registry. If this
// fell through to fetching some default, the allowlist would be decoration.
func TestARegistryThatIsNotConfiguredCannotBeInstalledFrom(t *testing.T) {
	uc, _, _, _, systemRoot, _ := newTestUC(t)

	_, err := uc.Install(context.Background(), registryCaller(), registrySource("0.2.0"))
	if err == nil {
		t.Fatal("installed from a registry this control plane was never told about")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error %q does not say the registry is unconfigured", err)
	}
	assertNotInstalled(t, systemRoot, registryPack)
}

// The request carries a URL field, because Source is one struct for every
// source type. If that field were honoured here, an admin could install any
// tarball while the ledger recorded it as having come from an allowlisted
// registry — which is exactly what the allowlist is for.
//
// So the assertion is not "it fails". The field is ignored, which means the
// install succeeds against the index's own url, and what must be checked is
// *which* package arrived.
func TestAURLHandedInWithARegistryInstallIsIgnored(t *testing.T) {
	packDir, digest := stageAndPublish(t)
	reg := serveRegistry(t, packDir, registryManifest, "0.2.0", digest)

	// A second, perfectly good pack, reachable and installable in its own
	// right. If the caller's url were honoured, this is what would land.
	otherDir := t.TempDir()
	writeTestPack(t, otherDir, "somebody-elses-pack")
	otherBody := packTarball(t, filepath.Join(otherDir, "somebody-elses-pack"))
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(otherBody)
	}))
	t.Cleanup(otherSrv.Close)

	uc, _, _, _, systemRoot, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: reg.indexURL}}

	src := registrySource("0.2.0")
	src.URL = otherSrv.URL + "/somebody-elses-pack.tgz"

	res, err := uc.Install(context.Background(), registryCaller(), src)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Pack.PackID != registryPack {
		t.Errorf("pack_id = %q; the caller's url was honoured, so a registry install can deliver "+
			"any tarball while the ledger names an allowlisted registry", res.Pack.PackID)
	}
	if _, err := os.Stat(filepath.Join(systemRoot, "somebody-elses-pack")); err == nil {
		t.Error("the caller's pack was installed despite the request being a registry install")
	}
}

// The pack name becomes a directory name, so a name carrying a separator
// would place the extraction outside the staging area.
//
// The check has to run *before* the index lookup, and this is the test that
// says so: an index whose row carried such a name is refused by ParseIndex,
// so a lookup-first implementation would fail this test for the wrong reason
// while never exercising the guard. Each name here therefore gets refused by
// the shape check, and the message has to name the field — a refusal that
// says "does not list …" would be a reader being told about the wrong thing.
func TestAPackIDThatIsNotAPackageNameIsRefusedBeforeAnythingIsFetched(t *testing.T) {
	packDir, digest := stageAndPublish(t)
	reg := serveRegistry(t, packDir, registryManifest, "0.2.0", digest)

	uc, _, _, _, systemRoot, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: reg.indexURL}}

	for _, bad := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "nested/pack"} {
		src := registrySource("0.2.0")
		src.PackID = bad
		_, err := uc.Install(context.Background(), registryCaller(), src)
		if err == nil {
			t.Fatalf("accepted pack_id %q, which is not a package name", bad)
		}
		if !strings.Contains(err.Error(), "not a usable package name") {
			t.Errorf("pack_id %q was refused with %q, which does not name the field that is wrong", bad, err)
		}
	}
	assertNotInstalled(t, systemRoot, registryPack)
}

func assertNotInstalled(t *testing.T, systemRoot, packID string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(systemRoot, packID)); err == nil {
		t.Errorf("%s is on disk after a refused install", packID)
	}
}

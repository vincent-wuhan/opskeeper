package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigrpc"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The install path's tests are about what survives.
//
// Every one of these cases is the same shape — a package arrives, and
// something about it is wrong — and the assertion is always two-part: the
// package did not become active, and the set that was active before is
// still exactly the set that is active after. The second half is the one
// that matters. A store that refuses a bad package but has already
// disturbed the good ones is a store that took a capability away, and the
// model cannot tell that from a tool that has stopped working.

// ---------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------

// archiveEntry is one member of a test tarball.
type archiveEntry struct {
	name string
	body string
	// typeflag defaults to a regular file.
	typeflag byte
	linkname string
	mode     int64
}

// buildTarGz assembles a tarball from entries.
//
// It is written out rather than reusing the production packer because the
// tests need to produce archives the production code should *refuse* — a
// symlink, a traversal, two top-level directories — and a packer that can
// only make well-formed archives cannot express those.
func buildTarGz(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name: e.name, Mode: mode, Size: int64(len(e.body)),
			Typeflag: typeflag, Linkname: e.linkname,
		}
		if typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// manifestYAML is a valid L1 governance manifest. Callers that need it
// wrong override the piece they are testing.
func manifestYAML(name, version, level, class string) string {
	return `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: ` + name + `
  version: ` + version + `
  vendor: acme
  homepage: https://example.invalid/` + name + `
spec:
  targets: [edge]
  safety_level: ` + level + `
  capabilities: [` + class + `]
  tools:
    - {name: host_probe_tcp, class: ` + class + `}
  required_scopes:
    - host.read
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`
}

// packageTar is a well-formed package archive: one top-level directory
// holding a manifest and a source file.
func packageTar(t *testing.T, name, version string) []byte {
	t.Helper()
	return buildTarGz(t,
		archiveEntry{name: name + "/", typeflag: tar.TypeDir, mode: 0o755},
		archiveEntry{name: name + "/" + pluginmanifest.ManifestFile, body: manifestYAML(name, version, "L1", "read")},
		archiveEntry{name: name + "/extensions/tool/tools.go", body: "package tool\n"},
	)
}

// signedPackageTar signs a package archive and returns the archive, its
// spec and a trust store that trusts the signer.
//
// The signature is computed over the *extracted* tree, so the archive is
// built first, unpacked into a scratch directory, signed there, and then
// repacked. Signing a directory and then packing it separately would
// produce a tree whose digest does not describe the bytes that ship, and
// the node would refuse a package that is perfectly correct — which is the
// kind of bug that reads as a trust-store problem and is not one.
// testKeys memoises one signing key per test.
//
// It has to be one key rather than one per package, because a trust store
// is a set of keys and a package is signed by whoever released it. A
// helper that generated a fresh key per call would produce a store holding
// a *different* key under the same key id, and every signature would fail
// with "signed by a key this node does not hold" — a refusal that is
// correct, that says nothing about the property under test, and that hides
// every real one behind it.
//
// Keyed by *testing.T rather than by package name, because the two
// packages a test builds are meant to come from one publisher. A test that
// wanted a second publisher asks for one explicitly.
var testKeys sync.Map // map[*testing.T]*pluginmanifest.Signer

func testSigner(t *testing.T) *pluginmanifest.Signer {
	t.Helper()
	if v, ok := testKeys.Load(t); ok {
		return v.(*pluginmanifest.Signer)
	}
	signer, _, err := pluginmanifest.GenerateSigner("acme-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	testKeys.Store(t, signer)
	t.Cleanup(func() { testKeys.Delete(t) })
	return signer
}

func signedPackageTar(t *testing.T, name, version string, mutate func(root string)) (body []byte, spec ports.PluginSpec, store *pluginmanifest.TrustStore) {
	t.Helper()
	signer := testSigner(t)

	scratch := t.TempDir()
	buildDir := filepath.Join(scratch, "build")
	if err := os.MkdirAll(buildDir, 0o750); err != nil {
		t.Fatalf("build dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, pluginmanifest.ManifestFile),
		[]byte(manifestYAML(name, version, "L1", "read")), 0o640); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(buildDir, "extensions", "tool"), 0o750); err != nil {
		t.Fatalf("ext dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "extensions", "tool", "tools.go"),
		[]byte("package tool\n"), 0o640); err != nil {
		t.Fatalf("tools: %v", err)
	}
	if mutate != nil {
		mutate(buildDir)
	}

	env, err := signer.Sign(buildDir)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(buildDir); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	store = pluginmanifest.NewTrustStore()
	if err := store.Trust(signer.KeyID(), signer.PublicKey()); err != nil {
		t.Fatalf("Trust: %v", err)
	}

	// Pack the signed tree.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	root := filepath.Base(buildDir)
	err = filepath.WalkDir(buildDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Dir(buildDir), path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Typeflag: tar.TypeReg}
		if d.IsDir() {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		} else {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hdr.Size = int64(len(content))
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			_, err = tw.Write(content)
			return err
		}
		return tw.WriteHeader(hdr)
	})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	_ = root

	sum := sha256.Sum256(buf.Bytes())
	spec = ports.PluginSpec{
		Name: name, Version: version,
		URL:       "https://releases.invalid/" + name + ".tar.gz",
		SHA256:    hex.EncodeToString(sum[:]),
		Signature: env.Transport(),
		KeyID:     signer.KeyID(),
	}
	return buf.Bytes(), spec, store
}

// newStore builds a store over temp directories with a fixed fetcher.
func newStore(t *testing.T, body []byte, trust *pluginmanifest.TrustStore, pol pluginmanifest.Policy) *pluginStore {
	t.Helper()
	dir := t.TempDir()
	return &pluginStore{
		pkgDir:  filepath.Join(dir, "packages"),
		workDir: filepath.Join(dir, "work"),
		trust:   trust,
		policy:  pol,
		fetch: func(_ context.Context, _ string) ([]byte, error) {
			return body, nil
		},
	}
}

// TestMain gives every test in this package a node that knows what
// version it runs.
//
// The default matters: this repository's binary carries "dev" unless it is
// built with -ldflags, and "dev" is not a version — so without this every
// fixture package, all of which declare min_edge_version, would be refused
// for a reason the test is not about. The fail-closed case is asserted
// explicitly by the tests that clear this variable.
func TestMain(m *testing.M) {
	if os.Getenv("OPSKEEPER_EDGE_VERSION") == "" {
		_ = os.Setenv("OPSKEEPER_EDGE_VERSION", testEdgeVersion)
	}
	// The agent's version is a second number from a second binary. A
	// fixture node that stated only the edge's would refuse every package
	// that declares a min_pig_version — and, worse, would look like it had
	// checked something.
	if os.Getenv(pigVersionEnv) == "" {
		_ = os.Setenv(pigVersionEnv, testPigVersion)
	}
	os.Exit(m.Run())
}

// testEdgeVersion is what the fixtures report as the node's own agent
// version. It is above every min_edge_version the helpers write (0.1.0),
// so a test about admission is not also a test about compatibility.
const testEdgeVersion = "0.8.0"

// testPigVersion is what the fixtures report as the PiG build they launch.
//
// It used to be a literal, and the literal is the bug this comment exists to
// describe. It read 0.3.0 while every shipped package declares
// min_pig_version 0.4.0, so a node fixture built from this tree refused its
// own boot packages "for a reason the test is not about" — which is exactly
// what the TestMain comment below was written to prevent, arriving anyway
// through the other door.
//
// So the number is read out of the root go.mod, which is the only place that
// decides which PiG this tree is built against. A PiG bump now moves the
// fixtures with it. A go.mod that cannot be read is a hard stop, because the
// alternative is falling back to a number that is wrong in exactly the quiet
// way that caused this.
var testPigVersion = pinnedPigVersion()

// pinnedPigVersion is the PiG version the root go.mod requires.
func pinnedPigVersion() string {
	raw, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugininstall_test: read the root go.mod: %v\n"+
			"the fixture node's PiG version comes from it; without it every version "+
			"below is a guess\n", err)
		os.Exit(1)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		// At least two fields, not exactly two: the requirement is
		// written with a trailing "// indirect" in a module that does
		// not import PiG directly, and a parser that insists on two
		// fields reports "no PiG here" for a tree that plainly has one.
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "github.com/MichaelKinsy/PiG" &&
			strings.HasPrefix(fields[1], "v") {
			return strings.TrimPrefix(fields[1], "v")
		}
	}
	fmt.Fprintln(os.Stderr,
		"plugininstall_test: the root go.mod requires no github.com/MichaelKinsy/PiG; "+
			"the tree and its fixtures have stopped agreeing about which PiG this is")
	os.Exit(1)
	return ""
}

// withEdgeVersion makes a store behave like a node that knows what
// version it runs.
//
// The version is read from the environment on every request rather than
// held on the store, deliberately: a node that is upgraded in place must
// start enforcing the new version immediately, and a captured value would
// keep the old ceiling until the next restart — which is exactly the
// window in which a bad release is most likely to arrive. So the fixture
// sets the environment, not a field, and the tests that check the
// fail-closed default clear it.
func withEdgeVersion(t *testing.T) {
	t.Helper()
	t.Setenv("OPSKEEPER_EDGE_VERSION", testEdgeVersion)
}

// readOnlyNode is a policy that admits an L1 read package — the shape a
// normally-provisioned node has. The scopes are the first-party read-only
// package's, so a test that copies a real bundle in can admit it without
// widening the policy to fit whatever the test happens to be checking.
func readOnlyNode() pluginmanifest.Policy {
	return pluginmanifest.PolicyFor(domain.SafetyL1, domain.RadiusNone,
		domain.Scopes{domain.ScopeHostRead, domain.ScopeTopologyRO, domain.ScopeAlertRO})
}

// namesOf renders a set for comparison.
func namesOf(infos []ports.PluginInfo) []string {
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Name+"@"+i.Version)
	}
	return out
}

// publishedPackages reads what the agent's settings file actually says.
//
// The store's own view is not the point. The property that matters is
// whether the *agent* was told, so the test reads the file the agent reads
// rather than asking the code that wrote it.
func publishedPackages(t *testing.T, s *pluginStore) []string {
	t.Helper()
	settings := filepath.Join(s.workDir, agentConfigDirName(), agentSettingsFile)
	body, err := os.ReadFile(settings)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read settings: %v", err)
	}
	var parsed agentSettings
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	out := make([]string, 0, len(parsed.Packages))
	for _, p := range parsed.Packages {
		out = append(out, filepath.Base(strings.TrimSuffix(p, "/")))
	}
	return out
}

// ---------------------------------------------------------------------
// the happy path
// ---------------------------------------------------------------------

func TestASignedPackageThatFitsTheNodeIsInstalledAndPublished(t *testing.T) {
	body, spec, store := signedPackageTar(t, "acme-probe", "1.2.0", nil)
	s := newStore(t, body, store, readOnlyNode())

	got := s.Install(t.Context(), spec)
	if !got.Installed {
		t.Fatalf("a signed L1 package was not installed: %+v", got)
	}
	if got.Refused != "" || got.Error != "" {
		t.Errorf("a successful install reported a problem: %+v", got)
	}
	if got.Digest == "" {
		t.Error("the install reported no digest, so a manager cannot tell what landed")
	}
	if want := []string{"acme-probe@1.2.0"}; !equalStrings(namesOf(s.Installed()), want) {
		t.Errorf("installed = %v, want %v", namesOf(s.Installed()), want)
	}
	// And the agent was told, which is the only part the model sees.
	if want := []string{"acme-probe-1.2.0"}; !equalStrings(publishedPackages(t, s), want) {
		t.Errorf("the agent's package list is %v, want %v — the store and the agent disagree, so the "+
			"package is installed and invisible", publishedPackages(t, s), want)
	}
}

func TestAPackageThatDoesNotMatchTheNameItWasOfferedAsIsRefused(t *testing.T) {
	// The manifest is authenticated — it is inside the signed tree — so
	// when it disagrees with the request, one of the two is a lie and the
	// node cannot tell which. Refusing is the only safe reading: a
	// release installed under a version it does not claim cannot be rolled
	// back to, and a release installed under the wrong name makes the
	// manager's own bookkeeping wrong in a way that looks like a node
	// that intermittently loses packages.
	body, spec, _ := signedPackageTar(t, "acme-probe", "1.2.0", nil)
	spec.Version = "9.9.9" // the manager mislabelled what it is shipping

	s, _, _, _ := seed(t)
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatal("a package whose manifest says a different version was installed")
	}
	if !strings.Contains(got.Refused, "9.9.9") || !strings.Contains(got.Refused, "1.2.0") {
		t.Errorf("refusal = %q, want it to name both the offered and the declared version", got.Refused)
	}
	assertUntouched(t, s)
}

func TestTheSamePackageArrivingTwiceConvergesRatherThanFailing(t *testing.T) {
	// A rollout retried against a node that installed and then lost the
	// reply must converge, not error. The digest was already checked, so
	// what is on disk is what was offered, and "already there" is the
	// correct answer rather than a failure a manager would retry forever.
	body, spec, store := signedPackageTar(t, "acme-probe", "1.2.0", nil)
	s := newStore(t, body, store, readOnlyNode())

	if first := s.Install(t.Context(), spec); !first.Installed {
		t.Fatalf("first install: %+v", first)
	}
	second := s.Install(t.Context(), spec)
	if !second.Installed {
		t.Errorf("the second, identical install reported %+v, want it to converge", second)
	}
}

// ---------------------------------------------------------------------
// refusals, and the set that must survive them
// ---------------------------------------------------------------------

// seed installs a good package and returns the store, so a test can assert
// that a later refusal left it alone.
func seed(t *testing.T) (*pluginStore, []byte, ports.PluginSpec, *pluginmanifest.TrustStore) {
	t.Helper()
	withEdgeVersion(t)
	body, spec, store := signedPackageTar(t, "acme-keep", "1.0.0", nil)
	s := newStore(t, body, store, readOnlyNode())
	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("seed install: %+v", got)
	}
	// Every later package in this test is signed with the same key, and
	// the store holds it, so a refusal a test asserts on is a refusal
	// about the thing under test rather than about the fixture.
	return s, body, spec, store
}

// assertUntouched is the assertion every refusal test ends with.
func assertUntouched(t *testing.T, s *pluginStore) {
	t.Helper()
	if want := []string{"acme-keep@1.0.0"}; !equalStrings(namesOf(s.Installed()), want) {
		t.Errorf("the active set is %v, want %v — a refused install changed what the node has",
			namesOf(s.Installed()), want)
	}
	if want := []string{"acme-keep-1.0.0"}; !equalStrings(publishedPackages(t, s), want) {
		t.Errorf("the agent's package list is %v, want %v", publishedPackages(t, s), want)
	}
	assertNoStagingDebris(t, s)
}

// assertNoStagingDebris checks that a failed install left nothing behind.
func assertNoStagingDebris(t *testing.T, s *pluginStore) {
	t.Helper()
	entries, err := os.ReadDir(s.pkgDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read the package directory: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging") {
			t.Errorf("a staging directory %q survived a failed install; a later boot could pick it up "+
				"as a package that no review ever passed", e.Name())
		}
	}
}

func TestAnUnsignedPackageIsRefusedOnAProvisionedNode(t *testing.T) {
	// The node has a trust store, so it has an opinion about who may
	// publish for it, and a package with no signature is a package from
	// nobody. This is the case the whole signing scheme exists for, and it
	// is asserted through the *install* path rather than the review alone,
	// because the install path is what a manager actually reaches.
	_, _, spec, store := seed(t)
	// The good package's fetcher is replaced with one serving an unsigned
	// body for a different package.
	_ = spec
	other, otherSpec, otherStore := signedPackageTar(t, "acme-sneaky", "0.1.0", nil)
	_ = otherStore

	// A body with no pig-ops.sig at all.
	unsigned := buildTarGz(t,
		archiveEntry{name: "acme-sneaky/", typeflag: tar.TypeDir, mode: 0o755},
		archiveEntry{name: "acme-sneaky/" + pluginmanifest.ManifestFile, body: manifestYAML("acme-sneaky", "0.1.0", "L1", "read")},
		archiveEntry{name: "acme-sneaky/extensions/tool/tools.go", body: "package tool\n"},
	)
	sum := sha256.Sum256(unsigned)
	otherSpec.URL = "https://releases.invalid/acme-sneaky.tar.gz"
	otherSpec.SHA256 = hex.EncodeToString(sum[:])
	_ = other

	// The node still trusts acme-2026, so AllowUnsigned is false and the
	// package has to carry a signature it does not carry.
	_ = store
	s := seedStore(t, unsigned, otherSpec, signedStore(t))
	got := s.Install(t.Context(), otherSpec)
	if got.Installed {
		t.Fatal("an unsigned package was installed on a node that has a trust store")
	}
	if got.Refused == "" {
		t.Errorf("the refusal carried no reason: %+v", got)
	}
	assertUntouched(t, s)
}

func TestABodyWhoseDigestDoesNotMatchIsRefused(t *testing.T) {
	// The package itself is valid and correctly signed. Only the digest
	// in the request is wrong — which is exactly the shape of a mirror
	// serving one body under another's name, and exactly the case the
	// transport digest exists for.
	//
	// The test deliberately does not use a corrupt or unsigned body. Such
	// a body is caught later anyway, by the signature step, so a test
	// using one would still pass if the digest check were deleted — and
	// would pass for the wrong reason, which is the failure mode a test
	// written to be mutation-sensitive exists to prevent.
	body, spec, _ := signedPackageTar(t, "acme-probe", "1.2.0", nil)
	spec.SHA256 = strings.Repeat("0", 64)

	s, _, _, _ := seed(t)
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatal("a correctly-signed body was installed under a digest it does not match")
	}
	if !strings.Contains(got.Refused, "digest") {
		t.Errorf("refusal = %q, want it to say the digest did not match", got.Refused)
	}
	assertUntouched(t, s)
}

func TestAPackageAboveTheNodeCeilingIsRefused(t *testing.T) {
	// A well-signed L2 package on a node whose ceiling is L1. The node is
	// the one that refuses, not the manager: the manager offered it, the
	// manager believes it is fine, and the manager is not the party that
	// gets to decide what this node may run.
	body, spec, store := signedPackageTar(t, "acme-repair", "2.0.0", func(root string) {
		writeFile(t, filepath.Join(root, pluginmanifest.ManifestFile),
			manifestYAML("acme-repair", "2.0.0", "L2", "write"))
	})
	s := seedStore(t, body, spec, store)

	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatal("an L2 package was installed on an L1 node")
	}
	if got.Refused == "" {
		t.Errorf("the refusal carried no reason: %+v", got)
	}
	// The refusal has to name the ceiling, because "it was refused" is
	// not something an operator can act on and "your node policy is L1"
	// is.
	if !strings.Contains(strings.ToLower(got.Refused), "l1") {
		t.Errorf("refusal = %q, want it to name the node's ceiling", got.Refused)
	}
	assertUntouched(t, s)
}

func TestAManagerTargetedPackageIsRefusedByTheInstallPathToo(t *testing.T) {
	// A manager-side plugin, well-signed, offered to a node. The manifest
	// validates; the targets do not include the edge. A declaration the
	// node does not honour is worse than none at all, because it reads as
	// "reviewed for the control plane" while running on a host.
	body, spec, store := signedPackageTar(t, "acme-control", "1.0.0", func(root string) {
		writeFile(t, filepath.Join(root, pluginmanifest.ManifestFile),
			strings.Replace(manifestYAML("acme-control", "1.0.0", "L1", "read"),
				"targets: [edge]", "targets: [manager]", 1))
	})
	s := seedStore(t, body, spec, store)

	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatal("a manager-targeted package was installed on an edge")
	}
	if !strings.Contains(got.Refused, "does not run on an edge") {
		t.Errorf("refusal = %q, want it to say the package does not target the edge", got.Refused)
	}
	assertUntouched(t, s)
}

func TestAStagedPackageWithMoreThanOneTopLevelDirectoryIsRefused(t *testing.T) {
	// The node picks the single directory to review. An archive with two
	// is not a package, and guessing which one was meant is how a package
	// gets reviewed in one directory and installed from another.
	tarball := buildTarGz(t,
		archiveEntry{name: "acme-probe/", typeflag: tar.TypeDir, mode: 0o755},
		archiveEntry{name: "acme-probe/" + pluginmanifest.ManifestFile, body: manifestYAML("acme-probe", "1.0.0", "L1", "read")},
		archiveEntry{name: "decoy/", typeflag: tar.TypeDir, mode: 0o755},
		archiveEntry{name: "decoy/payload.sh", body: "rm -rf /\n"},
	)
	sum := sha256.Sum256(tarball)
	spec := ports.PluginSpec{
		Name: "acme-probe", Version: "1.0.0",
		URL: "https://releases.invalid/x.tar.gz", SHA256: hex.EncodeToString(sum[:]),
		Signature: "irrelevant: the archive is refused before the signature is read",
	}
	s := seedStore(t, tarball, spec, signedStore(t))
	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatal("an archive with two top-level directories was installed")
	}
	if !strings.Contains(got.Refused, "top-level") {
		t.Errorf("refusal = %q, want it to say the archive is not a single package", got.Refused)
	}
	assertUntouched(t, s)
}

func TestAnArchiveThatEscapesItsStagingDirectoryIsRefused(t *testing.T) {
	tarball := buildTarGz(t,
		archiveEntry{name: "acme-probe/", typeflag: tar.TypeDir, mode: 0o755},
		archiveEntry{name: "acme-probe/" + pluginmanifest.ManifestFile, body: manifestYAML("acme-probe", "1.0.0", "L1", "read")},
		archiveEntry{name: "../escaped.sh", body: "echo pwned\n"},
	)
	sum := sha256.Sum256(tarball)
	spec := ports.PluginSpec{
		Name: "acme-probe", Version: "1.0.0",
		URL: "https://releases.invalid/x.tar.gz", SHA256: hex.EncodeToString(sum[:]),
		Signature: "irrelevant: the archive is refused before the signature is read",
	}
	s := seedStore(t, tarball, spec, signedStore(t))
	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatal("an archive with a path-traversal entry was installed")
	}
	if !strings.Contains(got.Refused, "escapes") {
		t.Errorf("refusal = %q, want it to name the escaping entry", got.Refused)
	}
	assertUntouched(t, s)
	// And the escaped file is not on disk, anywhere the node can see.
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.pkgDir), "escaped.sh")); err == nil {
		t.Error("the archive wrote outside its staging directory")
	}
}

func TestAnArchiveCarryingASymlinkIsRefused(t *testing.T) {
	// A symlink in a plugin package is never needed, and it is the
	// classic way to make a later write land somewhere else.
	tarball := buildTarGz(t,
		archiveEntry{name: "acme-probe/", typeflag: tar.TypeDir, mode: 0o755},
		archiveEntry{name: "acme-probe/" + pluginmanifest.ManifestFile, body: manifestYAML("acme-probe", "1.0.0", "L1", "read")},
		archiveEntry{name: "acme-probe/link", typeflag: tar.TypeSymlink, linkname: "/etc/shadow"},
	)
	sum := sha256.Sum256(tarball)
	spec := ports.PluginSpec{
		Name: "acme-probe", Version: "1.0.0",
		URL: "https://releases.invalid/x.tar.gz", SHA256: hex.EncodeToString(sum[:]),
		Signature: "irrelevant: the archive is refused before the signature is read",
	}
	s := seedStore(t, tarball, spec, signedStore(t))
	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatal("an archive carrying a symlink was installed")
	}
	if !strings.Contains(got.Refused, "symlink") && !strings.Contains(got.Refused, "files and directories") {
		t.Errorf("refusal = %q, want it to name the symlink", got.Refused)
	}
	assertUntouched(t, s)
}

// ---------------------------------------------------------------------
// the request itself
// ---------------------------------------------------------------------

func TestARequestThisNodeCannotActOnSafelyIsRefusedBeforeAnythingIsFetched(t *testing.T) {
	// Every one of these is refused on the request rather than on the
	// body, so a node that received them fetched nothing. The fetcher
	// panics if it is called, which is how "nothing was fetched" is
	// asserted rather than assumed.
	_, _, good := signedPackageTar(t, "acme-probe", "1.0.0", nil)

	for _, tc := range []struct {
		what string
		spec ports.PluginSpec
		want string
	}{
		{"no name", ports.PluginSpec{Version: "1.0.0", URL: "https://x.invalid/a", SHA256: strings.Repeat("a", 64), Signature: "s"}, "no package name"},
		{"no version", ports.PluginSpec{Name: "a", URL: "https://x.invalid/a", SHA256: strings.Repeat("a", 64), Signature: "s"}, "no version"},
		{"a traversing name", ports.PluginSpec{Name: "../escape", Version: "1.0.0", URL: "https://x.invalid/a", SHA256: strings.Repeat("a", 64), Signature: "s"}, "not a plain name"},
		{"a traversing version", ports.PluginSpec{Name: "a", Version: "../../etc", URL: "https://x.invalid/a", SHA256: strings.Repeat("a", 64), Signature: "s"}, "not a plain version"},
		{"no signature", ports.PluginSpec{Name: "a", Version: "1.0.0", URL: "https://x.invalid/a", SHA256: strings.Repeat("a", 64)}, "no signature"},
		{"a short digest", ports.PluginSpec{Name: "a", Version: "1.0.0", URL: "https://x.invalid/a", SHA256: "abc", Signature: "s"}, "64 hex"},
		{"a non-hex digest", ports.PluginSpec{Name: "a", Version: "1.0.0", URL: "https://x.invalid/a", SHA256: strings.Repeat("z", 64), Signature: "s"}, "not hex"},
		{"a file URL", ports.PluginSpec{Name: "a", Version: "1.0.0", URL: "file:///etc/passwd", SHA256: strings.Repeat("a", 64), Signature: "s"}, "http or https"},
		{"a scheme-less URL", ports.PluginSpec{Name: "a", Version: "1.0.0", URL: "releases.invalid/a.tar.gz", SHA256: strings.Repeat("a", 64), Signature: "s"}, "http or https"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			dir := t.TempDir()
			s := &pluginStore{
				pkgDir:  filepath.Join(dir, "packages"),
				workDir: filepath.Join(dir, "work"),
				trust:   signedStore(t),
				policy:  readOnlyNode(),
				fetch: func(context.Context, string) ([]byte, error) {
					panic("the store fetched a request it should have refused on its own terms")
				},
			}
			got := s.Install(t.Context(), tc.spec)
			if got.Installed {
				t.Fatalf("a request with %s was installed", tc.what)
			}
			if !strings.Contains(got.Refused, tc.want) {
				t.Errorf("refusal = %q, want it to mention %q", got.Refused, tc.want)
			}
		})
	}
	_ = good
}

func TestANodeThatCannotReadItsTrustStoreInstallsNothingThatNeedsVerifying(t *testing.T) {
	// A configured-but-unreadable trust store is a hard error, not a fall
	// back to unsigned. The operator asked for verification and the node
	// cannot do it, and quietly running unsigned is the one answer that is
	// certainly wrong.
	body, spec, _ := signedPackageTar(t, "acme-probe", "1.0.0", nil)
	// The shape TrustStoreFromConfig produces on a bad path: no keys were
	// loaded, and there is an error saying why. A store that had managed
	// to load a key *and* fail would be a different question, and this is
	// not it.
	broken := pluginmanifest.NewTrustStore()
	broken.SetLoadError(errors.New("permission denied"))

	s, _, _, _ := seed(t)
	s.trust = broken
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatal("a node with an unreadable trust store installed a package that needed verifying")
	}
	if !strings.Contains(got.Refused+got.Error, "permission denied") {
		t.Errorf("reason = %q, want it to carry the trust store's own error", got.Refused+got.Error)
	}
	assertUntouched(t, s)
}

// ---------------------------------------------------------------------
// what an install replaced
// ---------------------------------------------------------------------

func TestAnInstallSaysWhatItReplaced(t *testing.T) {
	// The manager cannot work this out for itself. The only list the node
	// reports afterwards has the new version in it, so a manager that
	// derived the previous version from the post-state would be deriving
	// it from the one piece of information that changed — and a rollback
	// built on that would restore the release it is rolling back.
	//
	// So the node has to say it, and it can only say it at the moment it
	// knows, which is before the old directory is overwritten.
	s, _, _, _ := seed(t)
	body, spec, _ := signedPackageTar(t, "acme-keep", "2.0.0", nil)
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }

	got := s.Install(t.Context(), spec)
	if !got.Installed {
		t.Fatalf("the upgrade did not install: %+v", got)
	}
	if got.Replaced == nil {
		t.Fatal("the upgrade reported no Replaced, so a rollback would leave the node on nothing")
	}
	if got.Replaced.Version != "1.0.0" {
		t.Errorf("Replaced.Version = %q, want the 1.0.0 the node was on", got.Replaced.Version)
	}
}

func TestAFirstInstallReplacesNothing(t *testing.T) {
	// The nil is load-bearing in the other direction: a node reporting
	// Replaced for a package it did not have would make a rollback
	// "restore" a version that never existed.
	s, _, _, _ := seed(t)
	body, spec, _ := signedPackageTar(t, "acme-new", "1.0.0", nil)
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }

	got := s.Install(t.Context(), spec)
	if !got.Installed {
		t.Fatalf("install: %+v", got)
	}
	if got.Replaced != nil {
		t.Errorf("a first install reported Replaced = %+v, want nil", got.Replaced)
	}
}

func TestReinstallingTheSameVersionReplacesNothing(t *testing.T) {
	// A re-install of what the node is already running is not an upgrade.
	// Reporting it as having replaced itself would make a rollback
	// restore the version it just removed — which is a rollback that
	// appears to work and leaves the node on the bad build.
	s, _, spec, _ := seed(t)
	got := s.Install(t.Context(), spec)
	if !got.Installed {
		t.Fatalf("re-install: %+v", got)
	}
	if got.Replaced != nil {
		t.Errorf("re-installing the same version reported Replaced = %+v, want nil", got.Replaced)
	}
}

// ---------------------------------------------------------------------
// the boot bundle
// ---------------------------------------------------------------------

func TestARuntimeInstallDoesNotDropTheBundleTheNodeBootedWith(t *testing.T) {
	// The settings file is written from scratch on every republish. A
	// store that wrote only what it installed would drop the operator's
	// boot bundle the first time anybody installed a plugin at run time,
	// and the symptom would be a node whose agent has quietly lost the
	// read-only package it has been running with since it booted.
	//
	// The bundle is built by copying a real shipped package, because the
	// property is about what the store does with a package it did not
	// install, and a synthetic one would not exercise the review.
	bundle := copyShippedPackage(t, "opskeeper-sre-readonly")

	body, spec, store := signedPackageTar(t, "acme-extra", "1.0.0", nil)
	s := newStore(t, body, store, readOnlyNode())
	s.base = []string{bundle}

	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("install: %+v", got)
	}
	// The bundle keeps its own directory name — the store does not rename
	// what the operator provisioned — while the runtime package gets the
	// "<name>-<version>" name the store gives everything it installs.
	published := publishedPackages(t, s)
	for _, want := range []string{"opskeeper-sre-readonly", "acme-extra-1.0.0"} {
		found := false
		for _, got := range published {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the agent's package list is %v, want it to include %q", published, want)
		}
	}
}

func TestABootPackageThatNoLongerReviewsFailsTheRepublishRatherThanBeingDropped(t *testing.T) {
	// The opposite of the runtime packages, and deliberate. A runtime
	// package that stops passing is dropped and reported, because the
	// node is refusing to run code it no longer trusts. A *boot* package
	// that stops passing is an operator's configuration that has become
	// invalid, and the node does not get to quietly decide the operator
	// no longer wanted it.
	bundle := copyShippedPackage(t, "opskeeper-sre-readonly")
	body, spec, store := signedPackageTar(t, "acme-extra", "1.0.0", nil)

	s := newStore(t, body, store, readOnlyNode())
	s.base = []string{bundle}
	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("install: %+v", got)
	}

	// The bundle's signature stops describing its tree.
	writeFile(t, filepath.Join(bundle, pluginmanifest.SignatureFile),
		`{"key_id":"acme-2026","algorithm":"ed25519","name":"opskeeper-sre-readonly","version":"0.1.0",`+
			`"tree_digest":"`+strings.Repeat("e", 64)+`","signature":"bm90LWEtc2lnbmF0dXJl"}`)

	err := s.republish()
	if err == nil {
		t.Fatal("a boot package that no longer passes review was silently dropped")
	}
	if !strings.Contains(err.Error(), "boot package") {
		t.Errorf("error = %q, want it to say this is a boot package rather than a runtime one", err)
	}
	// And nothing was written: the list on disk is still the last good one,
	// which is the safe direction to fail in.
	if !equalStrings(publishedPackages(t, s), []string{"opskeeper-sre-readonly", "acme-extra-1.0.0"}) {
		t.Errorf("the agent's package list is %v, want the last good list left alone",
			publishedPackages(t, s))
	}
}

// ---------------------------------------------------------------------
// removal
// ---------------------------------------------------------------------

func TestRemoveTakesAPackageOffAndRepublishesTheList(t *testing.T) {
	s, _, _, _ := seed(t)

	// The version is the one on the node, so the guard passes and the
	// package comes off. A caller that passes the wrong version is
	// refused, which the test below locks down.
	got := s.Remove(t.Context(), "acme-keep", "1.0.0")
	if got.Installed {
		t.Errorf("remove reported %+v, want Installed false — a package that was just removed is gone",
			got)
	}
	if got.Note != "removed" {
		t.Errorf("remove reported note %q, want it to say what it did", got.Note)
	}
	if names := namesOf(s.Installed()); len(names) != 0 {
		t.Errorf("installed = %v, want empty", names)
	}
	if names := publishedPackages(t, s); len(names) != 0 {
		t.Errorf("the agent's package list is %v, want empty — the store and the agent now disagree", names)
	}
}

func TestRemovingAPackageThatIsNotThereIsNotAnError(t *testing.T) {
	// A rollback that has already happened should succeed, and a manager
	// that removes a package it believes it installed should not be told
	// off for being right about nothing.
	s, _, _, _ := seed(t)

	got := s.Remove(t.Context(), "acme-never-installed", "")
	if got.Error != "" {
		t.Errorf("removing an absent package failed: %+v", got)
	}
	if got.Installed {
		t.Error("the response reports the package as installed, which it is not")
	}
	if got.Note == "" {
		t.Error("the response carries no note, so a caller cannot tell an absent package from a failed remove")
	}
	assertUntouched(t, s)
}

func TestRemoveFindsAPackageWhoseNameContainsADash(t *testing.T) {
	// The directory is named "<name>-<version>", so an implementation
	// that recovers the name by splitting the directory string fails on
	// exactly the names this repository uses. Every package here has a
	// dash, so the bug would have been found immediately in production
	// and not at all in a test whose package was called "a".
	s, _, _, _ := seed(t)
	body, spec, store := signedPackageTar(t, "acme-long-name-two", "3.1.4", nil)
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("install: %+v", got)
	}
	_ = store

	if got := s.Remove(t.Context(), "acme-long-name-two", "3.1.4"); got.Installed || got.Note != "removed" {
		t.Errorf("remove reported %+v, want the dashed package gone", got)
	}
	if want := []string{"acme-keep@1.0.0"}; !equalStrings(namesOf(s.Installed()), want) {
		t.Errorf("installed = %v, want %v", namesOf(s.Installed()), want)
	}
}

func TestRemoveRefusesWhenTheInstalledVersionIsNotTheOneAskedFor(t *testing.T) {
	// This is the guard that makes a rollback a rollback. A rollback that
	// raced a newer release sends "remove 1.0.0" to a node now running
	// 2.0.0; removing what is actually there would take the fleet back to
	// nothing while the operator watched a success. So the node refuses,
	// changes nothing, and says which version it found.
	s, _, _, _ := seed(t)

	got := s.Remove(t.Context(), "acme-keep", "0.9.0")
	if got.Refused == "" {
		t.Fatalf("removing with a version that is not installed reported %+v, want a refusal", got)
	}
	if got.Installed {
		t.Error("a refused remove still reports Installed, so a manager cannot tell what survived")
	}
	if got.Version != "1.0.0" {
		t.Errorf("refusal reported version %q, want the version actually installed (1.0.0)", got.Version)
	}
	// The package is untouched. A guard that refused *after* deleting
	// would be a guard in name only.
	if want := []string{"acme-keep@1.0.0"}; !equalStrings(namesOf(s.Installed()), want) {
		t.Errorf("installed = %v, want the package left alone", namesOf(s.Installed()))
	}
	if want := []string{"acme-keep-1.0.0"}; !equalStrings(publishedPackages(t, s), want) {
		t.Errorf("the agent's package list is %v, want it untouched", publishedPackages(t, s))
	}
}

func TestRemoveWithoutAVersionTakesWhateverIsThere(t *testing.T) {
	// The guard is opt-in, and that is deliberate: an operator revoking a
	// capability says "take this package off", not "take this version
	// off", because they do not care which one is running — they want it
	// gone. A store that demanded a version for that would be a store
	// that made the common case harder than the rare one.
	s, _, _, _ := seed(t)

	got := s.Remove(t.Context(), "acme-keep", "")
	if got.Refused != "" {
		t.Errorf("a version-less remove refused: %+v", got)
	}
	if got.Note != "removed" {
		t.Errorf("note = %q, want %q", got.Note, "removed")
	}
	if names := namesOf(s.Installed()); len(names) != 0 {
		t.Errorf("installed = %v, want empty", names)
	}
}

// ---------------------------------------------------------------------
// republish
// ---------------------------------------------------------------------

func TestAPackageThatStopsPassingReviewIsDroppedFromTheAgentList(t *testing.T) {
	// The review is a function of the tree, and a tree can change
	// underneath a node: an operator editing a file in the package
	// directory, a restore from backup, a disk that came back with
	// different contents. A node that republishes from the list it
	// remembers rather than the directory it finds is the one that never
	// notices.
	s, _, _, _ := seed(t)

	// A second install, so the set has something to lose.
	body, spec, store := signedPackageTar(t, "acme-other", "1.0.0", nil)
	s.trust = store
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("install: %+v", got)
	}

	// Something edits the installed package after the fact.
	victim := filepath.Join(s.pkgDir, "acme-other-1.0.0", pluginmanifest.ManifestFile)
	writeFile(t, victim, "not a manifest at all\n")

	err := s.republish()
	if err == nil {
		t.Fatal("a package that no longer loads was republished to the agent")
	}
	// The failure has to name the package. A republish that fails without
	// saying which directory is at fault leaves an operator with a
	// truncated package list and nothing to act on.
	if !strings.Contains(err.Error(), "acme-other") {
		t.Errorf("error = %q, want it to name the package that stopped loading", err)
	}
	// The good package is untouched on disk, and the tampered one is not
	// in what the agent is told — it is still the seed's list, because the
	// republish that would have changed it refused.
	assertUntouched(t, s)
}

func TestAPackageThatStillLoadsButNoLongerReviewsIsDroppedFromTheAgentList(t *testing.T) {
	// The other half of the republish property, and the half that is easy
	// to get wrong. A package whose manifest has been replaced does not
	// load, which is the loud failure; a package whose *signature* has been
	// replaced still loads perfectly — the manifest is untouched, the
	// agent can read every tool name out of it — and no longer passes
	// review, because the signature no longer describes the tree.
	//
	// That is the case a republish which trusts what is on disk would
	// wave through, and it is the case that matters: the node would be
	// running code whose contents nobody authenticated, with a manifest
	// that reads exactly as it did when it was.
	body, spec, _ := signedPackageTar(t, "acme-other", "1.0.0", nil)

	s, _, _, _ := seed(t)
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("install: %+v", got)
	}

	// Something replaces the signature. The manifest is left alone, so
	// the package still loads.
	writeFile(t, filepath.Join(s.pkgDir, "acme-other-1.0.0", pluginmanifest.SignatureFile),
		`{"key_id":"acme-2026","algorithm":"ed25519","name":"acme-other","version":"1.0.0","tree_digest":"`+
			strings.Repeat("f", 64)+`","signature":"bm90LWEtc2lnbmF0dXJl"}`)

	err := s.republish()
	if err == nil {
		t.Fatal("a package whose signature no longer describes its tree was republished to the agent")
	}
	if !strings.Contains(err.Error(), "acme-other") {
		t.Errorf("error = %q, want it to name the package that stopped passing", err)
	}
	// And it is gone from what the agent was told, not merely reported.
	assertUntouched(t, s)
}

func TestAPackageWithADashInItsNameRoundTripsThroughTheAgentList(t *testing.T) {
	// publishedPackages compares base names against "<name>-<version>",
	// so this also checks that a dashed name is not mangled on the way
	// into the settings file.
	body, spec, store := signedPackageTar(t, "opskeeper-sre-repair", "0.1.0", nil)
	s := newStore(t, body, store, readOnlyNode())
	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("install: %+v", got)
	}
	if want := []string{"opskeeper-sre-repair-0.1.0"}; !equalStrings(publishedPackages(t, s), want) {
		t.Errorf("the agent's package list is %v, want %v", publishedPackages(t, s), want)
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// copyShippedPackage copies one of the real packages out of
// plugins/pig-ops so a test can use a bundle the review path has already
// been written against.
//
// The copy is signed with the test's key. The shipped packages are
// deliberately unsigned — OpsKeeper does not ship a release private key,
// because shipping one would make every node trust whoever holds the
// repository — so on a node with a trust store they are refused, and a
// test that copied one verbatim would be asserting that the boot bundle
// breaks the node. That is a real property, but it is not the one these
// tests are about.
func copyShippedPackage(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	repo := filepath.Join(filepath.Dir(file), "..", "..")
	src := filepath.Join(repo, "plugins", "pig-ops", name)
	dst := filepath.Join(t.TempDir(), name)
	if err := copyTreeDir(src, dst); err != nil {
		t.Fatalf("copy %s: %v", name, err)
	}
	env, err := testSigner(t).Sign(dst)
	if err != nil {
		t.Fatalf("sign the copied bundle: %v", err)
	}
	if _, err := env.WriteTo(dst); err != nil {
		t.Fatalf("write the signature: %v", err)
	}
	return dst
}

// copyTreeDir copies a directory tree.
func copyTreeDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if !d.Type().IsRegular() {
			// Symlinks are exactly what TreeDigest refuses, so a copy
			// that followed them would fail the review for a reason that
			// has nothing to do with the property under test.
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o640)
	})
}

// signedStore is a trust store holding the test's key.
func signedStore(t *testing.T) *pluginmanifest.TrustStore {
	t.Helper()
	signer := testSigner(t)
	store := pluginmanifest.NewTrustStore()
	if err := store.Trust(signer.KeyID(), signer.PublicKey()); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	return store
}

// seedStore builds a store whose package directory already holds the good
// package, so a refusal can be shown not to have disturbed it.
func seedStore(t *testing.T, body []byte, spec ports.PluginSpec, store *pluginmanifest.TrustStore) *pluginStore {
	t.Helper()
	s, _, _, seedTrust := seed(t)
	_ = store
	s.trust = seedTrust
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	return s
}

// ---------------------------------------------------------------------
// the edge version
// ---------------------------------------------------------------------

// withMinEdgeVersion rewrites a staged manifest's min_edge_version. The
// package is signed after this runs, so the change is what the node
// reviews rather than something the signature catches.
func withMinEdgeVersion(version string) func(root string) {
	return func(root string) {
		path := filepath.Join(root, pluginmanifest.ManifestFile)
		body, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		out := strings.Replace(string(body),
			"install: {strategy: rolling, min_edge_version: 0.1.0}",
			"install: {strategy: rolling, min_edge_version: "+version+"}", 1)
		if err := os.WriteFile(path, []byte(out), 0o640); err != nil {
			panic(err)
		}
	}
}

// withPigVersion makes a store behave like a node whose agent version is
// known to the operator.
//
// The test-only default in TestMain covers the ordinary case; this is for
// a test that wants a specific version — typically an agent too old for
// the package under test.
func withPigVersion(t *testing.T, v string) {
	t.Helper()
	t.Setenv(pigVersionEnv, v)
}

// withMinPigVersion rewrites a fixture manifest's install block to declare
// a min_pig_version. It runs before signing, so the envelope covers the
// edited manifest.
func withMinPigVersion(version string) func(root string) {
	return func(root string) {
		path := filepath.Join(root, pluginmanifest.ManifestFile)
		body, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		out := strings.Replace(string(body),
			"install: {strategy: rolling, min_edge_version: 0.1.0}",
			"install: {strategy: rolling, min_edge_version: 0.1.0, min_pig_version: "+version+"}", 1)
		if err := os.WriteFile(path, []byte(out), 0o640); err != nil {
			panic(err)
		}
	}
}

func TestTheStoreRefusesAPackageNeedingANewerPiGThanItLaunches(t *testing.T) {
	// The second axis. A node can run an edge build new enough and still
	// be launching a pig too old to load the package's extensions — the
	// two are upgraded on different cadences, and a check that reused the
	// edge's version for both would pass this package.
	s, _, _, _ := seed(t)
	withPigVersion(t, "0.3.0")

	body, spec, store := signedPackageTar(t, "acme-needs-new-pig", "1.0.0", withMinPigVersion("0.4.0"))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	got := s.Install(t.Context(), spec)
	if got.Refused == "" {
		t.Fatalf("install = %+v, want a refusal — this node's agent is older than the package needs", got)
	}
	if !strings.Contains(got.Refused, "0.4.0") {
		t.Errorf("refusal %q does not name the PiG version the package needs", got.Refused)
	}
	// The refusal has to name the agent, not the edge, or the operator
	// upgrades the wrong binary.
	if !strings.Contains(got.Refused, "PiG") {
		t.Errorf("refusal %q does not name the component at fault", got.Refused)
	}
	if want := []string{"acme-keep@1.0.0"}; !equalStrings(namesOf(s.Installed()), want) {
		t.Errorf("installed = %v, want the node's own package untouched", namesOf(s.Installed()))
	}
}

func TestAnUnconfiguredNodeStillStatesItsAgentVersion(t *testing.T) {
	// The agent is not a separately-upgraded binary here: `pig` is built
	// from the same source as the edge and shipped inside it, so the
	// linked PiG release line *is* what the node launches. Leaving that
	// unread would make min_pig_version a field every node refuses, which
	// is a check that fires on correct configurations — the failure mode
	// that gets a safety control deleted.
	//
	// The edge's own version has no such fallback, deliberately: a binary
	// cannot report a tag it was not given, and "dev" is not a version.
	// That asymmetry is asserted in TestAnEdgeWithNoVersionStillRefuses.
	t.Setenv(pigVersionEnv, "")

	if got := pigSelfVersion(); got != pigrpc.PigVersion {
		t.Fatalf("an unconfigured node reports its agent as %q, want the linked %q",
			got, pigrpc.PigVersion)
	}
}

func TestAPackageWithinTheLinkedAgentsReachInstallsWithNoConfiguration(t *testing.T) {
	// The end-to-end half of the fallback: nothing set, and a package
	// asking for exactly the linked version installs.
	s, _, _, _ := seed(t)
	t.Setenv(pigVersionEnv, "")

	body, spec, store := signedPackageTar(t, "acme-pig-default", "1.0.0", withMinPigVersion(pigrpc.PigVersion))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("a fresh node refused a package matching the agent it ships: %+v", got)
	}
}

func TestAMinimumAboveTheLinkedAgentIsRefusedWithNothingConfigured(t *testing.T) {
	// And the check still bites when it should, on a node nobody has
	// configured. The refusal names both numbers so the operator can see
	// it is a version gap rather than a missing setting.
	s, _, _, _ := seed(t)
	t.Setenv(pigVersionEnv, "")

	body, spec, store := signedPackageTar(t, "acme-pig-too-new", "1.0.0", withMinPigVersion("99.0.0"))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatalf("install = %+v, want a refusal — the package needs an agent this node does not ship", got)
	}
	for _, want := range []string{"99.0.0", pigrpc.PigVersion} {
		if !strings.Contains(got.Refused, want) {
			t.Errorf("refusal %q does not name %q", got.Refused, want)
		}
	}
}

func TestAnEdgeWithNoVersionStillRefuses(t *testing.T) {
	// The contrast that makes the fallback above a decision rather than an
	// inconsistency. The edge's build version cannot be read from the
	// binary — "dev" is not a version — so a node that was not told one
	// refuses a package declaring min_edge_version, even though it can
	// state its agent's version perfectly well.
	s, _, _, _ := seed(t)
	t.Setenv(edgeVersionEnv, "")

	body, spec, store := signedPackageTar(t, "acme-edge-unknown", "1.0.0", withMinEdgeVersion("0.1.0"))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	got := s.Install(t.Context(), spec)
	if got.Installed {
		t.Fatalf("install = %+v, want a refusal — the node cannot say what edge it runs", got)
	}
	if !strings.Contains(got.Refused, "unknown") {
		t.Errorf("refusal %q does not say the node's own version is the problem", got.Refused)
	}
}

func TestANodeConfiguredWithTheAgentsOwnVersionStringStillInstalls(t *testing.T) {
	// `pig --version` prints "0.3.0+0.87.1", and pasting that into the
	// environment is the first thing anybody will do. If it did not parse
	// the node would refuse packages it can host, and the refusal would
	// blame a string that plainly is a version — the worst kind of
	// fail-closed, where the operator is told to fix something that is
	// already right.
	s, _, _, _ := seed(t)
	withPigVersion(t, "0.3.0+0.87.1")

	body, spec, store := signedPackageTar(t, "acme-pig-composite", "1.0.0", withMinPigVersion("0.3.0"))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("a node configured with pig's own version string refused a package it can host: %+v", got)
	}
}

func TestAPackageWithinTheAgentsReachInstalls(t *testing.T) {
	// The positive half. Without it every test above passes vacuously: a
	// store that never read the agent version at all would refuse every
	// min_pig_version package and look exactly like a working check.
	// This is the one that fails when the version stops being read.
	s, _, _, _ := seed(t)
	withPigVersion(t, "0.4.0")

	body, spec, store := signedPackageTar(t, "acme-pig-ok", "1.0.0", withMinPigVersion("0.4.0"))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	got := s.Install(t.Context(), spec)
	if !got.Installed {
		t.Fatalf("a package exactly at the node's agent version was refused: %+v", got)
	}
	want := []string{"acme-keep@1.0.0", "acme-pig-ok@1.0.0"}
	if !equalStrings(namesOf(s.Installed()), want) {
		t.Errorf("installed = %v, want %v", namesOf(s.Installed()), want)
	}
}

func TestAPiGMinimumIsNotCheckedAgainstTheEdgeVersion(t *testing.T) {
	// The wrong-way-round test. Everything here is new enough except the
	// agent, and the edge is deliberately far ahead — so a store that fed
	// NodeVersion into both axes would admit this package.
	s, _, _, _ := seed(t)
	t.Setenv(edgeVersionEnv, "9.9.9")
	withPigVersion(t, "0.2.0")

	body, spec, store := signedPackageTar(t, "acme-pig-split", "1.0.0", withMinPigVersion("0.4.0"))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	if got := s.Install(t.Context(), spec); got.Installed {
		t.Fatalf("install = %+v, want a refusal; the edge version is not the agent version", got)
	}
}

func TestTheStoreRefusesAPackageNeedingANewerEdgeThanItRuns(t *testing.T) {
	// min_edge_version is in every shipped manifest and was, until this
	// check existed, read by nothing. This is the compatibility matrix
	// reduced to the one edge a node can evaluate about itself.
	s, _, _, _ := seed(t)
	t.Setenv("OPSKEEPER_EDGE_VERSION", "0.5.0")

	body, spec, store := signedPackageTar(t, "acme-needs-new-edge", "1.0.0", withMinEdgeVersion("0.9.0"))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	got := s.Install(t.Context(), spec)
	if got.Refused == "" {
		t.Fatalf("install = %+v, want a refusal — this node is older than the package needs", got)
	}
	if !strings.Contains(got.Refused, "0.9.0") {
		t.Errorf("refusal %q does not name the version the package needs", got.Refused)
	}
	// The node's own package is untouched, so a refused install is not a
	// half-completed one.
	if want := []string{"acme-keep@1.0.0"}; !equalStrings(namesOf(s.Installed()), want) {
		t.Errorf("installed = %v, want the node's own package untouched", namesOf(s.Installed()))
	}
}

func TestAPackageWithNoMinimumInstallsOnANodeOfAnyVersion(t *testing.T) {
	// The check must not become a blanket refusal. A package that declared
	// no minimum is exactly what every package written before the field
	// did, and they still install.
	s, _, _, _ := seed(t)
	t.Setenv("OPSKEEPER_EDGE_VERSION", "0.5.0")

	body, spec, store := signedPackageTar(t, "acme-no-minimum", "1.0.0", withMinEdgeVersion(`""`))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	if got := s.Install(t.Context(), spec); !got.Installed {
		t.Fatalf("a package with no minimum was refused: %+v", got)
	}
}

func TestTheStoreRefusesWhenItCannotStateItsOwnVersion(t *testing.T) {
	// A node that cannot say what it runs is guessing, and the guess that
	// is wrong permissively installs a package the node may not host. It
	// is a refusal rather than an admission.
	s, _, _, _ := seed(t)
	// Clearing the override leaves the binary's own version, which under
	// `go test` is the untagged default — not a version this node can
	// compare, which is the state under test.
	t.Setenv("OPSKEEPER_EDGE_VERSION", "")

	body, spec, store := signedPackageTar(t, "acme-needs-a-version", "1.0.0", withMinEdgeVersion("0.9.0"))
	s.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	s.trust = store

	got := s.Install(t.Context(), spec)
	if got.Refused == "" {
		t.Fatalf("install = %+v, want a refusal when the node cannot state its version", got)
	}
}

func TestTheVersionOverrideWinsOverTheBuildVersion(t *testing.T) {
	// The override exists because an operator's deployment can know the
	// version better than the binary does — a node rolled back to an older
	// binary in place, for instance. If the build tag silently won, the
	// setting would be a lie.
	t.Setenv("OPSKEEPER_EDGE_VERSION", "1.2.3")
	if got := bootSelfVersion("0.1.0"); got != "1.2.3" {
		t.Errorf("bootSelfVersion with an override = %q, want the override", got)
	}
	if got := bootSelfVersion("dev"); got != "1.2.3" {
		t.Errorf("bootSelfVersion(dev) with an override = %q, want the override", got)
	}
	t.Setenv("OPSKEEPER_EDGE_VERSION", "")
	if got := bootSelfVersion("0.1.0"); got != "0.1.0" {
		t.Errorf("bootSelfVersion with no override = %q, want the build version", got)
	}
	if got := bootSelfVersion("dev"); got != unsupportedEdgeVersion {
		t.Errorf("bootSelfVersion(dev) = %q, want %q — dev is not a version",
			got, unsupportedEdgeVersion)
	}
	if got := bootSelfVersion(""); got != unsupportedEdgeVersion {
		t.Errorf("bootSelfVersion(\"\") = %q, want %q", got, unsupportedEdgeVersion)
	}
}

func TestTheRuntimeReviewUsesTheNodesOwnVersion(t *testing.T) {
	// The store has to reach the same conclusion the boot path does, or a
	// package the node cannot host would be installable at runtime and
	// refused at boot.
	s, _, _, _ := seed(t)
	t.Setenv("OPSKEEPER_EDGE_VERSION", "0.7.0")
	pol, err := s.reviewPolicy()
	if err != nil {
		t.Fatalf("reviewPolicy: %v", err)
	}
	if pol.NodeVersion != "0.7.0" {
		t.Errorf("reviewPolicy NodeVersion = %q, want the configured override", pol.NodeVersion)
	}
}

// The invariant the derived fixture exists to protect, asserted directly.
//
// Reading the pin is not enough on its own: a shipped package may declare a
// floor above the pin, and then the fixture is still below it and still
// refuses its own boot packages for a reason the admission tests are not
// about. This test fails at the point where the two numbers disagree rather
// than three tests later, in a message that names both of them.
func TestTheFixtureNodeIsNotBelowAnyShippedPackageFloor(t *testing.T) {
	base := filepath.Join("..", "..", "plugins", "pig-ops")
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read %s: %v", base, err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := pluginmanifest.Load(filepath.Join(base, e.Name()))
		if err != nil {
			t.Fatalf("load %s: %v", e.Name(), err)
		}
		seen++
		ok, _, why := pluginmanifest.CheckVersions(
			p.Manifest.Spec.Install.MinEdgeVersion,
			p.Manifest.Spec.Install.MinPigVersion,
			testEdgeVersion, testPigVersion)
		if !ok {
			t.Errorf("package %s would be refused by a node running the PiG this tree "+
				"pins (edge %s / pig %s): %s\n"+
				"The fixture reports what go.mod requires, so this is a real disagreement "+
				"between the floor a package declares and the agent this build launches — "+
				"not a stale fixture.",
				e.Name(), testEdgeVersion, testPigVersion, why)
		}
	}
	if seen == 0 {
		t.Fatal("no shipped package was checked, so this test proved nothing")
	}
}

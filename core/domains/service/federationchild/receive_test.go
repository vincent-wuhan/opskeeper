package federationchild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Receiving a policy tree, and refusing to.
//
// Every property under test is about ordering: what is checked before the
// bytes are unpacked, what is never recorded, and what a retry is allowed to
// change. None of them needs a socket, for the same reason the rest of this
// package does not — what the child decides does not need a root to be true.

// TestASourcedPushFetchesVerifiesAndPromotes is the whole path in one go:
// the tree is not on this machine, and the push ends with it live.
func TestASourcedPushFetchesVerifiesAndPromotes(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")

	archive := serveArchive(t, store, signer, "opskeeper-sre-readonly", "1.0.0")
	source := archive.source(t)
	client.fetchFor(source.URL, archive.read(t))

	req := bundle(id, 1, envelopeOf(t, archive.tree))
	req.Source = &source

	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)
	if !resp.Outcome.Accepted {
		t.Fatalf("outcome = %+v, want accepted (error %q)", resp.Outcome, resp.Error)
	}
	if resp.Retryable {
		t.Errorf("Retryable = true on an accepted push")
	}
	// The tree was fetched rather than copied in, so it landed under this
	// store's own versions directory, named for the version — and that is
	// what Switch is now pointing at.
	want := mustResolve(t, filepath.Join(store.VersionsDir(), "v1"))
	if store.Live() != want {
		t.Errorf("store live = %q, want %q", store.Live(), want)
	}
}

// TestTheDigestIsCheckedBeforeAnythingIsUnpacked is what makes the download
// safe to do at all.
//
// A corrupt or substituted archive is refused without a single byte landing
// on disk. The order is the whole claim: check the digest, *then* unpack. An
// implementation that unpacked first and verified afterwards would satisfy
// every other test in this file and still let anyone who can answer the URL
// fill this cluster's disk.
func TestTheDigestIsCheckedBeforeAnythingIsUnpacked(t *testing.T) {
	_, _, store, _ := newChild(t, "prod-cn-south")
	// A real, well-formed archive — it would unpack cleanly — served
	// against a digest computed over something else.
	body := tarGz(t,
		treeEntry{path: "policy/", dir: true},
		treeEntry{path: "policy/pig-ops.yaml", body: "apiVersion: opskeeper.io/v1"},
		treeEntry{path: "policy/extensions/tool/t.go", body: "package tool"},
	)
	store.fetch = func(context.Context, string) ([]byte, error) { return body, nil }

	_, err := store.Receive(t.Context(), &tunnel.PolicySource{
		URL:           "https://artifacts.invalid/p.tar.gz",
		ArchiveSHA256: digestOfBytes([]byte("some other archive")),
	}, 4)
	if err == nil {
		t.Fatalf("an archive that did not match the offered digest was accepted")
	}
	if !strings.Contains(err.Error(), "does not match the digest") {
		t.Errorf("error = %v, want it to name the digest", err)
	}
	// The whole claim: this archive is perfectly unpackable, and not one
	// byte of it reached the disk. An implementation that unpacked first
	// would fail exactly here.
	assertNothingStaged(t, store)
}

// TestACorruptTransferIsRetryableAndRecordsNothing: an interrupted download
// clears by itself, and a child that recorded it as a decision would refuse
// that version forever.
func TestACorruptTransferIsRetryableAndRecordsNothing(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")

	archive := serveArchive(t, store, signer, "opskeeper-sre-readonly", "1.0.0")
	source := archive.source(t)
	// The digest the root promised is over the archive as served. This is
	// what arrived instead: a transfer that lost a byte in flight.
	corrupt := append([]byte{}, archive.read(t)...)
	corrupt[len(corrupt)/2] ^= 0xff
	client.fetchFor(source.URL, corrupt)

	req := bundle(id, 7, envelopeOf(t, archive.tree))
	req.Source = &source

	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)
	if !resp.Retryable {
		t.Errorf("Retryable = false for a corrupted transfer: %+v", resp.Outcome)
	}
	if resp.Outcome.Accepted {
		t.Errorf("a corrupted archive was accepted: %+v", resp.Outcome)
	}
	if _, remembered := client.receiverOf(t).Seen(7); remembered {
		t.Errorf("version 7 was recorded, so the real push of it can never be applied")
	}
	assertNothingStaged(t, store)
}

// TestAMalformedSourceIsRefusedWithoutARequestLeaving is the cheap half: a
// digest that cannot be a digest, and a scheme that is not one this child
// fetches from. Neither costs a round trip, and neither is a statement about
// the policy.
func TestAMalformedSourceIsRefusedWithoutARequestLeaving(t *testing.T) {
	_, _, store, _ := newChild(t, "prod-cn-south")

	cases := map[string]struct {
		src    tunnel.PolicySource
		wantIn string
	}{
		"no scheme": {
			tunnel.PolicySource{URL: "artifacts.invalid/p.tar.gz", ArchiveSHA256: strings.Repeat("a", 64)},
			"scheme",
		},
		"an exotic scheme": {
			tunnel.PolicySource{URL: "ftp://artifacts.invalid/p.tar.gz", ArchiveSHA256: strings.Repeat("a", 64)},
			"scheme",
		},
		"a short digest": {
			tunnel.PolicySource{URL: "https://artifacts.invalid/p.tar.gz", ArchiveSHA256: "abc"},
			"64 hex characters",
		},
		"an uppercase digest": {
			tunnel.PolicySource{URL: "https://artifacts.invalid/p.tar.gz", ArchiveSHA256: strings.ToUpper(strings.Repeat("a", 64))},
			"lower hex",
		},
		"a file URL naming another host": {
			tunnel.PolicySource{URL: "file://elsewhere/p.tar.gz", ArchiveSHA256: strings.Repeat("a", 64)},
			"not this machine",
		},
	}
	for name, tc := range cases {
		fetched := false
		store.fetch = func(context.Context, string) ([]byte, error) {
			fetched = true
			return nil, nil
		}
		_, err := store.Receive(t.Context(), &tc.src, 1)
		if err == nil {
			t.Errorf("[%s] a malformed source was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("[%s] error = %v, want it to mention %q", name, err, tc.wantIn)
		}
		if fetched {
			t.Errorf("[%s] a request was made for a source that could never be one", name)
		}
	}
}

// TestAnArchiveThatEscapesItsStagingAreaIsRefused covers the archive-level
// traversal guard.
//
// The signature would catch a hostile tree anyway, but only after it had been
// written, and a child that writes outside its staging area on the way to
// finding out has already lost the property the rest of this file is about.
func TestAnArchiveThatEscapesItsStagingAreaIsRefused(t *testing.T) {
	_, _, store, _ := newChild(t, "prod-cn-south")
	outside := filepath.Join(t.TempDir(), "escaped.yaml")

	for name, entry := range map[string]treeEntry{
		"a parent segment": {path: "../escaped.yaml", body: "kind: Plugin"},
		"a nested parent":  {path: "policy/../../escaped.yaml", body: "kind: Plugin"},
		"an absolute path": {path: "/etc/escaped.yaml", body: "kind: Plugin"},
		"a bare traversal": {path: "..", dir: true},
		"a symlink entry":  {path: "policy/link", typeflag: tar.TypeSymlink, body: "/etc/passwd"},
		"a device node":    {path: "policy/dev", typeflag: tar.TypeChar},
	} {
		body := tarGz(t, entry)
		store.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
		_, err := store.Receive(t.Context(), &tunnel.PolicySource{
			URL:           "https://artifacts.invalid/p.tar.gz",
			ArchiveSHA256: digestOfBytes(body),
		}, 2)
		if err == nil {
			t.Errorf("[%s] an archive entry %q was accepted", name, entry.path)
		}
		if _, statErr := os.Stat(outside); statErr == nil {
			t.Fatalf("[%s] the archive wrote outside the staging area", name)
		}
		assertNothingStaged(t, store)
	}
}

// TestAnArchiveWithTwoRootsIsRefused: a tree with two top-level directories
// is a container of something, and picking one is how a tree gets verified in
// one directory and promoted from another.
func TestAnArchiveWithTwoRootsIsRefused(t *testing.T) {
	_, _, store, _ := newChild(t, "prod-cn-south")
	body := tarGz(t,
		treeEntry{path: "policy-a/", dir: true},
		treeEntry{path: "policy-a/pig-ops.yaml", body: "kind: Plugin"},
		treeEntry{path: "policy-b/", dir: true},
		treeEntry{path: "policy-b/pig-ops.yaml", body: "kind: Plugin"},
	)
	store.fetch = func(context.Context, string) ([]byte, error) { return body, nil }
	_, err := store.Receive(t.Context(), &tunnel.PolicySource{
		URL:           "https://artifacts.invalid/p.tar.gz",
		ArchiveSHA256: digestOfBytes(body),
	}, 3)
	if err == nil {
		t.Fatalf("an archive with two top-level directories was accepted")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error = %v, want it to say the archive is not one tree", err)
	}
	assertNothingStaged(t, store)
}

// TestAPushNamingBothAPathAndASourceIsRefused: a tree has one origin, and a
// request whose meaning depends on which field a reader checks first is not a
// request.
func TestAPushNamingBothAPathAndASourceIsRefused(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")
	root, env := signedTree(t, store, signer, "opskeeper-sre-readonly", "1.0.0")

	req := bundle(id, 1, env)
	req.StagedPath = root
	req.Source = &tunnel.PolicySource{URL: "https://artifacts.invalid/p.tar.gz", ArchiveSHA256: strings.Repeat("a", 64)}

	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)
	if resp.Outcome.Accepted {
		t.Errorf("a push naming two origins was applied: %+v", resp.Outcome)
	}
	if !resp.Retryable {
		t.Errorf("Retryable = false; a root sending this has a bug, and the answer is not a policy refusal")
	}
	// Nothing was recorded, so a corrected push of the same version works.
	if _, remembered := client.receiverOf(t).Seen(1); remembered {
		t.Errorf("version 1 was recorded for a malformed push and can never be applied")
	}
}

// TestAPushNamingNeitherPlaceIsRetryableNotStaged is the existing contract,
// stated where the new one meets it: a root whose tree has not arrived yet is
// told to come back, and burns nothing.
func TestAPushNamingNeitherPlaceIsRetryableNotStaged(t *testing.T) {
	_, client, _, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")
	env := pluginmanifest.Envelope{
		KeyID:      signer.KeyID(),
		Name:       "opskeeper-sre-readonly",
		Version:    "1.0.0",
		Algorithm:  pluginmanifest.SignAlgorithm,
		TreeDigest: strings.Repeat("b", 64),
	}

	req := bundle(id, 1, env)
	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)
	if !resp.Retryable {
		t.Errorf("Retryable = false for a push whose tree has not arrived: %+v", resp.Outcome)
	}
	if !strings.Contains(resp.Error, "not staged") {
		t.Errorf("error = %q, want it to say the tree is not staged", resp.Error)
	}
	if _, remembered := client.receiverOf(t).Seen(1); remembered {
		t.Errorf("an undelivered version was recorded and can never be applied")
	}
}

// TestASourcedTreeStillHasToBeSigned is the reason the URL is not the
// authority. A perfectly well-formed archive nobody signed is refused, and
// refused as a forgery rather than as a delivery problem.
func TestASourcedTreeStillHasToBeSigned(t *testing.T) {
	_, client, store, _ := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")

	// A real tree, built and archived, but signed by a key this cluster
	// does not trust.
	rogue := newSigner(t)
	dir := filepath.Join(t.TempDir(), "rogue")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, filepath.Join(dir, pluginmanifest.ManifestFile), manifestYAML("opskeeper-sre-readonly", "1.0.0"))
	write(t, filepath.Join(dir, "extensions", "tool", "tools.go"), "package tool")
	rogueEnv, err := rogue.Sign(dir)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := rogueEnv.WriteTo(dir); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	body := tarGzDir(t, filepath.Base(dir), dir)
	source := tunnel.PolicySource{URL: "https://artifacts.invalid/rogue.tar.gz", ArchiveSHA256: digestOfBytes(body)}
	client.fetchFor(source.URL, body)

	// The bundle the child would have been handed had the rogue been this
	// cluster's root.
	req := bundle(id, 1, rogueEnv)
	req.Source = &source

	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)
	if resp.Outcome.Accepted {
		t.Fatalf("a tree signed by an untrusted key was promoted: %+v", resp.Outcome)
	}
	if resp.Retryable {
		t.Errorf("Retryable = true for a signature failure: %+v", resp.Outcome)
	}
	// The archive was fetched — that is what happened — but nothing was
	// promoted, and the live link still points nowhere.
	if store.Live() != "" {
		t.Errorf("store live = %q after a signature failure, want nothing enforced", store.Live())
	}
}

// TestARepushOfAVersionAlreadyStagedDoesNotDisturbIt: a retry is the common
// case on this channel, and a retry must not unpack over the tree the cluster
// may be enforcing right now.
func TestARepushOfAVersionAlreadyStagedDoesNotDisturbIt(t *testing.T) {
	_, _, store, signer := newChild(t, "prod-cn-south")
	archive := serveArchive(t, store, signer, "opskeeper-sre-readonly", "1.0.0")
	source := archive.source(t)
	body := archive.read(t)
	store.fetch = func(context.Context, string) ([]byte, error) { return body, nil }

	first, err := store.Receive(t.Context(), &source, 5)
	if err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	// A marker inside the tree that only the first unpack wrote.
	write(t, filepath.Join(first, "marker"), "first")

	second, err := store.Receive(t.Context(), &source, 5)
	if err != nil {
		t.Fatalf("second Receive: %v", err)
	}
	if second != first {
		t.Errorf("second Receive = %q, want the same path %q", second, first)
	}
	if _, err := os.Stat(filepath.Join(second, "marker")); err != nil {
		t.Errorf("a re-push unpacked over a tree that was already staged: %v", err)
	}
}

// TestAFetchFailureIsRetryableAndLeavesNoDebris: a child that gives up
// halfway must not leave a directory the next push would mistake for a staged
// tree.
func TestAFetchFailureIsRetryableAndLeavesNoDebris(t *testing.T) {
	_, client, store, signer := newChild(t, "prod-cn-south")
	id := federation.ClusterID("prod-cn-south")
	archive := serveArchive(t, store, signer, "opskeeper-sre-readonly", "1.0.0")
	source := archive.source(t)
	// Nothing registered at this URL and no error configured, so the fake
	// answers os.ErrNotExist — a 404, which is the common real shape of "the
	// root has not finished putting the tree where it said".
	client.fetchErr = os.ErrNotExist

	req := bundle(id, 9, envelopeOf(t, archive.tree))
	req.Source = &source

	resp := client.invoke(t, tunnel.MethodClusterPolicy, req)
	if !resp.Retryable {
		t.Errorf("Retryable = false for a source that could not be fetched: %+v", resp.Outcome)
	}
	if _, remembered := client.receiverOf(t).Seen(9); remembered {
		t.Errorf("a failed fetch recorded a decision about version 9")
	}
	assertNothingStaged(t, store)
	if store.Live() != "" {
		t.Errorf("store live = %q after a failed fetch, want nothing enforced", store.Live())
	}
}

// TestTheArchiveCapIsEnforcedOnWhatArrives, not only on what is read: a
// fetcher returning more than the cap is refused rather than truncated into
// something that might still hash correctly by accident.
func TestTheArchiveCapIsEnforcedOnWhatArrives(t *testing.T) {
	_, _, store, _ := newChild(t, "prod-cn-south")
	store.fetch = func(context.Context, string) ([]byte, error) {
		return make([]byte, maxPolicyArchiveBytes+1), nil
	}
	_, err := store.Receive(t.Context(), &tunnel.PolicySource{
		URL:           "https://artifacts.invalid/p.tar.gz",
		ArchiveSHA256: strings.Repeat("a", 64),
	}, 1)
	if err == nil {
		t.Fatalf("an oversized archive was accepted")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error = %v, want it to name the cap", err)
	}
	assertNothingStaged(t, store)
}

// TestAFileSourceIsReadTheSameWay is why file is on the allowlist: a transfer
// between two clusters sharing a mount goes through exactly the same cap and
// digest as an https one.
func TestAFileSourceIsReadTheSameWay(t *testing.T) {
	_, _, store, _ := newChild(t, "prod-cn-small")
	archive := serveArchive(t, store, newSigner(t), "opskeeper-sre-readonly", "1.0.0")
	// The real transport, not the fake: what is under test is that a file
	// URL is resolved by the child at all, and a fake that answered every
	// URL with bytes from a map would pass whether or not fileFetch worked.
	store.fetch = defaultFetch

	got, err := store.Receive(t.Context(), &tunnel.PolicySource{
		URL:           "file://" + archive.archive,
		ArchiveSHA256: digestOfBytes(archive.read(t)),
	}, 1)
	if err != nil {
		t.Fatalf("Receive from a file URL: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got, pluginmanifest.ManifestFile)); err != nil {
		t.Errorf("the file-sourced tree is not a tree: %v", err)
	}

	// And a file whose bytes do not match is refused the same way.
	other := serveArchive(t, store, newSigner(t), "opskeeper-sre-readonly", "2.0.0")
	if _, err := store.Receive(t.Context(), &tunnel.PolicySource{
		URL:           "file://" + other.archive,
		ArchiveSHA256: digestOfBytes([]byte("not this one")),
	}, 2); err == nil {
		t.Errorf("a file source with a mismatched digest was accepted")
	}
}

// ---------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------

// served is a signed tree and the archive a root would serve it as.
//
// The two are returned together because a test needs both and re-deriving one
// from the other is how a test ends up reading a file that happens to have
// the right name.
type served struct {
	tree    string
	archive string
}

// serveArchive builds a signed tree and packs it as a tar.gz.
//
// The signer is a parameter because one test needs a tree this cluster does
// not trust, and handing the helper a different signer is clearer than
// duplicating the tree construction to change one key.
func serveArchive(t *testing.T, store *Store, signer *pluginmanifest.Signer, name, version string) served {
	t.Helper()
	tree := filepath.Join(store.VersionsDir(), "served-"+version)
	signedTreeAt(t, tree, signer, name, version)
	archive := filepath.Join(t.TempDir(), "policy-"+version+".tar.gz")
	writeTarGz(t, archive, filepath.Base(tree), tree)
	return served{tree: tree, archive: archive}
}

// readArchive reads what a root would have served.
func (s served) read(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(s.archive)
	if err != nil {
		t.Fatalf("read %s: %v", s.archive, err)
	}
	return body
}

// url is where a root would serve it, and digest is what it would promise.
func (s served) source(t *testing.T) tunnel.PolicySource {
	t.Helper()
	return tunnel.PolicySource{URL: "https://artifacts.invalid/" + filepath.Base(s.archive), ArchiveSHA256: digestOfBytes(s.read(t))}
}

// envelopeOf reads the sidecar out of a tree, which is the envelope the root
// would have published for it.
func envelopeOf(t *testing.T, tree string) pluginmanifest.Envelope {
	t.Helper()
	env, err := pluginmanifest.ReadEnvelope(tree)
	if err != nil {
		t.Fatalf("ReadEnvelope(%s): %v", tree, err)
	}
	return env
}

func digestOfBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// treeEntry is one file in a hand-built archive, so a test can say what the
// archive holds rather than how to hold it.
type treeEntry struct {
	path string
	// dir marks a directory entry. A tar that carries a directory is not
	// the same tar as one that does not, and one of the guards under test
	// is about exactly which entries exist.
	dir bool
	// typeflag overrides the type for an entry that is neither a regular
	// file nor a directory — a symlink, say, which has to be refused.
	typeflag byte
	body     string
}

// tarGz builds an archive from explicit entries.
func tarGz(t *testing.T, entries ...treeEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.path, Mode: 0o640, Size: int64(len(e.body))}
		switch {
		case e.typeflag != 0:
			hdr.Typeflag = e.typeflag
			hdr.Linkname = e.body
			hdr.Size = 0
		case e.dir:
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o750
			hdr.Size = 0
		default:
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.path, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write %s: %v", e.path, err)
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

// tarGzDir packs a real directory, so the archive is byte-for-byte the thing
// that was signed.
//
// Rebuilding a tree from a name-to-content map would produce a directory that
// hashes differently from the one the signature covers, and the signature
// check would then fail for a reason that has nothing to do with what is
// under test.
func tarGzDir(t *testing.T, prefix, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		hdr, hdrErr := tar.FileInfoHeader(info, "")
		if hdrErr != nil {
			return hdrErr
		}
		hdr.Name = filepath.ToSlash(filepath.Join(prefix, rel))
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		_, writeErr := tw.Write(body)
		return writeErr
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func writeTarGz(t *testing.T, archive, prefix, dir string) {
	t.Helper()
	if err := os.WriteFile(archive, tarGzDir(t, prefix, dir), 0o640); err != nil {
		t.Fatalf("write %s: %v", archive, err)
	}
}

// assertNothingStaged is the invariant every refusal has to leave behind: the
// staging area is exactly as it was, and no half-written version is sitting
// in it waiting for a push to mistake it for a tree.
func assertNothingStaged(t *testing.T, store *Store) {
	t.Helper()
	entries, err := os.ReadDir(store.VersionsDir())
	if err != nil {
		t.Fatalf("read the staging area: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".incoming") {
			t.Errorf("a failed receive left %s behind", e.Name())
		}
	}
}

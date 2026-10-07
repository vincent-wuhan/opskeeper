package pluginmanifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crypto/ed25519"
	"encoding/base64"
)

// Signing tests.
//
// The properties here are the ones a signature scheme fails silently.
// A test that only round-trips a signature proves that signing works,
// which is not the same as proving that tampering is caught — and the
// second is the only reason any of this code exists.

// newPackage writes a minimal but valid plugin directory and returns it.
//
// It is written to disk rather than constructed in memory because the
// things being tested — walk order, file modes, a file appearing after
// signing — only exist on a filesystem, and a digest computed from a map
// would pass tests that the shipped one fails.
func newPackage(t *testing.T, name, version string) string {
	t.Helper()
	// The directory is named after the package, as it is in a real
	// catalog. That matters beyond realism: a refusal at the signature
	// step identifies its subject by directory, because nothing inside
	// the package has been trusted yet.
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeManifest(t, root, name, version, "L1", "read")
	return root
}

// writeManifest puts a valid pig-ops.yaml in root, and a source file for
// the digest to have something to cover.
func writeManifest(t *testing.T, root, name, version, level, class string) {
	t.Helper()
	manifest := `apiVersion: opskeeper.io/v1
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
	write(t, filepath.Join(root, ManifestFile), manifest)
	write(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n\n// generated\n")
}

// write creates a file and its parents.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// trustFor returns a store that trusts the signer's public key.
func trustFor(t *testing.T, s *Signer) *TrustStore {
	t.Helper()
	store := NewTrustStore()
	if err := store.Trust(s.KeyID(), s.PublicKey()); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	return store
}

func signerFor(t *testing.T, keyID string) *Signer {
	t.Helper()
	s, _, err := GenerateSigner(keyID)
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	return s
}

func TestASignedPackageVerifiesAndTheSidecarDoesNotBreakIt(t *testing.T) {
	// The last part is the one that is easy to get wrong. The signature
	// lives inside the tree it signs, so a digest that included it could
	// never be reproduced; the whole detached-signature scheme depends on
	// excluding exactly that one file and nothing else.
	root := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	store := trustFor(t, s)

	// Read the envelope back off disk rather than reusing the value the
	// signer produced, so the round trip through JSON is covered too.
	got, err := VerifyDir(root, store)
	if err != nil {
		t.Fatalf("a freshly signed package did not verify: %v", err)
	}
	if got.TreeDigest != env.TreeDigest {
		t.Errorf("digest on disk = %s, want %s", got.TreeDigest, env.TreeDigest)
	}
	if got.Name != "acme-probe" || got.Version != "1.0.0" {
		t.Errorf("identity = %s v%s, want acme-probe v1.0.0", got.Name, got.Version)
	}
}

func TestAModifiedFileIsCaughtByTheSignature(t *testing.T) {
	// This is the whole point: the signature is over the tree, so a
	// package edited after review does not verify. Changing one byte of
	// one Go file is the attack this exists to stop.
	root := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	write(t, filepath.Join(root, "extensions", "tool", "tools.go"),
		"package tool\n\n// reviewed code\n// added afterwards\n")

	err = Verify(root, env, trustFor(t, s))
	if err == nil {
		t.Fatal("a package modified after signing verified")
	}
	if !strings.Contains(err.Error(), "does not match its signature") {
		t.Errorf("error = %q, want it to name the mismatch rather than something vaguer", err)
	}
}

func TestAModifiedManifestIsCaughtByTheSignature(t *testing.T) {
	// The most valuable file to edit, because the manifest is what the
	// host turns into an allow-list and a safety level. Widening
	// spec.tools or dropping safety_level after review must be as visible
	// as changing code.
	root := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	writeManifest(t, root, "acme-probe", "1.0.0", "L3", "destructive")

	if err := Verify(root, env, trustFor(t, s)); err == nil {
		t.Fatal("a package whose manifest was upgraded to L3 after signing verified")
	}
}

func TestAnAddedFileIsCaughtByTheSignature(t *testing.T) {
	// Adding is not editing, and a scheme that only hashes the files it
	// was given has a hole exactly the size of "drop a new executable
	// here". The file count is framed into the digest for this reason.
	root := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	write(t, filepath.Join(root, "extensions", "tool", "extra.go"), "package tool\n\nfunc Extra() {}\n")

	if err := Verify(root, env, trustFor(t, s)); err == nil {
		t.Fatal("a package with a file added after signing verified")
	}
}

func TestARemovedFileIsCaughtByTheSignature(t *testing.T) {
	root := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "extensions", "tool", "tools.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := Verify(root, env, trustFor(t, s)); err == nil {
		t.Fatal("a package with a file removed after signing verified")
	}
}

func TestAnUnsignedPackageIsRefusedByDefault(t *testing.T) {
	// A zero-value policy is a node that forgot to configure itself, and
	// it must refuse everything rather than accept anything.
	root := newPackage(t, "acme-probe", "1.0.0")
	store := NewTrustStore()

	d := Review(root, store, Policy{})
	if d.Allowed {
		t.Fatal("an unsigned package was installed by a node with no policy configured")
	}
	if d.Step != StepSignature {
		t.Errorf("step = %q, want %q", d.Step, StepSignature)
	}
}

func TestAPackageSignedByAnUntrustedKeyIsRefused(t *testing.T) {
	// The publisher named in the envelope is a claim by the envelope. A
	// node believes the key it was configured with and nobody else, which
	// is what stops a package from vouching for itself.
	root := newPackage(t, "acme-probe", "1.0.0")
	author := signerFor(t, "acme-2026")
	env, err := author.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	other := signerFor(t, "someone-else")

	err = Verify(root, env, trustFor(t, other))
	if err == nil {
		t.Fatal("a package signed by a key this node does not trust verified")
	}
	if !strings.Contains(err.Error(), "does not trust") {
		t.Errorf("error = %q, want it to say the key is not trusted", err)
	}
}

func TestARevokedKeyStopsVerifying(t *testing.T) {
	// Revocation and an unknown key are different situations and get
	// different answers, because "we used to trust this and stopped" is
	// the one an operator needs to recognise during an incident.
	root := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	store := trustFor(t, s)
	store.Revoke(s.KeyID())

	err = Verify(root, env, store)
	if err == nil {
		t.Fatal("a package signed by a revoked key verified")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error = %q, want it to say the key is revoked rather than merely unknown", err)
	}
}

func TestReTrustingAnIDWithADifferentKeyIsRefused(t *testing.T) {
	// Key rotation under one name is indistinguishable from a compromise
	// being laundered as a rotation, so a new key gets a new id.
	store := NewTrustStore()
	first, firstPub, err := GenerateSigner("opskeeper-release")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	_ = first
	if err := store.Trust("opskeeper-release", firstPub); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	_, secondPub, err := GenerateSigner("opskeeper-release")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	if err := store.Trust("opskeeper-release", secondPub); err == nil {
		t.Error("re-using a key id for a different key was accepted")
	}
	// Re-adding the identical key is a config that lists it twice, which
	// is a nuisance rather than an attack.
	if err := store.Trust("opskeeper-release", firstPub); err != nil {
		t.Errorf("re-adding the identical key was refused: %v", err)
	}
}

func TestAPackageSignedByAnotherKeyCannotKeepThatPackagesSignature(t *testing.T) {
	// The realistic version of "moving" a signature: take a package that
	// somebody trusted signed, relabel it, and present it as their own.
	//
	// The digest catches this one, and it is worth being explicit that it
	// is the digest and not the identity check doing the work — the name
	// lives in the manifest, the manifest lives in the tree, so relabelling
	// changes the digest. The identity check below exists so that a future
	// change to the digest cannot quietly leave the signature unbound.
	victim := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(victim)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	impostor := newPackage(t, "acme-drain", "9.9.9")
	if err := Verify(impostor, env, trustFor(t, s)); err == nil {
		t.Fatal("a genuine signature verified against a relabelled package")
	}
}

func TestAVerifiedSignatureMustNameThePackageItIsAttachedTo(t *testing.T) {
	// The identity check on its own.
	//
	// This is a white-box test and it has to be: because the manifest is
	// inside the signed tree, no sequence of edits a real attacker can
	// make reaches this check — a changed name changes the digest and the
	// package is refused earlier. So the only way to exercise it is to
	// build the one thing an attacker cannot: a genuine, correctly signed
	// envelope that names a different package than the tree it sits in.
	//
	// It is worth exactly that much. The check is the last thing standing
	// between "signed" and "signed *about this*", and an untested
	// last line is how a refactor deletes it.
	root := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	store := trustFor(t, s)

	digest, err := TreeDigest(root)
	if err != nil {
		t.Fatalf("TreeDigest: %v", err)
	}
	// Correct digest, correctly signed — for a package that is not this one.
	forged := Envelope{
		KeyID:      s.KeyID(),
		Algorithm:  SignAlgorithm,
		Name:       "acme-drain",
		Version:    "9.9.9",
		TreeDigest: digest,
	}
	raw, err := forged.canonicalPayload()
	if err != nil {
		t.Fatalf("canonicalPayload: %v", err)
	}
	forged.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(s.private, raw))

	err = Verify(root, forged, store)
	if err == nil {
		t.Fatal("a valid signature for a different package verified against this one")
	}
	if !strings.Contains(err.Error(), "cannot be moved") {
		t.Errorf("error = %q, want it to say the signature is for another package", err)
	}
}

func TestAnEnvelopeDeclaringAnUnknownAlgorithmIsRefused(t *testing.T) {
	// "Probably fine" is the failure mode here. A node that falls back to
	// accepting a signature in an algorithm it has not implemented has
	// removed the only property it had.
	root := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	env.Algorithm = "rsa-pss-sha256"

	err = Verify(root, env, trustFor(t, s))
	if err == nil {
		t.Fatal("an envelope in an unimplemented algorithm verified")
	}
	if !strings.Contains(err.Error(), SignAlgorithm) {
		t.Errorf("error = %q, want it to name the algorithm it does accept", err)
	}
}

func TestTheTreeDigestCannotBeConfusedByMovingContentBetweenFiles(t *testing.T) {
	// The framing attack: without length prefixes, a file "ab" holding "c"
	// and a file "a" holding "bc" produce the same byte stream, so a
	// signature over one verifies over the other. Two packages built to
	// collide must not.
	first := t.TempDir()
	write(t, filepath.Join(first, ManifestFile), minimalManifest("acme-probe", "1.0.0"))
	write(t, filepath.Join(first, "ab"), "c")

	second := t.TempDir()
	write(t, filepath.Join(second, ManifestFile), minimalManifest("acme-probe", "1.0.0"))
	write(t, filepath.Join(second, "a"), "bc")

	one, err := TreeDigest(first)
	if err != nil {
		t.Fatalf("TreeDigest: %v", err)
	}
	two, err := TreeDigest(second)
	if err != nil {
		t.Fatalf("TreeDigest: %v", err)
	}
	if one == two {
		t.Error("two differently-shaped trees produced the same digest; the field framing is ambiguous")
	}
}

func TestTheTreeDigestIgnoresWalkOrderButNotContent(t *testing.T) {
	first := t.TempDir()
	for name, body := range map[string]string{"a": "1", "b": "2", "c": "3"} {
		write(t, filepath.Join(first, name), body)
	}
	second := t.TempDir()
	for name, body := range map[string]string{"c": "3", "b": "2", "a": "1"} {
		write(t, filepath.Join(second, name), body)
	}

	one, _ := TreeDigest(first)
	two, _ := TreeDigest(second)
	if one != two {
		t.Error("the digest depends on the order files were created; it must be a property of the tree")
	}

	write(t, filepath.Join(second, "a"), "1-changed")
	three, _ := TreeDigest(second)
	if three == one {
		t.Error("changing a file's contents did not change the digest")
	}
}

func TestTheTreeDigestDistinguishesAnExecutableFromAPlainFile(t *testing.T) {
	// A package that ships a build script as executable and the same
	// script as data are not the same package, and the executable bit is
	// what changes what a build does.
	plain := t.TempDir()
	write(t, filepath.Join(plain, "run.sh"), "#!/bin/sh\nexit 0\n")
	exec := t.TempDir()
	run := filepath.Join(exec, "run.sh")
	write(t, run, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(run, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	one, _ := TreeDigest(plain)
	two, _ := TreeDigest(exec)
	if one == two {
		t.Error("an executable file and a plain one hash the same")
	}
}

func TestTheTreeDigestRefusesASymlink(t *testing.T) {
	// A symlink is a pointer, and what it points at can differ between
	// the machine that signed and the node that verifies. Hashing the
	// target would make the digest a statement about a file that is not
	// in the package.
	root := t.TempDir()
	write(t, filepath.Join(root, "real"), "content")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, err := TreeDigest(root); err == nil {
		t.Error("a tree containing a symlink was hashed instead of refused")
	}
}

func TestTheTreeDigestRefusesAnEmptyPackage(t *testing.T) {
	if _, err := TreeDigest(t.TempDir()); err == nil {
		t.Error("an empty package hashed successfully; there is nothing to sign")
	}
}

func TestATrustStoreWithNoKeysTrustsNothing(t *testing.T) {
	// A nil store and an empty one are the same answer, and it is "no".
	var nilStore *TrustStore
	root := newPackage(t, "acme-probe", "1.0.0")
	s := signerFor(t, "acme-2026")
	env, _ := s.Sign(root)

	if err := Verify(root, env, nilStore); err == nil {
		t.Error("a nil trust store verified a package")
	}
	if err := Verify(root, env, NewTrustStore()); err == nil {
		t.Error("an empty trust store verified a package")
	}
}

func TestASignerRefusesAKeyOfTheWrongSize(t *testing.T) {
	if _, err := NewSigner("k", ed25519.PrivateKey("too short")); err == nil {
		t.Error("a signer was built from a key that is not an ed25519 private key")
	}
	if _, err := NewSigner("", make(ed25519.PrivateKey, ed25519.PrivateKeySize)); err == nil {
		t.Error("a signer was built with no key id")
	}
}

// minimalManifest is a manifest with no tools, for digest tests where the
// contents of the manifest are not what is being tested.
func minimalManifest(name, version string) string {
	return `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: ` + name + `
  version: ` + version + `
  vendor: acme
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  tools: []
  required_scopes: [host.read]
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling}
`
}

func TestTrustStoreFromConfigRefusesAnEmptyFile(t *testing.T) {
	// A trust store that parses to nothing is far more likely to be a
	// mistake than a policy, and a node that silently trusts nothing
	// refuses every package for reasons nobody can see.
	path := filepath.Join(t.TempDir(), "trust.yaml")
	write(t, path, "# no keys here\n")
	if _, err := TrustStoreFromConfig(path); err == nil {
		t.Error("a trust store with no keys was accepted")
	}
}

func TestTrustStoreFromConfigLoadsARealStore(t *testing.T) {
	// Adding a publisher has to be a configuration change, or an
	// ecosystem nobody extends is the one that happens.
	s := signerFor(t, "acme-2026")
	path := filepath.Join(t.TempDir(), "trust.yaml")
	write(t, path, `# who is allowed to sign a package for this node
keys:
  - id: acme-2026
    key: `+base64.StdEncoding.EncodeToString(s.PublicKey())+`
`)

	store, err := TrustStoreFromConfig(path)
	if err != nil {
		t.Fatalf("TrustStoreFromConfig: %v", err)
	}
	root := newPackage(t, "acme-probe", "1.0.0")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify(root, env, store); err != nil {
		t.Errorf("a key loaded from configuration did not verify: %v", err)
	}
}

func TestTrustStoreFromConfigRefusesAnUnreadableKey(t *testing.T) {
	// A key that is not base64 is a typo or a paste of the wrong thing.
	// Either way it must not be silently skipped, because a store that
	// skips unparseable entries is a store that trusts fewer keys than the
	// operator wrote down, and the symptom is an unexplained refusal later.
	path := filepath.Join(t.TempDir(), "trust.yaml")
	write(t, path, "keys:\n  - id: acme-2026\n    key: not-base64!!\n")
	if _, err := TrustStoreFromConfig(path); err == nil {
		t.Error("a trust store with an unparseable key was accepted")
	}
}

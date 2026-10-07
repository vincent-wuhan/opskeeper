package pluginmanifest

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// The set of keys a node will believe.
//
// A trust store that starts empty and is added to is the only shape worth
// having. The alternative — a store seeded with "the" OpsKeeper key — is
// a store where a package is signed by whoever holds that key, and the
// question of who holds it is the entire security property.

// TrustStore maps key ids to the public keys a node accepts.
type TrustStore struct {
	keys map[string]ed25519.PublicKey
	// revoked holds ids removed from service. A revoked id is kept rather
	// than deleted so that an envelope naming it is refused as revoked
	// rather than as unknown: "this key was ours and we stopped trusting
	// it" is a different and more useful thing to tell an operator than
	// "who is this?".
	revoked map[string]bool
	// loadErr records a configured-but-unreadable trust store.
	//
	// It travels on the store rather than being returned, because the
	// caller that most needs it is the one deciding whether this node
	// accepts unsigned packages, and an empty store looks exactly like a
	// store nobody configured. Without this field the natural handling of
	// a broken trust file is to carry on with no keys — which on a node
	// whose rule is "no keys means unsigned allowed" turns a typo in a
	// path into a silent downgrade of the node's security.
	loadErr error
}

// SetLoadError records that this store could not be read from its
// configured source.
func (s *TrustStore) SetLoadError(err error) {
	if s != nil {
		s.loadErr = err
	}
}

// LoadError reports why the store could not be read, or nil.
func (s *TrustStore) LoadError() error {
	if s == nil {
		return nil
	}
	return s.loadErr
}

// NewTrustStore returns an empty store, which trusts nothing.
func NewTrustStore() *TrustStore {
	return &TrustStore{keys: map[string]ed25519.PublicKey{}, revoked: map[string]bool{}}
}

// Trust adds a public key under an id.
//
// Re-adding an id with a *different* key is refused, because a store
// where the same name resolves to two keys over time is a store where a
// compromise can be laundered as a rotation. Re-adding the identical key
// is a no-op, so a config that lists a key twice is not an error.
func (s *TrustStore) Trust(keyID string, pub ed25519.PublicKey) error {
	if s == nil {
		return errors.New("pluginmanifest: a nil trust store trusts nothing and cannot be added to")
	}
	if strings.TrimSpace(keyID) == "" {
		return errors.New("pluginmanifest: a key needs an id")
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("pluginmanifest: key %q is %d bytes, want an ed25519 public key (%d)",
			keyID, len(pub), ed25519.PublicKeySize)
	}
	if s.revoked[keyID] {
		return fmt.Errorf("pluginmanifest: key %q is revoked; re-trusting a revoked id needs a new id", keyID)
	}
	if s.keys == nil {
		s.keys = map[string]ed25519.PublicKey{}
	}
	if prev, ok := s.keys[keyID]; ok && !prev.Equal(pub) {
		return fmt.Errorf("pluginmanifest: key id %q is already trusted with a different key; "+
			"give a rotated key a new id rather than reusing one", keyID)
	}
	s.keys[keyID] = pub
	return nil
}

// Revoke removes a key from service.
func (s *TrustStore) Revoke(keyID string) {
	if s == nil {
		return
	}
	delete(s.keys, keyID)
	s.revoked[keyID] = true
}

// Revoked reports whether an id was explicitly revoked.
func (s *TrustStore) Revoked(keyID string) bool {
	return s != nil && s.revoked[keyID]
}

// KeyIDs returns the trusted ids, for diagnostics and for the console's
// "who can sign" listing.
func (s *TrustStore) KeyIDs() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.keys))
	for id := range s.keys {
		out = append(out, id)
	}
	return out
}

// TrustStoreFromConfig reads a store from the node's configuration file.
//
// The file is a list of id → base64 public key under a `keys:` key. It
// exists so that adding a publisher is a configuration change an operator
// makes, rather than a rebuild: a plugin ecosystem where adding a signer
// requires shipping a binary is one nobody extends.
//
// The wrapper is there for the file's own future, not for today's sake —
// a store that can also carry an expiry or a note will need a top-level
// object, and a format that is a bare top-level array cannot grow one.
func TrustStoreFromConfig(path string) (*TrustStore, error) {
	store := NewTrustStore()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the trust store %s: %w", path, err)
	}
	var file struct {
		Keys []struct {
			ID  string `yaml:"id"`
			Key string `yaml:"key"`
		} `yaml:"keys"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("%s: not a valid trust store: %w", path, err)
	}
	entries := file.Keys
	if len(entries) == 0 {
		// A store with no keys trusts nothing, which is safe. A file that
		// parses to nothing is more likely a mistake than a policy, so it
		// is refused rather than accepted.
		return nil, fmt.Errorf("%s declares no keys; a node with no trusted keys can install no plugins", path)
	}
	for _, e := range entries {
		raw, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(e.Key))
		if decErr != nil {
			return nil, fmt.Errorf("%s: key %q is not base64: %w", path, e.ID, decErr)
		}
		if err := store.Trust(e.ID, raw); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return store, nil
}

// Verify checks an envelope against the package on disk and a trust store.
//
// The order is the whole design:
//
//  1. Reject an algorithm we do not implement, before anything else.
//     A "signature" in an algorithm nobody has reviewed is not a
//     signature, and falling back to "probably fine" here would defeat
//     everything below.
//  2. Resolve the key. An unknown id and a revoked id are different
//     answers, and neither is "try another key".
//  3. Recompute the tree digest from what is actually on disk now and
//     compare it to what was signed. A package edited after signing
//     fails here, before its manifest is parsed.
//  4. Check the signature over the canonical payload.
//  5. Check the envelope's identity against the manifest, so a valid
//     signature cannot be moved to a package with a different name.
//
// Nothing here parses the manifest. That is deliberate — see Review,
// which is the place a manifest is read, and only after this returns.
func Verify(root string, env Envelope, store *TrustStore) error {
	if env.Algorithm != SignAlgorithm {
		return fmt.Errorf("%s was signed with %q; this node only verifies %q",
			env.Name, env.Algorithm, SignAlgorithm)
	}
	if strings.TrimSpace(env.KeyID) == "" {
		return errors.New("pluginmanifest: the envelope names no key")
	}
	if !validHexDigest(env.TreeDigest) {
		return fmt.Errorf("pluginmanifest: the envelope carries %q, which is not a sha256 digest",
			env.TreeDigest)
	}

	if store == nil {
		return fmt.Errorf("%s is signed by %q, and this node has no trusted keys",
			env.Name, env.KeyID)
	}
	if store.Revoked(env.KeyID) {
		return fmt.Errorf("%s is signed by %q, which this node has revoked", env.Name, env.KeyID)
	}
	pub, ok := store.keys[env.KeyID]
	if !ok {
		return fmt.Errorf("%s is signed by %q, which this node does not trust", env.Name, env.KeyID)
	}

	digest, err := TreeDigest(root)
	if err != nil {
		return err
	}
	if digest != env.TreeDigest {
		// The two most likely causes are a package edited after signing
		// and a package built from a different tree than the one that was
		// reviewed. Both are the operator's to resolve, so both are named.
		return fmt.Errorf("%s does not match its signature: it now hashes to %s but was signed as %s; "+
			"the package was either modified after signing or was not built from the reviewed tree",
			env.Name, short(digest), short(env.TreeDigest))
	}

	raw, err := env.canonicalPayload()
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		return fmt.Errorf("pluginmanifest: the envelope's signature is not base64: %w", err)
	}
	if !ed25519.Verify(pub, raw, sig) {
		return fmt.Errorf("%s carries a signature that key %q did not make", env.Name, env.KeyID)
	}

	// The signature is sound. Now it has to be about *this* package.
	name, version, err := manifestIdentity(root)
	if err != nil {
		return err
	}
	if name != env.Name || version != env.Version {
		return fmt.Errorf("the package is %s v%s but the signature is for %s v%s; "+
			"a valid signature cannot be moved to a different package",
			name, version, env.Name, env.Version)
	}
	return nil
}

// VerifyDir reads the sidecar out of root and verifies it.
func VerifyDir(root string, store *TrustStore) (Envelope, error) {
	env, err := ReadEnvelope(root)
	if err != nil {
		return env, err
	}
	return env, Verify(root, env, store)
}

// short trims a digest for an error message. A full sha256 in prose is
// unreadable and the first twelve characters are enough to compare two.
func short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

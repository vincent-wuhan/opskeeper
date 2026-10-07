package pluginmanifest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// Signing binds a plugin's contents to an identity.
//
// The problem it solves is specific and was not solvable by hashing. The
// node already refuses a tool its installed manifest did not declare, and
// the manifest already declares a safety level — but every one of those
// facts is a file sitting on the node's disk. Anyone who can write that
// directory can write a manifest that declares `host_restart_service` as
// read, or that names a scope nobody granted. Hashing does not help,
// because the attacker recomputes the hash.
//
// Signing helps because the key is not in that directory. A signature says
// "the operator's release key vouched for exactly these bytes", and it says
// it about the *whole tree*, so the manifest and the Go source it governs
// cannot be separated after the fact. That is the property the whole
// review pipeline rests on: without it, admission is a conversation about
// a file rather than a check.

// SignatureFile is the sidecar's filename inside a plugin root.
//
// It is a sidecar and not a field inside pig-ops.yaml because the
// signature covers the manifest. A signature carried by the file it signs
// would be covering itself, which is not verification.
const SignatureFile = "pig-ops.sig"

// SignAlgorithm is the only algorithm this package produces or accepts.
//
// It is a named constant rather than a field with a default so that a
// future algorithm is a deliberate addition: an envelope claiming some
// algorithm nobody has reviewed is refused, not guessed at.
const SignAlgorithm = "ed25519"

// signaturePayloadPrefix domain-separates the signed bytes from anything
// else in the system that might be signed with the same key. A key used
// for two purposes must not let one signature be replayed as the other.
const signaturePayloadPrefix = "opskeeper.io/plugin-signature/v1\n"

// Envelope is a detached signature over one plugin tree.
//
// Every field here is part of the claim, and the ones that are not the
// signature itself are covered by it. Signing the name and version as well
// as the digest costs a few bytes and means an envelope cannot be moved
// from one package to another: a signature for opskeeper-sre-readonly will
// not verify for a package that has renamed itself to it.
type Envelope struct {
	// KeyID names the key that signed, and the store resolves it. It is an
	// identifier, not a key: the public key ships with the node, and an
	// envelope that carried its own public key would be verifying itself.
	KeyID string `json:"key_id"`
	// Algorithm is SignAlgorithm. Anything else is refused.
	Algorithm string `json:"algorithm"`
	// Name and Version identify the package the signature is for. Both
	// are covered by the signature and both are re-checked against the
	// manifest after it is loaded.
	Name    string `json:"name"`
	Version string `json:"version"`
	// TreeDigest is the hex sha256 the signature was computed over.
	TreeDigest string `json:"tree_digest"`
	// Signature is the base64 ed25519 signature over the canonical payload.
	Signature string `json:"signature"`
	// SignedAt is advisory. It is not used in the payload, because a
	// clock is not a security property — an envelope with a wrong date is
	// a question for the operator, not a reason to refuse a package.
	SignedAt time.Time `json:"signed_at"`
}

// Signer produces envelopes for packages a release process owns.
type Signer struct {
	keyID   string
	private ed25519.PrivateKey
}

// NewSigner returns a Signer for an ed25519 private key.
//
// The key id is chosen by the operator rather than derived from the key,
// because a derived id makes rotation look like a new publisher and a
// human-chosen one lets the same id carry a new key on purpose.
func NewSigner(keyID string, private ed25519.PrivateKey) (*Signer, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, errors.New("pluginmanifest: a signer needs a key id")
	}
	if len(private) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("pluginmanifest: key %q is %d bytes, want an ed25519 private key (%d)",
			keyID, len(private), ed25519.PrivateKeySize)
	}
	return &Signer{keyID: keyID, private: private}, nil
}

// GenerateSigner creates a fresh key pair. Used by the release tooling and
// by tests; a production operator holds their key in their own vault and
// calls NewSigner with it.
func GenerateSigner(keyID string) (*Signer, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("pluginmanifest: generate key: %w", err)
	}
	s, err := NewSigner(keyID, priv)
	if err != nil {
		return nil, nil, err
	}
	return s, pub, nil
}

// PublicKey returns the verifying half.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.private.Public().(ed25519.PublicKey) }

// KeyID returns the signer's identifier.
func (s *Signer) KeyID() string { return s.keyID }

// Sign produces an envelope over the package at root.
//
// It hashes the tree as it is on disk, so signing and then modifying
// anything — the manifest included — produces an envelope that no longer
// verifies. There is no separate "sign the manifest" path, because that
// would be a path where a package's authority and its code are separate
// things.
func (s *Signer) Sign(root string) (Envelope, error) {
	digest, err := TreeDigest(root)
	if err != nil {
		return Envelope{}, err
	}
	// The identity is read from the manifest rather than taken as an
	// argument, so an envelope cannot claim a name the package does not
	// carry. The manifest is parsed for this only; the review pipeline
	// validates it properly afterwards.
	name, version, err := manifestIdentity(root)
	if err != nil {
		return Envelope{}, err
	}

	env := Envelope{
		KeyID:      s.keyID,
		Algorithm:  SignAlgorithm,
		Name:       name,
		Version:    version,
		TreeDigest: digest,
		SignedAt:   time.Now().UTC().Truncate(time.Second),
	}
	raw, err := env.canonicalPayload()
	if err != nil {
		return Envelope{}, err
	}
	env.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(s.private, raw))
	return env, nil
}

// WriteTo saves the envelope into the package root, and returns the path
// it was written to.
//
// It is a separate call from Sign because the file it writes is part of
// the tree — so writing it changes the digest, and signing again would
// produce a signature over a tree that includes the previous signature.
// Signing therefore covers the tree *without* the sidecar, and Verify
// excludes it for the same reason. A package that carries a signature
// therefore does not need to be re-signed to move from machine to
// machine, which is the entire point of a detached signature.
func (e Envelope) WriteTo(root string) (string, error) {
	path := filepath.Join(root, SignatureFile)
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode the signature: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write the signature: %w", err)
	}
	return path, nil
}

// Transport returns the envelope as a base64 string, for a control plane
// that ships a package out of band.
//
// It is the same envelope the sidecar carries, and it is not a second
// signature. The node's authority is still the file inside the tree — the
// one the tree digest covers — and this copy is read only to make a
// mismatch legible: a request whose KeyID names a key the node does not
// hold, or an envelope that disagrees with the sidecar, produces a refusal
// an operator can act on instead of a signature failure with no subject.
func (e Envelope) Transport() string {
	data, err := json.Marshal(e)
	if err != nil {
		// Envelope is four strings and a time; it cannot fail to encode.
		// Returning empty rather than panicking means a caller with no
		// transport to offer sends an empty string, and the node refuses
		// for want of a signature — which is the right outcome anyway.
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

// ReadEnvelope loads the sidecar from a package root.
func ReadEnvelope(root string) (Envelope, error) {
	var env Envelope
	data, err := os.ReadFile(filepath.Join(root, SignatureFile))
	if err != nil {
		if os.IsNotExist(err) {
			return env, fmt.Errorf("%s has no %s; an unsigned package cannot be reviewed",
				root, SignatureFile)
		}
		return env, fmt.Errorf("read %s: %w", SignatureFile, err)
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return env, fmt.Errorf("%s is not a valid envelope: %w", SignatureFile, err)
	}
	return env, nil
}

// canonicalPayload is the exact bytes the signature covers.
//
// The field order and the separators are fixed by construction, not by a
// marshaller, so a change to a struct tag elsewhere cannot silently change
// what a signature means. Only the fields that identify the claim are
// included; SignedAt is deliberately absent, and TreeDigest is the digest
// of the tree computed with this same file excluded.
func (e Envelope) canonicalPayload() ([]byte, error) {
	var b strings.Builder
	b.WriteString(signaturePayloadPrefix)
	fmt.Fprintf(&b, "name=%s\n", e.Name)
	fmt.Fprintf(&b, "version=%s\n", e.Version)
	fmt.Fprintf(&b, "tree=%s\n", e.TreeDigest)
	// The algorithm and the key id are signed too, so a signature made
	// with one algorithm cannot be relabelled as another and so a key
	// cannot be swapped on a replayed envelope.
	fmt.Fprintf(&b, "algorithm=%s\n", e.Algorithm)
	fmt.Fprintf(&b, "key=%s\n", e.KeyID)
	return []byte(b.String()), nil
}

// manifestIdentity pulls just the name and version out of a package's
// manifest, without running the full validation.
//
// It is used twice, and the difference between them is the whole reason
// it is not simply Load:
//
//   - At signing time, by the party that owns the key, on a package it
//     just built. A manifest that would fail admission is still a
//     package somebody may want signed — the signing key is not the
//     admission decision, and a release pipeline that cannot sign
//     something before it is reviewed is a pipeline where "reviewed" and
//     "signed" drift apart.
//   - At verification time, as the last check that an envelope is about
//     this package. By then the tree has already been hashed and the
//     signature checked, so the manifest it reads is authenticated
//     bytes; the strict parse happens in Review, immediately after.
//
// So the parse here is lenient about fields and strict about the two it
// needs. A misspelled metadata.name yields an empty name and an error,
// which is the same outcome either way.
func manifestIdentity(root string) (string, string, error) {
	var m domain.PluginManifest
	data, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", ManifestFile, err)
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return "", "", fmt.Errorf("%s: not valid: %w", ManifestFile, err)
	}
	if strings.TrimSpace(m.Metadata.Name) == "" || strings.TrimSpace(m.Metadata.Version) == "" {
		return "", "", fmt.Errorf("%s declares no metadata.name or metadata.version to sign",
			ManifestFile)
	}
	return m.Metadata.Name, m.Metadata.Version, nil
}

// validHexDigest reports whether s is a 64-character lower-hex sha256.
//
// Checked rather than assumed because a malformed digest in an envelope
// would otherwise be hashed and compared, turning a typo into a confusing
// "signature does not match" instead of "this envelope is not a digest".
func validHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}
	return strings.ToLower(s) == s
}

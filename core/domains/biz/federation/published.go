package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Getting a tree to a child that cannot read this root's disk.
//
// FileDistributor is the zero-configuration path and it is not a toy, but it
// only works when the two sides share a mount. The other deployment shape is
// the one most multi-cluster installations actually have: an artifact store,
// an object bucket, a CDN in front of one, with the root publishing into it by
// some means that is not this package.
//
// The ledger recorded that gap as "需要 CDN 或对象存储——外部条件", and that
// reading was wrong in a way worth correcting, because it put the missing
// piece on the wrong side of the wire. The child has been able to fetch
// http and https since the scheme allowlist was written
// (service/federationchild.checkSourceScheme), with the same 4 MiB cap, the
// same digest-before-unpack and the same signature gate as every other path.
// Nothing on the receiving end was waiting for a CDN.
//
// What was missing is a Distributor that *addresses* a store instead of
// writing to a directory. This file is that, and it is deliberately not a
// file server — serve.go's reasoning still holds, and a second unauthenticated
// copy of every policy tree on the internet is a worse outcome than a
// deployment that has to configure an artifact store.

// ErrNotPublished means the artifact store does not have this tree yet.
//
// It is retryable, and the reason it is not ErrNoDeliveryPath is that the two
// mean different things to whoever is looking: no delivery path is a root that
// was never configured for federation at all, while this is a configured root
// whose publish step has not finished. The second is a "not yet" that
// converges on its own, the first needs a human. Collapsing them would make a
// rollout that is thirty seconds from done look like a misconfiguration.
var ErrNotPublished = errors.New("federation: the artifact store does not have this policy tree yet")

// ErrPublishedMismatch means the store has bytes at that address and they are
// not the bytes this root signed.
//
// It is deliberately NOT retryable, and it is the only error in this file that
// is not. Every other failure here is a timing problem that a later attempt
// fixes. This one is a conflict: something published an archive under this
// cluster's name and version whose digest is not the digest of the tree this
// root just signed. Retrying produces the same conflict forever, and a
// delivery loop that never converges and never says why is the exact failure
// Distributor's own contract warns about — so this is reported as a conflict
// and stops the rollout instead of spinning it.
//
// The safe reading is that the store is authoritative about what it holds and
// this root is authoritative about what it signed, and those two disagree.
// Publishing would overwrite a tree a child may already be fetching, so the
// answer is to stop and let a human say which of the two is wrong.
var ErrPublishedMismatch = errors.New("federation: the artifact store holds a different tree under this name")

// PublishedLedger answers what a deployment's artifact store holds at a name.
//
// It is a port for the same reason Distributor is: the store is a deployment
// fact. An implementation backed by S3, OSS, a CDN's manifest API or a
// database all look the same from here, and this package deliberately knows
// none of them.
//
// The digest is required rather than optional because it is the whole
// verification. A distributor that returned a URL and hoped the store held the
// right bytes would turn every delivery into either a silent mismatch or a
// child-side integrity failure, and both look like the child's fault.
type PublishedLedger interface {
	// PublishedDigest returns the hex sha256 the store serves for name, and
	// whether it has it at all.
	PublishedDigest(name string) (digest string, ok bool)
}

// PublishedDistributor addresses a tree in a store this root does not serve.
//
// It wraps a FileDistributor rather than replacing it, and the reason is
// redelivery. A redelivery cannot repack: a tar of the same tree produced
// twice can differ in one header field, and the child compares the digest
// before it unpacks anything, so a repacked archive is a digest that matches
// nothing. FileDistributor already keeps the exact bytes of the first
// delivery and re-reads them, so the published distributor inherits that and
// adds a question — are those bytes the ones the store is serving — without
// reimplementing either half.
type PublishedDistributor struct {
	local  *FileDistributor
	base   string
	ledger PublishedLedger
}

var (
	_ Distributor = (*PublishedDistributor)(nil)
	_ Redeliverer = (*PublishedDistributor)(nil)
)

// NewPublishedDistributor addresses a store rooted at base.
//
// base must be http or https, and that is checked here rather than at first
// use for the same reason checkSourceScheme checks on the child: the scheme is
// a decision about whether this root is willing to publish policy to that
// place, and the only code entitled to make it is the code that owns the
// decision. A base of "example.com/policies" — no scheme, the shape a person
// types — is refused rather than assumed to be https, because assuming it
// would put signed policy trees on the internet at an address nobody chose.
func NewPublishedDistributor(local *FileDistributor, base string, ledger PublishedLedger) (*PublishedDistributor, error) {
	if local == nil {
		return nil, errors.New("federation: a published distributor needs the local distributor it keeps exact bytes in")
	}
	if ledger == nil {
		return nil, errors.New("federation: a published distributor needs a ledger to check the store against")
	}
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("federation: parse the artifact base URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	case "":
		return nil, fmt.Errorf("federation: the artifact base URL %q names no scheme, so it names no "+
			"place to publish to; write it as https://host/prefix", base)
	default:
		return nil, fmt.Errorf("federation: the artifact base URL scheme %q is not http or https", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("federation: the artifact base URL %q names no host", base)
	}
	return &PublishedDistributor{local: local, base: trimmed, ledger: ledger}, nil
}

// urlFor is where a child fetches one archive from the store.
func (d *PublishedDistributor) urlFor(name string) string {
	return d.base + "/" + name
}

// Distribute packs the tree, keeps the exact bytes locally, and then refuses
// to name a URL until the store is holding those same bytes.
//
// The order is the safety property and it is the reverse of what a naive
// implementation would do. Packing first means this root always knows the
// digest of what it signed; asking the store second means the URL it hands out
// is one whose bytes it has verified rather than one it hopes for. A
// distributor that named the URL first and checked later would, in the window
// between, have told a child to fetch bytes it had not compared against
// anything.
func (d *PublishedDistributor) Distribute(ctx context.Context, b federation.Bundle, stagedRoot string) (tunnel.PolicySource, error) {
	local, err := d.local.Distribute(ctx, b, stagedRoot)
	if err != nil {
		return tunnel.PolicySource{}, err
	}
	return d.verified(b, local.ArchiveSHA256)
}

// SourceFor answers for a delivery that already happened.
//
// It re-reads the local archive rather than repacking, for the reason
// Redeliverer documents: the child compares against the digest of the bytes
// the first delivery named, and only those bytes can produce it.
func (d *PublishedDistributor) SourceFor(b federation.Bundle) (tunnel.PolicySource, error) {
	local, err := d.local.SourceFor(b)
	if err != nil {
		return tunnel.PolicySource{}, err
	}
	return d.verified(b, local.ArchiveSHA256)
}

// verified is the half both entry points share: having a digest, decide
// whether the store is serving those bytes and only then name a URL.
func (d *PublishedDistributor) verified(b federation.Bundle, digest string) (tunnel.PolicySource, error) {
	name := archiveName(b)
	published, ok := d.ledger.PublishedDigest(name)
	switch {
	case !ok:
		return tunnel.PolicySource{}, fmt.Errorf("%w: %s is not in the store at %s yet",
			ErrNotPublished, name, d.base)
	case !strings.EqualFold(strings.TrimSpace(published), digest):
		return tunnel.PolicySource{}, fmt.Errorf("%w: %s is in the store at %s with digest %s, "+
			"and this root signed %s", ErrPublishedMismatch, name, d.base, published, digest)
	}
	return tunnel.PolicySource{
		URL:           d.urlFor(name),
		ArchiveSHA256: digest,
	}, nil
}

// Dir reports the local directory the exact bytes are kept in.
//
// It exists so a root wired with either distributor can answer the same
// operator question — "where are my archives" — without the wiring having to
// know which one it got. The local directory is still meaningful here, and
// not only as an implementation detail: it is where a redelivery reads the
// bytes it must not repack.
func (d *PublishedDistributor) Dir() string { return d.local.Dir() }

// ManifestLedger is a PublishedLedger backed by a JSON file the deployment's
// own publish step writes.
//
// It exists because the port needs an implementation that a deployment can
// actually run today, and the alternatives all add something: an S3 client
// adds a dependency and a credential path, a CDN manifest API adds a vendor
// protocol. A file of `{"<archive name>": "<sha256>"}` needs none of those —
// whatever already uploads the archive also writes one line, and that is a
// step the deployment was going to have anyway.
//
// The file is re-read on every query rather than loaded once. That is the
// property that makes it usable at all: the publish step runs out of band, so
// a manifest read at boot is a snapshot that is wrong for the rest of the
// process's life, and a rollout that needs a restart to notice its own
// artifact was uploaded is a rollout that will be restarted by somebody
// guessing why.
type ManifestLedger struct {
	path string
}

// PublishedDigest re-reads the manifest and answers for one name.
//
// A missing or unreadable manifest answers "not there" rather than an error,
// because "not there" and "I cannot tell" both mean the same thing to the
// only caller: do not name a URL yet. The distinction would matter for
// reporting, and reporting is the deployment's publish step's job — it knows
// whether it ran.
func (m ManifestLedger) PublishedDigest(name string) (string, bool) {
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return "", false
	}
	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", false
	}
	digest := strings.TrimSpace(manifest[name])
	if digest == "" {
		return "", false
	}
	return digest, true
}

// RecordPublished writes the digest the store is serving for one name.
//
// It is the write half of the same file PublishedDigest re-reads on every
// call, and it exists because a root that publishes its own trees has to
// remember that it did. Without it the loop is open at the last step: the
// publisher uploads, the store serves, and the very next delivery still reads
// "not there" because the only record of the upload was in a process that
// exited.
//
// The write is a read-modify-write of the whole map, which is the right shape
// for a file this small and is also the shape that can lose a concurrent
// writer's entry. That is accepted rather than solved: this file is written
// by one root, at publish time, and a deployment that wants more than one
// writer wants a store with an index, which is the thing this file stands in
// for until then.
func (m ManifestLedger) RecordPublished(name, digest string) error {
	trimmedName := strings.TrimSpace(name)
	if trimmedName == "" {
		return errors.New("federation: record a published tree under no name")
	}
	trimmedDigest := strings.TrimSpace(digest)
	if trimmedDigest == "" {
		return fmt.Errorf("federation: record %s with no digest; a child that fetched it would "+
			"have nothing to compare against", trimmedName)
	}
	manifest := map[string]string{}
	if raw, err := os.ReadFile(m.path); err == nil {
		// A missing file is the normal first-publish case. An unreadable or
		// unparseable one is not, and silently replacing it would drop every
		// other tree this root has published.
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &manifest); err != nil {
				return fmt.Errorf("federation: the published manifest at %s is not readable as JSON: %w",
					m.path, err)
			}
		}
	}
	manifest[trimmedName] = trimmedDigest
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("federation: render the published manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o750); err != nil {
		return fmt.Errorf("federation: create the published manifest's directory: %w", err)
	}
	if err := os.WriteFile(m.path, append(encoded, '\n'), 0o640); err != nil {
		return fmt.Errorf("federation: write the published manifest at %s: %w", m.path, err)
	}
	return nil
}

// NewManifestLedger reads digests from the manifest at path.
func NewManifestLedger(path string) (ManifestLedger, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ManifestLedger{}, errors.New("federation: a manifest ledger needs a path")
	}
	return ManifestLedger{path: trimmed}, nil
}

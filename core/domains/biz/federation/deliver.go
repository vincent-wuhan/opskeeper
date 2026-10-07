package federation

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Getting a decision to a child, which is the half of publishing that is not
// signing.
//
// A publish does two separable things: it issues a version, and it tells a
// cluster about that version. The first is local, durable and cheap. The
// second crosses a network, and almost everything that can go wrong goes
// wrong there. Keeping them apart is not tidiness — it is what lets a failed
// delivery be retried without minting a version, which is the whole reason
// Registry keeps IssuedBundle.
//
// What this file deliberately does not do is serve the bytes. A root that
// signed a tree does not thereby have somewhere to put it, and inventing a
// file server here would put a second, unauthenticated copy of every policy
// tree on the internet next to the one channel that is careful about who may
// say what. So the source is a port: a deployment says how its children
// fetch artifacts, and this package only puts the digest next to the URL.

// ErrNoDeliveryPath means this root has no way to hand a child the tree.
//
// It is the honest answer for a root with no distributor configured, and it
// is deliberately not fatal. The version is issued either way — the ledger
// is a record of what this root decided, not of what arrived — and a push
// with no source comes back retryable from the child, which is a
// well-formed "not yet" rather than a refusal. What must not happen is a
// publish that reports success while having told the child nothing.
var ErrNoDeliveryPath = errors.New("federation: this root has no configured way to deliver a policy tree to a child")

// ErrAlreadyDecided means a delivery was asked for a version the child has
// already reached a decision about, and re-asking is exactly what the
// child's record exists to prevent.
var ErrAlreadyDecided = errors.New("federation: the child already decided this version")

// Distributor turns a staged tree into somewhere a child can fetch it.
//
// It is a port rather than a function because "where a child can fetch it" is
// a deployment fact, not a protocol one. An installation whose children share
// a mount serves file:// URLs; one fronting a CDN or an artifact store serves
// https; one with neither has not configured federation delivery at all and
// says so. Deciding which of those is true is not this package's job, and
// guessing would be how a policy tree ends up published to somewhere it
// should not be.
//
// The contract has one clause that matters: the returned source's digest must
// be the digest of the bytes the URL serves. The child checks it before it
// unpacks anything, so a distributor that returns a URL and a digest which
// disagree turns every delivery into a retryable integrity failure — a
// rollout that never converges and never says why.
type Distributor interface {
	// Distribute publishes the tree at stagedRoot and returns where a child
	// can get it.
	Distribute(ctx context.Context, b federation.Bundle, stagedRoot string) (tunnel.PolicySource, error)
}

// The file distributor is both ports, and the assertions are here rather
// than at the use site so that a change to either interface breaks this
// package's build instead of surfacing as a wiring mistake.
var (
	_ Distributor = (*FileDistributor)(nil)
	_ Redeliverer = (*FileDistributor)(nil)
)

// maxPolicyArchiveBytes caps what this side will pack. It is the same number
// the child enforces, and it is duplicated rather than shared on purpose:
// the two are on opposite sides of a wire with a version skew of zero and a
// deployment skew of anything, and a root that packed more than its children
// accept would produce trees that are correct here and unstageable there.
const maxPolicyArchiveBytes = 4 << 20

// FileDistributor serves trees as file:// URLs out of a shared directory.
//
// It is the zero-configuration path, and it is real rather than a toy: a root
// and its children mounted on the same storage — which is the shape most
// on-prem multi-cluster deployments already have — need no web server at
// all. The directory it writes to is the only thing a deployment has to get
// right, and getting it wrong produces a file:// URL pointing at a path the
// child cannot read, which is a retryable delivery failure rather than a
// policy problem.
type FileDistributor struct {
	// dir is where archives are written, one per cluster and version.
	dir string
	// prefix is the path the *child* sees this directory at, joined with
	// the archive name to build the URL. It exists for the case where the
	// two sides do not spell the path the same way — a container mount, a
	// chroot, an NFS path the operator chose differently.
	//
	// Empty means "the child reads it where this root writes it", which is
	// the same-path case and the one that needs no configuration. It does
	// not mean "a relative path": the URL is resolved by the child, and a
	// relative one would be resolved against whatever directory that
	// process happens to be in.
	prefix string
}

// urlFor is where a child fetches one archive.
func (d *FileDistributor) urlFor(name string) string {
	base := d.prefix
	if base == "" {
		base = filepath.ToSlash(d.dir)
	}
	return "file://" + filepath.ToSlash(filepath.Join(base, name))
}

// NewFileDistributor builds a distributor writing into dir.
func NewFileDistributor(dir, childPrefix string) (*FileDistributor, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("federation: a file distributor needs a directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("federation: resolve the artifact directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("federation: create the artifact directory: %w", err)
	}
	prefix := strings.TrimSuffix(filepath.ToSlash(childPrefix), "/")
	return &FileDistributor{dir: abs, prefix: prefix}, nil
}

// Dir reports where archives are written, for an operator's log line and for
// whatever serves this directory over http when a deployment does that.
func (d *FileDistributor) Dir() string { return d.dir }

// SourceFor answers for a delivery that already happened.
//
// It reads the archive that is on disk rather than packing the tree again,
// and that is the whole point: the digest the child will compare against is
// the digest of the bytes the first delivery wrote, so recomputing it from
// those same bytes is exact, while repacking the tree would produce a
// different archive and a digest that matches nothing.
//
// A missing file is an error rather than a repack. It means the artifact
// directory was cleaned up or is on a volume that did not survive, and the
// honest answer is that this root no longer has the bytes — not a
// best-effort archive that would fail the child's integrity check anyway.
func (d *FileDistributor) SourceFor(b federation.Bundle) (tunnel.PolicySource, error) {
	name := archiveName(b)
	body, err := os.ReadFile(filepath.Join(d.dir, name))
	if err != nil {
		return tunnel.PolicySource{}, fmt.Errorf("%w: the archive for %s v%d is not where it was left: %v",
			ErrNoDeliveryPath, b.ClusterID, b.Version, err)
	}
	sum := sha256.Sum256(body)
	return tunnel.PolicySource{
		URL:           d.urlFor(name),
		ArchiveSHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// archiveName is where a decision's bytes live, derived from the cluster and
// the version and nothing else.
//
// Deriving it from the package would put two clusters on one file as soon as
// they were given the same tree, so the second delivery would silently
// replace the first while the first child was still fetching. Deriving it
// from the staging path would make the same decision occupy a different file
// on every root that published it, and a redelivery would not find it.
func archiveName(b federation.Bundle) string {
	return fmt.Sprintf("%s-v%d.tar.gz", b.ClusterID, b.Version)
}

// Distribute packs the tree and writes it out.
//
// The name is derived from the cluster and the version rather than from the
// package, which is what makes a redelivery of the same version idempotent:
// it rewrites the same file with the same bytes rather than accumulating one
// copy per attempt.
func (d *FileDistributor) Distribute(_ context.Context, b federation.Bundle, stagedRoot string) (tunnel.PolicySource, error) {
	body, err := packPolicyTree(stagedRoot)
	if err != nil {
		return tunnel.PolicySource{}, fmt.Errorf("%w: %v", ErrNoDeliveryPath, err)
	}
	name := archiveName(b)
	dest := filepath.Join(d.dir, name)
	if err := writeFileAtomic(dest, body); err != nil {
		return tunnel.PolicySource{}, fmt.Errorf("%w: %v", ErrNoDeliveryPath, err)
	}
	sum := sha256.Sum256(body)
	return tunnel.PolicySource{
		URL:           d.urlFor(name),
		ArchiveSHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// Delivery is what happened when a decision was put on the wire.
type Delivery struct {
	// Attempted is false when this root has no pusher or no distributor.
	// It is the field an operator's console has to check before showing a
	// rollout as delivered, because a publish that issued a version and
	// told nobody is otherwise indistinguishable from one that worked.
	Attempted bool `json:"attempted"`
	// Verdict is the child's own answer. Its Outcome carries the decision
	// and its Retryable says whether the same bytes are worth sending
	// again; the root does not read either as an error.
	Verdict tunnel.ClusterPolicyResponse `json:"verdict"`
	// Error is a transport or protocol failure. It is never a refusal: a
	// child that declined answers with a verdict and no error, and the
	// difference is why Retryable exists on the wire.
	Error string `json:"error,omitempty"`
	// Delivered is the one-line answer for a human: this version is in
	// force on that cluster, or it is not, or nobody has been told yet.
	Delivered bool `json:"delivered"`
}

// push is the shared body of publish-then-deliver and redeliver.
//
// The order is fixed and it is the point: issue the version, *then* try to
// deliver it. A root that delivered first and issued second would be racing
// a child that has to decide which number it is being asked about, and a
// delivery that lost the race would be about a version that does not exist.
func (s *Service) push(ctx context.Context, id federation.ClusterID, b federation.Bundle, src tunnel.PolicySource) Delivery {
	if s.push_ == nil {
		return Delivery{Error: "this root has no tunnel to its children"}
	}
	resp, err := s.push_.PushPolicy(ctx, id, tunnel.ClusterPolicyRequest{Bundle: b, Source: &src})
	if err != nil {
		// Transport. The child may never have seen the message, and the
		// version it was for is still worth pushing again.
		return Delivery{Attempted: true, Error: err.Error()}
	}
	// A retryable answer is not a decision, so it is not recorded: the
	// ledger would otherwise say a cluster had weighed in on a version
	// whose bytes it never managed to read.
	if !resp.Retryable {
		if err := s.reg.Acknowledge(id, resp.Outcome); err != nil {
			return Delivery{Attempted: true, Verdict: resp, Error: err.Error()}
		}
	}
	return Delivery{Attempted: true, Verdict: resp, Delivered: resp.Outcome.Accepted}
}

// packPolicyTree writes a directory out as a gzipped tar with exactly one
// top-level directory, which is the shape the child unpacks.
//
// The top-level directory is named after the package rather than left as the
// staging directory's own name, so the archive is a function of the tree and
// not of where on this root the operator happened to put it. A redelivery
// from a different path therefore produces the same bytes, and a child that
// already has the tree recognises it.
func packPolicyTree(root string) ([]byte, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	manifest, err := readManifestName(root)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	var written int64
	err = filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// A walk that includes the root itself would emit "./", which
		// unpacks to a second top-level entry and makes the child refuse
		// the archive for having two roots.
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(filepath.Join(manifest, rel))
		if fi.IsDir() {
			hdr.Name += "/"
		}
		// The mode a file happens to have on the root's disk is not
		// something the child's signature covers, so carrying it across
		// would make the same tree unpack differently on two machines and
		// fail a digest comparison for no reason.
		hdr.Mode = 0o640
		if fi.IsDir() {
			hdr.Mode = 0o750
		}
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		if !fi.Mode().IsRegular() {
			// A symlink or device in a tree this root is about to sign
			// is a packaging mistake at best. Failing here means it never
			// reaches a child, rather than reaching one and being refused
			// there as a forgery.
			return fmt.Errorf("%s is not a regular file", rel)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		n, err := io.Copy(tw, io.LimitReader(f, maxPolicyArchiveBytes-written))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		written += n
		if written > maxPolicyArchiveBytes {
			return fmt.Errorf("the tree packs to more than the %d byte cap", int64(maxPolicyArchiveBytes))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// readManifestName pulls the package name out of the tree being packed.
//
// It is read from the manifest rather than from the directory name because
// the directory name is whatever the operator called it and the manifest is
// what was signed. Two archives of the same tree must be the same bytes, and
// deriving the top-level entry from a staging path would make them differ.
func readManifestName(root string) (string, error) {
	body, err := os.ReadFile(filepath.Join(root, manifestFileName))
	if err != nil {
		return "", fmt.Errorf("read the policy manifest: %w", err)
	}
	name := manifestField(string(body), "name")
	if name == "" {
		return "", fmt.Errorf("the policy manifest at %s names no package", root)
	}
	return name, nil
}

// manifestFileName is the file a policy tree's identity is read from. It is
// spelled out rather than imported so this package does not depend on the
// manifest loader to learn a filename.
const manifestFileName = "pig-ops.yaml"

// manifestField reads one top-level scalar out of a manifest without a YAML
// parser.
//
// A parser would be the right tool and is deliberately not used here: this
// runs on a tree that is about to be verified by a real manifest loader on
// the far side, and a second parser with a second idea of what the file
// means is a second thing that can disagree. All this needs is the name for a
// tar entry, and a tree with no name is refused a few lines later anyway.
func manifestField(body, key string) string {
	inMetadata := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			inMetadata = trimmed == "metadata:"
			continue
		}
		if !inMetadata {
			continue
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// writeFileAtomic writes through a temporary in the same directory and
// renames over the target.
//
// The rename is what makes a redelivery safe to interrupt: a child fetching
// this path mid-rewrite gets either the old bytes or the new ones, never a
// half-written archive that hashes to nothing the root promised.
func writeFileAtomic(dest string, body []byte) error {
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, body, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

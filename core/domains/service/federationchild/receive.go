package federationchild

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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The child side of a policy rollout has two halves and only one of them is
// interesting.
//
// The interesting one is Store.Switch, which decides what this cluster
// enforces. This file is the other: getting bytes off a disk the root named
// and into the one directory Switch is allowed to promote from. It is
// deliberately dull. Every decision it makes is a refusal, and every refusal
// is retryable, because a network that did not answer and a URL that 404s
// are conditions that clear on their own — folding them into a decision would
// burn the version they belong to and make a rollout that was merely waiting
// for a file into a permanent refusal.

// maxPolicyArchiveBytes caps a policy tree archive.
//
// A policy tree is tens of kilobytes of manifests and extensions. Four
// megabytes is the same cap the plugin channel uses, deliberately: the two
// carry the same kind of thing, and a limit that differs between them is a
// limit somebody has to remember twice.
const maxPolicyArchiveBytes = 4 << 20

// policyDownloadTimeout bounds one fetch.
//
// Not a context deadline: the push that carried this URL is itself a
// tunnel call, and a fetch that hangs holds the caller's request open. The
// child is answering a push, not hosting a download, so this must be a
// ceiling the child imposes rather than one the root can raise.
const policyDownloadTimeout = 60 * time.Second

var (
	// ErrSourceRefused means the source itself is not one this child will
	// fetch from — a scheme that is not http/https/file, or a URL that
	// does not parse.
	//
	// It is retryable, and that is the interesting part: a retry of the
	// same URL produces the same answer, so a root that keeps sending it
	// will keep being told so. That is a bug on the root, and the honest
	// report is "this is not a thing I can fetch" rather than a policy
	// refusal — a refusal would be recorded against the version and make
	// the root's own ledger say the cluster objected to the policy.
	ErrSourceRefused = errors.New("federation: the policy source is not one this cluster will fetch from")

	// ErrSourceIntegrity means the bytes that arrived are not the bytes
	// that were offered.
	//
	// Also retryable, for the same reason. A corrupted transfer is not a
	// decision about the policy, and a child that recorded it as one would
	// be permanently refusing a version it never actually read.
	ErrSourceIntegrity = errors.New("federation: the policy archive does not match the digest that was offered")
)

// Fetcher is how bytes are pulled. It is a field so tests can drive the
// whole path without a socket, and an interface so a deployment that fronts
// its artifacts with an authenticating proxy can substitute one.
type Fetcher func(ctx context.Context, rawURL string) ([]byte, error)

// Receive fetches a policy tree and lays it out in the staging area,
// returning the directory to hand to Receiver.Apply.
//
// The order is the safety property: fetch, bound the URL, cap the body,
// check the digest, and only then unpack — and unpack into a directory that
// is not yet the version's own, renamed into place at the end. A child that
// crashed mid-unpack leaves a `.incoming` directory nobody will ever promote,
// and a child that unpacked directly into v9 would have a half-written v9
// that the next push of v9 would treat as already staged.
//
// The digest is checked before unpacking rather than after, which is the same
// reason the receiver stats before it verifies: unpacking bytes of unknown
// provenance is the part that costs disk, and the part that has to be
// provable to be the root's before it happens.
func (s *Store) Receive(ctx context.Context, src *tunnel.PolicySource, version uint64) (string, error) {
	if src == nil {
		return "", fmt.Errorf("%w: no source in the push", ErrSourceRefused)
	}
	if err := checkDigestShape(src.ArchiveSHA256); err != nil {
		return "", err
	}
	if err := checkSourceScheme(src.URL); err != nil {
		return "", err
	}
	body, err := s.fetch(ctx, src.URL)
	if err != nil {
		return "", err
	}
	// Bounded after the read as well as during it, for the same reason the
	// plugin channel is: a fetcher that returns a huge body should not
	// have been able to finish.
	if len(body) > maxPolicyArchiveBytes {
		return "", fmt.Errorf("%w: the archive is %d bytes, over the %d byte cap",
			ErrSourceIntegrity, len(body), int64(maxPolicyArchiveBytes))
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != strings.ToLower(src.ArchiveSHA256) {
		return "", fmt.Errorf("%w: got %s, was offered %s", ErrSourceIntegrity, got, src.ArchiveSHA256)
	}

	// The destination is named for the version, not for the package, so a
	// re-push of the same version lands in the same place and the second
	// one is a no-op rather than a second tree. versions/ is the only
	// directory this store will ever promote from, and the path is
	// assembled here rather than taken from the message.
	dest := filepath.Join(s.VersionsDir(), fmt.Sprintf("v%d", version))
	incoming := dest + ".incoming"
	_ = os.RemoveAll(incoming)
	if err := os.MkdirAll(incoming, 0o750); err != nil {
		return "", fmt.Errorf("federation: create the incoming directory: %w", err)
	}
	// Anything left over from a failed attempt at this exact version is
	// removed before unpacking, and it is the only thing RemoveAll is
	// aimed at here: the path is assembled from a uint64 and a constant,
	// so there is no caller-controlled string in it.
	root, err := unpackPolicyArchive(body, incoming)
	if err != nil {
		_ = os.RemoveAll(incoming)
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// If the version is already there and verifies as a directory, the
	// fetch was wasted but the outcome is the same. Removing the
	// destination would make a re-push that is merely retrying delete the
	// tree the cluster may be enforcing right now.
	if _, statErr := os.Stat(dest); statErr == nil {
		_ = os.RemoveAll(incoming)
		return dest, nil
	}
	if err := os.Rename(root, dest); err != nil {
		_ = os.RemoveAll(incoming)
		return "", fmt.Errorf("federation: stage version %d: %w", version, err)
	}
	_ = os.RemoveAll(incoming)
	return dest, nil
}

// checkDigestShape refuses a digest that is not 64 lower hex characters,
// before anything is fetched.
//
// It is checked first so a malformed source costs a round trip of zero, and
// so the error says "that is not a digest" rather than reporting a mismatch
// against a string that could never have been one.
func checkDigestShape(digest string) error {
	d := strings.TrimSpace(digest)
	if len(d) != 2*sha256.Size {
		return fmt.Errorf("%w: sha256 must be %d hex characters, got %d",
			ErrSourceRefused, 2*sha256.Size, len(d))
	}
	for _, c := range d {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("%w: sha256 is not lower hex", ErrSourceRefused)
		}
	}
	return nil
}

// checkSourceScheme is the whole of the URL policy, and it lives in Receive
// rather than in the fetcher.
//
// That placement is load-bearing. Fetcher is replaceable — a deployment
// behind an authenticating proxy supplies its own — and a scheme allowlist
// that lived in the default fetcher would be a rule that a replacement could
// quietly not have. The scheme is a decision about whether this cluster is
// willing to read from that place at all, and the only code that gets to make
// it is the code that owns the decision. The fetcher is told what to fetch,
// not what is safe to fetch.
//
// The three entries are deliberate. http and https are the transport the
// plugin channel already uses. file is there so a policy tree can be moved
// between two clusters sharing a mount without a web server in between — it
// is not a special case downstream either: the same cap, the same digest
// and the same signature apply, and a file URL is a path the child resolves
// itself.
func checkSourceScheme(rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSourceRefused, err)
	}
	switch parsed.Scheme {
	case "http", "https":
		return nil
	case "file":
		// A file URL with a host is somebody else's filesystem, reached
		// by a protocol that has no business reaching it. file://host/path
		// is how a tarball's own documentation spells "this file is on
		// the machine that holds the tarball", and a child that honoured
		// the host would be trusting a name to mean itself.
		if parsed.Host != "" && parsed.Host != "localhost" {
			return fmt.Errorf("%w: file URL host %q is not this machine", ErrSourceRefused, parsed.Host)
		}
		return nil
	case "":
		return fmt.Errorf("%w: the source names no scheme, so it names no place to fetch from", ErrSourceRefused)
	default:
		return fmt.Errorf("%w: scheme %q is not http, https or file", ErrSourceRefused, parsed.Scheme)
	}
}

// defaultFetch pulls one archive over a scheme checkSourceScheme has already
// accepted.
func defaultFetch(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceRefused, err)
	}
	switch parsed.Scheme {
	case "http", "https":
		return httpFetch(ctx, parsed.String())
	case "file":
		return fileFetch(parsed)
	default:
		// Unreachable through Receive, and refused rather than assumed
		// away so a future caller that reaches it directly still gets an
		// answer instead of a nil dereference.
		return nil, fmt.Errorf("%w: scheme %q is not http, https or file",
			ErrSourceRefused, parsed.Scheme)
	}
}

func httpFetch(ctx context.Context, rawURL string) ([]byte, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, policyDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceRefused, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("federation: fetch the policy archive: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("federation: the policy source answered %s", resp.Status)
	}
	// One byte past the cap, so an oversized body is detectable rather
	// than silently truncated into something that might still hash right
	// by accident.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPolicyArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("federation: read the policy archive: %w", err)
	}
	return body, nil
}

func fileFetch(parsed *url.URL) ([]byte, error) {
	path := parsed.Path
	if path == "" {
		return nil, fmt.Errorf("%w: file URL has no path", ErrSourceRefused)
	}
	// Bounded by the same cap as the http path, and by a stat rather than
	// a read-and-hope: a 4 GB file should be refused before it is read.
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("federation: the policy source is not readable: %w", err)
	}
	if info.Size() > maxPolicyArchiveBytes {
		return nil, fmt.Errorf("%w: the archive is %d bytes, over the %d byte cap",
			ErrSourceIntegrity, info.Size(), int64(maxPolicyArchiveBytes))
	}
	body, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("federation: read the policy archive: %w", err)
	}
	return body, nil
}

// unpackPolicyArchive lays a gzipped tar out under dst and returns the single
// package root inside it.
//
// The path checks are not against a hostile root — a hostile root is caught
// by the signature two steps later, on the far side of Apply. They are
// against a malformed or hostile *archive*, which can arrive from anywhere
// that can put bytes behind the URL, and a child that writes outside its
// staging area on the way to discovering that has already lost the property
// the rest of this file is about.
//
// Refusing symlinks rather than sanitising them is the same call the plugin
// channel makes and for the same reason: a symlink in a policy tree is never
// needed, and it is the classic way to make a later write land somewhere
// else.
func unpackPolicyArchive(body []byte, dst string) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("federation: the policy source is not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var written int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("federation: read the policy archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return "", fmt.Errorf("federation: archive entry %q has type %v; a policy tree holds files and directories only",
				hdr.Name, hdr.Typeflag)
		}
		clean, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return "", err
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(clean, 0o750); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(clean), 0o750); err != nil {
			return "", err
		}
		f, err := os.OpenFile(clean, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			return "", fmt.Errorf("federation: create %s: %w", hdr.Name, err)
		}
		// Bounded per-entry against what is left of the total cap, so a
		// zip bomb is stopped by the total and not only by the length of
		// the one entry that happens to be reading.
		n, err := io.Copy(f, io.LimitReader(tr, maxPolicyArchiveBytes-written))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", fmt.Errorf("federation: write %s: %w", hdr.Name, err)
		}
		written += n
		if written > maxPolicyArchiveBytes {
			return "", fmt.Errorf("%w: the archive extracts to more than the %d byte cap",
				ErrSourceIntegrity, int64(maxPolicyArchiveBytes))
		}
	}
	return singleRootWithin(dst)
}

// singleRootWithin finds the one package directory inside a staging directory.
//
// A tree with two top-level directories, or none, is not a policy. Guessing
// which entry was meant is how a tree gets verified in one directory and
// promoted from another, and the check that would catch it — VerifyDir
// against the promoted path — would then be verifying a directory the
// signature was never computed over.
func singleRootWithin(stage string) (string, error) {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return "", fmt.Errorf("federation: read the unpacked policy: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(stage, e.Name()))
		}
	}
	if len(dirs) != 1 {
		names := make([]string, 0, len(dirs))
		for _, d := range dirs {
			names = append(names, filepath.Base(d))
		}
		return "", fmt.Errorf("%w: the archive holds %d top-level directories (%s); a policy tree is exactly one",
			ErrSourceRefused, len(dirs), strings.Join(names, ", "))
	}
	return dirs[0], nil
}

// safeJoin refuses a path that leaves its root.
//
// A ".." segment is refused rather than clamped. Clamping would keep the
// child safe and lose the evidence: the operator would be told the policy
// arrived, and the archive that tried to write outside the staging area
// would be gone. A refusal names what the archive tried to do, and that name
// is the only thing pointing at wherever those bytes came from.
func safeJoin(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: an archive entry has no name", ErrSourceRefused)
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: archive entry %q is an absolute path", ErrSourceRefused, name)
	}
	for _, seg := range strings.Split(filepath.ToSlash(name), "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: archive entry %q escapes the staging directory", ErrSourceRefused, name)
		}
	}
	joined := filepath.Join(root, filepath.Clean("/"+name))
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: archive entry %q escapes the staging directory", ErrSourceRefused, name)
	}
	return joined, nil
}

package federationchild

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
)

// LiveLinkName is the symlink a Store keeps pointing at the policy tree in
// force. Everything that reads policy reads through it, so swapping it is
// what "applying a policy" means.
const LiveLinkName = "live"

// versionsDirName holds every staged tree. A tree is not deleted when it goes
// out of force, which is what makes a rollback a rename rather than a
// download: the previous policy is still on disk, addressed by its own name.
const versionsDirName = "versions"

// ErrPathOutsideStaging means a staged path pointed somewhere other than this
// store's own versions directory. It satisfies federation.ErrPromotionRefused,
// so the receiver records it as a decision rather than as a machine that is
// temporarily unable to swap.
//
// This check is the reason this type exists in this form. The root sends a
// path in tunnel.ClusterPolicyRequest.StagedPath, and a path the root can name
// is a path the root can write: without this, a policy push carrying
// /etc/whatever would be a remote file-write primitive wearing a policy's
// clothes, and the signature would be checking a tree the root had already
// chosen.
var ErrPathOutsideStaging = errors.New("federation: staged path is outside this cluster's staging area")

// Store is the filesystem half of a child cluster's policy state: a directory
// of staged trees and one symlink naming the one in force.
//
// The two properties it exists to guarantee are both about what a reader sees
// at an arbitrary instant:
//
//   - There is always a live tree, or there is knowingly none. A swap cannot
//     fail halfway, because the only step that changes what a reader resolves
//     is a rename, and a rename is atomic on POSIX.
//   - A reader that has resolved the symlink keeps reading the tree it
//     resolved. Nobody follows it into a policy that arrived mid-read.
//
// The first is why Switch has no rollback: there is no state between the two
// that needs undoing. The second is why it is a symlink and not a file
// containing a path.
type Store struct {
	base string

	// fetch is how a policy archive is pulled when a push names a source
	// instead of a path already on this machine.
	//
	// It is a field rather than a direct call so that a deployment behind
	// an authenticating proxy can supply its own transport, and so the
	// tests can drive the whole fetch-verify-unpack path without a socket.
	// NewStore always installs a working default; it is never nil, because
	// a store that could be built without a way to receive a tree would
	// answer every sourced push with a nil dereference.
	fetch Fetcher

	// mu serialises swaps, not reads. Two concurrent Switches would
	// otherwise both build a temporary link and race to rename it, and
	// whichever lost would have left a .next.<pid> file behind. It is not
	// here to protect lastPath, because there is no lastPath: the
	// symlink on disk is the state, and a cache of it would be a value
	// that goes stale the moment another process on this host swaps.
	mu sync.RWMutex
}

// NewStore prepares a store rooted at base, creating the versions directory.
//
// The symlink is deliberately not created if it is missing. "No policy yet" is
// a real and correct state for a cluster that has just been enrolled, and
// inventing an empty one would mean the cluster reports itself as enforcing
// something it has never been told.
func NewStore(base string) (*Store, error) {
	if strings.TrimSpace(base) == "" {
		return nil, errors.New("federation: a policy store needs a base directory")
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("federation: resolve the policy base: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(abs, versionsDirName), 0o750); err != nil {
		return nil, fmt.Errorf("federation: create the staging area: %w", err)
	}
	return &Store{base: abs, fetch: defaultFetch}, nil
}

// Base reports the store's root.
func (s *Store) Base() string { return s.base }

// VersionsDir is where staged trees are expected to live.
func (s *Store) VersionsDir() string { return filepath.Join(s.base, versionsDirName) }

// Live reports the tree currently in force, empty when none is.
//
// It reads the symlink rather than a cached value on purpose: a cached answer
// would be a lie the moment another process on this host swapped the policy,
// and the whole claim of this store is that the file on disk is the truth.
func (s *Store) Live() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	target, err := os.Readlink(filepath.Join(s.base, LiveLinkName))
	if err != nil {
		// No link, or an unreadable one. Both mean the same thing to a
		// reader: this cluster is not enforcing a policy, and the
		// caller needs to find that out from a failed read rather than
		// from a stale string.
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(s.base, target)
	}
	return filepath.Clean(target)
}

// Switch makes a staged tree the one in force.
//
// The sequence is three steps and the third is the only one a reader can
// observe: build a temporary symlink beside the real one, then rename it over
// the top. Every failure before the rename leaves the previous policy live;
// there is no failure after it.
func (s *Store) Switch(staged string) (string, error) {
	abs, err := s.bound(staged)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("federation: staged policy %s: %w", abs, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	link := filepath.Join(s.base, LiveLinkName)
	// The temporary name is inside the same directory as the link it will
	// replace, because rename is only atomic within a filesystem — and
	// crossing filesystems here would turn an atomic swap into a
	// copy-then-rename that a crash can interrupt.
	tmp := filepath.Join(s.base, fmt.Sprintf("%s.next.%d", LiveLinkName, os.Getpid()))
	_ = os.Remove(tmp)
	if err := os.Symlink(abs, tmp); err != nil {
		return "", fmt.Errorf("federation: stage the live link: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		// The old link is still exactly where it was; only the
		// temporary one has to go.
		_ = os.Remove(tmp)
		return "", fmt.Errorf("federation: swap the live link: %w", err)
	}
	return abs, nil
}

// bound resolves a staged path and refuses anything outside this store.
//
// Both halves matter and they catch different attacks. The prefix check stops
// a root from naming a path anywhere on the disk. The EvalSymlinks check stops
// it from naming a path *inside* the staging area that is a symlink pointing
// out of it, which the prefix check alone would wave through — and that is not
// a contrived case, because whoever can write into the staging area is exactly
// the party a second check is worth having against.
func (s *Store) bound(staged string) (string, error) {
	if strings.TrimSpace(staged) == "" {
		return "", errors.New("federation: no staged path in the push")
	}
	abs, err := filepath.Abs(staged)
	if err != nil {
		return "", fmt.Errorf("federation: resolve %q: %w", staged, err)
	}
	abs = filepath.Clean(abs)

	versions := s.VersionsDir()
	if abs != versions && !strings.HasPrefix(abs, versions+string(filepath.Separator)) {
		return "", promotionRefused("%s is not under %s", abs, versions)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// An unresolvable path is refused rather than assumed safe. The
		// common case is a tree that has not arrived yet, and the
		// receiver turns this into a retryable "not staged".
		return "", fmt.Errorf("federation: staged policy %s: %w", abs, err)
	}
	realVersions, err := filepath.EvalSymlinks(versions)
	if err != nil {
		return "", fmt.Errorf("federation: resolve the staging area: %w", err)
	}
	if resolved != realVersions && !strings.HasPrefix(resolved, realVersions+string(filepath.Separator)) {
		return "", promotionRefused("%s resolves to %s", abs, resolved)
	}
	return resolved, nil
}

// Store satisfies federation.Switcher. The assertion is here rather than only
// at the use site so that a change to that interface breaks this package's
// build instead of surfacing as a wiring mistake in whatever assembles them.
var _ federation.Switcher = (*Store)(nil)

// promotionRefused builds the error the receiver classifies as a decision.
//
// It carries both sentinels on purpose: ErrPathOutsideStaging is what a
// caller inspecting this package wants to match on, and ErrPromotionRefused
// is what the receiver above this one matches on. Wrapping one in the other
// rather than returning a single name is what lets both questions be asked
// without either caller having to know about the other's vocabulary.
func promotionRefused(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s",
		federation.ErrPromotionRefused, ErrPathOutsideStaging,
		fmt.Sprintf(format, args...))
}

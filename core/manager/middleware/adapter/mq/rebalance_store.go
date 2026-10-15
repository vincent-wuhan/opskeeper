package mq

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Persistence for the rebalance history.
//
// The first version kept the history in memory and said so, which is honest
// and is also a poor property for an operations tool: a control plane that
// restarts during an incident loses exactly the window the incident review
// asks about, and the tool then reports a group that "has never been
// sampled" — which reads as "this group never rebalanced" to anyone who does
// not read the source.
//
// The file format is one JSON document per group, rewritten whole on every
// change. Whole-file rather than an append log because the store is bounded
// (64 events, 24 hours) and rewriting keeps the on-disk shape identical to
// the in-memory one — no compaction step, no second format to reason about
// at read time. The write is atomic (temp file + rename) because a control
// plane that is killed mid-write must not leave a half document that reads
// back as an empty history.
//
// What persistence deliberately does NOT do is survive a change of the event
// shape: there is no version field in the document. A history written by an
// older build that fails to parse is reported as unreadable rather than
// silently emptied, because "I could not read the old window" and "there was
// no old window" are different sentences.

// rebalanceStore is the persistence seam.
//
// The in-memory history is the same type with a nil store, so the two paths
// cannot drift: one implementation of trimming, querying and diffing, with
// storage as the only difference.
type rebalanceStore interface {
	load(group string) ([]RebalanceEvent, GroupState, error)
	save(group string, events []RebalanceEvent, latest GroupState) error
}

// persistedHistory is the on-disk document for one group.
type persistedHistory struct {
	Group  string           `json:"group"`
	Latest GroupState       `json:"latest"`
	Events []RebalanceEvent `json:"events"`
}

// fileHistory keeps each group in its own file under one directory.
type fileHistory struct {
	dir string
	mu  sync.Mutex
}

func newFileHistory(dir string) (*fileHistory, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("mq: create the rebalance history directory %s: %w", dir, err)
	}
	return &fileHistory{dir: dir}, nil
}

// path maps a group name onto a file.
//
// The name is sanitized rather than trusted: a group is broker-supplied, so
// one containing a slash would otherwise write outside the directory the
// operator named. Sanitizing keeps every file under the root, at the cost of
// a name collision between two groups that sanitize the same — which is why
// the group name is also stored inside the document and checked on load.
func (f *fileHistory) path(group string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, group)
	if clean == "" {
		clean = "unnamed"
	}
	return filepath.Join(f.dir, clean+".json")
}

func (f *fileHistory) load(group string) ([]RebalanceEvent, GroupState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := os.ReadFile(f.path(group))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, GroupState{}, nil
		}
		return nil, GroupState{}, err
	}
	var doc persistedHistory
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, GroupState{}, fmt.Errorf("mq: the rebalance history for group %s is unreadable (%v); "+
			"it is left in place rather than emptied, because an unreadable old window and an empty one "+
			"are different answers", group, err)
	}
	if doc.Group != group {
		return nil, GroupState{}, fmt.Errorf("mq: %s holds the history of group %q, not %q; "+
			"two group names sanitized onto one filename", f.path(group), doc.Group, group)
	}
	return doc.Events, doc.Latest, nil
}

func (f *fileHistory) save(group string, events []RebalanceEvent, latest GroupState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, err := json.Marshal(persistedHistory{Group: group, Latest: latest, Events: events})
	if err != nil {
		return err
	}
	path := f.path(group)
	tmp, err := os.CreateTemp(f.dir, ".rebalance-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("mq: replace the rebalance history for group %s: %w", group, err)
	}
	return nil
}

// NewRebalanceHistoryStore returns a history backed by dir.
//
// This is the constructor an operator's deployment names; NewRebalanceHistory
// stays the in-memory one, and the difference is the only thing between them.
// An unreadable directory is an error at construction rather than a silent
// fallback to memory: a history that quietly forgets everything on a
// permission problem is the failure this file exists to prevent.
func NewRebalanceHistoryStore(dir string, maxEvents int, maxAge time.Duration) (*RebalanceHistory, error) {
	store, err := newFileHistory(dir)
	if err != nil {
		return nil, err
	}
	h := NewRebalanceHistory(maxEvents, maxAge)
	h.store = store
	return h, nil
}

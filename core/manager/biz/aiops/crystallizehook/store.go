package crystallizehook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/crystallize"
)

// FileStore keeps the ledger's runs in one JSON file.
//
// The ledger is the only object in the crystalliser that has to survive a
// restart, and it is the only one that cannot: every other thing the
// crystalliser produces is a draft package written out for a human to review,
// and a draft on disk outlives the process that made it. The counters behind
// those drafts do not. A deployment that has verified the same fix on the
// first try for two of the three runs it needs loses the first two on every
// restart, which means the third run starts the streak over — and a rule that
// re-arms itself every time the control plane rolls is not a cost saving, it
// is a cost saving nobody can ever collect.
//
// The durability pattern is core/domains/biz/federation's FileLedger, and for
// the same reason: write a temporary file, fsync it, rename over the target,
// then fsync the directory. A process killed halfway through leaves either
// the whole previous ledger or the whole new one, never a half-written file
// that parses as an empty ledger and silently restarts every streak at zero.
// The failure mode of truncating here is indistinguishable from a fresh
// install, and it would be indistinguishable forever.
//
// The file is 0600. It carries every argv the ledger ever learned from — SQL
// statements, API calls, the literal commands a fix dispatched through — and
// that is more sensitive than the federation ledger's token verifier, not
// less.
type FileStore struct {
	path string
	mu   sync.Mutex
}

// storeDoc is the on-disk shape.
//
// Version is written by every build that can read this file and checked on
// the way in, because the alternative to a version field on a file you intend
// to change the shape of is a version field discovered during an incident.
// A document whose version this build does not know is refused rather than
// best-effort parsed: silently dropping a field Restore reads would look
// exactly like a streak that was never earned.
type storeDoc struct {
	Version int               `json:"version"`
	SavedAt time.Time         `json:"savedAt"`
	Runs    []crystallize.Run `json:"runs"`
}

// storeVersion is the only layout this build reads or writes.
const storeVersion = 1

// NewFileStore builds a store at path.
//
// The file is not created here. A store that wrote an empty document on
// construction would be indistinguishable from one that had restored a ledger
// with no runs, and the first is a bug while the second is a fact.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// Path is where the store keeps its file, so a caller can log the location at
// boot: "why did my promotion progress reset" is a question an operator can
// only answer if they know which file to look at, and the default answer —
// the one that loses the data — is always "somewhere else".
func (s *FileStore) Path() string { return s.path }

// Probe reports whether the path could be used at all, without writing
// anything an operator would later read as state.
//
// It exists because the alternative is found at the worst possible moment: a
// control plane that restored nothing, counted toward nothing, and reported no
// error until someone asked why a runbook never appeared. The check writes
// and removes a real file rather than stat-ing the directory, because the
// common failure is a path that exists and is not writable, and a permission
// bit on the directory is not always what decides it.
func (s *FileStore) Probe() error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("crystallize: state directory %s is not usable: %w", directory, err)
	}
	temp, err := os.CreateTemp(directory, ".crystallize-probe-*.tmp")
	if err != nil {
		return fmt.Errorf("crystallize: state directory %s is not writable: %w", directory, err)
	}
	name := temp.Name()
	temp.Close()
	os.Remove(name)
	return nil
}

// Load returns the persisted runs, or none when there is nothing persisted.
//
// A missing file is not an error and not a warning: it is what a deployment
// that has never recorded a trial looks like, and it is the state every
// first boot passes through. A file that exists but cannot be read IS an
// error, because the operator's answer to "the streaks reset" has to be able
// to distinguish "there was nothing to restore" from "the restore failed" —
// and silently returning none would answer both the same way.
func (s *FileStore) Load() ([]crystallize.Run, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("crystallize: read state %s: %w", s.path, err)
	}
	var doc storeDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("crystallize: state %s is not readable: %w", s.path, err)
	}
	if doc.Version != storeVersion {
		return nil, fmt.Errorf("crystallize: state %s is version %d and this build reads version %d: it will not guess at the difference, because a field it skipped is a decision it would drop", s.path, doc.Version, storeVersion)
	}
	return doc.Runs, nil
}

// Save writes runs atomically, replacing whatever was there.
//
// Whole-document rather than incremental because the document is bounded by
// the number of distinct patterns the deployment has ever fixed, not by the
// number of trials: a pattern that has been seen is one row forever, and a
// partial update is the shape of bug this file exists to avoid.
func (s *FileStore) Save(runs []crystallize.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := json.MarshalIndent(storeDoc{
		Version: storeVersion,
		SavedAt: time.Now().UTC(),
		Runs:    runs,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("crystallize: encode state: %w", err)
	}
	raw = append(raw, '\n')

	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("crystallize: create state directory %s: %w", directory, err)
	}
	temp, err := os.CreateTemp(directory, ".crystallize-state-*.tmp")
	if err != nil {
		return fmt.Errorf("crystallize: create temp state: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("crystallize: chmod temp state: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("crystallize: write temp state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("crystallize: sync temp state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("crystallize: close temp state: %w", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("crystallize: replace state %s: %w", s.path, err)
	}
	// The rename is the durable event; without syncing the directory the file
	// can be gone after a power cut even though every byte was safely written.
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("crystallize: open state directory for sync: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("crystallize: sync state directory: %w", err)
	}
	return nil
}

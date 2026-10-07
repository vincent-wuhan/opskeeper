package federation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
)

// FileLedger keeps the registry in one JSON file.
//
// It is the shipping implementation of Ledger, and it is deliberately the
// boring one. The port's own comment names the two interesting
// implementations — Postgres for a single root, and the root's audit chain —
// and neither ships. What ships is the one that is correct for the topology
// that is actually supported: a single root process, whose whole membership
// is a handful of rows. For that shape a file is not a compromise, it is the
// store you would choose anyway: it needs no migration, no connection
// string, and no second system to be up at the exact moment the root is
// restarting — which is the only moment this file is read.
//
// The durability is the same pattern core/manager/biz/agentteams/state uses:
// write a temporary file, fsync it, rename over the target, then fsync the
// directory so the rename itself survives power loss. A root killed halfway
// through leaves either the old ledger or the new one, never a truncated
// file that parses as an empty membership and silently re-enrols everyone.

// FileLedger is a Ledger backed by one file.
type FileLedger struct {
	path string
	mu   sync.Mutex
}

// NewFileLedger builds a ledger at path.
//
// The file is not created here. A ledger that wrote an empty file on
// construction would be indistinguishable from one that had restored a root
// with no clusters, and the first is a mistake while the second is a fact.
func NewFileLedger(path string) *FileLedger {
	return &FileLedger{path: path}
}

// Probe reports whether this ledger could be used at all, without writing
// anything an operator would later mistake for state.
//
// It exists because the alternative is discovered at the worst possible
// moment. A root whose ledger path is unwritable — a read-only image, an
// unprivileged user, a missing volume — would otherwise enrol its first
// cluster, get an error nobody was watching for, and be left with a
// federation channel that cannot register anything at all. That is strictly
// worse than the problem the ledger fixes, and it is strictly better to find
// it at boot with one line in the log than at 3am during a rollout.
//
// The check writes and removes a real file rather than only stat-ing the
// directory, because the common failure is a path that exists and is not
// writable, and a permission bit on the directory is not always the thing
// that decides it.
func (l *FileLedger) Probe() error {
	directory := filepath.Dir(l.path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("federation: ledger directory %s is not usable: %w", directory, err)
	}
	temp, err := os.CreateTemp(directory, ".federation-probe-*.tmp")
	if err != nil {
		return fmt.Errorf("federation: ledger directory %s is not writable: %w", directory, err)
	}
	name := temp.Name()
	temp.Close()
	os.Remove(name)
	return nil
}

// Path is where this ledger keeps its file. It exists so a caller can log
// the location at boot: "the root forgot my cluster" is a question an
// operator can only answer if they know which file to look at.
func (l *FileLedger) Path() string { return l.path }

type ledgerFile struct {
	Members []Member `json:"members"`
}

// LoadMembers reads every enrolled cluster back.
func (l *FileLedger) LoadMembers() ([]Member, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	raw, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			// A root that has never enrolled anything has no file yet.
			// That is an empty membership, not a failure.
			return nil, nil
		}
		return nil, fmt.Errorf("federation: read ledger %s: %w", l.path, err)
	}
	var doc ledgerFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Refused rather than treated as empty. Reading a damaged
		// ledger as "no members" would re-enrol every cluster and
		// rotate every token, which is the worst response to a
		// recoverable problem.
		return nil, fmt.Errorf("federation: ledger %s is not readable: %w", l.path, err)
	}
	return doc.Members, nil
}

// SaveMember durably records a member.
func (l *FileLedger) SaveMember(m Member) error {
	return l.rewrite(func(doc *ledgerFile) {
		for i := range doc.Members {
			if doc.Members[i].Cluster.ID == m.Cluster.ID {
				doc.Members[i] = m
				return
			}
		}
		doc.Members = append(doc.Members, m)
	})
}

// SaveHighestIssued durably records the newest version handed to a cluster.
func (l *FileLedger) SaveHighestIssued(id federation.ClusterID, version uint64) error {
	return l.rewrite(func(doc *ledgerFile) {
		for i := range doc.Members {
			if doc.Members[i].Cluster.ID != id {
				continue
			}
			// Never move backwards. A replayed or out-of-order write
			// must not un-record a version, because the whole point of
			// the column is that a root never reissues one.
			if version > doc.Members[i].HighestIssued {
				doc.Members[i].HighestIssued = version
			}
			return
		}
	})
}

// rewrite applies a change and writes the whole file atomically.
//
// Whole-file rather than incremental because the document is a handful of
// rows, and because a partial update is the shape of bug this store exists
// to avoid.
func (l *FileLedger) rewrite(apply func(*ledgerFile)) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	doc, err := l.readLocked()
	if err != nil {
		return err
	}
	apply(&doc)
	return l.writeLocked(doc)
}

func (l *FileLedger) readLocked() (ledgerFile, error) {
	raw, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ledgerFile{}, nil
		}
		return ledgerFile{}, fmt.Errorf("federation: read ledger %s: %w", l.path, err)
	}
	var doc ledgerFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ledgerFile{}, fmt.Errorf("federation: ledger %s is not readable: %w", l.path, err)
	}
	return doc, nil
}

func (l *FileLedger) writeLocked(doc ledgerFile) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("federation: encode ledger: %w", err)
	}
	raw = append(raw, '\n')

	directory := filepath.Dir(l.path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("federation: create ledger directory %s: %w", directory, err)
	}
	temp, err := os.CreateTemp(directory, ".federation-ledger-*.tmp")
	if err != nil {
		return fmt.Errorf("federation: create temp ledger: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	// 0600, not the 0644 the agentteams state file uses. This one holds a
	// credential verifier: the SHA-256 of a provisioning token. Thirty-two
	// bytes of entropy cannot be searched, but the file also enumerates
	// every cluster this root governs, and neither fact is any of a
	// local user's business.
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("federation: chmod temp ledger: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("federation: write temp ledger: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("federation: sync temp ledger: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("federation: close temp ledger: %w", err)
	}
	if err := os.Rename(tempName, l.path); err != nil {
		return fmt.Errorf("federation: replace ledger %s: %w", l.path, err)
	}
	// The rename is the durable event; without syncing the directory the
	// file can be gone after a power cut even though every byte of it was
	// safely written.
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("federation: open ledger directory for sync: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("federation: sync ledger directory: %w", err)
	}
	return nil
}

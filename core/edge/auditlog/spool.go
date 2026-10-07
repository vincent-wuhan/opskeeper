// Package auditlog is the node's own ledger: where a tool call, a plugin
// install or an agent turn is written down on the machine that did it, and
// how those rows reach the central chain.
//
// It exists because the plumbing on both ends was already correct and the
// last hop was missing. core/ports declared an AuditSink and a closed
// vocabulary of actions for the node plane; policygate and the PiG agent
// were written to write through it; a real implementation of the port exists
// on the control plane. But the chain lives in the manager's database on
// another machine, so a node had no way to put a row in it — and the edge,
// meeting that, deliberately wired **no** sink at all rather than a dead one.
// Every tool call a node made was therefore invisible to the chain, and the
// six action constants nothing referenced were the visible symptom of a
// missing wire rather than of unused features.
//
// Three properties are the reason this is a file on the node and not a
// channel to the center, and all three are the same three core/edge/autonomy
// already had to solve — which is why both use the same spool primitive
// rather than a second implementation of "append a line, cap it, replay it
// in order":
//
//   - The row must exist before the action runs. A tool call whose audit
//     only appears on the success path records exactly the calls that did
//     not succeed, which are the ones worth reading.
//   - It must survive a power cut, and it must not be readable by anyone
//     but the node's user. The shared spool already writes 0600, discards a
//     half-written line on read rather than failing the replay, and refuses
//     a spool that is already too open rather than quietly widening it.
//   - It must not grow without bound. A node off the network for a week
//     should not fill its disk with the evidence of that week.
//
// What this package deliberately does NOT do is verify anything. The chain is
// the manager's; a node holds rows, and a node that could verify a chain
// would be a node holding the key that signs one.
package auditlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/edge/spool"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

const (
	// DefaultBytes is how large the node's ledger may grow before the
	// oldest rows are dropped. It is deliberately larger than the autonomy
	// spool's: autonomy rows are decisions about a handful of declared
	// self-heals, while a tool-call row is written on every call, and a
	// node running an agent for a week produces a great many more of them.
	DefaultBytes = 16 << 20
	// ClassLedger is the only bucket this spool writes to. One class, so
	// "which rows may go first" has the answer it has always had — none,
	// until the cap says otherwise, and then the oldest.
	ClassLedger = spool.Class("audit_ledger")
)

// ErrNoChainHere is what Verify answers on a node.
//
// It is an error rather than a nil because the honest answer is "this
// process cannot tell you", and a nil would be read as "verified" by any
// caller that checks the error and logs the result. A node has rows; the
// chain is one hop away and belongs to somebody else. Nothing calls Verify
// in production today, which is why this is a sentinel rather than a
// second verification path nobody would trust.
var ErrNoChainHere = errors.New("auditlog: a node holds rows, not a chain — verification belongs to the control plane")

// Sink is the node's ports.AuditSink: rows go to a local file, and the
// center takes them when the tunnel comes back.
type Sink struct{ *spool.Spool }

// Open opens or creates the node's ledger at path.
//
// The permission and symlink rules are the shared spool's, and they are not
// negotiable here for the same reason autonomy's are not: this file is a
// list of things an AI did to a host, and a world-readable one of those is
// an incident report for somebody else to read.
func Open(path string, maxBytes int64) (*Sink, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("auditlog: the ledger needs a path")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultBytes
	}
	inner, err := spool.Open(spool.Options{
		Path:     path,
		MaxBytes: maxBytes,
		Label:    "the node audit ledger",
		Classes: map[spool.Class]spool.Policy{
			// No MaxAge. "Is it too late to record that this node did this"
			// has no useful answer, and an age horizon on an audit class
			// deletes the record of an incident because the incident
			// lasted longer than somebody's retention policy.
			ClassLedger: {DropPriority: spool.PriorityCritical},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Sink{Spool: inner}, nil
}

// Record appends one row. It satisfies ports.AuditSink.
//
// It never refuses the caller. A full disk, a permission problem, a spool
// that is already too open — none of them is a reason to let a tool call
// through unrecorded *and* unrun, because the caller's choice between
// "record it" and "do it" has no third option that is better, and the one
// it usually picks (do it) is the one that loses the evidence. The error is
// returned so the caller can log it, and the row is dropped, and the ledger
// says so on its own health line.
func (s *Sink) Record(_ context.Context, entry ports.AuditEntry) error {
	if s == nil || s.Spool == nil {
		return errors.New("auditlog: record on an unopened ledger")
	}
	return s.Spool.Record(context.Background(), ClassLedger, entry)
}

// Verify implements ports.AuditSink. It answers ErrNoChainHere; see its doc.
func (s *Sink) Verify(_ context.Context) error { return ErrNoChainHere }

// Peek returns up to n of the oldest rows without removing them.
//
// It is what the pump reads. A drain that delivered rows and then failed to
// ack them has to be able to look again, and a pump that read the file by
// removing from it could not be retried.
func (s *Sink) Peek(n int) ([]ports.AuditEntry, error) {
	if s == nil || s.Spool == nil {
		return nil, errors.New("auditlog: peek on an unopened ledger")
	}
	raw, err := s.Spool.Peek(n)
	if err != nil {
		return nil, err
	}
	return decode(raw)
}

// Replay hands every stored row to fn, oldest first, and reports how many
// were delivered.
//
// A line that does not parse is skipped rather than treated as an error: it
// is almost certainly the tail of a write a power cut interrupted, and a
// ledger that cannot be read because of its own last row is a ledger that
// will never be replayed.
func (s *Sink) Replay(fn func(ports.AuditEntry) error) (int, error) {
	if s == nil || s.Spool == nil {
		return 0, errors.New("auditlog: replay on an unopened ledger")
	}
	return s.Spool.Replay(func(raw spool.Row) error {
		entry, err := decodeRow(raw)
		if err != nil {
			return err
		}
		return fn(entry)
	})
}

// decode converts the shared envelope back into a port entry.
//
// The envelope is deliberately not merged into the entry: the write time on
// the envelope is what the drop policy ages against, and the entry's own At
// is what an operator reads.
func decode(raw []spool.Row) ([]ports.AuditEntry, error) {
	out := make([]ports.AuditEntry, 0, len(raw))
	for _, r := range raw {
		entry, err := decodeRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

func decodeRow(raw spool.Row) (ports.AuditEntry, error) {
	var entry ports.AuditEntry
	if err := json.Unmarshal(raw.Payload, &entry); err != nil {
		return ports.AuditEntry{}, fmt.Errorf("auditlog: decode a ledger row: %w", err)
	}
	return entry, nil
}

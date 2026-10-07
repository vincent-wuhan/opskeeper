package autonomy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/edge/spool"
)

// The node's local record of the decisions it made on its own.
//
// The plan's line is "每次自治执行写本地审计 spool；隧道恢复后回传，补写中心审计链",
// and the interesting half is the failure modes rather than the mechanism.
// Three of them are the reason this is a file on the node and not a channel
// to the center:
//
//   - The row must exist **before** the action runs. A tunnel call that
//     fails is exactly the case the spool exists for, so a sink that only
//     receives rows on the success path records nothing at all.
//   - The row must survive a power cut. So the format is one JSON object
//     per line, a half-written line is discarded on read rather than
//     failing the replay, and the file is opened 0600 — a node's autonomy
//     decisions are a list of things it did to itself without asking, and
//     a world-readable one of those is an incident report for someone else
//     to read.
//   - The file must not grow without bound. A node that is off the network
//     for a week should not fill its own disk with the evidence of that
//     week, and the evidence is least useful oldest: once the newest rows
//     are gone, the outage is over and nobody is reading the log.
//
// None of that is implemented here. It lives in core/edge/spool, which
// exists because this node has three spools — this one, telemetry, and the
// change watcher's batches — and three implementations of "append a line,
// cap it, replay it in order" means three answers to "what happens to a
// half-written line" and "which end gets dropped". They would not have
// stayed the same, and the one that was wrong would have been whichever
// nobody was looking at.
//
// What *is* here is the one thing that differs: this spool's rows are audit
// records, so it is declared as the most valuable class and is given no
// age horizon. Everything else about it — permissions, cap, drop order,
// floor, crash tolerance — is the shared policy, and the forty-odd tests
// that used to live beside this file are the evidence that moving it
// changed nothing an operator could observe.
const (
	// DefaultSpoolBytes is how large the spool may grow before the oldest
	// rows are dropped. It is sized for a week of offline self-healing at
	// a rate no sane node reaches, so in practice the cap is a guard
	// against a run loop rather than a policy.
	DefaultSpoolBytes = spool.DefaultMaxBytes
	// ClassAudit is the only bucket this spool writes to. One class, so
	// that "which rows may go first" has the answer it has always had —
	// none, until the cap says otherwise, and then the oldest.
	ClassAudit = spool.Class("audit")
)

// Spool is a bounded, owner-only audit file, typed.
type Spool struct{ *spool.Spool }

// OpenSpool opens or creates the autonomy audit spool at path.
//
// The file is created 0600 and the directory 0700, and an existing file
// that is group- or world-readable is refused rather than quietly widened:
// a spool that was already too open is evidence that something else on the
// host is too open, and the arbiter's first act should not be to paper
// over it.
//
// A symlink at the path is refused for the same reason. The path comes from
// configuration, and a node writing audit rows through a link somebody else
// planted is a node whose audit trail ends up somewhere its owner did not
// choose.
func OpenSpool(path string, maxBytes int64) (*Spool, error) {
	if path == "" {
		return nil, fmt.Errorf("autonomy: the audit spool needs a path")
	}
	inner, err := spool.Open(spool.Options{
		Path:     path,
		MaxBytes: maxBytes,
		Label:    "the autonomy audit spool",
		Classes: map[spool.Class]spool.Policy{
			// No MaxAge. The question "is it too late to record that this
			// node did this" has no useful answer, and a horizon on an
			// audit class would delete the record of an incident because
			// the outage lasted longer than somebody's retention policy.
			ClassAudit: {DropPriority: spool.PriorityCritical},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Spool{Spool: inner}, nil
}

// Record appends one decision. It satisfies Audit.
//
// A full disk is not an error the caller has to handle: a node that has run
// out of room has bigger problems than a missing audit row, and refusing
// the self-heal because of it converts an availability problem into a
// correctness one. The row is still written when there is room for it, and
// the cap keeps "when there is room" true most of the time.
func (s *Spool) Record(ctx context.Context, row Row) error {
	return s.Spool.Record(ctx, ClassAudit, row)
}

// Peek returns up to n of the oldest rows without removing them.
//
// It is what the replay pump reads: a drain that delivered rows and then
// failed to ack them must be able to look again, and a pump that read the
// file by removing from it could not be retried.
func (s *Spool) Peek(n int) ([]Row, error) {
	raw, err := s.Spool.Peek(n)
	if err != nil {
		return nil, err
	}
	return decodeRows(raw)
}

// Replay hands every stored row to fn, oldest first, and returns how many
// were delivered.
//
// A line that does not parse is skipped rather than treated as an error: it
// is almost certainly the tail of a write that a power cut interrupted, and
// a spool that cannot be read because of its own last row is a spool that
// will never be replayed. The rows after it are still good.
func (s *Spool) Replay(fn func(Row) error) (int, error) {
	return s.Spool.Replay(func(raw spool.Row) error {
		row, err := decodeRow(raw)
		if err != nil {
			return err
		}
		return fn(row)
	})
}

// decodeRows converts the shared envelope back into the arbiter's own row
// type. The envelope is deliberately not merged into Row: the write time on
// the envelope is what the drop policy ages against, and the row's own At
// is what an operator reads.
func decodeRows(raw []spool.Row) ([]Row, error) {
	out := make([]Row, 0, len(raw))
	for _, r := range raw {
		row, err := decodeRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func decodeRow(raw spool.Row) (Row, error) {
	var row Row
	if err := json.Unmarshal(raw.Payload, &row); err != nil {
		return Row{}, fmt.Errorf("autonomy: decode an audit row: %w", err)
	}
	return row, nil
}

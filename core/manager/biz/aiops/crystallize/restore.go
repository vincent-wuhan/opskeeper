package crystallize

import (
	"fmt"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// Restore loads runs into the ledger as though they had been recorded.
//
// It is the inverse of Runs, and it is deliberately NOT a replay of Record.
// The two are not interchangeable, and the reason is the whole reason this
// method has to exist:
//
//   - Record takes one Trial and folds it into a running count. A fold is
//     lossy in a specific way — four verified attempts and one retried
//     attempt both leave Attempts=5, and only Verified tells them apart — so
//     a Run cannot be expanded back into the Trial sequence that produced it.
//   - Record's decisions also depend on state the aggregate does not keep:
//     the Outcome that retired a pattern, the timestamp of the promotion, and
//     the identity of the trials that reset a streak. Replaying a guessed
//     sequence would invent promotions the ledger never granted.
//
// So the aggregate is the state, and Restore reinstates it. That is possible
// because runState.snapshot is total: every field of runState reaches a Run,
// and the four fields that do not round-trip as scalars are derived here —
// the dedup set from the evidence list, and the two grant flags from the
// radius they describe.
//
// Every run is re-validated on the way in. A state file is a file, and a
// file can be edited, truncated by a bad disk, or written by an older build;
// restoring a pattern whose argv carries a shell metacharacter would put a
// runbook in front of an approver that the ledger would have refused to mint
// from evidence. The validator that guards Record guards Restore too, and it
// is the same one, not a second one that can drift.

// ErrUnusableRun reports a persisted run the ledger cannot reason about. It
// is the same class of failure as ErrUnusable on a trial — a producer bug, not
// a decision — and it changes no state because the whole load is refused.
var ErrUnusableRun = fmt.Errorf("%w: persisted run", ErrUnusable)

// Restore replaces the ledger's contents with runs.
//
// It is all-or-nothing: one unusable run rejects the entire load, leaving
// the ledger exactly as it was. A partial restore would be a ledger holding
// some of a snapshot's decisions and inventing the streak arithmetic for the
// rest, which is the one outcome that cannot be reconciled by a later
// Record.
func (l *Ledger) Restore(runs []Run) error {
	states := make([]*runState, 0, len(runs))
	keys := make([]string, 0, len(runs))
	seen := make(map[string]int, len(runs))

	for i, r := range runs {
		if err := r.usable(); err != nil {
			return fmt.Errorf("crystallize: run %d: %w", i, err)
		}
		key := r.Pattern.Key()
		if first, dup := seen[key]; dup {
			return fmt.Errorf("crystallize: run %d repeats the pattern of run %d under one key: a ledger holds one state per pattern, so the file says two things at once", i, first)
		}
		seen[key] = i
		states = append(states, stateOf(r))
		keys = append(keys, key)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs = make(map[string]*runState, len(states))
	for i, key := range keys {
		l.runs[key] = states[i]
	}
	l.order = keys
	return nil
}

// stateOf rebuilds the internal shape from its public projection.
//
// The three derived fields are the interesting ones. seen is the dedup set
// Record maintains so the evidence list holds distinct provenance ids; it is
// reconstructed from the list itself, which loses only the ids that were
// already evicted by maxEvidence — and those were dropped from the list on
// purpose, so their absence is correct rather than lossy. radiusKnown
// distinguishes "no evidence granted a reach" from "the evidence granted
// RadiusNone", which a zero-valued BlastRadius would otherwise collapse for
// us. radiusAgree is stored inverted so the snapshot is readable, and is
// inverted back here.
var _ = time.Time{}

func stateOf(r Run) *runState {
	s := &runState{
		pattern:     r.Pattern.snapshot(),
		streak:      r.Streak,
		verified:    r.Verified,
		attempts:    r.Attempts,
		rejected:    r.Rejections,
		firstSeen:   r.FirstSeen,
		lastSeen:    r.LastSeen,
		evidence:    append([]string(nil), r.Evidence...),
		seen:        make(map[string]struct{}, len(r.Evidence)),
		promoted:    r.Promoted,
		promotedAt:  r.PromotedAt,
		retired:     r.Retired,
		retiredAt:   r.RetiredAt,
		retireWhy:   r.RetireReason,
		ttl:         r.GrantedTTL,
		radius:      r.GrantedRadius,
		radiusKnown: r.GrantedRadius != "" && r.GrantedRadius != domain.RadiusNone,
		radiusAgree: !r.RadiusDisagreement,
	}
	for _, e := range r.Evidence {
		s.seen[e] = struct{}{}
	}
	return s
}

// usable reports whether a persisted run describes a decision this ledger
// could have made.
//
// The pattern half is Trial.validate's own test, reached by building the
// trial it would have come from. A run whose pattern could not have been
// recorded is not a run to restore, and reusing the validator is what keeps
// the two paths from disagreeing about what a legal pattern is.
//
// The counter checks are here for the same reason the pattern checks are: the
// arithmetic Record performs has no answers for them. A negative streak would
// make every subsequent clean trial read as further from promotion; an
// Attempts below Verified claims more first-try verifications than there were
// trials to produce them.
//
// A promoted run with no promotion time, or a retired one with no reason, is
// refused for the same underlying reason: a console reading "promoted on" and
// finding an empty cell is being shown a decision this ledger never made.
func (r Run) usable() error {
	probe := Trial{
		At:      r.FirstSeen,
		Pattern: r.Pattern,
		Outcome: OutcomeVerified,
	}
	if err := probe.validate(); err != nil {
		return err
	}
	switch {
	case r.Streak < 0:
		return fmt.Errorf("%w: negative streak %d", ErrUnusableRun, r.Streak)
	case r.Verified < 0:
		return fmt.Errorf("%w: negative verified count %d", ErrUnusableRun, r.Verified)
	case r.Attempts < 0:
		return fmt.Errorf("%w: negative attempt count %d", ErrUnusableRun, r.Attempts)
	case r.Rejections < 0:
		return fmt.Errorf("%w: negative rejection count %d", ErrUnusableRun, r.Rejections)
	case r.Attempts > 0 && (!r.GrantedRadius.Valid() || r.GrantedRadius == domain.RadiusNone):
		// Every trial that counted as an attempt carried a reach —
		// Trial.validate refuses RadiusNone — and Record notes the grant on
		// each one, so a run with attempts and no reach describes a state
		// Record cannot produce. Restoring it would leave noteGrant with a
		// "no grant seen yet" flag it should not have, and the next clean
		// trial would silently re-derive a reach from itself instead of
		// comparing against the one the evidence granted.
		return fmt.Errorf("%w: %d attempts but no granted reach, and no recorded trial could have left it empty", ErrUnusableRun, r.Attempts)
	case r.Attempts > 0 && r.GrantedTTL <= 0:
		// The same argument for the window, and the same consequence: a
		// zero TTL makes DraftFor fall back to the policy ceiling, which is
		// how a restore that dropped one grant quietly widened every
		// declaration it later emits.
		return fmt.Errorf("%w: %d attempts but no granted window", ErrUnusableRun, r.Attempts)
	case r.Streak > r.Verified:
		return fmt.Errorf("%w: streak %d is longer than the %d verified attempts it counts from", ErrUnusableRun, r.Streak, r.Verified)
	case r.Verified > r.Attempts:
		return fmt.Errorf("%w: %d verified attempts out of %d trials", ErrUnusableRun, r.Verified, r.Attempts)
	case r.Streak > 0 && !r.LastSeen.IsZero() && r.FirstSeen.After(r.LastSeen):
		return fmt.Errorf("%w: first seen %s is after last seen %s", ErrUnusableRun, r.FirstSeen, r.LastSeen)
	case r.Promoted && r.PromotedAt.IsZero():
		return fmt.Errorf("%w: promoted with no promotion time", ErrUnusableRun)
	case r.Promoted && r.PromotedAt.Before(r.FirstSeen):
		return fmt.Errorf("%w: promoted at %s, before it was first seen at %s", ErrUnusableRun, r.PromotedAt, r.FirstSeen)
	case r.Retired && r.RetiredAt.IsZero():
		return fmt.Errorf("%w: retired with no retirement time", ErrUnusableRun)
	case r.Retired && r.RetireReason == "":
		return fmt.Errorf("%w: retired with no reason: a taken-back runbook has to say what contradicted it", ErrUnusableRun)
	case !r.Retired && r.RetireReason != "":
		return fmt.Errorf("%w: carries a retirement reason %q while not retired", ErrUnusableRun, r.RetireReason)
	}
	return nil
}

// Package crystallize promotes a fix that keeps working into a declaration
// the node can run with no model in the path.
//
// The plan's item 7 is a cost claim (arXiv 2607.07052): a fault the platform
// has already diagnosed and fixed several times does not need to be reasoned
// about again, and paying a model to reach the same conclusion every time is
// the steady state the platform should be trying to leave. What was missing
// is the step that recognises "this one is done" from the record of what
// actually happened, and writes the result down in the one form the platform
// can execute without inference — the autonomy block of a plugin manifest,
// which the node evaluates by comparing a metric and running a literal argv.
//
// Three properties are load-bearing, and each one is the reason for a test in
// this package rather than a paragraph here:
//
//   - Promotion is a claim about repetition, not about a good outcome. One
//     working fix is an anecdote, and a runbook written from one is a restart
//     rule nobody can retract in time. The streak counts consecutive
//     first-try verifications and nothing else.
//   - Nothing is invented. Every field of the emitted declaration — the argv,
//     the trigger, the radius, the TTL — was observed on a real run. A
//     pattern the evidence does not describe is held with a reason, never
//     completed with a plausible default.
//   - Promotion is not admission. The output is a draft package that travels
//     the same review and signature channel as every other package; this
//     package has no authority to install anything, and a draft carries no
//     signature.
//
// What it deliberately does not do is generalise. An autonomy action's argv
// is a literal vector the node compares byte for byte (a placeholder is
// refused at load, and so is a shell metacharacter), so a runbook is for the
// target it was proved on. Reusing a restart across a fleet is a review-time
// decision to write a second package, not something this package may infer
// from one host's history.
package crystallize

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// Outcome is what a verification said about one executed fix.
//
// The vocabulary is deliberately wider than pass/fail. A verification that
// passed on the second attempt is not the same evidence as one that passed on
// the first, and a refusal is not evidence about the action at all — folding
// any of those into "ok" is how a ledger promotes a pattern that only works
// when a human is watching.
type Outcome string

const (
	// OutcomeVerified is a first-try pass: the fix ran, the metric came back
	// inside tolerance, and nothing had to be redone. Only this moves the
	// streak forward.
	OutcomeVerified Outcome = "verified"
	// OutcomeVerifiedWithRetry is a pass that took more than one trip
	// through the loop. It is contradicting evidence rather than weak
	// support: the runbook this would promote has no second attempt in it, so
	// a fix that needed one did not, on its own, restore the service.
	OutcomeVerifiedWithRetry Outcome = "verified_with_retry"
	// OutcomeFailed is a run that did not restore the metric.
	OutcomeFailed Outcome = "failed"
	// OutcomeRolledBack is a change that was reverted after the fact. It is
	// contradicting evidence for the same reason a failure is.
	OutcomeRolledBack Outcome = "rolled_back"
	// OutcomeRejected is a human declining to run the fix. It is recorded so
	// the ledger can say why a pattern has no traffic, and it is evidence
	// about neither correctness nor safety — so it moves no counter.
	OutcomeRejected Outcome = "rejected"
)

// Evidence reports whether the outcome says anything about the action.
func (o Outcome) Evidence() bool { return o != OutcomeRejected && o != "" }

// Clean reports whether the outcome is a first-try verification.
func (o Outcome) Clean() bool { return o == OutcomeVerified }

// Fault is the diagnosed fault, in the investigator's own vocabulary.
type Fault struct {
	// Kind is the controlled root-cause kind from the loop's
	// root_cause_object, e.g. "host.disk_full".
	Kind string
	// Family is the resource family the fix was aimed at ("host", "pg",
	// "redis"). It is part of the identity because the same kind on another
	// family is another runbook: the argv differs, and the argv is what the
	// node executes.
	Family string
}

// Action is the fix that ran, in the shape a node needs to run it again.
type Action struct {
	// Tool is the host tool the fix went through.
	Tool string
	// Class is the tool's declared class. It is carried because an autonomy
	// action may not drive a read (sdk.validateAutonomy refuses the package),
	// and because the class is what decides the emitted safety level.
	Class domain.ToolClass
	// Argv is the exact vector that ran. It is copied, never re-derived: an
	// argv assembled by this package would be a new program, and the point of
	// this one is that a human already read it.
	Argv []string
	// Target is the resource the fix was aimed at.
	Target string
	// Trigger is the detection signal the incident had.
	Trigger domain.AutonomyTrigger
	// BlastRadius is the reach the approving human granted.
	BlastRadius domain.BlastRadius
	// TTL is the window the approving human granted.
	TTL time.Duration
}

// Pattern is one (fault, fix) pair — the unit that either crystallizes or does
// not. Its identity includes every field the emitted declaration will carry,
// because two runs that disagree on any of them are two declarations wearing
// one name.
type Pattern struct {
	Fault  Fault
	Action Action
}

// Key is the ledger's grouping identity.
//
// The separator is NUL because none of the parts can contain one: a metric
// name with whitespace is refused at load, and the argv legality check in
// Trial.validate refuses control characters, so a key cannot be forged by
// moving a delimiter into a value.
func (p Pattern) Key() string {
	parts := []string{
		p.Fault.Kind, p.Fault.Family,
		string(p.Action.Class), p.Action.Tool, p.Action.Target,
		strings.Join(p.Action.Argv, "\x00"),
		string(p.Action.Trigger.Kind), p.Action.Trigger.Metric,
		fmt.Sprintf("%v", p.Action.Trigger.Threshold),
	}
	return strings.Join(parts, "\x1f")
}

// Evidence reports whether the pattern describes something a node could be
// asked to run. The zero value is fine for a key that is only compared; a
// declared tool that is not in this list is what the emitted package would
// fail admission over.
func (p Pattern) Evidence() bool {
	return p.Fault.Kind != "" && p.Action.Tool != "" && p.Action.Target != "" && len(p.Action.Argv) > 0
}

func (p Pattern) snapshot() Pattern {
	p.Action.Argv = append([]string(nil), p.Action.Argv...)
	return p
}

// Trial is one executed fix and what its verification said about it.
type Trial struct {
	// At is when the verification completed. Required: a ledger with no
	// ordering cannot say which run the streak counts from.
	At time.Time
	// Pattern is the fault and the fix.
	Pattern Pattern
	// Outcome is the verification's verdict.
	Outcome Outcome
	// Evidence identifies the run in whatever system recorded it (a
	// postmortem artifact, an incident id, an approval id). It is carried
	// into the draft's header so an approver can read the runs the promotion
	// rests on rather than trusting the count.
	Evidence string
}

// ErrUnusable reports a trial the ledger cannot reason about. The distinction
// from "a trial that does not promote" matters: unusable input is a producer
// bug and changes no state, while a failure is evidence and does.
var ErrUnusable = errors.New("crystallize: trial is not usable evidence")

func (t Trial) validate() error {
	p := t.Pattern
	switch {
	case t.At.IsZero():
		return fmt.Errorf("%w: no timestamp", ErrUnusable)
	case p.Fault.Kind == "":
		return fmt.Errorf("%w: no fault kind", ErrUnusable)
	case p.Action.Tool == "":
		return fmt.Errorf("%w: no tool", ErrUnusable)
	case p.Action.Target == "":
		return fmt.Errorf("%w: no target: a runbook is for the target it was proved on", ErrUnusable)
	case len(p.Action.Argv) == 0:
		return fmt.Errorf("%w: no argv: an action with no arguments is a program", ErrUnusable)
	case !p.Action.Class.Valid():
		return fmt.Errorf("%w: unknown tool class %q", ErrUnusable, p.Action.Class)
	case !p.Action.Trigger.Valid():
		return fmt.Errorf("%w: trigger %+v is not a declaration the node can evaluate", ErrUnusable, p.Action.Trigger)
	case !p.Action.BlastRadius.Valid() || p.Action.BlastRadius == domain.RadiusNone:
		return fmt.Errorf("%w: blast radius %q is not a reach a human can grant", ErrUnusable, p.Action.BlastRadius)
	case p.Action.TTL <= 0:
		return fmt.Errorf("%w: ttl must be positive", ErrUnusable)
	case !(domain.AutonomyAction{Argv: p.Action.Argv}).ArgvValid():
		return fmt.Errorf("%w: argv %q could never be a declaration: it is empty, unbounded, or contains a shell metacharacter", ErrUnusable, p.Action.Argv)
	}
	return nil
}

// Verdict is what one trial did to the ledger.
type Verdict string

const (
	// VerdictHeld is evidence recorded, threshold not crossed.
	VerdictHeld Verdict = "held"
	// VerdictPromoted is the trial that crossed the threshold.
	VerdictPromoted Verdict = "promoted"
	// VerdictKept is a clean trial on a pattern that was already promoted.
	VerdictKept Verdict = "kept"
	// VerdictRetired is a contradicting trial on a promoted pattern.
	VerdictRetired Verdict = "retired"
	// VerdictIgnored is a trial that is not evidence at all.
	VerdictIgnored Verdict = "ignored"
)

// Report is the ledger's answer to one Record call.
type Report struct {
	Verdict Verdict
	// Reason is a sentence for the audit row: whoever reads it at 04:00 has
	// to be able to tell "not yet" from "no longer".
	Reason string
	// Run is the pattern's state after the trial.
	Run Run
}

// Run is the ledger's record of one pattern, as a value.
type Run struct {
	Pattern Pattern
	// Streak is the consecutive first-try verifications since the last
	// contradicting trial.
	Streak int
	// Verified is the total number of first-try verifications. It is
	// evidence, so it never decreases.
	Verified int
	// Attempts is every trial that was evidence (verified, failed, rolled
	// back, or verified after a retry).
	Attempts int
	// Rejections is how many humans declined the fix. It is counted and it
	// is not evidence.
	Rejections int
	FirstSeen  time.Time
	LastSeen   time.Time
	// Evidence is the distinct provenance ids seen, in first-seen order and
	// bounded: this is what an approver reads, not a log.
	Evidence []string

	Promoted   bool
	PromotedAt time.Time
	Retired    bool
	RetiredAt  time.Time
	// RetireReason is why a promoted pattern was taken back.
	RetireReason string

	// GrantedRadius and GrantedTTL are the reach and the window the pattern
	// held when it was promoted. They are frozen there: the declaration was
	// signed with them, and a later grant is a new review rather than a new
	// observation.
	GrantedRadius domain.BlastRadius
	GrantedTTL    time.Duration

	// RadiusDisagreement is set when the evidence used for promotion granted
	// different reaches. A declaration carries one radius, and the ledger
	// will not choose between two things humans approved.
	RadiusDisagreement bool
}

// maxEvidence bounds the provenance list. A draft needs to show the runs it
// came from, not every run there has ever been.
const maxEvidence = 20

// Defaults for Policy.
const (
	// DefaultMinCleanStreak is three: two is a coincidence and one is an
	// anecdote.
	DefaultMinCleanStreak = 3
	// DefaultMaxTTL caps a crystallized declaration at half an hour. The node
	// caps at six; the operator who is willing to crystallize is not thereby
	// willing to leave the rule live for an afternoon.
	DefaultMaxTTL = 30 * time.Minute
	// DefaultPackagePrefix names the emitted packages.
	DefaultPackagePrefix = "opskeeper-crystallized"
	// DefaultDraftVersion is the version stamped into a draft. A draft is not
	// a release; admission writes the real one.
	DefaultDraftVersion = "0.1.0-draft"
	// DefaultVendor is the draft's vendor field.
	DefaultVendor = "opskeeper"
)

// Policy is the operator's rule for when a pattern has earned a runbook.
type Policy struct {
	// MinCleanStreak is how many consecutive first-try verifications promote
	// a pattern. Zero means DefaultMinCleanStreak.
	MinCleanStreak int
	// MaxTTL caps the window an emitted declaration may carry. Zero means
	// DefaultMaxTTL. It only ever shortens: the observed grant is the ceiling.
	MaxTTL time.Duration
	// OfflineAfter is stamped into the emitted autonomy block. Zero means the
	// domain default; anything below the floor is raised to it, because a
	// draft the loader would refuse is not a draft.
	OfflineAfter time.Duration
	// PackagePrefix names the emitted packages.
	PackagePrefix string
	// Version is stamped into the draft.
	Version string
	// Vendor is stamped into the draft.
	Vendor string
	// RequiredScopes are the scopes the operator grants the crystallized
	// package. They are a policy input and not a derivation: the ledger saw
	// the tool that ran, never the credential it used.
	RequiredScopes domain.Scopes
}

func (p Policy) withDefaults() Policy {
	if p.MinCleanStreak <= 0 {
		p.MinCleanStreak = DefaultMinCleanStreak
	}
	if p.MaxTTL <= 0 {
		p.MaxTTL = DefaultMaxTTL
	}
	if p.OfflineAfter < domain.MinAutonomyOfflineAfter {
		p.OfflineAfter = domain.DefaultAutonomyOfflineAfter
	}
	if p.PackagePrefix == "" {
		p.PackagePrefix = DefaultPackagePrefix
	}
	if p.Version == "" {
		p.Version = DefaultDraftVersion
	}
	if p.Vendor == "" {
		p.Vendor = DefaultVendor
	}
	return p
}

// Ledger accumulates trials and decides which patterns have earned a draft.
//
// It is in-memory and it is not a database. A ledger that survives a restart
// is a store with a migration, a retention policy and an owner, and none of
// those exist yet; what exists is the decision, and the decision is what this
// type is. Its state is a handful of rows bounded by the number of distinct
// patterns, so the durable form of it is a document rather than a table —
// see crystallizehook.FileStore for the one that ships.
//
// The seam is Runs and Restore, and it took two fields to find that out. This
// comment used to claim the seam was Record and Runs, on the theory that a
// caller replays the runs as trials into a fresh ledger. That theory is wrong
// in a way worth writing down, because it is the obvious one: Record folds a
// trial into a running count, and the fold is lossy. Attempts cannot be
// expanded back into the Outcomes that produced them, and the promotion and
// retirement timestamps are not recoverable from the counters at all. Restore
// is therefore the inverse of Runs, not a replay of Record, and it works
// because snapshot is total — every field of runState reaches a Run.
type Ledger struct {
	mu     sync.Mutex
	policy Policy
	runs   map[string]*runState
	order  []string
}

// NewLedger builds a ledger under a policy.
func NewLedger(p Policy) *Ledger {
	return &Ledger{policy: p.withDefaults(), runs: map[string]*runState{}}
}

// Policy returns the ledger's effective policy.
func (l *Ledger) Policy() Policy { return l.policy }

type runState struct {
	pattern  Pattern
	streak   int
	verified int
	attempts int
	rejected int

	evidence []string
	seen     map[string]struct{}

	firstSeen time.Time
	lastSeen  time.Time

	promoted   bool
	promotedAt time.Time
	retired    bool
	retiredAt  time.Time
	retireWhy  string

	radius      domain.BlastRadius
	radiusKnown bool
	radiusAgree bool
	ttl         time.Duration
}

// Record applies one trial and reports what it did.
func (l *Ledger) Record(t Trial) (Report, error) {
	if err := t.validate(); err != nil {
		return Report{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	key := t.Pattern.Key()
	s := l.runs[key]
	if s == nil {
		s = &runState{pattern: t.Pattern.snapshot(), seen: map[string]struct{}{}, radiusAgree: true}
		l.runs[key] = s
		l.order = append(l.order, key)
	}
	if s.firstSeen.IsZero() || t.At.Before(s.firstSeen) {
		s.firstSeen = t.At
	}
	if t.At.After(s.lastSeen) {
		s.lastSeen = t.At
	}
	if t.Evidence != "" {
		if _, dup := s.seen[t.Evidence]; !dup {
			s.seen[t.Evidence] = struct{}{}
			if len(s.evidence) < maxEvidence {
				s.evidence = append(s.evidence, t.Evidence)
			}
		}
	}

	if !t.Outcome.Evidence() {
		s.rejected++
		return Report{
			Verdict: VerdictIgnored,
			Reason:  "a refusal is not evidence about the fix: it moves no counter",
			Run:     s.snapshot(),
		}, nil
	}

	s.attempts++
	s.noteGrant(t.Pattern.Action)

	if !t.Outcome.Clean() {
		s.streak = 0
		if s.promoted && !s.retired {
			s.retired = true
			s.retiredAt = t.At
			s.retireWhy = fmt.Sprintf("%s: the pattern did not restore the service on the first attempt, and the declaration it earned has no second attempt in it", t.Outcome)
			return Report{Verdict: VerdictRetired, Reason: s.retireWhy, Run: s.snapshot()}, nil
		}
		return Report{
			Verdict: VerdictHeld,
			Reason:  fmt.Sprintf("%s resets the streak to zero", t.Outcome),
			Run:     s.snapshot(),
		}, nil
	}

	s.verified++
	s.streak++
	if s.retired {
		return Report{
			Verdict: VerdictHeld,
			Reason:  "the pattern was retired; a runbook taken back is not reissued by more of the same evidence",
			Run:     s.snapshot(),
		}, nil
	}
	if s.promoted {
		return Report{
			Verdict: VerdictKept,
			Reason:  fmt.Sprintf("already promoted on %s", s.promotedAt.UTC().Format(time.RFC3339)),
			Run:     s.snapshot(),
		}, nil
	}
	if s.streak < l.policy.MinCleanStreak {
		return Report{
			Verdict: VerdictHeld,
			Reason:  fmt.Sprintf("%d of %d consecutive first-try verifications", s.streak, l.policy.MinCleanStreak),
			Run:     s.snapshot(),
		}, nil
	}
	if why, ok := s.crystallizable(); !ok {
		return Report{Verdict: VerdictHeld, Reason: why, Run: s.snapshot()}, nil
	}
	s.promoted = true
	s.promotedAt = t.At
	return Report{
		Verdict: VerdictPromoted,
		Reason: fmt.Sprintf("%d consecutive first-try verifications: the pattern is done being reasoned about",
			s.streak),
		Run: s.snapshot(),
	}, nil
}

// noteGrant tracks the reach and the window of the evidence.
//
// The reach has to agree: a declaration carries one radius, and choosing
// between two reaches humans granted would be this package inventing the
// narrower grant. The window does not have to agree — a shorter window is
// still a grant for the same action, and taking the shortest can only reduce
// the time a wrong runbook is live.
func (s *runState) noteGrant(a Action) {
	if !s.radiusKnown {
		s.radius, s.radiusKnown = a.BlastRadius, true
	} else if s.radius != a.BlastRadius {
		s.radiusAgree = false
	}
	if s.ttl == 0 || a.TTL < s.ttl {
		s.ttl = a.TTL
	}
}

// crystallizable reports whether the pattern describes something a node could
// actually be handed. It is the second gate: the streak says the pattern
// works, this says it can be written down.
func (s *runState) crystallizable() (string, bool) {
	switch {
	case s.pattern.Action.Class == domain.ClassRead:
		return "the tool is a read, and an autonomy declaration may not drive one: a read runs without a human either way", false
	case !s.radiusAgree:
		return "the evidence granted more than one blast radius; a declaration carries one, and this package will not choose between two things humans approved", false
	case !s.pattern.Action.BlastRadius.AtMost(domain.RadiusSingleNS):
		return "the granted reach is wider than a namespace, which is an outage rather than a self-heal", false
	}
	return "", true
}

func (s *runState) snapshot() Run {
	return Run{
		Pattern:            s.pattern.snapshot(),
		Streak:             s.streak,
		Verified:           s.verified,
		Attempts:           s.attempts,
		Rejections:         s.rejected,
		FirstSeen:          s.firstSeen,
		LastSeen:           s.lastSeen,
		Evidence:           append([]string(nil), s.evidence...),
		Promoted:           s.promoted,
		PromotedAt:         s.promotedAt,
		Retired:            s.retired,
		RetiredAt:          s.retiredAt,
		RetireReason:       s.retireWhy,
		GrantedRadius:      s.radius,
		GrantedTTL:         s.ttl,
		RadiusDisagreement: !s.radiusAgree,
	}
}

// Runs returns every pattern, in a stable order.
func (l *Ledger) Runs() []Run {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Run, 0, len(l.order))
	for _, k := range l.order {
		out = append(out, l.runs[k].snapshot())
	}
	return out
}

// Promoted returns the patterns that have earned a draft and still hold it,
// ordered by the package each would emit so a listing is stable across
// processes and a diff shows the pattern that changed.
func (l *Ledger) Promoted() []Run {
	var out []Run
	for _, r := range l.Runs() {
		if r.Promoted && !r.Retired {
			out = append(out, r)
		}
	}
	orderByName(l.Policy().PackagePrefix, out)
	return out
}

// Drafts renders every promoted pattern as a package draft, in the same order
// Promoted reports them.
func (l *Ledger) Drafts() ([]Draft, error) {
	promoted := l.Promoted()
	out := make([]Draft, 0, len(promoted))
	for _, r := range promoted {
		d, err := l.DraftFor(r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// slug renders a string as a package-name fragment.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// shortHash is the pattern's fingerprint, so two patterns that slugify to the
// same readable name are still two packages.
func shortHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:6]
}

func (p Pattern) actionName() string {
	return fmt.Sprintf("%s-%s-%s", "crystallized", slug(p.Fault.Kind), shortHash(p.Key()))
}

func (p Pattern) packageName(prefix string) string {
	return fmt.Sprintf("%s-%s-%s", prefix, slug(p.Fault.Kind), shortHash(p.Key()))
}

// orderByName sorts runs by the package they would emit, so a listing is
// stable across processes and a diff shows the pattern that changed.
func orderByName(prefix string, runs []Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		ni, nj := runs[i].Pattern.packageName(prefix), runs[j].Pattern.packageName(prefix)
		if ni != nj {
			return ni < nj
		}
		return runs[i].Pattern.Key() < runs[j].Pattern.Key()
	})
}

// Package autonomy is the node's answer to a question the plan asks in one
// line: what does a machine do when the people who normally answer it are
// not there?
//
// The answer here is deliberately small. A package may declare, in its
// signed manifest, a fixed list of actions it may take with no human in the
// loop, and the only time that list is in force is while the control plane
// has been unreachable for longer than the package asked for. In every other
// moment — center reachable, center briefly unreachable, call not on the
// list — the arbiter defers to the approval gate and behaviour is exactly
// what it was before any of this existed.
//
// The deferral is the feature. An arbiter that had opinions about ordinary
// calls would be a second policy engine, and two policy engines disagree at
// the worst possible moment. This one only ever has an opinion about a call
// that somebody is explicitly asking to run *without* a human, and its
// opinion is "no" everywhere except the narrow case it was written for.
//
// The three things it checks before saying yes, and the hole each closes:
//
//	argv         The action's arguments are the declared ones, compared
//	             element by element. There is no shell anywhere between the
//	             manifest and the exec, so the worst a model can do is
//	             choose among the argv vectors a human reviewed. A claim
//	             that differs by one byte is a different call, and a call
//	             nobody declared is refused rather than repaired.
//	blast radius The action may not reach further than it declared, and the
//	             loader has already refused to load anything wider than a
//	             single namespace. A claim that asks for more than the
//	             declaration is refused, not clamped: clamping would run
//	             something other than what was signed.
//	idempotency  The key is derived by the host from the declaration and
//	             the call's own target and occurrence, never supplied by
//	             the caller, and it is spent before the action starts. A
//	             replay — the same self-heal arriving twice because a
//	             tunnel reconnected, or because an agent retried — finds
//	             the key spent and is refused.
//
// On top of those, the two timing bounds the plan names: the declaration
// has a TTL measured from when the node admitted the package, and autonomy
// only applies once the center has been gone for longer than the offline
// threshold. Both expire on their own, because a grant that only a human
// can renew is a grant, and one that never expires is configuration.
package autonomy

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// Verdict is the arbiter's answer to one claim.
type Verdict int

const (
	// Defer means "this is not mine to decide". The call goes to the
	// approval gate exactly as it would have if this package did not exist.
	//
	// It is the default and it is returned in every case that is not an
	// explicit, well-formed request to act alone. A node with no autonomy
	// installed returns Defer for everything, and the behaviour of a fleet
	// that has never heard of this package is byte-for-byte the behaviour
	// of one that has it and is fully connected.
	Defer Verdict = iota
	// Run means the action is declared, unexpired, unspent, and the center
	// has been away long enough. The caller may execute it with nobody
	// watching.
	Run
	// Refuse means this was an autonomy claim and it is not allowed. It is
	// terminal: the caller must not fall back to executing, because the
	// only reason a claim reaches here is that somebody wanted to skip the
	// human, and a refusal that degrades into "run it anyway" is worse
	// than no arbitration at all.
	Refuse
)

// String implements fmt.Stringer.
func (v Verdict) String() string {
	switch v {
	case Defer:
		return "defer"
	case Run:
		return "run"
	case Refuse:
		return "refuse"
	default:
		return "unknown"
	}
}

// Claim is one request to run an action with no human in the loop.
//
// It is a struct rather than a tool call because a claim has to say more
// than a call does: which declaration it is invoking, why it believes the
// trigger fired, and which occurrence of the incident it is. A claim that
// cannot fill those in is not a claim.
type Claim struct {
	// Action is the autonomy action's name. Required.
	Action string
	// Trigger is the detection signal the caller believes fired.
	//
	// It is optional, and the arbiter does not need it. The Detector is
	// what decides whether a trigger has fired, by measuring this host — a
	// caller asserting that a threshold was crossed is an assertion, and
	// the one party whose assertion matters least is the one asking for
	// the action. The host tool therefore names an action and leaves this
	// empty, and the declaration's own trigger is what gets evaluated.
	//
	// A claim that does supply one is still checked against the
	// declaration, because a claim carrying a *different* trigger is a
	// claim about a different situation wearing this action's name.
	Trigger domain.AutonomyTrigger
	// Argv is the argument vector the caller intends to run, and it must
	// be the declared one, in order, element for element — when the caller
	// expresses one at all.
	//
	// Empty is not a wildcard. It means the caller had no way to state an
	// argv, which is the case for the host tool: its parameters are an
	// action name, a target and an occurrence, and there is no field a
	// model could put a command in. Such a claim is executed with the
	// declaration's own argv, because Perform runs the declaration and
	// never the claim. A claim that does supply one and differs from the
	// declaration is a different call wearing the action's name, and it is
	// refused.
	Argv []string
	// Target is the resolved resource the action is aimed at. It feeds the
	// idempotency key, so it is part of the identity of the run and not
	// decoration.
	Target string
	// Window names the occurrence, for an action whose key declares one.
	// Two incidents against the same service are two keys; without this
	// the second one would be a replay of the first.
	Window string
}

// Decision is the arbiter's verdict together with the work it did to reach
// it. Everything a caller needs to act, and everything the audit row needs
// to be checkable later.
type Decision struct {
	Verdict Verdict
	// Reason is a human-readable explanation. It is written for whoever
	// reads the audit row at 04:00, not for the machine.
	Reason string
	// Key is the derived idempotency key, set on Run.
	Key string
	// Action is the declaration that was matched, set on Run.
	Action Action
}

// Link reports whether the control plane is reachable.
//
// It is an interface with one method because the tunnel owns that knowledge
// and reconnection is its business, not this package's. The arbiter asks;
// it does not dial, and it certainly does not decide when a link is healthy
// — a node that marked itself online because a heartbeat had not yet timed
// out would be online exactly when it is not.
type Link interface {
	// Reach reports the control plane's current reachability.
	Reach() Reach
}

// LinkFunc adapts a function to Link.
type LinkFunc func() Reach

// Reach implements Link.
func (f LinkFunc) Reach() Reach { return f() }

// Reach is a snapshot of the control plane's reachability.
type Reach struct {
	// Online is true when the center answered the most recent probe.
	Online bool
	// OfflineSince is when it last stopped answering. It is only consulted
	// when Online is false, and a zero value there means "offline for
	// longer than anyone has been keeping time", which fails every
	// threshold — the safe direction.
	OfflineSince time.Time
}

// Action is one compiled autonomy declaration.
//
// It is the manifest's block with the durations resolved and the package
// name attached, and nothing is added: the argv the node compares against is
// the argv a human read when they signed for it.
type Action struct {
	// Package is the admitted package the declaration came from. It is in
	// the audit row because "which package decided to restart this" is the
	// first question anybody asks afterwards.
	Package string
	Name    string
	Tool    string
	// Trigger is the only signal that may set this action off.
	Trigger domain.AutonomyTrigger
	// Argv is the exact vector, compared exactly.
	Argv []string
	// BlastRadius is the reach the declaration claims. The loader has
	// already refused anything wider than a single namespace, so this is
	// a ceiling the node still checks a claim against.
	BlastRadius domain.BlastRadius
	// TTL is how long after InstalledAt this action may still run.
	TTL time.Duration
	// InstalledAt is when the node admitted the package. The TTL is
	// measured from here rather than from the first run, so a declaration
	// cannot be kept alive by the thing it exists to do.
	InstalledAt time.Time
	// IdempotencyKey is the template the host derives the key from.
	IdempotencyKey string
}

// Declares reports whether the action's key depends on an occurrence the
// caller has to name.
func (a Action) DeclaresWindow() bool {
	return strings.Contains(a.IdempotencyKey, "{{"+domain.KeyWindow+"}}")
}

// Key derives the run's idempotency key.
//
// The host derives it. A caller that supplied its own key would be choosing
// the value it is checked against, which is the same as not checking it.
func (a Action) Key(target, window string) string {
	key := strings.ReplaceAll(a.IdempotencyKey, "{{"+domain.KeyTarget+"}}", target)
	return strings.ReplaceAll(key, "{{"+domain.KeyWindow+"}}", window)
}

// Expired reports whether the action is past its TTL as of now.
func (a Action) Expired(now time.Time) bool {
	if a.TTL <= 0 {
		return true
	}
	return now.Sub(a.InstalledAt) > a.TTL
}

// Registry is the autonomy block of every admitted package, compiled.
//
// It is built once, at boot, from the same manifests the tool allow-list is
// built from. A package that was not admitted has no actions here, which is
// what stops an action arriving at runtime from a package the node never
// reviewed.
type Registry struct {
	byName map[string]Action
	// offlineAfter is the shortest threshold any package asked for. The
	// node uses the strictest, not the average: a package that wants two
	// minutes of human presence should not get one because another
	// package wanted thirty seconds of it.
	offlineAfter time.Duration
	installedAt  time.Time
	// maxRadius is the widest reach any autonomy action on this node may
	// have. It is the node's setting rather than the package's, and the
	// single-namespace default is the same one the loader enforces: an edge
	// that can act alone across a namespace is not self-healing, it is the
	// outage.
	maxRadius domain.BlastRadius
}

// NewRegistry compiles the autonomy blocks of the admitted manifests.
//
// The manifests arrive already validated — the loader refused a malformed
// one — so this reports an error only for the thing validation cannot see:
// two packages declaring the same action name. That is a genuine collision
// and it is refused rather than resolved, because either resolution would
// run an argv the operator did not read for the name they were shown.
func NewRegistry(manifests []domain.PluginManifest, installedAt time.Time) (*Registry, error) {
	r := &Registry{byName: make(map[string]Action), installedAt: installedAt, maxRadius: domain.RadiusSingleNS}
	for _, m := range manifests {
		policy := m.Spec.Autonomy
		if len(policy.Actions) == 0 {
			continue
		}
		if r.offlineAfter == 0 || policy.OfflineDelay() < r.offlineAfter {
			r.offlineAfter = policy.OfflineDelay()
		}
		for _, decl := range policy.Actions {
			a := Action{
				Package:        m.Metadata.Name,
				Name:           decl.Name,
				Tool:           decl.Tool,
				Trigger:        decl.Trigger,
				Argv:           append([]string(nil), decl.Argv...),
				BlastRadius:    decl.BlastRadius,
				TTL:            time.Duration(decl.TTL),
				InstalledAt:    installedAt,
				IdempotencyKey: decl.IdempotencyKey,
			}
			if prior, dup := r.byName[a.Name]; dup {
				return nil, &NameCollisionError{
					Name:    a.Name,
					Package: a.Package,
					Other:   prior.Package,
				}
			}
			r.byName[a.Name] = a
		}
	}
	return r, nil
}

// NameCollisionError is two packages declaring the same autonomy action.
type NameCollisionError struct {
	Name, Package, Other string
}

func (e *NameCollisionError) Error() string {
	return "autonomy action " + e.Name + " is declared by both " + e.Package + " and " + e.Other +
		": an action name has to name one declaration, or review cannot tell which argv was agreed to"
}

// Actions returns the compiled actions, sorted by name, for a health page
// or a reviewer's listing.
func (r *Registry) Actions() []Action {
	out := make([]Action, 0, len(r.byName))
	for _, a := range r.byName {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Empty reports whether any package asked for autonomy at all.
func (r *Registry) Empty() bool { return len(r.byName) == 0 }

// OfflineAfter is the threshold autonomy applies at: the strictest one any
// package asked for, floored by the host.
func (r *Registry) OfflineAfter() time.Duration {
	if r.offlineAfter < domain.MinAutonomyOfflineAfter {
		return domain.MinAutonomyOfflineAfter
	}
	return r.offlineAfter
}

// Lookup returns the declaration for a name.
func (r *Registry) Lookup(name string) (Action, bool) {
	a, ok := r.byName[name]
	return a, ok
}

// Audit records one autonomous decision.
//
// It is an interface because the arbiter's job ends at the verdict, and the
// row that outlives the outage belongs to whatever is going to replay it.
// A nil Audit is a real configuration — a node that has no spool directory
// yet still arbitrates — and the arbiter says so out loud rather than
// pretending.
type Audit interface {
	Record(ctx context.Context, row Row) error
}

// Row is one decision, as it will be read back after the tunnel returns.
type Row struct {
	At       time.Time              `json:"at"`
	Action   string                 `json:"action"`
	Package  string                 `json:"package"`
	Tool     string                 `json:"tool"`
	Target   string                 `json:"target"`
	Argv     []string               `json:"argv"`
	Trigger  domain.AutonomyTrigger `json:"trigger"`
	Key      string                 `json:"idempotency_key"`
	Verdict  string                 `json:"verdict"`
	Reason   string                 `json:"reason,omitempty"`
	Phase    string                 `json:"phase"`
	Result   string                 `json:"result,omitempty"`
	ExitCode int                    `json:"exit_code,omitempty"`
}

// Audit phases. A decision is written before the action starts, because a
// node that crashes halfway through a self-heal must leave behind the fact
// that it started one.
const (
	// PhaseDecided is the verdict, written before execution.
	PhaseDecided = "decided"
	// PhaseCompleted is the outcome, written after.
	PhaseCompleted = "completed"
)

// Audit results, recorded on the completion row.
const (
	ResultOK      = "ok"
	ResultFailed  = "failed"
	ResultSkipped = "skipped"
)

// Stats are the arbiter's counters, for the node's health endpoint.
//
// A node that is refusing autonomy is either working as designed or broken,
// and the two look identical from outside. A Refuse count that is not zero
// is the difference between those two readings, so it is counted.
type Stats struct {
	// Deferred is every call the arbiter had no opinion on, which on a
	// connected node is every call.
	Deferred uint64
	// Run is every action it let run alone.
	Run uint64
	// Refused is every autonomy claim it turned down.
	Refused uint64
	// Replays is the subset of refusals that were a spent key, kept apart
	// because it is the number an operator reads first: it means something
	// is retrying.
	Replays uint64
}

// Detector reports whether a trigger's condition holds on this node right
// now.
//
// It exists because a declared trigger that nothing evaluates is decoration.
// The plan requires the trigger in the declaration, and a declaration alone
// does not make a condition true: without this, the argv, the radius, the
// TTL and the key would all check out on a node whose disk is at 4% and the
// restart would happen anyway, with a manifest line that reads as if
// something had decided it should.
//
// The answer carries the reason as well as the verdict, because "the action
// needs node_disk_used_ratio > 0.92 and it is at 0.41" is the single most
// useful sentence in the audit row: it distinguishes a node that was right
// to wait from a node whose metric pipeline is dead.
type Detector interface {
	// Fired reports whether the trigger holds, and why not if it does not.
	Fired(t domain.AutonomyTrigger) (bool, string)
}

// ValueDetector evaluates the metric_above trigger from a value lookup.
//
// The lookup is a function rather than a collector type so this package
// holds no dependency on how the node scrapes. The node passes something
// that answers for the most recent sample; a test passes a map.
type ValueDetector struct {
	// Value returns the current value of a named metric. The second result
	// is false when the node has no such metric at all, which is
	// different from a value of zero and must not be conflated with it.
	Value func(metric string) (float64, bool)
}

// Fired implements Detector.
func (d ValueDetector) Fired(t domain.AutonomyTrigger) (bool, string) {
	if t.Kind != domain.TriggerMetricAbove {
		return false, "this host does not know how to evaluate a " + string(t.Kind) + " trigger"
	}
	if d.Value == nil {
		return false, "this node has no metric source to evaluate " + t.Metric + " against"
	}
	value, ok := d.Value(t.Metric)
	if !ok {
		return false, "this node has never seen a sample of " + t.Metric +
			", and a trigger that cannot be measured is not a trigger that has fired"
	}
	if value <= t.Threshold {
		return false, t.Metric + " is at " + trimFloat(value) + ", and this action is declared for " +
			t.Metric + " > " + trimFloat(t.Threshold)
	}
	return true, ""
}

// Options configures an Arbiter.
type Options struct {
	// Registry is the compiled autonomy block. Required.
	Registry *Registry
	// Link reports the control plane's reachability. Required.
	Link Link
	// Audit is where decisions are recorded. Optional, and a node without
	// one is reported by Snapshot rather than assumed.
	Audit Audit
	// Detector evaluates a declaration's trigger. Optional, and a node
	// without one grants no autonomy at all — an unmeasurable trigger is
	// not an unmet one, and treating "I cannot tell" as "yes" would make
	// the whole feature a coin flip on the health of the metric pipeline.
	Detector Detector
	// Now defaults to time.Now.
	Now func() time.Time
	// Log is optional.
	Log *slog.Logger
}

// Arbiter decides whether a claim may run without a human.
type Arbiter struct {
	reg      *Registry
	link     Link
	audit    Audit
	detector Detector
	now      func() time.Time
	log      *slog.Logger

	mu    sync.Mutex
	spent map[string]time.Time
	stats Stats
}

// New returns an Arbiter.
func New(opts Options) (*Arbiter, error) {
	if opts.Registry == nil {
		return nil, &ConfigError{Field: "Registry", Reason: "is required: an arbiter with no declarations would never run anything, and saying so at construction is better than at 03:00"}
	}
	if opts.Link == nil {
		return nil, &ConfigError{Field: "Link", Reason: "is required: the arbiter cannot tell an outage from a healthy link without it"}
	}
	a := &Arbiter{
		reg:      opts.Registry,
		link:     opts.Link,
		audit:    opts.Audit,
		detector: opts.Detector,
		now:      opts.Now,
		spent:    make(map[string]time.Time),
	}
	if a.now == nil {
		a.now = time.Now
	}
	if opts.Log != nil {
		a.log = opts.Log
	}
	return a, nil
}

// ConfigError is an Arbiter built without something it cannot work without.
type ConfigError struct{ Field, Reason string }

func (e *ConfigError) Error() string { return "autonomy: " + e.Field + " " + e.Reason }

// Adjudicate decides one claim.
//
// The order of the questions is the design, and it is not the order they
// look like they should be:
//
//  1. Is this an autonomy claim at all? A call that names no action is not
//     ours to judge, and it defers before anything else is examined. This
//     is the regression guard: a node with this package installed and a
//     center on the end behaves exactly as it did before.
//  2. Is the center reachable, and has it been gone long enough? If a human
//     can be asked, they are asked — including about a claim that looks
//     malformed. Deferring a bad claim to a human is the safe answer; a
//     refusal is only necessary when nobody is left to overrule it.
//  3. Only then is the claim examined. Everything below this line is a
//     refusal, and a refusal means "do not run this, and do not retry it
//     through another path".
func (a *Arbiter) Adjudicate(ctx context.Context, claim Claim) Decision {
	if claim.Action == "" {
		a.count(func(s *Stats) { s.Deferred++ })
		return Decision{Verdict: Defer, Reason: "not an autonomy claim"}
	}
	action, declared := a.reg.Lookup(claim.Action)
	if !declared {
		// Not a claim the node can honour. It defers rather than refuses,
		// because the approval gate is the authority for a call nobody
		// declared, and a mutating one still gets a human.
		a.count(func(s *Stats) { s.Deferred++ })
		return Decision{Verdict: Defer, Reason: "no autonomy action named " + claim.Action + " is declared on this node"}
	}

	now := a.now()
	reach := a.link.Reach()
	if !a.centerIsAway(reach, now) {
		a.count(func(s *Stats) { s.Deferred++ })
		return Decision{
			Verdict: Defer,
			Reason:  "the control plane is reachable, so " + action.Name + " goes to the approval gate like any other call",
		}
	}

	// From here the answer is a refusal or a run. A caller that reached
	// this line wanted to skip a human, and a claim that does not match a
	// declaration is not going to be granted a softer answer.
	if reason, ok := a.mismatch(action, claim, now); !ok {
		row := a.row(action, claim, Refuse, reason, PhaseDecided, "", 0)
		a.write(ctx, row)
		a.count(func(s *Stats) { s.Refused++ })
		return Decision{Verdict: Refuse, Reason: reason, Action: action}
	}

	key := action.Key(claim.Target, claim.Window)
	if reason, spent := a.claimKey(key, now); spent {
		row := a.row(action, claim, Refuse, reason, PhaseDecided, key, 0)
		a.write(ctx, row)
		a.count(func(s *Stats) { s.Refused++; s.Replays++ })
		return Decision{Verdict: Refuse, Reason: reason, Key: key, Action: action}
	}

	// The row goes down before the action starts. A node that is
	// power-cycled mid-self-heal then leaves behind the fact that it began
	// one, which is the difference between an incident with a cause and an
	// incident with a gap in it.
	row := a.row(action, claim, Run, "declared, unexpired, unspent, and the control plane has been away long enough",
		PhaseDecided, key, 0)
	a.write(ctx, row)
	a.count(func(s *Stats) { s.Run++ })
	if a.log != nil {
		a.log.Info("autonomy action running without an approval",
			"action", action.Name, "package", action.Package, "target", claim.Target, "key", key)
	}
	return Decision{Verdict: Run, Key: key, Action: action, Reason: row.Reason}
}

// Complete records the outcome of an action the arbiter allowed to run.
//
// It is a separate call from Adjudicate on purpose. The decision is written
// before the work and the outcome after it, so a crash in between leaves
// exactly the record that matters: a self-heal that began and never
// reported.
func (a *Arbiter) Complete(ctx context.Context, d Decision, result string, exitCode int) {
	if d.Verdict != Run {
		return
	}
	a.write(ctx, Row{
		At:       a.now(),
		Action:   d.Action.Name,
		Package:  d.Action.Package,
		Tool:     d.Action.Tool,
		Target:   d.Key,
		Key:      d.Key,
		Verdict:  Run.String(),
		Phase:    PhaseCompleted,
		Result:   result,
		ExitCode: exitCode,
	})
}

// centerIsAway reports whether autonomy applies at all.
//
// The threshold is measured from OfflineSince, and a zero value there means
// the link never reported when it went down — in which case autonomy does
// not apply, because the alternative reading is "offline since the epoch",
// which would grant the whole list to a node whose link watcher is broken.
func (a *Arbiter) centerIsAway(reach Reach, now time.Time) bool {
	if reach.Online {
		return false
	}
	if reach.OfflineSince.IsZero() {
		return false
	}
	return now.Sub(reach.OfflineSince) >= a.reg.OfflineAfter()
}

// mismatch compares a claim against its declaration and reports the first
// difference, in the order that makes the reason useful to whoever reads it.
//
// The order is deliberate. The trigger is checked first because it is the
// claim's own account of why it is running, and a claim whose reason does
// not match is a claim about a different situation. The argv is checked
// next because it is the thing that would actually execute. The radius and
// the TTL are last because they are the ones a caller is least likely to be
// lying about deliberately.
func (a *Arbiter) mismatch(action Action, claim Claim, now time.Time) (string, bool) {
	if claim.Trigger.Kind != "" && !action.Trigger.SameTrigger(claim.Trigger) {
		return "the trigger " + describe(claim.Trigger) + " is not the one " +
			action.Name + " is declared for (" + describe(action.Trigger) + ")", false
	}
	// And the declared trigger has to be *true*. This is the check that
	// makes the field a condition rather than a comment: a declaration
	// whose trigger cannot be measured is refused, and so is one whose
	// metric is comfortably below the threshold.
	if a.detector == nil {
		return "this node has no way to evaluate " + describe(action.Trigger) +
			", and autonomy runs on measured conditions or not at all", false
	}
	if fired, why := a.detector.Fired(action.Trigger); !fired {
		return why, false
	}
	// A claim that carries an argv has to be carrying the declared one. A
	// claim that carries none is a caller with no way to express one — the
	// host tool — and gets the declaration, which is what executes
	// regardless of what the claim said.
	if len(claim.Argv) > 0 && !sameArgv(action.Argv, claim.Argv) {
		return "the arguments [" + strings.Join(claim.Argv, " ") + "] are not the declared [" +
			strings.Join(action.Argv, " ") + "]; autonomy runs the declared call or nothing", false
	}
	if claim.Target == "" && action.IdempotencyKey != "" && strings.Contains(action.IdempotencyKey, "{{"+domain.KeyTarget+"}}") {
		return "the action's key is derived from the resolved target and no target was given", false
	}
	if action.DeclaresWindow() && claim.Window == "" {
		return "the action's key names an occurrence and no occurrence was given", false
	}
	// The node's own ceiling is re-checked here even though the loader
	// already applied it. The loader is the review surface; this is the
	// enforcement point, and an enforcement point that trusts an upstream
	// check has no failure mode of its own — it just moves the failure
	// upstream. The declaration is a ceiling, not a grant: an action that
	// reaches further is refused rather than clamped, because clamping runs
	// something other than what was signed.
	if !action.BlastRadius.AtMost(a.reg.maxRadius) {
		return "the declaration reaches " + string(action.BlastRadius) +
			" and this node's autonomy ceiling is " + string(a.reg.maxRadius), false
	}
	if action.Expired(now) {
		return "the declaration expired " + now.Sub(action.InstalledAt).String() +
			" after it was admitted, and its TTL was " + action.TTL.String() +
			"; autonomy is not renewed by the outage it exists for", false
	}
	return "", true
}

// sameArgv compares two argument vectors element by element.
//
// There is no prefix match, no glob and no "the first element is a binary
// and the rest are whatever the model wanted". A vector that differs by one
// byte is a different call, and the only safe reading of a different call is
// that it is not the declared one.
func sameArgv(declared, claimed []string) bool {
	if len(declared) != len(claimed) {
		return false
	}
	for i := range declared {
		if declared[i] != claimed[i] {
			return false
		}
	}
	return true
}

// claimKey spends an idempotency key, and reports the reason if it was
// already spent.
//
// Spending happens under the lock, before the action runs, so two
// submissions of the same self-heal that arrive together cannot both get
// through. A key stays spent for the registry's window and is then
// forgotten: a node that refused the same restart tomorrow because it ran
// it today would be a policy nobody wrote, and the plan's own test — TTL
// 过期、幂等键去重 — treats the two as different bounds.
func (a *Arbiter) claimKey(key string, now time.Time) (reason string, spent bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneSpentLocked(now)
	if at, ok := a.spent[key]; ok {
		return "this exact self-heal already ran at " + at.UTC().Format(time.RFC3339) +
			" under the same idempotency key; a replay is refused rather than repeated", true
	}
	a.spent[key] = now
	return "", false
}

// pruneSpentLocked forgets keys past the window.
//
// The window is the strictest TTL any live declaration carries: a key
// outliving every action that could produce it is a key nothing consults.
func (a *Arbiter) pruneSpentLocked(now time.Time) {
	if len(a.spent) == 0 {
		return
	}
	longest := time.Duration(0)
	for _, action := range a.reg.byName {
		if action.TTL > longest {
			longest = action.TTL
		}
	}
	if longest <= 0 {
		return
	}
	for key, at := range a.spent {
		if now.Sub(at) > longest {
			delete(a.spent, key)
		}
	}
}

func (a *Arbiter) count(f func(*Stats)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(&a.stats)
}

// Snapshot returns the arbiter's counters.
func (a *Arbiter) Snapshot() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

func (a *Arbiter) row(action Action, claim Claim, v Verdict, reason, phase, key string, exit int) Row {
	return Row{
		At:       a.now(),
		Action:   action.Name,
		Package:  action.Package,
		Tool:     action.Tool,
		Target:   claim.Target,
		Argv:     claim.Argv,
		Trigger:  action.Trigger,
		Key:      key,
		Verdict:  v.String(),
		Reason:   reason,
		Phase:    phase,
		ExitCode: exit,
	}
}

// write puts a row on the spool.
//
// A failure to record is logged and not returned as a decision change: the
// verdict was already made, and refusing a legal action *because the disk
// is full* would turn an audit problem into an availability one — on a node
// whose whole reason for acting is that it is already in trouble.
func (a *Arbiter) write(ctx context.Context, row Row) {
	if a.audit == nil {
		if a.log != nil {
			a.log.Warn("an autonomy decision was made with nowhere to record it",
				"action", row.Action, "verdict", row.Verdict)
		}
		return
	}
	if err := a.audit.Record(ctx, row); err != nil && a.log != nil {
		a.log.Error("could not record an autonomy decision", "action", row.Action, "error", err)
	}
}

func describe(t domain.AutonomyTrigger) string {
	if t.Kind == domain.TriggerMetricAbove {
		return t.Metric + " > " + trimFloat(t.Threshold)
	}
	return string(t.Kind)
}

// trimFloat renders a threshold for a human without the noise a float64
// carries into an audit row: "0.92", not "0.92000000000000004".
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

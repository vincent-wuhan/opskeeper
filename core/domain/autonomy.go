package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Autonomy is the narrow, signed permission for a node to act while the
// control plane is unreachable.
//
// It is the only capability in the system that lets something run with no
// human in the loop, and it is deliberately the smallest one the plan could
// be talked into: a closed list of pre-declared actions, each one an exact
// argument vector, each one bounded to a single host or service, each one
// good for a bounded window, each one executable once per idempotency key.
//
// Four properties do the work, and each one closes a specific hole that a
// looser reading would open:
//
//   - An action's arguments are **the declared argv**, compared exactly.
//     There is no shell, no string, no interpolation and no model in the
//     path between the declaration and the exec. A model that decides to run
//     `rm -rf /` cannot express that call at all, because the arbiter has
//     nothing to match it against and an unmatched claim is refused.
//   - The reach is **pod or narrower**, checked at load. An action that
//     could take a namespace down is not an edge self-heal, it is an outage
//     with extra steps, and the moment the center is back it is an incident
//     nobody authorised.
//   - The window is **finite and host-capped**. Autonomy is for the outage,
//     not for a quieter operating model; a policy nobody re-reads is how a
//     temporary grant becomes a permanent one.
//   - The key is **spent once**. Replaying a key that already ran is
//     refused, which is what stops a reconnecting agent from re-running
//     yesterday's self-heal on the way back up.
//
// None of this is a grant. The control plane signs it, the node verifies
// the signature through the same channel every other package travels, and
// the arbiter still refuses anything it cannot match to a declaration. A
// plugin that ships an autonomy block is a package that has asked to be
// reviewed by a person.

// MaxAutonomyTTL bounds how long any single autonomy declaration can stay
// usable.
//
// The value is a policy choice, not a technical limit, and it is short on
// purpose. The thing this protects against is not a hostile action — the
// argv is fixed — it is a *stale* one: the operator who agreed to a restart
// of one service at 03:00 during an incident is not saying anything about
// the node at 23:00, and a declaration that outlives the incident has
// stopped being consent and become configuration.
const MaxAutonomyTTL = 6 * time.Hour

// DefaultAutonomyOfflineAfter is how long the control plane may be
// unreachable before autonomy applies at all.
//
// A node that has been alone for four seconds has not established that
// anything is wrong, and the human is likely still there. Autonomy is for
// the case where asking is impossible and waiting is worse than acting
// inside a declared boundary, so the threshold is measured in minutes rather
// than seconds. A node that declares a shorter one may: a package that
// wants faster self-heal is asking for a shorter window of human presence,
// which is its call to make and the host's to bound.
const DefaultAutonomyOfflineAfter = 2 * time.Minute

// MinAutonomyOfflineAfter is the floor on that threshold, whatever a
// manifest says.
//
// Zero would mean "the instant the socket blips", and a tunnel reconnect
// cycle is exactly a stream of blips: the node would run its whole autonomy
// list on every reconnect. Two minutes is below any incident's first useful
// action and above any reconnect storm.
const MinAutonomyOfflineAfter = 30 * time.Second

// AutonomyTriggerKind is the closed vocabulary of detection signals.
//
// This is a set of kinds rather than an expression language on purpose. An
// expression here — a metric name and an operator, or worse a little query
// language — would be evaluated by the node on data the node does not
// otherwise understand, and its failure mode is a *wrong* answer: a trigger
// that should not have fired, firing. The plan's own test line is the tell —
// "自治动作逃逸：篡改 argv" — so what matters is that a claim is bound to a
// declared signal, and that binding is checkable. Adding a kind is a change
// to this file and to the node's detector, reviewed like any other
// capability.
type AutonomyTriggerKind string

const (
	// TriggerMetricAbove fires when a named metric from the node's own
	// collector rises past a declared threshold. It is the canonical
	// self-heal: the node already scrapes the value, the threshold is in
	// the manifest, and the comparison is a float compare.
	TriggerMetricAbove AutonomyTriggerKind = "metric_above"
)

// AutonomyTrigger is the detection signal that may set an action off.
//
// The action is bound to its signal rather than carrying it, so a claim
// cannot invent one: a claim naming a trigger the action does not declare
// is refused, which is what stops a caller from attaching a plausible
// reason to a call that was chosen for a different reason.
type AutonomyTrigger struct {
	// Kind is the host-evaluated comparison. Required.
	Kind AutonomyTriggerKind `json:"kind" yaml:"kind"`
	// Metric is the collector's metric name for TriggerMetricAbove.
	// Required for that kind and rejected for any other.
	Metric string `json:"metric,omitempty" yaml:"metric,omitempty"`
	// Threshold is the value the metric must rise past. Required for
	// TriggerMetricAbove.
	Threshold float64 `json:"threshold,omitempty" yaml:"threshold,omitempty"`
}

// Valid reports whether the trigger is one this host knows how to evaluate.
//
// An unknown kind is a load error rather than a runtime no-op: a package
// that declares a trigger nobody implements must fail review, not install
// and quietly never fire.
func (t AutonomyTrigger) Valid() bool {
	switch t.Kind {
	case TriggerMetricAbove:
		return t.Metric != "" && !strings.ContainsAny(t.Metric, " \t\n")
	default:
		return false
	}
}

// SameTrigger reports whether a claim's trigger is the declared one.
//
// Both the kind and the comparison have to match. A claim that names the
// right metric with a different threshold is a different trigger wearing
// the same metric's name, and it would fire earlier than the operator
// agreed to.
func (t AutonomyTrigger) SameTrigger(c AutonomyTrigger) bool {
	if t.Kind != c.Kind {
		return false
	}
	return t.Metric == c.Metric && t.Threshold == c.Threshold
}

// AutonomyAction is one pre-declared thing the node may do by itself while
// the center is away.
//
// All five fields the plan names are required, and the reason each one is
// required is the reason it is here: trigger is why, argv is what, blast
// radius is how far, ttl is for how long, and idempotency key is the once.
type AutonomyAction struct {
	// Name identifies the action inside the package. Required, unique
	// within the package.
	Name string `json:"name" yaml:"name"`
	// Tool is the host tool this action runs, by the same name the
	// package's tools list uses. Required — an action with no tool is a
	// shell command, which is the one thing this may not be.
	Tool string `json:"tool" yaml:"tool"`
	// Trigger is the detection signal. Exactly one.
	Trigger AutonomyTrigger `json:"trigger" yaml:"trigger"`
	// Argv is the exact argument vector, in order.
	//
	// This is a list, not a string, and that is the whole point. A string
	// is a shell line waiting for a quoting mistake, a metacharacter or a
	// model that learned to enjoy `$()`. A list is a vector the host
	// compares element by element and hands to exec without a shell in
	// between.
	//
	// It is compared exactly — no wildcards, no prefix matches, no
	// "the first element names a binary and the rest are whatever the
	// model wanted". A claim whose argv differs by one byte is a different
	// call, and an undeclared call is refused.
	Argv []string `json:"argv" yaml:"argv"`
	// BlastRadius is the reach. Pod or narrower; the host refuses to load
	// anything wider, so this field is a declaration the loader checks
	// rather than a ceiling the runtime clamps.
	BlastRadius BlastRadius `json:"blast_radius" yaml:"blast_radius"`
	// TTL is how long after its declaration this action may still run.
	TTL Duration `json:"ttl" yaml:"ttl"`
	// IdempotencyKey is the template for the key that makes this action
	// run once.
	//
	// It is a template rather than a constant because a constant is spent
	// by the first execution and the second outage finds an action that
	// cannot run at all. The placeholders below are the only ones that
	// exist, and a claim supplies the values; the node derives the key
	// itself and refuses a claim whose key is not the derivation. That is
	// what makes replay detection possible without trusting the caller for
	// the key it is being checked against.
	IdempotencyKey string `json:"idempotency_key" yaml:"idempotency_key"`
}

// Idempotency key placeholders. A template may use these and no others.
const (
	// KeyTarget expands to the resolved resource the action was aimed at.
	KeyTarget = "target"
	// KeyWindow expands to an occurrence the caller supplies, so two
	// incidents against the same service are two keys rather than one.
	KeyWindow = "window"
)

// maxAutonomyArgv bounds the vector's length. There is no legitimate
// self-heal action with hundreds of arguments, and a bounded list cannot be
// used to smuggle a payload.
const maxAutonomyArgv = 32

// Valid reports whether the action is one the host will load.
//
// Every failure here is a load error rather than a runtime refusal on the
// node: a package that cannot be loaded is a package a human finds out
// about, and a package that installs and then refuses at 03:00 is one that
// is found out about by an incident.
func (a AutonomyAction) Valid() bool {
	if a.Name == "" || a.Tool == "" {
		return false
	}
	if !a.Trigger.Valid() {
		return false
	}
	if !a.TTL.Valid() || time.Duration(a.TTL) > MaxAutonomyTTL {
		return false
	}
	// The plan's own words: 单主机/单服务. A namespace or a cluster is not
	// an edge self-heal.
	if !a.BlastRadius.Valid() || a.BlastRadius.Rank() > RadiusSingleNS.Rank() {
		return false
	}
	if !a.ArgvValid() {
		return false
	}
	return KeyTemplateValid(a.IdempotencyKey)
}

// ArgvValid reports whether the argument vector is usable.
//
// The list must be non-empty, bounded, and free of anything that only
// means something to a shell. The forbidden set is small and blunt on
// purpose — this is not a parser, it is a refusal list for the characters
// that make an argument vector a program again.
func (a AutonomyAction) ArgvValid() bool {
	if len(a.Argv) == 0 || len(a.Argv) > maxAutonomyArgv {
		return false
	}
	for _, arg := range a.Argv {
		if arg == "" {
			return false
		}
		if strings.ContainsAny(arg, "\x00\n`$;|&<>(){}[]*?!#~\\\"'") {
			return false
		}
	}
	return true
}

// KeyTemplateValid reports whether the idempotency key is a template this
// host can derive.
//
// A template must name at least one placeholder: a constant key is spent by
// the first run, and a template with an unknown placeholder is a key the
// node cannot reproduce, which would make every claim look like a replay.
func KeyTemplateValid(tmpl string) bool {
	if tmpl == "" || !strings.Contains(tmpl, "{{") {
		return false
	}
	if strings.Count(tmpl, "{{") != strings.Count(tmpl, "}}") {
		return false
	}
	rest := tmpl
	for _, name := range []string{KeyTarget, KeyWindow} {
		for {
			i := strings.Index(rest, "{{"+name+"}}")
			if i < 0 {
				break
			}
			rest = rest[:i] + rest[i+len("{{"+name+"}}"):]
		}
	}
	// Anything left over is a placeholder this host does not know.
	return !strings.Contains(rest, "{{")
}

// DeriveKey renders the idempotency key for one occurrence.
//
// The host derives it, never the caller. A claim that supplies its own key
// is refused, because a caller that chooses the value it is checked against
// can always choose a fresh one.
func (a AutonomyAction) DeriveKey(target, window string) string {
	key := a.IdempotencyKey
	key = strings.ReplaceAll(key, "{{"+KeyTarget+"}}", target)
	key = strings.ReplaceAll(key, "{{"+KeyWindow+"}}", window)
	return key
}

// NeedsWindow reports whether the action's key depends on an occurrence the
// caller has to name.
func (a AutonomyAction) NeedsWindow() bool {
	return strings.Contains(a.IdempotencyKey, "{{"+KeyWindow+"}}")
}

// NeedsTarget reports whether the action's key depends on the resolved
// resource.
func (a AutonomyAction) NeedsTarget() bool {
	return strings.Contains(a.IdempotencyKey, "{{"+KeyTarget+"}}")
}

// AutonomyPolicy is a package's autonomy block.
//
// A package with no actions in it has no autonomy, which is the default and
// the state of every package that ships today. The offline threshold lives
// here rather than on the node because it is part of what the control plane
// signs: a package that wants self-heal within thirty seconds of losing the
// center is asking the operator to accept a shorter window of human
// presence, and that is a decision made at review rather than at runtime.
type AutonomyPolicy struct {
	// OfflineAfter is how long the control plane must have been
	// unreachable before any action here applies. Zero means the host
	// default; the host floor applies either way.
	OfflineAfter Duration `json:"offline_after,omitempty" yaml:"offline_after,omitempty"`
	// Actions are the pre-declared things this package may do alone.
	Actions []AutonomyAction `json:"actions,omitempty" yaml:"actions,omitempty"`
}

// Valid reports whether the policy is loadable.
//
// An empty policy is valid and means no autonomy. A non-empty one is
// checked whole, and any action that fails takes the package with it —
// there is no partial autonomy, because "this package is partly trusted to
// act alone" is not a state a node can enforce.
func (p AutonomyPolicy) Valid() bool {
	if len(p.Actions) == 0 {
		// A threshold with nothing to do under it is a declaration of
		// intent that would read, at review, as something it is not.
		return time.Duration(p.OfflineAfter) == 0
	}
	if !p.OfflineAfter.Valid() {
		return false
	}
	if time.Duration(p.OfflineAfter) != 0 && time.Duration(p.OfflineAfter) < MinAutonomyOfflineAfter {
		return false
	}
	seen := make(map[string]struct{}, len(p.Actions))
	tools := make(map[string]struct{}, len(p.Actions))
	for _, a := range p.Actions {
		if !a.Valid() {
			return false
		}
		if _, dup := seen[a.Name]; dup {
			return false
		}
		seen[a.Name] = struct{}{}
		// One action per tool. Two actions driving the same tool differ
		// only in their argv, and a second one is a wider grant wearing
		// the first one's name at review time.
		if _, dup := tools[a.Tool]; dup {
			return false
		}
		tools[a.Tool] = struct{}{}
	}
	return true
}

// OfflineDelay is the threshold autonomy applies at, with the host's
// defaults and floors filled in.
func (p AutonomyPolicy) OfflineDelay() time.Duration {
	d := time.Duration(p.OfflineAfter)
	if d == 0 {
		d = DefaultAutonomyOfflineAfter
	}
	if d < MinAutonomyOfflineAfter {
		d = MinAutonomyOfflineAfter
	}
	return d
}

// Duration is a manifest-friendly time.Duration.
//
// It renders as a Go duration string ("90s", "6h") and parses the same,
// plus a bare integer count of seconds for the manifests that would rather
// not write the unit.
//
// The codecs are TextMarshaler/TextUnmarshaler rather than the YAML and
// JSON ones on purpose. This module has no dependencies at all — that is
// its defining property — so it cannot reach for a YAML library to
// implement UnmarshalYAML. The text interfaces are honoured by both
// encoding/json and gopkg.in/yaml.v3, which is enough for the two formats
// a manifest actually arrives in, and it keeps the contract module free of
// the parser that reads it.
//
// Negative values parse, and are then refused by Valid(): "a TTL of -1s"
// should be a load error with a name, not a duration that silently behaves
// like a very large one.
type Duration time.Duration

// Valid reports whether the duration is usable as a positive bound.
func (d Duration) Valid() bool { return time.Duration(d) > 0 }

// String renders the duration.
func (d Duration) String() string { return time.Duration(d).String() }

// Or returns the duration, or the given value when it is unset.
func (d Duration) Or(zero time.Duration) time.Duration {
	if time.Duration(d) == 0 {
		return zero
	}
	return time.Duration(d)
}

// MarshalText renders the duration.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// UnmarshalText parses "30s", "6h" or a bare number of seconds.
func (d *Duration) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if secs, err := strconv.Atoi(s); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q is neither a Go duration nor a number of seconds: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

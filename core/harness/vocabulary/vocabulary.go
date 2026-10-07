// Package vocabulary answers one question about a golden case: could the
// running system have produced this case's expected answer at all?
//
// The evaluation corpus names capabilities as `<family>.<method>`. So does
// the platform — but the two are written down in different places, and
// nothing joins them. A case that expects `pg.kill_session` is a reasonable
// specification of what an operator would do; if no registry in the build
// offers a tool by that name, the case is unpassable however well the agent
// reasons. Scoring it produces a zero, and a zero in a leaderboard is read
// as "the agent was wrong" — a false statement about a question that was
// never asked of the agent.
//
// So this package refuses to let that happen quietly. It reports the gap
// explicitly, with provenance: a symbol missing from every provider is a
// different fact from a symbol that some provider does offer, and a report
// that could not tell them apart would send someone to fix the wrong thing.
//
// A Capability is a *set of providers*, not a flat list, precisely because
// the answer is only actionable when you can say which subsystem owns the
// capability. "pg.kill_session is missing" is a question; "pg.kill_session is
// missing, and the pg adapter is the subsystem that would own it" is a task.
package vocabulary

import (
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

// Provider is one subsystem's declared capability set.
//
// Two shapes of coverage exist in this codebase and they are not
// interchangeable. The middleware adapters register tools by exact name, so
// `pg.kill_session` is a fact about a function that exists. Plugin packages
// declare a *capability family* — a read-only PG package serves `pg`, not
// `pg.kill_session` specifically — because what a package offers is a
// judgement about the whole family, made once, and re-deriving it per
// method would be a second opinion the package never agreed to. Both are
// recorded, and a report says which kind of coverage applied.
type Provider struct {
	// Name identifies the subsystem, e.g. "middleware-adapter" or
	// "plugin:opskeeper-sre-readonly". It is printed in the report so a
	// reader knows where a capability would have to come from.
	Name string
	// Symbols are the exact `<family>.<method>` names this provider offers.
	Symbols []string
	// Families are capability prefixes this provider covers wholesale.
	Families []string
}

// Capability is everything one concrete build can serve, kept per provider
// so a report can attribute a gap to a subsystem.
type Capability struct {
	Providers []Provider
}

// ProviderOf returns the name of a provider that covers sym, and how.
//
// An exact symbol match anywhere in the build beats a family match
// anywhere, regardless of declaration order. A package that declares the
// `pg` family is a real answer for `pg.kill_session`, but an adapter that
// registers a tool by that literal name is a better one, and reporting the
// family when the exact fact is available sends a reader to the wrong
// subsystem to look. Within one kind of match, declaration order decides.
func (c Capability) ProviderOf(sym string) (string, Coverage, bool) {
	for _, p := range c.Providers {
		for _, s := range p.Symbols {
			if s == sym {
				return p.Name, CoverageExact, true
			}
		}
	}
	family := FamilyOf(sym)
	for _, p := range c.Providers {
		for _, f := range p.Families {
			if f == family {
				return p.Name, CoverageFamily, true
			}
		}
	}
	return "", CoverageNone, false
}

// Coverage says how a provider covers a symbol.
type Coverage string

const (
	// CoverageNone means nothing in the build covers the symbol.
	CoverageNone Coverage = "none"
	// CoverageExact means a provider registers the symbol by that name.
	CoverageExact Coverage = "exact"
	// CoverageFamily means a provider covers the symbol's capability
	// family, without naming the method.
	CoverageFamily Coverage = "family"
)

// FamilyOf returns the capability prefix of a `<family>.<method>` symbol.
// A symbol with no dot is returned whole, so a malformed expectation is
// reported as a miss rather than matching everything.
func FamilyOf(sym string) string {
	if i := strings.IndexByte(sym, '.'); i >= 0 {
		return sym[:i]
	}
	return sym
}

// Symbols is the union of every provider's exact symbols, sorted and
// deduplicated. Families are excluded: a family is not a symbol anyone can
// call.
func (c Capability) Symbols() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range c.Providers {
		for _, s := range p.Symbols {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ProviderNames lists the subsystems consulted, in declaration order.
func (c Capability) ProviderNames() []string {
	out := make([]string, 0, len(c.Providers))
	for _, p := range c.Providers {
		out = append(out, p.Name)
	}
	return out
}

// Gap is the set of a case's expectations that no provider in the build can
// produce.
type Gap struct {
	// CaseID is the golden case, e.g. "pg/lock-waits".
	CaseID string
	// UnservableRootCauses are expected root-cause symbols no provider
	// offers.
	UnservableRootCauses []string
	// UnservableRemediations are expected remediation symbols no provider
	// offers.
	UnservableRemediations []string
}

// Servable reports whether some provider can produce every symbol the case
// expects. A false here means any score for this case is a statement about
// the corpus, not about the agent.
//
// The CaseID check is not redundant with the two length checks. A Gap with
// nothing in it is what Check returns for a nil case, and "we found no
// unservable symbol" is not the same claim as "we found a case and could
// serve all of it" — a caller that skipped loading the corpus and one that
// loaded a perfectly servable corpus would otherwise be indistinguishable.
func (g Gap) Servable() bool {
	return g.CaseID != "" &&
		len(g.UnservableRootCauses) == 0 &&
		len(g.UnservableRemediations) == 0
}

// Reason renders a one-line explanation naming the case and the first
// missing symbol, for use in a CLI error or a stored artifact.
func (g Gap) Reason() string {
	if g.Servable() {
		return ""
	}
	if len(g.UnservableRootCauses) > 0 {
		return g.CaseID + ": no provider in this build offers the root cause " +
			g.UnservableRootCauses[0] + " that this case expects"
	}
	return g.CaseID + ": no provider in this build offers the remediation " +
		g.UnservableRemediations[0] + " that this case expects"
}

// Check compares one case against one build's capability set.
//
// Comparison is exact, and deliberately so. The heuristic judge scores root
// causes by exact symbol equality, so a case the build cannot satisfy
// exactly is a case the judge cannot score correctly. Fuzzy matching here
// would produce a "servable" verdict that the scorer then contradicts,
// which is the one outcome worse than an honest gap.
func Check(c *schema.Case, cap Capability) Gap {
	if c == nil {
		return Gap{}
	}
	gap := Gap{CaseID: c.ID}
	for _, sym := range c.Expect.RootCauseLines {
		if _, _, ok := cap.ProviderOf(sym); !ok {
			gap.UnservableRootCauses = append(gap.UnservableRootCauses, sym)
		}
	}
	for _, sym := range c.Expect.RemediationOptions {
		if _, _, ok := cap.ProviderOf(sym); !ok {
			gap.UnservableRemediations = append(gap.UnservableRemediations, sym)
		}
	}
	return gap
}

// CheckAgainst reports the gap for a single provider instead of the whole
// build.
//
// This exists because "the platform can do it" and "the closed loop can do
// it" are different claims, and collapsing them hides a real failure. The
// middleware adapters register pg.kill_session, so pg/lock-waits looks
// servable — but the closed loop's remediation planner proposes
// pg.kill_backend, and the judge scores remediation_quality by exact
// symbol. Every closed-loop run of that case therefore scores zero on that
// dimension for a reason that has nothing to do with how the agent reasoned,
// and a platform-level gate says nothing about it.
func CheckAgainst(c *schema.Case, cap Capability, provider string) Gap {
	if c == nil {
		return Gap{}
	}
	p, ok := providerByName(cap, provider)
	if !ok {
		// An unknown provider name is a caller bug, and reporting an
		// empty capability would turn it into "nothing is servable",
		// which is a finding rather than an error.
		return Gap{CaseID: c.ID, UnservableRootCauses: append([]string(nil), c.Expect.RootCauseLines...),
			UnservableRemediations: append([]string(nil), c.Expect.RemediationOptions...)}
	}
	only := Capability{Providers: []Provider{p}}
	return Check(c, only)
}

// ProviderNamesPresent lists the providers a capability actually declares.
func ProviderNamesPresent(cap Capability) []string { return cap.ProviderNames() }

func providerByName(cap Capability, name string) (Provider, bool) {
	for _, p := range cap.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// CheckAll reports every case in the corpus, preserving corpus order so two
// runs of the gate produce byte-identical output.
func CheckAll(cases []*schema.Case, cap Capability) []Gap {
	gaps := make([]Gap, 0, len(cases))
	for _, c := range cases {
		gaps = append(gaps, Check(c, cap))
	}
	return gaps
}

// ServableCount is the number of cases the build can fully satisfy.
func ServableCount(gaps []Gap) int {
	n := 0
	for _, g := range gaps {
		if g.Servable() {
			n++
		}
	}
	return n
}

// LoopProviderName is the provider holding the closed loop's own
// remediation vocabulary. It lives here rather than in the command so the
// assessment and the report cannot disagree about which provider they mean.
const LoopProviderName = "loop-investigator"

// ── closed-loop reachability ────────────────────────────────────────────
//
// A platform-level Gap answers "can anything here do this". It cannot answer
// "will a run of the system itself be scoreable here", because the closed
// loop reaches a case's expectations by two independent routes that the
// platform view conflates into one:
//
//   - its root cause arrives as root_cause_object.kind, a closed enum, and
//     is bridged onto case vocabulary by a declared kind map. The loop's own
//     action list has nothing to do with this axis.
//   - its remedy arrives as remediation_options[].action, which does have to
//     be one the loop can actually propose.
//
// Collapsing them produces a gate that is structurally always zero — the
// action list contains no dotted root-cause symbols, so no case can ever
// clear the root-cause axis no matter what is fixed on the remediation side.
// A gate that cannot go green measures nothing, and is indistinguishable from
// a real finding. So the two axes are assessed apart.

// LoopCase is whether one case is reachable by a closed-loop run.
type LoopCase struct {
	// CaseID is the golden case.
	CaseID string
	// MissingRemediations are the case's expected remedies the loop cannot
	// propose. Non-empty means remediation_quality is a fixed zero.
	MissingRemediations []string
	// ResolvingKinds are the contract root-cause kinds whose mapping lands
	// on a symbol this case expects. Empty means rca_accuracy is a fixed
	// zero for every run, whatever the agent does.
	ResolvingKinds []string
	// RootCauseAssessed is false when no kind map was supplied. The
	// root-cause axis then has not been checked, which is a different fact
	// from "checked and found nothing", and reporting the second when only
	// the first is true would put a permanent, meaningless zero in front of
	// a reader.
	RootCauseAssessed bool
}

// Servable reports whether a closed-loop run can score on both axes. A false
// here means any judge score for this case is a statement about the wiring,
// not about the agent.
//
// An unassessed root-cause axis is not servability, and is not its negation
// either: the answer is "not known yet", so Servable is false and
// RootCauseAssessed is false, and a caller that wants to tell those apart
// has to look at the flag.
func (l LoopCase) Servable() bool {
	return l.CaseID != "" && len(l.MissingRemediations) == 0 &&
		l.RootCauseAssessed && len(l.ResolvingKinds) > 0
}

// RemediationServable reports the half that is always assessable: can the
// loop propose something this case expects to see done.
func (l LoopCase) RemediationServable() bool {
	return l.CaseID != "" && len(l.MissingRemediations) == 0
}

// Reason names the first thing that is missing, for a CLI or an artifact.
func (l LoopCase) Reason() string {
	switch {
	case l.Servable():
		return ""
	case len(l.MissingRemediations) > 0:
		return l.CaseID + ": the loop cannot propose " + l.MissingRemediations[0] +
			", which this case expects"
	default:
		return l.CaseID + ": no root-cause kind maps onto a symbol this case expects"
	}
}

// KindResolver is the kind map: contract root-cause kind to the case
// vocabulary symbols it stands for.
type KindResolver func(kind string) (symbols []string, ok bool)

// AssessLoop reports, per case, whether a closed-loop run can reach it on
// both axes.
//
// kinds is the contract's root-cause enum — every value the investigated
// phase may emit. A case counts as reachable on the root-cause axis when at
// least one of those kinds resolves onto a symbol the case expects, because
// which kind a particular run produces is not knowable in advance and
// penalising every case for the kinds nobody happened to emit would be a
// statement about the corpus, not about the run.
func AssessLoop(cases []*schema.Case, cap Capability, kinds []string, resolve KindResolver) []LoopCase {
	actions, ok := providerSymbols(cap, LoopProviderName)
	if !ok {
		actions = nil
	}
	have := make(map[string]bool, len(actions))
	for _, a := range actions {
		have[a] = true
	}

	out := make([]LoopCase, 0, len(cases))
	for _, c := range cases {
		if c == nil {
			continue
		}
		lc := LoopCase{CaseID: c.ID}
		for _, want := range c.Expect.RemediationOptions {
			if !have[want] {
				lc.MissingRemediations = append(lc.MissingRemediations, want)
			}
		}
		lc.RootCauseAssessed = resolve != nil
		for _, k := range kinds {
			if resolve == nil {
				continue
			}
			syms, ok := resolve(k)
			if !ok {
				continue
			}
			for _, sym := range syms {
				for _, want := range c.Expect.RootCauseLines {
					if sym == want {
						lc.ResolvingKinds = append(lc.ResolvingKinds, k)
					}
				}
			}
		}
		lc.ResolvingKinds = dedupe(lc.ResolvingKinds)
		out = append(out, lc)
	}
	return out
}

// LoopServableCount is the number of cases a closed-loop run can score on
// both axes.
func LoopServableCount(cases []LoopCase) int {
	n := 0
	for _, c := range cases {
		if c.Servable() {
			n++
		}
	}
	return n
}

// LoopRootCauseServableCount is the number of cases whose root-cause axis
// the loop can reach. Reported on its own because the two axes fail for
// unrelated reasons, and printing the combined number under either label
// would attribute a remediation gap to the root cause or the reverse.
func LoopRootCauseServableCount(cases []LoopCase) int {
	n := 0
	for _, c := range cases {
		if c.CaseID != "" && c.RootCauseAssessed && len(c.ResolvingKinds) > 0 {
			n++
		}
	}
	return n
}

// LoopRemediationServableCount is the number of cases whose remediation axis
// the loop can satisfy. It is meaningful with or without a kind map, which
// is why it is reported separately from LoopServableCount: with no kind map
// the second is 0 by construction and only the first says anything.
func LoopRemediationServableCount(cases []LoopCase) int {
	n := 0
	for _, c := range cases {
		if c.RemediationServable() {
			n++
		}
	}
	return n
}

func providerSymbols(cap Capability, name string) ([]string, bool) {
	for _, p := range cap.Providers {
		if p.Name == name {
			return append([]string(nil), p.Symbols...), true
		}
	}
	return nil, false
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

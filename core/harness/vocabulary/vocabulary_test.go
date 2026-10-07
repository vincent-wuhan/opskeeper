package vocabulary

import (
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

func caseWith(id string, rootCauses, remediations []string) *schema.Case {
	return &schema.Case{
		ID:     id,
		Expect: schema.Expect{RootCauseLines: rootCauses, RemediationOptions: remediations},
	}
}

func full() Capability {
	return Capability{Providers: []Provider{
		{Name: "middleware-adapter", Symbols: []string{"pg.kill_session", "pg.explain_query"}},
		{Name: "plugin:readonly", Symbols: []string{"pg_lock"}},
		// A package declares the family it covers, not each method.
		{Name: "plugin:observability", Families: []string{"host"}},
	}}
}

func TestACaseTheBuildCanFullySatisfyIsServable(t *testing.T) {
	gap := Check(caseWith("pg/x", []string{"pg_lock"}, []string{"pg.kill_session"}), full())
	if !gap.Servable() {
		t.Fatalf("a case whose every symbol some provider offers was reported unservable: %+v", gap)
	}
	if gap.Reason() != "" {
		t.Errorf("Reason() = %q, want empty for a servable case", gap.Reason())
	}
}

// The whole reason this package exists: a case no provider can satisfy must
// not look like a case the agent failed.
func TestACaseExpectingASymbolNoProviderOffersIsUnservable(t *testing.T) {
	// The root cause here IS servable, so the only gap is on the
	// remediation axis and Reason() has to name that one.
	gap := Check(caseWith("pg/lock-waits", []string{"pg_lock"}, []string{"pg.kill_backend"}), full())
	if gap.Servable() {
		t.Fatal("a case expecting pg.kill_backend was called servable when no provider offers it")
	}
	if len(gap.UnservableRemediations) != 1 || gap.UnservableRemediations[0] != "pg.kill_backend" {
		t.Errorf("unservable remediations = %v, want [pg.kill_backend]", gap.UnservableRemediations)
	}
	if !strings.Contains(gap.Reason(), "pg.kill_backend") {
		t.Errorf("Reason() = %q, want it to name the symbol no provider offers", gap.Reason())
	}
}

// A near-miss is still a miss. pg.kill_session vs pg.kill_backend differ by
// one word, and accepting prefixes or substrings here would produce a
// "servable" verdict the exact-match judge then contradicts.
func TestANearMissIsStillAMiss(t *testing.T) {
	for _, want := range []string{"pg.kill", "kill_session", "pg.kill_session_v2", "PG.KILL_SESSION"} {
		if Check(caseWith("pg/x", nil, []string{want}), full()).Servable() {
			t.Errorf("%q was accepted as servable; the comparison is not exact", want)
		}
	}
}

// A capability is only actionable if the report can say which subsystem
// owns it. Without provenance a gap says nothing about where to add the
// missing tool.
func TestTheProviderOfASymbolIsReported(t *testing.T) {
	cap := full()
	if name, cov, ok := cap.ProviderOf("pg.kill_session"); !ok || name != "middleware-adapter" || cov != CoverageExact {
		t.Errorf("ProviderOf(pg.kill_session) = %q, %q, %v; want middleware-adapter, exact, true", name, cov, ok)
	}
	if name, cov, ok := cap.ProviderOf("pg_lock"); !ok || name != "plugin:readonly" || cov != CoverageExact {
		t.Errorf("ProviderOf(pg_lock) = %q, %q, %v; want plugin:readonly, exact, true", name, cov, ok)
	}
	if _, cov, ok := cap.ProviderOf("nope"); ok || cov != CoverageNone {
		t.Error("ProviderOf found a provider for a symbol nobody offers")
	}
}

// The first provider in declaration order wins, so a caller can put the
// subsystem it considers authoritative at the front.
func TestTheFirstProviderInDeclarationOrderIsCredited(t *testing.T) {
	cap := Capability{Providers: []Provider{
		{Name: "first", Symbols: []string{"pg.lock_waits"}},
		{Name: "second", Symbols: []string{"pg.lock_waits"}},
	}}
	if name, _, _ := cap.ProviderOf("pg.lock_waits"); name != "first" {
		t.Errorf("credited %q, want first", name)
	}
}

func TestSymbolsIsTheSortedDeduplicatedUnion(t *testing.T) {
	cap := Capability{Providers: []Provider{
		{Name: "a", Symbols: []string{"z.one", "a.two"}},
		{Name: "b", Symbols: []string{"a.two", "m.three"}},
	}}
	got := cap.Symbols()
	want := []string{"a.two", "m.three", "z.one"}
	if len(got) != len(want) {
		t.Fatalf("Symbols() = %v, want %v", got, want)
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("Symbols() is not sorted: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Symbols() = %v, want %v", got, want)
		}
	}
}

func TestBothAxesAreReportedIndependently(t *testing.T) {
	gap := Check(caseWith("pg/x", []string{"pg_lock", "k8s_oom"}, []string{"pg.kill_session", "k8s.scale"}), full())
	if len(gap.UnservableRootCauses) != 1 || gap.UnservableRootCauses[0] != "k8s_oom" {
		t.Errorf("unservable root causes = %v, want [k8s_oom]", gap.UnservableRootCauses)
	}
	if len(gap.UnservableRemediations) != 1 || gap.UnservableRemediations[0] != "k8s.scale" {
		t.Errorf("unservable remediations = %v, want [k8s.scale]", gap.UnservableRemediations)
	}
	// The root-cause complaint is surfaced first because that is the axis
	// that becomes rca_accuracy.
	if !strings.Contains(gap.Reason(), "k8s_oom") {
		t.Errorf("Reason() = %q, want the root cause named first", gap.Reason())
	}
}

// Covers the other branch of Reason(): when only the remediation axis is
// unservable, that is what the message has to name.
func TestReasonNamesTheRemediationWhenThatIsTheOnlyGap(t *testing.T) {
	gap := Check(caseWith("pg/lock-waits", []string{"pg_lock"}, []string{"pg.kill_backend"}), full())
	if gap.Servable() {
		t.Fatal("a case with one unservable remediation was called servable")
	}
	if !strings.Contains(gap.Reason(), "pg.kill_backend") {
		t.Errorf("Reason() = %q, want the missing remediation named", gap.Reason())
	}
}

func TestACaseWithNoExpectationsIsServableRatherThanVacuous(t *testing.T) {
	// The loader already rejects such a case; a second, differently-worded
	// rejection here would only obscure the first.
	if gap := Check(caseWith("pg/x", nil, nil), full()); !gap.Servable() {
		t.Errorf("an empty expectation was reported as a gap: %+v", gap)
	}
}

func TestNilCaseIsHandledRatherThanPanicking(t *testing.T) {
	if Check(nil, full()).Servable() {
		t.Error("a nil case was reported servable")
	}
}

func TestCheckAllPreservesCorpusOrder(t *testing.T) {
	cases := []*schema.Case{
		caseWith("a/1", []string{"pg_lock"}, []string{"pg.kill_session"}),
		caseWith("b/1", []string{"nope"}, []string{"pg.kill_session"}),
		caseWith("c/1", []string{"pg_lock"}, []string{"also_nope"}),
	}
	gaps := CheckAll(cases, full())
	if len(gaps) != 3 {
		t.Fatalf("got %d gaps, want 3", len(gaps))
	}
	for i, want := range []string{"a/1", "b/1", "c/1"} {
		if gaps[i].CaseID != want {
			t.Errorf("gap[%d].CaseID = %q, want %q", i, gaps[i].CaseID, want)
		}
	}
	if ServableCount(gaps) != 1 {
		t.Errorf("ServableCount = %d, want 1", ServableCount(gaps))
	}
}

// A build that declares nothing is a misconfigured caller, not a corpus
// that suddenly works. Silently reporting "nothing is servable" would be
// indistinguishable from a real regression.
func TestAnEmptyCapabilityServesNothing(t *testing.T) {
	gaps := CheckAll([]*schema.Case{caseWith("a/1", []string{"pg_lock"}, []string{"pg.kill_session"})}, Capability{})
	if ServableCount(gaps) != 0 {
		t.Error("an empty capability was treated as serving the corpus")
	}
}

// A package that declares a family is a real answer, and it has to be
// reported as family coverage so a reader knows the method name itself was
// never checked.
func TestAFamilyMatchIsReportedAsFamilyCoverage(t *testing.T) {
	name, cov, ok := full().ProviderOf("host.top_cpu_procs")
	if !ok || name != "plugin:observability" || cov != CoverageFamily {
		t.Errorf("ProviderOf(host.top_cpu_procs) = %q, %q, %v; want plugin:observability, family, true", name, cov, ok)
	}
	// Family coverage is prefix-anchored, not a substring match: a symbol
	// whose family is not declared is still a miss.
	if _, _, ok := full().ProviderOf("hostile.thing"); ok {
		t.Error("family coverage matched across a dot boundary")
	}
}

// An exact registration anywhere in the build beats a family claim made
// earlier, so the report attributes the capability to the subsystem that
// can actually be pointed at.
func TestAnExactMatchBeatsAnEarlierFamilyClaim(t *testing.T) {
	cap := Capability{Providers: []Provider{
		{Name: "plugin:pg", Families: []string{"pg"}},
		{Name: "middleware-adapter", Symbols: []string{"pg.lock_waits"}},
	}}
	name, cov, ok := cap.ProviderOf("pg.lock_waits")
	if !ok || cov != CoverageExact || name != "middleware-adapter" {
		t.Errorf("ProviderOf = %q, %q, %v; want middleware-adapter, exact, true", name, cov, ok)
	}
	name, cov, ok = cap.ProviderOf("pg.something_else")
	if !ok || cov != CoverageFamily || name != "plugin:pg" {
		t.Errorf("ProviderOf = %q, %q, %v; want plugin:pg, family, true", name, cov, ok)
	}
}

// A case whose expectations are all family-covered is servable, which is
// the whole point of consulting the packages at all.
func TestACaseCoveredByAPackageFamilyIsServable(t *testing.T) {
	gap := Check(caseWith("host/cpu-spike", []string{"host.host_load", "host.top_cpu_procs"}, []string{"host.kill_process"}), full())
	if !gap.Servable() {
		t.Errorf("a case covered by a declaring package was reported unservable: %+v", gap)
	}
}

func TestFamilyOfIsPrefixAnchored(t *testing.T) {
	for in, want := range map[string]string{
		"pg.lock_waits":  "pg",
		"host.host_load": "host",
		"nodots":         "nodots",
	} {
		if got := FamilyOf(in); got != want {
			t.Errorf("FamilyOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// The closed loop and the platform are scored apart on purpose. An adapter
// can register pg.kill_session while the loop's remediation planner only
// proposes pg.kill_backend; the case then looks servable and every
// closed-loop run scores zero on remediation_quality anyway.
func TestCheckAgainstScoresOneProviderAlone(t *testing.T) {
	cap := Capability{Providers: []Provider{
		{Name: "middleware-adapter", Symbols: []string{"pg.kill_session", "pg.lock_waits"}},
		{Name: "loop", Symbols: []string{"pg.kill_backend"}},
	}}
	c := caseWith("pg/lock-waits", []string{"pg.lock_waits"}, []string{"pg.kill_session"})

	if gap := Check(c, cap); !gap.Servable() {
		t.Error("the platform as a whole should be able to serve this case")
	}
	loopGap := CheckAgainst(c, cap, "loop")
	if loopGap.Servable() {
		t.Error("the loop was reported able to propose pg.kill_session")
	}
	if len(loopGap.UnservableRemediations) != 1 || loopGap.UnservableRemediations[0] != "pg.kill_session" {
		t.Errorf("unservable remediations = %v, want [pg.kill_session]", loopGap.UnservableRemediations)
	}
	// The root-cause axis is reported too, and correctly so: the loop names
	// its conclusion with a closed enum (pg_lock) rather than a case symbol,
	// so it cannot express this case's root cause either. That is a separate
	// fact from the remediation mismatch and the projection package is where
	// the root-cause side gets bridged, via a declared kind map.
	if len(loopGap.UnservableRootCauses) != 1 || loopGap.UnservableRootCauses[0] != "pg.lock_waits" {
		t.Errorf("unservable root causes = %v, want [pg.lock_waits]", loopGap.UnservableRootCauses)
	}
}

// An unknown provider name is a caller bug. Returning "nothing is servable"
// would dress that bug up as a finding about the corpus.
func TestCheckAgainstAnUnknownProviderIsAFullMissRatherThanAFullHit(t *testing.T) {
	cap := full()
	gap := CheckAgainst(caseWith("pg/x", []string{"pg_lock"}, []string{"pg.kill_session"}), cap, "no-such-provider")
	if gap.Servable() {
		t.Error("an unknown provider was reported as covering the case")
	}
	if len(gap.UnservableRemediations) != 1 {
		t.Errorf("unservable remediations = %v, want the whole expectation list", gap.UnservableRemediations)
	}
}

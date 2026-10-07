package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDomainOfUsesTheSameLayerRuleDomaincheckDoes(t *testing.T) {
	for path, want := range map[string]string{
		"core/manager/biz/aiops/loop":         "aiops",
		"core/manager/server/federation/http": "federation",
		"core/domains/service/federationlink": "federationlink",
		"core/manager/model/audit":            "audit",
		"core/manager/data/hitl/store":        "hitl",
		"core/base/pkg/audit":              "pkg",
		"core/manager/knowledge/index":        "knowledge",
		// Outside the control plane entirely.
		"core/edge/agentprofile/profile.go": "",
		"core/floor/skill/builtin/host.go":  "",
		"cmd/opskeeper/main.go":             "",
	} {
		if got := domainOf(path); got != want {
			t.Errorf("domainOf(%q) = %q, want %q", path, got, want)
		}
	}
}

// A build file is not a bounded context, and ranking it as one would put
// go.mod at the top of a report about which domains evolve on their own.
func TestAModuleFileIsNotADomain(t *testing.T) {
	for _, path := range []string{"core/manager/go.mod", "core/manager/go.sum"} {
		if got := domainOf(path); got != "" {
			t.Errorf("domainOf(%q) = %q, want no domain", path, got)
		}
	}
}

func TestSummariseCountsADomainChangedAloneOnlyWhenItWasAlone(t *testing.T) {
	r := summarise([]Change{
		{Commit: "a", Domains: []string{"aiops"}},
		{Commit: "b", Domains: []string{"aiops"}},
		{Commit: "c", Domains: []string{"aiops", "edge"}},
		{Commit: "d", Domains: []string{"edge"}},
		{Commit: "e", Domains: []string{"edge", "audit"}},
	})

	byName := map[string]Solo{}
	for _, s := range r.Solo {
		byName[s.Domain] = s
	}
	if got := byName["aiops"]; got.Solo != 2 || got.Total != 3 {
		t.Errorf("aiops = %+v, want 2 solo of 3", got)
	}
	if got := byName["edge"]; got.Solo != 1 || got.Total != 3 {
		t.Errorf("edge = %+v, want 1 solo of 3", got)
	}
	if got := byName["audit"]; got.Solo != 0 || got.Total != 1 {
		t.Errorf("audit = %+v, want 0 solo of 1", got)
	}
	// The ranking exists to put the most independently changed first.
	if r.Solo[0].Domain != "aiops" {
		t.Errorf("top of the ranking = %q, want aiops", r.Solo[0].Domain)
	}
}

func TestSummariseCountsCoChangeOncePerCommitNotPerFile(t *testing.T) {
	// Five files in one domain, one commit: that is one change, and a
	// pair that appears in a single commit is worth one, not five.
	r := summarise([]Change{
		{Commit: "a", Domains: []string{"aiops", "edge"}},
		{Commit: "b", Domains: []string{"aiops", "edge"}},
	})
	if len(r.Pairs) != 1 || r.Pairs[0].Times != 2 {
		t.Fatalf("pairs = %+v, want a single pair at 2", r.Pairs)
	}
	if r.Pairs[0].A != "aiops" || r.Pairs[0].B != "edge" {
		t.Errorf("pair = %+v, want aiops/edge", r.Pairs[0])
	}
}

// The report tells the reader whether the ranking is worth anything. A
// repository where every commit touches everything produces a high median,
// and presenting that ranking without saying so would be the whole failure
// this tool could cause.
func TestTheMedianIsWhatTheWarningIsBasedOn(t *testing.T) {
	spread := summarise([]Change{
		{Commit: "a", Domains: []string{"x"}},
		{Commit: "b", Domains: []string{"x", "y", "z"}},
		{Commit: "c", Domains: []string{"x", "y", "z", "w"}},
	})
	if spread.MedianDomains != 3 {
		t.Errorf("median = %v, want 3", spread.MedianDomains)
	}
	if spread.Widest != 4 {
		t.Errorf("widest = %d, want 4", spread.Widest)
	}
}

// Even counts take the upper of the two middle values, not their average.
// The report prints "domains per commit" as a whole number, and 1.5
// domains is not a thing anyone can act on; the convention is pinned here
// so a later "fix" to a true median is a decision rather than a drift.
func TestTheMedianTakesTheUpperMiddleOnAnEvenCount(t *testing.T) {
	r := summarise([]Change{
		{Commit: "a", Domains: []string{"x"}},
		{Commit: "b", Domains: []string{"x", "y", "z"}},
	})
	if r.MedianDomains != 3 {
		t.Errorf("median = %v, want the upper middle value 3", r.MedianDomains)
	}
}

func TestACommitThatTouchedNothingInTheControlPlaneIsNotCounted(t *testing.T) {
	r := summarise([]Change{
		{Commit: "a", Domains: []string{"aiops"}},
		{Commit: "b", Domains: nil},
	})
	if r.Touching != 1 {
		t.Errorf("Touching = %d, want 1", r.Touching)
	}
}

// A ratio of 50% reads the same whether a domain was worked on all quarter
// or refactored in one sitting. The run count is what tells them apart, and
// a reader who only sees the ratio will over-read every single-run domain.
func TestSoloRunsSeparatesOnePushFromAQuarterOfWork(t *testing.T) {
	cases := []struct {
		name        string
		positions   []int
		wantRuns    int
		wantLongest int
	}{
		{name: "nothing at all", positions: nil, wantRuns: 0, wantLongest: 0},
		{name: "one commit", positions: []int{4}, wantRuns: 1, wantLongest: 1},
		{name: "one unbroken push", positions: []int{3, 4, 5, 6, 7}, wantRuns: 1, wantLongest: 5},
		{name: "one push and a later one", positions: []int{3, 4, 5, 9}, wantRuns: 2, wantLongest: 3},
		{name: "all separate", positions: []int{1, 4, 7, 10}, wantRuns: 4, wantLongest: 1},
		// The order git hands commits back in must not change the answer.
		{name: "unsorted input is sorted first", positions: []int{9, 5, 4, 3}, wantRuns: 2, wantLongest: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs, longest := soloRuns(tc.positions)
			if runs != tc.wantRuns || longest != tc.wantLongest {
				t.Fatalf("soloRuns(%v) = %d runs, longest %d; want %d runs, longest %d",
					tc.positions, runs, longest, tc.wantRuns, tc.wantLongest)
			}
		})
	}
}

// A domain created inside the measured window scores 100% solo for free:
// there was nothing to co-change with while it did not exist. Without this
// correction, "written once" is indistinguishable from "ships on its own".
func TestSummariseMarksSoloWorkThatIsOnlyTheDomainBeingBuilt(t *testing.T) {
	r := summarise([]Change{
		// newguy is born here and its opening three commits touch only it.
		{Commit: "1", Domains: []string{"newguy"}},
		{Commit: "2", Domains: []string{"newguy"}},
		{Commit: "3", Domains: []string{"newguy"}},
		{Commit: "4", Domains: []string{"newguy", "edge"}},
		// veteran is born already wired to edge, then goes quiet, and only
		// comes back for two commits of its own.
		{Commit: "5", Domains: []string{"veteran", "edge"}},
		{Commit: "6", Domains: []string{"unrelated"}},
		{Commit: "7", Domains: []string{"veteran"}},
		{Commit: "8", Domains: []string{"veteran"}},
	})

	byName := map[string]Solo{}
	for _, s := range r.Solo {
		byName[s.Domain] = s
	}

	if got := byName["newguy"]; got.Built != got.Solo || got.Solo != 3 {
		t.Errorf("newguy = %+v, want Built == Solo == 3 (all of it is construction)", got)
	}
	// veteran's first change touched edge, and it stayed quiet for a
	// commit, so its opening run is one commit long. The two solo commits
	// land after that, on a domain that already existed: real standalone
	// work, not construction.
	if got := byName["veteran"]; got.Built != 0 || got.Solo != 2 {
		t.Errorf("veteran = %+v, want Built == 0 and Solo == 2", got)
	}
}

// A commit that was part of building a domain does not stop being
// construction because it happened to touch a second domain.
func TestFirstRunCountsTheWholeOpeningStretchNotJustTheSoloPart(t *testing.T) {
	in := firstRun([]int{2, 3, 4, 9})
	want := map[int]bool{2: true, 3: true, 4: true}
	if len(in) != len(want) {
		t.Fatalf("firstRun = %v, want %v", in, want)
	}
	for pos := range want {
		if !in[pos] {
			t.Fatalf("firstRun is missing %d: %v", pos, in)
		}
	}
	if in[9] {
		t.Fatalf("firstRun swallowed the later change at 9: %v", in)
	}
	if got := countIn([]int{2, 3, 9}, in); got != 2 {
		t.Fatalf("countIn = %d, want 2", got)
	}
	if got := firstRun(nil); len(got) != 0 {
		t.Fatalf("firstRun(nil) = %v, want empty", got)
	}
}

// A commit that touched no domain is outside the study, not a gap in a
// domain's history. Letting it break a run would report a single campaign
// as several, which is the error this whole section exists to prevent.
func TestACommitTouchingNoDomainDoesNotBreakACampaign(t *testing.T) {
	withGap := summarise([]Change{
		{Commit: "1", Domains: []string{"aiops"}},
		{Commit: "2", Domains: nil}, // docs-only
		{Commit: "3", Domains: []string{"aiops"}},
	})
	byName := map[string]Solo{}
	for _, s := range withGap.Solo {
		byName[s.Domain] = s
	}
	if got := byName["aiops"]; got.Runs != 1 || got.LongestRun != 2 {
		t.Errorf("aiops = %+v, want 1 run of 2 — a docs commit is not a day", got)
	}
	if withGap.Touching != 2 {
		t.Errorf("Touching = %d, want 2 (the no-domain commit is not in the study)", withGap.Touching)
	}
}

// --- the calendar axis -------------------------------------------------
//
// The run count answers "how many stretches of consecutive commits", and a
// run breaks on any commit in between -- including one that touched some
// other domain entirely. So six runs can be one afternoon. The day count is
// the axis that cannot be fooled that way, and the split proposal's whole
// question ("is this domain worked on alone, or was it refactored in one
// sitting") is a question about days.

func onDay(commit, day string, domains ...string) Change {
	when, err := time.Parse("2006-01-02", day)
	if err != nil {
		panic(err)
	}
	return Change{Commit: commit, Subject: commit, Domains: domains, When: when}
}

func TestDayShapeCountsDistinctDaysAndTheSpanBetweenThem(t *testing.T) {
	ordered := []Change{
		onDay("a", "2026-01-01", "x"),
		onDay("b", "2026-01-01", "x"),
		onDay("c", "2026-01-04", "x"),
	}
	days, span := dayShape(ordered, []int{0, 1, 2})
	if days != 2 {
		t.Errorf("distinct days = %d, want 2: two commits on one day are one day of work", days)
	}
	if span != 3 {
		t.Errorf("span = %d day(s), want 3", span)
	}
	if days, span := dayShape(ordered, nil); days != 0 || span != 0 {
		t.Errorf("an empty set reported %d day(s) over %d day(s), want 0 and 0", days, span)
	}
}

// The case that decided the axis. Six solo commits for one domain, each
// separated by a commit belonging to another domain, so the log shows six
// runs -- and all six happened on the same day. The old report would have
// read that as recurrence.
func TestSixRunsInsideOneDayIsNotRecurrence(t *testing.T) {
	var changes []Change
	for i := 0; i < 3; i++ {
		changes = append(changes,
			onDay("solo-a", "2026-03-04", "aiops"),
			onDay("other", "2026-03-04", "edge"))
	}
	r := summarise(changes)
	var solo *Solo
	for i := range r.Solo {
		if r.Solo[i].Domain == "aiops" {
			solo = &r.Solo[i]
		}
	}
	if solo == nil {
		t.Fatal("aiops is missing from the report")
	}
	if solo.Runs != 3 {
		t.Errorf("runs = %d, want 3 (each solo commit is separated by the other's)", solo.Runs)
	}
	if solo.Days != 1 {
		t.Errorf("days touched = %d, want 1: every one of those commits is the same day", solo.Days)
	}
	if solo.SpanDays != 0 {
		t.Errorf("span = %d day(s), want 0", solo.SpanDays)
	}

	var out bytes.Buffer
	printReport(&out, r)
	if !strings.Contains(out.String(), "ONE DAY, not a habit") {
		t.Errorf("the report does not say the work was one day:\n%s", out.String())
	}
}

// Spread over real days, the same ratio is the opposite claim.
func TestTheSameRatioSpreadOverDaysIsNotOneDay(t *testing.T) {
	var changes []Change
	for i := 0; i < 3; i++ {
		changes = append(changes,
			onDay("solo", "2026-03-0"+string(rune('1'+i)), "aiops"),
			onDay("other", "2026-03-0"+string(rune('1'+i)), "edge"))
	}
	r := summarise(changes)
	for _, s := range r.Solo {
		if s.Domain != "aiops" {
			continue
		}
		if s.Days != 3 || s.SpanDays != 2 {
			t.Errorf("days = %d, span = %d; want 3 days over a 2-day span", s.Days, s.SpanDays)
		}
	}
	var out bytes.Buffer
	printReport(&out, r)
	// The verdict phrase, not the bare words: the limits section explains
	// what ONE DAY means, so the report always contains the phrase.
	if strings.Contains(out.String(), "ONE DAY, not a habit") {
		t.Errorf("work spread over three days was reported as one day:\n%s", out.String())
	}
}

// The window is printed before any ratio, because a ratio without it is the
// more comfortable half of the same sentence.
func TestTheWindowIsReportedAndShortOnesAreCalledShort(t *testing.T) {
	r := summarise([]Change{
		onDay("a", "2026-05-01", "aiops"),
		onDay("b", "2026-05-01", "edge"),
	})
	var out bytes.Buffer
	printReport(&out, r)
	if r.WindowFrom != "2026-05-01" || r.WindowTo != "2026-05-01" || r.WindowDays != 0 {
		t.Errorf("window = %s..%s over %d day(s), want a single day", r.WindowFrom, r.WindowTo, r.WindowDays)
	}
	text := out.String()
	if !strings.Contains(text, "window: 2026-05-01 to 2026-05-01") {
		t.Errorf("the report does not state the window:\n%s", text)
	}
	if !strings.Contains(text, "cannot support any claim about how a domain") {
		t.Errorf("a one-day window was not called too short to read as shipping behaviour:\n%s", text)
	}
	if strings.Index(text, "window:") > strings.Index(text, "changed alone") {
		t.Error("the window is printed after the ranking, so the ranking is read first")
	}
}

// The per-domain view is the thing a reviewer actually opens: which days, in
// which runs, and what else was in the control plane between them.
func TestSoloDetailPrintsTheDaysAndSaysWhenThereIsNothingToShow(t *testing.T) {
	changes := []Change{
		onDay("c1", "2026-04-01", "aiops"),
		onDay("c2", "2026-04-01", "aiops", "edge"),
		onDay("c3", "2026-04-09", "aiops"),
	}
	var out bytes.Buffer
	if !soloDetail(&out, changes, "aiops") {
		t.Fatal("soloDetail reported no domain for one that has commits")
	}
	text := out.String()
	for _, want := range []string{"2 solo of 3", "2026-04-01", "2026-04-09", "2 run(s)", "2 distinct day(s)"} {
		if !strings.Contains(text, want) {
			t.Errorf("the per-domain view is missing %q:\n%s", want, text)
		}
	}

	out.Reset()
	if soloDetail(&out, changes, "nothing-here") {
		t.Error("soloDetail claimed to have detail for a domain no commit touched")
	}
	if !strings.Contains(out.String(), "no commit in this window touched nothing-here") {
		t.Errorf("an absent domain produced no explanation:\n%s", out.String())
	}
}

// The window crossing seven days is the moment the third question -- which
// domains actually ship on their own -- stops being unanswerable for lack of
// history. Before this, maturity was inferred from an absent caveat, which is
// indistinguishable from nobody having checked. So both branches must be
// explicit, and exactly one of them must speak.
func TestAShortWindowSaysTheRatiosCannotCarryAClaimAboutShipping(t *testing.T) {
	var buf bytes.Buffer
	printReport(&buf, Report{Commits: 164, Touching: 164, WindowFrom: "2026-10-02", WindowTo: "2026-10-07", WindowDays: 4})
	out := buf.String()
	if !strings.Contains(out, "cannot support any claim") {
		t.Fatalf("a four day window was reported without saying so:\n%s", out)
	}
	if strings.Contains(out, "long enough for the ratios") {
		t.Fatalf("a four day window was reported as mature:\n%s", out)
	}
}

func TestAWindowLongEnoughSaysSoOutLoud(t *testing.T) {
	var buf bytes.Buffer
	printReport(&buf, Report{Commits: 400, Touching: 400, WindowFrom: "2026-09-20", WindowTo: "2026-10-07", WindowDays: 17})
	out := buf.String()
	if !strings.Contains(out, "long enough for the ratios") {
		t.Fatalf("a seventeen day window did not announce itself as readable:\n%s", out)
	}
	// Maturity is not a verdict, and the report must not start sounding like one.
	if !strings.Contains(out, "still needs a person") {
		t.Fatalf("a mature window claimed the question answered itself:\n%s", out)
	}
	if strings.Contains(out, "cannot support any claim") {
		t.Fatalf("a mature window still carries the short-window caveat:\n%s", out)
	}
}

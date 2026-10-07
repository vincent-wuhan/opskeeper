package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestTheSameNameSeveralOwnersListIsReal is the assertion behind the report's
// most useful column.
//
// Four symbols in this tree were selected by more than one domain and reached
// by more than one: `Event`, `Rule`, `Usecase`, `Caller`. Each of those was two
// or more unrelated types that happen to share a name — alert.Event is not
// audit.Event, marketplace.Caller carries a TenantID that skill.Caller does
// not. **None of them are still true**, and the note below says how each left,
// because there are three distinct ways out and they call for opposite next
// steps: a name can lose its second target while staying in the report, lose
// its second consumer and leave the report entirely, or — the one this file
// was written to prevent — get merged by somebody who read the ranking.
//
// That makes them the most dangerous entries in the shared-symbol list, and
// the reason a shared-symbol ranking cannot be acted on by sorting it. A move
// that carries "Caller" to a shared home without noticing it has several
// owners does not fail to compile; it compiles, and it silently changes what
// one of the owners means. Nothing downstream can see it.
//
// So the list is pinned. The assertion is that these are still ambiguous, so
// that the day someone gives one of them a single owner this test goes red and
// the name is promoted to a candidate — and, just as importantly, so nobody
// reads the current ranking and concludes the top of the list is safe to move.
//
// `ListFilter` was the fifth name here until decision 235 cut alert -> edge
// and systemhealth -> edge, which removed the only two consumers besides
// aiops. A symbol needs two or more *consuming* domains to be reported at
// all, so ListFilter left the report rather than becoming single-owner. That
// is a different end state from the one this test was written to catch, and
// the failure message below says which one happened — the earlier version
// reported both as "now has a single declaring domain", which was wrong for
// the case that actually occurred.
//
// Decision 259 removed the last two, and both need the word "targets" pinned
// down first, because reading it wrong here is how a reader concludes a
// parallel copy went away when it did not. `targets` is the set of domains the
// CONSUMERS reach into, not a census of who declares the name anywhere in the
// tree: a domain selecting its own type is skipped (from == to), so a parallel
// copy living inside a consuming domain is invisible to this column.
//
//   - `Rule` went from three consumers and two targets to two consumers and
//     one. The consumer that made it ambiguous was the `aiopsconfig` domain,
//     which reached into `aiops` for `alertconfig.Rule` — the parallel copy
//     that `service/alert.Rule` also answers to. Moving that adapter to the
//     composition root removed the only path that put the two in front of one
//     consumer at the same time. **The copy is still there and is still used
//     inside aiops**; what changed is that no consumer is now in a position to
//     confuse them. That is a real improvement, and it is not the same thing as
//     the ambiguity being settled, so the name is unpinned rather than promoted
//     to a move candidate.
//   - `Usecase` left the report entirely at decision 281, and for the ordinary
//     reason: its second consuming domain was `frontierbound`, and the cut
//     removed the last name it shared with the edge domain. A symbol needs two
//     or more *consuming* domains to be reported at all. What that does NOT
//     mean is what it says — see TestTheUsecaseDeclarationsAreUnrelatedStructs,
//     which pins the finding directly because the column stopped showing it.
//   - `Event` is the third, and it is the same shape as `Rule`: decision 280
//     cut `imbridge -> aiops`, and the two domains that had been selecting two
//     different `Event` declarations were `aiops` and `demo`. After the cut
//     both reach the same one, so there is no consumer left in a position to
//     confuse two same-named types. **The other declaration is still there** —
//     what a cut removed is the path that put two in front of one consumer,
//     not a type. So the name is unpinned for the same reason `Rule` was, and
//     for the same reason that is not the same as "settled": a reader must not
//     read one target as evidence that a duplicate was deleted.
//   - `Caller` left the report entirely, for the ordinary reason: fewer than
//     two consuming domains. It was `aiopsconfig` and `imbridge`, and the
//     adapter was one of the two. Note what that does NOT mean — the
//     composition root still selects `service/alert.Caller`, but this graph is
//     built from core/manager alone and cannot see cmd/, so a move there
//     legitimately shrinks the report without shrinking the coupling. The same
//     blindness is stated in the release report; repeating it here is what stops
//     the next reader from reading "left the report" as "no longer coupled".
func TestTheSameNameSeveralOwnersListIsReal(t *testing.T) {
	// Decision 281 emptied this list, and the way it emptied is the third
	// distinct ending — which is why the list is asserted as empty rather
	// than deleted.
	//
	//   - `Event` (decision 280) stayed in the report and lost its second
	//     TARGET: two consumers, one declaration.
	//   - `Usecase` (decision 281) LEFT the report entirely. Its second
	//     consuming domain was `frontierbound`, and the cut removed the last
	//     name it shared with the edge domain. Fewer than two visible consumers
	//     is the report's own exit condition, and the type is still declared by
	//     many unrelated packages.
	//
	// So the trap column is empty, and the findings behind it are not. They
	// moved to direct pins, the way `ListFilter` did at decision 235:
	// TestTheUsecaseDeclarationsAreUnrelatedStructs below, and
	// TestTheThreeListFilterDeclarationsAreStillThreeVocabularies further down.
	//
	// This test therefore no longer pins a list. It pins the emptiness, and it
	// fails on purpose if the column ever fills again — because a name landing
	// back in the trap column is a new fact about who selects what, and nobody
	// should read that as drift.
	ambiguous := map[string]bool{}
	reported := map[string]bool{}
	for _, r := range sharedRows(t) {
		reported[r.sym] = true
		if len(r.targets) > 1 {
			ambiguous[r.sym] = true
		}
	}
	if len(ambiguous) != 0 {
		t.Fatalf("the trap column has %d entries (%v) after decision 281 emptied it. That is a "+
			"change in who selects what, not drift: either a new ambiguity is real and belongs "+
			"in the report's warning column with its shapes, or a cut has to say which of the "+
			"three endings below it was", len(ambiguous), keysOfBool(ambiguous))
	}
	// Emptying the column must not have emptied the report with it. A report
	// that finds nothing reads exactly like a report that stopped working, and
	// the reason the column can be empty is that the main list is not.
	if len(reported) == 0 {
		t.Fatal("no shared symbol is selected by more than one domain at all; the trap column is " +
			"empty because the report is, not because the ambiguity is")
	}
}

func keysOfBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTheReportFindsSharedSymbols keeps the report from silently going empty.
// A report that finds nothing reads exactly like a report that stopped
// working, and the conclusion anyone would draw from the empty version — "no
// shape is worth moving" — is the one this test exists to prevent being drawn
// by accident.
func TestTheReportFindsSharedSymbols(t *testing.T) {
	rows := sharedRows(t)
	if len(rows) == 0 {
		t.Fatal("the shared report found no symbol selected by more than one domain; either the " +
			"tree stopped having shared vocabulary or the report stopped reading it")
	}
	// The heaviest entry in the current tree is selected by four domains. If
	// the ceiling collapses, the report has lost its input rather than the
	// tree having simplified, and a reader would see a much shorter list and
	// conclude the work is nearly done.
	max := 0
	for _, r := range rows {
		if len(r.consumers) > max {
			max = len(r.consumers)
		}
	}
	if max < 3 {
		t.Errorf("the heaviest shared symbol now has %d consumers, down from 4 when this was "+
			"written; a collapsed ceiling is more likely a broken report than a simplified tree", max)
	}
}

type sharedSummary struct {
	sym       string
	consumers map[string]bool
	targets   map[string]bool
	shapes    []string
}

func sharedRows(t *testing.T) []sharedSummary {
	t.Helper()
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("reading the shipped tree: %v", err)
	}
	var sb strings.Builder
	printShared(&sb, sources, defaultRules())
	var out []sharedSummary
	lines := strings.Split(sb.String(), "\n")
	for i := 0; i < len(lines); i++ {
		m := sharedRowRE.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		row := sharedSummary{
			sym:       m[2],
			consumers: setOf(m[3]),
			targets:   setOf(m[4]),
		}
		// The shape lines are indented under the row they belong to and carry
		// no leading number, so they are consumed by position: everything
		// indented deeper than the row, until the next row starts.
		for j := i + 1; j < len(lines); j++ {
			if !strings.HasPrefix(lines[j], "        ") || !strings.Contains(lines[j], ":") {
				break
			}
			row.shapes = append(row.shapes, strings.TrimSpace(lines[j]))
		}
		out = append(out, row)
	}
	return out
}

func setOf(field string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Fields(field) {
		out[s] = true
	}
	return out
}

var sharedRowRE = regexp.MustCompile(
	`^\s+(\d+)\s+(\S+)\s+consumers:\s+(.*?)\s+targets:\s+(.*?)(?:\s+<--.*)?$`)

// TestTheTrapColumnSaysWhyNotJustThat is the assertion behind the shapes the
// ambiguous rows print.
//
// A warning that only says "these two are different types" leaves the reader
// with a puzzle, and puzzles get skipped. The finding this report exists to
// deliver is the opposite: `Usecase` is not one shared shape under sixteen
// names, it is sixteen unrelated service structs — biz/edge's holds
// {repo devices links mirror plugins log phMu pluginHealth}, biz/audit's holds
// {repo log chain chainStore}, biz/report's holds {repo read gen idGen
// defaultLocale}. They agree on the word "repo" and on nothing else. Once that
// is printed, the row stops looking like a move candidate and starts looking
// like a coincidence, which is the correct reading and the one a ranking by
// consumer count would never have produced.
//
// The subject used to be `ListFilter`, and moved here when decision 235 cut
// the two consumers that kept it in the report. The finding it guarded did
// not become false when the report stopped showing it — three packages still
// declare three unrelated `ListFilter` vocabularies — so that half is now
// pinned directly, in the test below, rather than through a column that no
// longer prints it.
func TestTheUsecaseDeclarationsAreUnrelatedStructs(t *testing.T) {
	// This finding used to be carried by the trap column and no longer is, for
	// the reason recorded above: `Usecase` left the report when decision 281
	// removed `frontierbound` as a second consuming domain. The finding itself
	// is untouched by that — `Usecase` is still not one shared shape under many
	// names, it is many unrelated service structs that agree on the word
	// "repo" and on nothing else.
	//
	// Asserted against the parsed declarations rather than the report, exactly
	// as the ListFilter pin below does it. The report's silence is a fact about
	// who consumes a symbol; this is a fact about who declares it, and only the
	// first one moved. Asserting it through the column would have made the
	// finding disappear the moment the last consumer went away, which is the
	// opposite of what a trap column is for.
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the control plane: %v", err)
	}
	owners := map[string][]string{}
	for pkg, syms := range collectStructFields(sources) {
		if f, ok := syms["Usecase"]; ok {
			owners[pkg] = f
		}
	}
	if len(owners) < 2 {
		t.Fatalf("Usecase is declared by %d package(s), want the several unrelated service structs "+
			"it has always had; if they were merged that is a decision to record, not a drift to "+
			"absorb", len(owners))
	}
	// The owners must be visibly different. Comparing the field list alone, not
	// the package path: two owners with identical fields would still be two
	// declarations, and a reader who merged them would be wrong either way —
	// but a report that printed the same struct three times under three names
	// would be printing something false, and that is what this catches.
	shapes := map[string]bool{}
	for pkg, f := range owners {
		shapes[strings.Join(f, ",")+" @"+pkg] = true
	}
	for _, f := range owners {
		shapes[strings.Join(f, ",")] = true
	}
	if len(shapes) < len(owners) {
		t.Errorf("the %d Usecase owners do not all differ: %v. A reader would merge types that "+
			"are not alike", len(owners), keysOfBool(shapes))
	}
	// And the specific shape that makes the point: at most one owner of Usecase
	// holds an audit chain, which is what a single shared service struct would
	// look like if it existed. biz/audit is the one that has `chain`.
	withChain := 0
	for _, f := range owners {
		for _, name := range f {
			if name == "chain" {
				withChain++
				break
			}
		}
	}
	if withChain > 1 {
		t.Errorf("%d owners of Usecase carry an audit chain; they are documented as unrelated, "+
			"so either the tree changed or the premise of this file is wrong", withChain)
	}
}

// TestTheThreeListFilterDeclarationsAreStillThreeVocabularies pins the finding
// decision 233 recorded, now that the report can no longer show it.
//
// The report only prints a symbol two or more domains *select*, and after
// decision 235 only aiops still selects `ListFilter`. So the column that used
// to carry this warning is silent — and the warning was not about the report.
// It is about the tree: three packages declare a type with the same name and
// unrelated fields, and a future move that treats them as one type will
// compile and change what one of them means.
//
// Asserted against the parsed declarations rather than the report, because
// the report's silence is a property of who consumes a symbol and this is a
// property of who declares it. Those are different facts and only one of them
// moved.
func TestTheThreeListFilterDeclarationsAreStillThreeVocabularies(t *testing.T) {
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the control plane: %v", err)
	}
	fields := collectStructFields(sources)
	owners := map[string][]string{}
	for pkg, syms := range fields {
		if f, ok := syms["ListFilter"]; ok {
			owners[pkg] = f
		}
	}
	if len(owners) < 3 {
		t.Fatalf("ListFilter is declared by %d package(s), want the 3 unrelated vocabularies it "+
			"has always had; if two of them were merged that is a decision to record, not a drift "+
			"to absorb", len(owners))
	}
	// The only field the three are documented to share is Limit. Asserting the
	// intersection is empty of everything else is stronger than asserting the
	// three differ, and it is the form that catches a merge: give one owner a
	// field the others have and this goes red.
	shared := map[string]bool{}
	for _, f := range owners {
		for _, name := range f {
			shared[name] = true
		}
	}
	common := map[string]bool{}
	for name := range shared {
		inAll := true
		for _, f := range owners {
			found := false
			for _, x := range f {
				if x == name {
					found = true
					break
				}
			}
			if !found {
				inAll = false
				break
			}
		}
		if inAll {
			common[name] = true
		}
	}
	for name := range common {
		if name != "Limit" {
			t.Errorf("every ListFilter owner now carries %q, so the three have stopped being "+
				"unrelated vocabularies and this file's premise is stale", name)
		}
	}
	if !common["Limit"] {
		t.Error("the three ListFilter owners no longer share Limit; the shape of the coincidence " +
			"changed and whoever reads this should say whether the two still look alike for a " +
			"different reason")
	}
}

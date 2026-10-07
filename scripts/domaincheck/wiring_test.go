package main

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// wiring_test.go guards the release floor against its own closed world.
//
// The floor says a domain with no inbound cross-domain import can be released
// without coordinating with any other bounded context, and it prints that as
// "releasing it breaks nobody's build". The graph behind it is built from
// core/manager alone, so the composition roots under cmd/ were never in the
// room when that sentence was written — and 27 of the floor's 28 rows are
// wired there. The claim was false for all but one of them.
//
// The fix is to read the outside tree and say what it costs. These tests exist
// because the first version of that fix could plausibly have been wrong in a
// way that looks right: domaincheck and modulecheck both spell the manager's
// import prefix as a string constant, so any scan that greps for the prefix
// instead of parsing imports reports this checker as a dependent of all 58
// domains. That is not a subtle error — it is a number four times too large.

// scanWiringUnder recomputes, for one top-level directory, which manager
// domains the Go files there import. It is deliberately a second implementation
// of what wiringUse does: same parser, written twice, so a bookkeeping slip in
// one does not have to be believed by the other.
func scanWiringUnder(t *testing.T, root, top string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]bool{}
	dir := filepath.Join(root, top)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			v, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if d := domainOf(v); d != "" {
				out[d] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", top, err)
	}
	return out
}

// TestTheWiringScanAgreesWithTheTreeOnWhatTheCompositionRootImports is the
// gate proper: the same fact, computed twice, compared.
//
// It was checked against `go list -f '{{.Imports}}' ./cmd/opskeeper` while it
// was being written, and the two agreed on all 54 domains with no difference
// in either direction. That oracle is not available inside a test without
// shelling out to the toolchain on every run, so the comparison kept here is
// against a second reading of the same files.
func TestTheWiringScanAgreesWithTheTreeOnWhatTheCompositionRootImports(t *testing.T) {
	wiring, err := wiringUse("../..")
	if err != nil {
		t.Fatalf("wiringUse: %v", err)
	}
	got := map[string]bool{}
	for domain, files := range wiring {
		for f := range files {
			if strings.HasPrefix(f, "cmd/opskeeper/") {
				got[domain] = true
			}
		}
	}
	want := scanWiringUnder(t, "../..", "cmd/opskeeper")
	if len(got) != len(want) {
		t.Fatalf("the composition root wires %d domains, a second reading of the same files\n"+
			"says %d.  only-wiringUse: %v\n  only-rescan: %v",
			len(got), len(want), diffDomains(got, want), diffDomains(want, got))
	}
	for d := range want {
		if !got[d] {
			t.Fatalf("domain %q is imported by cmd/opskeeper and the wiring scan missed it", d)
		}
	}
	if len(got) == 0 {
		t.Fatal("the scan found nothing; a test that cannot fail is not a test")
	}
}

func diffDomains(a, b map[string]bool) []string {
	var out []string
	for d := range a {
		if !b[d] {
			out = append(out, d)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// TestACheckerNamingTheImportPrefixIsNotADependentOfAnything pins the false
// positive that made a text scan unusable.
//
// This file, and modulecheck's main, both hold the manager's import prefix as
// string constants — they have to, to recognise the tree they check. A scan
// that searched for the prefix as text would therefore report domaincheck as a
// dependent of every domain in the tree, and the number it produced would have
// been roughly four times the truth while looking entirely reasonable.
func TestACheckerNamingTheImportPrefixIsNotADependentOfAnything(t *testing.T) {
	wiring, err := wiringUse("../..")
	if err != nil {
		t.Fatalf("wiringUse: %v", err)
	}
	for _, self := range []string{"scripts/domaincheck/main.go", "scripts/modulecheck/main.go"} {
		for domain, files := range wiring {
			if _, ok := files[self]; ok {
				t.Fatalf("%s is wired as a dependent of domain %q. That file names the manager's\n"+
					"import prefix as a constant; only a real import counts, and a scan that read\n"+
					"the prefix as text would have reported the checker as wiring all 58 domains.", self, domain)
			}
		}
	}
}

// TestTheFloorTellsTheReaderHowManyOfItsRowsAreWired keeps the caveat attached
// to the number it is about.
//
// The count lives in the prose, and prose is the part of a report nobody
// re-derives. If it were left to be filled in by hand it would keep its shape
// while describing a tree that no longer existed — the same failure the door
// headline had, and the reason that number is watched rather than trusted.
func TestTheFloorTellsTheReaderHowManyOfItsRowsAreWired(t *testing.T) {
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	g := buildGraph(sources, r)
	wiring, err := wiringUse("../..")
	if err != nil {
		t.Fatalf("wiringUse: %v", err)
	}
	g.wiring = wiring

	floor, _, _, _ := g.releaseTiers(r.shared)
	var wired int
	for _, d := range floor {
		if len(g.wiring[d]) > 0 {
			wired++
		}
	}
	var buf bytes.Buffer
	g.printReleaseFloor(&buf, r.shared)
	out := buf.String()

	want := fmt.Sprintf("%d of the %d below are wired there anyway", wired, len(floor))
	if !strings.Contains(out, want) {
		t.Fatalf("the report must state how many of its own rows are wired from outside.\n"+
			"  looked for: %q", want)
	}
	if wired == 0 {
		t.Fatal("no floor domain is wired, so this test is asserting a coincidence rather than a rule")
	}
}

// TestTheWiredCountMovesWhenTheWiringMoves is the drift guard, for the same
// reason the door headline needed one: a number pasted into a format string
// produces byte-identical output and stays green.
func TestTheWiredCountMovesWhenTheWiringMoves(t *testing.T) {
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	base := buildGraph(sources, r)
	wiring, err := wiringUse("../..")
	if err != nil {
		t.Fatalf("wiringUse: %v", err)
	}
	base.wiring = wiring
	floor, _, _, _ := base.releaseTiers(r.shared)

	// Find a floor domain the tree says is NOT wired, and wire it.
	var victim string
	for _, d := range floor {
		if len(base.wiring[d]) == 0 {
			victim = d
			break
		}
	}
	if victim == "" {
		t.Skip("every floor domain is already wired; there is nothing to move")
	}

	var before bytes.Buffer
	base.printReleaseFloor(&before, r.shared)
	beforeWired := wiredCountOf(before.String())

	flipped := buildGraph(sources, r)
	flipped.wiring = wiring
	flipped.wiring[victim] = map[string]int{"cmd/opskeeper/main.go": 1}
	var after bytes.Buffer
	flipped.printReleaseFloor(&after, r.shared)

	if got := wiredCountOf(after.String()); got != beforeWired+1 {
		t.Fatalf("wiring %s must move the printed count up by one, because a count that cannot\n"+
			"follow a change is a literal in a format string. before %d, after %d",
			victim, beforeWired, got)
	}
}

func wiredCountOf(out string) int {
	i := strings.Index(out, " below are wired there anyway")
	if i < 0 {
		return -1
	}
	fields := strings.Fields(out[:i])
	if len(fields) < 4 {
		return -1
	}
	n, err := strconv.Atoi(fields[len(fields)-4])
	if err != nil {
		return -1
	}
	return n
}

// TestTheReportMayNotSayNothingBreaksWhenSomethingIsWired is the assertion
// that the old sentence is gone.
//
// It is here as a test rather than as a review note because the sentence was
// true of the graph and false of the repository, which is exactly the kind of
// claim a reader cannot check by looking. If someone edits the wording back,
// this goes red.
func TestTheReportMayNotSayNothingBreaksWhenSomethingIsWired(t *testing.T) {
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	g := buildGraph(sources, r)
	wiring, err := wiringUse("../..")
	if err != nil {
		t.Fatalf("wiringUse: %v", err)
	}
	g.wiring = wiring

	var buf bytes.Buffer
	g.printReleaseFloor(&buf, r.shared)
	out := buf.String()

	if strings.Contains(out, "releasing it breaks nobody's build") {
		t.Fatal("the report still claims that releasing a floor domain breaks nobody's build, which\n" +
			"is false for every row the wiring column marks as wired")
	}
	if !strings.Contains(out, "The claim is about bounded contexts only") {
		t.Fatal("the report must narrow the claim to bounded contexts when anything outside core/manager\n" +
			"is wiring the floor")
	}
}

// TestTheWiringColumnPricesTheAssemblyEdit pins the column's own contract: a
// domain nothing outside wires is the one row that is free, and a domain that
// is wired says how much of the edit there is.
func TestTheWiringColumnPricesTheAssemblyEdit(t *testing.T) {
	free := (&domainGraph{}).wiringColumn("anything")
	if free != "not wired outside core/manager" {
		t.Fatalf("an unwired domain is the one row in this report that is free to extract, and it\n"+
			"must say so plainly; got %q", free)
	}

	one := (&domainGraph{wiring: map[string]map[string]int{
		"d": {"cmd/opskeeper/main.go": 1},
	}}).wiringColumn("d")
	if !strings.HasPrefix(one, "wired: 1 import, 1 file in cmd") {
		t.Fatalf("one import in one file must read in the singular, got %q", one)
	}

	many := (&domainGraph{wiring: map[string]map[string]int{
		"d": {"cmd/opskeeper/main.go": 3, "cmd/opskeeper/eval.go": 2, "scripts/x.go": 1},
	}}).wiringColumn("d")
	if !strings.Contains(many, "6 imports, 3 files in cmd, scripts") {
		t.Fatalf("the column must total the imports and name every top-level place, sorted; got %q", many)
	}
}

// TestATestFileIsNotADependent pins a rule the real tree cannot demonstrate.
//
// A scan that counted _test.go files would be wrong in principle and turned
// out to be unobservable here: every manager domain that a test file under
// cmd/ imports is also imported by a non-test file beside it, so the set came
// out identical and every test stayed green. That is the worst kind of gap —
// a wrong rule with a passing witness — and it is why this one is written
// against a fixture where the test file is the only importer.
//
// The fixture needs no manager files at all: domainOf is a lexical reading of
// an import path, so a file that imports a manager package is a dependent of
// that domain whether or not the package exists on disk.
func TestATestFileIsNotADependent(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cmd/app/main.go": `package main

import "%Mbiz/logs"

var _ = logs.New
`,
		"cmd/app/main_test.go": `package main

import "%Mbiz/iam"

var _ = iam.New
`,
	})
	wiring, err := wiringUse(root)
	if err != nil {
		t.Fatalf("wiringUse: %v", err)
	}
	if _, ok := wiring["logs"]; !ok {
		t.Fatal("a non-test file importing a manager domain must be recorded as a dependent")
	}
	if files, ok := wiring["iam"]; ok {
		t.Fatalf("iam is imported by a test file and nothing else, so nothing ships against it;\n"+
			"not a release dependent, and counting it would make a domain look wired that nobody\n"+
			"ships against. got %v", files)
	}
}

// cutFixture is a two-domain tree with an external composition root, which is
// the smallest shape that can show all three prices at once.
func cutFixture(t *testing.T) *domainGraph {
	t.Helper()
	root := writeTree(t, map[string]string{
		"biz/a/a.go": `package a

type A struct{}
`,
		"biz/b/b.go": `package b

import "%Mbiz/a"

var _ = a.A{}
`,
	})
	sources, _, err := parseTree(root, managerPrefix, rules{})
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	g := buildGraph(sources, rules{})
	g.wiring = map[string]map[string]int{
		"a": {"cmd/app/main.go": 30},
		"b": {"cmd/app/main.go": 2, "cmd/other/main.go": 1},
	}
	return g
}

func cutOutput(t *testing.T, g *domainGraph, body string) string {
	t.Helper()
	grouping, order, err := loadGrouping(groupingFile(t, body))
	if err != nil {
		t.Fatalf("load the grouping: %v", err)
	}
	var buf bytes.Buffer
	g.printCut(&buf, grouping, order, nil)
	return buf.String()
}

// TestTheThirdPriceCountsEachGroupsWiredDomains pins the arithmetic of the
// section decision 219 added: per group, how many of its domains the outside
// world imports, in how many files, worth how many import statements.
func TestTheThirdPriceCountsEachGroupsWiredDomains(t *testing.T) {
	g := cutFixture(t)
	out := cutOutput(t, g, "left  = a\nright = b\n")

	if !strings.Contains(out, "1 of  1 domains wired from outside, across  1 file(s),  30 import(s)") {
		t.Fatalf("group left should carry a's 3 imports in one file:\n%s", out)
	}
	if !strings.Contains(out, "1 of  1 domains wired from outside, across  2 file(s),   3 import(s)") {
		t.Fatalf("group right should carry b's 2+1 imports across two files:\n%s", out)
	}
	if !strings.Contains(out, "2 wired domain(s) in total, 2 file(s) outside core/manager to edit, 33 import(s).") {
		t.Fatalf("the totals must add across groups:\n%s", out)
	}
	// The single file carrying most of the work is named, because "21 files to
	// edit" and "one 6606-line file is all of it" are different conversations.
	if !strings.Contains(out, "cmd/app/main.go") {
		t.Fatalf("the heaviest file must be named rather than left inside a count:\n%s", out)
	}
}

// TestASplitThatSeversNothingCanStillBeExpensiveOutsideTheTree is the reason
// the section exists at all.
//
// Both groupings below put a and b in the same group, so neither severs a
// single edge and neither moves a line of code. The first two prices are
// therefore both zero, and a report with only those two prices would call them
// free. They are not free: two domains have to be re-pointed in three files
// under cmd/. A price that reads zero for an operation with real work in it is
// worse than no price, because it is believed.
func TestASplitThatSeversNothingCanStillBeExpensiveOutsideTheTree(t *testing.T) {
	g := cutFixture(t)
	out := cutOutput(t, g, "one = a, b\n")

	if !strings.Contains(out, "0 cross one") {
		t.Fatalf("this grouping is meant to sever nothing, so the edge price must read zero:\n%s", out)
	}
	if !strings.Contains(out, "2 wired domain(s) in total, 2 file(s) outside core/manager to edit, 33 import(s).") {
		t.Fatalf("the composition-root price must not be zero just because the edge price is:\n%s", out)
	}
}

// TestTheThirdPriceIsAbsentWhenNothingIsWired keeps the section from printing
// an empty, alarming paragraph over a tree that has no composition roots.
func TestTheThirdPriceIsAbsentWhenNothingIsWired(t *testing.T) {
	root := writeTree(t, map[string]string{
		"biz/a/a.go": `package a

type A struct{}
`,
	})
	sources, _, err := parseTree(root, managerPrefix, rules{})
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	g := buildGraph(sources, rules{})
	out := cutOutput(t, g, "one = a\n")
	if strings.Contains(out, "the third price") {
		t.Fatalf("with nothing wired there is no third price to print, and an empty section reads\n"+
			"as a warning about a tree that has no problem:\n%s", out)
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestALiveFilterIsNotReported(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"."}, []string{"pkg/audit"})
	if err := os.WriteFile(filepath.Join(root, "pkg", "audit", "a_test.go"),
		[]byte("package audit\nfunc TestThePortIsFree(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := "check:\n\tcd . && GOWORK=off go test ./pkg/audit/ -count=1 -run 'TestThePortIsFree'\n"
	if err := checkRunFilters(root, mk, map[string]bool{"check": true}); err != nil {
		t.Fatalf("a filter that matches a live test was reported: %v", err)
	}
}

// Go matches -run unanchored, so a filter may be a prefix of a longer name.
// The first version of this file compared for equality and reported two of
// the repository's own filters as dead, which is the kind of false alarm
// that ends with the check deleted instead of the bug fixed.
func TestAPrefixOfALongerNameStillMatches(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"."}, []string{"pkg/audit"})
	if err := os.WriteFile(filepath.Join(root, "pkg", "audit", "a_test.go"),
		[]byte("package audit\nfunc TestThePortIsFreeOfEverything(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := "check:\n\tcd . && GOWORK=off go test ./pkg/audit/ -count=1 -run 'TestThePortIsFree'\n"
	if err := checkRunFilters(root, mk, map[string]bool{"check": true}); err != nil {
		t.Fatalf("a prefix filter was treated as an exact name: %v", err)
	}
}

func TestAFilterThatNamesNothingIsReported(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"."}, []string{"pkg/audit"})
	if err := os.WriteFile(filepath.Join(root, "pkg", "audit", "a_test.go"),
		[]byte("package audit\nfunc TestSomethingElseEntirely(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := "check:\n\tcd . && GOWORK=off go test ./pkg/audit/ -count=1 -run 'TestThePortIsFree'\n"
	err := checkRunFilters(root, mk, map[string]bool{"check": true})
	if err == nil {
		t.Fatal("a -run filter matching no test was not reported; the recipe asserts nothing and passes")
	}
	for _, want := range []string{"check", "TestThePortIsFree"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report does not name %q:\n%s", want, err)
		}
	}
}

// One alternative matching is enough. A filter is a disjunction, and reading
// it as a conjunction would report every gate that names several tests.
func TestOneMatchingAlternativeIsEnough(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"."}, []string{"pkg/audit"})
	if err := os.WriteFile(filepath.Join(root, "pkg", "audit", "a_test.go"),
		[]byte("package audit\nfunc TestTheSecondOne(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := "check:\n\tcd . && GOWORK=off go test ./pkg/audit/ -count=1 -run 'TestTheFirstOne|TestTheSecondOne'\n"
	if err := checkRunFilters(root, mk, map[string]bool{"check": true}); err != nil {
		t.Fatalf("a filter with one live alternative was reported: %v", err)
	}
}

// The module comes off the command's own line, not the filter's. The filter
// is nearly always on the line below, which is how the first version of this
// file resolved every recipe in the repository to the root module and
// reported a clean bill of health for the wrong directory.
func TestTheFilterOnTheNextLineStillResolvesItsOwnModule(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/base"}, []string{"core/base/pkg/audit"})
	if err := os.WriteFile(filepath.Join(root, "core", "base", "pkg", "audit", "a_test.go"),
		[]byte("package audit\nfunc TestThePortIsFree(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := "check:\n\tcd core/base && GOWORK=off go test ./pkg/audit/ -count=1 -run \\\n\t\t'TestThePortIsFree'\n"
	filters := goTestRunFilters(mk, map[string]bool{"check": true})
	if len(filters) != 1 {
		t.Fatalf("parsed %d filter(s), want 1", len(filters))
	}
	if filters[0].Module != "core/base" {
		t.Errorf("the filter resolved to module %q, want core/base; resolving it from the "+
			"filter's own line is the bug this test exists for", filters[0].Module)
	}
	if err := checkRunFilters(root, mk, map[string]bool{"check": true}); err != nil {
		t.Fatalf("a live filter in a nested module was reported: %v", err)
	}
}

// A recipe no workflow invokes compiles nothing, which is the mistake
// checkBuildTagCoverage already refuses to make one level up. Its filter
// naming nothing is therefore not a finding either.
func TestAFilterCINeverInvokesIsNotChecked(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"."}, []string{"pkg/audit"})
	if err := os.WriteFile(filepath.Join(root, "pkg", "audit", "a_test.go"),
		[]byte("package audit\nfunc TestSomethingElseEntirely(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := "check:\n\tcd . && GOWORK=off go test ./pkg/audit/ -count=1 -run 'TestThePortIsFree'\n"
	if err := checkRunFilters(root, mk, map[string]bool{}); err != nil {
		t.Fatalf("an unreachable recipe was checked: %v", err)
	}
}

// The two counters must agree, because the vacuity guard is the statement
// that they do. A Makefile that filters by name on a continued line is the
// shape most of this repository's gates use, and it is the shape where a
// counter and a parser drift apart without anyone noticing.
func TestTheWitnessCountAndTheParserAgree(t *testing.T) {
	src := "check:\n" +
		"\tcd core/base && GOWORK=off go test ./pkg/audit/ -count=1 -run \\\n" +
		"\t\t'TestOne|TestTwo'\n" +
		"\tcd core/domains && GOWORK=off go test ./model/audit/ -count=1 -run 'TestThree'\n" +
		"other:\n" +
		"\tcd core/base && GOWORK=off go test ./pkg/audit/ -count=1 -run 'TestFour'\n"
	reachable := map[string]bool{"check": true}
	raw := countRunFlags(src, reachable)
	parsed := len(goTestRunFilters(src, reachable))
	if raw != 2 {
		t.Errorf("the witness counted %d reachable -run recipes, want 2 (the one under `other` is "+
			"not in CI)", raw)
	}
	if parsed != raw {
		t.Errorf("the witness counted %d and the parser read %d; the vacuity guard is exactly the "+
			"claim that these are equal", raw, parsed)
	}
}

// The witness. A parser that reads 11 of the 23 recipes would report "0
// problems" for the 23, and the number would be indistinguishable from a
// clean repository. So the parsed count is pinned against a raw count of the
// flag in the file, which no parser is involved in producing.
func TestEveryRunFlagInTheRealMakefileIsParsed(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	reachable := reachableTargets(src, invokedTargets(string(ci)))

	rawCount := countRunFlags(src, reachable)
	parsed := 0
	for _, c := range goTestCommands(src) {
		if c.Target == "" || !reachable[c.Target] {
			continue
		}
		if c.Line < 0 {
			continue
		}
		lines := strings.Split(src, "\n")
		text := lines[c.Line]
		for j := c.Line + 1; j < len(lines) && continuationRE.MatchString(lines[j-1]); j++ {
			text += " " + lines[j]
		}
		if runRE.MatchString(text) {
			parsed++
		}
	}
	if parsed != rawCount {
		t.Fatalf("the Makefile has %d `go test -run` recipe(s) and the parser saw %d; the missing "+
			"%d would be reported as clean, which is the failure this test exists to prevent",
			rawCount, parsed, rawCount-parsed)
	}
	if rawCount < 10 {
		t.Fatalf("only %d `go test -run` recipes found; this repository filters by name in more "+
			"than a dozen gates, so a small count means the counting rule changed", rawCount)
	}
}

// And the same witness for the check as a whole, against the real repository.
func TestTheRealRepositoryHasNoVacuousFilter(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	reachable := reachableTargets(src, invokedTargets(string(ci)))
	if err := checkRunFilters(root, src, reachable); err != nil {
		t.Fatalf("a real gate filters by a name no test has: %v", err)
	}
}

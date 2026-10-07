package main

import (
	"strings"
	"testing"
)

// TestTheRealTableCarriesNoDuplicateKey is the load-bearing one.
//
// Every other check in this file compares the table against the tree, and a
// duplicate is invisible to all of them: `lookup` returns the **first** match,
// so the second row is never read, never judged stale, never orphaned — while
// the headline count silently includes it. Decision 339 left ten of them
// behind and the run still printed a clean 177.
func TestTheRealTableCarriesNoDuplicateKey(t *testing.T) {
	got := duplicateKeys(Verdicts)
	if len(got) != 0 {
		t.Fatalf("the table records %d key(s) more than once: %v", len(got), got)
	}
}

// TestLookupReturnsTheFirstRowSoASecondOneIsInvisible is the reason the check
// above has to exist. Without this, somebody reading `lookup` would reasonably
// conclude a duplicate is harmless.
func TestLookupReturnsTheFirstRowSoASecondOneIsInvisible(t *testing.T) {
	saved := Verdicts
	t.Cleanup(func() { Verdicts = saved })
	Verdicts = []Verdict{
		{File: "a.go", Route: "/x", Handler: "h.a", Backlog: ""},
		{File: "a.go", Route: "/x", Handler: "h.a", Backlog: "an older, now-wrong reason"},
	}
	key := routeKey("a.go", "/x", "h.a")
	got, ok := lookup(key)
	if !ok {
		t.Fatal("lookup missed a key that is present twice")
	}
	if got.Backlog != "" {
		t.Fatalf("lookup returned %q, want the first row — the second is simply never consulted", got.Backlog)
	}
	// …and that is exactly why a duplicate is a silent defect: the run is
	// clean, and the row nobody reads is still in the table.
	if res := (Result{}); !res.OK() {
		t.Fatal("a fresh Result is not OK")
	}
	if n := len(duplicateKeys(Verdicts)); n != 1 {
		t.Fatalf("duplicateKeys found %d, want 1", n)
	}
}

// TestADuplicateFailsTheRun: the check has to reach OK(), or it is a report
// nobody reads — the same failure the row it reports would have had.
func TestADuplicateFailsTheRun(t *testing.T) {
	res := Result{Duplicate: []string{"a.go /x h.a — 2 rows"}}
	if res.OK() {
		t.Fatal("a Result carrying a duplicate still reports OK")
	}
	// And a duplicate must not be able to pass by being the only field set:
	// the gate treats every one of these as fatal, not as a warning.
	for name, r := range map[string]Result{
		"missing":   {Missing: []string{"x"}},
		"stale":     {Stale: []string{"x"}},
		"orphan":    {Orphan: []string{"x"}},
		"gone":      {Gone: []string{"x"}},
		"slots":     {SlotMissing: []string{"x"}},
		"unscanned": {Unscanned: []string{"x"}},
	} {
		if r.OK() {
			t.Errorf("%s alone still reports OK — a gate that only fails on one of its findings is not a gate", name)
		}
	}
}

// TestDuplicateKeysReportTheCountRatherThanOneName: "you left one row behind"
// and "you left ten" are different bugs, and the count is the only thing in
// the message that distinguishes them.
func TestDuplicateKeysReportTheCountRatherThanOneName(t *testing.T) {
	got := duplicateKeys([]Verdict{
		{File: "a.go", Route: "/x", Handler: "h.a"},
		{File: "a.go", Route: "/x", Handler: "h.a"},
		{File: "a.go", Route: "/x", Handler: "h.a"},
		{File: "b.go", Route: "/y", Handler: "h.b"},
	})
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %v", len(got), got)
	}
	if !strings.Contains(got[0], "3 rows") {
		t.Fatalf("entry = %q, want it to say 3 rows", got[0])
	}
	if !strings.Contains(got[0], "a.go") {
		t.Fatalf("entry = %q, want it to name the file", got[0])
	}
}

// TestRunItselfFillsTheDuplicateField is the one that pins the wiring, because
// every other assertion here calls `duplicateKeys` or `OK` directly. If `Run`
// stopped consulting the table, this file would stay entirely green while the
// command it belongs to went back to printing a clean report over 187 rows.
func TestRunItselfFillsTheDuplicateField(t *testing.T) {
	saved := Verdicts
	t.Cleanup(func() { Verdicts = saved })
	// Clone one real verdict so the key also matches a route in the tree: the
	// duplicate must be caught by the table check alone, with nothing about
	// the tree involved.
	Verdicts = append(append([]Verdict{}, saved...), saved[0])

	res := Run(".")
	if len(res.Duplicate) != 1 {
		t.Fatalf("Run reported %d duplicates, want 1: %v", len(res.Duplicate), res.Duplicate)
	}
	if !strings.Contains(res.Duplicate[0], "2 rows") {
		t.Errorf("entry = %q, want it to say 2 rows", res.Duplicate[0])
	}
	if res.OK() {
		t.Fatal("a run over a table with a duplicate key reports OK")
	}
}

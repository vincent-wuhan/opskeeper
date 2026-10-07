package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proposal")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The shape the proposal actually has today: a header carrying the live price,
// then a comment block quoting older prices as history.
const currentHeadline = "# 定价：make split-cost FILE=docs/manager-split.proposed\n" +
	"#   → 50 条 import 留在组内，6 条跨组，最重的一条缝 3 条 import。\n"

func TestTheHeadlinePriceIsTheOneTheReaderTakesAway(t *testing.T) {
	path := write(t, currentHeadline+`# 决策 254 切边之后组内不变：**95 / 26**。
# 决策 257 切边之后跨组再降 2：**95 / 24**。
`)
	got, err := headlinePrice(path)
	if err != nil {
		t.Fatalf("headlinePrice: %v", err)
	}
	if want := (price{internal: 50, crossing: 6, seam: 3}); got != want {
		t.Fatalf("headline = %v, want %v", got, want)
	}
}

// This is the whole reason the checker reads the headline and not the file:
// decisions 249 and 257 are in the file in that order, so "the last number
// written" and "the current number" are different things. A checker that
// reached for the last one would fail a correct document.
func TestAStaleNumberInTheHistoryDoesNotFailTheCheck(t *testing.T) {
	path := write(t, "# 定价：x\n"+
		"#   → 50 条 import 留在组内，6 条跨组，最重的一条缝 3 条 import。\n"+
		"# 决策 249：**96 / 30**。决策 257：**95 / 24**。\n")
	got, err := headlinePrice(path)
	if err != nil {
		t.Fatalf("history entry was read as the headline: %v", err)
	}
	if want := (price{internal: 50, crossing: 6, seam: 3}); got != want {
		t.Fatalf("headline = %v, want %v", got, want)
	}
}

func TestAProposalThatStatesNoPriceIsRefusedRatherThanPassing(t *testing.T) {
	path := write(t, "# manager 拆分候选方案\n# 状态：已定价的候选。\n")
	_, err := headlinePrice(path)
	if err == nil {
		t.Fatal("a document with no headline price was accepted; that is the one state this command exists to catch")
	}
	if !strings.Contains(err.Error(), "no headline price line found") {
		t.Fatalf("error does not say what is missing: %v", err)
	}
}

// A reworded headline that drops the seam weight must not match a prefix and
// report a crossing count against the wrong pair of numbers.
func TestAHeadlineMissingItsSeamWeightIsNotAMatch(t *testing.T) {
	path := write(t, "# 定价：x\n#   → 50 条 import 留在组内，6 条跨组。\n")
	if _, err := headlinePrice(path); err == nil {
		t.Fatal("a two-number headline was accepted as a three-number price")
	}
}

func TestThePricersOutputIsReadInFull(t *testing.T) {
	out := `  2 domain(s) the grouping does not mention: container federationchild
  50 import statements stay inside a group, 6 cross one
  the edges this split severs, heaviest first:
    3  chatdiagnose     -> loop              (apps -> core)
    3  demo             -> alert             (apps -> core)
  54 wired domain(s) in total, 31 file(s) outside core/manager to edit, 235 import(s).
`
	got, err := parseCutOutput(out)
	if err != nil {
		t.Fatalf("parseCutOutput: %v", err)
	}
	if want := (price{internal: 50, crossing: 6, seam: 3}); got != want {
		t.Fatalf("parsed = %v, want %v", got, want)
	}
}

// Taking the maximum rather than the first row means a pricer that reorders
// the list does not silently change which number is compared.
func TestTheSeamIsTheHeaviestRowNotTheFirstOne(t *testing.T) {
	out := `  50 import statements stay inside a group, 6 cross one
  the edges this split severs, heaviest first:
    1  a -> b
    7  c -> d
    2  e -> f
`
	got, err := parseCutOutput(out)
	if err != nil {
		t.Fatalf("parseCutOutput: %v", err)
	}
	if got.seam != 7 {
		t.Fatalf("seam = %d, want 7", got.seam)
	}
}

func TestAnUnreadablePricerReportFailsWithItsOwnOutput(t *testing.T) {
	_, err := parseCutOutput("the pricer changed its wording\n")
	if err == nil {
		t.Fatal("output with none of the three numbers was accepted")
	}
	if !strings.Contains(err.Error(), "the pricer changed its wording") {
		t.Fatalf("the error drops the output that would explain it: %v", err)
	}
}

// The numbers in the tree, not a fixture. This is the one that would have
// caught the defect: the proposal said 95/26/4 and the tree says 50/6/3.
func TestTheProposalInThisRepositoryAgreesWithTheTree(t *testing.T) {
	// Not a Skip. This test skipped once, and the skip was green: the walk
	// looked for go.work, go.work is gitignored, and a checkout or a CI runner
	// that does not have one reported SKIP for the one case that reads the
	// real document. A test that cannot find what it needs must fail, because
	// "did not run" and "passed" print the same thing without -v.
	// Absolute, because reporoot.Find walks with filepath.Dir and filepath.Dir(".")
	// is "." — handed a relative start it stops on the first step, which is
	// how this test skipped the first time it ran.
	here, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root, ok := reporoot.Find(here, 16)
	if !ok {
		t.Fatalf("no repository root above %s; this test reads the real proposal "+
			"and must not decline to run", here)
	}
	proposal := filepath.Join(root, "docs", "manager-split.proposed")
	quoted, err := headlinePrice(proposal)
	if err != nil {
		t.Fatalf("%v\nre-price it with `make split-cost FILE=docs/manager-split.proposed`", err)
	}
	live, err := computedPrice(root, proposal)
	if err != nil {
		t.Fatalf("computedPrice: %v", err)
	}
	if quoted != live {
		t.Fatalf("the proposal's headline is out of date\n  docs say: %v\n  tree has: %v", quoted, live)
	}
}

// Command splitprice checks one number in a split proposal: the price the
// proposal's headline claims, against the price the tree actually produces.
//
// The proposal is deliberately a report, not a gate. `split-cost` prints what
// a grouping would cost, and the Makefile refuses to make that a gate for a
// reason worth keeping: a proposal written on the day it is wrong is a
// proposal nobody argues with, because the tool that says it is wrong also
// breaks the build. That reasoning is right about the computed value and wrong
// about the quoted one. A printed number rots in nobody's memory; a number
// written into a document is a sentence a reader believes, and this repository
// has already been bitten by exactly that shape twice — the ledger declaring a
// count the tree no longer had (decision 402), and a `mayDependOn` entry that
// authorised nothing (decision 74).
//
// So the split stays a report, and this guards the sentence. It compares the
// headline — the `→ N 条 import 留在组内，M 条跨组` line a reader takes away —
// with what `domaincheck -cut` computes now, and fails when they disagree.
//
// It deliberately does NOT read the running log beneath that headline. Those
// entries are a history of prices, and a history that has been overtaken is
// correct history; decision 249's entry sits after decision 257's because the
// log was appended to as cuts landed, so "the last number in the file" is not
// "the current number" and a checker that reached for it would fail on a
// correct document.
//
// The seam weight is included because the headline quotes three numbers and a
// check that only compared two would leave the third to rot on its own.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "splitprice:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: splitprice PROPOSAL TREE")
	}
	proposal, tree := args[0], args[1]

	quoted, err := headlinePrice(proposal)
	if err != nil {
		return err
	}
	live, err := computedPrice(tree, proposal)
	if err != nil {
		return err
	}
	if quoted == live {
		fmt.Printf("splitprice: the proposal's headline and the tree agree — %s\n", live)
		return nil
	}
	return fmt.Errorf(
		"the proposal's headline is out of date\n"+
			"  docs say:  %d internal / %d crossing, heaviest seam %d\n"+
			"  tree has:  %d internal / %d crossing, heaviest seam %d\n"+
			"  fix:       re-price with `make split-cost FILE=%s` and correct the headline.\n"+
			"             The running log underneath is history and is NOT checked; only the headline is.",
		quoted.internal, quoted.crossing, quoted.seam,
		live.internal, live.crossing, live.seam,
		proposal)
}

// price is the three-number quote a headline carries.
//
// All three or none: a price with a seam weight missing is not a price this
// checker can verify, and silently comparing two of the three would let the
// third drift on its own.
type price struct {
	internal int
	crossing int
	seam     int
}

func (p price) String() string {
	return fmt.Sprintf("%d internal / %d crossing, heaviest seam %d", p.internal, p.crossing, p.seam)
}

// headlinePattern matches the arrow line the proposal opens its price with.
//
// The three numbers are captured separately rather than as one run so that a
// reworded line without the seam weight fails to match at all, instead of
// matching a prefix and reporting a crossing count against the wrong pair.
var headlinePattern = regexp.MustCompile(`→\s*(\d+)\s*条\s*import\s*留在组内[，,]\s*(\d+)\s*条跨组[，,]\s*最重的一条缝\s*(\d+)\s*条\s*import`)

// headlinePrice reads the price a proposal claims.
//
// The LAST arrow line wins, not the first. The file is a header followed by a
// long comment block that quotes the number more than once as history, and the
// live quote is the one nearest the "定价：" instruction that names the command
// that produces it.
func headlinePrice(path string) (price, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return price{}, err
	}
	var last price
	var found bool
	for _, line := range strings.Split(string(raw), "\n") {
		m := headlinePattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		internal, err1 := strconv.Atoi(m[1])
		crossing, err2 := strconv.Atoi(m[2])
		seam, err3 := strconv.Atoi(m[3])
		if err1 != nil || err2 != nil || err3 != nil {
			return price{}, fmt.Errorf("%s: headline numbers are not integers", path)
		}
		last = price{internal: internal, crossing: crossing, seam: seam}
		found = true
	}
	if !found {
		return price{}, fmt.Errorf("%s: no headline price line found — expected one shaped like\n"+
			"  → N 条 import 留在组内，M 条跨组，最重的一条缝 K 条 import。\n"+
			"  Without it this document states no current price, which is the one thing\n"+
			"  this checker exists to keep true.", path)
	}
	return last, nil
}

var (
	stayInside = regexp.MustCompile(`(\d+) import statements stay inside a group`)
	crossOne   = regexp.MustCompile(`(\d+) cross one`)
	// The seam list is printed as ranked rows, `3  chatdiagnose -> loop`. Only
	// the leading integer is wanted and only from the rows, so the match is
	// anchored to the start of a trimmed line.
	seamRow = regexp.MustCompile(`^\s*(\d+)\s{2,}\S`)
)

// computedPrice asks the tool that prices a split what the tree says now.
//
// It shells out rather than importing, because the pricer is `package main` in
// another directory and a second implementation of the same arithmetic is
// precisely the defect this command is here to prevent: two codes computing
// one number is how the document and the tree come to disagree in the first
// place.
func computedPrice(tree, proposal string) (price, error) {
	// The tree root is the argument, not something to go looking for. An
	// earlier version walked upwards looking for go.work to find it, and that
	// was wrong twice over: go.work is gitignored, so a clean clone and CI do
	// not have one; and a walk that gave up quietly turned the one test that
	// reads the real proposal into a green SKIP. Every Makefile target runs
	// from the repository root and already passes the tree in, so there is
	// nothing to discover.
	// Dir is the tree, not the process's own working directory: `go test`
	// runs a package in its own directory, so without this the relative
	// package path below resolves under scripts/splitprice/ and the pricer
	// is not found.
	cmd := exec.Command("go", "run", "./scripts/domaincheck", tree, "-cut", proposal)
	cmd.Dir = tree
	out, err := cmd.CombinedOutput()
	if err != nil {
		return price{}, fmt.Errorf("domaincheck -cut failed: %v\n%s", err, out)
	}
	return parseCutOutput(string(out))
}

// parseCutOutput reads the three numbers out of the pricer's report.
//
// Split out from the exec so the reading is testable without spending a `go
// run` per case, and so a change to the pricer's wording fails here — with the
// real line in the failure — rather than as an opaque "could not read all
// three numbers" from a gate.
func parseCutOutput(out string) (price, error) {
	var (
		result     price
		haveInside bool
		haveCross  bool
		haveSeam   bool
	)
	for _, line := range strings.Split(out, "\n") {
		if m := stayInside.FindStringSubmatch(line); m != nil {
			result.internal, _ = strconv.Atoi(m[1])
			haveInside = true
		}
		if m := crossOne.FindStringSubmatch(line); m != nil {
			result.crossing, _ = strconv.Atoi(m[1])
			haveCross = true
		}
		// The heaviest seam is the top row of a ranked list, but taking the
		// maximum rather than the first row means a pricer that reorders or
		// interleaves the list does not silently change which number this
		// compares.
		if m := seamRow.FindStringSubmatch(line); m != nil {
			seam, err := strconv.Atoi(m[1])
			if err == nil && seam > result.seam {
				result.seam = seam
				haveSeam = true
			}
		}
	}
	if !haveInside || !haveCross || !haveSeam {
		return price{}, fmt.Errorf("could not read all three numbers out of domaincheck -cut output:\n%s", out)
	}
	return result, nil
}

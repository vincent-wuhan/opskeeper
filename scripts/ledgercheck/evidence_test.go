package ledgercheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The fourth number the repository cannot answer for itself: whether the
// tests a completion percentage rests on still exist, and whether anything
// runs them.
//
// The other five checks in this package are arithmetic. They read the plan
// table and prove the weights sum, that the stated total equals the rows
// above it, that the formula in brackets still lists those rows, that the
// four stage averages are the means they claim, that the module count and
// the tool count still match the Makefile and the manifests. All of that is
// true of a table whose every claim has lost its evidence — delete
// TestATransportFailureStopsBeforeTheRowsThatDidNotGo and stage 1 is still
// 100% and all six checks are still green.
//
// That is the shape cigate's own header describes, one table over: a quoted
// number, a command somebody typed, and no owner for the property. So the
// percentages are anchored to files here, and this check asks three
// questions of each anchor: does it exist, does some module CI builds and
// tests contain it, and does the ledger's own column of "not covered by CI"
// agree with what that computation finds.
//
// It is deliberately a check on existence and execution rather than on
// behaviour. Whether those tests are strong is a question this cannot answer,
// and the place to argue about it is the decision that added them, not here.

const anchorHeading = "### 四阶段的证据锚点"

// repoRoot is this package's own two levels down. Every path the ledger
// writes is repository-relative — that is how a person reads it — while a
// test's working directory is the package it lives in, so the join has to
// happen once, here, rather than being remembered at each of the four call
// sites that touch the disk.
const repoRoot = "../.."

// anchorRowRE reads one row of the anchors table. The stage cell is matched
// by its leading digit so the table cannot grow a fifth row under a heading
// that names four stages without this noticing.
var anchorRowRE = regexp.MustCompile(`(?m)^\| ([0-3]) [^|]*\| ([^|]+)\| ([^|]+)\|$`)

// topLevelFuncRE finds the start of any top-level func, named or not.
var topLevelFuncRE = regexp.MustCompile(`(?m)^func ([A-Za-z0-9_]+)\(`)

// anchor is one stage's evidence list, as the ledger states it.
type anchor struct {
	stage     string
	evidence  []string
	uncovered []string
}

func parseAnchors(t *testing.T, ledger string) []anchor {
	t.Helper()
	start := strings.Index(ledger, anchorHeading)
	if start < 0 {
		t.Fatalf("the ledger has no %q section; the percentages have nothing to point at", anchorHeading)
	}
	rest := ledger[start+len(anchorHeading):]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		rest = rest[:end]
	}
	var out []anchor
	for _, m := range anchorRowRE.FindAllStringSubmatch(rest, -1) {
		out = append(out, anchor{
			stage:     m[1],
			evidence:  splitPaths(m[2]),
			uncovered: splitPaths(m[3]),
		})
	}
	if len(out) != 4 {
		t.Fatalf("the anchors table has %d stage rows, not 4; stages 0-3 each carry a percentage and each needs an owner", len(out))
	}
	return out
}

// splitPaths reads a cell of `a`、`b` or a lone `-` for none.
//
// Backticks are stripped rather than required, so a path written without them
// is still checked — the point of this file is that a path cannot be written
// down and then quietly mean nothing.
func splitPaths(cell string) []string {
	if strings.TrimSpace(cell) == "-" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(cell, "、") {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, "`")
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// makefileVar reads a top-level `NAME := value` assignment from the Makefile.
//
// Line by line rather than one regexp, because the module list this package
// already reads is joined with trailing backslashes and a regexp written for
// it silently stops after the first line — which reads seven of fourteen and
// turns every check that depends on it into a permanent false positive.
func makefileVar(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(makefilePath))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	prefix := name + " :="
	collecting := false
	var fields []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if collecting {
			fields = append(fields, strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), `\`))...)
			if !strings.HasSuffix(line, `\`) {
				break
			}
			continue
		}
		if strings.HasPrefix(line, prefix) {
			collecting = true
			head := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			fields = append(fields, strings.Fields(strings.TrimSuffix(head, `\`))...)
			if !strings.HasSuffix(line, `\`) {
				break
			}
		}
	}
	if !collecting {
		t.Fatalf("the Makefile declares no %s; this check reads that list to decide what CI runs", name)
	}
	return strings.Join(fields, "|")
}

// brokerSkipped says whether CI runs the tests in this tests/e2e file.
//
// The suite runs in the e2e job except for the two that pull a tunnel broker
// container, and those are named in the Makefile. A file is skipped when any
// test it declares is on that list — file granularity, deliberately: the
// broker tests live in files of their own, and a per-function reader here
// would be a second implementation of a rule that already has one.
func brokerSkipped(t *testing.T, path string) bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	skipped := makefileVar(t, "E2E_BROKER_TESTS")
	for _, m := range topLevelFuncRE.FindAllStringSubmatch(string(body), -1) {
		for _, name := range strings.Split(skipped, "|") {
			if m[1] == name {
				return true
			}
		}
	}
	return false
}

// runsInCI says whether something CI invokes on every push executes this path.
//
// Two answers, and the second one is a correction to a belief this
// repository held for its whole life: a path inside a module in
// PIG_MODULES is built and tested by module-standalone-check, and a path
// under tests/e2e is run by the e2e job unless the broker list skips it.
// Longest-prefix wins, because `.` is in PIG_MODULES and would otherwise
// swallow core/edge.
func runsInCI(t *testing.T, path string) bool {
	t.Helper()
	if strings.HasPrefix(path, "tests/e2e") {
		return !brokerSkipped(t, path)
	}
	_, modules := declaredModules(t)
	best := ""
	rootHasIt := false
	for _, m := range modules {
		if m == "." {
			// The root module is the fallback rather than a prefix: it is
			// the shortest possible match, so scoring it alongside
			// core/edge would put it first and read every path as covered
			// by it.
			rootHasIt = true
			continue
		}
		if strings.HasPrefix(path, m+"/") && len(m) > len(best) {
			best = m
		}
	}
	// `go test ./...` from the root builds everything nothing more specific
	// claimed, which is how tests/agentgateway is run on every push.
	return best != "" || rootHasIt
}

// TestEveryCompletionPercentageIsAnchoredToTestsThatExistAndRun is the check.
//
// The four stages are four numbers this repository quotes more than any
// other, and until now each one was a sentence. A stage claiming 100% while
// the test that carried it was deleted is the failure this refuses, and the
// ledger's own "not covered by CI" column is compared against the computed
// set rather than trusted, so the column cannot quietly go stale either.
func TestEveryCompletionPercentageIsAnchoredToTestsThatExistAndRun(t *testing.T) {
	var problems []string
	for _, a := range parseAnchors(t, readLedger(t)) {
		if len(a.evidence) == 0 {
			problems = append(problems, fmt.Sprintf(
				"stage %s anchors its percentage to no test at all, so the number has no owner", a.stage))
			continue
		}

		var computed []string
		for _, path := range a.evidence {
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(path))); err != nil {
				problems = append(problems, fmt.Sprintf(
					"stage %s anchors its percentage to %s, which does not exist; the claim lost its evidence and the percentage did not move", a.stage, path))
				continue
			}
			if !runsInCI(t, path) {
				computed = append(computed, path)
			}
		}
		sort.Strings(computed)
		declared := append([]string(nil), a.uncovered...)
		sort.Strings(declared)

		if strings.Join(computed, ",") != strings.Join(declared, ",") {
			problems = append(problems, fmt.Sprintf(
				"stage %s lists %v as its CI-uncovered evidence, and the computed set is %v; one of the two is stale",
				a.stage, declared, computed))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("the completion percentages are not anchored to evidence that exists and runs:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

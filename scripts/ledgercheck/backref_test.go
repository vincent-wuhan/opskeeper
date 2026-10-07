package ledgercheck

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file guards the back-reference table that decision 183 added to the
// progress section: the list of §四 decisions that moved a §六 line, and
// whether that line has been re-checked against the code since.
//
// The reason it exists is a pattern that has now happened three times: a
// decision is appended to §四, and the §六 line it invalidated is never
// revisited, so the progress section keeps stating a conclusion the code
// stopped supporting. §4.110 (the four-stage table), §4.178 (stage C) and
// §4.182 (federation) are three instances of the same shape, and two of them
// sat there long enough to be quoted in hand-offs.
//
// Of the nine checks in this package, the sixth catches the numeric form of
// that (a total that no longer equals its rows) and the ninth catches the
// cross-reference form (a row that ends on a pointer to the wrong section).
// Neither catches the form found in §4.182: a line that says a capability is
// not wired while §四 says the very same capability was wired in the same
// paragraph. There is no shape to match — it is a semantic contradiction, and
// pretending a regex can see it would be a check that teaches its reader to
// trust it.
//
// So this file does not attempt to detect the contradiction. It does the one
// thing that can be done mechanically: every decision this table cites must
// actually exist in §四. A table that names a decision which is not there is
// worse than no table, because it looks like provenance and is not.

// backrefHeading is matched as a heading, not as a bare substring. The first
// version searched for the phrase anywhere, and decision 183's own mutation
// table quotes it — so the search found that quotation, above the progress
// section, and the slice it then read contained no rows at all. A locator
// that can be satisfied by prose about the thing it locates is a locator
// that will eventually be.
const backrefHeading = "\n### §四 决策 → §六 回核清单"

// backrefRowRE matches one row of the back-reference table. The decision cell
// always begins with `决策 NNN`, and a cell may name more than one (`78/79`,
// `141/168`) — each of those is a real decision that must exist, so the
// extractor below pulls every number out of the cell rather than just the
// first.
var backrefRowRE = regexp.MustCompile(`(?m)^\| (决策 [0-9/、]+[^|]*?) \| ([^|]*?) \|`)

// decisionHeadingRE matches a decision number in a decision *heading*.
//
// The ledger uses two heading shapes and both are real: `### 4.116 决策 183：…`
// (91 of them) and `### 4.16 一个节点不是掉线才丢隧道…（决策 78）` (20). The
// first version matched only the former, so every decision written in the
// second shape — 78, 79, 125, 126 among them — came back "not existing" and
// the check rejected a table of true citations. Hence the loose body: the
// anchor `^### N.N` is what makes this safe, not the position of 决策.
var decisionHeadingRE = regexp.MustCompile(`(?m)^### [0-9]+\.[0-9]+ [^\n]*?决策 ([0-9]+)`)

// decisionNumberRE finds a decision number, e.g. `决策 178` or `决策 78/79`.
var decisionNumberRE = regexp.MustCompile(`决策 ([0-9]+)`)

// TestEveryBackreferencedDecisionExists is the tenth gate. It reads the
// back-reference table out of the progress section and checks that every §四
// decision it names is present in the document.
func TestEveryBackreferencedDecisionExists(t *testing.T) {
	ledger := readLedger(t)
	start := strings.Index(ledger, backrefHeading)
	if start < 0 {
		t.Fatalf("the ledger has no %q table; decision 183 added it so that a §四 decision "+
			"which moves a §六 line has a place to be recorded, and this check has to be told "+
			"where it moved", backrefHeading)
	}
	// The table runs from just past its own heading to the next heading.
	// The first version sliced from `start` itself, which begins with `\n### `
	// — so the search for the next heading matched the table's *own* heading
	// at offset 0 and the slice came back empty. The check then reported "no
	// rows" on a table that has seven, which is the worst possible failure:
	// it looks like the table is empty rather than like the reader is wrong.
	table := ledger[start+len(backrefHeading):]
	if end := strings.Index(table, "\n### "); end >= 0 {
		table = table[:end]
	}

	// The corpus for "does this decision exist" is everything *before* the
	// progress section. The first version searched the whole document, and
	// the table then made its own claims true: a cell naming 决策 999 put
	// "999" into the set, so the check passed on exactly the mutation it
	// exists to catch. The source of truth must not contain the thing being
	// checked against it.
	//
	// Two rounds were needed to get this right, and both failures are the
	// same mistake at different scopes. Scanning the whole document let the
	// table's own claims prove themselves. Restricting the corpus to §四 was
	// not enough, because §四 quotes the numbers it is talking about: the
	// mutation table in decision 183 names 决策 999 in prose, and that prose
	// is above §六, so a fabricated citation still found itself "existing".
	//
	// A decision exists if and only if it has a heading. Headings are the one
	// place a number is *asserted* rather than *mentioned*, so they are the
	// only trustworthy source — and quoting a number can never mint a
	// decision.
	decisions := map[string]bool{}
	for _, m := range decisionHeadingRE.FindAllStringSubmatch(ledger, -1) {
		decisions[m[1]] = true
	}

	var problems []string
	rows := backrefRowRE.FindAllStringSubmatch(table, -1)
	if len(rows) == 0 {
		t.Fatalf("the back-reference table has no rows; decision 183 added it with rows and an " +
			"empty one would look like \"nothing needs re-checking\" when it means " +
			"\"nobody recorded what needs re-checking\"")
	}
	for _, row := range rows {
		cell := row[1]
		// Pull every number in the cell, so `决策 78/79` and `决策 141/168`
		// are both checked. Numbers are separated from prose by the `决策 `
		// prefix on the first and by `/` on the rest.
		for _, num := range decisionNumbersInCell(cell) {
			if !decisions[num] {
				problems = append(problems, fmt.Sprintf(
					"the back-reference table cites 决策 %s, which does not appear anywhere in "+
						"the ledger; a table that names a decision which is not there looks like "+
						"provenance and is not (cell: %q)", num, cell))
			}
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("the back-reference table must cite decisions that exist:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// decisionNumbersInCell pulls every decision number out of a back-reference
// cell. The cell reads like `决策 78/79（连接规模三项落地）` or
// `决策 141/168（节点工具链 0/18 关闭）`, so the first number is introduced by
// `决策 ` and any further ones follow a `/` or `、`. A trailing `（…）` is
// prose and is skipped.
func decisionNumbersInCell(cell string) []string {
	m := decisionNumberRE.FindStringSubmatch(cell)
	if m == nil {
		return nil
	}
	rest := cell[len(m[0]):]
	// Stop at the first non-numeric, non-separator character: the parenthetical.
	if i := strings.IndexAny(rest, "（( 、,，"); i >= 0 {
		rest = rest[:i]
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool {
		return r == '/' || r == '、' || r == ','
	})
	out := make([]string, 0, len(parts)+1)
	if _, err := strconv.Atoi(m[1]); err == nil {
		out = append(out, m[1])
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

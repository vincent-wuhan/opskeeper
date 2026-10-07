package ledgercheck

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file checks the one thing every percentage row owes the reader and
// that no arithmetic check can see: what is still missing.
//
// The other eight checks all verify relations between numbers — weights sum
// to 100, the total equals the sum of the rows, the bracket formula lists the
// same rows, the four stages average to the stated mean, the module count and
// the tool count match what is on disk. Every one of them is blind to a row
// whose "remaining" cell ends on a verdict the reader can't use.
//
// Decision 178 found exactly that. The C row's remaining cell ended with
// "详见 §4.23" — §4.23 is the MCP correction, which has nothing to do with
// what C still owes. The word 剩下 did appear earlier in that cell, inside a
// sentence about an earlier correction ("决策 85 更正了此处的「剩下」"), which
// is why a naive "does the cell mention 剩下 somewhere" test passes on the
// broken row. The cell wasn't missing the word; it was *concluding* with a
// pointer to the wrong section.
//
// So this check does not ask *how much* is left. It asks a weaker, but
// checkable, question: does the row's final clause actually account for what
// is left? A finished row must say it has nothing left; an unfinished row must
// name what is left. A trailing cross-reference like 详见 §X is not a
// remainder unless X is about this row's gap — and this check cannot know
// that, so it refuses to accept the pointer as the verdict.
//
// The accepted wordings are the ones the table actually uses today, so this is
// not a demand for a new phrasing:
//
//   - "剩下"      — a named remainder (rows C, D, E)
//   - "无剩余项"  — nothing left (row A)
//   - "不是缺口"  — nothing left, in B's words
//
// A row whose last clause carries none of these has stopped accounting for
// its own deduction, which is the failure this file exists to make loud.

const (
	// planRowPrefixes are the five stage rows of the A–E table, in order. Each
	// must account for its own remaining.
	planRowPrefixes = "ABCDE"
)

// remainingMarkers are the wordings a row's closing clause may use to account
// for its remaining. Kept as a set of alternatives, not a single phrase,
// because the table legitimately says "无剩余项" in one row and "不是缺口" in
// another, and forcing one wording would be churn, not a check.
var remainingMarkers = []string{"剩下", "无剩余项", "不是缺口"}

// clauseBreakRE splits a cell into clauses. The full-width forms are what
// this document actually uses between clauses; the ASCII ones are included so
// a future edit that reaches for English punctuation still gets a fair split
// instead of silently failing to separate anything.
var clauseBreakRE = regexp.MustCompile(`[。；;！!]`)

// TestEveryStageRowAccountsForItsOwnRemaining is the ninth gate. It reads the
// last cell of each A–E row in the progress section and requires the row's
// final clause to carry a remaining marker.
func TestEveryStageRowAccountsForItsOwnRemaining(t *testing.T) {
	ledger := readLedger(t)
	var problems []string
	for _, stage := range planRowPrefixes {
		line := progressRow(t, ledger, "| "+string(stage)+" ")
		clause := finalClause(lastCell(line))
		if !saysSomethingAboutRemaining(clause) {
			problems = append(problems, fmt.Sprintf(
				"row %s does not close by saying what is left: it ends on %q, which "+
					"names no remainder (expected one of %s). A row that deducts from "+
					"its own percentage has to account for the deduction in its closing "+
					"words — a trailing cross-reference like 详见 §X points at a "+
					"section rather than at a remainder, and §4.23 is the MCP "+
					"correction, not this row's gap (decision 178)",
				string(stage), tailOf(clause), strings.Join(remainingMarkers, " / ")))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("every A–E row must close by stating its own remaining:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// lastCell returns the final `|`-delimited cell of a table row, with the
// surrounding spaces trimmed.
func lastCell(line string) string {
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return line
	}
	return strings.TrimSpace(parts[len(parts)-2])
}

// finalClause returns the last non-empty clause of a cell — the part a reader
// reads last, and therefore the part that has to be the verdict. Requiring
// the *final* clause (rather than the whole cell) is what makes this check
// able to see a row that mentions 剩下 in passing but concludes with a pointer
// somewhere unrelated.
func finalClause(cell string) string {
	var last string
	for _, seg := range clauseBreakRE.Split(cell, -1) {
		if seg = strings.TrimSpace(seg); seg != "" {
			last = seg
		}
	}
	return last
}

// saysSomethingAboutRemaining reports whether a clause accounts for a
// remaining, by looking for any of the accepted markers.
func saysSomethingAboutRemaining(clause string) bool {
	for _, marker := range remainingMarkers {
		if strings.Contains(clause, marker) {
			return true
		}
	}
	return false
}

// tailOf is a short suffix of a string, for the failure message. A row's
// closing clause can be long; the tail is where the cross-reference or the
// verdict actually reads.
func tailOf(s string) string {
	const n = 40
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return "…" + string(runes[len(runes)-n:])
}

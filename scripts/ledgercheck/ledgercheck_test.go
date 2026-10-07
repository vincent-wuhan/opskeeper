// Package ledgercheck holds the check that the architecture ledger's own
// arithmetic holds up.
//
// It exists because of a specific failure, recorded in the ledger as §4.108.6:
// the plan A–E table stated a weighted total of 98.0% next to a formula that
// evaluates to 97.0%. 98.0 had been correct when one row was still 100%; the
// row moved, the total did not, and nobody recomputed it for a dozen
// decisions — while the number next to it kept being quoted.
//
// This package does not judge whether a percentage is right. It judges one
// thing: whether the total equals the sum of the rows printed above it, and
// whether the formula in brackets still lists those same rows. A judgement
// about the percentages is a human decision with a reason attached; this is
// arithmetic, and arithmetic that is only ever done in someone's head is
// arithmetic that silently stops being true.
package ledgercheck

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const ledgerPath = "../../docs/opskeeper2-architecture.md"

// planRow is one line of the plan A–E table: a stage, its weight, and the
// completion the ledger claims for it.
type planRow struct {
	stage  string
	weight float64
	value  float64
}

// rowRE captures the `C ... 20% ... **95%**` shape of a plan row. It keys on
// the stage letter and the bold percentage because those two are the
// machine-readable part; the prose between them is for people.
var rowRE = regexp.MustCompile(`^\| ([A-E]) [^|]*\| (\d+(?:\.\d+)?)% \| \*\*(\d+(?:\.\d+)?)%\*\* \|`)

// totalRE captures the A–E weighted total. Only the line whose bracket holds a
// multiplication sign is the plan table's; the older 阶段 0–3 chain writes its
// sums differently and is checked by the mean rule below.
var totalRE = regexp.MustCompile(`加权合计 ≈ \*\*(\d+(?:\.\d+)?)%\*\*（([^）]*×[^）]*)）`)

// termRE is one `20×0.95` term of that formula.
var termRE = regexp.MustCompile(`(\d+(?:\.\d+)?)×(\d+(?:\.\d+)?)`)

// meanRE captures the 阶段 0–3 equal-weight average in both forms the ledger
// uses: with an equals sign and in bold, and bare.
var meanRE = regexp.MustCompile(`(\d+(?:\.\d+)?) / (\d+(?:\.\d+)?) / (\d+(?:\.\d+)?) / (\d+(?:\.\d+)?) 的均值 (?:= \*\*)?(\d+(?:\.\d+)?)`)

func readLedger(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(ledgerPath))
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	return string(raw)
}

func parseRows(t *testing.T, ledger string) []planRow {
	t.Helper()
	var rows []planRow
	for _, line := range strings.Split(ledger, "\n") {
		m := rowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		weight, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("stage %s has an unreadable weight %q: %v", m[1], m[2], err)
		}
		value, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Fatalf("stage %s has an unreadable completion %q: %v", m[1], m[3], err)
		}
		rows = append(rows, planRow{stage: m[1], weight: weight, value: value / 100})
	}
	if len(rows) != 5 {
		t.Fatalf("found %d plan rows, want 5 (A–E); the table's shape changed, so this check has to be taught the new shape before it can be trusted again", len(rows))
	}
	return rows
}

// TestThePlanTablesWeightsAddUpToOneHundred is the first thing that has to
// hold: a weighted total over weights that do not sum to 100 is not a
// percentage of anything.
func TestThePlanTablesWeightsAddUpToOneHundred(t *testing.T) {
	rows := parseRows(t, readLedger(t))
	var total float64
	for _, r := range rows {
		total += r.weight
	}
	if math.Abs(total-100) > 1e-9 {
		var parts []string
		for _, r := range rows {
			parts = append(parts, fmt.Sprintf("%s=%g", r.stage, r.weight))
		}
		t.Fatalf("the plan table's weights sum to %g, want 100 (%s)", total, strings.Join(parts, " "))
	}
}

// TestTheStatedTotalIsTheSumOfTheRowsAboveIt is the check §4.108.6 asks for.
//
// The message names the recomputed value rather than only saying "wrong",
// because the person who has to fix it needs the number, and because a check
// that only says "wrong" eventually gets disabled.
func TestTheStatedTotalIsTheSumOfTheRowsAboveIt(t *testing.T) {
	ledger := readLedger(t)
	rows := parseRows(t, ledger)

	m := totalRE.FindStringSubmatch(ledger)
	if m == nil {
		t.Fatal("no weighted-total line with a multiplication formula found; the plan table's total is no longer written in the form this check reads")
	}
	stated, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("unreadable total %q: %v", m[1], err)
	}

	var sum float64
	for _, r := range rows {
		sum += r.weight * r.value
	}
	if math.Abs(stated-sum) > 0.05 {
		t.Errorf("the ledger states %g%% but its own five rows sum to %.2f%%; recompute the total (%s) or change a row — not both",
			stated, sum, describe(rows))
	}
}

// TestTheFormulaInBracketsStillListsThoseRows catches the other half of the
// same mistake: a total that is right while the formula beside it describes
// something else. The formula is the only place the arithmetic is written
// down, so a formula that has drifted from the table is worse than no
// formula — it is one that looks checkable.
func TestTheFormulaInBracketsStillListsThoseRows(t *testing.T) {
	ledger := readLedger(t)
	rows := parseRows(t, ledger)

	m := totalRE.FindStringSubmatch(ledger)
	if m == nil {
		t.Fatal("no weighted-total line with a multiplication formula found")
	}
	terms := termRE.FindAllStringSubmatch(m[2], -1)
	if len(terms) != len(rows) {
		t.Fatalf("the formula has %d terms and the table has %d rows (%s); one of them moved and the other did not",
			len(terms), len(rows), describe(rows))
	}
	for i, term := range terms {
		weight, err := strconv.ParseFloat(term[1], 64)
		if err != nil {
			t.Fatalf("term %q: %v", term[0], err)
		}
		value, err := strconv.ParseFloat(term[2], 64)
		if err != nil {
			t.Fatalf("term %q: %v", term[0], err)
		}
		if weight != rows[i].weight || math.Abs(value-rows[i].value) > 1e-9 {
			t.Errorf("formula term %d is %s but row %s is %g×%g", i+1, term[0],
				rows[i].stage, rows[i].weight, rows[i].value)
		}
	}
}

// TestTheFourStageAveragesAreTheMeansTheyClaim covers the 阶段 0–3 chain, which
// is written as an equal-weight mean of four numbers rather than as a
// weighted total.
//
// Those lines are historical — each was true when it was written, and several
// sit inside decision records describing a superseded state. This does not
// reopen them: it checks only that each stated mean is the mean of the four
// numbers on its own line, which is a property of the sentence rather than of
// the plan.
func TestTheFourStageAveragesAreTheMeansTheyClaim(t *testing.T) {
	ledger := readLedger(t)
	checked := 0
	for _, line := range strings.Split(ledger, "\n") {
		m := meanRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var sum float64
		for _, raw := range m[1:5] {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				t.Fatalf("unreadable stage value %q: %v", raw, err)
			}
			sum += v
		}
		claimed, err := strconv.ParseFloat(m[5], 64)
		if err != nil {
			t.Fatalf("unreadable mean %q: %v", m[5], err)
		}
		want := sum / 4
		// The ledger rounds to one decimal, so half of that is the
		// tolerance; one historical line rounds up (76.175 to 76.2).
		if math.Abs(claimed-want) > 0.06 {
			t.Errorf("line states a mean of %g but %g / %g / %g / %g averages %.3f: %s",
				claimed, mustFloat(m[1]), mustFloat(m[2]), mustFloat(m[3]), mustFloat(m[4]), want, firstLine(line))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no four-stage mean lines found; this check is not looking at what it claims to")
	}
}

// fourStageRowRE reads one of the four stage rows of the distributed
// programme's progress table: `| 2 生态与治理加固（P2） | **96.7%** | ... |`.
var fourStageRowRE = regexp.MustCompile(`(?m)^\| ([0-3]) [^|]*\| \*\*(\d+(?:\.\d+)?)%\*\* \|`)

// fourStageTotalRE reads the stated weighted total of the same table, and
// the four values it is said to be the mean of.
//
// Both the bold and the 四阶段等比 are required. The bold is how the current
// line is told from the historical ones stacked under it, and 四阶段等比 is
// what separates this total from the A–E one that shares the section.
var fourStageTotalRE = regexp.MustCompile(
	`加权合计 ≈ \*\*(\d+(?:\.\d+)?)%\*\*（[^）]*四阶段等比 (\d+(?:\.\d+)?) / (\d+(?:\.\d+)?) / (\d+(?:\.\d+)?) / (\d+(?:\.\d+)?)`)

// TestTheStageRowsAndTheStatedTotalAreTheSameNumber is the check that was
// missing for a long time, and its absence is worth more than the check.
//
// The existing average check scans the whole ledger for a formula of the
// shape "a / b / c / d 的均值 n" and verifies the arithmetic. That is true of
// every historical formula in the document, all of which were correct when
// written, and it never once looks at the four rows of the progress table.
// So the table's rows could say 98 / 100 / 91.7 / 79.3, the stated total
// under them could say 76.2%, and the decision records three screens down
// could say 93.6% — and every check in this package would be green.
//
// It was green that way for long enough that "四阶段 93.6%" was quoted from
// the decision records in every hand-off while the section titled 当前实现进度
// said 76.2%. Nothing in this repository noticed, because nothing asked the
// two whether they agreed. See decision 177.
func TestTheStageRowsAndTheStatedTotalAreTheSameNumber(t *testing.T) {
	ledger := readLedger(t)
	start := strings.Index(ledger, progressHeading)
	if start < 0 {
		t.Fatalf("the ledger has no %q section", progressHeading)
	}
	progress := ledger[start:]
	if end := strings.Index(progress, "\n## "); end >= 0 {
		progress = progress[:end]
	}

	totals := fourStageTotalRE.FindAllStringSubmatch(progress, -1)
	if len(totals) != 1 {
		t.Fatalf("the progress section has %d bold four-stage totals, not 1; "+
			"the current one has to be unambiguous, or this check cannot tell which is which", len(totals))
	}

	rows := map[string]string{}
	for _, m := range fourStageRowRE.FindAllStringSubmatch(progress, -1) {
		if _, dup := rows[m[1]]; dup {
			t.Fatalf("the progress section states stage %s twice", m[1])
		}
		rows[m[1]] = m[2]
	}
	if len(rows) != 4 {
		t.Fatalf("the progress section has %d stage rows, not 4: %v", len(rows), rows)
	}

	var problems []string
	for i, stage := range []string{"0", "1", "2", "3"} {
		said := totals[0][i+2]
		if rows[stage] != said {
			problems = append(problems, fmt.Sprintf(
				"stage %s reads %s%% in its row but %s%% in the stated total; "+
					"one of the two is stale and the weighted number below them is derived from it",
				stage, rows[stage], said))
		}
	}

	var sum float64
	for i := 2; i <= 5; i++ {
		sum += mustFloat(totals[0][i])
	}
	want := sum / 4
	claimed := mustFloat(totals[0][1])
	if math.Abs(claimed-want) > 0.06 {
		problems = append(problems, fmt.Sprintf(
			"the stated total is %g but the four values it names average %.3f", claimed, want))
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("the progress section's stage rows and its stated total disagree:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

func describe(rows []planRow) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s=%g×%g", r.stage, r.weight, r.value))
	}
	return strings.Join(parts, " + ")
}

func mustFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

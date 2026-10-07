package ledgercheck

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestEveryStageRowThatSpellsOutItsCompositionAddsUpToIt is the twelfth
// gate, and it exists because of a specific wrong number rather than a
// general worry.
//
// Decision 147 moved the federation line of stage 3 from 0.94 to 0.97 and
// wrote that stage 3 therefore went "from 79.3% to 79.7%". The three parts
// are averaged, so +0.03 on one part is +0.01 on the row: 79.3% should have
// become 79.4%, and with the parts it settled on it is 80.3%. The ledger then
// carried 79.7% in fifty-two places, every consistency check stayed green —
// the tenth and eleventh gates compare a number against the rows and the
// tables around it, and a number that is consistently wrong passes both.
//
// So the property this gate owns is different from the ones before it: not
// "the number agrees with itself" but "the number is the mean of the parts
// the row itself names". A row that spells out its composition has to add
// up, and a row that does not spell it out is not asked about a composition
// it never claimed to have.
//
// The format is the one the stage 3 row uses:
//
//	本行 = (1.00 审计端口 + 0.44 manager 拆分 + 0.97 多集群联邦) / 3
//
// The labels between the numbers are prose and are ignored; what is checked
// is the arithmetic and the agreement with the percentage in the same row.
// progressSection is the current-state half of the ledger: the progress
// heading and everything up to the next top-level section. The numbers in
// there are the ones a reader quotes; the same numbers appearing further up
// are the history of how they got there, and holding those to a current
// arithmetic would be holding a decision record to something it never
// claimed.
func progressSection(t *testing.T) string {
	t.Helper()
	ledger := readLedger(t)
	start := strings.Index(ledger, progressHeading)
	if start < 0 {
		t.Fatalf("the ledger has no %q section", progressHeading)
	}
	progress := ledger[start:]
	if end := strings.Index(progress, "\n## "); end >= 0 {
		progress = progress[:end]
	}
	return progress
}

// The three values and the three labels are captured separately. Decision 258
// is why: the labels used to be skipped as prose and the distinctness check
// compared the *numbers*, so two different parts that happened to score the
// same (audit ports 1.00 and the manager split 1.00) were reported as "one
// part listed twice" — which is the defect the check exists to catch, not an
// instance of it. The stated reason for the check is about parts, so it now
// reads the parts.
var compositionRE = regexp.MustCompile(
	`\((\d+(?:\.\d+)?)([^\d()]*?)\+ (\d+(?:\.\d+)?)([^\d()]*?)\+ (\d+(?:\.\d+)?)([^\d()]*?)\)\s*/\s*(\d+)`)

// stageRowRE reads the stage number and the percentage out of one row of the
// four-stage table in the progress section. It is the same shape the
// existing consistency gate uses; the two agreeing is not a coincidence, and
// if one is loosened the other should be looked at.
var stageRowRE = regexp.MustCompile(`(?m)^\| ([0-3]) [^|]*\| \*\*(\d+(?:\.\d+)?)%\*\* \|`)

func TestEveryStageRowThatSpellsOutItsCompositionAddsUpToIt(t *testing.T) {
	progress := progressSection(t)

	rows := map[string]string{}
	for _, m := range stageRowRE.FindAllStringSubmatch(progress, -1) {
		rows[m[1]] = m[2]
	}
	if len(rows) != 4 {
		t.Fatalf("expected the four stage rows, found %d: %v", len(rows), rows)
	}

	var problems []string
	for _, line := range strings.Split(progress, "\n") {
		m := stageRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		stage, stated := m[1], m[2]

		parts := compositionRE.FindStringSubmatch(line)
		if parts == nil {
			// Not every stage's parts are a fixed short list, so a row is
			// only held to this when it claims one.
			continue
		}

		var sum float64
		for _, raw := range []string{parts[1], parts[3], parts[5]} {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				problems = append(problems, fmt.Sprintf(
					"stage %s spells out a composition containing %q, which is not a number: %s",
					stage, raw, line))
				sum = math.NaN()
				break
			}
			sum += v
		}
		if math.IsNaN(sum) {
			continue
		}
		divisor, err := strconv.Atoi(parts[7])
		if err != nil || divisor == 0 {
			problems = append(problems, fmt.Sprintf(
				"stage %s divides its composition by %q, which is not a usable divisor: %s",
				stage, parts[7], line))
			continue
		}

		// The parts are fractions and the ledger quotes percentages, which is
		// a unit conversion this gate got wrong the first time it ran: it
		// reported "0.8%" for a row that reads 80.3%, and it was right that
		// something did not add up.
		want := roundOne(sum / float64(divisor) * 100)
		if want != stated {
			problems = append(problems, fmt.Sprintf(
				"stage %s reads %s%%, but the composition it spells out (%s + %s + %s) / %d is %s%%; "+
					"a weighted number derived from a wrong part is wrong everywhere it is quoted",
				stage, stated, parts[1], parts[3], parts[5], divisor, want))
		}
	}

	if len(problems) > 0 {
		t.Errorf("a stage row's percentage is not the mean of the parts it names:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// TestStageThreeNamesItsThreeParts guards the reason the gate above is not
// optional. Stage 3 is the only row whose plan lines are a fixed, countable
// three, so a row that stopped naming them would leave the one number in the
// ledger that is known to have been computed by hand with no way to check it
// completely unverified.
func TestStageThreeNamesItsThreeParts(t *testing.T) {
	progress := progressSection(t)
	var row string
	for _, line := range strings.Split(progress, "\n") {
		if strings.HasPrefix(line, "| 3 ") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("the progress section has no stage 3 row")
	}
	parts := compositionRE.FindStringSubmatch(row)
	if parts == nil {
		t.Fatalf("stage 3 no longer spells out its composition, so its percentage has no owner again: %s", row)
	}
	if parts[7] != "3" {
		t.Errorf("stage 3 divides by %s; the plan's stage 3 has three lines, so a row that divides by "+
			"another number is averaging something else", parts[7])
	}
	// The three parts have to be distinct, because three copies of one part
	// would make the mean a constant that never moves. The comparison is on
	// the labels, not the values: "1.00 audit + 1.00 manager + 0.99
	// federation" is three parts that happen to include two equal scores,
	// and reading it as one part twice would forbid the row from ever
	// reaching a full row (decision 258).
	seen := map[string]bool{}
	for i, raw := range []string{parts[2], parts[4], parts[6]} {
		label := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "*"))
		if label == "" {
			t.Errorf("stage 3's composition part %d has no label, so it cannot be told apart from the others: %s", i+1, row)
			continue
		}
		if seen[label] {
			t.Errorf("stage 3's composition lists %s twice: %s", label, row)
		}
		seen[label] = true
	}
}

// roundOne rounds to one decimal place, which is the precision the ledger
// quotes every one of these numbers at.
func roundOne(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', 1, 64)
}

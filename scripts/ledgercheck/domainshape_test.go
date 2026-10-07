package ledgercheck

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestEveryDomainShapeInTheProgressSectionIsTheTreesOwn is the fourteenth
// gate.
//
// The stage 3 row of the progress table said, for a long time, that the
// control plane has "55 domains, 55 declared cross-domain edges and 7
// mutually-dependent cycles" — the reading decision 111 took. Every one of
// those three numbers had since been overturned by decisions 112 through 118,
// and the same row says so three sentences later: it walks through the cycles
// being cut one at a time. A reader who stops at the first sentence reads a
// shape this tree no longer has.
//
// A gate could have caught it. The three numbers have a machine answer —
// `make domain-check` prints them — and the ledger quotes them as a triple in
// the form "N 域 / M 边 / K 环". That form is what this gate reads, in the
// progress section only, and every triple it finds has to be the triple the
// tree actually has.
//
// Two deliberate limits. It does not read a bare "N 域", because a progress
// row is allowed to say "决策 111 当时量出 55 个域" — naming a past reading is
// honest and required. It only reads the triple, which is written down as the
// current shape. And it reads the progress section only, because the same
// numbers appear throughout the decision records as what they were at the
// time, which are not stale and must not be edited into the present.
var domainShapeRE = regexp.MustCompile(`(\d+) 域 / (\d+) 边 / (\d+) 环`)

// quotedRE matches text the ledger is quoting rather than asserting. The
// mutation tables in the progress section are full of it — a row reads
// "台账改成「57 域 / 43 边 / 0 环」 | 红 | red: the ledger states 57 domains"
// — and that is a record of a check firing, not a shape this repository
// claims. Checking a quoted number is how a gate starts failing on the
// evidence of its own past work.
var quotedRE = regexp.MustCompile("「[^」]*」|`[^`]*`")

// domaincheckRE reads the shape out of `go run ./scripts/domaincheck .`.
// Running the tool rather than recomputing it here is the point: a second
// implementation of the domain map is a second thing that can disagree, and
// this gate exists because a number drifted away from the code once already.
var domaincheckRE = regexp.MustCompile(
	`(\d+) domains, \d+ shared, (\d+) declared edges, (\d+) declared cycles`)

func TestEveryDomainShapeInTheProgressSectionIsTheTreesOwn(t *testing.T) {
	cmd := exec.Command("go", "run", "./scripts/domaincheck", ".")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./scripts/domaincheck . failed (%v); the tree's own domain shape could not be read, so no triple in the progress section can be checked:\n%s",
			err, out)
	}
	m := domaincheckRE.FindSubmatch(out)
	if m == nil {
		t.Fatalf("domaincheck printed no shape to read:\n%s", out)
	}
	wantDomains, _ := strconv.Atoi(string(m[1]))
	wantEdges, _ := strconv.Atoi(string(m[2]))
	wantCycles, _ := strconv.Atoi(string(m[3]))

	progress := quotedRE.ReplaceAllString(progressSection(t), "")
	var problems []string
	seen := 0
	for _, match := range domainShapeRE.FindAllStringSubmatch(progress, -1) {
		seen++
		gotDomains, _ := strconv.Atoi(match[1])
		gotEdges, _ := strconv.Atoi(match[2])
		gotCycles, _ := strconv.Atoi(match[3])
		if gotDomains != wantDomains || gotEdges != wantEdges || gotCycles != wantCycles {
			problems = append(problems, fmt.Sprintf(
				"the progress section states %s 域 / %s 边 / %s 环, but this tree has %d / %d / %d; "+
					"if the number is a past reading, say so where it stands instead of leaving it to be read as the present one",
				match[1], match[2], match[3], wantDomains, wantEdges, wantCycles))
		}
	}
	if seen == 0 {
		t.Fatal("the progress section states no domain shape outside a quotation, so this gate is looking at nothing")
	}
	if len(problems) > 0 {
		t.Errorf("a domain shape in the progress section is not the tree's own:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

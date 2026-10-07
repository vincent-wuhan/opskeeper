package main

import (
	"regexp"
	"strings"
	"testing"
)

// TestTheCheapBucketIsEmpty is the assertion this tool was built for.
//
// Decision 228 sorted the remaining edges by hand and filed `flow -> scheduler`
// — one interface, implemented in the consuming domain — under "data shape, has
// to move with its GORM entity". Decision 230 cut it anyway and found it cheap.
// That is the second time a hand table filed a port as a data shape, so the
// hand table is no longer trusted on this question and the answer is read off
// the tree instead.
//
// This test pins the answer: of the edges that remain, none is a port whose
// implementation sits on the consumer's side of the boundary, which is the
// only shape decisions 227 and 230 turned out to be able to cut for the price
// of moving an interface into core/floor.
//
// It is a count rather than a list of names on purpose. A list would have to be
// edited every time an edge is cut, and an edit to a list is an edit nobody
// makes carefully. A count changes only when the tree does, and when it goes
// red the message names the edges that made it worth looking.
func TestTheCheapBucketIsEmpty(t *testing.T) {
	rows := seamRows(t)
	var cheap []string
	for _, r := range rows {
		if r.verdict == "port-opposite" {
			cheap = append(cheap, r.from+" -> "+r.to)
		}
	}
	if len(cheap) > 0 {
		t.Errorf("these edges select only interfaces and hold the implementation themselves, "+
			"so moving the port to core/floor removes the edge (decisions 227, 230): %v\n"+
			"  each is a decision-228-class misclassification waiting to happen again", cheap)
	}
}

// TestEveryEdgeIsClassified keeps the report honest about its own coverage. An
// unrecognised verdict would mean the tool is reading something it does not
// understand, and a report that shrugs is worse than no report, because it
// reads like an answer.
func TestEveryEdgeIsClassified(t *testing.T) {
	for _, r := range seamRows(t) {
		switch r.verdict {
		case "data", "mixed", "port-here", "port-opposite":
		default:
			t.Errorf("%s -> %s is classified %q, which is not a verdict this tool knows how to "+
				"act on; a reader would have no way to tell that from a shrug", r.from, r.to, r.verdict)
		}
	}
}

// TestTheReportCoversEveryDeclaredEdge ties the report to the graph. A report
// that silently dropped an edge could report an empty cheap bucket without
// meaning one, and that is the single conclusion nobody should draw from a
// tool this new.
func TestTheReportCoversEveryDeclaredEdge(t *testing.T) {
	if got, want := len(seamRows(t)), len(edges); got != want {
		t.Errorf("the seam report has %d rows for %d declared edges; a report that drops an edge "+
			"can report an empty cheap bucket without meaning one", got, want)
	}
}

type seamSummary struct {
	from, to, verdict string
}

func seamRows(t *testing.T) []seamSummary {
	t.Helper()
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("reading the shipped tree: %v", err)
	}
	var sb strings.Builder
	printSeams(&sb, sources, defaultRules())
	var out []seamSummary
	for _, line := range strings.Split(sb.String(), "\n") {
		m := seamRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, seamSummary{verdict: m[1], from: m[3], to: m[4]})
	}
	if len(out) == 0 {
		t.Fatal("the seam report produced no rows for the shipped tree")
	}
	return out
}

var seamRowRE = regexp.MustCompile(
	`^  (data|mixed|port-here\??|port-opposite)\s+(\d+)\s+(\S+)\s+->\s+(\S+)`)

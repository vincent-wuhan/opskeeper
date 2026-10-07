package main

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// The ledger quotes the control plane's domain graph in its current-state
// section, and until decision 175 that number existed only as a clause at the
// end of a paragraph — in the same paragraph as decision 119's "42 条边",
// which was also true when written and is not true now.
//
// Both numbers are in the progress section because that section's cells carry
// narrative as well as state. Rewriting the 42 would falsify a decision
// record, and the repository has a standing rule about exactly that. So the
// fix is not to change a number; it is to give the current one a place and an
// owner, and this is the owner.
//
// The owner is this package and not scripts/ledgercheck, because domains,
// declared edges and declared cycles are only computable here. Another package
// counting them would be a second implementation of this walk, and two
// implementations of a domain walk is how a repository ends up with two
// answers and one gate.
//
// Like the module count this guards (decision 172), it is a gate rather than
// a note because the number only moves when somebody adds or removes a
// domain or a declared edge — and both of those are deliberate acts that edit
// a table. It is a gate and not a fresh line count because manager's size
// changes on every ordinary feature; §4.57 already ruled that one out.

const ledgerPath = "../../docs/opskeeper2-architecture.md"

// ledgerGraphRE reads the one row that states the graph as current.
//
// It is anchored on the row's own label rather than on the number's shape,
// because the numbers appear dozens of times in the ledger — as history, as
// intermediate readings, as the "before" of a change. Only the labelled row
// is a claim about the tree as it stands.
var ledgerGraphRE = regexp.MustCompile(`(?m)^\| 控制面域图 \| \*\*(\d+) 域 / (\d+) 边 / (\d+) 环\*\* \|`)

// TestTheLedgerStatesTheDomainGraphThisTreeHas is the check.
//
// A domain is added by editing the grouping map and a declared edge by
// editing .go-arch-lint.yml. Both leave the gate green — the new domain has
// no cycles, so the boundaries still hold — and both leave the ledger
// quietly wrong. That is the shape this refuses: a number that is quoted in
// the section people read to decide what is left, with nothing that notices.
func TestTheLedgerStatesTheDomainGraphThisTreeHas(t *testing.T) {
	_, stats, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the control plane: %v", err)
	}
	r := defaultRules()

	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	m := ledgerGraphRE.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("the ledger has no `| 控制面域图 | **N 域 / N 边 / N 环** |` row in its progress section; "+
			"the domain graph is the size most often quoted when arguing about the manager split, and "+
			"this tree has %d domains, %d declared edges and %d declared cycles", stats.domains, len(r.edges), len(r.cycles))
	}

	for _, c := range []struct {
		what string
		want int
		said string
	}{
		{"domains", stats.domains, m[1]},
		{"declared edges", len(r.edges), m[2]},
		{"declared cycles", len(r.cycles), m[3]},
	} {
		said, err := strconv.Atoi(c.said)
		if err != nil {
			t.Fatalf("unreadable %s count %q in the ledger: %v", c.what, c.said, err)
		}
		if said != c.want {
			t.Errorf("the ledger states %d %s; this tree has %d (make domain-check)",
				said, c.what, c.want)
		}
	}
}

// A domain that spans two modules is still one domain.
//
// Cutting the release floor out of core/manager put a bounded context on both
// sides of a module line: middleware is the HTTP chain in core/domains
// (server/middleware) and the chain plus the tool adapters in core/manager
// (middleware/adapter). The walk used to count domains per tree and add the
// two totals, so that one context was counted twice and the counter printed
// 58 while the graph, the layering levels and the release report all said 57.
//
// The number that reached the ledger was the sum, so this is the test that
// keeps the counter and the graph from drifting apart again: the counter must
// equal the number of distinct context names, and the graph is what the rest
// of the tool believes.
func TestAContextSpanningTwoModulesIsCountedOnce(t *testing.T) {
	sources, stats, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the control plane: %v", err)
	}
	names := map[string]bool{}
	for _, src := range sources {
		if d := domainOf(src.path); d != "" {
			names[d] = true
		}
	}
	if stats.domains != len(names) {
		t.Errorf("the walk counted %d domains but the tree holds %d distinct names %v; "+
			"a context that spans two modules is one context, and a counter that says "+
			"otherwise is the number the ledger is asked to repeat",
			stats.domains, len(names), sortedKeys(names))
	}
	if g := buildGraph(sources, defaultRules()); len(g.levels()) != stats.domains {
		t.Errorf("the counter says %d domains and the graph places %d; the two must be "+
			"the same set, because the layering table has one row per name and cannot "+
			"hold a name twice",
			stats.domains, len(g.levels()))
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

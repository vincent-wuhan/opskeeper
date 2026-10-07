package main

import (
	"strings"
	"testing"
)

// fixture builds one source file inside the manager module.
func fixture(path string, imports ...string) source {
	return source{path: managerPrefix + path, imports: imports}
}

// testFile is a fixture marked as a test file.
func testFile(path string, imports ...string) source {
	src := fixture(path, imports...)
	src.test = true
	return src
}

// testRules is a world with three domains and nothing declared. Every test
// adds the one thing it is about, so a failure names the rule that changed
// rather than whatever else the fixture happened to contain.
func testRules() rules {
	return rules{
		shared: map[string]string{"shared": "a BC-free tree"},
		edges:  map[edge]string{},
		cycles: map[[2]string]string{},
	}
}

// world is the same three domains with no edges between them, so the only
// violations a test sees are the ones it caused.
func world(extra ...source) []source {
	base := []source{
		fixture("biz/alpha/a.go"),
		fixture("biz/beta/b.go"),
		fixture("shared/thing.go"),
	}
	return append(base, extra...)
}

// withEdge declares one direction and puts it in the tree.
func withEdge(r rules, from, to string, sources ...source) (rules, []source) {
	r.edges[edge{from, to}] = from + " asks " + to + " for something"
	sources = append(sources, fixture("biz/"+from+"/a.go", managerPrefix+"biz/"+to+"/b"))
	return r, sources
}

func TestDomainOfCollapsesTheLayerTreesOntoOneName(t *testing.T) {
	cases := map[string]string{
		managerPrefix + "biz/alert":              "alert",
		managerPrefix + "model/alert":            "alert",
		managerPrefix + "server/mcp/middleware":  "mcp",
		managerPrefix + "service/alert/whatever": "alert",
		managerPrefix + "data/audit/store":       "audit",
		// The layer is not part of a domain's identity. These four are the
		// same two domains, and decision 276 is what made them explicit:
		// a port that borrowed model/topology's structs cut nothing, and
		// only the deleted declared edge revealed it.
		managerPrefix + "biz/hitl":       "hitl",
		managerPrefix + "model/hitl":     "hitl",
		managerPrefix + "biz/topology":   "topology",
		managerPrefix + "model/topology": "topology",
		// A domain-shaped context names itself, and its own layer
		// directories do not turn it into five domains.
		managerPrefix + "iam/biz/user": "iam",
		managerPrefix + "iam/model":    "iam",
		// Trees that are already domain-shaped.
		managerPrefix + "dataguard":      "dataguard",
		managerPrefix + "knowledge/rule": "knowledge",
		// The shared floor is not a domain of this tree. It used to be
		// manager/pkg and is the core/base module now (decision 221), and a
		// path under it names no bounded context here — which is why it is
		// absent from sharedDomains rather than listed with a domain that
		// has no packages.
		"github.com/vincent-wuhan/opskeeper/core/base/pkg/audit": "",
		// A package sitting directly in a layer directory is almost
		// certainly a mistake, but it must still be checked rather than
		// silently escaping every rule.
		managerPrefix + "biz": "biz",
		// Outside the module entirely.
		"github.com/MichaelKinsy/PiG/ai":                     "",
		"gopkg.in/yaml.v3":                                   "",
		managerPrefix + "":                                   "",
		"github.com/vincent-wuhan/opskeeper/core/edge/biz/x": "",
	}
	for path, want := range cases {
		if got := domainOf(path); got != want {
			t.Errorf("domainOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestAPairIsCanonicalWhateverOrderItArrivesIn(t *testing.T) {
	// Map iteration hands pairs over in whichever order the first importing
	// package used, so a table written one way has to match an observation
	// the other way round. A checker that does not canonicalise fails on an
	// unrelated reordering, which is how a boundary check gets disabled.
	if pair("beta", "alpha") != pair("alpha", "beta") {
		t.Fatal("pair() is not order-independent")
	}
	if got := pair("beta", "alpha"); got != [2]string{"alpha", "beta"} {
		t.Fatalf("pair = %v, want the lexicographically smaller name first", got)
	}
}

func TestAConsistentTreeIsGreen(t *testing.T) {
	if got := check(world(), testRules()); len(got) != 0 {
		t.Fatalf("a tree that matches its rules was reported: %v", got)
	}
}

func TestADeclaredEdgeIsNotAViolation(t *testing.T) {
	r, sources := withEdge(testRules(), "alpha", "beta", world()...)
	if got := check(sources, r); len(got) != 0 {
		t.Fatalf("a declared edge was reported: %v", got)
	}
}

func TestAnUndeclaredCrossDomainImportIsAViolation(t *testing.T) {
	sources := world(fixture("biz/alpha/a.go", managerPrefix+"biz/beta/b"))
	got := check(sources, testRules())
	if len(got) != 1 {
		t.Fatalf("want exactly one violation, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "alpha imports beta") {
		t.Errorf("the violation does not name the offending direction: %s", got[0])
	}
}

func TestAnImportInsideOneDomainIsNeverAViolation(t *testing.T) {
	// The whole point of collapsing the layer trees: alert's five
	// directories are one domain, so a rule inside alert is not a
	// cross-domain import no matter which layer it sits in.
	sources := []source{
		fixture("biz/alpha/a.go", managerPrefix+"model/alpha/m", managerPrefix+"service/alpha/s"),
		fixture("model/alpha/m/m.go"),
		fixture("service/alpha/s/s.go"),
		fixture("shared/thing.go"),
	}
	if got := check(sources, testRules()); len(got) != 0 {
		t.Fatalf("an intra-domain import across layers was reported: %v", got)
	}
}

func TestASharedDomainNeedsNoDeclarationButItsOwnEdgesDo(t *testing.T) {
	r := testRules()
	sources := world(fixture("biz/alpha/a.go", managerPrefix+"shared/thing"))
	if got := check(sources, r); len(got) != 0 {
		t.Fatalf("a shared tree required a declaration: %v", got)
	}
	// Being depended upon is a privilege, not a pass: the shared tree's
	// own outbound edges are declared like anyone else's.
	sources = append(sources, fixture("shared/thing.go", managerPrefix+"biz/beta/b"))
	if got := check(sources, r); len(got) != 1 {
		t.Fatalf("the shared tree's own undeclared edge was let through: %v", got)
	}
}

func TestATwoWayDependencyIsACycleAndNeedsDeclaring(t *testing.T) {
	sources := world(
		fixture("biz/alpha/a.go", managerPrefix+"biz/beta/b"),
		fixture("biz/beta/b.go", managerPrefix+"biz/alpha/a"),
	)
	got := check(sources, testRules())
	// Both directions are undeclared edges, and the pair they form is an
	// undeclared cycle: three findings, because an undeclared cycle is two
	// undeclared edges plus the thing that makes them worth reporting.
	if len(got) != 3 {
		t.Fatalf("want three violations, got %d: %v", len(got), got)
	}
	var sawCycle bool
	for _, v := range got {
		if strings.Contains(v, "reach each other both ways") {
			sawCycle = true
		}
	}
	if !sawCycle {
		t.Error("the mutual pair was not reported as a cycle")
	}
}

func TestADeclaredCycleIsNotAViolation(t *testing.T) {
	r, sources := withEdge(testRules(), "alpha", "beta", world()...)
	r.edges[edge{"beta", "alpha"}] = "the other direction, with its own reason"
	r.cycles[pair("alpha", "beta")] = "the debt, and what cutting it would take"
	sources = append(sources, fixture("biz/beta/b.go", managerPrefix+"biz/alpha/a"))
	if got := check(sources, r); len(got) != 0 {
		t.Fatalf("a fully declared cycle was reported: %v", got)
	}
}

func TestATableThatNoLongerMatchesTheTreeIsAViolation(t *testing.T) {
	// The other direction of the same rule. An entry nothing uses is a
	// justification for something that is no longer true, which is worse
	// than no entry at all: it reads like an answer to a question nobody is
	// asking any more, and it is the kind of thing nobody prunes.
	r := testRules()
	r.edges[edge{"alpha", "beta"}] = "alpha used to ask beta"
	r.cycles[pair("alpha", "beta")] = "and they used to reach each other both ways"
	got := check(world(), r)
	if len(got) != 2 {
		t.Fatalf("want two stale-entry violations, got %d: %v", len(got), got)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "no longer happens") {
		t.Error("the stale edge was not reported")
	}
	if !strings.Contains(joined, "no longer reach each other both ways") {
		t.Error("the stale cycle was not reported")
	}
}

func TestASharedDomainThatNoLongerExistsIsAViolation(t *testing.T) {
	// The shared list rots the same way the edge list does, and it rots
	// more quietly: a tree that stops being shared is now a normal domain
	// that every dependent has to declare, and nothing says so.
	r := testRules()
	sources := []source{fixture("biz/alpha/a.go"), fixture("biz/beta/b.go")}
	got := check(sources, r)
	if len(got) != 1 {
		t.Fatalf("want one violation for the vanished shared tree, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "shared is declared a shared domain") {
		t.Errorf("the violation does not name the shared domain: %s", got[0])
	}
}

func TestATestFileIsNotHeldToTheRule(t *testing.T) {
	// .go-arch-lint.yml excludes _test.go for the same reason: a test that
	// reaches across a boundary is the boundary being exercised, and
	// holding it to the production rule would make the honest tests the
	// forbidden ones.
	sources := world(
		testFile("biz/beta/b_test.go", managerPrefix+"biz/alpha/a"),
		fixture("biz/beta/b.go", managerPrefix+"biz/alpha/a"),
	)
	got := check(sources, testRules())
	if len(got) != 1 {
		t.Fatalf("want one violation from the production file only, got %d: %v", len(got), got)
	}
}

func TestAnEmptyTreeIsNotSilentlyGreen(t *testing.T) {
	// check() over nothing has no opinion, so parseTree is the place that
	// has to refuse. A walk that found no files would otherwise report
	// "every domain boundary holds" for a tree it never looked at, which is
	// the failure mode this repository keeps having to fix.
	if _, _, err := parseTree(t.TempDir(), managerPrefix, defaultRules()); err == nil {
		t.Fatal("parsing an empty directory reported success")
	}
}

func TestTheShippedTablesDescribeTheShippedTree(t *testing.T) {
	// The same walk main() does, so the checker's own gate is a test: a
	// new cross-domain import, a new cycle, or a table that stopped
	// matching the tree all fail here with a message naming the domain.
	sources, stats, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the manager module: %v", err)
	}
	if got := check(sources, defaultRules()); len(got) != 0 {
		t.Fatalf("the shipped tables disagree with the shipped tree:\n%s", strings.Join(got, "\n"))
	}
	if stats.domains < 40 {
		t.Errorf("only %d domains found; the walk is broken, not the tree small", stats.domains)
	}
}

// The sentence domaincheck prints on every run is the only place a reader
// is told that the layer is not part of a domain's identity, and decision
// 276 is the knife that made it worth saying. It is built from layerDirs,
// so this test is about the derivation: every layer the walk understands has
// to appear in the sentence, because a layer added to the map without
// appearing there is a reader who is told a rule that has a hole in it.
func TestTheLayerRuleSentenceNamesEveryLayerTheWalkKnows(t *testing.T) {
	got := layerRuleSentence()
	// The layer list is the clause between "segment after " and " — so ",
	// and only that clause. Checking the whole sentence is a check that
	// passes for the wrong reason: the example pair also contains "model/",
	// so a list that had dropped the model layer still looked complete.
	// That false pass was found by mutating the sentence, not by reading
	// the test.
	start := strings.Index(got, "segment after ")
	end := strings.Index(got, " — so ")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("the printed rule has no layer clause: %s", got)
	}
	list := got[start+len("segment after ") : end]
	for name := range layerDirs {
		if !strings.Contains(list, name+"/") {
			t.Errorf("the printed layer list does not mention %q: %q", name, list)
		}
	}
	if !strings.Contains(got, "SAME domain") {
		t.Errorf("the printed rule does not say the two layers are one domain: %s", got)
	}
	// It has to name a concrete pair, because a rule that names none reads
	// as a rule about nothing in particular — and the pair it names has to
	// be a real one, checked through domainOf rather than against a list
	// written here. A second example was tried and dropped: a sentence that
	// grows one clause per knife eventually stops being read, which is the
	// whole reason this is printed instead of filed.
	first := strings.Index(got, " — so ")
	if first < 0 {
		t.Fatalf("the printed rule has no example clause: %s", got)
	}
	example := got[first+len(" — so ") : strings.Index(got[first:], " are the SAME domain")+first]
	pair := strings.SplitN(example, " and ", 2)
	if len(pair) != 2 {
		t.Fatalf("the example clause is not a pair: %q", example)
	}
	left, right := domainOf(managerPrefix+pair[0]), domainOf(managerPrefix+pair[1])
	if left == "" || left != right {
		t.Errorf("the printed example %q / %q does not name one domain (%q vs %q)", pair[0], pair[1], left, right)
	}
}

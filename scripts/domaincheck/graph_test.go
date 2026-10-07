package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// graph_test.go covers the report modes, which check() has no opinion about.
//
// The gate answers "is the declared shape true". Stage 3's second item —
// split the monolith into pieces that evolve independently — needs a
// different answer, and it needs it to be arithmetic rather than taste:
// how deep does the tree go, and how many declared edges would a proposed
// grouping sever. A tool that answers those questions is only worth
// keeping if it cannot quietly disagree with the gate it sits next to, so
// the first tests here pin the two to the same reading of the same tree.

func groupingFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "split")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write the grouping: %v", err)
	}
	return path
}

func reportCut(t *testing.T, g *domainGraph, body string) string {
	t.Helper()
	return reportCutWithHard(t, g, body, nil)
}

// reportCutWithHard is reportCut with the hard-constraint set the caller wants
// priced. The shipped set is nil by default so the edge tests, which are about
// cost, do not have to know the constraint exists.
func reportCutWithHard(t *testing.T, g *domainGraph, body string, hard map[edge]string) string {
	t.Helper()
	grouping, order, err := loadGrouping(groupingFile(t, body))
	if err != nil {
		t.Fatalf("load the grouping: %v", err)
	}
	var buf bytes.Buffer
	g.printCut(&buf, grouping, order, hard)
	return buf.String()
}

// sizedFixture is a file with a package and a line count, which is what
// the size axis measures. fixture() leaves both empty because the edge
// tests do not care about them.
func sizedFixture(pkg string, lines int) source {
	return source{
		path:  managerPrefix + pkg + "/f.go",
		pkg:   pkg,
		lines: lines,
	}
}

func reportSize(t *testing.T, g *domainGraph) string {
	t.Helper()
	var buf bytes.Buffer
	g.printSize(&buf)
	return buf.String()
}

func TestASplitIsPricedInLinesAndNotInFiles(t *testing.T) {
	// Many small files and a few large ones are different propositions,
	// and only one of them is what a split actually has to move.
	sources := world(
		sizedFixture("biz/alpha/split", 900),
		sizedFixture("biz/beta/split", 100),
	)
	out := reportSize(t, buildGraph(sources, testRules()))
	if !strings.Contains(out, "biz/alpha/split") {
		t.Fatalf("the biggest package is missing from the report:\n%s", out)
	}
	alpha, beta := strings.Index(out, "biz/alpha/split"), strings.Index(out, "biz/beta/split")
	if alpha > beta {
		t.Errorf("the larger package should be reported first:\n%s", out)
	}
	if !strings.Contains(out, "900") {
		t.Errorf("the report should price a package in lines:\n%s", out)
	}
}

func TestATestFileIsInvisibleToTheSizeToo(t *testing.T) {
	// The weight already ignores test files. If the size axis counted
	// them, the two halves of the same report would disagree about which
	// code is the code.
	test := sizedFixture("biz/alpha", 100000)
	test.test = true
	sources := world(test, sizedFixture("biz/beta", 40))
	out := reportSize(t, buildGraph(sources, testRules()))
	if strings.Contains(out, "100000") {
		t.Errorf("only non-test lines belong in the size report:\n%s", out)
	}
	if !strings.Contains(out, "where the code is (40 lines") {
		t.Errorf("the one real file should be the whole total:\n%s", out)
	}
}

func TestAPackageHiddenInsideAWideDomainIsStillPricedOnItsOwn(t *testing.T) {
	// The reason the second axis exists. domainOf collapses
	// biz/alpha/* and biz/beta/* onto two names, and a domain that is one
	// enormous package beside forty small ones reads as merely a domain.
	sources := world(sizedFixture("biz/alpha/huge", 5000))
	for i := 0; i < 40; i++ {
		sources = append(sources, sizedFixture("biz/alpha/small", 10))
	}
	out := reportSize(t, buildGraph(sources, testRules()))
	if !strings.Contains(out, "biz/alpha/huge") {
		t.Fatalf("the one large package must be reported by its own name:\n%s", out)
	}
	if !strings.Contains(out, "5000") {
		t.Errorf("its size is the finding and should be printed:\n%s", out)
	}
}

func TestADomainOfOnePackageSaysThereIsNothingUnderItToSplit(t *testing.T) {
	// A domain that is a single package cannot be decomposed without
	// first being given more than one. Reporting it as a peer of a wide
	// domain prices a rename as a decomposition.
	sources := world(sizedFixture("biz/alpha", 300))
	sources = append(sources, sizedFixture("biz/beta", 200), sizedFixture("biz/beta/more", 100))
	out := reportSize(t, buildGraph(sources, testRules()))
	if !strings.Contains(out, "nothing under it to split") {
		t.Errorf("a one-package domain should be marked as such:\n%s", out)
	}
}

func reportVerdict(t *testing.T, g *domainGraph, body string) string {
	t.Helper()
	grouping, _, err := loadGrouping(groupingFile(t, body))
	if err != nil {
		t.Fatalf("load the grouping: %v", err)
	}
	var buf bytes.Buffer
	g.printCutVerdict(&buf, grouping)
	return buf.String()
}

func TestAGroupThatIsOnePackageIsReportedAsARename(t *testing.T) {
	// A group holding a single package has not been split. The domain
	// name is new and the code is where it was, and saying so is cheaper
	// than a week of moving files to find out.
	sources := world(sizedFixture("biz/alpha", 900), sizedFixture("biz/beta", 100))
	out := reportVerdict(t, buildGraph(sources, testRules()),
		"one = alpha\n")
	if !strings.Contains(out, "a name, not a split") {
		t.Errorf("a one-package group should be called a rename:\n%s", out)
	}
}

func TestAGroupIsJudgedAgainstItsOwnSizeNotTheTrees(t *testing.T) {
	// The bug this locks: a catch-all "everything else" group holds most
	// of the tree, so a tree-wide share reads about 80% and washes out
	// the group that is actually the problem. Each group is therefore
	// priced against itself.
	sources := world(
		sizedFixture("biz/alpha", 9000),
		sizedFixture("biz/alpha/sub", 10),
		sizedFixture("biz/beta", 500),
	)
	out := reportVerdict(t, buildGraph(sources, testRules()),
		"big = alpha\nrest = beta\n")
	if !strings.Contains(out, "is biz/alpha") {
		t.Errorf("the dominant package inside a group should be named:\n%s", out)
	}
	if !strings.Contains(out, "of the group is biz/alpha") {
		t.Errorf("the share should be of the group, not of the tree:\n%s", out)
	}
}

func TestAGroupSpreadAcrossManyPackagesIsNotCalledARename(t *testing.T) {
	// The marking has to be earned. A wide group is a group.
	sources := world(sizedFixture("biz/alpha", 300), sizedFixture("biz/beta", 200))
	out := reportVerdict(t, buildGraph(sources, testRules()),
		"wide = alpha, beta\n")
	if strings.Contains(out, "a name, not a split") {
		t.Errorf("a two-package group is not a rename:\n%s", out)
	}
}

func TestTheReportSeesEveryEdgeTheGateWouldForbid(t *testing.T) {
	// One undeclared import is one edge. If buildGraph dropped or invented
	// edges relative to check(), the two tools would answer "how tangled is
	// this tree" differently on the same day, and the report would be the
	// one nobody believed.
	sources := world(
		fixture("biz/alpha/a.go", managerPrefix+"biz/beta/b"),
		fixture("biz/beta/b.go", managerPrefix+"pkg/pack"),
	)
	g := buildGraph(sources, testRules())
	// Two undeclared edges, and no mutual pair, so no cycle finding.
	if got := check(sources, testRules()); len(got) != 2 {
		t.Fatalf("fixture assumption changed: want 2 violations, got %d: %v", len(got), got)
	}
	for _, want := range [][2]string{{"alpha", "beta"}, {"beta", "pkg"}} {
		if g.weight[edge{want[0], want[1]}] == 0 {
			t.Errorf("edge %s -> %s is missing from the report's graph", want[0], want[1])
		}
	}
	// alpha and beta are not declared, so nothing here is a cycle, and the
	// report must say so rather than inventing an entanglement.
	var buf bytes.Buffer
	g.printStructure(&buf)
	if !strings.Contains(buf.String(), "the graph is a DAG") {
		t.Error("a tree with no mutual pair was reported as entangled")
	}
}

func TestAWeightIsAnImportCountNotAnEdgeCount(t *testing.T) {
	// The reason weight exists: an edge held up by one import and an edge
	// held up by twenty-five are not the same seam, and a split that treats
	// them alike is a guess. Ranking by edge count would flatten exactly
	// the distinction the report is for.
	heavy := fixture("biz/alpha/a.go",
		managerPrefix+"biz/beta/b", managerPrefix+"biz/beta/c", managerPrefix+"biz/beta/d")
	g := buildGraph(world(heavy), testRules())
	if got := g.weight[edge{"alpha", "beta"}]; got != 3 {
		t.Fatalf("alpha -> beta weighs %d, want 3 import statements", got)
	}
	rs := g.rank()
	for _, v := range rs {
		if v.name != "beta" {
			continue
		}
		if v.in != 3 || v.inEdges != 1 {
			t.Errorf("beta in %d across %d edges, want 3 across 1: the count and the weight are being mixed up", v.in, v.inEdges)
		}
		return
	}
	t.Fatal("beta is not in the ranking at all")
}

func TestASharedTreeIsADomainButNotAnEdge(t *testing.T) {
	// The gate lets anything import the shared trees without declaring it.
	// The report has to keep that: counting shared imports as edges would
	// make every domain look tangled and every proposed split look
	// expensive, in a way the gate does not agree with.
	sources := world(fixture("biz/alpha/a.go", managerPrefix+"shared/thing"))
	g := buildGraph(sources, testRules())
	for e := range g.weight {
		if e.to == "shared" || e.from == "shared" {
			t.Errorf("the shared tree was counted as the edge %s -> %s", e.from, e.to)
		}
	}
	if !g.domains["shared"] {
		t.Error("the shared tree is not listed as a domain, so the report cannot say it is one")
	}
}

func TestATestFileIsInvisibleToTheWeight(t *testing.T) {
	// Same rule as the gate, same reason: a test reaching across a boundary
	// is the boundary being exercised, and pricing a split on the back of
	// one would invent a seam that production does not have.
	sources := world(testFile("biz/beta/b_test.go", managerPrefix+"biz/alpha/a"))
	g := buildGraph(sources, testRules())
	for e := range g.weight {
		t.Errorf("a test file put the edge %s -> %s in the graph", e.from, e.to)
	}
	if !g.domains["beta"] {
		t.Error("a domain that only exists in tests is missing from the domain list")
	}
}

func TestTheLayeringPutsADependedOnDomainAboveWhoeverDependsOnIt(t *testing.T) {
	// Longest path, not shortest: a domain sitting on top of the tree can
	// change with everything under it, which is what makes it expensive to
	// split. Levels are the bound on which pieces can move first.
	sources := world(
		fixture("biz/alpha/a.go", managerPrefix+"biz/beta/b"),
		fixture("biz/beta/b.go", managerPrefix+"pkg/pack"),
	)
	lv := buildGraph(sources, testRules()).levels()
	if lv["alpha"] != 0 || lv["beta"] != 1 || lv["pkg"] != 2 {
		t.Fatalf("levels = %v, want alpha 0, beta 1, pkg 2 (one below its deepest dependent)", lv)
	}
}

func TestAnIslandIsLevelZeroAndSaysNothing(t *testing.T) {
	// A domain nobody imports is not a problem, but it is a fact the report
	// has to hold: silently dropping it would make the domain count in the
	// headline a smaller number than the gate's.
	sources := world(fixture("biz/alpha/a.go", managerPrefix+"biz/beta/b"))
	g := buildGraph(sources, testRules())
	if got := g.levels()["shared"]; got != 0 {
		t.Errorf("an island sits at level %d, want 0", got)
	}
	var buf bytes.Buffer
	g.printStructure(&buf)
	if !strings.Contains(buf.String(), "3 domains") {
		t.Errorf("the headline does not count the island:\n%s", buf.String())
	}
}

func TestAMutualPairIsReportedAsEntangled(t *testing.T) {
	// A cycle means the layering has no answer, and the report that stays
	// quiet about it hands people a number computed from a broken walk.
	sources := world(
		fixture("biz/alpha/a.go", managerPrefix+"biz/beta/b"),
		fixture("biz/beta/b.go", managerPrefix+"biz/alpha/a"),
	)
	var buf bytes.Buffer
	buildGraph(sources, testRules()).printStructure(&buf)
	if !strings.Contains(buf.String(), "<->") {
		t.Errorf("two domains reaching each other were not reported as entangled:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "the graph is a DAG") {
		t.Error("a tree with a mutual pair still calls itself a DAG")
	}
}

func TestAMutualPairDoesNotHangTheLayering(t *testing.T) {
	// The peel has no indeg-0 node left in a cycle, so an unguarded walk
	// spins. It must terminate: a report that hangs is a report that is
	// disabled, and then nobody is checking anything.
	sources := world(
		fixture("biz/alpha/a.go", managerPrefix+"biz/beta/b"),
		fixture("biz/beta/b.go", managerPrefix+"biz/alpha/a"),
	)
	g := buildGraph(sources, testRules())
	lv := g.levels()
	if len(lv) != 3 {
		t.Fatalf("layering covers %d domains, want 3", len(lv))
	}
}

func TestAGroupingIsPricedByWhatItActuallyCuts(t *testing.T) {
	// Hand arithmetic, checked against the tool: alpha and beta inside one
	// group, pkg outside, and the gate says alpha -> beta is declared and
	// beta -> pkg is not, so the grouping severs exactly one import.
	r, sources := withEdge(testRules(), "alpha", "beta", world()...)
	sources = append(sources, fixture("biz/beta/b.go", managerPrefix+"pkg/pack"))
	g := buildGraph(sources, r)
	out := reportCut(t, g, "tier1 = alpha, beta\ntier2 = pkg, shared\n")
	// alpha -> beta stays inside tier1; beta -> pkg crosses.
	if !strings.Contains(out, "1 import statements stay inside a group, 1 cross one") {
		t.Errorf("the split was not priced as the imports say it should be:\n%s", out)
	}
	if !strings.Contains(out, "beta") || !strings.Contains(out, "pkg") || !strings.Contains(out, "tier1 -> tier2") {
		t.Errorf("the severed edge is not named with the groups it severs:\n%s", out)
	}
}

func TestAGroupingThatMissesADomainIsToldSo(t *testing.T) {
	// The most likely way a proposal is wrong is a domain nobody assigned.
	// Counting only the assigned ones would understate the price and make a
	// broken grouping look cheap.
	r, sources := withEdge(testRules(), "alpha", "beta", world()...)
	g := buildGraph(sources, r)
	out := reportCut(t, g, "tier1 = alpha, beta\n")
	// Only the shared tree is left out: alpha and beta were named.
	if !strings.Contains(out, "1 domain(s) the grouping does not mention: shared") {
		t.Errorf("the forgotten domains were not reported:\n%s", out)
	}
}

func TestTheReportNamesTheGroupingsOwnTypos(t *testing.T) {
	// A name in the file that is not a domain here is either a typo or a
	// domain that used to exist. Both mean the proposal was not written
	// against this tree, and both are silent failures otherwise.
	r, sources := withEdge(testRules(), "alpha", "beta", world()...)
	g := buildGraph(sources, r)
	out := reportCut(t, g, "tier1 = alpha, beta, alhpa\ntier2 = pkg, shared\n")
	if !strings.Contains(out, "not domains here: alhpa") {
		t.Errorf("a name that is not a domain was accepted:\n%s", out)
	}
}

func TestAGroupingThatCutsNothingIsPricedAtZeroNotSkipped(t *testing.T) {
	// "This split is free" is the most valuable sentence this tool can
	// print, and it only means something if it is printed.
	r, sources := withEdge(testRules(), "alpha", "beta", world()...)
	g := buildGraph(sources, r)
	out := reportCut(t, g, "tier1 = alpha, beta, pkg, shared\n")
	if !strings.Contains(out, "1 import statements stay inside a group, 0 cross one") {
		t.Errorf("a split that cuts nothing was not priced at zero:\n%s", out)
	}
	if !strings.Contains(out, "does not cut a single edge") {
		t.Errorf("a free split did not say so plainly:\n%s", out)
	}
}

func TestAGroupingHasToBeReadableBeforeItIsPriced(t *testing.T) {
	// Every one of these is a file a person typed by hand. A malformed one
	// priced as "zero crossings" would be the worst possible answer.
	cases := map[string]string{
		"no groups at all": "\n\n# just a comment\n",
		"no equals sign":   "tier1 alpha, beta\n",
		"empty group name": " = alpha, beta\n",
		"a domain in two":  "tier1 = alpha\ntier2 = beta, alpha\n",
	}
	for name, body := range cases {
		if _, _, err := loadGrouping(groupingFile(t, body)); err == nil {
			t.Errorf("%s: a grouping that cannot be read was accepted", name)
		}
	}
}

func TestAGroupingWithNoCrossingsIsNotAnError(t *testing.T) {
	// A proposal that turns out to be free must not be rejected as
	// malformed. Otherwise the only way to learn a split is free is to
	// already know it.
	r, sources := withEdge(testRules(), "alpha", "beta", world()...)
	g := buildGraph(sources, r)
	if _, _, err := loadGrouping(groupingFile(t, "tier1 = alpha, beta, shared\n")); err != nil {
		t.Fatalf("a well-formed grouping was rejected: %v", err)
	}
	if out := reportCut(t, g, "tier1 = alpha, beta, shared\n"); strings.Contains(out, "not domains here") {
		t.Errorf("a grouping of exactly the real domains was reported as containing names that are not domains:\n%s", out)
	}
}

func TestTheShippedTreeIsADagFourLevelsDeep(t *testing.T) {
	// The real number, pinned. Decision 118 closed the last cycle, so the
	// layering has an answer; if a future edge reopens one this test is the
	// thing that says the headline changed, and the DAG claim in the report
	// stops being true.
	sources, stats, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the manager module: %v", err)
	}
	g := buildGraph(sources, defaultRules())
	lv := g.levels()
	if len(lv) != stats.domains {
		t.Fatalf("layering covers %d of %d domains; some were never placed", len(lv), stats.domains)
	}
	max := 0
	for _, l := range lv {
		if l > max {
			max = l
		}
	}
	// Seven until decision 235, which cut alert -> edge and systemhealth ->
	// edge. Both were leaf-reaching edges into a domain that sits in the
	// middle of the layering, so removing them did not just drop two
	// entries — it collapsed the two longest chains the graph had, and the
	// depth fell by exactly the number of edges cut. That is the assertion
	// being a number and not a direction: it cannot tell a deliberate cut
	// from an accidental layer, which is why the edge count is pinned on
	// the very next lines and the two have to move together.
	//
	// Decision 241 took it from five to four, and the mechanism is the one
	// worth recording: it did not shorten a chain, it removed a link from
	// the top of one. Levels here are longest-path-from-a-source, so a
	// domain's level is set by what *depends on it*, not by what it depends
	// on. `pluginimport` sat at level 1 for exactly one reason — the
	// marketplace route imported it — and `aiops` sat at level 2 for exactly
	// one reason: `pluginimport` imported it. Cut the first edge and
	// `pluginimport` has in-degree zero, so it drops to level 0, and
	// `aiops` follows it up. The graph is one level shallower and not one
	// dependency weaker; what changed is that a domain nothing depends on
	// any more no longer holds another one down.
	//
	// Decision 281 did not change the depth — cutting frontierbound -> edge
	// removed an edge between two domains that were already on the same level,
	// which is the case where the layer count is the wrong number to watch.
	//
	// Decision 280 took it from four to three, and it is the same mechanism a
	// second time, which is why it is worth saying rather than just moving the
	// number. That cut removed `imbridge -> aiops`, the last edge into aiops,
	// so aiops has in-degree zero and drops to level 0. Its other consumer
	// direction is gone too, so nothing is left holding the level that sat
	// under it. Three levels with 51 of 57 domains on level 0 is not a graph
	// that got simpler in a meaningful way — it is a graph where one more
	// domain has nothing depending on it, and the depth fell because the
	// thing that was deep was a single chain of length four with two members.
	//
	// Decision 283 took it from three to two, and for the third time by the
	// same mechanism, which is why the number keeps moving without the
	// architecture getting simpler. Cutting `aiops -> edge` left edge with an
	// in-degree of zero, so edge drops to level 0 — and edge was the only
	// thing holding `device` at level 2, because `edge -> device` is still
	// declared. Two levels with 54 of 57 domains on level 0: what is left of
	// the depth is one domain (device) that something depends on, and it
	// depends on exactly one thing. Read the in-degree column for this
	// number; the level count is downstream of it.
	if max+1 != 2 {
		t.Errorf("the tree is %d levels deep, want 3: a level appearing or disappearing changes what a split costs", max+1)
	}
	var buf bytes.Buffer
	g.printStructure(&buf)
	if !strings.Contains(buf.String(), "the graph is a DAG") {
		t.Error("the shipped tree is no longer a DAG and the report does not say so")
	}
	// 8 = decision 118's 42, plus the edge decision 123 added when the root
	// side of the cluster channel became a domain of its own, minus the eight
	// since cut: decision 227 (frontierbound -> metric, whose port moved
	// next to HostMetricPoint in core/floor/tunnel), decision 229
	// (imbridge -> iam, the only remaining edge that selected nothing but a
	// constant, cut by moving the role vocabulary down to tenantctx), decision
	// 230 (flow -> scheduler, whose MissedRunInfo/Repo port moved down to
	// core/floor/scheduler because the flow data store is the only
	// implementor and lives in a different domain), decision 235's two
	// (alert -> edge and systemhealth -> edge, both cut by moving the
	// six-column node presence projection down to core/domain and giving
	// both domains the one-method domain.EdgeQuery port), decision 236's
	// (integration -> grafana, cut the same way: the consumer had already
	// written its own three-method interface and the only thing its
	// signature named from the producer was one three-field struct), and
	// decision 238's (mcp -> aiops, the narrowest of the seven: the consumer
	// named two event structs and nothing else, and the whole edge existed
	// because those two lived in the decorators package instead of
	// core/domain, which is where the plan already puts event contracts), and
	// decision 240's (aiops -> skill, where the consumer had already written
	// its own one-method SkillRunner and the only thing its signature named
	// from the producer were three structs — one of which, Caller, is
	// declared seven times in this repository for five different things, so
	// the three moved down under names that say which skill they belong to)., and
	// decision 241's (marketplace -> pluginimport, where an HTTP route that
	// never called a method on the converter named its two parameter types in
	// a function signature the composition root had already handed it — and
	// the price column's "two types" turned out to be a closure of six, two
	// of which were a shared vocabulary type that one third domain had
	// already copied by hand), decision 242's (grafana -> monitor, where the
	// mirror wanted six columns of an eleven-column entity) and decision
	// 247's (agentteams -> alert, where a recovery closure wanted two columns
	// of a twenty-five-column entity and a status constant that was a
	// precondition of the question rather than a filter it set) and decision
	// 248's (webshell -> device, where the whole edge was one parameter that
	// could not vary: the handler passed Host on its only call site, and the
	// test beside it asserted the value arriving was Host) and decision 249's
	// (agentteams -> mcp, where the dependency was three package functions —
	// which no port can narrow, because a package function cannot be injected
	// — and a six-field credential struct for routes that read three fields)
	// and decision 251's (webshell -> edge, where the consumer read one
	// column of a fifteen-column row purely to compare it against a status
	// constant, so the row itself was the only reason that boundary existed
	// — and where handing that one column back as a string deleted a nil
	// check that no repository in this tree could ever trigger) and
	// decision 252's (pluginimport -> aiops, where the importer wanted six
	// strings and was importing a 692-line container detector plus two
	// parsed skill trees it never read — and where the two calls it used to
	// make meant the `kind` in the conversion report came from a second
	// reading of the same directory) and decision 253's
	// (report -> aiops, where the consumer had already written its own
	// one-method port and the only thing its signature named from the
	// producer was two structs holding fourteen fields of which it read
	// ten — and where a value return deleted a nil-worker branch that no
	// implementation in this tree could produce) and decision 254's
	// (chatdiagnose -> aiops, where the port was already written in the
	// consumer's own types and the only thing crossing the boundary was
	// the adapter — and the adapter was living inside the consumer, so the
	// domain had a correct seam and a package dependency at the same time,
	// held open by a line that said `_ = aiopsmodel.Message{}`), and decision
	// 257's (systemhealth -> alert, where the health probe held a
	// nineteen-column rule model and read one column of it — Enabled, plus
	// len() on the slice — and threaded a Caller that both alert methods
	// declare as `_`, so the port asked for rows nobody read and an identity
	// nobody read) and decision 258's (grafana -> setting, where the port
	// signature was already all built-in types — string, bool, error — so
	// `*setting.Service` satisfied it structurally and no adapter was
	// needed; the only thing this edge named was the type of one field, which
	// means this cut needed no seam at all, only a port and thirteen
	// constants) and decision 259's, which is the first one on this list that
	// cut no edge at all: the `aiopsconfig` domain was one hundred and ten
	// lines of translation whose only caller was the composition root, so it
	// moved there and the domain stopped existing. A cut edge lowers this
	// number the same way a deleted domain does, and the two are not the same
	// event — the ledger's 34-of-34 counts cuts, so it needed saying.
	// Decisions 260 through 280 are not narrated one at a time above. The
	// shape they share is worth the one paragraph: almost all of them cut a
	// port down to the columns its consumer actually read, moved the shared
	// half of the type down to core/domain or core/base so both sides named
	// the same declaration, and deleted the adapter that the cut made
	// unnecessary. 276 (aiops -> topology) also established that
	// `model/<domain>` and `biz/<domain>` are one domain rather than two, so
	// moving a type across that layer boundary counts for nothing. 279 (loop
	// -> alert) took loop's out-degree to zero, the only domain in the tree
	// that has it, and 280 (imbridge -> aiops) took aiops's in-degree to zero
	// and moved the only remaining `Event` target out of the shared-symbol
	// trap column. 281 (frontierbound -> edge) then emptied the trap column
	// entirely: the cut took Usecase's second consuming domain with it, so the
	// name left the report rather than becoming single-owner, and its finding
	// moved to a direct assertion. 282 (nodeagent -> nodefleet) took
	// nodefleet's in-degree to zero, which is the second of the two remaining
	// single-inbound domains to come free — and the reason the in-degree
	// column is the one worth watching is that it is the only one that
	// predicts a domain's release independence without a judgement call.
	// 283 (aiops -> edge) is the third of that kind, and the largest of
	// them: the RCA tool set held `*edgebiz.Usecase` outright across
	// twenty-two production files and named three more types besides, and
	// the cut moved two of them (ChangeEvent, PluginRow) down to
	// core/domain while the estate became domain.EdgeCatalog. It is also
	// what emptied the production cross-domain import column from 84 to
	// 58 — the largest single move in that column since the count
	// started being tracked, and the reason the number is worth reading
	// next to the edge count rather than instead of it.
	//
	// A cut edge lowers this number the same way an added one raises it,
	// which is the whole reason this assertion is written as a number and
	// not as a direction.
	if !strings.Contains(buf.String(), "8 edges") {
		t.Errorf("the edge count moved; the ledger in docs/opskeeper2-architecture.md is now wrong:\n%s", firstLines(buf.String(), 6))
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func TestAGroupingCanWrapAndACommentMaySitInTheMiddle(t *testing.T) {
	// The real proposal file has 38 names in a group and the reasoning in
	// comments next to them, so it is hard-wrapped. A reader that refuses
	// that pushes people back to the one version nobody can review.
	grouping, order, err := loadGrouping(groupingFile(t, `
# why these two groups
core = alpha, beta,
       # device sits under everything
       device,
       edge
apps = everythingelse
`))
	if err != nil {
		t.Fatalf("a wrapped grouping was rejected: %v", err)
	}
	if len(order) != 2 || order[0] != "core" {
		t.Fatalf("groups = %v, want core first", order)
	}
	for _, want := range []string{"alpha", "beta", "device", "edge"} {
		if grouping[want] != "core" {
			t.Errorf("%s landed in %q, want core", want, grouping[want])
		}
	}
	if grouping["everythingelse"] != "apps" {
		t.Error("the group after a wrapped one was lost")
	}
}

func TestWrappingJoinsNamesWithACommaNotASpace(t *testing.T) {
	// The bug this pins: the trailing comma is the author's request to
	// continue, and it is also the only separator between the last name on
	// one line and the first on the next. Swallowing it turns "mcp," +
	// "monitor" into the domain "mcp monitor", which exists in no tree —
	// and the price that comes back is confidently wrong.
	grouping, _, err := loadGrouping(groupingFile(t, "core = aiops, mcp,\n       monitor\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, want := range []string{"aiops", "mcp", "monitor"} {
		if _, ok := grouping[want]; !ok {
			t.Errorf("%s is missing; the names were joined instead of continued: %v", want, grouping)
		}
	}
	if len(grouping) != 3 {
		t.Errorf("grouping has %d names, want 3: %v", len(grouping), grouping)
	}
}

func TestAGroupingThatStopsMidListIsRejected(t *testing.T) {
	// A trailing comma with nothing after it is a file that was cut off in
	// an editor, not a proposal. Pricing it would drop every domain on the
	// missing line from the count without saying so.
	if _, _, err := loadGrouping(groupingFile(t, "core = alpha, beta,\n")); err == nil {
		t.Error("a grouping that stops mid-list was accepted")
	}
}

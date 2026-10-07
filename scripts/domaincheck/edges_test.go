package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The edge report is the only one here that reads method calls, and it exists
// because two earlier measurements got that question wrong in the same
// direction: both counted a domain that calls four methods as one that calls
// none, because they keyed on the field's name. So most of what follows is
// about the three ways a call can be missed, and one of them is a call that
// should not be counted at all.

// treeWith writes a two-domain control-plane tree to a temp directory and
// parses it the way the shipped tree is parsed. Parsing a fixture through
// parser.ParseFile and hand-building the `used` map instead would test a
// second implementation of the import walk, and a second implementation is
// exactly the thing that was wrong twice.
func treeWith(t *testing.T, files map[string]string) []source {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("make %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	sources, _, err := parseTree(dir, managerPrefix, testRules())
	if err != nil {
		t.Fatalf("parse the fixture tree: %v", err)
	}
	return sources
}

const producerPkg = managerPrefix + "biz/producer"

// floorPkg is a module path BELOW the control plane. It is only ever an import
// in a fixture: no file is written for it, and none has to be, because the
// question the closure walk asks about it is answered by its path.
const floorPkg = "github.com/vincent-wuhan/opskeeper/core/floor/bundle"

func edgeReport(t *testing.T, sources []source, r rules) string {
	t.Helper()
	var sb strings.Builder
	printEdges(&sb, sources, r)
	return sb.String()
}

// TestAMethodCalledThroughAFieldNamedAfterNothingFindsTheEdge is the regression
// for the measurement that reported a confident zero.
//
// `frontierbound` names its edge dependency `w.EdgeUC`. The regex that keyed
// on the field's name matched `edges` and `edge` and nothing else, so a domain
// calling four methods came out as a domain calling none — and a zero here
// reads exactly like "this edge is cheap to cut". The report resolves the
// method from the producer's own declarations instead, so the field's name is
// not in the question.
func TestAMethodCalledThroughAFieldNamedAfterNothingFindsTheEdge(t *testing.T) {
	sources := treeWith(t, map[string]string{
		"biz/producer/p.go": `package producer

import "context"

type Usecase struct{ n int }

func (u *Usecase) HandleHeartbeat(ctx context.Context) error { return nil }

func (u *Usecase) HandleOffline(ctx context.Context) error  { return nil }
`,
		"biz/consumer/c.go": `package consumer

import (
	"context"

	pb "` + producerPkg + `"
)

type worker struct{ EdgeUC *pb.Usecase }

func (w *worker) tick(ctx context.Context) error {
	if err := w.EdgeUC.HandleHeartbeat(ctx); err != nil {
		return err
	}
	return w.EdgeUC.HandleOffline(ctx)
}
`,
	})
	r := testRules()
	r.edges[edge{from: "consumer", to: "producer"}] = "a fixture edge"
	out := edgeReport(t, sources, r)

	for _, want := range []string{"HandleHeartbeat", "HandleOffline"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %s.\n%s\nA method reached through a field "+
				"whose name shares nothing with the domain is still a method on this edge, and "+
				"missing it is how a four-method edge was once reported as a zero-method one",
				want, out)
		}
	}
}

// TestACallInsideACommentIsNotACall is the other half of reading the tree
// instead of grepping it.
//
// server/webshell has carried `// edges, _ := h.edges.List(` for long enough
// that a text search counted webshell as a consumer of edge. It is a comment.
// A report whose numbers decide what gets cut next cannot be built on a
// matcher that reads prose.
func TestACallInsideACommentIsNotACall(t *testing.T) {
	sources := treeWith(t, map[string]string{
		"biz/producer/p.go": `package producer

import "context"

type Usecase struct{}

func (u *Usecase) List(ctx context.Context) error { return nil }
`,
		"biz/consumer/c.go": `package consumer

import (
	"context"

	pb "` + producerPkg + `"
)

type h struct{ edges *pb.Usecase }

// Kept for reference while the projection lands:
//
//	_, _ = h.edges.List(context.Background())
func (h *h) live(ctx context.Context) error { return nil }
`,
	})
	r := testRules()
	r.edges[edge{from: "consumer", to: "producer"}] = "a fixture edge"
	out := edgeReport(t, sources, r)

	if strings.Contains(out, "List ") {
		t.Errorf("the report counted a commented-out call.\n%s", out)
	}
	// The import itself is still live — the struct field names the type — so
	// the row legitimately reports one selected type. What it must not report
	// is the method, and the row has to say so in words rather than just
	// omitting a column a reader is looking for.
	if !strings.Contains(out, "no method") {
		t.Errorf("an edge whose only call is commented out must read as selecting a type and no "+
			"method, so a reader can tell it apart from an edge nothing was ever measured "+
			"against.\n%s", out)
	}
}

// TestASameNamedCallInAFileThatDoesNotImportTheProducerIsNotThisEdge keeps the
// method column from being a guess. The column is documented as an upper
// bound, and the bound only means something if it is bounded by something.
func TestASameNamedCallInAFileThatDoesNotImportTheProducerIsNotThisEdge(t *testing.T) {
	sources := treeWith(t, map[string]string{
		"biz/producer/p.go": `package producer

import "context"

type Usecase struct{}

func (u *Usecase) HandleHeartbeat(ctx context.Context) error { return nil }
`,
		"biz/consumer/c.go": `package consumer

import "context"

type unrelated struct{}

func (u *unrelated) HandleHeartbeat(ctx context.Context) error { return nil }

func run(ctx context.Context) error { return (&unrelated{}).HandleHeartbeat(ctx) }
`,
	})
	r := testRules()
	r.edges[edge{from: "consumer", to: "producer"}] = "a fixture edge"
	out := edgeReport(t, sources, r)

	if strings.Contains(out, "HandleHeartbeat") {
		t.Errorf("a same-named method on an unrelated type, in a file that does not import the "+
			"producer, was attributed to this edge.\n%s", out)
	}
}

// TestTheMethodColumnSaysItIsAnUpperBound keeps the report honest about the one
// thing about it that is not exact. A column of method names printed without
// that caveat would be read as a count, and a count is a thing people plan
// against.
func TestTheMethodColumnSaysItIsAnUpperBound(t *testing.T) {
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("reading the shipped tree: %v", err)
	}
	out := edgeReport(t, sources, defaultRules())
	if !strings.Contains(out, "UPPER BOUND") {
		t.Errorf("the report does not say its method column is an upper bound.\n%s\n"+
			"Without that line every number below it reads as exact, and the ones that are not "+
			"are the ones a reader will plan a refactor around", out)
	}
	// Every hit names its file, so a wrong attribution is one glance away
	// rather than something the reader has to take on trust.
	var hits int
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "        method ") {
			hits++
			fields := strings.Fields(line)
			if len(fields) < 4 || !strings.Contains(fields[len(fields)-1], "/") {
				t.Errorf("method line does not name the file it was seen in: %q", line)
			}
		}
	}
	if hits == 0 {
		t.Fatal("no method lines in the report for the shipped tree; it stopped reading calls")
	}
}

var edgeRowRE = regexp.MustCompile(`^\s+(\d+)\s+(\S+)\s+->\s+(\S+)\s+(.*)$`)

// TestTheEdgeReportCoversEveryDeclaredEdge is the same tie to the graph the seam
// report has. A report that dropped an edge could report an empty cheap bucket
// without meaning one, and that is the conclusion nobody should draw from a
// tool this new.
func TestTheEdgeReportCoversEveryDeclaredEdge(t *testing.T) {
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("reading the shipped tree: %v", err)
	}
	out := edgeReport(t, sources, defaultRules())
	seen := map[edge]bool{}
	for _, line := range strings.Split(out, "\n") {
		m := edgeRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		seen[edge{from: m[2], to: m[3]}] = true
	}
	for e := range edges {
		if !seen[e] {
			t.Errorf("the edge report has no row for the declared edge %s -> %s", e.from, e.to)
		}
	}
	if len(seen) != len(edges) {
		t.Errorf("the edge report has %d rows for %d declared edges", len(seen), len(edges))
	}
}

// TestTheReportIsSortedCheapestFirst is the deliverable, so it is asserted
// rather than assumed: the ordering is what tells a reader which edge to cut
// today, and an unsorted table with the same content in it is a different
// report.
func TestTheReportIsSortedCheapestFirst(t *testing.T) {
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("reading the shipped tree: %v", err)
	}
	out := edgeReport(t, sources, defaultRules())
	prev := -1
	rows := 0
	for _, line := range strings.Split(out, "\n") {
		m := edgeRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n := atoiOrFail(t, m[1])
		if n < prev {
			t.Errorf("the report goes from %d symbols back up to %d at %s -> %s; the ordering is "+
				"the deliverable and a table out of order is a different report",
				prev, n, m[2], m[3])
		}
		prev = n
		rows++
	}
	if rows == 0 {
		t.Fatal("the edge report produced no rows for the shipped tree")
	}
}

// TestTheTwoEdgesDecision235CutAreNotOnTheList is the reason the report exists
// in this shape, stated as a fact about the tree rather than as a comment
// about a commit. `alert` and `systemhealth` each called one method on the edge
// domain and passed it a bare limit; they now hold a one-method port declared
// in core/domain. If either edge ever comes back, it comes back with a
// compiler error or a red gate, not with a reader noticing.
func TestTheTwoEdgesDecision235CutAreNotOnTheList(t *testing.T) {
	for _, e := range []edge{{from: "alert", to: "edge"}, {from: "systemhealth", to: "edge"}} {
		if _, ok := edges[e]; ok {
			t.Errorf("%s -> %s is declared again; if it came back, say what now needs it, because "+
				"both of those domains hold a one-method port in core/domain and neither needs "+
				"the edge domain's twenty methods", e.from, e.to)
		}
	}
}

// The closure is what a type drags behind it, and it is the half of the price
// the direct count could not see. Everything below is about that half, and
// about the two ways the first version of the walk was wrong: it followed
// unexported fields, which a dependent cannot read, and it read "declared
// elsewhere" off a string comparison, which called a type foreign when the
// producer declared it too.

// priceOf pulls the price column out of the row for one edge.
func priceOf(t *testing.T, out string, from, to string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		m := edgeRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[2] == from && m[3] == to {
			return atoiOrFail(t, m[1])
		}
	}
	t.Fatalf("no row for %s -> %s in:\n%s", from, to, out)
	return 0
}

// TestAFieldTypeTheConsumerNeverNamesIsInThePrice is the regression for the
// number this round set out to correct.
//
// `marketplace` writes down two symbols when it uses `pluginimport`: an
// `Options` and a `Report`. It does not write down that `Report` has a `Kind`
// field of a type declared in a third domain and a `Warnings` field of a type
// declared in two. Moving `Report` to core/domain means moving those too, so
// the edge is six, not two — and a ranking built on two sends the next cut at
// the wrong edge.
func TestAFieldTypeTheConsumerNeverNamesIsInThePrice(t *testing.T) {
	sources := treeWith(t, map[string]string{
		"biz/producer/p.go": `package producer

type Kind string

const KindNone Kind = "none"

type Report struct {
	Name     string
	Kind     Kind
	Warnings []LoadWarning
}

type LoadWarning struct {
	Path   string
	Reason string
}
`,
		"biz/consumer/c.go": `package consumer

import pb "` + producerPkg + `"

type Handler struct{ Root string }

// The consumer names exactly one symbol and reads the other two out of it.
func (h *Handler) show(r *pb.Report) string { return string(r.Kind) }
`,
	})
	r := testRules()
	r.edges[edge{from: "consumer", to: "producer"}] = "a fixture edge"
	out := edgeReport(t, sources, r)

	if got := priceOf(t, out, "consumer", "producer"); got != 3 {
		t.Errorf("the edge is priced at %d, and it should be 3: one named type plus the two "+
			"types its fields name.\n%s\nA struct that names another type cannot be moved "+
			"without moving that type, and a price that says otherwise is a price that "+
			"picks the wrong edge to cut next", got, out)
	}
	for _, want := range []string{"closure Kind", "closure LoadWarning"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not list %q in the closure.\n%s", want, out)
		}
	}
	// The path is what makes a closure checkable against the code. A bare
	// list of names could be in the report and still be wrong.
	if !strings.Contains(out, "via Report.Kind") {
		t.Errorf("a closure hit is printed without the field path that reaches it.\n%s", out)
	}
}

// TestAClosureIntoAnotherDomainIsNamedIs the other half: not counting is not
// enough, the reader has to be told the cut leaves the edge it was priced
// under. `Pusher` is an interface here, so nothing would follow from it — the
// drag has to be visible on the row.
func TestAClosureIntoAnotherDomainIsNamed(t *testing.T) {
	sources := treeWith(t, map[string]string{
		"biz/other/o.go": `package other

type Bundle struct {
	Digest string
}
`,
		// The producer itself reaches into the other domain, which is the
		// shape the real federation edge has: `federation.Member` carries a
		// field typed `federation.Bundle`, and that type is declared in
		// aiops. The drag is in the producer's own shape, so it is in the
		// closure of anything that moves the shape.
		"biz/producer/p.go": `package producer

import ob "github.com/vincent-wuhan/opskeeper/core/manager/biz/other"

type Member struct {
	Cluster string
	Bundle  ob.Bundle
}
`,
		"biz/consumer/c.go": `package consumer

import (
	pb "` + producerPkg + `"
	ob "github.com/vincent-wuhan/opskeeper/core/manager/biz/other"
)

type Handler struct{ Members []pb.Member }

// The consumer holds a slice of the producer's type and also imports the
// other domain directly, so the Bundle below is not the only way in.
func (h *Handler) count(b ob.Bundle) int { return len(b.Digest) }
`,
	})
	r := testRules()
	r.edges[edge{from: "consumer", to: "producer"}] = "a fixture edge"
	out := edgeReport(t, sources, r)

	if !strings.Contains(out, "reaches other") {
		t.Errorf("an edge whose closure reaches a second domain is printed without saying so.\n%s\n"+
			"\"cheap\" and \"cheap and drags a third domain in\" are different amounts of work, "+
			"and the row is where a reader finds out which one this is", out)
	}
	// The row summary names the domain; this line names the type. Both are
	// printed because a reader deciding whether to cut an edge needs to know
	// which domain it lands in and what is landing there, and a row that
	// carried only the first would send them to the producer to find out.
	if !strings.Contains(out, "ob.Bundle") || !strings.Contains(out, "lives in other, not in producer") {
		t.Errorf("a cross-domain closure hit is printed without naming the type or the domain it "+
			"lives in.\n%s", out)
	}
}

// TestAnUnexportedFieldIsNotInThePrice is the mutation the first version of
// this walk needed.
//
// `audit.Usecase` is four unexported fields, and following them reported it as
// dragging nineteen domains — every one of them reached through `repo` and
// `log`. A dependent in another package cannot read those fields, so cutting
// the edge does not move them, and the price was inflated by exactly the
// fields that were never in question.
func TestAnUnexportedFieldIsNotInThePrice(t *testing.T) {
	sources := treeWith(t, map[string]string{
		"biz/producer/p.go": `package producer

type Repo interface{ List() error }

type Usecase struct {
	repo Repo
	Name string
}
`,
		"biz/consumer/c.go": `package consumer

import pb "` + producerPkg + `"

type Handler struct{ UC *pb.Usecase }

func (h *Handler) run() string { return h.UC.Name }
`,
	})
	r := testRules()
	r.edges[edge{from: "consumer", to: "producer"}] = "a fixture edge"
	out := edgeReport(t, sources, r)

	if got := priceOf(t, out, "consumer", "producer"); got != 1 {
		t.Errorf("the edge is priced at %d, and it should be 1.\n%s\n"+
			"Usecase has one exported field and one unexported one, and the unexported one is "+
			"not reachable from another package, so it is not part of what a cut moves",
			got, out)
	}
	if strings.Contains(out, "closure Repo") {
		t.Errorf("an unexported field's type was counted in the closure.\n%s", out)
	}
	// The shape column still lists it, and the header has to say that the two
	// columns differ on purpose — otherwise the shape looks like the price.
	if !strings.Contains(out, "Usecase") {
		t.Errorf("the selected type is no longer printed at all.\n%s", out)
	}
}

// TestATypeNoDomainDeclaresIsDropped keeps the walk from counting things that
// were never going to move. `time.Time` belongs to the standard library; a
// price that included it would be charging for a type the cut cannot touch.
func TestATypeNoDomainDeclaresIsDropped(t *testing.T) {
	sources := treeWith(t, map[string]string{
		"biz/producer/p.go": `package producer

import "time"

type Row struct {
	At   time.Time
	Name string
}
`,
		"biz/consumer/c.go": `package consumer

import pb "` + producerPkg + `"

type Handler struct{ Rows []pb.Row }

func (h *Handler) first() pb.Row { return h.Rows[0] }
`,
	})
	r := testRules()
	r.edges[edge{from: "consumer", to: "producer"}] = "a fixture edge"
	out := edgeReport(t, sources, r)

	if got := priceOf(t, out, "consumer", "producer"); got != 1 {
		t.Errorf("the edge is priced at %d, and it should be 1.\n%s\n"+
			"a time.Time field does not become a piece of work when a domain boundary moves",
			got, out)
	}
}

// TestAMarketplacePluginImportIsNotATwoSymbolEdge used to be a shipped-tree
// guard and is not any more, and saying why is more useful than the test was.
//
// It read the real edge report and refused a row that priced
// `marketplace -> pluginimport` as the two types the route names. It was
// right, and decision 241 then cut the edge — so the row it was reading is
// gone, and a guard that t.Fatalf's on a missing row would have failed on the
// cut that satisfied it.
//
// The claim it made is not lost. That the price column undercounts a shape
// whose fields point into a third domain is a property of the pricer, and it
// is asserted at fixture level by TestAClosureIntoAnotherDomainIsNamed, where
// it costs four lines instead of a whole tree walk. What is asserted at
// shipped-tree level now is the thing the fixture cannot know: that no file
// in the marketplace domain imports the converter at all, which is
// TestNoMarketplaceFileImportsTheConverter below.

// TestATypeTheProducerAlsoDeclaresIsNotReportedAsSomebodyElses is decision 233's
// shape arriving in the closure walk.
//
// `pluginimport.Decision` is declared in pluginimport and in three other
// domains. The first version of this walk compared the declaring domain to the
// producer's with `!=`, so a name the producer declares itself came back as
// "lives in agentteams|control|nodefleet|pluginimport, not in pluginimport" —
// which is false in the direction that costs the most, because it tells a
// reader the type has to come from somewhere it already is.
func TestATypeTheProducerAlsoDeclaresIsNotReportedAsSomebodyElses(t *testing.T) {
	sources := treeWith(t, map[string]string{
		"biz/producer/p.go": `package producer

type Decision struct {
	Field    string
	Question string
}

type Report struct {
	Decisions []Decision
}
`,
		"biz/other/o.go": `package other

type Decision struct {
	Field string
	Other string
}
`,
		"biz/consumer/c.go": `package consumer

import pb "` + producerPkg + `"

type Handler struct{ Report *pb.Report }

func (h *Handler) unanswered() int { return len(h.Report.Decisions) }
`,
	})
	r := testRules()
	r.edges[edge{from: "consumer", to: "producer"}] = "a fixture edge"
	out := edgeReport(t, sources, r)

	if strings.Contains(out, "not in producer") {
		t.Errorf("a type the producer itself declares is reported as living in another domain.\n%s\n"+
			"string inequality is not membership: the producer is one of the four domains that "+
			"declare Decision, and the mover is already holding it", out)
	}
	// The ambiguity is still worth saying, because "which of the four shapes"
	// is a real question — but it is a different sentence from "not yours".
	//
	// Decision 274 changed what this sentence can say, and the change is the
	// point rather than a rewording. The old marker named the pipe-joined list
	// of every domain declaring the name, because the walk itself could not
	// say which one it had followed: it resolved by bare name, so a name with
	// four owners came back as four owners. The walk now follows the field's
	// import path, which means it knows, and a marker that said "not settled"
	// would now be understating what the tool knows. So the test asks for the
	// stronger sentence — the ambiguity is named AND the resolved owner is
	// named — and a marker that only did the first would fail here.
	if !strings.Contains(out, "also declares this name") {
		t.Errorf("a name declared by two domains is reported with no word about the "+
			"ambiguity.\n%s", out)
	}
	if !strings.Contains(out, "the shape above is producer's") {
		t.Errorf("the marker does not say which owner's shape the walk followed, so a reader "+
			"cannot tell which of the two declarations the price is about.\n%s", out)
	}
}

// TestAFloorTypeIsNotAttributedToAControlPlaneDomainThatSharesItsName is
// decision 274, the half of the fix that is not about wording.
//
// The fixture is the real one: a producer whose field is typed with a type
// from a module BELOW the control plane, while a control-plane domain declares
// a type of the same name. The documented rule is that a type the cut does not
// move is not part of the cut's price, and before this test there was no way
// to tell whether the rule held: the walk resolved homes by bare name, found
// the same-named control-plane declaration, and reported a foreign drag that
// does not exist.
//
// The assertion is on the ABSENCE of a foreign line rather than on a price,
// because a price can be right for the wrong reason. "not in producer" and
// "reaches" are both claims about a domain boundary, and a fixture that
// asserted only the total would pass whether the closure was dropped, counted
// as the producer's own, or counted as somebody else's.
func TestAFloorTypeIsNotAttributedToAControlPlaneDomainThatSharesItsName(t *testing.T) {
	sources := treeWith(t, map[string]string{
		"biz/producer/p.go": `package producer

import floor "` + floorPkg + `"

type Member struct {
	Issued floor.Bundle
	Plain  string
}
`,
		// A control-plane domain that declares a Bundle of its own. This is
		// the collision that used to capture the field above.
		"biz/other/o.go": `package other

type Bundle struct {
	Tool string
	Argv []string
}
`,
		"biz/consumer/c.go": `package consumer

import pb "` + producerPkg + `"

type Handler struct{ Member *pb.Member }

func (h *Handler) unanswered() int { return len(h.Member.Plain) }
`,
	})
	r := testRules()
	r.edges[edge{from: "consumer", to: "producer"}] = "a fixture edge"
	out := edgeReport(t, sources, r)

	if strings.Contains(out, "reaches") {
		t.Errorf("a floor type was attributed to a control-plane domain that declares a type "+
			"of the same name, and the report says this cut reaches outside its edge.\n%s\n"+
			"a bare type name does not say which package it came from; the field's import path "+
			"does, and cutting this edge does not move a floor type", out)
	}
	// Matched on the row prefix rather than on the word: the report's own
	// header explains what a closure is, so a bare Contains("closure") reads
	// the explanation and calls it a finding. An earlier version of this
	// assertion did exactly that, which is worth writing down because the
	// failure it produced — a red test on a correct tool — is the kind that
	// gets "fixed" by deleting the assertion.
	if strings.Contains(out, "\n        closure ") {
		t.Errorf("the floor type is in the closure at all.\n%s\n"+
			"the rule this violates is the one the report already states: a type from a module "+
			"below the control plane is not a cost, because cutting an edge does not move it", out)
	}
}

// TestTheEdgeDecision238CutIsNotOnTheList states the fact about the tree rather
// than about a commit, in the same shape as decision 235's test.
//
// `server/mcp` needed two event structs and an interface, and all three lived
// in `biz/aiops/tools/decorators` — the package that also holds governance,
// review gates, rate limiters and untrusted-output marking. So the edge
// existed to carry two structs with no behaviour in them. They now live in
// core/domain, which the plan already gives event contracts to.
func TestTheEdgeDecision238CutIsNotOnTheList(t *testing.T) {
	if _, ok := edges[edge{from: "mcp", to: "aiops"}]; ok {
		t.Error("mcp -> aiops is declared again; if it came back, say what now needs it, because " +
			"the tool-call audit seam is a port in core/domain and the only thing server/mcp " +
			"took from aiops was the two shapes that port is made of")
	}
}

// TestTheEdgeDecision240CutIsNotOnTheList states the fact about the tree, in
// the same shape as decisions 235's and 238's.
//
// `biz/aiops/tools/skill_bridge.go` had already written its own one-method
// `SkillRunner`; the only thing its signature named from the skill service were
// three structs. Those now live in core/domain as SkillCaller / SkillExecution
// / SkillOutcome, and the bridge holds `domain.SkillExecutor` by alias.
//
// The rename is the part worth remembering: `Caller` is declared seven times in
// this repository for five different things, so putting an eighth one in the
// namespace every domain shares would have made an already-ambiguous name
// ambiguous in one more place — and core/domain has a test that refuses a bare
// `Caller` for exactly that reason.
func TestTheEdgeDecision240CutIsNotOnTheList(t *testing.T) {
	if _, ok := edges[edge{from: "aiops", to: "skill"}]; ok {
		t.Error("aiops -> skill is declared again; if it came back, say what now needs it, " +
			"because the tool bridge holds a one-method port in core/domain and nothing in it " +
			"needs the skill service's audit rows, scope routing, tunnel round trip or catalogue")
	}
}

// The cut below is the first one in this file whose guard reads the tree
// rather than the table, and the reason is that the table is the wrong place
// to learn this from.
//
// `marketplace -> pluginimport` was declared, so "is it off the list" is a real
// question — but a list entry is one line somebody can add back, and every
// other cut guard in this file asks only about that line. The thing that
// actually went wrong upstream was an import in a file, and the file is what
// has to stay clean. So these two tests read the shipped tree through the same
// parseTree the gate reads it through: a second walk of the import graph in a
// test is the thing this package's header already warns about.
func TestTheEdgeDecision241CutIsNotOnTheList(t *testing.T) {
	if _, ok := edges[edge{from: "marketplace", to: "pluginimport"}]; ok {
		t.Error("marketplace -> pluginimport is declared again; the route behind " +
			"POST /v1/marketplace/import is handed the converter as a function by the " +
			"composition root, and both parameter and result types now live in core/domain. " +
			"If the edge came back, say what in the HTTP layer needs the converter itself — " +
			"nothing did before this cut and nothing should now")
	}
}

// TestNoMarketplaceFileImportsTheConverter is the guard that would have caught
// the original shape, whichever way the declaration table was edited.
//
// It ran for one decision looking at zero files; see domainFiles at the bottom
// of this file for why, and for the rule that came out of it. It is written on
// the shared helper now, and it fails loudly rather than silently if the
// domain ever moves out from under the walk.
func TestNoMarketplaceFileImportsTheConverter(t *testing.T) {
	const producer = "github.com/vincent-wuhan/opskeeper/core/manager/biz/pluginimport"
	// Both roots, because the marketplace domain spans two modules today — the
	// usecase under core/manager/biz and the import route under
	// core/manager/server. A guard that only covered whichever one happened to
	// hold the offending import would pass the day the import moves to the
	// other, which is the same green problem in a smaller box.
	files := domainFiles(t, "marketplace", "../../core/manager", "../../core/domains")
	for path, imports := range files {
		for _, imp := range imports {
			if imp == producer || strings.HasPrefix(imp, producer+"/") {
				t.Errorf("%s imports %s. One HTTP file naming two type names was the "+
					"whole reason this edge existed; main.go is the one place allowed to "+
					"wire the converter, and it hands it over as a function", path, imp)
			}
		}
	}
}

// TestLoadWarningIsDeclaredOnce guards the second half of the cut, which is

// not about an edge at all.
//
// `biz/marketplace` used to carry its own `LoadWarning`, copied field for field
// out of aiops' chatruntime, under a comment saying the point was to keep the
// chatruntime import out of biz/marketplace — in a package whose usecase.go
// imported chatruntime eight symbols over. The stated reason was false, and a
// copy defended by a false reason is a copy that will drift: the two had the
// same three fields and the same three tags, and nothing would have said so if
// a fourth field arrived on one side only.
//
// This one deliberately does not use parseTree, which is worth saying because
// every other guard in this file does. The tree walk resolves a type alias to
// whatever it names (kindOf, on purpose, so `type Repo = store.Repo` is judged
// as the interface it points at) and `declKind`'s zero value is kindOther — so
// "chatruntime declares LoadWarning" and "nowhere declares LoadWarning" read
// identically through `source.declared`. The walk is built to answer whether a
// door is substitutable, and this question is whether two packages each own a
// struct of the same name. Those are different questions and they need
// different instruments; using the wrong one here would have produced a green
// test, which is the failure this repository keeps paying for.
func TestLoadWarningIsDeclaredOnce(t *testing.T) {
	// The walk root is the repository, so trim paths back to repo-relative.
	rel := func(path string) string {
		if i := strings.Index(path, "core/"); i >= 0 {
			return filepath.ToSlash(path[i:])
		}
		return filepath.ToSlash(path)
	}
	var owners []string
	// Only the roots that hold Go source. Walking the repository root took
	// this test from eight seconds to a hundred and nineteen, because it went
	// looking for .go files inside a node_modules and a set of built
	// artefacts — and a guard that is slow gets run less, which is a quieter
	// way to lose it than deleting it.
	goRoots := map[string]bool{"core": true, "cmd": true, "sdk": true}
	err := filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip a subtree when its FIRST path segment is not one of the
			// roots. Testing the first segment rather than the directory's
			// own name is what lets core/domain and core/manager/biz/aiops
			// through while node_modules and bin stay out.
			if seg := strings.Split(filepath.ToSlash(path), "/"); len(seg) >= 4 && !goRoots[seg[2]] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return nil //nolint:nilerr // a file that does not parse fails the build elsewhere
		}
		for _, decl := range file.Decls {
			gen, isGen := decl.(*ast.GenDecl)
			if !isGen || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, isType := spec.(*ast.TypeSpec)
				// An alias is not a second declaration: `type LoadWarning =
				// domain.LoadWarning` is the same type under a second name,
				// and chatruntime has one on purpose so its 31 uses and its
				// tests keep spelling what they always spelled.
				if isType && ts.Name.Name == "LoadWarning" && !ts.Assign.IsValid() {
					owners = append(owners, rel(path))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	want := "core/domain/pluginimport.go"
	if len(owners) != 1 || owners[0] != want {
		t.Errorf("LoadWarning is declared in %v; it belongs to exactly one file, %s, because "+
			"three domains hand it to a client. A second struct declaration is the shape "+
			"decision 241 deleted: two types with the same name, the same three fields and "+
			"the same three tags, kept in step by nothing", owners, want)
	}
}

// The grafana -> monitor cut is the second one in this file whose guard reads
// the tree rather than the table, and it is the one where the table was least
// to blame: the declaration said "grafana monitors are configured from the
// monitor model", which is true and not why the edge existed.
//
// What existed instead is eleven columns crossing a boundary whose entire job
// is to render six of them. So the guard is on the width, not on the import —
// and there are two directions, which is why there are two tests. An import
// that comes back is a boundary that widened; a projection that stops
// covering the spec is a dashboard that goes quietly blank.
func TestTheEdgeDecision242CutIsNotOnTheList(t *testing.T) {
	if _, ok := edges[edge{from: "grafana", to: "monitor"}]; ok {
		t.Error("grafana -> monitor is declared again. The mirror used to take the " +
			"monitor domain's eleven-column entity and read six of it; it now takes a " +
			"core/domain value projection of exactly those six, and the projection is " +
			"built in one place in the monitor domain. If the edge came back, say which " +
			"of the other five columns the dashboard now needs — and add it to the " +
			"six-field guard in core/domain rather than to the type")
	}
}

// TestNoGrafanaFileImportsTheMonitorModel is the structural half: the reason
// the edge existed was one file naming one struct, so that file not importing
// the model package is the fact worth keeping, whatever the declaration table
// says.
//
// The first version of this test was looking at zero files and reported green
// through the exact regression it was written for. See domainFiles at the
// bottom of this file.
func TestNoGrafanaFileImportsTheMonitorModel(t *testing.T) {
	const producer = "github.com/vincent-wuhan/opskeeper/core/domains/model/monitor"
	files := domainFiles(t, "grafana", "../../core/domains")
	for path, imports := range files {
		for _, imp := range imports {
			if imp == producer {
				t.Errorf("%s imports the monitor model. The mirror reads a panel's id, "+
					"title, type, query, legend and unit; ordinal, last_sync_error, "+
					"last_sync_at, updated_at and created_at are the monitor domain's own "+
					"bookkeeping, and two of those five record whether the mirror worked",
					path)
			}
		}
	}
}

// domainFiles walks one of the control-plane module roots and returns the
// non-test files of one domain, keyed by repo-relative path.
//
// It exists because two guards in this file were, until decision 242,
// silently looking at nothing, and both were wrong in the same way: they used
// parseTree, which is rooted at core/manager and reconstructs a file's path as
// `managerPrefix + relative`, so a file under core/domains came back as
//
//	.../core/manager/core/domains/biz/grafana/service
//
// and domainOf — which matches the layer-tree prefixes — returned "" for it.
// The guard's filter then excluded everything, the body never ran, and the
// test reported green. The grafana one passed through the exact regression it
// was written for; the marketplace one, committed a decision earlier, had
// never examined a file at all.
//
// The general lesson is worth more than the helper: **an assertion over a
// filtered set has to assert the filter matched something.** "No offending
// file" and "no candidate file" are different facts, and only the second one
// is a bug in the test. Every guard here that walks a tree now fails loudly on
// an empty candidate set.
func domainFiles(t *testing.T, domain string, roots ...string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, root := range roots {
		collectDomainFiles(t, root, domain, out)
	}
	if len(out) == 0 {
		t.Fatalf("no non-test file of domain %q was found under %v, so the guard that "+
			"asked for it is looking at nothing. A filter that stopped matching and a "+
			"regression that is absent are the same green unless the first one is ruled out",
			domain, roots)
	}
	return out
}

// collectDomainFiles does the walking for one root.
func collectDomainFiles(t *testing.T, root, domain string, out map[string][]string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		// <root>/<layer>/<domain>/... — the layer segment is one of a fixed
		// set and is the only part of the path that is not a domain name.
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 3 || parts[1] != domain {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil //nolint:nilerr // a file that does not parse fails the build elsewhere
		}
		imports := make([]string, 0, len(file.Imports))
		for _, spec := range file.Imports {
			if v, uerr := strconv.Unquote(spec.Path.Value); uerr == nil {
				imports = append(imports, v)
			}
		}
		repo := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(path), "../"), "../../")
		out[repo] = imports
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

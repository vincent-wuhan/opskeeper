package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// door_test.go covers the difference between a door and a package boundary.
//
// The release floor (entrypoint_test.go, decision 217) found that 11 domains
// are reached through exactly one package, and wrote that down as "the seam
// already exists". That sentence turned out to be wrong in a way that matters,
// so this file is the gate for the correction: a single package is a boundary
// in the import graph and mostly nothing else, because what the far side
// selects through it turns out to be structs and functions 10 times out of 11.
//
// The checker that draws that line is a lookup keyed by import path, filled in
// from a map filled in per file. The first version of it keyed that map by
// FILE path and looked it up by PACKAGE path, every lookup missed, and all
// eleven doors came out concrete — a number that looks like a finding and is
// actually a bug. The first test below is the guardrail against that, and it
// is a guardrail rather than an assertion of a number on purpose: the number
// 1-of-11 is a fact about the tree today and will change the moment someone
// adds an interface, while the rule "a package exporting only an interface is
// a substitutable door" will not.

// writeTree materialises a small manager tree and returns its root, so a test
// can state a shape in four lines instead of forty. Files are written under
// biz/, which domainOf collapses, so a path like biz/gate reads as domain
// "gate" exactly as it does in the shipped tree.
//
// %M in a body stands for the manager's import prefix. A fixture that hardcoded
// it would drift the day the module is renamed, and it would fail by finding
// no edges at all — a test that stops testing without going red is the exact
// failure mode these gates exist to prevent.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	// Every fixture tree has to have all three control-plane modules on
	// disk, even the ones that only care about two of them. parseControlPlane
	// walks the whole list in controlPlanePrefixes and reports a missing
	// directory as a walk that is broken rather than as a module that is
	// absent, which is the right default for the repository and the wrong
	// one for a fixture — so the empty module is created here, once, rather
	// than by every test that happens to call the control-plane walk.
	for _, prefix := range controlPlanePrefixes {
		dir := filepath.Join(root, "core", filepath.Base(strings.TrimSuffix(prefix, "/")))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
		// A directory is not a module to the walk; a Go file in it is. The
		// placeholder is a package with no declarations, so a fixture that
		// never mentions this module measures exactly what it would have
		// measured before the module existed.
		stub := filepath.Join(dir, "stub.go")
		if err := os.WriteFile(stub, []byte("package "+filepath.Base(dir)+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", stub, err)
		}
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("make %s: %v", rel, err)
		}
		filled := strings.ReplaceAll(body, "%M", managerPrefix)
		if err := os.WriteFile(path, []byte(filled), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

// doorKinds builds the graph for a written tree and hands back what the
// classifier said about each domain's single door, so the tests can talk about
// the classification rather than about the maps underneath it.
func doorKinds(t *testing.T, root string) map[string]string {
	t.Helper()
	sources, _, err := parseTree(root, managerPrefix, rules{})
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	g := buildGraph(sources, rules{})
	out := map[string]string{}
	for domain, pkgs := range g.doorUse {
		if len(pkgs) != 1 {
			continue
		}
		out[domain] = classifyDoor(pkgs[firstKey(pkgs)])
	}
	if len(out) == 0 {
		t.Fatalf("the fixture produced no door at all; a tree that yields no edges tests nothing")
	}
	return out
}

// TestAPackageExportingOnlyAnInterfaceIsASubstitutableDoor is the guardrail
// for the indexing bug described above.
//
// The dependent here selects exactly one symbol and that symbol is an
// interface, so the only possible answer is "interface door". An earlier
// version of the index looked the package up by a path it was never stored
// under, the lookup came back empty, and the symbol defaulted to concrete —
// turning all eleven real doors into a false finding that looked measured.
// There is no way to tell that apart from a real result by reading the
// report, which is exactly why it needs a test.
func TestAPackageExportingOnlyAnInterfaceIsASubstitutableDoor(t *testing.T) {
	root := writeTree(t, map[string]string{
		"biz/gate/gate.go": `package gate

type Gate interface{ Open() }
`,
		"biz/app/app.go": `package app

import "%Mbiz/gate"

var _ gate.Gate
`,
	})
	kinds := doorKinds(t, root)
	if got := kinds["gate"]; got != "interface" {
		t.Fatalf("a package whose only selected symbol is an interface is a door that can be\n"+
			"swapped behind, so it must classify as an interface door; got %q. A concrete here\n"+
			"means the declared surface was not found under the import path the lookup used, and\n"+
			"it is the same bug that once reported all eleven real doors as concrete.", got)
	}
}

// TestADependentHoldingAStructHasReachedAConcreteDoor is the other half, so
// the test above cannot pass by classifying everything as substitutable.
func TestADependentHoldingAStructHasReachedAConcreteDoor(t *testing.T) {
	root := writeTree(t, map[string]string{
		"biz/box/box.go": `package box

type Box struct{ ID string }
`,
		"biz/app/app.go": `package app

import "%Mbiz/box"

var _ = box.Box{}
`,
	})
	kinds := doorKinds(t, root)
	if got := kinds["box"]; got != "concrete" {
		t.Fatalf("a dependent naming a struct must be told the door is concrete, because the\n"+
			"type travels with the code; got %q", got)
	}
}

// TestAStructBesideAnInterfaceIsAMixedDoor pins the third state. Mixed is the
// answer that should make a reader suspicious: the package is part seam and
// part naming convention, and rounding it up to "interface" would promise a
// substitutability the package does not have.
func TestAStructBesideAnInterfaceIsAMixedDoor(t *testing.T) {
	root := writeTree(t, map[string]string{
		"biz/half/half.go": `package half

type Port interface{ Call() }

type Client struct{}
`,
		"biz/app/app.go": `package app

import "%Mbiz/half"

var (
	_ half.Port
	_ = half.Client{}
)
`,
	})
	kinds := doorKinds(t, root)
	if got := kinds["half"]; got != "mixed" {
		t.Fatalf("a package reached through both an interface and a struct is mixed, which is\n"+
			"neither reassurance nor refusal; got %q", got)
	}
}

// TestAnAliasToATypeInASiblingFileIsResolvedAgainstThePackage is the reason
// resolveDeclared takes a second pass instead of judging each file alone.
//
// `type Port = half.Port` written in one file names a type declared in the
// next file, which is a single fact spread across two files. Judged per file,
// the alias is an unresolvable name and the safe direction is concrete — so
// the domain lands in the wrong tier for a reason that is purely about which
// file was read first. The test writes the declaration and the alias in
// separate files, in that order, because that is the order that breaks it.
func TestAnAliasToATypeInASiblingFileIsResolvedAgainstThePackage(t *testing.T) {
	root := writeTree(t, map[string]string{
		"biz/port/port.go": `package port

type Port interface{ Call() }
`,
		"biz/port/alias.go": `package port

type Renamed = Port
`,
		"biz/app/app.go": `package app

import "%Mbiz/port"

var _ port.Renamed
`,
	})
	kinds := doorKinds(t, root)
	if got := kinds["port"]; got != "interface" {
		t.Fatalf("an alias is the name it points at, so a package whose interface is reached\n"+
			"through a sibling-file alias is still an interface door; got %q", got)
	}
}

// TestAnAliasPointingOutOfThePackageIsCalledConcrete records which way the
// checker guesses when a name resolves outside what it can see.
//
// It cannot look at the other package here, so it says concrete. That is
// deliberate and the test says so, because the asymmetry matters: a false
// concrete costs a domain its place in the substitutable tier, which is a
// slower mistake, while a false interface would promise a boundary that may
// not be there and send a split into a break.
func TestAnAliasPointingOutOfThePackageIsCalledConcrete(t *testing.T) {
	root := writeTree(t, map[string]string{
		"biz/store/store.go": `package store

type Repo struct{}
`,
		"biz/bridge/bridge.go": `package bridge

import "%Mbiz/store"

type Repo = store.Repo
`,
		"biz/app/app.go": `package app

import "%Mbiz/bridge"

var _ bridge.Repo
`,
	})
	kinds := doorKinds(t, root)
	if got := kinds["bridge"]; got != "concrete" {
		t.Fatalf("an alias this checker cannot follow must be called concrete, the safe\n"+
			"direction; got %q", got)
	}
}

// TestADoorNobodySelectsIsUnknownRatherThanGuessed covers the case the other
// three cannot: a dependent that reaches through a name this checker does not
// model, such as a dot-import. Folding that into "concrete" or "interface"
// would both be inventions, so the honest answer is its own.
func TestADoorNobodySelectsIsUnknownRatherThanGuessed(t *testing.T) {
	if got := classifyDoor(nil); got != "unknown" {
		t.Fatalf("an empty selection is unknown, not an interface door nobody proved; got %q", got)
	}
}

// TestTheDoorColumnLeadsWithThePartThatIsNotSubstitutable pins the order the
// summary prints in.
//
// It is worst-first because the reader scanning this column is asking how
// much of a domain's surface can be swapped, and the answer should lead with
// the part that cannot. Repeating one word per package was the first version
// and it made aiops print the same two words seven times, which is noise
// shaped like information.
func TestTheDoorColumnLeadsWithThePartThatIsNotSubstitutable(t *testing.T) {
	g := &domainGraph{doorUse: map[string]map[string]map[string]declKind{
		"aiops": {
			managerPrefix + "biz/aiops/a": {"One": kindOther, "Two": kindOther},
			managerPrefix + "biz/aiops/b": {"Three": kindInterface},
			managerPrefix + "biz/aiops/c": {"Four": kindInterface},
			managerPrefix + "biz/aiops/d": {"Five": kindOther},
			managerPrefix + "biz/aiops/e": {"Six": kindOther},
			managerPrefix + "biz/aiops/f": {"Seven": kindOther},
			managerPrefix + "biz/aiops/g": {"Eight": kindInterface, "Nine": kindOther},
		},
	}}
	got := strings.Join(g.doorSummary("aiops"), ", ")
	want := "4 concretes, mixed door, 2 interfaces"
	if got != want {
		t.Fatalf("the summary must lead with what is not substitutable and count repeats:\n  got  %q\n  want %q", got, want)
	}
}

// TestTheHeadlineCountInTheReportIsRecomputedNotAsserted keeps the number in
// the prose tied to the map it claims to summarise.
//
// The report says "N of these M have an interface door" and reads it off
// doorUse. If that number were ever pasted in as a literal, the sentence would
// keep its shape while describing a tree that no longer exists, and nothing
// else in the report would move. So the test recomputes both counts from the
// map by a different route than printReleaseFloor takes — every door of each
// domain rather than only the first — and compares against the printed text.
func TestTheHeadlineCountInTheReportIsRecomputedNotAsserted(t *testing.T) {
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	g := buildGraph(sources, r)

	// The single-door set, recomputed from the sources rather than read off
	// g.entryUse, which is a map filled in as a side effect of counting edges.
	single := map[string]bool{}
	for domain, pkgs := range doorUseFromSources(sources, r) {
		if len(pkgs) == 1 {
			single[domain] = true
		}
	}
	var substitutable int
	for domain := range single {
		doors := g.doorUse[domain]
		if len(doors) == 0 {
			continue
		}
		all := true
		for _, syms := range doors {
			if classifyDoor(syms) != "interface" {
				all = false
			}
		}
		if all {
			substitutable++
		}
	}

	var buf bytes.Buffer
	g.printReleaseFloor(&buf, map[string]string{})
	out := buf.String()
	want := fmt.Sprintf("%d of these %d have an interface door", substitutable, len(single))
	if !strings.Contains(out, want) {
		t.Fatalf("the printed headline must match the doors as recomputed.\n  looked for: %q\n  the report said:\n%s", want, out)
	}
}

// doorUseFromSources recomputes which packages each domain is reached through,
// walking the sources directly instead of trusting the map buildGraph filled
// in as a side effect of counting edges.
func doorUseFromSources(sources []source, r rules) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, src := range sources {
		from := domainOf(src.path)
		if from == "" || src.test {
			continue
		}
		for _, imp := range src.imports {
			to := domainOf(imp)
			if to == "" || to == from || r.shared[to] != "" {
				continue
			}
			if out[to] == nil {
				out[to] = map[string]bool{}
			}
			out[to][imp] = true
		}
	}
	return out
}

// headlineCounts pulls the two numbers out of the sentence the report prints,
// so a test can watch them move instead of only agreeing with them once.
func headlineCounts(t *testing.T, out string) (substitutable, total int) {
	t.Helper()
	i := strings.Index(out, " have an interface door")
	if i < 0 {
		t.Fatalf("the report printed no headline count:\n%s", out)
	}
	head := out[:i]
	// The sentence reads "N of these M have an interface door", so the two
	// numbers are the fourth-from-last and the last field.
	fields := strings.Fields(head)
	if len(fields) < 4 {
		t.Fatalf("cannot read the headline out of %q", head)
	}
	var err error
	if substitutable, err = strconv.Atoi(fields[len(fields)-4]); err != nil {
		t.Fatalf("the substitutable count is not a number in %q: %v", head, err)
	}
	if total, err = strconv.Atoi(fields[len(fields)-1]); err != nil {
		t.Fatalf("the total is not a number in %q: %v", head, err)
	}
	return substitutable, total
}

// TestTheHeadlineCountMovesWhenADoorDoes closes the gap the previous test
// could not.
//
// That test recomputed the count and compared, which catches a stale map but
// not a hardcoded one: pasting the current true numbers into the format call
// produces byte-identical output and stays green, which is precisely how a
// number stops describing the tree. So this test moves the tree instead — it
// turns one concrete door into an interface door, reprints, and requires the
// headline to follow by one. A literal cannot follow anything.
//
// The flip is on a copy of the graph because doorUse is the checker's own
// bookkeeping; mutating the shipped one in place would leak into any later
// test in this package.
func TestTheHeadlineCountMovesWhenADoorDoes(t *testing.T) {
	sources, _, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	g := buildGraph(sources, r)

	var before bytes.Buffer
	g.printReleaseFloor(&before, map[string]string{})
	wasSubstitutable, total := headlineCounts(t, before.String())

	// Pick the same domain twice, deterministically: the lexicographically
	// first single-door domain that is not already substitutable.
	single := map[string]bool{}
	for domain, pkgs := range doorUseFromSources(sources, r) {
		if len(pkgs) == 1 {
			single[domain] = true
		}
	}
	candidates := make([]string, 0, len(single))
	for domain := range single {
		if len(g.doorUse[domain]) == 1 && classifyDoor(g.doorUse[domain][firstKey(g.doorUse[domain])]) != "interface" {
			candidates = append(candidates, domain)
		}
	}
	if len(candidates) == 0 {
		t.Skip("no concrete single door to flip; the headline has nothing to prove")
	}
	sort.Strings(candidates)
	victim := candidates[0]

	flipped := buildGraph(sources, r)
	for pkg, syms := range flipped.doorUse[victim] {
		for sym := range syms {
			syms[sym] = kindInterface
		}
		_ = pkg
	}

	var after bytes.Buffer
	flipped.printReleaseFloor(&after, map[string]string{})
	nowSubstitutable, afterTotal := headlineCounts(t, after.String())

	if afterTotal != total {
		t.Fatalf("flipping a door must not change how many single-door domains there are;\n"+
			"  before %d, after %d", total, afterTotal)
	}
	if nowSubstitutable != wasSubstitutable+1 {
		t.Fatalf("turning %s's door into an interface door must move the headline up by one,\n"+
			"because a count that cannot follow a change is a number pasted into a format string.\n"+
			"  before %d, after %d", victim, wasSubstitutable, nowSubstitutable)
	}
}

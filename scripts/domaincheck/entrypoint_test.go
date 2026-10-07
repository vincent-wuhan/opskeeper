package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// How many doors the rest of the tree knocks on.
//
// The release floor (candidate_test.go, decision 216) answers which domains
// nothing imports. This file answers the next question for the ones something
// does import: is that dependence already funnelled through one package, or is
// it spread across a domain's internals.
//
// The distinction is worth a gate because it is the difference between "the
// seam is already drawn" and "the seam has to be built", and the two cost
// completely different amounts of work to separate. It is also the easiest
// place in this checker to be wrong by accident, because the answer comes out
// of a bookkeeping map that buildGraph fills in as a side effect of counting
// edges — and a map that is filled in as a side effect is a map that can be
// filled in wrongly while every other number in the report stays correct.
//
// So the test here recomputes the entry points from the parsed tree directly,
// without touching the map, and compares. A disagreement means the report is
// describing a boundary that does not exist.

// entryPointsFromSources recomputes, for every domain, which of its packages
// are imported by another domain — walking the sources directly rather than
// reading g.entryUse.
func entryPointsFromSources(sources []source, r rules) map[string]map[string]bool {
	byDomain := map[string]map[string]bool{}
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
			if byDomain[to] == nil {
				byDomain[to] = map[string]bool{}
			}
			byDomain[to][imp] = true
		}
	}
	return byDomain
}

// TestTheEntryPointBookkeepingAgreesWithTheTree is the gate proper.
func TestTheEntryPointBookkeepingAgreesWithTheTree(t *testing.T) {
	root := "../.."
	sources, _, err := parseControlPlane(root)
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	g := buildGraph(sources, r)
	want := entryPointsFromSources(sources, r)

	if len(want) == 0 {
		t.Fatal("the independent recomputation found no cross-domain imports at all, so agreeing " +
			"with it would prove nothing")
	}

	for domain, gotPkgs := range g.entryUse {
		wantPkgs, ok := want[domain]
		if !ok {
			t.Errorf("the graph records entry packages for %s, and the tree has nothing importing it", domain)
			continue
		}
		var got, expect []string
		for p := range gotPkgs {
			got = append(got, p)
		}
		for p := range wantPkgs {
			expect = append(expect, p)
		}
		sort.Strings(got)
		sort.Strings(expect)
		if strings.Join(got, ",") != strings.Join(expect, ",") {
			t.Errorf("%s: the graph says it is entered through %v, the tree says %v", domain, got, expect)
		}
	}
	for domain := range want {
		if _, ok := g.entryUse[domain]; !ok {
			t.Errorf("the tree says %s is entered through %d package(s) and the graph records none",
				domain, len(want[domain]))
		}
	}
}

// TestTheReportSplitsTheCoupledDomainsIntoOneDoorAndMany keeps the two tiers
// honest as a partition, because a report that puts a domain in both or in
// neither reads as a classification when it is a leftover.
func TestTheReportSplitsTheCoupledDomainsIntoOneDoorAndMany(t *testing.T) {
	root := "../.."
	sources, _, err := parseControlPlane(root)
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	g := buildGraph(sources, r)
	entries := entryPointsFromSources(sources, r)

	inbound := map[string]int{}
	for e, n := range g.weight {
		inbound[e.to] += n
	}

	var single, multi int
	for d, pkgs := range entries {
		if r.shared[d] != "" {
			// A shared domain's in-degree is not measurable here at all,
			// so putting it in either tier would be a claim the graph
			// cannot back.
			continue
		}
		if inbound[d] == 0 {
			continue // the floor, covered by the other file
		}
		switch len(pkgs) {
		case 1:
			single++
		default:
			multi++
		}
	}
	if single == 0 {
		t.Error("no coupled domain is reached through a single package, so the tier the report " +
			"calls \"the seam already exists\" is empty and the ordering it offers is not an ordering")
	}
	if multi == 0 {
		t.Error("every coupled domain is reached through one package; the report's other tier is " +
			"empty, which means either the tree changed or the classification stopped discriminating")
	}
	// The two tiers have to be reported with the same unit the rest of the
	// tool uses. "importers" counted as import statements is what the edge
	// weight means, and calling it anything else would invite a reader to
	// compare it with a different number.
	for d := range entries {
		if r.shared[d] != "" || inbound[d] == 0 {
			continue
		}
		importers := map[string]bool{}
		for e := range g.weight {
			if e.to == d {
				importers[e.from] = true
			}
		}
		if len(importers) == 0 {
			t.Errorf("%s is counted as coupled but no domain imports it", d)
		}
		if len(importers) > inbound[d] {
			t.Errorf("%s has %d importing domains but only %d inbound import statements; the two "+
				"are different units and the report must not present one as the other",
				d, len(importers), inbound[d])
		}
	}
}

// TestTheImporterColumnCountsDomainsAndNotImportStatements closes a hole that
// was found by hand rather than by a test, which is the only kind worth
// writing one for afterwards.
//
// The column is headed "importers", so it has to count importing domains. It
// briefly counted inbound import statements instead, which is a different
// unit and happens to be larger: loop is imported by four domains across ten
// statements, alert by six domains across thirty. Nothing else in the report
// changed, no number looked wrong, and the report was quietly comparing two
// different measures under one heading.
func TestTheImporterColumnCountsDomainsAndNotImportStatements(t *testing.T) {
	root := "../.."
	sources, _, err := parseControlPlane(root)
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	g := buildGraph(sources, r)

	statements := map[string]int{}
	domains := map[string]int{}
	for e, n := range g.weight {
		statements[e.to] += n
		domains[e.to]++
	}

	// The claim is only interesting if some domain actually has the two
	// counts differ. If they were equal everywhere, this test would pass
	// against a report that used either one.
	differing := 0
	for d, st := range statements {
		if domains[d] == st {
			continue
		}
		differing++
		row := g.entryRow(d)
		if !strings.Contains(row, plural(domains[d], "", "s")) {
			continue // the substring check below is on the importer field only
		}
		if strings.Contains(row, fmt.Sprintf("%d importer", st)) {
			t.Errorf("%s: the row says %q, which counts import statements (%d) under a heading "+
				"that says importers (%d importing domains)", d, row, st, domains[d])
		}
	}
	if differing == 0 {
		t.Fatal("no domain has a different importer count and import statement count, so this " +
			"test cannot tell the two units apart and is proving nothing")
	}
	t.Logf("%d domain(s) distinguish the two units, so the column is checked against the "+
		"measure its heading names", differing)
}

// TestTheReleaseTiersPartitionTheTreeAndKeepSharedOnTheOutside closes the one
// hole the reverse verification found in this file's first draft: mutating the
// report so that shared base components stop being excluded left every test
// green.
//
// The reason nothing caught it is that the shared exclusion lived inside the
// printing, and a printing is not something a test can assert against without
// string-matching it. So the computation was pulled out into releaseTiers, and
// the invariant it has to hold is now checkable directly:
//
//   - the four tiers cover every domain exactly once, so a domain cannot be
//     counted as both independently shippable and coupled, or in neither;
//   - no shared base component is in the floor or in either coupled tier.
//
// That second one is the one that matters. A shared domain reads as a leaf in
// this import graph — being depended on needs no declaration, so its in-degree
// is simply not measured here — and reporting "middleware is independently
// shippable" would be reporting a bug as a finding. Nothing else in this
// checker would notice: the module boundary check does not model shared
// components, and the graph report excludes them for a different reason.
func TestTheReleaseTiersPartitionTheTreeAndKeepSharedOnTheOutside(t *testing.T) {
	root := "../.."
	sources, _, err := parseControlPlane(root)
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	g := buildGraph(sources, r)
	floor, single, multi, excluded := g.releaseTiers(r.shared)

	seen := map[string]string{}
	for tier, names := range map[string][]string{
		"floor": floor, "single": single, "multi": multi, "excluded": excluded,
	} {
		for _, d := range names {
			if other, dup := seen[d]; dup {
				t.Errorf("%s is in both the %s tier and the %s tier", d, other, tier)
				continue
			}
			seen[d] = tier
		}
	}
	for d := range g.domains {
		if _, ok := seen[d]; !ok {
			t.Errorf("%s is in no tier at all; a domain the report says nothing about is a "+
				"domain a reader will assume is not a candidate", d)
		}
	}

	for _, d := range excluded {
		if r.shared[d] == "" {
			t.Errorf("%s is excluded as a shared base component, but it is not one", d)
		}
	}
	for _, tier := range []struct {
		name  string
		names []string
	}{{"floor", floor}, {"single", single}, {"multi", multi}} {
		for _, d := range tier.names {
			if why := r.shared[d]; why != "" {
				t.Errorf("%s is in the %s tier, but it is a shared base component (%s); its "+
					"in-degree is not measurable in this import graph, so no tier derived from "+
					"that in-degree can speak for it", d, tier.name, why)
			}
		}
	}
	if len(floor) == 0 || len(single) == 0 || len(multi) == 0 {
		t.Errorf("the tiers are floor=%d single=%d multi=%d; an empty tier means the report is "+
			"about to print a heading for nothing", len(floor), len(single), len(multi))
	}
}

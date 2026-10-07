package main

import (
	"testing"
)

// Why this file exists.
//
// The progress section's stage-3 component ("manager 拆分") is derived from a
// headline the ledger has carried for dozens of decisions as "已切 N / 34".
// Nothing in this repository could produce that number: it was incremented by
// hand, once per cut, in prose. The two counters in this package that *are*
// computed — the declared edge count and the test-only cross-domain count —
// are different units from it, and the ledger's own §4.56.8 quotes a
// production import count that today is six times larger than the edge count
// on a tree with fewer edges.
//
// So the number the progress percentage is made of had no gauge. This file is
// the gauge's owner. It does two things and refuses the third:
//
//   - recomputes the production cross-domain import count from the parsed
//     sources by a second, independent walk, and requires the two to agree.
//     A counter that disagrees with a recomputation of itself is the exact
//     failure this repository has recorded fourteen times.
//   - requires the count to be non-zero and to be less than the total number
//     of import statements in the tree, so a predicate that silently stopped
//     filtering cannot pass by landing on a number that happens to look right.
//   - refuses to compare it to 34, 19, or any other historical figure. Those
//     were a different unit on a different tree, and a gate that demanded a
//     match with them would be a gate asserting a number nobody can reproduce.

// prodCrossDomainFromSources recomputes the counter without parseTree, so a
// bug in the walk's own bookkeeping cannot hide behind itself.
func prodCrossDomainFromSources(sources []source, r rules) int {
	n := 0
	for _, src := range sources {
		if src.test {
			continue
		}
		from := domainOf(src.path)
		if from == "" {
			continue
		}
		for _, imp := range src.imports {
			to := domainOf(imp)
			if to == "" || to == from || r.shared[to] != "" {
				continue
			}
			n++
		}
	}
	return n
}

func TestTheProductionCrossDomainCounterAgreesWithAnIndependentWalk(t *testing.T) {
	sources, stats, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the control plane: %v", err)
	}
	r := defaultRules()

	independently := prodCrossDomainFromSources(sources, r)
	if stats.prodCrossDomain != independently {
		t.Errorf("the walk counted %d production cross-domain imports and an independent "+
			"walk over the same sources counted %d; the ledger's stage-3 component is "+
			"derived from this number, so the two cannot be allowed to disagree",
			stats.prodCrossDomain, independently)
	}
}

func TestTheProductionCrossDomainCounterCountsSomethingAndIsStillAFilter(t *testing.T) {
	sources, stats, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the control plane: %v", err)
	}
	if stats.prodCrossDomain == 0 {
		t.Fatal("the production cross-domain count is zero; a predicate that stopped " +
			"filtering would report the same number as a tree with no cross-domain " +
			"imports at all, which is the state this repository is trying to leave")
	}

	// The upper bound is the point. Without it, a predicate that lost its
	// `to != from` or its shared-base exclusion would report a strictly
	// larger number and still pass every other test in this file.
	total := 0
	for _, src := range sources {
		if src.test {
			continue
		}
		total += len(src.imports)
	}
	if stats.prodCrossDomain >= total {
		t.Errorf("the production cross-domain count is %d and the tree has %d production "+
			"import statements in all; every stdlib and intra-domain import would have to "+
			"be excluded for that to hold, so the filter is not doing what its name says",
			stats.prodCrossDomain, total)
	}
}

// The two counters are separate questions and must not be able to stand in
// for each other. Today they print the same number, which is a coincidence of
// this tree and not a property; if a change to the walk ever made one of them
// read the other's field, this would notice only by accident.
func TestTheProductionAndTestOnlyCountersAreCountedSeparately(t *testing.T) {
	sources, stats, err := parseControlPlane("../..")
	if err != nil {
		t.Fatalf("parse the control plane: %v", err)
	}
	r := defaultRules()

	wantTestOnly := 0
	for _, src := range sources {
		if !src.test {
			continue
		}
		from := domainOf(src.path)
		if from == "" {
			continue
		}
		for _, imp := range src.imports {
			to := domainOf(imp)
			if to == "" || to == from || r.shared[to] != "" {
				continue
			}
			wantTestOnly++
		}
	}
	if stats.testOnlyEdges != wantTestOnly {
		t.Errorf("the walk counted %d test-only cross-domain imports and an independent "+
			"walk counted %d", stats.testOnlyEdges, wantTestOnly)
	}
}

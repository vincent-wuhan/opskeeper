package gatename

import "testing"

// The two rules were moved here so that cigate and the gate report cannot
// disagree about which targets are gates. A shared rule that nothing tests is
// a shared rule that one command can drift out of, so each of the shapes it
// has to get right is broken here on purpose.

// The Makefile this repository actually ships, reduced to the shapes that
// have each broken a parser at least once.
func TestMakeTargetsReadsTheShapesThatMatter(t *testing.T) {
	src := "" +
		".PHONY: module-check eval-gates\n" + // make's own syntax
		"CC := gcc\n" + // variable, not a target
		"SRC ?= ./cmd\n" + // conditional variable
		"OUT = out\n" + // recursive variable
		"check: build\n" + // a real target
		"\t@echo building\n" + // a recipe line with no colon
		"weird:\n" +
		"\t@echo 'x: y'\n" // a colon inside a recipe must not make a target
	out := MakeTargets(src)
	for _, want := range []string{"check", "weird"} {
		if !out[want] {
			t.Errorf("%q was not read as a target; got %v", want, keys(out))
		}
	}
	for _, unwanted := range []string{".PHONY", "CC", "SRC", "OUT", "echo"} {
		if out[unwanted] {
			t.Errorf("%q was read as a target; got %v", unwanted, keys(out))
		}
	}
}

// The rule is loose on purpose, and the looseness is what makes an exemption
// visible. A strict rule would let a gate-shaped target nobody runs vanish
// from both commands without either noticing.
func TestLooksLikeGateIsLooseEnoughToCatchAnUnaccountedGate(t *testing.T) {
	for _, want := range []string{"module-check", "check", "anything-check"} {
		if !LooksLikeGate(want) {
			t.Errorf("%q does not read as a gate", want)
		}
	}
	for _, unwanted := range []string{"build", "test", "gates-report", "checklist", "check-my-work"} {
		if LooksLikeGate(unwanted) {
			t.Errorf("%q reads as a gate; the suffix rule must not match a word that merely starts with it", unwanted)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

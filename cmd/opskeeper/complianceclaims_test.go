package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard"
)

// Data-Guard's vocabulary promised four controls that no code enforced, and
// the promises lived in comments and in a persisted `enforced` flag — which is
// why they survived. This gate is the receipt: every promise in the registry
// must still be true about itself, in the tree, today.
//
// The walk is a name search, and it is deliberately narrow about what it can
// see: it counts references in non-test files, so a symbol reachable only
// through a build tag or a string-built name is invisible to it. That blindness
// is the safe direction — it can only ever report a row as less reachable than
// it is, which makes a human go and look.

func repoRoot(t *testing.T) string {
	t.Helper()
	// reporoot.Find walks up by tracked markers rather than looking for a
	// go.work, which is gitignored and therefore absent in a clean clone and
	// in CI — a test that keys on it passes itself green in exactly the
	// place a gate matters.
	// Absolute, and this is not a detail: reporoot.Find walks up by calling
	// filepath.Dir, and Dir(".") is "." — handed a relative start it stops
	// after one step and reports "no root", which this test then turned into
	// a skip. Every assertion below silently stopped running while the gate
	// kept reporting success, which is the exact failure this file is about.
	// Two of the mutation runs below caught it, and only because a mutation
	// that must turn the gate red stayed green.
	start, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve the package directory: %v", err)
	}
	root, ok := reporoot.Find(start, 8)
	if !ok {
		t.Fatal("the repository root was not found from " + start +
			"; a gate that cannot see the tree must fail, not skip")
	}
	return root
}

// productionFiles returns every Go file that is not a test, skipping the
// directories that hold none of the product.
func productionFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the walk found no production files; it is looking at nothing")
	}
	return files
}

// referencesTo counts the places production code *uses* a symbol, and returns
// the files that declare it.
//
// Three things are deliberately not counted as uses, each of which produced a
// false reading on this gate's earlier runs:
//
//   - the declaration itself, or a declared-but-unwired symbol looks wired;
//   - doc comments, which name the symbol they document — the whole file can
//     discuss a function without anyone calling it;
//   - test files, or "inert" would be a statement about the test suite instead
//     of about the product.
//
// Counting occurrences rather than files is what makes a use in the declaring
// package itself visible; a wiring written two lines under the function is
// still wiring, and the day somebody writes it there is exactly when the row
// has to be told to flip.
func referencesTo(files []string, symbol string) (uses int, declaredIn []string) {
	isDeclaration := func(line string) bool {
		return strings.Contains(line, "func "+symbol) ||
			strings.Contains(line, ") "+symbol+"(")
	}
	for _, file := range files {
		// The registry is not evidence for itself. Its Probe strings are
		// production code, so counting them would make every inert symbol
		// look wired — which is exactly what happened on the gate's first
		// run of the flip-to-enforced mutation, and exactly the silent
		// green this file exists to make impossible.
		if strings.HasSuffix(filepath.ToSlash(file), "dataguard/enforcement.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		declares := false
		fileUses := 0
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, symbol) {
				continue
			}
			if isDeclaration(line) {
				declares = true
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			fileUses++
		}
		if declares {
			declaredIn = append(declaredIn, file)
		}
		uses += fileUses
	}
	return uses, declaredIn
}

// A row that says "enforced" must point at a symbol production code actually
// calls. Without this the registry is a list of good intentions wearing a
// status column.
func TestEveryEnforcedClaimIsReachableFromProductionCode(t *testing.T) {
	root := repoRoot(t)
	files := productionFiles(t, root)

	enforced := 0
	for _, claim := range dataguard.Claims() {
		if claim.Status != dataguard.StatusEnforced {
			continue
		}
		enforced++
		if claim.Probe == "" {
			t.Errorf("%s claims enforcement but names nothing to check", claim.ID)
			continue
		}
		mentions, declared := referencesTo(files, claim.Probe)
		if len(declared) == 0 {
			t.Errorf("%s names the probe %q, which does not exist in the tree", claim.ID, claim.Probe)
			continue
		}
		if mentions == 0 {
			t.Errorf("%s claims enforcement, but %q is only declared (%v) and never used by "+
				"production code", claim.ID, claim.Probe, declared)
		}
	}
	if enforced == 0 {
		// Today this is the truth, and a gate that forbade it would be a
		// gate forcing a lie. What it must not allow is the truth being
		// true and unwritten: a registry in which nothing is enforced has
		// to be a decision the ledger records, not a state somebody
		// discovers later.
		ledger, err := os.ReadFile(filepath.Join(root, "docs", "opskeeper2-architecture.md"))
		if err != nil {
			t.Fatalf("read the ledger: %v", err)
		}
		if !strings.Contains(string(ledger), "compliance.enforced-tag") {
			t.Fatal("no claim in the registry is enforced and the ledger does not say so; " +
				"an architecture nobody wrote down is the failure this gate exists to prevent")
		}
		t.Logf("no Data-Guard control is enforced in production code; the ledger records it")
	}
}

// The opposite direction matters just as much. A row that says "inert" is a
// statement about the tree, so the day somebody wires the thing up the gate
// goes red — otherwise "inert" becomes the answer nobody revisits.
func TestEveryInertClaimIsStillUnreachableFromProductionCode(t *testing.T) {
	root := repoRoot(t)
	files := productionFiles(t, root)

	for _, claim := range dataguard.Claims() {
		if claim.Status != dataguard.StatusInert {
			continue
		}
		if claim.Probe == "" {
			t.Errorf("%s claims to be inert but names nothing to check", claim.ID)
			continue
		}
		mentions, declared := referencesTo(files, claim.Probe)
		if len(declared) == 0 {
			t.Errorf("%s names the probe %q, which does not exist in the tree", claim.ID, claim.Probe)
			continue
		}
		// Zero uses, not "fewer uses than declarations": the two numbers are
		// not on the same scale, and the first version of this line compared
		// them anyway — which read a same-package wiring as still inert.
		if mentions > 0 {
			t.Errorf("%s says %q is inert, but production code uses it in %d place(s) "+
				"(declared in %v); flip the row to enforced and describe what runs",
				claim.ID, claim.Probe, mentions, declared)
		}
	}
}

// A declaration names no code at all. The moment it does, it is one of the two
// other statuses, and the difference is exactly what an operator is misled by.
func TestNoDeclaredClaimNamesAnImplementation(t *testing.T) {
	for _, claim := range dataguard.Claims() {
		if claim.Status != dataguard.StatusDeclared {
			continue
		}
		if claim.Probe != "" {
			t.Errorf("%s is declared but names the probe %q; a declaration with an "+
				"implementation behind it is one of the other two statuses", claim.ID, claim.Probe)
		}
	}
}

// Every control name the built-in catalogs advertise must appear in the
// registry. A new catalog entry that nobody has classified is a new promise,
// and this is what stops it from arriving quietly.
func TestEveryAdvertisedControlIsClassified(t *testing.T) {
	var registry strings.Builder
	for _, claim := range dataguard.Claims() {
		registry.WriteString(claim.Claim)
		registry.WriteString(claim.Note)
	}
	for _, control := range dataguard.DeclaredControls() {
		if !strings.Contains(registry.String(), control) {
			t.Errorf("the built-in catalogs advertise %q and the registry says nothing about it; "+
				"add a row saying whether this build enforces it", control)
		}
	}
}

// The same rule for the sensitivity vocabulary: a level whose comment promises
// a control must be named in the registry.
func TestEverySensitivityLevelThatPromisesAControlIsClassified(t *testing.T) {
	claims := dataguard.Claims()
	for _, level := range []dataguard.Sensitivity{
		dataguard.Confidential, dataguard.Restricted, dataguard.TopSecret,
	} {
		classified := false
		for _, claim := range claims {
			if strings.Contains(claim.Claim, string(level)) || strings.Contains(claim.Note, string(level)) {
				classified = true
				break
			}
		}
		if !classified {
			t.Errorf("the %s level promises controls in its comment and the registry does not "+
				"say what is true about them", level)
		}
	}
}

// Every row must say what is true even when the answer is "nothing". A row with
// an empty note is a row nobody will look at twice.
func TestEveryClaimSaysWhatIsActuallyTrue(t *testing.T) {
	seen := map[string]bool{}
	for _, claim := range dataguard.Claims() {
		if claim.ID == "" || claim.Claim == "" || claim.Note == "" {
			t.Errorf("a registry row is missing an id, a claim or a note: %+v", claim)
		}
		switch claim.Status {
		case dataguard.StatusEnforced, dataguard.StatusInert, dataguard.StatusDeclared:
		default:
			t.Errorf("%s has the unknown status %q", claim.ID, claim.Status)
		}
		if seen[claim.ID] {
			t.Errorf("the registry has two rows with the id %q", claim.ID)
		}
		seen[claim.ID] = true
	}
	if _, ok := dataguard.Claim("compliance.enforced-tag"); !ok {
		t.Error("the compliance tag row is missing; it is the one the API field still lies about")
	}
}

// backtickedIdentifier pulls the code-shaped words out of a row's prose, which
// is where a claim names the thing it is about.
var backtickedIdentifier = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_.]{3,})`")

// A declared row is the strongest statement in the registry: "there is no
// code at all". It is also the statement most likely to be wrong, because
// writing it is cheap and checking it used to mean reading the package the row
// happens to sit in. This is that check, and it found its own error: two rows
// written as declared named controls that were implemented one module over.
func TestADeclaredRowNamesNoFunctionAnywhereInTheTree(t *testing.T) {
	root := repoRoot(t)
	files := productionFiles(t, root)

	// Every declaration in the tree, once, so the check below is a lookup
	// rather than a walk per name.
	declared := map[string]bool{}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "func ") {
				continue
			}
			// Both declaration shapes, and the method shape is the one
			// this extractor first got wrong: for
			// `func (a *Enforcer) AllowWithSensitivity(` the first "(" it
			// finds belongs to the receiver, so the name came out empty and
			// every method in the tree read as undeclared — which is the
			// same class of mistake as the earlier `) NAME(` probe, and it
			// hid the exact function this assertion exists to find.
			rest := strings.TrimPrefix(trimmed, "func ")
			if strings.HasPrefix(rest, "(") {
				closing := strings.Index(rest, ")")
				if closing < 0 {
					continue
				}
				rest = strings.TrimSpace(rest[closing+1:])
			}
			idx := strings.Index(rest, "(")
			if idx < 0 {
				continue
			}
			name := strings.TrimSpace(rest[:idx])
			if idx := strings.Index(name, "["); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			}
			if name != "" {
				declared[name] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no declarations were found; the check is looking at nothing")
	}

	for _, claim := range dataguard.Claims() {
		if claim.Status != dataguard.StatusDeclared {
			continue
		}
		for _, match := range backtickedIdentifier.FindAllStringSubmatch(claim.Claim+" "+claim.Note, -1) {
			name := match[1]
			if declared[name] {
				t.Errorf("%s says there is no code, but it names %q and %q is declared in "+
					"production code; the row is inert, not declared", claim.ID, name, name)
			}
		}
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoCommentClaimsTheAssemblyRootWiresSomethingItDoesNot is the second
// production-claim rule, and it exists because the first one answered the
// wrong question.
//
// TestNoSymbolClaimsProductionWiringWhileBeingUnreachableFromIt asks "is this
// symbol dead?" A comment can be wrong without its symbol being dead.
// dataguard.Redactor's doc said "Production code wires NewRedactor(mode) from
// cmd/main.go" — and Redactor is very much alive, taken as a parameter by
// report/postmortem.go. What was false is the *location*: no non-test file
// under cmd/ had ever named either the type or the constructor, and the only
// place a Redactor is built in non-test code is postmortem.go's nil-default,
// which is RedactModeNone.
//
// Adding "from cmd/main.go" to productionClaims does not catch that, and this
// file says why at the point where it would have been tempting: the phrase list
// holds "wired from cmd/main.go", the comment reads "wires ... from cmd/main.go",
// and a phrase list makes its own exact wording the size of its blind spot.
// **The gate was asking whether the symbol was dead; the defect was that it was
// alive in the wrong place.** Reachability cannot answer that, so this rule
// reads the claim's target instead: if a sentence says a symbol is wired from
// the assembly root, the assembly root has to name it.
//
// It is one-directional on purpose. A comment that claims production wiring
// without naming a location belongs to the older rule. A sentence that names a
// location and a verb but no symbol ("Call from cmd/main.go once the LLM
// client is constructed") is an instruction to a human and asserts nothing.
func TestNoCommentClaimsTheAssemblyRootWiresSomethingItDoesNot(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "scripts", "domaincheck")); err != nil {
		t.Skipf("not at the repository root: %v", err)
	}
	records, err := parseAll([]string{root})
	if err != nil {
		t.Fatalf("walking the shipped tree: %v", err)
	}
	for _, c := range assemblyRootClaimViolations(records) {
		t.Errorf("%s:%d %s\n"+
			"  %s\n"+
			"  doc: %s\n"+
			"  Either name the file that actually does the wiring, or say in the comment "+
			"that nothing does — a reader who believes the sentence is being told the "+
			"opposite of what the binary does.",
			c.path, c.line, c.name, c.why, firstLine(c.doc))
	}
}

// TestTheAssemblyRootRuleIsNotVacuous is the assertion that keeps the rule from
// being decoration.
//
// A rule with zero findings on the shipped tree is indistinguishable from a
// rule that cannot fire, and this one has already failed that way three times
// (§ decision 371): it inherited productionClaims' exact wording, it read two
// of the three reference maps, and it read a doc field that is always empty
// for a type declaration. Each of those versions reported zero findings on a
// tree that contained the very defect the rule was written for.
//
// So the fixture below is the red case, and it is the red case the repository
// actually shipped — not a synthetic one.
func TestTheAssemblyRootRuleIsNotVacuous(t *testing.T) {
	dir := t.TempDir()
	// The shipped shape: the package under a subdirectory, the assembly root
	// under cmd/, and nothing in between naming the type.
	pkgDir := filepath.Join(dir, "dataguard")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "redactor.go"), []byte(`package dataguard

// Redactor is the public interface used by report to redact structured data.
// Production code wires NewRedactor(mode) from cmd/main.go.
type Redactor interface{ Mode() string }

// NewRedactor builds a Redactor.
func NewRedactor(mode string) Redactor { return nil }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdDir := filepath.Join(dir, "cmd", "app")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The assembly root exists and is a real main package. It just never names
	// the type or the constructor, which is precisely the defect.
	if err := os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte(`package main

func main() {}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	records, err := parseAll([]string{dir})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	violations := assemblyRootClaimViolations(records)
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(violations), violations)
	}
	if violations[0].name != "Redactor" {
		t.Errorf("the violation names %q, want Redactor", violations[0].name)
	}
	if !strings.Contains(violations[0].why, "NewRedactor") {
		t.Errorf("the violation blames %q; the comment's claim is about NewRedactor, and a "+
			"rule that named the comment's own subject instead would have missed it: %s",
			violations[0].why, violations[0].why)
	}
}

// TestTheAssemblyRootRuleAcceptsAClaimTheAssemblyRootCorroborates is the other
// half, and it is the half that keeps the first half honest.
//
// The repository has two live setters — Registry.SetChatToQueryLLM and
// Registry.SetWorkerSpawner — whose docs sit next to sentences mentioning
// cmd/main.go, and both are called from cmd/opskeeper/toolwiring.go. An earlier
// version of this rule reported both as unwired, because a method reached
// through a value (`reg.SetChatToQueryLLM(llm)`) is a selector on something
// that is not a package and lands in the unattributed map. **A rule that cries
// wolf on the first two real examples in the tree gets switched off, and a
// switched-off rule is worse than no rule because it still appears in the
// Makefile.**
func TestTheAssemblyRootRuleAcceptsAClaimTheAssemblyRootCorroborates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.go"), []byte(`package tools

// SetLLM wires the LLM client consumed by chat_to_query.
// Call from cmd/main.go once the LLM client is constructed.
func SetLLM(c LLMClient) {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdDir := filepath.Join(dir, "cmd", "app")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte(`package main

import "example.com/tools"

func main() { tools.SetLLM(nil) }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	records, err := parseAll([]string{dir})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if violations := assemblyRootClaimViolations(records); len(violations) != 0 {
		t.Fatalf("a corroborated claim was reported: %+v", violations)
	}
}

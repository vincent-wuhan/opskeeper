package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A doc comment that tells a reader "this is how the running binary gets it"
// is a claim about reachability, and this repository has already shipped one
// that was false: core/manager/biz/report/postmortem.go's
// NewPostmortemService was documented as the production constructor while
// having no caller outside _test.go, and neither PostmortemService nor
// PostmortemConfig is named anywhere else in the tree -- 1,027 lines of a
// finished feature that nothing turns on.
//
// The difference between that and ordinary dead code is that a reader is
// actively told the opposite. An unreferenced helper is invisible; an
// unreferenced constructor with a production claim in its doc is a trap,
// and the trap is only dangerous to somebody who believed it.
//
// So this is a gate rather than a report line, and it is deliberately
// one-directional: a reachable constructor whose comment never mentions
// production is ordinary, and flagging those would make the check cry wolf
// on the first well-commented constructor in the tree.

func TestNoSymbolClaimsProductionWiringWhileBeingUnreachableFromIt(t *testing.T) {
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
	res := analyse(records)

	claims := productionClaimViolations(res)
	if len(claims) == 0 {
		return
	}
	for _, c := range claims {
		t.Errorf("%s:%d %s is %s from production code, and its doc comment says the "+
			"binary reaches it that way.\n"+
			"  A reader who believes the comment is being told the opposite of what the "+
			"binary does. Either wire it, or say in the comment that it is not wired and "+
			"what is wired in its place -- the second is what decision 290 did here.\n"+
			"  doc: %s", c.path, c.line, c.name, c.why, firstLine(c.doc))
	}
}

// The check is only as good as the phrase list, and this is the assertion
// that keeps the list honest about being a list: a symbol nobody ever
// mentioned in a comment cannot be reported, so the empty result has to be
// distinguishable from "the walk read no comments at all".
func TestTheClaimCheckActuallySeesComments(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "honest.go"), []byte(`package p

// NewThing is the production constructor for the thing.
func NewThing() int { return 1 }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	records, err := parseAll([]string{dir})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := analyse(records)
	claims := productionClaimViolations(res)
	if len(claims) != 1 {
		t.Fatalf("got %d claim violations, want 1: %+v", len(claims), claims)
	}
	if claims[0].name != "NewThing" {
		t.Errorf("the violation names %q, want NewThing", claims[0].name)
	}
	// And the line, because a gate that cannot say where is a gate that has
	// to be re-run by hand. It is the func's own line rather than the first
	// line of its doc, which is the line an editor jumps to.
	if claims[0].line != 4 {
		t.Errorf("the violation is reported at line %d, want 4", claims[0].line)
	}
}

// A comment that merely mentions the words is not the same as a comment that
// claims them, and the fixture that would have caught decision 290's own
// fix -- a comment explaining that the phrase used to be there -- is the
// reason this is written down rather than left to the next person to
// rediscover by having their build go red.
func TestThePhraseListIsWhatItClaimsToBe(t *testing.T) {
	for _, phrase := range productionClaims {
		if phrase == "" {
			t.Fatal("the phrase list has an empty entry, which matches every comment")
		}
		if phrase != strings.ToLower(phrase) && phrase != strings.ToUpper(phrase) {
			// Mixed case is fine as long as the comparison lowercases both
			// sides, which claimsProduction does. This assertion is here to
			// make someone read that line before adding a phrase.
			continue
		}
	}
	for _, tc := range []struct {
		doc  string
		want bool
	}{
		{"NewX is the production constructor.", true},
		{"NewX is the Production Constructor.", true},
		{"NewX is the production entry point for the sink.", true},
		{"NewX 是生产构造函数。", true},
		{"NewX is a constructor.", false},
		{"NewX is called by main.", false},
		{"", false},
	} {
		if got := claimsProduction(tc.doc); got != tc.want {
			t.Errorf("claimsProduction(%q) = %v, want %v", tc.doc, got, tc.want)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// The half of the tree the check did not read. A setter is the shape this
// gate exists for — a comment that tells the reader which root wires it, and
// a method that nothing calls — and the walk used to copy the doc comment
// off top-level functions only, so every method in the repository was
// examined with a blank string. That is not a gap in the phrase list; it is
// the phrase list never being consulted for half the declarations, and it is
// invisible because a symbol the check cannot read produces the same result
// as a symbol that is honestly documented.
//
// It is what let ROADMAP C.1's chat_to_query BaseTool ship as "delivered"
// while no manager had ever registered it: SetChatToQueryLLM's doc said
// "Call from cmd/main.go once the LLM client is constructed", and the
// claim gate passed over the line that said it.
func TestAMethodsDocCommentIsReadToo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seam.go"), []byte(`package p

type Registry struct{}

func (r *Registry) SetThing(t string) {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seamdoc.go"), []byte(`package p

// SetOther wires the other thing. Call from cmd/main.go once it exists.
func (r *Registry) SetOther(t string) {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	records, err := parseAll([]string{dir})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claims := productionClaimViolations(analyse(records))
	if len(claims) != 1 {
		t.Fatalf("got %d claim violations, want 1: %+v", len(claims), claims)
	}
	if claims[0].name != "SetOther" {
		t.Errorf("the violation names %q, want SetOther", claims[0].name)
	}
}

// The two new phrases are what make the fixture above a violation rather
// than a merely well-commented method, and "call from cmd/main.go" is the
// exact wording the seam used. They are locked in here because a phrase list
// nobody asserts is a phrase list that gets edited by accident.
func TestASetterThatNamesTheAssemblyRootIsAProductionClaim(t *testing.T) {
	for _, doc := range []string{
		"SetThing wires the thing. Call from cmd/main.go once it exists.",
		"SetThing wires the thing. Called from cmd/main.go.",
		"SetThing wires the thing. Wired from main.go.",
		"SetThing wires the thing. nil-safe.",
	} {
		want := !strings.Contains(doc, "nil-safe")
		if got := claimsProduction(doc); got != want {
			t.Errorf("claimsProduction(%q) = %v, want %v", doc, got, want)
		}
	}
}

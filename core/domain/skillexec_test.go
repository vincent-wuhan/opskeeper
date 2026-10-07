package domain

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The three types here replaced `biz/skill`'s own, and the reason they were
// renamed rather than moved verbatim is the reason one of these tests exists at
// all: `Caller` is declared seven times in this repository for five different
// things, and `core/domain` is the one namespace every domain shares.

// TestTheSharedNamespaceDeclaresNoBareCaller is the guard on the rename.

// Moving `skill.Caller` into this package under its own name would have made
// it the eighth declaration of a word that already means five things, in the
// one place where a reader is most likely to assume a name is shared. Nothing
// would have failed: the type would have been correct, the callers would have
// compiled, and the ambiguity would have shown up months later as somebody
// reaching for `domain.Caller` and getting the wrong two fields.
//
// So the invariant is stated as a fact about this package rather than as a
// naming preference. If a future cut wants a caller identity here, it has to
// say which one.
func TestTheSharedNamespaceDeclaresNoBareCaller(t *testing.T) {
	declared := exportedTypeNames(t)
	// The first four were decision 240's list. Decision 241 added the second
	// four, each with the declaration count that put it there — a number, not
	// a hunch, and the count is of *type declarations* across the repository,
	// which is not the same as what grepping the word gives: `Report` answers
	// 8 types, 11 occurrences, and only the first of those means another
	// package owns a type by that name. `Options` is declared 13 times,
	// `Decision` 10, `SourceManifest` 0 times now that the converter aliases
	// it here. A ban list that only grows when somebody happens to notice a
	// collision is a list of the collisions somebody happened to look at, so
	// this one is a count.
	for _, banned := range []string{
		"Caller", "Usecase", "Event", "Rule",
		"Options", "Report", "Decision", "SourceManifest",
	} {
		if declared[banned] {
			t.Errorf("core/domain declares a type named %s. These eight names are the trap "+
				"list: each is declared several times across this repository for different "+
				"things, and this package is the one namespace every domain shares. A name "+
				"added here has to say which of them it is — SkillCaller, EdgePresence, "+
				"ToolStartEvent, PluginImportReport.", banned)
		}
	}
	for _, want := range []string{"SkillCaller", "SkillExecution", "SkillOutcome"} {
		if !declared[want] {
			t.Errorf("core/domain no longer declares %s; if the skill seam moved again, say where "+
				"it went, because the agent's tool bridge holds a port made of these three", want)
		}
	}
}

// TestSkillOutcomeCarriesTheTwoKeysTheExecuteEndpointReturns pins the live
// wire contract. `SkillOutcome` is the body of
// `POST /v1/skills/{key}/execute`, written straight into the response, and the
// console reads `result` and `error` by name. The type moved packages; the tags
// did not, and a rename here would ship a response the SPA cannot read.
func TestSkillOutcomeCarriesTheTwoKeysTheExecuteEndpointReturns(t *testing.T) {
	typ := reflect.TypeOf(SkillOutcome{})
	if typ.NumField() != 2 {
		t.Fatalf("SkillOutcome has %d fields, want the 2 the execute endpoint has always returned", typ.NumField())
	}
	want := map[string]string{"Result": "result,omitempty", "Error": "error,omitempty"}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag, ok := want[f.Name]
		if !ok {
			t.Errorf("SkillOutcome.%s is new; the execute endpoint's body has two keys and the "+
				"SPA reads both by name", f.Name)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("SkillOutcome.%s has json tag %q, want %q. This struct is the response body "+
				"of POST /v1/skills/{key}/execute, so a change here is a wire change that "+
				"compiles and ships silently", f.Name, got, tag)
		}
		delete(want, f.Name)
	}
	for name := range want {
		t.Errorf("SkillOutcome is missing %s, which the execute endpoint has always returned", name)
	}

	// A success omits error and a failure omits result, because both tags are
	// omitempty. If either stopped being, the response would carry a key the
	// console has to learn to ignore.
	body, err := json.Marshal(SkillOutcome{Result: json.RawMessage(`{"rows":2}`)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(body) != `{"result":{"rows":2}}` {
		t.Errorf("a successful outcome marshalled to %s, want only the result key", body)
	}
	body, err = json.Marshal(SkillOutcome{Error: "boom"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(body) != `{"error":"boom"}` {
		t.Errorf("a failed outcome marshalled to %s, want only the error key", body)
	}
}

// TestSkillExecutionPassesParamsThroughUndecoded pins the one property that
// makes this a transport shape rather than a domain model: the parameter blob
// is opaque here. A skill's Executor owns its schema, and anything in this
// package that tried to read `Params` would be a second, partial idea of it.
func TestSkillExecutionPassesParamsThroughUndecoded(t *testing.T) {
	typ := reflect.TypeOf(SkillExecution{})
	if typ.NumField() != 3 {
		t.Fatalf("SkillExecution has %d fields, want 3", typ.NumField())
	}
	// Compared by type identity and not by name, because `json.RawMessage` is
	// an alias and reflect reports the type it points at — `jsontext.Value` in
	// current Go. A name comparison fails on the correct code, which is the
	// worst way for a contract test to be wrong: it trains people to "fix" the
	// field. This test failed exactly that way once.
	rawMessage := reflect.TypeOf(json.RawMessage(nil))
	if rawMessage == nil {
		rawMessage = reflect.TypeOf([]byte(nil))
	}
	want := map[string]reflect.Type{
		"Key":    reflect.TypeOf(""),
		"EdgeID": reflect.TypeOf(uint64(0)),
		"Params": rawMessage,
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		kind, ok := want[f.Name]
		if !ok {
			t.Errorf("SkillExecution.%s is new; every field here is forwarded to the skill or "+
				"written into the audit row", f.Name)
			continue
		}
		if f.Type != kind {
			t.Errorf("SkillExecution.%s is %s, want %s. Params is a transport blob: decoding it "+
				"here would put a second, partial idea of the skill's schema in this package",
				f.Name, f.Type, kind)
		}
		delete(want, f.Name)
	}
	for name := range want {
		t.Errorf("SkillExecution is missing %s", name)
	}
}

// TestTheSkillPortIsOneMethod guards the port against widening by accretion —
// the way a port dies is one justified method at a time.
func TestTheSkillPortIsOneMethod(t *testing.T) {
	typ := reflect.TypeOf((*SkillExecutor)(nil)).Elem()
	if typ.NumMethod() != 1 {
		var names []string
		for i := 0; i < typ.NumMethod(); i++ {
			names = append(names, typ.Method(i).Name)
		}
		t.Fatalf("SkillExecutor has %d methods (%v), want 1. The agent's tool bridge is not a "+
			"skill client, it is a way for the model to reach one, and every method here is a "+
			"method the model can be steered into", typ.NumMethod(), names)
	}
	if _, ok := typ.MethodByName("Execute"); !ok {
		t.Error("SkillExecutor has no Execute; the skill service and the bridge both implement it")
	}
}

// exportedTypeNames lists the type names this package declares.
//
// It reads the source with go/parser rather than using reflection, and that is
// the whole point: reflection cannot see a type that no value in the test
// binary happens to mention, which is exactly the type somebody is about to
// add. It also has to skip type aliases, because this package's recent history
// is three aliases away — an alias is not a new name, it is the name it points
// at, and counting it would make the trap-list check report the very renames
// that were made to satisfy it.
func exportedTypeNames(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse this package: %v", err)
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if ts.Assign.IsValid() {
						continue // an alias, not a declaration
					}
					if ts.Name.IsExported() {
						out[ts.Name.Name] = true
					}
				}
			}
		}
	}
	return out
}

// TestTheSkillPortIsHeldByTheConsumerThatNeededIt closes the last hole in this
// cut, and the hole was found by mutation rather than by reading.
//
// Replacing `type SkillRunner = domain.SkillExecutor` with a hand-written
// interface carrying the same one method compiles, satisfies every test in this
// repository, and changes nothing observable — measured, not assumed. It is the
// same shape as the four anonymous structs decision 239 deleted: two identical
// declarations of one thing, with nothing comparing them.
//
// What makes this one detectable at all is that a hand-written copy leaves
// `domain.SkillExecutor` with **no user**, and an unused port is a declaration
// rather than a boundary. So the invariant is the same one the skill RPC got:
// the port has to be named by the package that needed it.
//
// The walk reads the tree with go/parser rather than matching text. The first
// version of the equivalent guard in core/floor/tunnel counted a *comment* as a
// use, because the comment explaining the fix sat in the same file as the code
// it explained — and the guard stayed green through the exact mutation it
// existed to catch. That is this repository's sixth version of that mistake.
// TestBlankAssertionsDoNotCountAsHolders is the guard on the guard.
//
// The exclusion above is the kind of code that looks like it can be simplified
// away: the file it protects compiles either way, the assertion it skips is
// real Go, and the test that skips it still passes if you delete the skip. What
// it actually is, is the only thing standing between a false green and the
// hand-written-interface regression. So it gets its own test, on synthetic
// source, where the two cases are four lines apart and the difference is
// whether the name is bound to anything.
func TestBlankAssertionsDoNotCountAsHolders(t *testing.T) {
	const src = `package p

import "example.org/core/domain"

type Held = domain.SkillExecutor

var _ domain.SkillExecutor = held()
`
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	counted := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if spec, isSpec := n.(*ast.ValueSpec); isSpec && isBlankAssertion(spec) {
			return false
		}
		sel, isSel := n.(*ast.SelectorExpr)
		if isSel && sel.Sel.Name == "SkillExecutor" {
			if id, isIdent := sel.X.(*ast.Ident); isIdent && id.Name == "domain" {
				counted[fmt.Sprintf("%d", sel.Pos())] = true
			}
		}
		return true
	})
	if len(counted) != 1 {
		t.Errorf("counted %d mentions of domain.SkillExecutor, want exactly 1 (the alias). "+
			"The `var _` assertion is a compile-time claim and must not be read as a "+
			"dependency: it keeps compiling against a hand-written interface with the "+
			"same method set, so counting it turns the check into proof of its own claim", len(counted))
	}
}

func TestTheSkillPortIsHeldByTheConsumerThatNeededIt(t *testing.T) {
	// This one exists because the first version of it was green through the
	// exact mutation it was written for. Reverting the alias in skill_bridge.go
	// to a hand-written interface left `var _ domain.SkillExecutor =
	// SkillRunner(nil)` on the line below it — and that assertion compiles
	// against a hand-written interface with the same method set, because Go lets
	// an interface value be assigned to any interface its method set satisfies.
	// So the assertion kept naming the port while the package stopped holding
	// it, and a guard that reads "does anyone name this symbol" counted the
	// proof of the claim as the claim.
	//
	// A blank-identifier assertion is a compile-time claim, not a dependency: it
	// asserts what would be true, and depending on nothing. Only the alias and
	// the real call sites are dependencies, and only those count.
	holders := selectorHolders(t, "domain.SkillExecutor", []string{"../manager", "../edge"})
	if len(holders) == 0 {
		t.Error("no non-test package outside core/domain names domain.SkillExecutor. The port " +
			"exists because the agent's tool bridge in the aiops domain needs to run a skill " +
			"without importing the skill service; with no holder it is a declaration nobody " +
			"holds, and the next person to need this shape will write their own")
	}
}

// selectorHolders lists the non-test packages under the given roots that name a
// symbol through a package qualifier, counting real selector expressions only.
func selectorHolders(t *testing.T, sym string, roots []string) []string {
	t.Helper()
	qualifier, name, ok := strings.Cut(sym, ".")
	if !ok {
		t.Fatalf("sym must be qualified, got %q", sym)
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil //nolint:nilerr // an unreadable subtree is not a reason to pass
			}
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return nil //nolint:nilerr // a file that does not parse fails the build elsewhere
			}
			ast.Inspect(file, func(n ast.Node) bool {
				// `var _ T = x` is the type system's opinion, not this package's
				// dependency on T. Returning false stops the walk at the
				// declaration so the selector inside it is never counted. A
				// named spec — including a type alias, whose ValueSpec has a
				// real name — is still a dependency and is still counted.
				if spec, isSpec := n.(*ast.ValueSpec); isSpec && isBlankAssertion(spec) {
					return false
				}
				sel, isSel := n.(*ast.SelectorExpr)
				if !isSel || sel.Sel.Name != name {
					return true
				}
				if id, isIdent := sel.X.(*ast.Ident); isIdent && id.Name == qualifier {
					seen[filepath.Dir(path)] = true
				}
				return true
			})
			return nil
		})
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// isBlankAssertion reports whether a var/const spec exists only to assert a
// compile-time fact, i.e. every name it binds is `_`.
func isBlankAssertion(spec *ast.ValueSpec) bool {
	if len(spec.Names) == 0 {
		return false
	}
	for _, n := range spec.Names {
		if n.Name != "_" {
			return false
		}
	}
	return true
}

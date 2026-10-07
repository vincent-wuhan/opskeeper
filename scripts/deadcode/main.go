// Command deadcode reports production code that only its own tests reach.
//
// domaincheck answers "which bounded contexts depend on which". It works
// at package granularity, and decision 116 found what that granularity
// cannot see: a 569-line migration window inside data/hitl/store that no
// production caller had ever invoked, while 3,869 tests passed. The tests
// were good tests of code nothing ran.
//
// This tool looks one level below a package. A file is reported when every
// symbol it declares — functions, methods, types, vars, consts — is
// unreachable from any non-test file in the tree. The line count that comes
// out is the size of the "wire it up or delete it" backlog, and it is the
// number stage 3 needs before deciding whether the remaining volume is
// split or cut.
//
// It reports and exits 0. That is deliberate, and the reasons are in
// falsePositives below: a name-based reachability walk cannot be made
// sound, because interface satisfaction, reflection, cgo and go:linkname
// all reach code without ever naming it. A gate built on it would train
// people to add escape hatches, and an escape hatch to a deadness checker
// is a comment that says "trust me".
//
// Usage:
//
//	go run ./scripts/deadcode [dir ...]
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	gopath "path"
	"path/filepath"
	"regexp"

	"github.com/vincent-wuhan/opskeeper/scripts/internal/modpath"
	"sort"
	"strings"
)

// knownContracts are method names the standard library, the runtime and
// the language reach without a visible call site. A method with one of
// these names is never reported even if nothing in the tree names it,
// because "nothing calls MarshalJSON" is not a statement about whether
// json.Marshal will call it.
var knownContracts = map[string]bool{
	"Error": true, "String": true, "Read": true, "Write": true,
	"Close": true, "Len": true, "Less": true, "Swap": true,
	"Scan": true, "Value": true, "MarshalJSON": true, "UnmarshalJSON": true,
	"MarshalYAML": true, "UnmarshalYAML": true, "MarshalText": true,
	"UnmarshalText": true, "GobEncode": true, "GobDecode": true,
	"ServeHTTP": true, "Reset": true, "Equal": true, "Is": true,
	"Format": true, "Greet": true,
}

// falsePositives is the honest list of what this walk cannot see. It is
// printed with every run so that nobody treats the number as a proof.
//
//  1. Reflection. reflect.Value.MethodByName("X") and a template calling a
//     method reach code by string. Grep for the string before believing a
//     method is dead.
//  2. go:linkname and //go:linkname, plus assembly stubs.
//  3. cgo: an exported //export-ed symbol is called from C.
//  4. Struct tags that name a codec: json:"-" is invisible, and so is a
//     yaml tag that a driver looks up by name.
//  5. A method promoted by embedding: the outer type's method set is the
//     union, and nothing in this tree names the promoted name.
//  6. Build tags. A file excluded by the current GOOS/GOARCH is still
//     parsed here, so its symbols are counted as live. That errs toward
//     reporting less, which is the right direction for a report.
//  7. An import this walk cannot map to a directory it walked — a dot
//     import, or a module it was not pointed at — drops that whole file
//     back to name-only matching. In those files two same-named symbols in
//     different packages can still vouch for each other, which is how
//     middleware/adapter/decorator's WithTenant stayed invisible for as
//     long as it did: basetool has a WithTenant too.
//
// A receiver that is a value rather than a package (svc.Method(),
// pkg.Constructor().Field) is not on this list: those names count
// everywhere, because the use site genuinely does not say which package
// declared them. Reporting them as dead was the false positive this
// attribution had to avoid — cmd/opskeeper calls setting.AgentWriteEnabled
// through a variable, and the first version of this walk called it
// unreachable.
const falsePositives = `reflection · go:linkname · cgo //export · struct-tag codecs ·
embedded-method promotion · build tags · unmappable imports (name-only fallback)`

// decl is one symbol a file declares.
type decl struct {
	name     string
	receiver string // "" for a top-level symbol
	pos      token.Position
	// doc is the declaration's own doc comment, and it is read for one
	// reason only: a comment that tells a reader this symbol is how the
	// running binary gets the thing it builds is a claim about
	// reachability, and the walk that produced the verdict is the thing
	// that can check it. Nothing else here looks at prose.
	doc string
}

// fileRecord is one parsed .go file.
type fileRecord struct {
	path  string
	test  bool
	decls []decl
	// refs counts, per name, how many times this file names it somewhere
	// that is not a declaration. A bare name reaches only this file's own
	// package; pkg.Name reaches the package that import resolves to.
	refs map[string]int
	// qualified counts "<import path>.<Symbol>" references, which are the
	// only ones that may reach another package.
	qualified map[string]int
	// pkgDir is the file's directory, which is how this walk names a
	// package. Directory rather than package clause: two files in one
	// directory are one package even when one of them is package foo_test.
	pkgDir string
	// imports maps the local name of each import to its path. A file with
	// a dot or blank import sets nameOnly, because a dot import reaches
	// symbols this walk cannot attribute to a package.
	imports  map[string]string
	nameOnly bool
	// unattributed counts selector names whose receiver is a value rather
	// than a package: svc.Method(), pkg.Constructor().Field. Nothing at the
	// use site says which package declared the symbol, so these names count
	// everywhere. That is the conservative direction, and it is what keeps
	// a live method from being reported dead just because it is called
	// through a variable.
	unattributed map[string]int
	// lines is the file's line count, used to size the report.
	lines int
}

func main() {
	dirs := os.Args[1:]
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	records, err := parseAll(dirs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deadcode:", err)
		os.Exit(2)
	}
	report := analyse(records)
	report.print(os.Stdout)
	// Always 0. See the package comment.
	os.Exit(0)
}

// parseAll reads every .go file under dirs, skipping vendor, testdata,
// dot-directories and generated output.
func parseAll(dirs []string) ([]*fileRecord, error) {
	var out []*fileRecord
	// Callers pass overlapping roots (".", "core", "core/manager"), and
	// walking them separately visits the same file once per root. Dedupe on
	// the absolute path or every finding is reported N times.
	seen := map[string]bool{}
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				switch info.Name() {
				case "vendor", "testdata", ".git", "node_modules":
					return filepath.SkipDir
				}
				if strings.HasPrefix(info.Name(), ".") && info.Name() != "." {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			abs, aerr := filepath.Abs(path)
			if aerr != nil {
				return aerr
			}
			if seen[abs] {
				return nil
			}
			seen[abs] = true
			rec, err := parseFile(path)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			out = append(out, rec)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func parseFile(path string) (*fileRecord, error) {
	fset := token.NewFileSet()
	// ParseComments is required and not optional: without it every Doc field
	// is nil, so the production-claim check below would see no comments at
	// all and report "nothing claims production" on a tree where something
	// does. That is the same failure shape as a gate that greps for a string
	// the build already stripped: green, and wrong.
	src, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	abs, aerr := filepath.Abs(path)
	if aerr != nil {
		return nil, aerr
	}
	rec := &fileRecord{
		path:         filepath.ToSlash(path),
		test:         strings.HasSuffix(path, "_test.go"),
		refs:         map[string]int{},
		qualified:    map[string]int{},
		pkgDir:       filepath.ToSlash(filepath.Dir(abs)),
		imports:      map[string]string{},
		unattributed: map[string]int{},
	}
	for _, imp := range src.Imports {
		ipath := strings.Trim(imp.Path.Value, `"`)
		local := gopath.Base(ipath)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == "." || local == "_" {
			rec.nameOnly = true
			continue
		}
		rec.imports[local] = ipath
	}
	rec.lines = fset.Position(src.End()).Line

	// Pass 1: declarations. Their own names are not references.
	for _, d := range src.Decls {
		switch n := d.(type) {
		case *ast.FuncDecl:
			if n.Recv != nil && len(n.Recv.List) > 0 {
				rec.decls = append(rec.decls, decl{
					name: n.Name.Name, receiver: receiverName(n), pos: fset.Position(n.Name.Pos()),
					doc: docText(n.Doc),
				})
			} else {
				rec.decls = append(rec.decls, decl{
					name: n.Name.Name, pos: fset.Position(n.Name.Pos()), doc: docText(n.Doc),
				})
			}
		case *ast.GenDecl:
			// A doc comment above `type Foo interface{...}` attaches to the
			// GenDecl, not to the TypeSpec, so reading only s.Doc reads nothing
			// for the most common shape a declaration takes. That is why the
			// production-claim gate never once looked at a type's comment — and
			// why the false claim on Redactor survived a rule written to catch
			// it: the gate was reading a field that was always empty for types.
			//
			// **A check that reads an always-empty field passes for the same
			// reason a broken instrument reads zero.**
			groupDoc := docText(n.Doc)
			if n.Lparen.IsValid() {
				groupDoc = ""
			}
			for _, spec := range n.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					doc := docText(s.Doc)
					if doc == "" {
						doc = groupDoc
					}
					rec.decls = append(rec.decls, decl{name: s.Name.Name, pos: fset.Position(s.Name.Pos()), doc: doc})
				case *ast.ValueSpec:
					for _, nm := range s.Names {
						rec.decls = append(rec.decls, decl{name: nm.Name, pos: fset.Position(nm.Pos())})
					}
				}
			}
		}
	}

	// Pass 2: every identifier that is not a declaration site. Doc comments
	// are not parsed as idents, so a symbol mentioned only in prose does
	// not count as used — which is the whole point: a name in a comment is
	// a claim, not a call.
	// Pass 2a: qualified references. Recorded against the import path so
	// that pkg.Foo reaches the package that import names, and is removed
	// from the bare-name pass below — otherwise the same symbol would also
	// count as a mention inside the referencing file's own package.
	qualifiedIdents := map[token.Pos]bool{}
	ast.Inspect(src, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if qual, isIdent := sel.X.(*ast.Ident); isIdent {
			if ipath, imported := rec.imports[qual.Name]; imported {
				rec.qualified[ipath+"."+sel.Sel.Name]++
				qualifiedIdents[qual.Pos()] = true
				qualifiedIdents[sel.Sel.Pos()] = true
				return true
			}
		}
		// Either the receiver is a value (svc.Method) or it is a chain
		// (pkg.Constructor().Method). Neither names the declaring package,
		// so the symbol name counts everywhere rather than only here.
		rec.unattributed[sel.Sel.Name]++
		qualifiedIdents[sel.Sel.Pos()] = true
		return true
	})

	ast.Inspect(src, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && !isDeclarationIdent(src, id) && !qualifiedIdents[id.Pos()] {
			rec.refs[id.Name]++
		}
		return true
	})
	return rec, nil
}

func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	var b strings.Builder
	ast.Inspect(fn.Recv.List[0].Type, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			b.WriteString(id.Name)
		}
		return true
	})
	return b.String()
}

// isDeclarationIdent reports whether id is the name a declaration binds,
// which is the one occurrence of that name in the file that is not a use.
func isDeclarationIdent(src *ast.File, id *ast.Ident) bool {
	for _, d := range src.Decls {
		switch n := d.(type) {
		case *ast.FuncDecl:
			if n.Name == id {
				return true
			}
		case *ast.GenDecl:
			for _, spec := range n.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name == id {
						return true
					}
				case *ast.ValueSpec:
					for _, nm := range s.Names {
						if nm == id {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// verdict is why a symbol is unreachable. The two tiers are kept apart
// because they call for different decisions: dead means nothing calls it,
// test-only means something does and it is a test, and a test for a
// function nothing calls is the shape decision 116 had to find by hand.
type verdict int

const (
	live verdict = iota
	dead
	testOnly
)

func (v verdict) String() string {
	switch v {
	case dead:
		return "dead"
	case testOnly:
		return "test-only"
	}
	return "live"
}

// result is the analysis over a parsed tree.
type result struct {
	// methodNames is every method name any interface in the tree declares.
	methodNames map[string]bool
	// file report
	files []*fileFinding
	// deadSymbols counts every unreachable symbol, whole file or not.
	deadSymbols int
	// byTierExact splits deadSymbols per symbol rather than per file. The
	// byTier map below is per file and therefore a coarser measure: a file
	// with one dead symbol and nine test-only ones lands entirely in the
	// dead bucket. Decision 285 needed the per-symbol number because the
	// finding it acted on was a test-only classification — two access
	// points on a documented control that only tests reach — and the report
	// could not be counted in the class the finding was in.
	deadOnlySymbols int
	testOnlySymbols int
	// byTier splits those symbols by why they are unreachable.
	byTier map[verdict]int
	// unreachableFiles / unreachableLines count only files where every
	// declared symbol is unreachable, because only then is the line count
	// a claim about how much could go.
	unreachableFiles int
	unreachableLines int
	deadFiles        int
	testOnlyFiles    int
}

// fileFinding is one file with at least one unreachable symbol.
type fileFinding struct {
	path     string
	lines    int
	findings []symbolFinding
	// unreachable is true when every symbol the file declares is
	// unreachable, which is the only case where the file's line count is
	// meaningful as "this many lines could go".
	unreachable bool
}

type symbolFinding struct {
	name string
	why  verdict
	line int
	// claimDoc is the declaration's doc comment, carried only so the
	// production-claim check can compare what the comment asserts against
	// the verdict the walk reached. It is empty for symbols whose comment
	// says nothing about production, which is nearly all of them.
	claimDoc string
}

func analyse(records []*fileRecord) *result {
	res := &result{methodNames: map[string]bool{}, byTier: map[verdict]int{}}

	// An interface method is reached by assignment, never by name. Collect
	// them all first so a satisfying method is never called dead.
	for _, rec := range records {
		if rec.test {
			continue
		}
		collectInterfaceMethods(rec, res.methodNames)
	}

	// Package directories the walk saw, and the import path that names each
	// one. A reference is attributed to a package through this index, not
	// through the name it happens to share with a symbol elsewhere.
	pkgDirs := map[string]bool{}
	for _, rec := range records {
		pkgDirs[rec.pkgDir] = true
	}
	moduleCache := map[string]modpath.Answer{}
	importIndex := map[string]string{}
	for dir := range pkgDirs {
		ans, cached := moduleCache[dir]
		if !cached {
			ans = modpath.Of(dir)
			moduleCache[dir] = ans
		}
		mpath, mroot, ok := ans.Path, ans.Root, ans.OK
		if !ok {
			continue
		}
		rel, err := filepath.Rel(mroot, dir)
		if err != nil {
			continue
		}
		ipath := mpath
		if rel != "." {
			ipath = mpath + "/" + filepath.ToSlash(rel)
		}
		importIndex[ipath] = dir
	}

	// pkgDir -> name -> mentioned. prodPkg is what non-test files named,
	// testPkg what test files named.
	prodPkg := map[string]map[string]bool{}
	testPkg := map[string]map[string]bool{}
	// prodAnywhere / testAnywhere hold the mentions from files whose
	// imports could not be resolved, which keep the old tree-wide matching
	// rather than inventing a dead symbol out of a walk that gave up.
	prodAnywhere := map[string]bool{}
	testAnywhere := map[string]bool{}
	mention := func(pkg map[string]map[string]bool, anywhere map[string]bool, rec *fileRecord) {
		byName := pkg[rec.pkgDir]
		if byName == nil {
			byName = map[string]bool{}
			pkg[rec.pkgDir] = byName
		}
		for name := range rec.refs {
			byName[name] = true
			if rec.nameOnly {
				anywhere[name] = true
			}
		}
		for name := range rec.unattributed {
			anywhere[name] = true
		}
		for ref := range rec.qualified {
			// Split at the LAST dot: an import path is full of dots
			// ("example.com/x/pkg") and a Go identifier has none, so the
			// last one is the only place the two can be told apart.
			cut := strings.LastIndex(ref, ".")
			if cut <= 0 {
				continue
			}
			ipath, name := ref[:cut], ref[cut+1:]
			dir, resolved := importIndex[ipath]
			if !resolved {
				anywhere[name] = true
				continue
			}
			target := pkg[dir]
			if target == nil {
				target = map[string]bool{}
				pkg[dir] = target
			}
			target[name] = true
		}
	}
	for _, rec := range records {
		if rec.test {
			mention(testPkg, testAnywhere, rec)
			continue
		}
		mention(prodPkg, prodAnywhere, rec)
	}

	for _, rec := range records {
		if rec.test || len(rec.decls) == 0 {
			continue
		}
		var fs []symbolFinding
		for _, d := range rec.decls {
			why := classify(d, rec, prodPkg, testPkg, prodAnywhere, testAnywhere, res.methodNames)
			if why == live {
				continue
			}
			fs = append(fs, symbolFinding{name: d.name, why: why, line: d.pos.Line, claimDoc: d.doc})
		}
		if len(fs) == 0 {
			continue
		}
		f := &fileFinding{
			path:        rec.path,
			lines:       rec.lines,
			findings:    fs,
			unreachable: len(fs) == len(rec.decls),
		}
		if f.unreachable {
			res.unreachableFiles++
			res.unreachableLines += rec.lines
			switch worst(fs) {
			case dead:
				res.deadFiles++
			case testOnly:
				res.testOnlyFiles++
			}
		}
		res.deadSymbols += len(fs)
		for _, s := range fs {
			switch s.why {
			case dead:
				res.deadOnlySymbols++
			case testOnly:
				res.testOnlySymbols++
			}
		}
		res.byTier[worst(fs)] += len(fs)
		res.files = append(res.files, f)
	}
	sort.Slice(res.files, func(i, j int) bool {
		if res.files[i].unreachable != res.files[j].unreachable {
			return res.files[i].unreachable
		}
		if res.files[i].lines != res.files[j].lines {
			return res.files[i].lines > res.files[j].lines
		}
		return res.files[i].path < res.files[j].path
	})
	return res
}

// worst returns the more actionable of two verdicts: something nothing
// names at all is a stronger signal than something only a test names.
func worst(fs []symbolFinding) verdict {
	w := testOnly
	for _, f := range fs {
		if f.why == dead {
			return dead
		}
	}
	return w
}

func classify(
	d decl,
	rec *fileRecord,
	prodPkg, testPkg map[string]map[string]bool,
	prodAnywhere, testAnywhere map[string]bool,
	ifaces map[string]bool,
) verdict {
	switch d.name {
	case "main", "init":
		// Reached by the runtime, never by a name.
		return live
	}
	if d.receiver != "" && (ifaces[d.name] || knownContracts[d.name]) {
		// Reached by being put in a variable of an interface type.
		return live
	}
	if knownContracts[d.name] {
		return live
	}
	if prodPkg[rec.pkgDir][d.name] || prodAnywhere[d.name] {
		// Named by another file of the same package, or by a file that
		// reached it through an import.
		return live
	}
	if rec.refs[d.name] > 0 {
		// Named inside its own file outside the declaration: a same-file
		// call is still a call.
		return live
	}
	if testPkg[rec.pkgDir][d.name] || testAnywhere[d.name] {
		return testOnly
	}
	return dead
}

func collectInterfaceMethods(rec *fileRecord, out map[string]bool) {
	fset := token.NewFileSet()
	src, err := parser.ParseFile(fset, rec.path, nil, 0)
	if err != nil {
		return
	}
	ast.Inspect(src, func(n ast.Node) bool {
		it, ok := n.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, m := range it.Methods.List {
			for _, name := range m.Names {
				out[name.Name] = true
			}
		}
		return true
	})
}

func (r *result) print(w *os.File) {
	fmt.Fprintf(w, "deadcode: %d symbols unreachable from production code\n", r.deadSymbols)
	fmt.Fprintf(w, "deadcode: of those, %d are dead and %d are test-only\n", r.deadOnlySymbols, r.testOnlySymbols)
	fmt.Fprintf(w, "deadcode: %d whole files / %d lines are unreachable (%d files name nothing at all, %d are named only by tests)\n",
		r.unreachableFiles, r.unreachableLines, r.deadFiles, r.testOnlyFiles)
	if claims := productionClaimViolations(r); len(claims) > 0 {
		fmt.Fprintf(w, "deadcode: %d symbols whose doc comment claims production wiring are unreachable from it\n", len(claims))
		for _, c := range claims {
			fmt.Fprintf(w, "  CLAIMS-PRODUCTION %s:%d  %s  (%s)\n", c.path, c.line, c.name, c.why)
		}
	} else {
		fmt.Fprintf(w, "deadcode: no symbol claims production wiring while being unreachable from it\n")
	}
	for _, f := range r.files {
		kind := "partial"
		if f.unreachable {
			kind = worst(f.findings).String()
		}
		parts := make([]string, 0, len(f.findings))
		for _, s := range f.findings {
			parts = append(parts, s.name+":"+s.why.String())
		}
		fmt.Fprintf(w, "  %-10s %6d lines  %-62s %s\n",
			kind, f.lines, f.path, strings.Join(parts, " "))
	}
	fmt.Fprintf(w, "deadcode: this walk cannot see %s\n", falsePositives)
	fmt.Fprintln(w, "deadcode: report only, exit 0 — see the package comment before turning this into a gate")
}

// docText flattens a doc comment. Only the first line of each sentence group
// is kept by the caller that cares, but flattening here means the claim
// check does not have to know how go/ast splits a comment into groups.
func docText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	return g.Text()
}

// productionClaims are the phrases that assert a symbol is how the running
// binary reaches what it builds.
//
// It is a list, and a list is the weak point: a comment can say "this is what
// main() calls" in a hundred ways and the walk will not see any of them. What
// the list buys is narrower and real -- when one of these exact phrases is
// written, the verdict has to agree with it or the comment is wrong. The
// failure this catches is not an unusual phrasing, it is the one this
// repository already shipped: core/manager/biz/report/postmortem.go's
// NewPostmortemService was documented as "the production constructor" while
// having no caller outside tests, and the type it returns had none at all
// (decision 290).
var productionClaims = []string{
	"production constructor",
	"production entry point",
	"the production wiring",
	"生产构造函数",
	"生产入口",
	"call from cmd/main.go",
	"called from cmd/main.go",
	"wired from cmd/main.go",
	"call from main.go",
	"called from main.go",
	"wired from main.go",
}

// claimsProduction reports whether a doc comment asserts production wiring.
func claimsProduction(doc string) bool {
	lowered := strings.ToLower(doc)
	for _, phrase := range productionClaims {
		if strings.Contains(lowered, strings.ToLower(phrase)) {
			return true
		}
	}
	return false
}

// claimVerdict pairs a production claim with what the walk found.
type claimVerdict struct {
	path string
	line int
	name string
	doc  string
	why  string
}

// productionClaimViolations returns every symbol whose doc comment claims it is
// production wiring while the walk found it unreachable from production.
//
// The check is deliberately one-directional. A reachable constructor whose
// comment does not mention production is ordinary; an unreachable one whose
// comment says it is the production constructor is a reader being told to
// believe something the running binary does not do.
func productionClaimViolations(res *result) []claimVerdict {
	var out []claimVerdict
	for _, f := range res.files {
		for _, sym := range f.findings {
			if sym.claimDoc == "" || !claimsProduction(sym.claimDoc) {
				continue
			}
			out = append(out, claimVerdict{
				path: f.path, line: sym.line, name: sym.name, doc: sym.claimDoc, why: sym.why.String(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// --- decision 371: a claim that names where the wiring lives is checkable ---
//
// `claimsProduction` above answers "does this comment claim production
// wiring". The gate that uses it answers "is the symbol unreachable". Those
// are different questions, and the gap between them is where a false claim
// survives: **a symbol can be reachable and still not be wired the way its
// comment says it is.**
//
// This repository shipped exactly that. `dataguard.Redactor`'s doc said
// "Production code wires NewRedactor(mode) from cmd/main.go". `Redactor` is
// reachable — postmortem.go takes one as a parameter — so the existing gate
// stayed green while the claim was false: nothing under `cmd/` had ever named
// the type, and the one place a Redactor is constructed in non-test code is
// postmortem.go's nil-default, which is `RedactModeNone`.
//
// Adding "from cmd/main.go" to the phrase list does not catch that, and this
// file says so at the point where it would have been tempting: the list is a
// list, and the reason it missed is not wording. **The gate asked whether the
// symbol was dead; the defect was that it was alive in the wrong place.**
//
// So the second rule reads the claim's *target*: when a doc comment attributes
// wiring to the assembly root, the assembly root has to name the symbol. That
// is a local check — no call graph, no reachability analysis — and it is
// exactly the thing a reader would check by hand before believing the comment.

// wiringVerbs are the ways a sentence can assert that something gets built.
//
// They are listed separately from productionClaims on purpose. The earlier
// version of this rule asked claimsAssemblyRootWiring to call claimsProduction
// first, and that made it a strict subset of the phrase list — so a comment
// reading "Production code **wires** NewRedactor(mode) from cmd/main.go"
// produced no claim, because the list holds "wired from cmd/main.go" and not
// "wires". **A rule that leans on a phrase list it does not control inherits
// that list's exact wording as its own blind spot.**
var wiringVerbs = []string{
	"wire", "wires", "wired", "wiring",
	"call from", "called from", "calls from",
	"construct", "constructs", "constructed",
	"built in", "created in", "created by", "instantiated",
	"接", "调用", "构造",
}

// assemblyRootLocations are the ways a sentence can name where the wiring
// lives. Unlike productionClaims these are a *location*, and a location can be
// checked: the assembly root either names the symbol or it does not.
var assemblyRootLocations = []string{
	"cmd/", "main.go", "the assembly root", "assembly root", "装配根",
}

// claimSubjects returns the symbols a sentence asserts are wired from the
// assembly root, and whether that sentence makes such an assertion at all.
//
// The subject is whatever declared symbol the sentence names, not the symbol
// the comment happens to be attached to. That distinction is the whole point:
// "Production code wires NewRedactor(mode) from cmd/main.go" sits on Redactor's
// doc and is a claim about NewRedactor, so asking "does cmd/ name Redactor"
// would miss it while asking "does cmd/ name a symbol this sentence mentions"
// catches it.
//
// A sentence that names no declared symbol makes no checkable claim, and
// returns false rather than a vacuous violation. "Call from cmd/main.go once the
// LLM client is constructed" is an instruction to a human and asserts nothing
// about any particular symbol — flagging it would be the cry-wolf this file
// already warns about twice.
func claimSubjects(sentence string, declared map[string]bool) ([]string, bool) {
	lowered := strings.ToLower(sentence)
	wired := false
	for _, v := range wiringVerbs {
		if containsWord(sentence, v) {
			wired = true
			break
		}
	}
	if !wired {
		return nil, false
	}
	named := false
	for _, loc := range assemblyRootLocations {
		if strings.Contains(lowered, strings.ToLower(loc)) {
			named = true
			break
		}
	}
	if !named {
		return nil, false
	}
	var subjects []string
	for _, loc := range identifierRE.FindAllStringIndex(sentence, -1) {
		ident := sentence[loc[0]:loc[1]]
		if !declared[ident] {
			continue
		}
		// The symbol has to appear **as code**, not as a word. The first
		// version took every identifier in the sentence, and a paragraph in
		// scripts/domaincheck produced "the comment says of is wired from the
		// assembly root" — `of` is a parameter name somewhere in the tree, and
		// the sentence was prose about domains rather than a claim about it.
		//
		// **A check that reads prose with a code-shaped regex will eventually
		// find a code-shaped word in the prose**, and the fix is never to widen
		// the exceptions; it is to require the sentence to name its subject the
		// way the sentence would name it if it were talking about code.
		if !namedAsCode(sentence, loc) {
			continue
		}
		subjects = append(subjects, ident)
	}
	sort.Strings(subjects)
	return subjects, len(subjects) > 0
}

// namedAsCode reports whether an identifier occurrence is written as code:
// inside backticks, or immediately followed by `(` or `.` (a call or a
// selector). `NewRedactor(mode)` and `dataguard.Redactor` qualify;
// the word "of" in an English sentence does not.
func namedAsCode(sentence string, loc []int) bool {
	before := strings.TrimRight(sentence[:loc[0]], "` ")
	if strings.HasSuffix(before, "`") {
		return true
	}
	after := sentence[loc[1]:]
	return strings.HasPrefix(after, "(") || strings.HasPrefix(after, ".")
}

// containsWord reports whether needle appears in haystack delimited by
// non-identifier characters on both sides.
//
// The wiring-verb list needs this. "unwired" contains "wired", and a
// substring match made a sentence about domains that are *not* wired read as a
// claim that something is — which is the check's whole failure mode inverted.
func containsWord(haystack, needle string) bool {
	loweredHay, loweredNeedle := strings.ToLower(haystack), strings.ToLower(needle)
	for i := 0; i+len(loweredNeedle) <= len(loweredHay); i++ {
		if loweredHay[i:i+len(loweredNeedle)] != loweredNeedle {
			continue
		}
		if i > 0 && isIdentByte(loweredHay[i-1]) {
			continue
		}
		end := i + len(loweredNeedle)
		if end < len(loweredHay) && isIdentByte(loweredHay[end]) {
			continue
		}
		return true
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// identifierRE finds Go identifiers in prose: capitalised or snake_case words
// that could be a symbol name. It deliberately also matches lowercase words,
// because this repository spells a great many symbols in snake_case.
var identifierRE = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// splitSentences breaks a flattened doc comment into sentences on `.` and
// the CJK full stop. Grouping matters: a comment can carry one true claim and
// three unrelated sentences, and a rule that reads the whole comment either
// over- or under-fires depending on which sentence carries the claim.
func splitSentences(doc string) []string {
	fields := strings.FieldsFunc(doc, func(r rune) bool {
		return r == '.' || r == '\n' || r == 0x3002
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// isThisChecker reports whether a path is this package's own source.
func isThisChecker(path string) bool {
	return strings.HasSuffix(filepath.ToSlash(path), "/scripts/deadcode/main.go")
}

// isAssemblyRootPath reports whether a file lives under a cmd/ directory,
// which is where a main package is assembled in this repository.
func isAssemblyRootPath(path string) bool {
	slashed := filepath.ToSlash(path)
	return strings.Contains(slashed, "/cmd/") || strings.HasPrefix(slashed, "cmd/")
}

// assemblyRootClaimViolations returns every symbol whose doc comment says the
// assembly root wires it while no non-test file under cmd/ names it.
//
// Both directions are deliberately quiet. A comment that claims production
// wiring without naming a location is the existing gate's business, not this
// one's. And a comment that names the assembly root but not this symbol is
// ordinary prose — "PostmortemContent is consumed by the loop phase, which the
// assembly root starts" is true without cmd/ naming PostmortemContent. What
// this rule forbids is the narrower and much more checkable sentence: *this
// symbol, wired from over there*, with nothing over there.
func assemblyRootClaimViolations(records []*fileRecord) []claimVerdict {
	declared := map[string]bool{}
	for _, rec := range records {
		for _, d := range rec.decls {
			declared[d.name] = true
		}
	}

	namedByAssembly := map[string]bool{}
	for _, rec := range records {
		if rec.test || !isAssemblyRootPath(rec.path) {
			continue
		}
		for name := range rec.refs {
			namedByAssembly[name] = true
		}
		for qualified := range rec.qualified {
			if dot := strings.LastIndex(qualified, "."); dot >= 0 {
				namedByAssembly[qualified[dot+1:]] = true
			}
		}
		// A method reached through a value — reg.SetChatToQueryLLM(llm) — is a
		// selector on something that is not a package, so it lands in
		// unattributed rather than in refs or qualified. The first version of
		// this rule read only the two attributed maps and reported two live
		// setters as unwired: both are called from cmd/opskeeper/toolwiring.go,
		// and both calls are exactly the shape this walk cannot attribute.
		//
		// **A rule that only sees the references it can name has a blind spot
		// shaped like the most common call in the tree**, and it cries wolf on
		// the first well-wired method it meets. Attribution is the right
		// question when deciding whether a symbol is *used*; it is the wrong
		// question when deciding whether a file *mentions* it, which is all
		// this rule asks.
		for name := range rec.unattributed {
			namedByAssembly[name] = true
		}
	}

	var out []claimVerdict
	for _, rec := range records {
		if rec.test {
			continue
		}
		// This file is exempt, and the reason is not politeness: it quotes the
		// false claim it was written to catch, in the paragraph explaining why
		// the phrase list missed it. A checker that reads its own source would
		// otherwise report itself forever, and the fix for that is never to
		// weaken the rule — it is to exempt the one file that talks about the
		// rule in the rule's own vocabulary.
		if isThisChecker(rec.path) {
			continue
		}
		for _, d := range rec.decls {
			if d.doc == "" {
				continue
			}
			for _, sentence := range splitSentences(d.doc) {
				subjects, ok := claimSubjects(sentence, declared)
				if !ok {
					continue
				}
				corroborated := false
				var uncorroborated []string
				for _, name := range subjects {
					if namedByAssembly[name] {
						corroborated = true
						break
					}
					uncorroborated = append(uncorroborated, name)
				}
				if corroborated {
					continue
				}
				out = append(out, claimVerdict{
					path: rec.path, line: d.pos.Line, name: d.name, doc: d.doc,
					why: fmt.Sprintf("the comment says %s is wired from the assembly root, "+
						"and no non-test file under cmd/ names it", strings.Join(uncorroborated, ", ")),
				})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].line < out[j].line
	})
	return out
}

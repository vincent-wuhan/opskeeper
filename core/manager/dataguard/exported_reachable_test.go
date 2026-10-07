package dataguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
)

// 这个包里的每个导出符号都必须有一个**生产调用方**。
//
// 这条守卫是冲着一份具体的实现写的：`RaisedClass`（决策 368 删掉的那个）
// 写着「It is the one function callers should use」，然后一个生产调用方都没有，
// 只有它自己的四条测试在调。它从包内读像"已实现"，从包外读像不存在——
// 而一个只有自己测试证明正确的东西，是这个仓库反复记录的失效形态。
//
// 守卫住在它要看的东西旁边，而不是放在 scripts/ 下。理由与决策 327 写在
// audit-port-check 里的一样：**有人改那个东西的时候，会不会同时看见闸门。**
//
// 它只在**本包**范围内成立。本包之外当然也有只被测试引用的导出符号——
// 那是别的包的账，由 deadcode 工具报、ratchet 闸门挡增长。
func TestEveryExportedSymbolHereHasAProductionCaller(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := repoRootOf(t, dir)

	decls := exportedDecls(t, dir)
	if len(decls) == 0 {
		t.Fatal("no exported symbol was read out of this package, so the check is " +
			"inspecting nothing; the parser has probably stopped matching")
	}
	for name, file := range decls {
		if isGateVocabulary(name) {
			continue
		}
		if !referencedOutsideTests(t, repoRoot, name) {
			t.Errorf("%s declares %s, and nothing outside a _test.go file mentions it.\n"+
				"  A symbol only its own tests reach is not a feature: it reads as "+
				"\"implemented\" from inside the package and as absent from outside it, and "+
				"its doc comment gets to say whatever it likes about who should call it.\n"+
				"  Either wire it into production or delete it — and if it was meant to be the "+
				"one callers use, say so in the same commit that makes it true.",
				filepath.Base(file), name)
		}
	}
}

// exportedDecls reads every top-level exported declaration in the package's
// non-test files. Methods are excluded on purpose: a method is reached through
// its receiver, and a receiver type with a live caller drags its methods along
// whether or not each one is spelled out anywhere.
func exportedDecls(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				switch decl := d.(type) {
				case *ast.FuncDecl:
					if decl.Recv != nil {
						continue
					}
					if decl.Name.IsExported() {
						out[decl.Name.Name] = f.Name.Name
					}
				case *ast.GenDecl:
					for _, spec := range decl.Specs {
						switch sp := spec.(type) {
						case *ast.TypeSpec:
							if sp.Name.IsExported() {
								out[sp.Name.Name] = f.Name.Name
							}
						case *ast.ValueSpec:
							for _, n := range sp.Names {
								if n.IsExported() {
									out[n.Name] = f.Name.Name
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

// referencedOutsideTests parses every non-test Go file in the repository and
// collects the identifiers it actually uses.
//
// The first version of this was a textual search, and it **survived its own
// mutation**: the doc comment left behind when RaisedClass was deleted still
// spelled its name, so putting the function back did not make the guard go red.
// A doc comment that explains why something was removed is exactly the kind of
// sentence a text search cannot tell from code — and a guard that a comment can
// switch off is not a guard. Parsing is slower and buys the only property that
// matters here: a name counts only where the compiler would see it.
func referencedOutsideTests(t *testing.T, repoRoot, name string) bool {
	t.Helper()
	fset := token.NewFileSet()
	found := false
	walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "dist", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, pErr := parser.ParseFile(fset, path, nil, 0)
		if pErr != nil {
			// A file this parser cannot read is a file this guard cannot
			// judge. Saying so is better than quietly not looking.
			return fmt.Errorf("parse %s: %w", path, pErr)
		}
		// The declaration itself is not a reference. Without this the guard
		// passes for every symbol it is supposed to police: declaring a
		// function puts its name in an ast.Ident, and a name that appears in
		// its own declaration has, by this rule, a production caller — which
		// is precisely the thing being checked. **A check that cannot fail on
		// the thing it checks is an assertion wearing a guard's clothes.**
		declared := declaredNamePositions(file)
		ast.Inspect(file, func(n ast.Node) bool {
			if found {
				return false
			}
			ident, ok := n.(*ast.Ident)
			if !ok || ident.Name != name {
				return true
			}
			if _, isDecl := declared[ident.Pos()]; isDecl {
				return true
			}
			found = true
			return false
		})
		return nil
	})
	if walkErr != nil {
		// A file this guard cannot read is a file it cannot judge. Swallowing
		// it would make an unreadable tree look like a clean one.
		t.Fatalf("walking %s for %q: %v", repoRoot, name, walkErr)
	}
	return found
}

// repoRootOf hands the walk to core/floor/reporoot rather than re-implementing it.
//
// The first version of this guard walked up looking for **go.work**, and
// `make module-check` rejected it in one line: go.work is gitignored, so a clean
// clone does not have one, and a test that hunts for it either fails or skips
// itself green there. A guard that only ever runs on a developer's machine is
// not the guard the same file claims to be. reporoot walks by tracked markers,
// which is the same reason `scripts/deadcode` uses ../domaincheck rather than
// go.work (decision 364).
func repoRootOf(t *testing.T, dir string) string {
	t.Helper()
	root, ok := reporoot.Find(dir, 8)
	if !ok {
		t.Fatalf("could not find the repository root from %s, so this guard is about "+
			"to pass by inspecting nothing", dir)
	}
	return root
}

// declaredNamePositions is the set of identifier positions that belong to a
// declaration's own name, so a caller can tell "somebody used this" from
// "somebody wrote this down".
//
// Without it the guard passes for every symbol it is supposed to police:
// declaring a function puts its name in an ast.Ident, so by this rule a symbol
// that exists and nothing else would count as having a production caller.
// **A check that cannot fail on the thing it checks is an assertion wearing a
// guard's clothes.**
func declaredNamePositions(file *ast.File) map[token.Pos]bool {
	out := map[token.Pos]bool{}
	add := func(n *ast.Ident) {
		if n != nil {
			out[n.Pos()] = true
		}
	}
	for _, d := range file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			add(decl.Name)
			if decl.Recv != nil {
				for _, p := range decl.Recv.List {
					for _, n := range p.Names {
						add(n)
					}
				}
			}
			if decl.Type.Params != nil {
				for _, p := range decl.Type.Params.List {
					for _, n := range p.Names {
						add(n)
					}
				}
			}
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					add(sp.Name)
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						add(n)
					}
				}
			}
		}
	}
	return out
}

// gateVocabulary is the closed list of symbols this package exports for the
// sake of the gate that guards the registry in enforcement.go, and for nothing
// else. Two entries, and the reason they need naming rather than deleting is
// specific: `compliance-claims-check` lives in cmd/opskeeper and reads this
// registry's vocabulary — StatusInert to compare a row against, DeclaredControls
// to know which control names the tree has to classify. Delete either and the
// gate stops guarding the thing it exists to guard.
//
// The narrow alternative — exempting "anything a _test.go file in another
// package mentions" — was rejected because it is not a narrower version of this
// rule, it is a different and much weaker one: NewRedactorForSensitivity was
// reachable from two tests in biz/report and was still not a feature, and the
// first version of this guard reported exactly that. **A test in another package
// is a consumer, and a consumer that only ever consumes tests is still a
// consumer of tests.**
var gateVocabulary = map[string]string{
	"StatusInert":      "cmd/opskeeper/complianceclaims_test.go",
	"DeclaredControls": "cmd/opskeeper/complianceclaims_test.go",
}

func isGateVocabulary(name string) bool {
	_, ok := gateVocabulary[name]
	return ok
}

// TestTheGateVocabularyExemptionIsStillTrue 是那份名单自己的守卫。
//
// 一张豁免表如果不检查它所豁免的东西还在被使用，它就是一张永久有效的白名单——
// 而白名单的失效方向永远是**多**豁免：闸门改了名字、换了位置或者干脆不再读
// 这两个符号，这两行会安静地继续替它们挡着真正的回归。
//
// 所以这一条断言的是名单指向的那个文件**仍然真的在引用那个符号**。删掉闸门里
// 的一次引用，这里就红，然后要么改名单要么改回去。
func TestTheGateVocabularyExemptionIsStillTrue(t *testing.T) {
	root := repoRootOf(t, mustGetwd(t))
	if len(gateVocabulary) == 0 {
		t.Fatal("gateVocabulary is empty; either the gate stopped reading this package's " +
			"vocabulary or the list was emptied without looking")
	}
	for name, rel := range gateVocabulary {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, rel)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("gateVocabulary names %s for %s, which does not exist: %v\n"+
					"  The exemption is now covering a symbol with no justification behind it.",
					rel, name, err)
			}
			if !referencedOutsideTestsInFile(t, path, name) {
				t.Errorf("%s no longer mentions %s, so exempting it from "+
					"TestEveryExportedSymbolHereHasAProductionCaller covers nothing.\n"+
					"  Drop the entry, or point it at wherever the gate reads that symbol now.",
					rel, name)
			}
		})
	}
}

// referencedOutsideTestsInFile is referencedOutsideTests restricted to one file,
// which is what makes an exemption checkable: "you are exempt because that file
// uses you" is only a statement about the tree while somebody re-reads it.
func referencedOutsideTestsInFile(t *testing.T, path, name string) bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	declared := declaredNamePositions(file)
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		ident, ok := n.(*ast.Ident)
		if !ok || ident.Name != name {
			return true
		}
		if _, isDecl := declared[ident.Pos()]; isDecl {
			return true
		}
		found = true
		return false
	})
	return found
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Command deadpkg reports packages that nothing imports.
//
// scripts/deadcode answers "which symbols inside a package are unreachable",
// at file granularity, and it is the tool stage 3 reaches for when it asks
// how much of the manager can go. It cannot answer the coarser question, and
// the coarser question is the one that decides whether a deletion is a
// one-file edit or a whole-directory one: if a package has no importers, the
// whole directory can go, and no file inside it has to be read to know that.
//
// That question was open because the only evidence anyone had was a count
// written down in the ledger with no way to re-derive it. This is that way.
// It is in the repository so the number can be re-taken instead of
// remembered.
//
// Three tiers, kept apart because they call for different decisions:
//
//	unreferenced  no file anywhere imports it and it has no tests of its own.
//	              The whole directory is a candidate, subject to the
//	              limitations below.
//	suite         no file imports it, but its own tests do. The production
//	              build does not contain it, and deleting it would delete a
//	              check rather than delete weight — core/pig/pigcontract is
//	              in the tree precisely to pin a shape, and `go test` is what
//	              runs it.
//	test-only     production files do not import it; _test.go files do.
//	              Somebody wrote down what it was for, which is a question
//	              about intent rather than about volume.
//
// A directory with func main is an entry point and is reported separately.
// Nothing is supposed to import a binary, and counting them as unreferenced
// buries the real answer under every command and tool in the repository —
// which is exactly what the first version of this walk did, 24 times.
//
// It reports and exits 0, for the same reason deadcode does: the limitations
// below are real, and a gate built on an unsound walk teaches people to add
// escape hatches.
//
// Usage:
//
//	go run ./scripts/deadpkg [dir ...]
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/scripts/internal/modpath"
)

// limitations is the honest list of what an import-graph walk cannot see. It
// is printed with every run so nobody treats the number as a proof.
//
//  1. Reflection and string-keyed lookup: a registry that resolves a type by
//     name reaches a package without naming it in an import.
//  2. go:generate and code generators: a generated file's inputs live in a
//     package nothing imports.
//  3. Build tags. A file excluded by the current GOOS/GOARCH is still parsed,
//     so its imports count. That errs toward reporting less.
//  4. Plugins and manifests. A plugin extension directory is loaded by path
//     from a YAML manifest, never by a Go import. Every
//     plugins/pig-ops/**/extensions/** package in this repository is in that
//     category, so it is reported as unreferenced and is not a candidate.
//  5. Fixtures and tooling directories read by path rather than imported.
const limitations = `reflection · go:generate · build tags · manifests/plugins · fixture directories`

// pkg is one package directory the walk saw.
type pkg struct {
	dir     string
	impPath string
	files   int // non-test files
	lines   int // non-test lines
	// testFiles counts _test.go files. Its only use is the suite tier.
	testFiles int
	isMain    bool
	// importedProd and importedTest are the directories that import this
	// package, split by whether the importing file was a test. The split is
	// per edge, not per importing directory: a directory with one production
	// file and three tests still reaches production.
	importedProd map[string]bool
	importedTest map[string]bool
}

type result struct {
	pkgs         []*pkg
	entryPoints  []*pkg
	unreferenced []*pkg
	suites       []*pkg
	testOnly     []*pkg
}

func (r *result) lines(of []*pkg) int {
	n := 0
	for _, p := range of {
		n += p.lines
	}
	return n
}

func main() {
	dirs := os.Args[1:]
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	pkgs, err := scan(dirs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deadpkg:", err)
		os.Exit(2)
	}
	analyse(pkgs).print(os.Stdout)
	// Always 0. See the package comment.
	os.Exit(0)
}

func scan(dirs []string) ([]*pkg, error) {
	byDir := map[string]*pkg{}
	// import path -> importing directory -> "every import of it from that
	// directory so far came from a test file".
	importers := map[string]map[string]bool{}
	seen := map[string]bool{}
	cache := map[string]modpath.Answer{}

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

			dirSlash := filepath.ToSlash(filepath.Dir(abs))
			isTest := strings.HasSuffix(path, "_test.go")
			p := byDir[dirSlash]
			if p == nil {
				ans, ok := cache[dirSlash]
				if !ok {
					ans = modpath.Of(filepath.Dir(abs))
					cache[dirSlash] = ans
				}
				p = &pkg{dir: dirSlash, importedProd: map[string]bool{}, importedTest: map[string]bool{}}
				if ans.OK {
					p.impPath = importPathOf(ans, filepath.Dir(abs))
				}
				byDir[dirSlash] = p
			}

			fset := token.NewFileSet()
			src, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return fmt.Errorf("%s: %w", path, perr)
			}
			if isTest {
				p.testFiles++
			} else {
				p.files++
				p.lines += fset.Position(src.End()).Line
			}
			// Imports are collected from test files too. That is what makes
			// the test-only tier possible at all.
			ast.Inspect(src, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.FuncDecl:
					if node.Recv == nil && node.Name.Name == "main" {
						p.isMain = true
					}
				case *ast.ImportSpec:
					if node.Path == nil {
						return true
					}
					// Blank and dot imports count: a blank import is still an
					// edge the package cannot survive losing, and a dot import
					// makes every exported name reachable from here.
					ipath := strings.Trim(node.Path.Value, `"`)
					if importers[ipath] == nil {
						importers[ipath] = map[string]bool{}
					}
					if _, seenBefore := importers[ipath][dirSlash]; seenBefore {
						if !isTest {
							importers[ipath][dirSlash] = false
						}
						return true
					}
					importers[ipath][dirSlash] = isTest
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	out := make([]*pkg, 0, len(byDir))
	for _, p := range byDir {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].lines != out[j].lines {
			return out[i].lines > out[j].lines
		}
		return out[i].dir < out[j].dir
	})

	for ipath, dirs := range importers {
		for d, fromTest := range dirs {
			for _, p := range out {
				if p.impPath != ipath {
					continue
				}
				if fromTest {
					p.importedTest[d] = true
				} else {
					p.importedProd[d] = true
				}
			}
		}
	}
	return out, nil
}

// importPathOf turns a directory into the import path that names it.
func importPathOf(ans modpath.Answer, dir string) string {
	rel, err := filepath.Rel(ans.Root, dir)
	if err != nil || rel == "." {
		return ans.Path
	}
	return ans.Path + "/" + filepath.ToSlash(rel)
}

func analyse(pkgs []*pkg) *result {
	res := &result{}
	for _, p := range pkgs {
		if p.files == 0 {
			// A directory with only tests is not production code; it has no
			// line count to contribute to "how much can go".
			continue
		}
		res.pkgs = append(res.pkgs, p)
		if p.isMain {
			res.entryPoints = append(res.entryPoints, p)
			continue
		}
		prod, test := 0, 0
		for d := range p.importedProd {
			if d != p.dir {
				prod++
			}
		}
		for d := range p.importedTest {
			if d != p.dir && !p.importedProd[d] {
				test++
			}
		}
		switch {
		case prod > 0:
			// Reached from production. Nothing to report.
		case test > 0:
			res.testOnly = append(res.testOnly, p)
		case p.testFiles > 0:
			res.suites = append(res.suites, p)
		default:
			res.unreferenced = append(res.unreferenced, p)
		}
	}
	return res
}

func (r *result) print(w *os.File) {
	fmt.Fprintf(w, "deadpkg: %d packages with production files (%d are entry points, which nothing is supposed to import)\n",
		len(r.pkgs), len(r.entryPoints))
	fmt.Fprintf(w, "deadpkg: deletion candidates: %d packages / %d lines; standalone suites: %d / %d lines; test-only importers: %d / %d lines\n",
		len(r.unreferenced), r.lines(r.unreferenced),
		len(r.suites), r.lines(r.suites),
		len(r.testOnly), r.lines(r.testOnly))
	for _, group := range []struct {
		kind  string
		items []*pkg
	}{
		{"unreferenced", r.unreferenced},
		{"suite", r.suites},
		{"test-only", r.testOnly},
	} {
		for _, p := range group.items {
			ipath := p.impPath
			if ipath == "" {
				ipath = "(no module — import path unknown)"
			}
			fmt.Fprintf(w, "  %-13s %6d lines %3d files  %-58s %s\n",
				group.kind, p.lines, p.files, p.dir, ipath)
		}
	}
	fmt.Fprintf(w, "deadpkg: this walk cannot see %s\n", limitations)
	fmt.Fprintln(w, "deadpkg: report only, exit 0 — see the package comment before turning this into a gate")
}

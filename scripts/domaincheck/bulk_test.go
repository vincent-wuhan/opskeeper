package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These tests exist because the first version of the shape counter was
// aimed at the wrong thing and nobody noticed for a whole cut.
//
// Decision 254 measured two consumers of one package — marketplace and
// pluginimport, both off chatruntime — and found the same selected symbols
// and completely different work: one read four scalar fields off a small
// struct, the other walked a parsed object graph. The obvious fix is to count
// how often each symbol is used, and that is what `refs` does. Applied to
// marketplace it reports 7 uses, which reads like the cheapest edge in the
// tree. It is not cheap at all, and the reason is that the work is inside
// `res.Skills` and `sk.Metadata.Requires` — field selections on a value, not
// selections of a package-qualified name, so no reference counter can see it.
//
// So the counter moved down a level: how big is the thing behind the name.
// These tests pin that measurement, and the last one pins the pair it was
// built for, so that a change to either the tool or the tree has to be argued
// rather than discovered.

// bulkOf parses one struct declaration and returns what the tool measures, so
// the arithmetic can be tested without building a tree around it.
func bulkOf(t *testing.T, src string) structBulk {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found *ast.TypeSpec
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range gd.Specs {
			if ts, ok := s.(*ast.TypeSpec); ok && ts.Name.Name == "T" {
				found = ts
			}
		}
	}
	if found == nil {
		t.Fatal("no type T in the fixture")
	}
	return measureBulk(found)
}

// A struct of four scalars is the cheap end of the scale, and it is the shape
// decision 254's systemhealth side actually has: `Caller` is two strings.
func TestBulkCountsScalarsAndContainers(t *testing.T) {
	got := bulkOf(t, `type T struct {
		UserID uint64
		Role   string
		Name   string
		Skills []string
	}`)
	if got.fields != 4 {
		t.Errorf("fields = %d, want 4", got.fields)
	}
	if got.containers != 1 {
		t.Errorf("containers = %d, want 1 — only the slice reaches past its own type", got.containers)
	}
	if got.nested != 0 {
		t.Errorf("nested = %d, want 0 — none of these is a struct declared here", got.nested)
	}
}

// The pair this tool was built to separate. Same field count, opposite
// meaning: `LoadResult`'s four fields are four containers and it carries no
// scalar at all, so selecting it means carrying two parsed trees.
func TestBulkSeparatesAllScalarFromAllContainer(t *testing.T) {
	thin := bulkOf(t, `type T struct {
		Kind string
		ID   string
		Name string
		Ver  string
	}`)
	fat := bulkOf(t, `type T struct {
		Pack     *Pack
		Skills   []Skill
		Agents   []Agent
		Warnings []LoadWarning
	}`)
	if thin.fields != fat.fields {
		t.Fatalf("the fixture is not a fair comparison: %d vs %d fields", thin.fields, fat.fields)
	}
	if thin.containers != 0 {
		t.Errorf("the thin fixture has %d containers, so it is not the scalar-only case", thin.containers)
	}
	if fat.containers != 4 {
		t.Errorf("containers = %d, want 4 — a pointer counts, because Pack drags a "+
			"second type across the boundary exactly as a slice does", fat.containers)
	}
}

// A pointer is a container. This is the one judgement call in measureBulk
// that could reasonably have gone the other way, and decision 254's
// `LoadResult` is the case that settles it: with `Pack *Pack` counted as a
// scalar, the type reads as one wide row and the edge looks like a table copy.
func TestBulkCountsAPointerAsAContainer(t *testing.T) {
	got := bulkOf(t, `type T struct { P *Pack }`)
	if got.containers != 1 {
		t.Errorf("containers = %d, want 1 for a lone pointer field", got.containers)
	}
	if got.nested != 0 {
		t.Errorf("nested = %d — Pack is qualified from another file, and the tool "+
			"deliberately does not resolve cross-file type identity", got.nested)
	}
}

// An embedded field is one dependency wearing no name. Counting it per
// promoted field would let a single embed outweigh a dozen explicit ones,
// which is how a `time.Time` or a base struct would come to dominate a report
// about coupling.
func TestBulkCountsAnEmbeddedFieldOnce(t *testing.T) {
	got := bulkOf(t, `type T struct {
		Base
		Name string
	}`)
	if got.fields != 2 {
		t.Errorf("fields = %d, want 2 — the embed is one field, not one per promoted name", got.fields)
	}
	if got.containers != 0 {
		t.Errorf("containers = %d, want 0 — Base is a bare identifier here", got.containers)
	}
}

// Non-structs carry no shape, and that has to be the zero value rather than a
// guess: an edge selects functions and constants all the time, and reporting
// a shape for them would be inventing one.
func TestBulkIsTheZeroValueForANonStruct(t *testing.T) {
	for _, src := range []string{`type T int`, `type T string`, `type T = int`} {
		if got := bulkOf(t, src); got != (structBulk{}) {
			t.Errorf("%s measured %+v, want the zero value", src, got)
		}
	}
}

var shapeLineRE = regexp.MustCompile(`(?m)^\s+shape: (.*)$`)

// bulkShapeOf runs the seam report over a written tree and hands back the shape
// line for one edge, so the tests below talk about the report a reader reads
// rather than about the maps underneath it.
func bulkShapeOf(t *testing.T, root, edgeName string) string {
	t.Helper()
	sources, _, err := parseTree(root, managerPrefix, rules{})
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	var sb strings.Builder
	printSeams(&sb, sources, defaultRules())
	lines := strings.Split(sb.String(), "\n")
	for i, line := range lines {
		if !strings.Contains(line, edgeName) || !strings.Contains(line, "->") {
			continue
		}
		for _, follow := range lines[i+1:] {
			if m := shapeLineRE.FindStringSubmatch(follow); m != nil {
				return m[1]
			}
			if !strings.HasPrefix(follow, "                 ") {
				break
			}
		}
	}
	t.Fatalf("no shape line under %s in:\n%s", edgeName, sb.String())
	return ""
}

// The end-to-end case, and the one that matters: a consumer selecting a fat
// type and a consumer selecting a thin one from the same producer, reported
// the same way, are told apart by the shape line. Before this counter the
// report had one number for both and they were equally unrankable.
func TestTheShapeLineSeparatesTwoConsumersOfOnePackage(t *testing.T) {
	root := writeTree(t, map[string]string{
		"biz/prod/model.go": `package prod

type Wide struct {
	Pack     *Pack
	Skills   []Skill
	Agents   []Agent
	Warnings []LoadWarning
}

type Narrow struct {
	ID   string
	Name string
}

type Pack struct{ ID string }
type Skill struct{ Name string }
type Agent struct{ Name string }
type LoadWarning struct{ Path string }
`,
		"biz/fat/use.go": `package fat

import "github.com/vincent-wuhan/opskeeper/core/manager/biz/prod"

func Use(w *prod.Wide) int { return len(w.Skills) }
`,
		"biz/thin/use.go": `package thin

import "github.com/vincent-wuhan/opskeeper/core/manager/biz/prod"

func Use(n prod.Narrow) string { return n.ID }
`,
	})

	fat := bulkShapeOf(t, root, "fat")
	thin := bulkShapeOf(t, root, "thin")
	if !strings.Contains(fat, "Wide(4f 4c") {
		t.Errorf("the consumer of the wide type reads %q, want Wide reported as 4 fields "+
			"and 4 containers", fat)
	}
	if !strings.Contains(thin, "Narrow(2f 0c") {
		t.Errorf("the consumer of the narrow type reads %q, want Narrow reported as 2 fields "+
			"and 0 containers", thin)
	}
}

// The report must not leak the producer's whole exported surface. An earlier
// draft looked the shapes up from the consumer's own file, which meant it
// printed every struct the imported package declares — thirty-one of them for
// chatruntime — and the one number that mattered was buried in the middle.
// A fact about the package is not a fact about the edge.
func TestTheShapeLineNamesOnlyWhatTheConsumerSelected(t *testing.T) {
	root := writeTree(t, map[string]string{
		"biz/prod/model.go": `package prod

type Selected struct{ ID string }
type Unrelated struct {
	A []string
	B []string
	C []string
}
`,
		"biz/consumer/use.go": `package consumer

import "github.com/vincent-wuhan/opskeeper/core/manager/biz/prod"

func Use(s prod.Selected) string { return s.ID }
`,
	})
	got := bulkShapeOf(t, root, "consumer")
	if strings.Contains(got, "Unrelated") {
		t.Errorf("the shape line names a type the consumer never selected: %q", got)
	}
	if !strings.Contains(got, "Selected") {
		t.Errorf("the shape line is %q, want the selected type named", got)
	}
}

// The measurement is pinned to the tree it was built for, and the thing it
// measures moved.
//
// Decision 254's argument was that `marketplace -> aiops` is not the cheap
// edge its import count makes it, and the reason is that LoadResult has no
// scalar in it: four fields, four containers, two of them parsed trees.
// Decision 271 cut that edge — the marketplace now reads the container loader
// directly, and `container` is a shared domain, so the seam report has no row
// to read the shape off any more. The measurement is worth more now than it
// was then: LoadResult is what a plugin author's build pulls in, so its size
// is the price of `go get`-ing the loader.
//
// So the test stopped reading a seam report and started reading the
// declaration. Same number, measured at the place it now lives.
func TestTheShapeOfLoadResultIsStillTheReasonItIsNotAOneFieldResult(t *testing.T) {
	got := measureLoadResult(t, filepath.Join("..", "..", "core", "extension", "biz", "container"))
	want := "4f 4c 0n"
	if got != want {
		t.Errorf("LoadResult is now %q, was %q. Decision 254 priced the plugin result "+
			"shape on the strength of that number and decision 271 moved the declaration "+
			"to core/extension; if it was narrowed on purpose, say so here rather than "+
			"letting the next reader assume the old reading still holds", got, want)
	}
}

// measureLoadResult finds the LoadResult declaration anywhere in dir and
// formats it the way the seam report does, so the number this test pins is
// the same number the report used to print.
func measureLoadResult(t *testing.T, dir string) string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name.Name != "LoadResult" {
						continue
					}
					b := measureBulk(ts)
					return fmt.Sprintf("%df %dc %dn", b.fields, b.containers, b.nested)
				}
			}
		}
	}
	t.Fatalf("no LoadResult declaration under %s", dir)
	return ""
}

func grepLines(s, needle string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

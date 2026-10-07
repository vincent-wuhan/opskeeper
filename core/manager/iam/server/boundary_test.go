package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The prefixes this bounded context is allowed to reach. Anything under
// managerPrefix that is neither of these is a package above iam in the same
// module, and importing one is the shape of the dependency decision 109
// removed.
const (
	managerPrefix = "github.com/vincent-wuhan/opskeeper/core/manager/"
	// basePrefix is the control plane's shared infrastructure, a module of
	// its own since decision 221. The audit port used to be manager/pkg/audit
	// and is now base/pkg/audit, and the difference is not cosmetic: a test
	// that kept resolving the port under managerPrefix would stop finding it,
	// and a boundary check that only recognised managerPrefix would skip
	// every base import without saying so. A gate that quietly stops looking
	// is worse than one that is missing, because it still reports green.
	basePrefix = "github.com/vincent-wuhan/opskeeper/core/base/"
	// domainsPrefix is the release floor, a module of its own since decision
	// 222. It is deliberately NOT a shared tree and does not belong in
	// sharedTrees below: it is a set of bounded contexts, so iam reaching into
	// it is the very thing this test exists to refuse. It is named here only
	// so such an import is *seen*. Left out of the prefix list below, the
	// check would skip it silently and keep reporting green — the third time
	// this file has been taught about a module by a move instead of a review.
	domainsPrefix = "github.com/vincent-wuhan/opskeeper/core/domains/"
	iamPrefix     = managerPrefix + "iam/"
	pkgPrefix     = basePrefix + "pkg/"
	auditPortPath = pkgPrefix + "audit"
)

// sharedTrees are the parts of the manager module that are not bounded
// contexts: the architecture rules give them mayDependOn that names no BC,
// which is what makes them safe to depend on from a leaf. iam_biz reaches
// dataguard for field-level sensitivity classification, and that edge
// predates this decision and is a legitimate one.
//
// This list is deliberately short and hand-written. The authority is
// .go-arch-lint.yml — TestTheArchitectureRulesGrantThisContextNothingAboveIt
// below reads the real grants and fails if iam is given anything else, so
// adding a grant without updating the rule here leaves one of the two red.
var sharedTrees = []string{
	pkgPrefix,
	basePrefix + "pkg",          // errors, tenant context, and now the audit port
	managerPrefix + "dataguard", // field sensitivity, shared by authz
}

// retiredEdges are the three imports iam's handlers used to carry. They are
// named here so a regression says which edge came back, rather than just
// "something outside the context".
var retiredEdges = map[string]string{
	managerPrefix + "biz/audit":         "the audit row type; the shape now comes from pkg/audit and the writer stays in biz/audit",
	managerPrefix + "model/audit":       "the action vocabulary; now re-exported from pkg/audit",
	managerPrefix + "server/middleware": "SetAuditEvent; now pkg/audit.SetAuditEvent",
}

// contextRoot is the iam bounded context, relative to this package. It is
// one level up, not two: this package is iam/server, so ".." is the context
// root and "../.." would be the whole manager module.
const contextRoot = ".."

// parsedFile is one source file of the context, with the path kept for
// error messages.
type parsedFile struct {
	path string
	file *ast.File
}

// parseContext walks the whole bounded context. parser.ParseDir is not
// recursive and the context is four directories deep, so a walk is what
// makes "this context" mean the context rather than its top folder.
func parseContext(t *testing.T) []parsedFile {
	t.Helper()
	fset := token.NewFileSet()
	var out []parsedFile
	err := filepath.WalkDir(contextRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		out = append(out, parsedFile{path: path, file: file})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the iam context: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no source files were parsed; the walk is broken, not the boundary")
	}
	return out
}

// TestThisContextReachesNothingAboveItself is the acceptance test for
// decision 109.
//
// The audit chain is deliberately a single throat in manager's biz layer
// (decision 35), and that is the right shape for the writer. What it should
// not have done is drag every reader up with it: iam's handlers had to name
// a row, the row's shape sat three layers above them, and the only reason
// the resulting edge was not a cycle is that none of those three packages
// happened to import iam. A later move that adds such an import turns a
// coincidence into a cycle — a build failure in the least confusing place, a
// tangle in the most confusing one.
//
// The permitted shape is narrow on purpose: this context, plus the shared
// pkg/ tree, which the architecture rules forbid from depending on any
// bounded context. That is the whole reason the audit port could move there
// without dragging the ledger along.
func TestThisContextReachesNothingAboveItself(t *testing.T) {
	files := parseContext(t)
	for _, pf := range files {
		for _, imp := range pf.file.Imports {
			target, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Errorf("%s: unquote import: %v", pf.path, err)
				continue
			}
			if !strings.HasPrefix(target, managerPrefix) &&
				!strings.HasPrefix(target, basePrefix) &&
				!strings.HasPrefix(target, domainsPrefix) {
				continue // stdlib, a third party, or another module
			}
			if strings.HasPrefix(target, iamPrefix) {
				continue
			}
			if isSharedTree(target) {
				continue
			}
			if why, retired := retiredEdges[target]; retired {
				t.Errorf("%s imports %s — %s. Name the row through pkg/audit instead: "+
					"the throat does not move, only the shape does", pf.path, target, why)
				continue
			}
			t.Errorf("%s imports %s, which is neither this context nor a BC-free shared tree; "+
				"iam is a leaf bounded context and must not reach into the layers above it",
				pf.path, target)
		}
	}
}

// TestEveryAuditRowThisContextEmitsIsNamedThroughThePort is the positive
// half of the same claim, and the test that keeps the port honest rather
// than decorative.
//
// Pointing at a port and then spelling the action inline would satisfy the
// boundary above while quietly giving up what the port is for: a closed,
// greppable vocabulary. A string literal compiles, reaches the database,
// and is invisible to the hygiene tests in pkg/audit — which is how an
// audit trail ends up with two spellings of one action and a filter
// dropdown that lies.
func TestEveryAuditRowThisContextEmitsIsNamedThroughThePort(t *testing.T) {
	rows := 0
	for _, pf := range parseContext(t) {
		alias := portAlias(pf.file)
		if alias == "" {
			continue
		}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isEventType(lit.Type, alias) {
				return true
			}
			rows++
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "Action", "ResourceType", "Status":
					if _, isSelector := kv.Value.(*ast.SelectorExpr); !isSelector {
						t.Errorf("%s: Event.%s is spelled inline (%s); name it through %s.%s "+
							"so the vocabulary stays closed and greppable",
							pf.path, key.Name, exprString(kv.Value), alias, key.Name)
					}
				}
			}
			return true
		})
	}
	if rows == 0 {
		t.Fatal("no audit rows were found in this context; the walk is broken, or the handlers " +
			"stopped auditing, which is the other thing this test exists to catch")
	}
}

// isSharedTree reports whether an import lands in one of the BC-free trees
// the context is allowed to reach.
func isSharedTree(target string) bool {
	for _, tree := range sharedTrees {
		trimmed := strings.TrimSuffix(tree, "/")
		if target == trimmed || strings.HasPrefix(target, trimmed+"/") {
			return true
		}
	}
	return false
}

// portAlias returns the name a file refers to the audit port by, or "" if
// the file does not import it.
func portAlias(file *ast.File) string {
	for _, imp := range file.Imports {
		target, err := strconv.Unquote(imp.Path.Value)
		if err != nil || target != auditPortPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "audit" // the package's own name
	}
	return ""
}

// isEventType reports whether a composite literal's type is the port's
// Event, spelled through the file's import alias.
func isEventType(expr ast.Expr, alias string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == alias && sel.Sel.Name == "Event"
}

// exprString renders an expression roughly, for an error message. It is not
// a printer and does not try to be; the point is to show the literal that
// should not be there.
func exprString(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.BasicLit:
		return v.Value
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.Ident:
		return v.Name
	default:
		return "<expression>"
	}
}

// archLint is the shape of .go-arch-lint.yml this test reads. Only the two
// top-level maps matter; the rest of the file is documentation.
type archLint struct {
	Components map[string]struct {
		In any `yaml:"in"`
	} `yaml:"components"`
	Deps map[string]struct {
		MayDependOn []string `yaml:"mayDependOn"`
	} `yaml:"deps"`
}

// TestTheArchitectureRulesGrantThisContextNothingAboveIt tests the place
// the exception actually lived.
//
// The import graph and the arch-lint rule are two different artifacts, and
// the retired edge was permitted by both: the import was in the exceptions
// ledger, and the grant was in mayDependOn. Fixing only the code would have
// left a rule that authorises nothing, which is how a boundary erodes one
// quiet re-grant at a time.
//
// It also covers the half arch-lint's own dead-grant check cannot: that
// check fires when a grant is unused, and says nothing when a grant is used
// again. This one fires on the grant itself.
func TestTheArchitectureRulesGrantThisContextNothingAboveIt(t *testing.T) {
	// The rules file, relative to this package: iam/server -> iam ->
	// manager -> core -> repo root.
	const rulesPath = "../../../../.go-arch-lint.yml"

	raw, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read %s: %v", rulesPath, err)
	}
	var cfg archLint
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", rulesPath, err)
	}
	if len(cfg.Deps) == 0 {
		t.Fatal("no dependency rules were parsed; either the file moved or this test is stale")
	}

	checked := 0
	for component, rule := range cfg.Deps {
		if !strings.HasPrefix(component, "iam_") {
			continue
		}
		checked++
		for _, grant := range rule.MayDependOn {
			if strings.HasPrefix(grant, "manager_") {
				t.Errorf("%s mayDependOn %s, which is a bounded context above iam. The audit "+
					"port in pkg/audit exists so that this grant is unnecessary (decision 109)",
					component, grant)
			}
			if !isSharedOrOwnComponent(grant) {
				t.Errorf("%s mayDependOn %s, which is neither one of iam's own layers nor a "+
					"BC-free shared tree; keep the grant and this test in step", component, grant)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no iam rules were found; the walk is broken, not the grants")
	}
}

// isSharedOrOwnComponent checks a granted component name against the two
// shapes a legitimate grant can take: another layer of this context, or a
// shared tree. It is the name-level twin of isSharedTree.
func isSharedOrOwnComponent(component string) bool {
	if strings.HasPrefix(component, "iam_") {
		return true
	}
	return component == "shared_pkg" || component == "shared_dataguard"
}

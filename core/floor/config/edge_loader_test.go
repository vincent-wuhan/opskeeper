package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// The node process runs on a customer host with root-level tools in it —
// restart_service, a bash sandbox, a webshell — and this file is the
// environment a deployment can hand it. Load reads the whole platform
// configuration, and that configuration includes six model-vendor API keys,
// the admin password, the JWT signing secret and the database DSN.
//
// Until this test existed, the node called Load. The edge env example named
// none of those variables and the compose file does not run a node, so the
// plan's acceptance clause — "no cloud-vendor credential under
// /etc/opskeeper-edge or in the node's process environment" — held for the
// wrong reason: nothing had been set, rather than the node refusing it. A
// single exported variable in a shared profile would have put a vendor key
// in every node's memory, and no test, gate or comment would have said so.
//
// LoadEdge is the fix and this test is the reason it cannot be undone by
// adding one line.

// vendorCredentialPrefixes are the shapes a credential belonging to somebody
// else's model account arrives in. It is a prefix test rather than an exact
// list on purpose: a new provider added to Load tomorrow is exactly the edit
// this must catch, and an exact list would have to be updated in the same
// commit as the leak.
var vendorCredentialPrefixes = []string{
	"OPSKEEPER_OPENAI_",
	"OPSKEEPER_ANTHROPIC_",
	"OPSKEEPER_DEEPSEEK_",
	"OPSKEEPER_DASHSCOPE_",
	"OPSKEEPER_KIMI_",
	"OPSKEEPER_MOONSHOT_",
	"OPSKEEPER_ZHIPU_",
	"OPSKEEPER_VOLCENGINE_",
	"OPSKEEPER_ARK_",
	"OPSKEEPER_GEMINI_",
	"OPSKEEPER_GOOGLE_",
	"OPSKEEPER_QWEN_",
	"OPSKEEPER_QWEN_",
	"ANTHROPIC_",
	"OPENAI_",
	"AZURE_OPENAI_",
	"GEMINI_",
	"GOOGLE_API_",
	"DEEPSEEK_",
}

// edgeEnvPrefix is the only namespace a node is allowed to read.
const edgeEnvPrefix = "OPSKEEPER_EDGE_"

// TestLoadEdgeReadsNothingButTheNodesOwnVariables walks the call graph
// reachable from LoadEdge and checks every environment variable it names.
//
// It follows the delegation to loadEdge rather than reading LoadEdge's own
// body, because the delegation is the obvious way to make this test pass
// while still reading everything: a body with no getEnv call in it says
// nothing about what the function it calls reads. And it fails when the
// closure comes out empty, for the reason the tree guards in
// scripts/domaincheck learned across seven rounds — an assertion over a set
// that turned out to hold nothing reports green for having looked at
// nothing.
func TestLoadEdgeReadsNothingButTheNodesOwnVariables(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "config.go", nil, 0)
	if err != nil {
		t.Fatalf("parse config.go: %v", err)
	}

	funcs := map[string]*ast.FuncDecl{}
	calls := map[string][]string{}
	// reads is keyed by the ENCLOSING function and holds every variable that
	// function reads.
	//
	// The first version of this test keyed it by the getter's name instead,
	// which meant one entry per getEnv-shaped callee and every variable after
	// the first was dropped on the floor. Measured: adding a vendor key read
	// to loadEdge — the exact edit this exists to catch — left the test green.
	// That is the eleventh hole of this shape in this repository, and it was
	// written eleven minutes after the tenth, which is the most useful thing
	// this repository has to say about its own tests.
	reads := map[string][]string{}

	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		funcs[fd.Name.Name] = fd
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			calls[fd.Name.Name] = append(calls[fd.Name.Name], ident.Name)
			if name, ok := envVarName(ident.Name, call.Args); ok {
				reads[fd.Name.Name] = append(reads[fd.Name.Name], name)
			}
			return true
		})
	}

	if _, ok := funcs["LoadEdge"]; !ok {
		t.Fatalf("config has no LoadEdge, so a node has no way to read its own " +
			"configuration without reading everything")
	}

	// Breadth-first over the closure, so a function that only reads a
	// variable four calls deep is found just as surely as one that reads it
	// directly.
	seen := map[string]bool{}
	queue := []string{"LoadEdge"}
	variables := map[string]string{}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		for _, v := range reads[name] {
			variables[v] = name
		}
		queue = append(queue, calls[name]...)
	}

	if len(variables) == 0 {
		t.Fatalf("the closure reachable from LoadEdge names no environment " +
			"variables at all, which means this test is measuring nothing. Either the " +
			"loader moved to another file — in which case point it there and say so — or " +
			"the reads are no longer getEnv-shaped and the check has to be rewritten")
	}

	// Report every offender rather than the first, because the realistic
	// failure is a pasted block of provider defaults, not a single key.
	var offenders []string
	for _, v := range sortedKeys(variables) {
		if strings.HasPrefix(v, edgeEnvPrefix) {
			continue
		}
		for _, p := range vendorCredentialPrefixes {
			if strings.HasPrefix(v, p) {
				offenders = append(offenders, v)
				break
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("a node reads %d variable(s) that are not its own: %s\n\n"+
			"They are read because loadEdge is in LoadEdge's call closure, and this "+
			"process runs with root-level tools on someone else's host. Move the read "+
			"to Load, or give the node its own loader — do not widen edgeEnvPrefix",
			len(offenders), strings.Join(offenders, ", "))
	}
}

// TestLoadEdgeReadsAtLeastTheVariablesTheNodeNeeds is the other half.
//
// The check above is satisfied by a loader that reads nothing, which would
// be a node that cannot be configured at all — so it would pass while being
// broken. Pinning the floor is what stops "delete the reads" from being a
// way to make the security test green.
func TestLoadEdgeReadsAtLeastTheVariablesTheNodeNeeds(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "config.go", nil, 0)
	if err != nil {
		t.Fatalf("parse config.go: %v", err)
	}
	var found []string
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if name, ok := envVarName(ident.Name, call.Args); ok && strings.HasPrefix(name, edgeEnvPrefix) {
				found = append(found, name)
			}
			return true
		})
	}
	for _, required := range []string{
		"OPSKEEPER_EDGE_CLOUD_ADDR",
		"OPSKEEPER_EDGE_ACCESS_KEY",
		"OPSKEEPER_EDGE_SECRET_KEY",
	} {
		if !contains(found, required) {
			t.Errorf("no function in this file reads %s. A node that cannot be told "+
				"where the control plane is, or given its own credential pair, is not a "+
				"configurable node — and deleting reads to satisfy the security test is "+
				"not a fix", required)
		}
	}
}

// envVarName recognises the getEnv family and returns the variable it reads.
func envVarName(fn string, args []ast.Expr) (string, bool) {
	switch fn {
	case "getEnv", "getEnvBool", "getEnvCSV", "getEnvInt", "getEnvFloat", "getEnvDuration":
	default:
		return "", false
	}
	if len(args) == 0 {
		return "", false
	}
	lit, ok := args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	return strings.Trim(lit.Value, `"`), true
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

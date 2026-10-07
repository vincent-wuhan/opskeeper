package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// This file is the second layer. The first layer — a text scan for path-shaped
// string literals — answers "does this string exist in the tree", and that is
// not the question a documented endpoint needs answered. It is a question a
// constant answers, a glob answers, a filepath.Join answers, an error message
// answers, and a config default answers.
//
// That matters here because of how this gate failed the first time. It was
// written to catch two documents describing servers that do not exist, and it
// reported 26 of 26 claims satisfied while doing it — three of its own header
// comment's paths were counted as routes, an unbounded /api prefix matched
// everything, and a "/*/pig-ops.yaml" glob was read as a subtree mount. Every
// one of those was a path-shaped string that had nothing to do with a router.
// A check that a phantom can satisfy is not a weaker version of a check, it
// is a check that trains the reader to add phantoms.
//
// So the question becomes: is this path handed to something that registers
// routes? That is decidable from the syntax tree rather than from the text —
// a comment is not an expression, a test is not production, and a string
// constant is not an argument. No name list of "things that look like routes"
// is involved; the only judgement is whether the call is a method call on a
// router-shaped name, and the rest is the parser's.

// routerMethods is the set of method names this repository registers routes
// through: the HTTP verbs chi and net/http spell, plus the registration and
// mounting methods.
//
// It is a list rather than a dataflow analysis on purpose. Resolving "is this
// value a chi.Router" means tracking every constructor and every struct field
// a router is ever stored in, and a check that complicated is a check whose
// false negatives nobody can see. A method name is a much smaller thing to
// argue about, and the failure it permits — a non-router method with a router's
// name being read as a registration — makes the gate lenient, not strict,
// which is the safe direction to be wrong in for an inventory this gate prints
// but does not match against.
//
// Group is deliberately absent: it takes a prefix, not a route, and recording
// it would put "/api" into the route set.
var routerMethods = map[string]bool{
	// The verbs.
	"Get": true, "Post": true, "Put": true, "Delete": true, "Patch": true,
	"Head": true, "Options": true, "Connect": true, "Trace": true,
	// Registration and mounting.
	"Handle": true, "HandleFunc": true, "Route": true,
	"Mount": true, "MountFunc": true, "Methods": true, "All": true, "Any": true,
}

// registeredRoutes returns every path this tree hands to a router.
//
// It fails rather than skipping a file it cannot parse. A parse error that
// were swallowed would quietly shrink the route set, and a shrunken route set
// makes this gate report delivered endpoints as phantoms — the opposite error
// from the one it exists to catch, and the one that costs a reader their
// trust in the tool.
func registeredRoutes(root string) (map[string]bool, error) {
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			// A test asserting a request is a claim about a handler, not a
			// registration, and letting one count is how a deleted endpoint
			// stays documented for ever.
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !routerMethods[sel.Sel.Name] {
				return true
			}
			// Every router method takes a handler as well as a path. A
			// one-argument call with a router's name is something else that
			// happens to share it.
			if len(call.Args) < 2 {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if path, ok := routeFromPattern(value); ok {
					out[path] = true
				}
			}
			return true
		})
		return nil
	})
	return out, err
}

// routeFromPattern turns one string argument into a route, or reports that it
// is not one.
func routeFromPattern(value string) (string, bool) {
	if strings.HasPrefix(value, "/") {
		return value, true
	}
	// net/http's method-prefixed ServeMux pattern. The verb is spelled the
	// HTTP way here and the Go way in routerMethods, so it gets its own set
	// rather than a case-folding trick that would also fold whatever else
	// happens to share a name with a router method.
	verb, rest, found := strings.Cut(value, " ")
	if !found || !httpVerbs[verb] || !strings.HasPrefix(rest, "/") {
		return "", false
	}
	return rest, true
}

// httpVerbs is how the HTTP methods are spelled inside a path argument.
var httpVerbs = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true,
	"HEAD": true, "OPTIONS": true, "CONNECT": true, "TRACE": true,
}

// skipDir reports whether a directory is not this repository's source.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist":
		return true
	}
	return false
}

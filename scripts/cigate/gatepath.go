package main

// A gate can be wired into CI, defined in the Makefile, and still run
// nothing at all: `go test ./some/package/` where that package moved to
// another module answers "[setup failed]" and exits non-zero, but the
// failure names a path rather than a behaviour, and every reader of a red
// build looks at the test output first.
//
// The four that rotted were all the same drift. The manager module was split
// into core/base and core/domains, the packages moved with it, the tests
// moved with the packages, and the Makefile kept the old module and the old
// package. `audit-port-check` and `promptguard-check` are named gates in
// this file's own tables, ci.yml invokes both, and both had been failing on
// a directory that no longer existed.
//
// Nothing else in this command could have caught it, and the reason is
// written down in main.go: the check answers "is it wired", because running
// the gates is CI's job. That reasoning was sound while a failing gate
// failed on a test. It does not survive a gate whose package path is a
// corpse, because the only thing that goes red is a line no one reads.
//
// So this check looks inside the recipes instead of at the target names. It
// is deliberately narrow in the same way checkBuildTagCoverage is: the
// question per package pattern is not "does this gate test the right thing"
// but "does the directory this recipe names still exist and still hold Go
// files". Anything stronger needs to know what a gate is for, and only the
// table in main.go knows that.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GatePath is one package a CI-reachable recipe claims to test.
type GatePath struct {
	// Target is the make target the recipe lives in, because a bare path in
	// a 900-line Makefile does not say which gate to go and fix.
	Target string
	// Module is the directory the recipe cds into, relative to the root.
	Module string
	// Pattern is the package pattern as the recipe wrote it.
	Pattern string
	// Dir is the literal directory it must resolve to. Empty for a
	// recursive or whole-module pattern, which names no single directory.
	Dir string
}

// gatePaths is every CI-reachable package reference, literal or not.
//
// `commands` counts what was read, which is the honest answer to "did this
// parse the Makefile at all". It is deliberately NOT the number of literal
// directories found: a Makefile whose recipes all say `./...` has nothing to
// check and is perfectly healthy, and the first version of this function
// counted literals instead and called that repository broken.
func gatePaths(makefileSrc string, reachable map[string]bool) (paths []GatePath, commands int) {
	for _, c := range goTestCommands(makefileSrc) {
		if c.Target == "" || !reachable[c.Target] {
			continue
		}
		commands++
		for _, raw := range c.Pkgs {
			paths = append(paths, GatePath{
				Target:  c.Target,
				Module:  c.Module,
				Pattern: raw,
				Dir:     literalDir(raw),
			})
		}
	}
	return paths, commands
}

// checkGatePackagePaths is the entry point wired into check().
func checkGatePackagePaths(root string, makefileSrc string, reachable map[string]bool) error {
	paths, commands := gatePaths(makefileSrc, reachable)
	if commands == 0 {
		// The guard against a reader that has stopped matching. Producing
		// zero problems because zero commands were read is indistinguishable
		// from a clean repository, and the honest report for that is a
		// failure.
		return fmt.Errorf("cigate: read no CI-reachable go test command out of the Makefile, " +
			"so no gate path was checked; the recipe parser has probably stopped matching")
	}

	var dead []string
	deadModules := map[string]bool{}
	checked := map[string]bool{}
	for _, p := range paths {
		modPath := filepath.Join(root, p.Module)
		// The directory the recipe cds into. A recipe that cannot get there
		// runs nothing, and this is the module half of the same rot.
		if p.Module != "." && !deadModules[p.Module] {
			if !withinModule(modPath, root) {
				deadModules[p.Module] = true
				dead = append(dead, fmt.Sprintf("target %q: cd %s -- no such module directory",
					p.Target, p.Module))
				continue
			}
		}
		if p.Dir == "" {
			continue
		}
		key := filepath.Join(p.Module, p.Dir)
		if checked[key] {
			continue
		}
		checked[key] = true
		if !dirHasGoFiles(filepath.Join(modPath, p.Dir)) {
			dead = append(dead, fmt.Sprintf("target %q: cd %s && go test %s -- that package is gone or holds no Go files",
				p.Target, p.Module, p.Pattern))
		}
	}
	if len(dead) == 0 {
		return nil
	}
	sort.Strings(dead)
	return fmt.Errorf("cigate: %d CI-reachable go test package path(s) in the Makefile no longer exist:\n  %s",
		len(dead), strings.Join(dead, "\n  "))
}

// literalDir turns a package pattern into the directory it must resolve to,
// or returns "" when the pattern is not a plain directory reference.
//
// `./...` and `./a/...` are the two shapes that are not: the first names the
// module itself and the second names everything under `./a`. Neither can be
// checked by statting one path, and reading either as one is the mistake the
// author of this file made first — the check reported `core/floor/config` as
// missing when `./...` was what the recipe said, and `core/floor/config` was
// sitting right there.
func literalDir(pattern string) string {
	p := strings.Trim(strings.TrimSpace(pattern), "'\"")
	if !strings.HasPrefix(p, "./") {
		return ""
	}
	p = strings.TrimSuffix(filepath.Clean(p), string(filepath.Separator))
	if p == "." || p == ".." {
		return ""
	}
	if p == "..." || strings.HasSuffix(p, string(filepath.Separator)+"...") {
		return ""
	}
	return p
}

// withinModule reports whether a `cd` anchor is somewhere the toolchain can
// actually build from: an existing directory, with a go.mod in it or in one
// of its ancestors up to the repository root.
//
// Requiring the go.mod to be *in* the anchor was too strict, and it fired on
// the real Makefile the moment a gate got wired into CI for the first time.
// `edge-credential-check` cds into core/floor/config, which is a package
// inside the core/floor module and not a module root of its own; `cd` there
// and `go test ./...` works, and the check reported it as gone. The earlier
// version of this predicate was wrong in the other direction -- it looked for
// .go files, so every nested module in the repository (core/manager holds
// biz/, server/ and model/ but not one .go file of its own, because a module
// root that had source in it would not be a boundary) read as dead. Both
// failures are the same mistake: a cd anchor is a *place*, and the question
// about a place is whether it is still there, not whether it looks like the
// kind of place the author expected.
func withinModule(dir, root string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	for d := dir; ; {
		if fileExists(filepath.Join(d, "go.mod")) {
			return true
		}
		if d == root {
			return false
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// dirHasGoFiles reports whether a directory is a package the toolchain could
// build: a directory that exists but holds no Go file fails `go test` just as
// loudly as one that is gone, and the two produce the same unhelpful message.
func dirHasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

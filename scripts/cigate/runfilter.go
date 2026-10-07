package main

// `go test -run 'X'` that names nothing passes. The toolchain prints
// "testing: warning: no tests to run" and exits zero, so a gate whose whole
// recipe is a -run filter can assert nothing at all and still be green in CI.
//
// The Makefile leans on -run heavily: 23 recipes filter by name rather than
// running a package whole, because a gate that runs forty unrelated tests
// tells a reader nothing when it goes red. That habit is the exposure. The
// filter is the entire assertion, and nothing checked that the assertion had
// a subject.
//
// The parse has to be right or the check is worse than nothing, and getting
// it wrong is easy in a specific way: most of these recipes wrap the filter
// onto the next line, so the `cd core/manager &&` that says which module the
// tests live in is on a DIFFERENT LINE from the filter. A reader that looks
// for the module on the filter's own line resolves every one of them to the
// root module, checks the filter against the root's test names, and reports
// a clean bill of health for a check that examined the wrong directory. The
// first version of this file did exactly that, and its "0 problems" was an
// artifact of the bug.
//
// So the filter is collected in the same pass that already tracks targets and
// modules, and TestEveryRunFilterInTheRealMakefileIsParsed pins that pass
// against a raw count of the flag in the file. A parser that quietly stops
// matching half the recipes fails there rather than passing everything.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// RunFilter is one CI-reachable `go test -run` recipe.
type RunFilter struct {
	Target string
	Module string
	// Alternatives are the `|`-separated patterns as the Makefile wrote them.
	// They are regexes, not names: Go matches them unanchored, so a filter
	// may legitimately be a prefix of a longer test name. An earlier reader
	// of this file compared them for equality and reported two live filters
	// as dead.
	Alternatives []string
	// Pkgs is the package list of the same command, for the message.
	Pkgs []string
}

// runRE captures the quoted filter. The value is either on the command line
// or after a trailing backslash, and the Makefile uses both.
var runRE = regexp.MustCompile(`-run\s+(?:\\)?\s*'([^']*)'`)

// continuationRE matches a Makefile line-continuation backslash.
var continuationRE = regexp.MustCompile(`\\\s*$`)

// goTestRunFilters returns every `go test` command in the Makefile that
// filters by name, with the target and module it runs under.
//
// The module and target come from goTestCommands, which already reads the
// `cd` off the command's own line and tracks the enclosing target; the only
// thing added here is the filter, which usually sits on a continued line
// below it. Re-deriving the module from the filter's own line is the mistake
// this file's header describes, so it is not done.
func goTestRunFilters(src string, reachable map[string]bool) []RunFilter {
	lines := strings.Split(src, "\n")
	var out []RunFilter
	for _, c := range goTestCommands(src) {
		if c.Target == "" || !reachable[c.Target] {
			continue
		}
		if c.Line < 0 || c.Line >= len(lines) {
			continue
		}
		// Walk forward from the command's own line over the continuations
		// the recipe actually declares, and no further.
		text := lines[c.Line]
		for j := c.Line + 1; j < len(lines) && continuationRE.MatchString(lines[j-1]); j++ {
			text += " " + lines[j]
		}
		m := runRE.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		out = append(out, RunFilter{
			Target:       c.Target,
			Module:       c.Module,
			Alternatives: strings.Split(m[1], "|"),
			Pkgs:         c.Pkgs,
		})
	}
	return out
}

// countRunFlags is the number of `go test -run` recipes in the source that a
// workflow can reach, counted without going through goTestCommands.
//
// It exists to be a witness the parser cannot bias, so it tracks the
// enclosing target itself rather than asking the thing it is checking. The
// first version of this function counted every -run line in the file and the
// first version of the test compared that against a reachable-only parse:
// 23 against 14, which reads exactly like a parser that dropped nine gates
// and sent me looking for a bug in the parser. Nine recipes are simply not
// invoked by any workflow, which is a legitimate state -- a target someone
// runs by hand is still a target.
func countRunFlags(src string, reachable map[string]bool) int {
	n := 0
	target := ""
	for _, line := range strings.Split(src, "\n") {
		stripped := line
		if c := strings.IndexByte(stripped, '#'); c >= 0 {
			stripped = stripped[:c]
		}
		if stripped == "" {
			continue
		}
		if stripped[0] != ' ' && stripped[0] != '\t' {
			target = ""
			if m := targetRE.FindStringSubmatch(stripped); m != nil {
				target = m[1]
			}
			continue
		}
		if target == "" || !reachable[target] {
			continue
		}
		if strings.Contains(stripped, "go test") && strings.Contains(stripped, "-run") {
			n++
		}
	}
	return n
}

// checkRunFilters reports every CI-reachable -run recipe whose filter matches
// no test that exists.
func checkRunFilters(root string, makefileSrc string, reachable map[string]bool) error {
	filters := goTestRunFilters(makefileSrc, reachable)
	// The guard is conditioned on the flag being present at all. A Makefile
	// with no -run recipe is a Makefile that runs whole packages, which is
	// healthy, and failing it turns this check into a false alarm on the
	// several small fixtures the rest of this package is written with. What
	// is a failure is seeing the flag in the source and reading none of it.
	if len(filters) == 0 && countRunFlags(makefileSrc, reachable) > 0 {
		return fmt.Errorf("cigate: the Makefile has %d reachable `go test -run` recipe(s) and none "+
			"of them parsed; the recipe parser has probably stopped matching",
			countRunFlags(makefileSrc, reachable))
	}
	if len(filters) == 0 {
		return nil
	}
	cache := map[string][]string{}
	var vacuous []string
	for _, f := range filters {
		names, ok := cache[f.Module]
		if !ok {
			names = testNamesInModule(root, f.Module)
			cache[f.Module] = names
		}
		if filterMatchesNothing(f.Alternatives, names) {
			vacuous = append(vacuous, fmt.Sprintf(
				"target %q: cd %s && go test %s -run '%s' -- no test in that module matches any of those names, "+
					"so the recipe passes without asserting anything",
				f.Target, f.Module, strings.Join(f.Pkgs, " "), strings.Join(f.Alternatives, "|")))
		}
	}
	if len(vacuous) == 0 {
		return nil
	}
	sort.Strings(vacuous)
	return fmt.Errorf("cigate: %d CI-reachable `go test -run` filter(s) match no test:\n  %s",
		len(vacuous), strings.Join(vacuous, "\n  "))
}

// filterMatchesNothing reports whether every alternative fails to match any
// name. An unparseable alternative is treated as a match so that a broken
// regex is not silently reported as an empty filter -- and so that it is
// reported by `go test` itself rather than by a check that guessed.
func filterMatchesNothing(alternatives []string, names []string) bool {
	for _, alt := range alternatives {
		re, err := regexp.Compile(alt)
		if err != nil {
			return false
		}
		for _, n := range names {
			if re.MatchString(n) {
				return false
			}
		}
	}
	return true
}

var testFuncRE = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

// testNamesInModule is every Test function declared under one module.
func testNamesInModule(root, module string) []string {
	base := filepath.Join(root, module)
	var out []string
	_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		for _, m := range testFuncRE.FindAllStringSubmatch(string(raw), -1) {
			out = append(out, m[1])
		}
		return nil
	})
	return out
}

package main

// A test file behind a build tag is compiled by nothing that does not name
// that tag. That is the whole mechanism, and it is why a tag can remove
// coverage without removing a line of code or turning anything red.
//
// The hole found in decision 187 was one file: the whole "replay the migration
// list on the engine the deployment uses" gate sat behind //go:build
// integration, and `go test ./...` does not compile files that carry a tag. It
// had never run in CI. Wiring it up exposed a second one in the same tag — a
// package the newly wired command did not name — which is the shape this check
// exists to stop: a tag is not a coverage switch, it is a hole with a name.
//
// The question this asks per file is deliberately narrow. Not "is it covered
// somewhere" but "does one command in a CI-reachable target name the tag AND
// the module AND the package". Anything weaker lets a command that runs the
// right tag in the right module but for a different package read as coverage,
// which is exactly how the second file stayed invisible.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// implicitTags are satisfied by the toolchain's own settings. A file guarded
// by one of these is platform-specific, not opt-in, and `go test ./...` builds
// it on every platform that matches. Treating `linux` as an opt-in tag would
// report two changewatcher test files as uncovered on a Linux runner, which is
// the kind of false alarm that gets a check deleted instead of fixed.
var implicitTags = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true,
	"linux": true, "netbsd": true, "openbsd": true, "plan9": true,
	"solaris": true, "wasip1": true, "windows": true, "zos": true,
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
	"mips": true, "mips64": true, "mips64le": true, "mipsle": true,
	"ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true,
	"wasm": true, "unix": true, "cgo": true, "ignore": true,
	"gc": true, "gccgo": true,
}

// goVersionTagRE matches the go1.N family, which the constraint parser
// treats as always-satisfied at or above the toolchain's own version.
var goVersionTagRE = regexp.MustCompile(`^go1\.[0-9]+`)

// tagTokenRE extracts the identifiers of a //go:build expression. Splitting on
// the operators and keeping the identifier-shaped pieces is enough here: the
// check needs the set of tags a file asks for, not a re-implementation of
// constraint evaluation. Which side of an expression is required is answered
// by the toolchain instead — see ignoredTestFiles, which asks `go list` which
// files are excluded rather than deciding it here.
var tagTokenRE = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.]*`)

// buildLineRE reads the //go:build line out of a test file's header.
var buildLineRE = regexp.MustCompile(`(?m)^//go:build\s+(.+)$`)

// optInTags is the set of tags in a build expression that a caller has to ask
// for. Everything implicit is dropped, because the toolchain supplies it.
func optInTags(expr string) []string {
	seen := map[string]bool{}
	for _, tok := range tagTokenRE.FindAllString(expr, -1) {
		if implicitTags[tok] || goVersionTagRE.MatchString(tok) {
			continue
		}
		seen[tok] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// TestCommand is one `go test` invocation recovered from a Makefile recipe,
// together with enough of its context to decide what it actually covers.
type TestCommand struct {
	// Target is the make target the command lives in.
	Target string
	// Module is the directory the recipe cd's into, relative to the repo
	// root. "." is the root module.
	Module string
	// Tags are the build tags the command passes.
	Tags []string
	// Pkgs are the package patterns it names, in their written form.
	Pkgs []string
	// Line is the 0-based index of the physical line the command starts on.
	// A `-run` filter usually lives on a continued line, so a reader that
	// wants the whole recipe needs to know where the command began.
	Line int
}

// covers reports whether this command would compile the test files of one
// package: right module, every tag the file needs, and a pattern that reaches
// the package — either "./..." for the module or the package's own path.
func (c TestCommand) covers(module, pkgDir string, tags []string) bool {
	if filepath.Clean(c.Module) != filepath.Clean(module) {
		return false
	}
	for _, need := range tags {
		if !contains(c.Tags, need) {
			return false
		}
	}
	for _, p := range c.Pkgs {
		if p == "./..." {
			return true
		}
		// A directory pattern covers everything beneath it, which is what
		// `go test ./tests/e2e/` does. Matching it exactly reported the
		// e2e suite's own testenv package as uncovered, and a check that
		// cries wolf on the suite it is protecting gets turned off.
		base := filepath.Clean(strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/"))
		target := filepath.Clean(pkgDir)
		if base == target || strings.HasPrefix(target, base+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// goTestCommands parses every `go test` line in the Makefile into a TestCommand.
//
// The two shapes it has to read are `cd <dir> && go test ...` (which is how a
// multi-module repository addresses a package that is not in the root module)
// and a bare `go test ...` (root module). A recipe that changes directory with
// a shell `cd` on its own line is not recognised, and that is reported as an
// absence rather than guessed at.
func goTestCommands(src string) []TestCommand {
	var out []TestCommand
	target := ""
	inRecipe := false
	for lineNo, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		// A comment is not a command, including the silenced form make lets
		// you write inside a recipe: `@# ...` never runs. Skipping only a
		// leading `#` meant a recipe comment that quoted a command — which
		// is how people explain what a gate does — was parsed as that
		// command, package path and all. It surfaced as a dead package
		// pointing at a path with a backtick in it, which is the least
		// actionable message this tool can produce.
		if trimmed == "" || strings.HasPrefix(strings.TrimPrefix(trimmed, "@"), "#") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			inRecipe = false
			if m := targetRE.FindStringSubmatch(line); m != nil {
				target = m[1]
			}
			continue
		}
		if !strings.HasPrefix(trimmed, "@") && !inRecipe {
			// The line after a target header is still the header's own
			// options; a recipe line is what follows them.
			if targetRE.MatchString(line) || strings.HasPrefix(trimmed, "##") {
				continue
			}
			inRecipe = true
		}
		if !inRecipe || !strings.Contains(trimmed, "go test") {
			continue
		}
		cmd := strings.TrimSpace(strings.TrimPrefix(trimmed, "@"))
		c := TestCommand{Target: target, Module: ".", Line: lineNo}
		module := "."
		if cd := regexp.MustCompile(`(?:^|[;&|]\s*)cd\s+([^\s&|;]+)\s*(?:&&|;)`).FindStringSubmatch(cmd); cd != nil {
			module = filepath.Clean(cd[1])
		}
		rest := cmd[strings.Index(cmd, "go test"):]
		c.Module = module
		for _, m := range regexp.MustCompile(`-tags[= ]([^\s\\]+)`).FindAllStringSubmatch(rest, -1) {
			for _, t := range strings.Split(m[1], ",") {
				c.Tags = append(c.Tags, strings.TrimSpace(t))
			}
		}
		for _, f := range strings.Fields(rest) {
			if strings.HasPrefix(f, "-") || strings.HasPrefix(f, "'") {
				continue
			}
			if strings.HasPrefix(f, "./") {
				c.Pkgs = append(c.Pkgs, strings.Trim(f, "'"))
			}
		}
		out = append(out, c)
	}
	return out
}

// targetRE matches a Makefile target header line.
var targetRE = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9._-]*):`)

// gatedTestFile is one test file the toolchain excludes from a default build.
type gatedTestFile struct {
	Module string
	Dir    string // package directory relative to Module
	File   string // path relative to the repository root
	Tags   []string
}

// findModules lists every module directory as an absolute path, root first.
//
// Absolute, because the caller has to compare a module directory against the
// absolute .Dir that `go list` prints, and filepath.Rel refuses to relate a
// relative base to an absolute target. Making them comparable is the whole
// job here; a module list that walks fine and then matches nothing is the
// quiet version of the bug this file exists to find.
func findModules(root string) ([]string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.Walk(absRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		base := info.Name()
		if base == "web" || base == "plugins" || base == "node_modules" || base == ".git" || base == "vendor" {
			return filepath.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
			// Record and keep walking. This repository nests its modules —
			// core/manager, core/pig and six extension modules all live inside
			// the root module's directory — so stopping at the first go.mod
			// finds exactly one module and reports every gated file as absent.
			// That is the failure mode this check must not have: a bug that
			// makes it silently check nothing.
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) < len(out[j])
		}
		return out[i] < out[j]
	})
	return out, nil
}

// listTemplate prints, per package, the directory and the files the toolchain
// excluded. Asking `go list` is the point: which files a build tag removes is
// the toolchain's judgement about constraint semantics, and re-implementing it
// here would eventually disagree with it.
// The separator is a token that cannot appear in a path rather than a tab:
// go list formats its output through a tabwriter, so a literal \t arrives as
// spaces and the split below silently yields zero packages -- a check that
// found no gated files would read exactly like a repository that has none.
const listTemplate = `{{.Dir}}<<SEP>>{{range .IgnoredGoFiles}}{{.}} {{end}}`

// listSep is the token listTemplate writes between the two columns.
const listSep = "<<SEP>>"

// ignoredTestFiles returns every test file that a default build does not
// compile, across every module, with the tags it is gated on.
func ignoredTestFiles(root string, modules []string) ([]gatedTestFile, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var out []gatedTestFile
	for _, mod := range modules {
		rel, err := filepath.Rel(absRoot, mod)
		if err != nil {
			rel = "."
		}
		cmd := exec.Command("go", "list", "-f", listTemplate, "./...")
		cmd.Dir = mod
		cmd.Env = append(os.Environ(), "GOWORK=off")
		buf, err := cmd.Output()
		if err != nil {
			// A module that cannot even list is module-standalone-check's
			// job, not this one's; saying so is better than guessing.
			continue
		}
		for _, line := range strings.Split(string(buf), "\n") {
			if !strings.Contains(line, listSep) {
				continue
			}
			parts := strings.SplitN(line, listSep, 2)
			dir := parts[0]
			pkgRel, err := filepath.Rel(mod, dir)
			if err != nil {
				continue
			}
			if pkgRel == "." {
				pkgRel = "."
			}
			for _, name := range strings.Fields(parts[1]) {
				if !strings.HasSuffix(name, "_test.go") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					continue
				}
				m := buildLineRE.FindStringSubmatch(string(raw))
				if m == nil {
					continue
				}
				tags := optInTags(m[1])
				if len(tags) == 0 {
					// Platform-specific, not opt-in: built by default
					// wherever the platform matches.
					continue
				}
				file := filepath.ToSlash(filepath.Join(rel, pkgRel, name))
				if pkgRel == "." {
					file = filepath.ToSlash(filepath.Join(rel, name))
				}
				out = append(out, gatedTestFile{
					Module: rel,
					Dir:    filepath.ToSlash(pkgRel),
					File:   file,
					Tags:   tags,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// BuildTagHole is one test file no CI-reachable command would compile.
type BuildTagHole struct {
	File string
	Tags []string
	Why  string
	Near []string // commands that got close, for the message
}

func (h BuildTagHole) String() string {
	return fmt.Sprintf("%s is behind //go:build %s and no CI command names that tag for its package: %s",
		h.File, strings.Join(h.Tags, " "), h.Why)
}

// buildTagHoles is the check. cmds must already be restricted to targets a
// workflow reaches — a `go test` in a target nobody calls covers nothing.
func buildTagHoles(files []gatedTestFile, cmds []TestCommand) []BuildTagHole {
	var holes []BuildTagHole
	for _, f := range files {
		if anyCovers(f, cmds) {
			continue
		}
		h := BuildTagHole{File: f.File, Tags: f.Tags}
		need := strings.Join(f.Tags, " ")
		// Explain the hole with the commands that come closest, and only
		// with commands that actually pass the tag. Quoting a command that
		// does not -- "every command that passes integration runs in another
		// module", said by a target that passes no tag at all -- reads as a
		// reason and is not one.
		passing := 0
		for _, c := range cmds {
			if !hasAll(c.Tags, f.Tags) {
				continue
			}
			passing++
			reason := "it does not name this package"
			if filepath.Clean(c.Module) != filepath.Clean(f.Module) {
				reason = fmt.Sprintf("it runs in %s", c.Module)
			}
			h.Near = append(h.Near, fmt.Sprintf("%s: %s", c.Target, reason))
		}
		switch {
		case passing == 0:
			h.Why = fmt.Sprintf("no CI-reachable command passes %s at all", need)
		case len(h.Near) == 1:
			h.Why = fmt.Sprintf("the only one that does is %s, and %s", h.Near[0], "it runs elsewhere")
		default:
			h.Why = fmt.Sprintf("no command that passes %s names ./%s/", need, f.Dir)
		}
		holes = append(holes, h)
	}
	return holes
}

func anyCovers(f gatedTestFile, cmds []TestCommand) bool {
	for _, c := range cmds {
		if c.covers(f.Module, f.Dir, f.Tags) {
			return true
		}
	}
	return false
}

func hasAll(have, need []string) bool {
	for _, n := range need {
		if !contains(have, n) {
			return false
		}
	}
	return true
}

// checkBuildTagCoverage is the entry point wired into check().
func checkBuildTagCoverage(root string, makefileSrc string, reachable map[string]bool) error {
	modules, err := findModules(root)
	if err != nil {
		return fmt.Errorf("cigate: find modules: %w", err)
	}
	files, err := ignoredTestFiles(root, modules)
	if err != nil {
		return fmt.Errorf("cigate: list gated test files: %w", err)
	}
	if len(files) == 0 {
		return nil
	}
	all := goTestCommands(makefileSrc)
	// Only commands a workflow can reach count. A `go test` in a target nobody
	// invokes compiles nothing, which is the same mistake this check exists to
	// catch one level up.
	var cmds []TestCommand
	for _, c := range all {
		if c.Target == "" || !reachable[c.Target] {
			continue
		}
		cmds = append(cmds, c)
	}
	holes := buildTagHoles(files, cmds)
	if len(holes) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d test file(s) sit behind a build tag that no CI-reachable command passes, "+
		"so `go test ./...` never compiles them and nothing is red:\n", len(holes))
	for _, h := range holes {
		fmt.Fprintf(&b, "  - %s\n", h)
	}
	return fmt.Errorf("cigate: %s", b.String())
}

// targetDeps reads the prerequisite list off a Makefile target header.
//
// Only prerequisites written on the header line count. A prerequisite built by
// a recipe with $(MAKE) is followed in spirit by the closure below only when
// it is also declared; a target this parser cannot see is treated as
// unreachable, which is the direction that reports rather than the direction
// that hides.
var targetDepsRE = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9._-]*):[ \t]*([^=]*)$`)

func targetDeps(src string) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(src, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		m := targetDepsRE.FindStringSubmatch(strings.TrimRight(line, " \t"))
		if m == nil {
			continue
		}
		var deps []string
		for _, tok := range strings.Fields(m[2]) {
			// Variables and pattern rules are not static edges.
			if strings.ContainsAny(tok, "$%") {
				continue
			}
			deps = append(deps, tok)
		}
		out[m[1]] = deps
	}
	return out
}

// reachableTargets is the transitive closure of what a workflow can start. A
// `go test` living in a target nothing reaches compiles nothing, so the
// coverage question has to be asked of this set and not of the Makefile.
func reachableTargets(src string, invoked map[string]bool) map[string]bool {
	deps := targetDeps(src)
	seen := map[string]bool{}
	var walk func(string)
	walk = func(t string) {
		if seen[t] {
			return
		}
		seen[t] = true
		for _, d := range deps[t] {
			walk(d)
		}
	}
	for t := range invoked {
		walk(t)
	}
	return seen
}

package ledgercheck

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The third number the repository can answer for itself: how many modules
// OpsKeeper is split into.
//
// The Makefile is the source of truth rather than a constant written here,
// because PIG_MODULES is the list `module-standalone-check` actually iterates
// and `module-standalone-check` is the gate that builds each one on its own
// published tags. A count written in a test would be a third copy of a list
// that already has two, and the copy nobody maintains is the one that gets
// quoted.

const makefilePath = "../../Makefile"

// moduleCountRE reads the count the progress table's architecture row claims.
var moduleCountRE = regexp.MustCompile(`(\d+) 个模块落地`)

// declaredModules counts the entries of PIG_MODULES in the Makefile.
//
// The list is nine lines long and joined with trailing backslashes, so it is
// read line by line rather than with a single regexp. A regex written for this
// compiles, passes its own reading of the file, and silently stops after the
// first line — which reports seven of fourteen modules and turns the check
// below into a permanent false positive that nobody would take seriously.
func declaredModules(t *testing.T) (int, []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(makefilePath))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}

	var fields []string
	collecting := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if collecting {
			fields = append(fields, strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), "\\"))...)
			if !strings.HasSuffix(line, "\\") {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "PIG_MODULES :=") {
			collecting = true
			fields = append(fields, strings.Fields(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, "PIG_MODULES :=")), "\\"))...)
			if !strings.HasSuffix(line, "\\") {
				break
			}
		}
	}
	if !collecting {
		t.Fatal("the Makefile declares no PIG_MODULES; module-standalone-check iterates that list, so a check against it would be checking nothing")
	}
	if len(fields) == 0 {
		t.Fatal("PIG_MODULES is empty")
	}
	return len(fields), fields
}

// planARow returns the architecture row of the progress table.
func planARow(t *testing.T, ledger string) string {
	t.Helper()
	row := progressRow(t, ledger, "| A 模块化地基 |")
	return row
}

// TestTheProgressTableCountsTheModulesTheMakefileDeclares is the check for the
// "13 个模块" that should have read 14.
//
// It is a gate rather than a note because the number only changes when someone
// adds or removes a module — which is exactly the kind of change that ought to
// make somebody look at the architecture row again.
func TestTheProgressTableCountsTheModulesTheMakefileDeclares(t *testing.T) {
	declared, list := declaredModules(t)
	row := planARow(t, readLedger(t))

	m := moduleCountRE.FindStringSubmatch(row)
	if m == nil {
		t.Fatalf("the architecture row quotes no module count (`<n> 个模块落地`), so nothing can be checked; "+
			"the Makefile declares %d: %s", declared, strings.Join(list, " "))
	}
	claimed, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("unreadable module count %q: %v", m[1], err)
	}
	if claimed != declared {
		t.Errorf("the progress table says %d modules; the Makefile's PIG_MODULES lists %d (%s)",
			claimed, declared, strings.Join(list, " "))
	}
}

// progressRow returns the first row of the progress table whose line starts
// with prefix.
//
// It scopes the search to the progress section on purpose. Several decisions
// carry their own copy of these tables, and those are history: decision 108's
// D row still says four packages, which was true when it was written.
// Rewriting them would be falsifying the record, so the checks read the
// progress section and only that one.
func progressRow(t *testing.T, ledger, prefix string) string {
	t.Helper()
	start := strings.Index(ledger, progressHeading)
	if start < 0 {
		t.Fatalf("the ledger has no %q section; this check reads the progress table and has to be told where it moved", progressHeading)
	}
	rest := ledger[start:]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	for _, line := range strings.Split(rest, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no line starting with %q inside %q; the progress table's shape changed", prefix, progressHeading)
	return ""
}

// moduleDirsInTree returns every directory in the repository that carries a
// go.mod, as repository-relative paths with "." for the root.
//
// `plugins/` is excluded, and the exclusion is a fact about the tree rather
// than a convenience: those are packaged copies of the extension modules that
// already exist under core/pig/extensions, and they are built and checked by
// make plugin-extension-build-check. Counting them would make this a check
// about vendored duplicates rather than about the modules that ship.
func moduleDirsInTree(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	// The root is derived from the Makefile this package already reads, not
	// found by walking upwards for a marker: a marker walk is a second way to
	// answer "where is the repository", and this package has no reason to own
	// one.
	absMakefile, errAbs := filepath.Abs(filepath.FromSlash(makefilePath))
	if errAbs != nil {
		t.Fatalf("abs %s: %v", makefilePath, errAbs)
	}
	root := filepath.Dir(absMakefile)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (name == ".git" || name == "node_modules") {
			return fs.SkipDir
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(rel, "plugins/") {
			return fs.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
			out[rel] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for go.mod: %v", err)
	}
	out["."] = true
	return out
}

// TestTheStandaloneListCoversEveryModuleInTheTree is the third link.
//
// The gate above compares the progress table against PIG_MODULES, and that
// pair is kept in step — so a new module that nobody adds to PIG_MODULES leaves
// both numbers describing the same, smaller reality, and everything is green
// while `module-standalone-check` quietly stops building and testing it. Two
// documents agreeing with each other is not evidence that they describe the
// tree.
//
// Measured this round before writing it: the list and the tree were both 18, so
// the hole was latent, not open. That is exactly the state a check should be
// added in — a hole nobody has walked into yet, rather than one somebody has.
func TestTheStandaloneListCoversEveryModuleInTheTree(t *testing.T) {
	_, declared := declaredModules(t)
	tree := moduleDirsInTree(t)

	inList := map[string]bool{}
	for _, m := range declared {
		inList[m] = true
	}

	var problems []string
	for module := range tree {
		if !inList[module] {
			problems = append(problems, fmt.Sprintf(
				"%s has a go.mod but is not in PIG_MODULES, so module-standalone-check never "+
					"builds or tests it; every gate is still green", module))
		}
	}
	for _, module := range declared {
		if !tree[module] {
			problems = append(problems, fmt.Sprintf(
				"PIG_MODULES names %s, which has no go.mod in the tree; the standalone check "+
					"would fail there for a directory that does not exist", module))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("the standalone module list does not match the tree:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

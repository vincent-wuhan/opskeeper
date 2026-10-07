// Package reporoot locates the OpsKeeper repository root from a test's or a
// script's working directory, using markers that are tracked in git.
//
// Why this package exists at all: eight test suites needed to walk up from
// their own package directory to the repository root, and each one asked
// "does this directory contain go.work?". go.work is gitignored on purpose
// (it is the local development workspace), so a checkout that never ran
// `go work init` does not have one — which is exactly the checkout CI has,
// and the checkout `make module-standalone-check` is meant to prove. In that
// tree the question is false at every level, and the eight callers split
// three ways:
//
//   - five walkers never find a root and fail the test outright;
//   - two guards treat "no go.work" as "not this repository" and skip
//     silently, so a gate that looks green is proving nothing;
//   - one had already noticed and written its own Markerfile/VERSION/
//     plugins check, which is the rule this package now holds for all of
//     them.
//
// So the marker set is the fix, and it is deliberately a set of files that
// only occur together at the repository root and are all committed: the
// Makefile, the VERSION file, and the plugins/pig-ops tree. Any one of them
// can move; all three together do not.
package reporoot

import (
	"os"
	"path/filepath"
)

// Markers are the tracked paths that occur together only at the repository
// root.
//
// They are committed files rather than go.work, which is gitignored, and they
// are a set rather than a single file, so a copy of one of them somewhere in
// the tree cannot masquerade as the root. `TestTheMarkersPickOutTheRoot` in
// this package's test file is what keeps that true against the real tree.
var Markers = []string{"Makefile", "VERSION", "plugins/pig-ops"}

// IsRoot reports whether dir is the repository root: every marker is present.
//
// It is a stat-only test, so it is safe to call on a path that does not
// exist or that the process cannot read; both read as "not the root" rather
// than as an error, because the caller is looking for a root and a
// permission failure on some unrelated ancestor is not its business.
func IsRoot(dir string) bool {
	for _, marker := range Markers {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(marker))); err != nil {
			return false
		}
	}
	return true
}

// Find walks up from start until it finds the repository root, or returns
// ok=false if it reaches the filesystem root first.
//
// maxUp bounds the walk so a caller cannot, by pointing at a deep path,
// spend its whole budget statting ancestors. The real walks in this repo are
// at most eight levels, so the bound is generous rather than tight.
func Find(start string, maxUp int) (root string, ok bool) {
	dir := start
	for i := 0; i <= maxUp; i++ {
		if IsRoot(dir) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

package reporoot

import (
	"os"
	"path/filepath"
	"testing"
)

// findThisRepo walks up from the test's own directory the way every caller
// does, and it must land on the real root. This is the assertion the whole
// package exists for: the root has to be findable in a checkout that has no
// go.work, which is what a clean clone and CI both are.
func findThisRepo(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, ok := Find(wd, 12)
	if !ok {
		t.Fatalf("could not find the repository root from %s", wd)
	}
	return root
}

// The root this package finds must be the root the rest of the repository
// means: the directory that holds the Makefile. Anything else would make
// every caller read the wrong tree.
func TestTheMarkersPickOutTheRoot(t *testing.T) {
	root := findThisRepo(t)
	if _, err := os.Stat(filepath.Join(root, "Makefile")); err != nil {
		t.Fatalf("found root %s has no Makefile: %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("found root %s has no go.mod: %v", root, err)
	}
	// The walk must not stop at a nested module: core/floor has a go.mod of
	// its own, and four of the callers live at or below it.
	if got := filepath.Base(root); got == "floor" || got == "core" {
		t.Fatalf("the walk stopped at %s, a module directory, not the repository root", root)
	}
}

// A directory with only some of the markers is not the root. This is the
// property that makes the set better than a single file: a stray Makefile or
// VERSION somewhere in the tree cannot masquerade as the top.
func TestAPartialMarkerSetIsNotTheRoot(t *testing.T) {
	for i := range Markers {
		dir := t.TempDir()
		for j, marker := range Markers {
			if i == j {
				continue
			}
			path := filepath.Join(dir, filepath.FromSlash(marker))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatalf("write %s: %v", marker, err)
			}
		}
		if IsRoot(dir) {
			t.Errorf("a directory missing %q was taken for the repository root", Markers[i])
		}
	}
}

// Every marker present is the root.
func TestTheFullMarkerSetIsTheRoot(t *testing.T) {
	dir := t.TempDir()
	for _, marker := range Markers {
		path := filepath.Join(dir, filepath.FromSlash(marker))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", marker, err)
		}
	}
	if !IsRoot(dir) {
		t.Fatal("a directory with every marker was not taken for the root")
	}
}

// A path that does not exist is not the root, and asking must not panic.
func TestAMissingPathIsNotTheRoot(t *testing.T) {
	if IsRoot(filepath.Join(t.TempDir(), "does", "not", "exist")) {
		t.Fatal("a missing path was taken for the root")
	}
	if _, ok := Find(filepath.Join(t.TempDir(), "absent"), 4); ok {
		t.Fatal("Find returned a root above a path that does not exist")
	}
}

// Find walks up; given a nested directory it returns the same root as when
// given the root itself.
func TestFindWalksUpToTheSameRoot(t *testing.T) {
	root := findThisRepo(t)
	nested := filepath.Join(root, "core", "floor", "reporoot")
	got, ok := Find(nested, 12)
	if !ok {
		t.Fatalf("Find from %s found nothing", nested)
	}
	if got != root {
		t.Fatalf("Find from a nested directory returned %s, want %s", got, root)
	}
}

// The markers must occur together at exactly one directory in the tree. If a
// second directory every bit as marker-complete as the root appears, the walk
// becomes order-dependent and every caller could silently read the wrong
// tree. This walks the real repository to prove it.
func TestNoSecondCompleteMarkerSetExists(t *testing.T) {
	root := findThisRepo(t)
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		switch info.Name() {
		case ".git", "node_modules", "vendor", "dist", "bin", "tmp", "testdata":
			return filepath.SkipDir
		}
		if IsRoot(path) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("the walk found no root at all; it is not looking at the tree")
	}
	if len(hits) > 1 {
		// Only the repository root may look like the repository root. A
		// second hit means the marker set is too weak and a caller could
		// read a nested tree as the top of the repository.
		t.Fatalf("the marker set matches %d directories, so the root is ambiguous: %v", len(hits), hits)
	}
}

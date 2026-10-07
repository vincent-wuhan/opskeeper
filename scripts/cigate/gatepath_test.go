package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree materialises a fake repository: every path in dirs becomes a
// directory holding one Go file, and every path in mods becomes a module
// root (a directory holding go.mod). Paths may overlap.
func writeTree(t *testing.T, root string, mods, dirs []string) {
	t.Helper()
	for _, m := range mods {
		full := filepath.Join(root, m)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(filepath.Join(full, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatalf("write go.mod in %s: %v", full, err)
		}
	}
	for _, d := range dirs {
		full := filepath.Join(root, d)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(filepath.Join(full, "a.go"), []byte("package a\n"), 0o644); err != nil {
			t.Fatalf("write a.go in %s: %v", full, err)
		}
	}
}

const liveMakefile = "check:\n\tcd core/base && GOWORK=off go test ./pkg/audit/ -count=1\n"

func TestALivePathIsNotReported(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/base"}, []string{"core/base/pkg/audit"})
	if err := checkGatePackagePaths(root, liveMakefile, map[string]bool{"check": true}); err != nil {
		t.Fatalf("a package that exists was reported: %v", err)
	}
}

// This is the false alarm that made the author distrust the check on the day
// it was written: `cd core/floor/config && go test ./...` names no directory
// of its own, and reading the pattern as one reported a module that was
// sitting right there. The whole module is the module check's business.
// The regression the first version of the vacuity guard caused: it counted
// literal directories, so a repository whose every recipe says `./...` was
// told it had a broken parser. Counting what was read is the honest answer.
func TestAMakefileOfOnlyRecursivePatternsIsHealthy(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/base"}, []string{"core/base/pkg/audit"})
	mk := "check:\n\tcd core/base && GOWORK=off go test ./... -count=1\n" +
		"\tcd core/base && GOWORK=off go test ./pkg/... -count=1\n"
	if err := checkGatePackagePaths(root, mk, map[string]bool{"check": true}); err != nil {
		t.Fatalf("a Makefile with no literal package to check was reported: %v", err)
	}
}

func TestTheWholeModulePatternIsNotADirectory(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/floor/config"}, []string{"core/floor/config"})
	mk := "check:\n\tcd core/floor/config && GOWORK=off go test ./... -count=1\n"
	if err := checkGatePackagePaths(root, mk, map[string]bool{"check": true}); err != nil {
		t.Fatalf("`./...` was read as a directory: %v", err)
	}
}

func TestARecursivePatternIsAnsweredByItsPrefix(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/base"}, []string{"core/base/pkg/audit"})
	mk := "check:\n\tcd core/base && GOWORK=off go test ./pkg/... -count=1\n"
	if err := checkGatePackagePaths(root, mk, map[string]bool{"check": true}); err != nil {
		t.Fatalf("a recursive pattern over a live tree was reported: %v", err)
	}
}

// The false alarm that decision 348 found by wiring a gate into CI for the
// first time: `edge-credential-check` cds into core/floor/config, which is a
// package inside the core/floor module rather than a module root, and
// `go test ./...` from there works. Demanding a go.mod in the anchor reported
// a live tree as gone -- and a gate that cries wolf on correct input is
// deleted, which would have taken the path check with it.
func TestACdAnchorInsideAModuleIsNotAMissingModule(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/floor"}, []string{"core/floor/config"})
	mk := "check:\n\tcd core/floor/config && GOWORK=off go test ./... -count=1\n"
	if err := checkGatePackagePaths(root, mk, map[string]bool{"check": true}); err != nil {
		t.Fatalf("a cd anchor inside a live module was reported as gone: %v", err)
	}
}

// The other half, and the rot the check was built for: the anchor is gone
// altogether, and no ancestor of it carries a go.mod either.
func TestACdAnchorThatNoLongerBelongsToAnyModuleIsReported(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/floor"}, []string{"core/floor/config"})
	mk := "check:\n\tcd core/domains/config && GOWORK=off go test ./... -count=1\n"
	if err := checkGatePackagePaths(root, mk, map[string]bool{"check": true}); err == nil {
		t.Fatal("a cd anchor that left its module behind was accepted")
	}
}

func TestAPackageThatMovedIsReportedWithItsTarget(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/base"}, []string{"core/base/pkg/promptguard"})
	err := checkGatePackagePaths(root, liveMakefile, map[string]bool{"check": true})
	if err == nil {
		t.Fatal("a package that no longer exists was not reported")
	}
	msg := err.Error()
	for _, want := range []string{"check", "./pkg/audit/", "core/base"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report does not name %q, so a reader cannot tell which recipe to fix:\n%s", want, msg)
		}
	}
}

// A directory that is present but holds no Go file fails `go test` exactly
// as loudly as one that is gone, and the two produce the same message, so
// treating it as live would leave the gate broken in the one way this check
// exists to prevent.
func TestADirectoryWithoutGoFilesIsReported(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/base"}, []string{"core/base/pkg/promptguard"})
	if err := os.MkdirAll(filepath.Join(root, "core/base/pkg/audit"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := checkGatePackagePaths(root, liveMakefile, map[string]bool{"check": true}); err == nil {
		t.Fatal("an empty directory was accepted as a package")
	}
}

func TestAModuleThatMovedIsReportedOnce(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"other"}, []string{"other/pkg/audit"})
	err := checkGatePackagePaths(root, liveMakefile, map[string]bool{"check": true})
	if err == nil {
		t.Fatal("a module directory that no longer exists was not reported")
	}
	if n := strings.Count(err.Error(), "check\""); n != 1 {
		t.Errorf("the missing module produced %d lines; one cause gets one line, because four "+
			"copies of it teach a reader nothing the first did not:\n%s", n, err)
	}
}

// A recipe nobody in CI invokes compiles nothing, which is the mistake
// checkBuildTagCoverage already refuses to make one level up.
func TestARecipeCINeverInvokesIsNotChecked(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"other"}, []string{"other/pkg/audit"})
	if err := checkGatePackagePaths(root, liveMakefile, map[string]bool{}); err == nil {
		t.Fatal("a dead path in an unreachable target was reported")
	}
}

// The guard that makes the check honest about itself. A parser that stops
// matching produces zero paths and zero problems, which is indistinguishable
// from a clean repository until the next package moves.
func TestAParserThatFindsNothingIsAFailure(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, []string{"core/base"}, []string{"core/base/pkg/audit"})
	err := checkGatePackagePaths(root, "check:\n\t@echo nothing to see here\n", map[string]bool{"check": true})
	if err == nil {
		t.Fatal("a recipe with no go test command passed; the check was inspecting nothing")
	}
	if !strings.Contains(err.Error(), "read no CI-reachable go test command") {
		t.Errorf("the vacuity report does not say what went wrong:\n%s", err)
	}
}

func TestEveryDisagreementIsReportedNotJustTheFirst(t *testing.T) {
	root := t.TempDir()
	// The module itself has to be live, or the module check answers first
	// and the second package is never looked at -- which is the whole
	// disagreement this test is about.
	writeTree(t, root, []string{"core/base"}, nil)
	mk := "check:\n" +
		"\tcd core/base && GOWORK=off go test ./pkg/audit/ -count=1\n" +
		"\tcd core/base && GOWORK=off go test ./pkg/promptguard/ -count=1\n"
	err := checkGatePackagePaths(root, mk, map[string]bool{"check": true})
	if err == nil {
		t.Fatal("two dead paths were not reported")
	}
	if !strings.Contains(err.Error(), "pkg/audit") || !strings.Contains(err.Error(), "pkg/promptguard") {
		t.Errorf("only part of the disagreement was reported, so fixing one at a time is the "+
			"only way to converge:\n%s", err)
	}
}

// The same property against the real repository, so the unit tests above
// cannot all be right about a Makefile nobody ships.
func TestTheRealRepositoryHasLivePathsAndTheCheckIsNotVacuous(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	reachable := reachableTargets(string(makefile), invokedTargets(string(ci)))
	if len(reachable) == 0 {
		t.Fatal("no reachable target; this test would pass by checking nothing")
	}
	if err := checkGatePackagePaths(root, string(makefile), reachable); err != nil {
		t.Fatalf("the real Makefile has a dead gate path: %v", err)
	}

	// "No problems" and "nothing was examined" look identical from outside,
	// and the real repository is the only fixture that can tell them apart.
	// Every gate in the plan's section 6 names its package literally, so a
	// literal count of zero here means the pattern reader has stopped
	// recognising patterns -- which is exactly the mutation that made the
	// first version of the vacuity guard look necessary.
	paths, commands := gatePaths(string(makefile), reachable)
	literals := 0
	for _, p := range paths {
		if p.Dir != "" {
			literals++
		}
	}
	if literals == 0 {
		t.Fatalf("read %d CI-reachable go test command(s) out of the real Makefile and found "+
			"%d literal package pattern(s); the real gates all name their package, so zero "+
			"means the pattern reader is broken and the check above passed by looking at nothing",
			commands, literals)
	}
}

// A recipe comment is not a recipe command. make runs nothing on a `@#` line,
// and this parser used to read the command those comments quote — which is how
// they are written, since the comment is there to explain the command — as if
// it were being run. The path it then invented carried a backtick into the
// "package is gone" report, which is the least actionable thing this tool can
// say.
func TestARecipeCommentIsNotACommand(t *testing.T) {
	// The comment has to come AFTER a real recipe line: a leading one is
	// skipped for a different reason (a recipe's first line is read as the
	// target's own options), and a test that passes for the wrong reason is
	// the same failure this tool exists to catch.
	mk := "check:\n" +
		"\tcd core/base && GOWORK=off go test ./pkg/audit/ -count=1\n" +
		"\t@# 上面那条 `go test ./...` 不编译带 tag 的测试。\n"
	cmds := goTestCommands(mk)
	if len(cmds) != 1 {
		t.Fatalf("read %d command(s) from a recipe with one command and one comment", len(cmds))
	}
	if len(cmds[0].Pkgs) != 1 || cmds[0].Pkgs[0] != "./pkg/audit/" {
		t.Errorf("packages = %v, want only the one the recipe actually runs", cmds[0].Pkgs)
	}
}

package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheHarnessBuildsEveryBinaryTheWayAReleaseDoes is the guard on the
// three build call sites this package has.
//
// It exists because the three drifted. The node agent was built with the
// workspace off — with a comment explaining why — while the manager and the
// node binary were not, so two of the three linked whatever PiG checkout the
// developer's go.work happened to point at. The failure that surfaced it was
// a compile error naming OpsKeeper's own file, because a tag-pinned 0.x
// dependency and the local checkout of it are different libraries wearing
// the same import path.
//
// A source scan rather than a behavioural test, because the behaviour is
// "which bytes get linked" and that is not observable from inside the test
// process. The rule is mechanical: every `go build` in this package takes
// its environment from buildEnv.
func TestTheHarnessBuildsEveryBinaryTheWayAReleaseDoes(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no source files found; this test would pass on anything")
	}
	var checked int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			if !strings.Contains(line, `"go", "build"`) {
				continue
			}
			checked++
			// The environment must be set somewhere between building the
			// command and running it. That is the precise rule — not "within
			// N lines" — so a call site may explain itself at length without
			// tripping the guard, and may not defer the Env assignment past
			// the Run that would use it.
			end := len(lines)
			for j := i + 1; j < len(lines); j++ {
				if strings.Contains(lines[j], "cmd.Run()") {
					end = j + 1
					break
				}
			}
			window := strings.Join(lines[i:end], "\n")
			if !strings.Contains(window, "buildEnv()") {
				t.Errorf("%s:%d builds a binary without buildEnv(), so it inherits whatever "+
					"workspace the developer has: the release it is supposed to be testing "+
					"is pinned by tag", name, i+1)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no go build call sites found; the scan is not looking at what it claims")
	}
}

// TestBuildEnvKeepsTheWorkspaceOut pins the one property buildEnv exists for.
func TestBuildEnvKeepsTheWorkspaceOut(t *testing.T) {
	env := buildEnv()
	var found bool
	for _, kv := range env {
		if kv == "GOWORK=off" {
			found = true
		}
	}
	if !found {
		t.Errorf("buildEnv() = %v, which does not turn the workspace off", env)
	}
}

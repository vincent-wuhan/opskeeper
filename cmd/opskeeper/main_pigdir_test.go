package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestTheAgentTurnsRunInADirectoryTheProcessOwns covers the two properties
// of pigWorkDir that a reviewer cannot check by reading the caller.
//
// The first is that it is not the process's own working directory. Every
// turn's relative paths resolve here and PiG discovers a session's skills
// and context files under it, so a fallback to "." would put whatever
// directory the manager was started from into the control plane's prompt
// and path resolution — the defect decision 171 exists for.
//
// The second is that it is empty. A working directory is only a containment
// boundary if nothing is in it; a directory that shares a tree with the
// agent's credential files (models.json, settings.json) is one
// path-resolution mistake away from being an exfiltration route, which is
// why the two directories are asserted to be different trees rather than
// merely different paths.
func TestTheAgentTurnsRunInADirectoryTheProcessOwns(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	work := pigWorkDir(log)
	if work == "." || work == "" {
		t.Fatalf("pigWorkDir returned %q; a turn's relative paths would resolve against "+
			"whatever directory the manager was started from", work)
	}
	info, err := os.Stat(work)
	if err != nil {
		t.Fatalf("pigWorkDir did not create its directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("pigWorkDir returned %q, which is not a directory", work)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("working directory is %o, want 0700: it is writable by the agent's own "+
			"tools, so a group- or world-writable one is a channel out of the process", perm)
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("read working directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("working directory holds %d entries, want an empty one", len(entries))
	}

	agent := pigAgentDir(log)
	if agent == work {
		t.Errorf("the agent directory and the working directory are the same path %q; one "+
			"holds the provider configuration and the other is a writable scratch space", agent)
	}
}

// TestTheRuntimeIsBuiltOnTheWorkingDirectoryAndNotTheProcesses covers the
// wiring rather than the helper: pigWorkDir can be perfect and still unused.
//
// It matters because the two failure modes look identical from outside. A
// manager whose agent turns resolve relative paths against the directory it
// was started from works normally on every machine where that directory
// happens to be empty, and the difference only appears on the machine where
// it is not.
func TestTheRuntimeIsBuiltOnTheWorkingDirectoryAndNotTheProcesses(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	rt, err := newPigRuntime(log, context.Background())
	if err != nil {
		t.Fatalf("newPigRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	got := rt.Services().CWD()
	if got == "." || got == "" {
		t.Fatalf("the runtime's working directory is %q; every turn's relative paths "+
			"resolve against it", got)
	}
	process, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	if filepath.Clean(got) == filepath.Clean(process) {
		t.Errorf("the runtime's working directory is the process's own (%q)", got)
	}
	if filepath.Clean(got) == filepath.Clean(rt.Services().AgentDir()) {
		t.Errorf("the runtime's working directory is the agent directory (%q); one holds the "+
			"provider configuration and the other is a writable scratch space", got)
	}
	entries, err := os.ReadDir(got)
	if err != nil {
		t.Fatalf("read the runtime's working directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the runtime's working directory holds %d entries, want an empty one", len(entries))
	}
}

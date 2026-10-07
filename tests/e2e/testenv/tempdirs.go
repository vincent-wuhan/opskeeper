package testenv

import (
	"os"
	"sync"
)

// tempDirs collects every scratch directory this package creates for a
// compiled binary, so that a test run can hand them back.
//
// The three binaries this package builds — the manager, the edge agent and
// the node's `pig` — are each built once per `go test` behind a sync.Once, and
// each one gets its own `os.MkdirTemp` directory to live in. That directory
// was never removed, so every run of the e2e suite left 74–104 MiB behind in
// the system temp directory. On a developer machine that is invisible; on a CI
// runner that is the difference between a green job and `no space left on
// device` partway through, and the failure it produces looks like a compiler
// problem rather than a housekeeping one.
//
// The directory has to outlive the individual test that happened to trigger
// the build, which is why this is a registry keyed off TestMain rather than a
// `t.Cleanup` at the build site: the Once that builds the binary and the test
// that needed it are frequently not the same test, and a cleanup registered
// against the wrong one would delete the binary out from under its siblings.
var (
	tempDirsMu sync.Mutex
	tempDirs   []string
)

// mkTempDir is os.MkdirTemp plus a registry entry, so that a scratch directory
// created for a build cannot outlive the test binary that made it.
func mkTempDir(pattern string) (string, error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	tempDirsMu.Lock()
	tempDirs = append(tempDirs, dir)
	tempDirsMu.Unlock()
	return dir, nil
}

// Cleanup removes every scratch directory registered by mkTempDir. It is
// idempotent and safe to call from a TestMain after m.Run has returned, which
// is the only point at which no test can still be holding one of the binaries.
func Cleanup() {
	tempDirsMu.Lock()
	dirs := tempDirs
	tempDirs = nil
	tempDirsMu.Unlock()
	for _, dir := range dirs {
		_ = os.RemoveAll(dir)
	}
}

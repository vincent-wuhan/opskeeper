// This file is the one file in this package without the e2e build tag, and
// that is the whole reason it exists.
//
// Every other file here is gated, so an untagged test in this package
// references symbols that only exist under -tags e2e — and the failure it
// produces is `undefined: buildEnv` in a file nobody is looking at, on a
// build nobody asked for. The default `go test ./...` sweep is the one that
// has to notice a mistake like this, so the rule it enforces and the rule it
// is built with have to be in the same build.
//
// The alternative — tagging this file too — was the obvious one and it is
// wrong: nothing in CI runs `go test -tags e2e ./tests/e2e/testenv/`, so a
// tagged guard is a guard that never runs. A check that only exists on the
// branch somebody remembers to take is not a check.

package testenv

import "os"

// buildEnv is the environment every binary this harness runs is built in.
//
// One function rather than one line per call site, because the three call
// sites have to agree and there was a release where they did not: the node
// agent was built with the workspace off while the manager and the node
// binary were not, so two of the three silently linked a developer's local
// PiG checkout. That is not a small difference in a repository whose
// dependency is a 0 library pinned by tag — it is the difference between
// testing the release and testing the desk it was written at.
//
// GOWORK=off is what a release does. CI has no workspace file at all, so
// this makes the local run and the CI run the same run.
func buildEnv() []string {
	return append(os.Environ(), "GOWORK=off")
}

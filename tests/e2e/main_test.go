//go:build e2e

package e2e

import (
	"os"
	"testing"

	"github.com/vincent-wuhan/opskeeper/tests/e2e/testenv"
)

// TestMain owns the package-level lifecycle. The only thing it does is
// terminate the shared testcontainers containers on process exit —
// without this, each `go test ./tests/e2e/...` invocation leaks a
// mysql:8.0 container (≈500 MB) into the host because the testcontainers
// reaper (ryuk) is disabled (mac startup is too flaky). On the 3.6 GiB
// test box, ~10 leaked containers exhaust RAM and kill sshd. The tunnel
// broker is reaped here for the same reason: the delivery test brings one
// up, and nothing else in the process would.
func TestMain(m *testing.M) {
	code := m.Run()
	testenv.TerminateSharedFrontier()
	testenv.TerminateSharedMySQL()
	// The three binaries this suite builds — manager, edge agent and the
	// node's pig — each live in their own scratch directory, and a run used
	// to leave all three behind. Same reason as the two calls above: a
	// leaked 74–104 MiB per run is invisible once and fatal on a runner
	// that has to survive a queue of them.
	testenv.Cleanup()
	os.Exit(code)
}

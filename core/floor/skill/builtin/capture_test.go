package builtin

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
)

// A child that streams far past the cap. If the cap were not applied this
// would return tens of megabytes, and the assertion below would fail — but
// the assertion alone could also pass for the wrong reason (a command that
// simply failed to run). So the first assertion is that the *uncapped* path
// really does produce more than the cap, which is what makes the second one
// mean something.
const floodSize = 40 * 1024 * 1024 // 4x MaxSubprocessStdout

func TestCappedCaptureHoldsTheLine(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a 40MB writer")
	}
	writer := "head -c " + strconv.Itoa(floodSize) + " /dev/zero | tr '\\0' 'x'"

	// First: unbounded, to prove the child really can exceed the cap.
	unbounded := runUnbounded(t, writer)
	if len(unbounded) <= skill.MaxSubprocessStdout {
		t.Fatalf("the probe did not exceed the cap: got %d bytes, cap is %d; "+
			"a cap test on a child that cannot reach it proves nothing",
			len(unbounded), skill.MaxSubprocessStdout)
	}
	t.Logf("unbounded capture: %d bytes (cap is %d)", len(unbounded), skill.MaxSubprocessStdout)

	capped, _, err := runCapped(context.Background(), "sh", "-c", writer)
	if err != nil && capped == nil {
		t.Fatalf("capped run failed outright: %v", err)
	}
	if len(capped) > skill.MaxSubprocessStdout {
		t.Errorf("capped capture returned %d bytes, over the %d cap",
			len(capped), skill.MaxSubprocessStdout)
	}
	t.Logf("capped capture: %d bytes", len(capped))

	// And the surviving prefix is real data, not a truncation notice or a
	// zero-filled placeholder: this is the assertion that would catch a
	// "cap" implemented by returning an empty slice.
	if len(capped) > 0 && strings.Trim(string(capped[:1024]), "x") != "" {
		t.Errorf("the capped output is not the head of the child's stream")
	}
}

// runUnbounded is the pattern this change removed, kept here so the test can
// demonstrate the difference rather than assert it. If someone reintroduces
// CombinedOutput in a tool, this is the shape it will have.
func runUnbounded(t *testing.T, script string) []byte {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		t.Fatalf("unbounded probe failed: %v", err)
	}
	return out
}

// A tool that never started has no stderr of its own. Every caller decides
// "did this fail?" by looking at whether stdout came back, so returning an
// empty tail on a start failure makes a missing binary indistinguishable from
// a tool that ran correctly and found nothing — dmesg would answer "0 kernel
// messages" on a host where dmesg is simply not installed, and the model
// reading that would conclude the kernel is quiet rather than that it cannot
// look. That regression is silent: no error, no panic, a plausible answer.
func TestAMissingBinaryIsReportedAsMissing(t *testing.T) {
	out, errTail, err := runCapped(context.Background(), "opskeeper-no-such-binary")
	if err == nil {
		t.Fatalf("starting a missing binary returned no error")
	}
	if len(out) != 0 {
		t.Errorf("a missing binary produced stdout: %q", out)
	}
	// The reason has to arrive in the tail, because that is the slot the
	// tool implementations read. An empty tail here is exactly the bug.
	if len(errTail) == 0 {
		t.Fatalf("a missing binary returned an empty stderr tail; every caller " +
			"reads that slot to decide failure, so this makes the tool look successful")
	}
	if !strings.Contains(string(errTail), "opskeeper-no-such-binary") {
		t.Errorf("the tail does not name what was missing: %q", errTail)
	}
}

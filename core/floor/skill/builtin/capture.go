package builtin

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
)

// runCapped runs entry and returns its output with the *capture* bounded.
//
// The reason this exists rather than cmd.CombinedOutput: CombinedOutput buffers
// the child's entire stdout and stderr into one slice in memory, with no
// ceiling. A tool that declares limits.output_bytes does not get that bound
// applied at capture — the host's replyFor caps the JSON reply much later,
// after everything the child wrote is already resident. So for a tool whose
// child can be talked into a large stream, output_bytes is not a memory bound
// and never was.
//
// The cap here is the same ceiling the shared subprocess runner already applies
// (skill.MaxSubprocessStdout), so a tool that comes through here is not held to
// a stricter standard than one that goes through the runner. Bytes past the cap
// are dropped rather than returned as ErrShortWrite: a short write surfaces as
// a non-fatal exec error and would make a tool that succeeded look broken.
//
// stderr is kept as a tail rather than dropped, because for several of these
// tools the stderr is the answer — dmesg reports "Operation not permitted" on
// one line and exits non-zero, and the caller has nothing else to show.
func runCapped(ctx context.Context, entry string, args ...string) (stdout []byte, stderrTail []byte, err error) {
	// Bound the wait as well as the buffer: a child that stops producing but
	// never exits would otherwise sit here until the caller's own context
	// deadline, and the manifest's timeout_seconds is enforced further up.
	cmd := exec.CommandContext(ctx, entry, args...)
	cmd.WaitDelay = time.Second

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &cappedBuffer{w: &outBuf, max: skill.MaxSubprocessStdout}
	cmd.Stderr = &cappedBuffer{w: &errBuf, max: skill.MaxSubprocessStderrTail}

	if err := cmd.Start(); err != nil {
		// The child's own stderr is not available when it never started, and
		// every caller below decides "did this tool fail?" by looking at
		// whether stdout came back empty. Returning an empty tail here would
		// make a missing binary look like a tool that ran and found nothing --
		// dmesg on a host without kernel access would answer "0 messages"
		// instead of "dmesg is not available", which is the more useful of
		// those two answers to exactly the wrong question. So the real reason
		// is handed back in the slot the callers already read.
		return nil, []byte(err.Error()), err
	}
	waitErr := cmd.Wait()

	// A non-zero exit is returned rather than swallowed: whether that is an
	// error or merely information is the caller's call. traceroute exits
	// non-zero when a hop is unreachable and still has a usable answer, while
	// strace exiting non-zero with no output is a failure the caller reports.
	_ = errors.As
	return outBuf.Bytes(), errBuf.Bytes(), waitErr
}

// cappedBuffer drops bytes past max and reports that it did, so a caller can
// tell a truncated capture from a short one.
type cappedBuffer struct {
	w       *bytes.Buffer
	max     int
	n       int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.n >= c.max {
		c.dropped += len(p)
		return len(p), nil
	}
	room := c.max - c.n
	if len(p) > room {
		c.w.Write(p[:room])
		c.n = c.max
		c.dropped += len(p) - room
		return len(p), nil
	}
	c.w.Write(p)
	c.n += len(p)
	return len(p), nil
}

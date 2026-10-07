package host

// The two writes that act on what pressure.go observed.
//
// Both are irreversible and neither is reversible by the platform, so each
// one refuses on conditions that would otherwise produce an outage the tool
// is supposed to be fixing. The refusals are the substance of this file;
// the happy path is two lines.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// killProcess terminates one process and reports whether it actually went.
//
// SIGTERM first and never SIGKILL automatically. The escalation decision
// belongs to whoever owns the process: a process that ignores SIGTERM is
// usually mid-write, and SIGKILL turns that into a truncated file. The
// message says so explicitly rather than leaving the operator to work out
// why the CPU is still high.
//
// Three things are refused outright, each because the mistake is
// unrecoverable rather than merely inconvenient:
//   - pid 1 and pid 0, which are init and the scheduler. Killing init stops
//     the machine, and no approval flow is fast enough to matter.
//   - this process and its parent. The adapter is usually running *inside*
//     the agent it is diagnosing, so a pid taken from a stale process list
//     can name the tool that is about to make the call.
//   - kernel threads, whose command is bracketed. They are not ordinary
//     processes and SIGTERM to one is meaningless at best.
func (a *Adapter) killProcess(ctx context.Context, p params) (int, string, bool, []string, error) {
	r, err := a.handle()
	if err != nil {
		return 0, "", false, nil, err
	}
	raw, ok := p["pid"]
	if !ok || raw == nil {
		return 0, "", false, nil, fmt.Errorf("host: pid is required: killing a process needs the pid the process list recorded")
	}
	pid, err := toInt(raw)
	if err != nil {
		return 0, "", false, nil, fmt.Errorf("host: pid: %w", err)
	}
	if pid <= 1 {
		return 0, "", false, nil, fmt.Errorf("host: refusing to signal pid %d: that is not an ordinary process", pid)
	}
	if pid == os.Getpid() {
		return 0, "", false, nil, fmt.Errorf("host: refusing to signal pid %d: that is this adapter, and the call would not return", pid)
	}
	if pid == os.Getppid() {
		return 0, "", false, nil, fmt.Errorf("host: refusing to signal pid %d: that is this adapter's parent, which supervises it", pid)
	}
	before, err := processCommand(ctx, r, pid)
	if err != nil {
		return 0, "", false, nil, fmt.Errorf("host: could not read pid %d before signalling it: %w", pid, err)
	}
	if strings.HasPrefix(before, "[") && strings.HasSuffix(before, "]") {
		return 0, "", false, nil, fmt.Errorf("host: refusing to signal pid %d (%s): it is a kernel thread, not a process that can be stopped",
			pid, before)
	}

	killArgv := []string{"kill", "-TERM", strconv.Itoa(pid)}
	if _, err := r.run(ctx, killArgv); err != nil {
		return 0, "", false, nil, fmt.Errorf("host: kill -TERM %d failed: %w", pid, err)
	}
	// The signal is asynchronous: kill(2) returning 0 says the signal was
	// delivered, not that the process acted on it. Reporting success here
	// without looking would be the "it said it worked" failure this
	// adapter exists to avoid, so the state is read back.
	after, err := processCommand(ctx, r, pid)
	switch {
	case err != nil:
		// ps no longer lists it, which is the outcome we wanted. A pid
		// that has been reaped is indistinguishable from one that never
		// existed, and both mean the process is gone.
		return 1, fmt.Sprintf("sent SIGTERM to pid %d (%s); it is no longer listed", pid, before), true, killArgv, nil
	case after == "":
		return 1, fmt.Sprintf("sent SIGTERM to pid %d (%s); it is no longer listed", pid, before), true, killArgv, nil
	default:
		return 1, fmt.Sprintf("sent SIGTERM to pid %d (%s) but it is still running as %q. "+
			"A process that ignores SIGTERM is usually mid-write; check what it is before escalating to SIGKILL, "+
			"which will truncate whatever it was writing", pid, before, after), false, killArgv, nil
	}
}

// processCommand reads one process's command, returning "" when ps does not
// list it — which is how a dead process is observed.
func processCommand(ctx context.Context, r runner, pid int) (string, error) {
	raw, err := r.run(ctx, []string{"ps", "-p", strconv.Itoa(pid), "-o", "comm="})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// protectedPaths are the directories this adapter will never delete from.
//
// The list is a denylist on purpose, and it is short on purpose: it names
// the directories where deleting a file breaks the machine rather than an
// application, which is the boundary that matters. A denylist of "safe"
// paths would have to enumerate every application's layout, and a path not
// on the list would then be deletable — the wrong default for an
// irreversible action.
var protectedPaths = []string{
	"/", "/bin", "/boot", "/dev", "/etc", "/lib", "/lib64",
	"/proc", "/root", "/sbin", "/sys", "/usr",
}

// removeOldLogs deletes the log files under a path that nothing has written
// in the given number of days.
//
// It re-runs the same find the read tool ran, rather than deleting a list
// handed to it. Two reasons: the list would have to be trusted as still
// accurate, and a caller that could name arbitrary paths would turn this
// into `rm` with extra steps. Re-deriving means the deletion can only ever
// cover files that satisfy the stated criterion *now*.
//
// dry_run reports the same set without touching anything, and it is the
// right first call: an operator approving a deletion should see what it
// would remove before it is removed.
func (a *Adapter) removeOldLogs(ctx context.Context, p params) (int, string, bool, []string, error) {
	r, err := a.handle()
	if err != nil {
		return 0, "", false, nil, err
	}
	path, err := p.requireString("path")
	if err != nil {
		return 0, "", false, nil, err
	}
	clean := filepath.Clean(path)
	for _, protected := range protectedPaths {
		if clean == protected {
			return 0, "", false, nil, fmt.Errorf("host: refusing to delete logs from %s: it is a system directory, "+
				"and reclaiming space never justifies removing the files a machine needs to boot", clean)
		}
	}
	info, err := os.Stat(clean)
	if err != nil {
		return 0, "", false, nil, fmt.Errorf("host: %s: %w", clean, err)
	}
	if !info.IsDir() {
		return 0, "", false, nil, fmt.Errorf("host: %s is not a directory; this tool removes old logs from a log directory, "+
			"not an individual file", clean)
	}
	days, err := p.optionalInt("older_than_days", 30)
	if err != nil {
		return 0, "", false, nil, err
	}
	if days < 1 {
		return 0, "", false, nil, fmt.Errorf("host: older_than_days must be at least 1, got %d", days)
	}
	dryRun := false
	if raw, ok := p["dry_run"]; ok && raw != nil {
		if dryRun, err = toBool(raw); err != nil {
			return 0, "", false, nil, fmt.Errorf("host: dry_run: %w", err)
		}
	}

	candidates, _, err := a.oldLogFiles(ctx, params{"path": clean, "older_than_days": days})
	if err != nil {
		return 0, "", false, nil, err
	}
	if len(candidates) == 0 {
		return 0, fmt.Sprintf("no file under %s has been unchanged for %d day(s); nothing to remove", clean, days), true, nil, nil
	}
	var total int64
	var removed []string
	var failed []string
	for _, row := range candidates {
		file, _ := row["path"].(string)
		size, _ := row["size_bytes"].(float64)
		total += int64(size)
		if dryRun {
			removed = append(removed, file)
			continue
		}
		if _, err := r.run(ctx, []string{"rm", "-f", "--", file}); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", file, err))
			continue
		}
		removed = append(removed, file)
	}
	if dryRun {
		return len(removed), fmt.Sprintf("dry run: %d file(s) under %s unchanged for %d day(s) would be removed, %s reclaimable: %s",
			len(removed), clean, days, humanBytes(total), preview(removed)), true, nil, nil
	}
	message := fmt.Sprintf("removed %d file(s) from %s, %s reclaimable", len(removed), clean, humanBytes(total))
	if len(failed) > 0 {
		return len(removed), message + fmt.Sprintf("; %d could not be removed: %s", len(failed), strings.Join(failed, "; ")), false, nil, nil
	}
	if len(removed) > 0 {
		// No argv is recorded for this op on purpose. Its effect is a set
		// of per-file `rm` calls, not one vector; a declaration carrying
		// any single one of them would re-run a program that removes one
		// file rather than the criterion-derived set. Leaving Argv nil
		// makes TrialOf refuse to crystallise it, which is the honest
		// outcome: this action has no literal vector to promote.
		return len(removed), message + ": " + preview(removed), true, nil, nil
	}
	return 0, message, true, nil, nil
}

// preview names a bounded sample, because a log directory with ten thousand
// rotated files would otherwise put ten thousand paths into one message
// that gets written to an incident transcript.
func preview(files []string) string {
	const sample = 5
	if len(files) <= sample {
		return strings.Join(files, ", ")
	}
	return fmt.Sprintf("%s, … and %d more", strings.Join(files[:sample], ", "), len(files)-sample)
}

func toBool(raw any) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "1":
			return true, nil
		case "false", "no", "0", "":
			return false, nil
		}
		return false, fmt.Errorf("not a boolean: %q", v)
	default:
		return false, fmt.Errorf("expected a boolean, got %T", raw)
	}
}

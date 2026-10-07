package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

const psBusy = `  4211   1 root     98.5 12.0 runaway-worker
  4300   1 root      0.3  0.1 systemd
  4402 4211 ops       0.1  0.2 helper
`

func TestTopProcesses_KeepsOnlyTheProcessesActuallyUsingCPU(t *testing.T) {
	f := newFakeRunner()
	f.responses["ps -eo pid=,ppid=,user=,pcpu=,pmem=,comm="] = []byte(psBusy)
	rows, summary, err := newTestAdapter(f).busyProcesses(context.Background(), params{})
	if err != nil {
		t.Fatalf("busyProcesses: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want only the process above the floor: %#v", len(rows), rows)
	}
	row := rows[0]
	if row["pid"] != 4211 || row["comm"] != "runaway-worker" {
		t.Errorf("row = %#v, want pid 4211 runaway-worker", row)
	}
	if !strings.Contains(summary, "98") && !strings.Contains(summary, "CPU") {
		t.Errorf("summary should say what it found: %q", summary)
	}
}

// A host at 98% CPU with no single process above the floor is a different
// fault from one runaway process, and naming one would be a guess.
func TestTopProcesses_SaysSoWhenNoProcessIsTheCulprit(t *testing.T) {
	f := newFakeRunner()
	f.responses["ps -eo pid=,ppid=,user=,pcpu=,pmem=,comm="] = []byte(psBusy[:strings.Index(psBusy, "runaway")])
	rows, summary, err := newTestAdapter(f).busyProcesses(context.Background(), params{})
	if err != nil {
		t.Fatalf("busyProcesses: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %#v, want none", rows)
	}
	if !strings.Contains(summary, "no process") {
		t.Errorf("the summary must say the load is not one process: %q", summary)
	}
}

func TestParsePS_SkipsUnparseableLinesRatherThanFailing(t *testing.T) {
	rows, err := parsePS("  1   0 root  0.0 0.0 init\nbroken line here\n  99   1 root 80.0 1.0 worker\n")
	if err != nil {
		t.Fatalf("parsePS: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the two parseable lines: %#v", len(rows), rows)
	}
}

func TestOldLogFiles_ListsBySizeAndTotalsWhatIsReclaimable(t *testing.T) {
	dir := t.TempDir()
	f := newFakeRunner()
	f.responses["find "+dir+" -xdev -type f -mtime +30 -printf %s\t%T@\t%p\n"] =
		[]byte("1024\t1700000000\t/var/log/old.log\n1048576\t1700000001\t/var/log/big.log\n")

	rows, summary, err := newTestAdapter(f).oldLogFiles(context.Background(), params{"path": dir})
	if err != nil {
		t.Fatalf("oldLogFiles: %v", err)
	}
	if len(rows) != 2 || rows[0]["path"] != "/var/log/big.log" {
		t.Fatalf("rows should be ordered by size: %#v", rows)
	}
	if !strings.Contains(summary, "2 file(s)") {
		t.Errorf("summary = %q", summary)
	}
}

// find -printf is GNU; a zero-day floor would ask for files written today,
// which is never what anyone means.
func TestOldLogFiles_RefusesAZeroDayFloor(t *testing.T) {
	_, _, err := newTestAdapter(newFakeRunner()).oldLogFiles(context.Background(),
		params{"path": t.TempDir(), "older_than_days": 0})
	if err == nil || !strings.Contains(err.Error(), "at least 1") {
		t.Fatalf("a zero-day floor must be refused, got %v", err)
	}
}

func TestKillProcess_SendsTERMAndReadsTheStateBack(t *testing.T) {
	f := newFakeRunner()
	f.queue["ps"] = [][]byte{
		[]byte("runaway-worker\n"), // before
		[]byte(""),                 // after: no longer listed
	}
	impacted, message, ok, _, err := newTestAdapter(f).killProcess(context.Background(), params{"pid": 4211})
	if err != nil {
		t.Fatalf("killProcess: %v", err)
	}
	if impacted != 1 || !ok {
		t.Errorf("impacted=%d ok=%v, want 1/true", impacted, ok)
	}
	if !strings.Contains(message, "SIGTERM") || !strings.Contains(message, "runaway-worker") {
		t.Errorf("message = %q", message)
	}
	if !f.called("kill -TERM 4211") {
		t.Error("the process must be signalled with TERM, not KILL")
	}
}

// kill(2) returning 0 means the signal was delivered, not that the process
// acted on it. Reporting success without looking back is the "it said it
// worked" failure this adapter exists to avoid.
func TestKillProcess_ReportsFailureWhenTheProcessSurvives(t *testing.T) {
	f := newFakeRunner()
	f.queue["ps"] = [][]byte{[]byte("runaway-worker\n"), []byte("runaway-worker\n")}
	_, message, ok, _, err := newTestAdapter(f).killProcess(context.Background(), params{"pid": 4211})
	if err != nil {
		t.Fatalf("killProcess: %v", err)
	}
	if ok {
		t.Error("a process that ignored SIGTERM did not complete the remediation")
	}
	if !strings.Contains(message, "still running") || !strings.Contains(message, "SIGKILL") {
		t.Errorf("the message must say it survived and what escalating would mean: %q", message)
	}
}

func TestKillProcess_RefusesProcessesItMustNotSignal(t *testing.T) {
	cases := []struct {
		name string
		pid  int
		want string
	}{
		{"init", 1, "not an ordinary process"},
		{"this adapter", os.Getpid(), "this adapter"},
		{"the parent", os.Getppid(), "parent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := newTestAdapter(newFakeRunner()).killProcess(context.Background(), params{"pid": tc.pid})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to refuse with %q", err, tc.want)
			}
		})
	}
}

func TestKillProcess_RefusesAKernelThread(t *testing.T) {
	f := newFakeRunner()
	f.responses["ps -p 2 -o comm="] = []byte("[kthreadd]\n")
	_, _, _, _, err := newTestAdapter(f).killProcess(context.Background(), params{"pid": 2})
	if err == nil || !strings.Contains(err.Error(), "kernel thread") {
		t.Fatalf("error = %v, want a kernel-thread refusal", err)
	}
	if f.called("kill -TERM") {
		t.Error("a refused process must never be signalled")
	}
}

func TestKillProcess_RequiresAPid(t *testing.T) {
	if _, _, _, _, err := newTestAdapter(newFakeRunner()).killProcess(context.Background(), params{}); err == nil {
		t.Fatal("killing a process must not proceed without a pid")
	}
}

func TestRemoveOldLogs_DryRunReportsWithoutDeleting(t *testing.T) {
	dir := t.TempDir()
	f := newFakeRunner()
	f.responses["find "+dir+" -xdev -type f -mtime +30 -printf %s\t%T@\t%p\n"] =
		[]byte("4096\t1700000000\t" + filepath.Join(dir, "old.log") + "\n")

	impacted, message, ok, _, err := newTestAdapter(f).removeOldLogs(context.Background(),
		params{"path": dir, "dry_run": true})
	if err != nil {
		t.Fatalf("removeOldLogs: %v", err)
	}
	if impacted != 1 || !ok {
		t.Errorf("impacted=%d ok=%v, want 1/true", impacted, ok)
	}
	if !strings.Contains(message, "dry run") {
		t.Errorf("a dry run must say so: %q", message)
	}
	if f.called("rm ") {
		t.Fatal("a dry run must not delete anything")
	}
}

func TestRemoveOldLogs_DeletesWhatTheCriteriaName(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "old.log")
	f := newFakeRunner()
	f.responses["find "+dir+" -xdev -type f -mtime +30 -printf %s\t%T@\t%p\n"] =
		[]byte("4096\t1700000000\t" + target + "\n")

	_, message, ok, _, err := newTestAdapter(f).removeOldLogs(context.Background(), params{"path": dir})
	if err != nil {
		t.Fatalf("removeOldLogs: %v", err)
	}
	if !ok {
		t.Errorf("message = %q", message)
	}
	if !f.called("rm -f -- " + target) {
		t.Errorf("the file the criteria named must be removed; calls: %v", f.calls)
	}
}

// The list is re-derived from the same criterion rather than taken from the
// caller, so a caller cannot turn this into `rm` with extra steps.
func TestRemoveOldLogs_NamesNothingItDidNotDerive(t *testing.T) {
	dir := t.TempDir()
	f := newFakeRunner()
	f.responses["find "+dir+" -xdev -type f -mtime +30 -printf %s\t%T@\t%p\n"] =
		[]byte("4096\t1700000000\t" + filepath.Join(dir, "old.log") + "\n")

	if _, _, _, _, err := newTestAdapter(f).removeOldLogs(context.Background(), params{
		"path": dir, "files": []string{"/etc/passwd"},
	}); err != nil {
		t.Fatalf("removeOldLogs: %v", err)
	}
	if f.called("rm -f -- /etc/passwd") {
		t.Fatal("a caller-supplied path must never be deleted")
	}
}

func TestRemoveOldLogs_RefusesSystemDirectories(t *testing.T) {
	for _, dir := range protectedPaths {
		if _, _, _, _, err := newTestAdapter(newFakeRunner()).removeOldLogs(context.Background(),
			params{"path": dir, "dry_run": true}); err == nil {
			t.Errorf("removing logs from %s must be refused", dir)
		}
	}
}

func TestRemoveOldLogs_RefusesAFileAndAMissingPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := newTestAdapter(newFakeRunner()).removeOldLogs(context.Background(),
		params{"path": file}); err == nil {
		t.Error("a single file is not what this tool removes")
	}
	if _, _, _, _, err := newTestAdapter(newFakeRunner()).removeOldLogs(context.Background(),
		params{"path": filepath.Join(dir, "nope")}); err == nil {
		t.Error("a missing directory must be refused, not silently treated as empty")
	}
}

func TestRemoveOldLogs_ReportsWhenThereIsNothingToRemove(t *testing.T) {
	dir := t.TempDir()
	f := newFakeRunner()
	f.responses["find "+dir+" -xdev -type f -mtime +30 -printf %s\t%T@\t%p\n"] = []byte("")
	impacted, message, ok, _, err := newTestAdapter(f).removeOldLogs(context.Background(), params{"path": dir})
	if err != nil {
		t.Fatalf("removeOldLogs: %v", err)
	}
	if impacted != 0 || !ok || !strings.Contains(message, "nothing to remove") {
		t.Errorf("impacted=%d ok=%v message=%q", impacted, ok, message)
	}
}

func TestOpRiskLevel_GradesTheNewWritesAboveTheRestart(t *testing.T) {
	a := New()
	for _, op := range []string{"kill_process", "remove_old_logs"} {
		if got := a.OpRiskLevel(op); got != adapter.RiskL3HardWrite {
			t.Errorf("OpRiskLevel(%s) = %v, want L3: neither is recoverable by retrying", op, got)
		}
	}
	if got := a.OpRiskLevel("restart_service"); got != adapter.RiskL2SoftWrite {
		t.Errorf("OpRiskLevel(restart_service) = %v, want L2", got)
	}
}

func TestTheNewWriteOpsAreNotApprovedByDefault(t *testing.T) {
	for _, op := range []string{"kill_process", "remove_old_logs"} {
		_, err := New().Execute(context.Background(), adapter.ExecOp{Operation: op})
		if !errors.Is(err, adapter.ErrApprovalRequired) {
			t.Errorf("%s must refuse without an approver, got %v", op, err)
		}
	}
}

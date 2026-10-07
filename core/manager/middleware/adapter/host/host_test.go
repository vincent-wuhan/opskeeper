package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// fakeRunner answers with scripted output and records the argv it was given.
//
// Recording the argv is the point. Every operation in this package is an
// argv vector rather than a shell string, and the property worth testing is
// that a hostile argument arrives as *one* argument — which is what stops a
// unit name from becoming a second command.
type fakeRunner struct {
	responses map[string][]byte
	queue     map[string][][]byte
	calls     [][]string
	fail      map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: map[string][]byte{}, queue: map[string][][]byte{}, fail: map[string]error{}}
}

func (f *fakeRunner) describe() string { return "fake" }

func (f *fakeRunner) run(_ context.Context, argv []string) ([]byte, error) {
	f.calls = append(f.calls, argv)
	key := strings.Join(argv, " ")
	if err, ok := f.fail[key]; ok {
		return []byte("boom"), err
	}
	if q, ok := f.queue[argv[0]]; ok && len(q) > 0 {
		out := q[0]
		f.queue[argv[0]] = q[1:]
		return out, nil
	}
	if out, ok := f.responses[key]; ok {
		return out, nil
	}
	if out, ok := f.responses[argv[0]]; ok {
		return out, nil
	}
	return []byte(""), nil
}

func (f *fakeRunner) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

func newTestAdapter(f *fakeRunner) *Adapter {
	a := NewWithRunner(f)
	a.lookupEnv = func(k string) (string, bool) {
		if k == allowlistEnv {
			return "orders-api.service, payments.service", true
		}
		return "", false
	}
	return a
}

const meminfoBefore = "MemTotal:       32768000 kB\nMemFree:         1024000 kB\nMemAvailable:    2048000 kB\nBuffers:          100000 kB\nCached:          8000000 kB\nSwapTotal:       8192000 kB\nSwapFree:        4096000 kB\n"
const meminfoAfter = "MemTotal:       32768000 kB\nMemFree:         1024000 kB\nMemAvailable:   14336000 kB\nBuffers:          100000 kB\nCached:           800000 kB\nSwapTotal:       8192000 kB\nSwapFree:        4096000 kB\n"

func TestAdapter_TypeAndUnconnected(t *testing.T) {
	if got := New().Type(); got != adapter.TypeHost {
		t.Errorf("Type() = %s, want host", got)
	}
	if _, err := New().Health(context.Background()); !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Health on a fresh adapter: %v, want ErrNotConnected", err)
	}
	if _, err := New().Execute(context.Background(), adapter.ExecOp{Operation: "restart_service"}); !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Errorf("an unapproved restart must be refused before the connection is considered, got %v", err)
	}
}

func TestNewRunner_DSNForms(t *testing.T) {
	r, err := newRunner("local://", time.Second)
	if err != nil || r.describe() != "local" {
		t.Fatalf("local:// → %v, %v", r, err)
	}
	r, err = newRunner("ssh://ops@10.0.0.5:2222?key=/etc/ops.key", time.Second)
	if err != nil {
		t.Fatalf("ssh DSN: %v", err)
	}
	ssh, ok := r.(sshRunner)
	if !ok || ssh.user != "ops" || ssh.host != "10.0.0.5:2222" || ssh.keyPath != "/etc/ops.key" {
		t.Fatalf("ssh runner = %+v", r)
	}
	if _, err := newRunner("ssh://10.0.0.5", time.Second); err == nil {
		t.Error("an ssh DSN with no user must be refused")
	}
	if _, err := newRunner("http://example.com", time.Second); err == nil {
		t.Error("an unsupported DSN form must be refused")
	}
}

func TestRegisterTools_ExposesLoopActionsAndTheirArguments(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	spec, ok := reg.LookupTool("host.garbage_collect")
	if !ok {
		t.Fatal("host.garbage_collect is not registered")
	}
	// The safe, auto-approved host action must be dispatchable from a
	// RemediationOption, which carries no arguments at all.
	if len(spec.RequiredArgs) != 0 {
		t.Errorf("host.garbage_collect requires %v; the loop runs it unattended and must be able to", spec.RequiredArgs)
	}
	spec, ok = reg.LookupTool("host.restart_service")
	if !ok {
		t.Fatal("host.restart_service is not registered")
	}
	if strings.Join(spec.RequiredArgs, ",") != "unit" {
		t.Errorf("host.restart_service RequiredArgs = %v, want unit", spec.RequiredArgs)
	}
	if _, err := reg.CallTool(context.Background(), "host.restart_service", map[string]any{"unit": "x.service"}); !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Errorf("got %v, want ErrApprovalRequired", err)
	}
}

func TestValidateUnit_RefusesAnythingThatCouldBecomeAFlag(t *testing.T) {
	for _, ok := range []string{"orders-api.service", "nginx.service", "db@replica-1.service", "a_b.service"} {
		if err := validateUnit(ok); err != nil {
			t.Errorf("validateUnit(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"orders-api", "orders api.service", "--now.service", "-x.service", "a/b.service", "x.service --now", "; rm -rf /.service"} {
		if err := validateUnit(bad); err == nil {
			t.Errorf("validateUnit(%q) accepted a name that must be refused", bad)
		}
	}
}

func TestRestartService_RefusesWithoutAnAllowlist(t *testing.T) {
	a := NewWithRunner(newFakeRunner())
	a.lookupEnv = func(string) (string, bool) { return "", false }
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "restart_service",
		Params:     map[string]any{"unit": "orders-api.service"},
		ApprovedBy: "op-7",
	})
	if err == nil || !strings.Contains(err.Error(), allowlistEnv) {
		t.Errorf("got %v, want a refusal naming the missing allowlist", err)
	}
}

func TestRestartService_RefusesAUnitOutsideTheAllowlist(t *testing.T) {
	a := newTestAdapter(newFakeRunner())
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "restart_service",
		Params:     map[string]any{"unit": "sshd.service"},
		ApprovedBy: "op-7",
	})
	if err == nil || !strings.Contains(err.Error(), "not on this node's restart allowlist") {
		t.Errorf("got %v, want a refusal listing the allowlist", err)
	}
}

func TestRestartService_ConfirmsTheUnitCameBack(t *testing.T) {
	f := newFakeRunner()
	f.responses["systemctl show -p LoadState -p ActiveState -p SubState -p ExecMainPID -p NRestarts -p ActiveEnterTimestamp orders-api.service"] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nExecMainPID=4242\nNRestarts=1\nActiveEnterTimestamp=Mon 2026-09-30 10:00:00 CST\n")
	f.responses["systemctl restart orders-api.service"] = []byte("")
	a := newTestAdapter(f)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "restart_service",
		Params:     map[string]any{"unit": "orders-api.service"},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success || res.Impacted != 1 {
		t.Errorf("result = %+v, want success", res)
	}
	if !f.called("systemctl restart orders-api.service") {
		t.Error("the restart was never issued")
	}
	// The result must carry the literal vector that ran. The crystalliser
	// promotes a runbook by re-running exactly this vector; a result that
	// only described the outcome left the platform unable to write one.
	if strings.Join(res.Argv, " ") != "systemctl restart orders-api.service" {
		t.Errorf("res.Argv = %v, want the executed restart vector", res.Argv)
	}
	if !strings.Contains(res.Message, "now active") {
		t.Errorf("message = %q, want the confirmed state", res.Message)
	}
}

// TestGarbageCollect_CarriesTheArgvThatChangedState keeps the capture honest
// for the one write the loop runs unattended: the vector is the sysctl, not
// the sync that merely flushed pages ahead of it.
func TestGarbageCollect_CarriesTheArgvThatChangedState(t *testing.T) {
	f := newFakeRunner()
	f.queue["cat"] = [][]byte{[]byte(meminfoBefore), []byte(meminfoAfter)}
	a := newTestAdapter(f)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "garbage_collect",
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Join(res.Argv, " ") != "sysctl -w vm.drop_caches=3" {
		t.Errorf("res.Argv = %v, want the sysctl vector", res.Argv)
	}
}

func TestRestartService_ReportsAUnitThatDidNotComeBack(t *testing.T) {
	f := newFakeRunner()
	// systemctl restart exits 0 for a unit that starts and immediately
	// crashes, so the state afterwards is the only verdict that matters.
	f.responses["systemctl show -p LoadState -p ActiveState -p SubState -p ExecMainPID -p NRestarts -p ActiveEnterTimestamp orders-api.service"] = []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\nNRestarts=2\n")
	f.responses["systemctl restart orders-api.service"] = []byte("")
	a := newTestAdapter(f)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "restart_service",
		Params:     map[string]any{"unit": "orders-api.service"},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Success {
		t.Error("a unit that did not come back must not be reported as a success")
	}
	if !strings.Contains(res.Message, "failed/failed") {
		t.Errorf("message = %q, want the observed state", res.Message)
	}
}

func TestRestartService_RefusesAUnitThatDoesNotExistOnTheNode(t *testing.T) {
	f := newFakeRunner()
	f.responses["systemctl show -p LoadState -p ActiveState -p SubState -p ExecMainPID -p NRestarts -p ActiveEnterTimestamp orders-api.service"] = []byte("LoadState=not-found\nActiveState=inactive\nSubState=dead\n")
	a := newTestAdapter(f)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "restart_service",
		Params:     map[string]any{"unit": "orders-api.service"},
		ApprovedBy: "op-7",
	})
	if err == nil || !strings.Contains(err.Error(), "not-found") {
		t.Errorf("got %v, want a refusal naming LoadState", err)
	}
	if f.called("systemctl restart") {
		t.Error("a unit that does not exist must not be restarted")
	}
}

func TestGarbageCollect_ReportsWhatItReclaimed(t *testing.T) {
	f := newFakeRunner()
	f.queue["cat"] = [][]byte{[]byte(meminfoBefore), []byte(meminfoAfter)}
	f.responses["sync"] = []byte("")
	f.responses["sysctl -w vm.drop_caches=3"] = []byte("vm.drop_caches = 3\n")
	a := newTestAdapter(f)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "garbage_collect",
		ApprovedBy: "policy:auto",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success {
		t.Fatalf("result = %+v, want success", res)
	}
	if res.Impacted != 12000 { // (14336000-2048000) KiB = 12000 MiB
		t.Errorf("Impacted = %d MiB, want the reclaimed size", res.Impacted)
	}
	if !strings.Contains(res.Message, "No process was touched") {
		t.Errorf("message = %q, want it to say what was not done", res.Message)
	}
	if !f.called("sync") {
		t.Error("sync must run before dropping caches, or dirty pages are lost")
	}
}

func TestGarbageCollect_RefusesAnUndefinedLevel(t *testing.T) {
	a := newTestAdapter(newFakeRunner())
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "garbage_collect",
		Params:     map[string]any{"level": 9},
		ApprovedBy: "policy:auto",
	})
	if err == nil || !strings.Contains(err.Error(), "level must be") {
		t.Errorf("got %v, want a refusal naming the accepted levels", err)
	}
}

func TestGarbageCollect_SaysSoWhenNothingChanged(t *testing.T) {
	f := newFakeRunner()
	f.queue["cat"] = [][]byte{[]byte(meminfoAfter), []byte(meminfoAfter)}
	f.responses["sync"] = []byte("")
	f.responses["sysctl -w vm.drop_caches=3"] = []byte("vm.drop_caches = 3\n")
	a := newTestAdapter(f)
	res, err := a.Execute(context.Background(), adapter.ExecOp{Operation: "garbage_collect", ApprovedBy: "policy:auto"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Impacted != 0 {
		t.Errorf("Impacted = %d, want 0", res.Impacted)
	}
	if !strings.Contains(res.Message, "did not change") {
		t.Errorf("message = %q, want a zero delta stated plainly", res.Message)
	}
}

func TestMemInfo_ParsesKibibytesIntoBytes(t *testing.T) {
	f := newFakeRunner()
	f.responses["cat /proc/meminfo"] = []byte(meminfoBefore)
	a := newTestAdapter(f)
	rows, summary, err := a.memInfo(context.Background())
	if err != nil {
		t.Fatalf("memInfo: %v", err)
	}
	row := rows[0]
	if row["mem_total_bytes"] != int64(32768000)*1024 {
		t.Errorf("mem_total_bytes = %v, want the KiB value scaled", row["mem_total_bytes"])
	}
	if row["mem_total_human"] != "31.2Gi" {
		t.Errorf("mem_total_human = %v", row["mem_total_human"])
	}
	if !strings.Contains(summary, "available") {
		t.Errorf("summary = %q", summary)
	}
}

func TestLoadAverage_DividesByTheCpuCount(t *testing.T) {
	f := newFakeRunner()
	f.responses["cat /proc/loadavg"] = []byte("8.00 4.00 2.00 3/1234 5678\n")
	f.responses["nproc"] = []byte("4\n")
	a := newTestAdapter(f)
	rows, summary, err := a.loadAverage(context.Background())
	if err != nil {
		t.Fatalf("loadAverage: %v", err)
	}
	if rows[0]["cpus"] != 4 || rows[0]["load1_per_cpu_pct"] != 200.0 {
		t.Errorf("row = %v, want 200%% of one core per cpu", rows[0])
	}
	if !strings.Contains(summary, "over 4 cpu(s)") {
		t.Errorf("summary = %q", summary)
	}
}

func TestDf_ParsesPosixOutput(t *testing.T) {
	out := "Filesystem     1024-blocks      Used Available Capacity Mounted on\n" +
		"/dev/nvme0n1p1   104857600  99614720   5242880      95% /\n" +
		"tmpfs              8192000         0   8192000       0% /dev/shm\n"
	f := newFakeRunner()
	f.responses["df -Pk /"] = []byte(out)
	a := newTestAdapter(f)
	rows, summary, err := a.diskUsage(context.Background(), params{})
	if err != nil {
		t.Fatalf("diskUsage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0]["mount"] != "/" || rows[0]["use_pct"] != 95.0 {
		t.Errorf("first row = %v, want the busiest filesystem first", rows[0])
	}
	if rows[0]["available_bytes"] != int64(5242880)*1024 {
		t.Errorf("available_bytes = %v", rows[0]["available_bytes"])
	}
	if !strings.Contains(summary, "1 at or above 80%") {
		t.Errorf("summary = %q", summary)
	}
}

func TestSSHRunner_BatchesAndSeparatesTheCommand(t *testing.T) {
	r := sshRunner{user: "ops", host: "h:22", keyPath: "/k", timeout: time.Second}
	args := r.sshArgs([]string{"systemctl", "restart", "--now.service"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-o BatchMode=yes", "-i /k", "ops@h:22 -- systemctl restart --now.service"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh argv = %q, want it to contain %q", joined, want)
		}
	}
	// `--` must come before the remote command, or a command whose first
	// word starts with a dash is parsed by ssh as an option.
	if strings.Index(joined, "--") > strings.Index(joined, "systemctl") {
		t.Errorf("ssh argv = %q, want the command after --", joined)
	}
	_ = fmt.Sprint(args)
}

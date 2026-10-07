package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/autonomy"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
)

// This file is the evidence that the node's self-heal stack is wired.
//
// The autonomy package is thoroughly tested on its own, and every one of
// those tests constructs its own arbiter. That is the right way to test a
// decision, and it leaves a specific hole: nothing proves that the thing the
// composition root builds is the thing the arbiter package documents, that
// the tool a model can reach is the tool the arbiter answers, and that the
// argv which reaches the runner is the argv a human signed for. Those are
// three separate wirings, and a test of any one component passes when any
// of them is wrong.
//
// So this drives it the way a node does: build the stack from a manifest,
// then call the registered tool with the arguments a model is able to
// produce, and check what actually ran.

// autonomyFixture is the declaration under test. It is the same shape the
// loader accepts, written out so that the argv the test expects to see
// executed is visible in the same file as the test that checks it.
var autonomyDeclaredArgv = []string{"systemctl", "restart", "orders-api"}

func autonomyPlugin() pluginmanifest.Plugin {
	return pluginmanifest.Plugin{
		Root: "/nonexistent",
		Manifest: domain.PluginManifest{
			APIVersion: "opskeeper.io/v1",
			Kind:       "Plugin",
			Metadata:   domain.PluginMeta{Name: "opskeeper-sre-autonomy", Version: "0.1.0"},
			Spec: domain.PluginSpec{
				Targets:     domain.Targets{domain.TargetEdge},
				SafetyLevel: domain.SafetyL2,
				// The action names host_restart_service; the *tool a
				// model calls* to ask for the action is host_autonomy_run,
				// and it has to be in the same inventory or the node's
				// allow-list refuses the call before the arbiter is ever
				// consulted. It is declared at the class the skill really
				// is, because the gate compares the declaration against the
				// call site's independent assessment and refuses a package
				// that understates its own tool.
				Tools: domain.Tools{
					{Name: "host_restart_service", Class: domain.ClassWrite},
					{Name: builtin.ToolKey, Class: domain.ClassDestructive},
				},
				Autonomy: domain.AutonomyPolicy{
					OfflineAfter: domain.Duration(2 * time.Minute),
					Actions: []domain.AutonomyAction{{
						Name:           "restart-orders-on-disk-full",
						Tool:           "host_restart_service",
						Trigger:        domain.AutonomyTrigger{Kind: domain.TriggerMetricAbove, Metric: "node_disk_used_ratio", Threshold: 0.92},
						Argv:           autonomyDeclaredArgv,
						BlastRadius:    domain.RadiusPod,
						TTL:            domain.Duration(30 * time.Minute),
						IdempotencyKey: "restart-orders:{{target}}:{{window}}",
					}},
				},
			},
		},
	}
}

// fakeObservations is a node whose control plane and metrics are whatever the
// test says they are. It is the smallest honest stand-in for the heartbeat
// and the collector: two facts, both of which a real node already records.
type fakeObservations struct {
	mu       sync.Mutex
	online   bool
	since    time.Time
	values   map[string]float64
	linkCall int
	edgeID   uint64
}

// EdgeID is what register_edge established. Tests that do not set it get
// zero, which is the honest value for a node that has not registered: the
// center answers "took none of it" and the pump keeps the rows.
func (f *fakeObservations) EdgeID() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.edgeID
}

func (f *fakeObservations) LinkReach() (bool, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linkCall++
	return f.online, f.since
}

func (f *fakeObservations) MetricValue(name string) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[name]
	return v, ok
}

func (f *fakeObservations) goOffline(since time.Time) {
	f.mu.Lock()
	f.online = false
	f.since = since
	f.mu.Unlock()
}

func (f *fakeObservations) set(name string, value float64) {
	f.mu.Lock()
	f.values[name] = value
	f.mu.Unlock()
}

// recordingRunner stands in for the node's sandbox. It records the argv it
// was handed rather than executing anything, because the property under test
// is *which* argv arrives here — the sandbox's own allow-list has its own
// tests and running a real systemctl on a developer machine is not one of
// them.
type recordingRunner struct {
	mu   sync.Mutex
	argv [][]string
}

func (r *recordingRunner) RunArgv(_ context.Context, argv []string) (autonomy.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argv = append(r.argv, append([]string(nil), argv...))
	return autonomy.Outcome{ExitCode: 0, Stdout: "restarted"}, nil
}

func (r *recordingRunner) runs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.argv...)
}

// newAutonomyStack builds the node's stack the way main does, and registers
// the tool, and puts both back afterwards. The cleanup is not tidiness: the
// runner is process-global, and a test that left it installed would hand the
// next test in this package a node with autonomy it never asked for.
func newAutonomyStack(t *testing.T, plugins []pluginmanifest.Plugin, obs autonomyObservations, runner autonomy.Runner) *autonomyStack {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stack, err := buildAutonomy(context.Background(), &fakeTunnel{}, plugins, obs, runner, t.TempDir(), log)
	if err != nil {
		t.Fatalf("buildAutonomy: %v", err)
	}
	t.Cleanup(func() {
		builtin.SetAutonomyRunner(nil)
		if stack != nil {
			_ = stack.spool.Close()
		}
	})
	return stack
}

// callAutonomyRun invokes the tool exactly the way the agent runtime does:
// through the registry, with JSON, as a model would.
func callAutonomyRun(t *testing.T, action, target, window string) builtin.AutonomyOutcome {
	t.Helper()
	raw, err := builtin.AutonomyRun{}.Execute(context.Background(), json.RawMessage(
		`{"action":`+mustJSON(t, action)+`,"target":`+mustJSON(t, target)+`,"window":`+mustJSON(t, window)+`}`))
	if err != nil {
		t.Fatalf("host_autonomy_run: %v", err)
	}
	var out builtin.AutonomyOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding the tool's answer %s: %v", raw, err)
	}
	return out
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshalling %q: %v", s, err)
	}
	return string(b)
}

// TestAutonomyToolDefersWhileCenterIsReachable is the regression that matters
// most, because it is the one whose failure is silent: a node that acts
// while a human is present does not look broken, it looks helpful, right up
// until the first time it is not.
func TestAutonomyToolDefersWhileCenterIsReachable(t *testing.T) {
	obs := &fakeObservations{online: true, values: map[string]float64{"node_disk_used_ratio": 0.99}}
	runner := &recordingRunner{}
	stack := newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)
	if stack == nil {
		t.Fatal("a package with an autonomy block produced no stack")
	}

	out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1")
	if out.Verdict != autonomy.Defer.String() {
		t.Fatalf("verdict = %q, want %q: autonomy must not be in force while the control plane answers", out.Verdict, autonomy.Defer.String())
	}
	if out.Ran {
		t.Fatal("the action ran with the control plane online")
	}
	if runs := runner.runs(); len(runs) != 0 {
		t.Fatalf("the runner was handed %d argv vectors, want none: %v", len(runs), runs)
	}
}

// TestAutonomyToolRefusesUntilTriggerFires is the second half of the same
// property: being alone is necessary and not sufficient. A node on a network
// with nothing wrong must not self-heal, and — this is the part worth
// asserting — it must *refuse* rather than defer. Deferring would put a
// restart of a healthy service in front of a human, and the humans who would
// learn to click through it are the reason the next outage is not the last.
func TestAutonomyToolRefusesUntilTriggerFires(t *testing.T) {
	obs := &fakeObservations{online: false, since: time.Now().Add(-5 * time.Minute), values: map[string]float64{"node_disk_used_ratio": 0.10}}
	runner := &recordingRunner{}
	newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)

	out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1")
	if out.Verdict != autonomy.Refuse.String() {
		t.Fatalf("verdict = %q (%s), want %q: the trigger has not fired", out.Verdict, out.Reason, autonomy.Refuse.String())
	}
	if !strings.Contains(out.Reason, "node_disk_used_ratio") {
		t.Fatalf("reason = %q, want it to name the metric that was measured", out.Reason)
	}
	if runs := runner.runs(); len(runs) != 0 {
		t.Fatalf("the runner ran with the metric at 0.10: %v", runs)
	}
}

// TestAutonomyToolRefusesWhenTheNodeCannotMeasure is the fail-closed case,
// and it is the one that separates a trigger from a comment. A node whose
// metric pipeline is dead has no reading, which is not a reading of zero,
// and "we cannot tell" must not read as "nothing is wrong".
func TestAutonomyToolRefusesWhenTheNodeCannotMeasure(t *testing.T) {
	obs := &fakeObservations{online: false, since: time.Now().Add(-5 * time.Minute), values: map[string]float64{}}
	runner := &recordingRunner{}
	newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)

	out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1")
	if out.Verdict != autonomy.Refuse.String() {
		t.Fatalf("verdict = %q (%s), want %q: no measurement, no action", out.Verdict, out.Reason, autonomy.Refuse.String())
	}
	if runs := runner.runs(); len(runs) != 0 {
		t.Fatalf("the runner ran with no metric to justify it: %v", runs)
	}
}

// TestAutonomyToolDefersUntilCenterIsLongEnoughGone covers the threshold that
// keeps a reconnect blip from being an outage.
func TestAutonomyToolDefersUntilCenterIsLongEnoughGone(t *testing.T) {
	obs := &fakeObservations{
		online: false,
		// One second is a socket, not an incident, and a node that treated
		// it as an outage would run its whole list on every reconnect.
		since:  time.Now().Add(-1 * time.Second),
		values: map[string]float64{"node_disk_used_ratio": 0.99},
	}
	runner := &recordingRunner{}
	newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)

	out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1")
	if out.Verdict != autonomy.Defer.String() {
		t.Fatalf("verdict = %q after a one-second blip, want %q", out.Verdict, autonomy.Defer.String())
	}
}

// TestAutonomyToolRunsDeclaredArgvWhenAlone is the positive case, and the
// assertion is the one that cannot be faked: the runner must receive the
// declared vector, element for element. A model has no way to express any
// other command through this tool, and this is where that becomes true rather
// than merely intended.
func TestAutonomyToolRunsDeclaredArgvWhenAlone(t *testing.T) {
	obs := &fakeObservations{online: false, since: time.Now().Add(-5 * time.Minute), values: map[string]float64{}}
	runner := &recordingRunner{}
	stack := newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)

	// The trigger fires only on a reading the collector actually took.
	obs.set("node_disk_used_ratio", 0.97)

	out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1")
	if out.Verdict != autonomy.Run.String() {
		t.Fatalf("verdict = %q (%s), want %q: center gone, disk at 0.97, action declared and unspent",
			out.Verdict, out.Reason, autonomy.Run.String())
	}
	if !out.Ran {
		t.Fatal("ran = false on a verdict of run")
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", out.ExitCode)
	}
	runs := runner.runs()
	if len(runs) != 1 {
		t.Fatalf("the runner ran %d commands, want exactly 1: %v", len(runs), runs)
	}
	if len(runs[0]) != len(autonomyDeclaredArgv) {
		t.Fatalf("argv = %q, want %q", runs[0], autonomyDeclaredArgv)
	}
	for i := range autonomyDeclaredArgv {
		if runs[0][i] != autonomyDeclaredArgv[i] {
			t.Fatalf("argv[%d] = %q, want %q (the whole vector came from the manifest, not from the caller)", i, runs[0][i], autonomyDeclaredArgv[i])
		}
	}
	if h := stack.Health(); h.Run != 1 || h.Deferred != 0 || h.Refused != 0 {
		t.Fatalf("health = %+v, want run=1 deferred=0 refused=0", h)
	}
}

// TestAutonomyToolDefersUndeclaredActionToTheGate is the plan's own test
// line — "自治动作逃逸" — and its answer is defer rather than refuse, which
// is the subtle part. A name nobody declared is not a request to act
// without a human, it is a call the arbiter has no opinion about, so it
// goes where every other call goes. Refusing it would be defensible too, and
// would also be wrong: the gate is the authority for undeclared work, and a
// mutating call still gets a person.
func TestAutonomyToolDefersUndeclaredActionToTheGate(t *testing.T) {
	obs := &fakeObservations{online: false, since: time.Now().Add(-5 * time.Minute), values: map[string]float64{"node_disk_used_ratio": 0.97}}
	runner := &recordingRunner{}
	newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)

	out := callAutonomyRun(t, "reboot-the-node", "orders-api", "win-1")
	if out.Verdict != autonomy.Defer.String() {
		t.Fatalf("verdict = %q (%s), want %q: no package declared a reboot",
			out.Verdict, out.Reason, autonomy.Defer.String())
	}
	if out.Ran {
		t.Fatal("an undeclared action ran")
	}
	if runs := runner.runs(); len(runs) != 0 {
		t.Fatalf("an undeclared action reached the runner: %v", runs)
	}
}

// TestAutonomyToolRefusesReplayOfSpentKey is the reconnect case. A tunnel
// that came back may redeliver the self-heal it asked for just before it
// dropped, and a node that ran it twice would be restarting a service that
// has already been restarted, on the way back up, with nobody watching.
func TestAutonomyToolRefusesReplayOfSpentKey(t *testing.T) {
	obs := &fakeObservations{online: false, since: time.Now().Add(-5 * time.Minute), values: map[string]float64{"node_disk_used_ratio": 0.97}}
	runner := &recordingRunner{}
	newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)

	if first := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1"); first.Verdict != autonomy.Run.String() {
		t.Fatalf("the first claim was %q (%s), want run", first.Verdict, first.Reason)
	}
	second := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1")
	if second.Verdict != autonomy.Refuse.String() {
		t.Fatalf("the replay was %q (%s), want %q", second.Verdict, second.Reason, autonomy.Refuse.String())
	}
	if second.Ran {
		t.Fatal("the replay ran")
	}
	if runs := runner.runs(); len(runs) != 1 {
		t.Fatalf("the runner ran %d commands across a replay, want 1: %v", len(runs), runs)
	}
}

// TestAutonomyToolCannotExpressACommand closes the door the tool's shape
// leaves open in the other direction. A model that has decided to run
// something gets one string back and no process: the name is looked up, not
// parsed, so a command line has nowhere to go but the gate.
func TestAutonomyToolCannotExpressACommand(t *testing.T) {
	obs := &fakeObservations{online: false, since: time.Now().Add(-5 * time.Minute), values: map[string]float64{"node_disk_used_ratio": 0.97}}
	runner := &recordingRunner{}
	newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)

	for _, attempt := range []string{"rm -rf /", "systemctl restart orders-api", "; reboot"} {
		out := callAutonomyRun(t, attempt, "orders-api", "win-1")
		if out.Verdict == autonomy.Run.String() {
			t.Fatalf("%q was accepted as an action name and ran", attempt)
		}
		if out.Ran {
			t.Fatalf("%q reported a run", attempt)
		}
	}
	if runs := runner.runs(); len(runs) != 0 {
		t.Fatalf("a command line reached the runner: %v", runs)
	}
}

// TestAutonomySpoolRecordsBothPhases is the audit half. A node acting alone
// is exactly the situation where the record cannot be reconstructed later,
// and a record that only holds the verdict leaves no way to tell a refused
// action from one that was never tried.
func TestAutonomySpoolRecordsBothPhases(t *testing.T) {
	obs := &fakeObservations{online: false, since: time.Now().Add(-5 * time.Minute), values: map[string]float64{"node_disk_used_ratio": 0.97}}
	runner := &recordingRunner{}
	stack := newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)

	if out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1"); out.Verdict != autonomy.Run.String() {
		t.Fatalf("verdict = %q, want run", out.Verdict)
	}
	if out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1"); out.Verdict != autonomy.Refuse.String() {
		t.Fatalf("verdict = %q, want refuse", out.Verdict)
	}

	rows, err := stack.spool.Replay(func(autonomy.Row) error { return nil })
	if err != nil {
		t.Fatalf("replaying the spool: %v", err)
	}
	// One run is two rows — allowed, then completed — and the refusal is
	// one. Anything else means a phase was lost, and a lost phase is a
	// hole in the audit chain rather than a cosmetic gap.
	if rows != 3 {
		t.Fatalf("the spool holds %d rows, want 3 (run=2 phases, refusal=1)", rows)
	}
	if fi, err := os.Stat(stack.spool.Path()); err != nil {
		t.Fatalf("the spool file: %v", err)
	} else if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("the spool is mode %o: a node's autonomous decisions must not be world-readable", perm)
	}
	h := stack.Health()
	if h.Spooled != 3 {
		t.Fatalf("health reports %d spooled rows, want 3", h.Spooled)
	}
}

// TestBuildAutonomyIsAbsentWhenNobodyDeclaredIt is the compatibility
// guarantee, stated as a test. Every package that ships today has no
// autonomy block, so the stack must be nil and the tool must refuse — and
// the refusal has to be a *refusal*, not a deferral, because a node with no
// autonomy has nothing to hand to the approval gate either.
func TestBuildAutonomyIsAbsentWhenNobodyDeclaredIt(t *testing.T) {
	plain := pluginmanifest.Plugin{
		Root: "/nonexistent",
		Manifest: domain.PluginManifest{
			APIVersion: "opskeeper.io/v1",
			Kind:       "Plugin",
			Metadata:   domain.PluginMeta{Name: "opskeeper-sre-readonly", Version: "0.1.0"},
			Spec: domain.PluginSpec{
				Targets:     domain.Targets{domain.TargetEdge},
				SafetyLevel: domain.SafetyL1,
				Tools:       domain.Tools{{Name: "host_disk_probe", Class: domain.ClassRead}},
			},
		},
	}
	obs := &fakeObservations{online: false, since: time.Now().Add(-time.Hour), values: map[string]float64{"node_disk_used_ratio": 0.99}}
	runner := &recordingRunner{}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stack, err := buildAutonomy(context.Background(), &fakeTunnel{}, []pluginmanifest.Plugin{plain}, obs, runner, t.TempDir(), log)
	if err != nil {
		t.Fatalf("buildAutonomy on a package with no autonomy block: %v", err)
	}
	if stack != nil {
		t.Fatalf("a package with no autonomy block produced a stack: %+v", stack.Health())
	}
	// buildAutonomy returned before touching the process-global runner, so
	// the tool still answers for a node that has nothing it may do alone.
	if _, err := os.Stat(filepath.Join(t.TempDir(), autonomySpoolFile)); !os.IsNotExist(err) {
		t.Fatal("a node with no autonomy opened a spool")
	}
	if out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1"); out.Verdict != autonomy.Refuse.String() {
		t.Fatalf("the tool answered %q on a node with no autonomy, want %q", out.Verdict, autonomy.Refuse.String())
	}
}

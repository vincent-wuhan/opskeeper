package pigsupervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// fastConfig keeps the backoff short so a restart test does not sleep. The
// backoff's growth is asserted directly on growBackoff instead, which tests
// the arithmetic rather than the passage of time.
func fastConfig(f Factory) Config {
	return Config{
		Factory:           f,
		RestartBackoff:    time.Millisecond,
		RestartBackoffMax: 5 * time.Millisecond,
		CrashWindow:       time.Second,
		MaxCrashAttempts:  3,
	}
}

// newTestSupervisor builds a supervisor and registers its shutdown so a
// failing test does not leave a restart loop running.
func newTestSupervisor(t *testing.T, cfg Config) *Supervisor {
	t.Helper()
	if cfg.Factory == nil {
		t.Fatal("newTestSupervisor: Factory is nil")
	}
	if cfg.RestartBackoff == 0 {
		cfg.RestartBackoff = time.Millisecond
		cfg.RestartBackoffMax = 5 * time.Millisecond
		cfg.CrashWindow = time.Second
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

// waitFor polls until cond holds, and fails with what it saw when it does
// not. A supervisor's work happens on its own goroutine, so every assertion
// about it is eventually-consistent rather than immediate.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNewRequiresAFactory(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("a supervisor with no Factory was accepted: it could only fail at the first turn")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	s, err := New(Config{Factory: func() ports.AgentProcess { return newFakeProcess() }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.cfg.RestartBackoff != DefaultRestartBackoff ||
		s.cfg.RestartBackoffMax != DefaultRestartBackoffMax ||
		s.cfg.CrashWindow != DefaultCrashWindow {
		t.Errorf("defaults not applied: %+v", s.cfg)
	}
	if s.cfg.Now == nil || s.log == nil {
		t.Error("New left a nil clock or logger: a nil clock panics on the first crash report")
	}
}

// --- happy path ---------------------------------------------------------

func TestStartBringsTheProcessUp(t *testing.T) {
	rec := &factoryRecorder{procs: []*fakeProcess{newFakeProcess()}}
	s := newTestSupervisor(t, fastConfig(rec.next))

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !s.Running() {
		t.Error("Running is false after a successful start")
	}
	if rec.count() != 1 {
		t.Errorf("factory called %d times, want 1", rec.count())
	}
}

func TestStartIsNotRepeatable(t *testing.T) {
	// A second Start would put two supervisors over one node, each
	// restarting the other's process.
	rec := &factoryRecorder{procs: []*fakeProcess{newFakeProcess()}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Error("a second Start was accepted")
	}
}

func TestStartFailsWhenTheAgentCannotStart(t *testing.T) {
	// The caller asked for a process and got none. Reporting that now is
	// the point: finding out from a health check ten minutes later is how
	// a node stays broken unnoticed.
	bad := newFakeProcess()
	bad.startErr = errors.New("no such file or directory")
	rec := &factoryRecorder{exhausted: bad}
	s := newTestSupervisor(t, fastConfig(rec.next))

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded against a binary that cannot be executed")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("err = %v, want it to carry the spawn failure", err)
	}
	if s.Running() {
		t.Error("Running is true after a failed start")
	}
	// A failed first start must not leave a restart loop churning.
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Errorf("factory called %d times after a failed start, want 1: a retry loop here would spin forever", rec.count())
	}
}

func TestProcessIsReachableWhileRunning(t *testing.T) {
	proc := newFakeProcess()
	rec := &factoryRecorder{procs: []*fakeProcess{proc}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	live, err := s.Process()
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := live.Prompt(context.Background(), "why is the pod crashlooping?"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if proc.promptCount() != 1 {
		t.Errorf("prompt count = %d, want 1", proc.promptCount())
	}
}

// --- restart ------------------------------------------------------------

func TestCrashIsRestarted(t *testing.T) {
	first := newFakeProcess()
	second := newFakeProcess()
	rec := &factoryRecorder{procs: []*fakeProcess{first, second}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first.Crash(errors.New("segmentation fault"))

	waitFor(t, "the agent to be restarted", func() bool { return s.Running() && rec.count() >= 2 })
	if second.wasStopped() {
		t.Error("the replacement was stopped: the supervisor is shutting down the process it just started")
	}
	if h := s.Health(); h.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1", h.Restarts)
	}
}

func TestStopIsNotTreatedAsACrash(t *testing.T) {
	// The single most important distinction in this file: an operator
	// stopping the agent must never be answered with a respawn.
	proc := newFakeProcess()
	rec := &factoryRecorder{procs: []*fakeProcess{proc}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !proc.wasStopped() {
		t.Error("Stop did not reach the process")
	}
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Errorf("factory called %d times, want 1: an operator's stop was answered with a restart", rec.count())
	}
}

func TestStopIsIdempotent(t *testing.T) {
	rec := &factoryRecorder{procs: []*fakeProcess{newFakeProcess()}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop %d: %v", i, err)
		}
	}
	if s.Running() {
		t.Error("Running is true after Stop")
	}
}

func TestOperationsAfterStopAreRefused(t *testing.T) {
	rec := &factoryRecorder{procs: []*fakeProcess{newFakeProcess()}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = s.Stop()

	if _, err := s.Process(); !errors.Is(err, ErrStopped) {
		t.Errorf("Process after Stop = %v, want %v", err, ErrStopped)
	}
	if err := s.Restart(); !errors.Is(err, ErrStopped) {
		t.Errorf("Restart after Stop = %v, want %v", err, ErrStopped)
	}
	// Start after Stop must not resurrect the node behind the operator's
	// back.
	if err := s.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Errorf("Start after Stop = %v, want %v", err, ErrStopped)
	}
}

func TestProcessIsRefusedWhileDown(t *testing.T) {
	proc := newFakeProcess()
	rec := &factoryRecorder{procs: []*fakeProcess{proc}}
	cfg := fastConfig(rec.next)
	cfg.MaxCrashAttempts = 1
	s := newTestSupervisor(t, cfg)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	proc.Crash(errors.New("boom"))
	waitFor(t, "the supervisor to give up", func() bool { return !s.Running() })

	// A caller must be told the process is gone, not handed a dead one.
	if _, err := s.Process(); err == nil {
		t.Error("Process returned a handle to a process that is not running")
	}
}

// --- crash-loop protection ----------------------------------------------

func TestCrashLoopDegradesInsteadOfSpinning(t *testing.T) {
	// An agent that dies on launch every time is the failure mode a naive
	// supervisor turns into a fork bomb. The supervisor must give up and
	// say so, while staying up to keep reporting.
	bad := newFakeProcess()
	bad.exitOnStart = true
	bad.exitErr = errors.New("cannot load plugin manifest")
	rec := &factoryRecorder{exhausted: bad}
	cfg := fastConfig(rec.next)
	cfg.MaxCrashAttempts = 3
	s := newTestSupervisor(t, cfg)

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the supervisor to give up restarting", func() bool { return s.Health().Degraded })

	h := s.Health()
	if h.Running {
		t.Error("Running is true on a degraded supervisor")
	}
	if !strings.Contains(h.LastError, "crashed") {
		t.Errorf("LastError = %q, want it to name the crash loop so the page explains itself", h.LastError)
	}
	if h.Restarts != 3 {
		t.Errorf("Restarts = %d, want 3: the limit is attempts, and the first start is not an attempt", h.Restarts)
	}

	// The give-up must be final, not a pause.
	before := rec.count()
	time.Sleep(80 * time.Millisecond)
	if rec.count() != before {
		t.Errorf("factory called %d more times after giving up: the supervisor kept respawning a process that cannot start", rec.count()-before)
	}
}

func TestCrashLoopKeepsTheSupervisorReporting(t *testing.T) {
	// A supervisor that exits takes its health endpoint with it. A node
	// that has vanished from the fleet is worse than one visibly broken.
	bad := newFakeProcess()
	bad.exitOnStart = true
	rec := &factoryRecorder{exhausted: bad}
	cfg := fastConfig(rec.next)
	cfg.MaxCrashAttempts = 2
	s := newTestSupervisor(t, cfg)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "degradation", func() bool { return s.Health().Degraded })

	// Health stays answerable, repeatedly.
	for i := 0; i < 3; i++ {
		if h := s.Health(); !h.Degraded {
			t.Fatalf("health call %d lost the degraded state", i)
		}
	}
	if err := s.Stop(); err != nil {
		t.Errorf("a degraded supervisor must still stop cleanly: %v", err)
	}
}

func TestMaxCrashAttemptsZeroRestartsForever(t *testing.T) {
	// Zero is a legitimate choice for a node whose agent is known-good
	// and whose failure mode is transient: giving up would be wrong there.
	rec := &factoryRecorder{}
	cfg := fastConfig(rec.next)
	cfg.MaxCrashAttempts = 0
	s := newTestSupervisor(t, cfg)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Crash whatever is live, repeatedly, past the point where a bounded
	// supervisor would have given up.
	//
	// Each round waits for the supervisor to be *running a process* rather
	// than for the factory to have been called. The count increments
	// before the replacement is installed, so a test that read the window
	// in between would see the process it had just crashed, crash it again
	// to no effect, and then wait forever for a restart nothing triggered.
	for i := 0; i < 6; i++ {
		var live *fakeProcess
		waitFor(t, "a live process", func() bool {
			p, err := s.Process()
			if err != nil {
				return false
			}
			fake, ok := p.(*fakeProcess)
			if !ok {
				return false
			}
			live = fake
			return true
		})
		live.Crash(errors.New("transient"))
		waitFor(t, "a replacement process", func() bool {
			p, err := s.Process()
			if err != nil {
				return false
			}
			fake, ok := p.(*fakeProcess)
			return ok && fake != live
		})
	}
	if rec.count() < 5 {
		t.Errorf("factory called %d times, want at least 5: a supervisor configured to restart forever gave up", rec.count())
	}
	if s.Health().Degraded {
		t.Error("a supervisor configured to restart forever reported degraded")
	}
}

func TestCrashWindowForgetsOldCrashes(t *testing.T) {
	// An agent that crashes occasionally is not a crash loop. The window
	// is what tells those two apart, so it has to actually expire.
	now := time.Now()
	window := 10 * time.Minute
	crashes := []time.Time{
		now.Add(-9 * time.Minute),  // inside
		now.Add(-11 * time.Minute), // outside
		now.Add(-1 * time.Minute),  // inside
	}
	kept := trimCrashes(crashes, now, window)
	if len(kept) != 2 {
		t.Fatalf("kept %d crashes, want 2", len(kept))
	}
	if !kept[0].After(now.Add(-window)) || !kept[1].After(now.Add(-window)) {
		t.Errorf("kept a crash outside the window: %v", kept)
	}
}

func TestGrowBackoffDoublesAndCaps(t *testing.T) {
	base, max := 2*time.Second, time.Minute
	got := growBackoff(0, base, max)
	if got != base {
		t.Errorf("first backoff = %s, want %s: a process that failed to start needs a moment before retrying", got, base)
	}
	prev := got
	for i := 0; i < 10; i++ {
		got = growBackoff(prev, base, max)
		if got < prev {
			t.Fatalf("backoff shrank from %s to %s", prev, got)
		}
		if got > max {
			t.Fatalf("backoff %s exceeded the cap %s", got, max)
		}
		prev = got
	}
	if prev != max {
		t.Errorf("backoff settled at %s, want the cap %s: a long-lived broken agent must not respawn every few minutes forever", prev, max)
	}
}

func TestGrowBackoffHonoursABaseAboveTheCap(t *testing.T) {
	// A misconfigured base above the cap would otherwise produce a first
	// delay larger than every later one.
	if got := growBackoff(0, 5*time.Minute, time.Minute); got != time.Minute {
		t.Errorf("first backoff = %s, want the cap", got)
	}
}

// --- health -------------------------------------------------------------

func TestHealthReportsALiveProcess(t *testing.T) {
	proc := newFakeProcess()
	proc.state = ports.ProcessState{SessionID: "s-1", Version: "0.3.0", Model: "gpt-5.6"}
	rec := &factoryRecorder{procs: []*fakeProcess{proc}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	h := s.Health()
	if !h.Running {
		t.Error("Running is false on a live agent")
	}
	// The version comes from the process, not from config: a node is only
	// as current as the binary it actually launched.
	if h.Version != "0.3.0" {
		t.Errorf("Version = %q, want the live process's 0.3.0", h.Version)
	}
	if h.LastStartAt.IsZero() {
		t.Error("LastStartAt is zero")
	}
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty on a healthy agent", h.LastError)
	}
}

func TestHealthRecordsTheExitReason(t *testing.T) {
	proc := newFakeProcess()
	rec := &factoryRecorder{procs: []*fakeProcess{proc, newFakeProcess()}}
	cfg := fastConfig(rec.next)
	cfg.MaxCrashAttempts = 1
	s := newTestSupervisor(t, cfg)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	proc.Crash(errors.New("out of memory"))
	waitFor(t, "the crash to be recorded", func() bool { return s.Health().LastExit != "" })
	if got := s.Health().LastExit; !strings.Contains(got, "out of memory") {
		t.Errorf("LastExit = %q, want the process's own reason: 'exit status 1' tells an operator nothing", got)
	}
}

func TestHealthDegradesToNoVersionWhenTheProcessCannotReportIt(t *testing.T) {
	// A process whose state call fails must not make the whole snapshot
	// fail: an operator still needs to be told the agent is up.
	proc := newFakeProcess()
	proc.stateErr = errors.New("pipe closed")
	rec := &factoryRecorder{procs: []*fakeProcess{proc}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := s.Health()
	if !h.Running {
		t.Error("Running is false: a failed state call must not make a live agent look dead")
	}
	if h.Version != "" {
		t.Errorf("Version = %q, want empty rather than a stale guess", h.Version)
	}
}

func TestHealthIsSafeBeforeStart(t *testing.T) {
	rec := &factoryRecorder{procs: []*fakeProcess{newFakeProcess()}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	h := s.Health()
	if h.Running || h.Degraded || h.Restarts != 0 {
		t.Errorf("health before start = %+v, want a clean idle report", h)
	}
}

func TestHealthIsSafeUnderConcurrency(t *testing.T) {
	// Health is read by the node's own telemetry loop while the supervisor
	// is restarting; the race detector is the only thing that can prove
	// this, which is why it is a test and not a comment.
	rec := &factoryRecorder{}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.Health()
					_ = s.Running()
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		// Crash whichever process is currently live, then ask for a
		// replacement, so liveness flips underneath the readers rather
		// than on a schedule the readers can predict.
		if live, err := s.Process(); err == nil {
			if fake, ok := live.(*fakeProcess); ok {
				fake.Crash(errors.New("churn"))
			}
		}
		_ = s.Restart()
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// --- manual restart -----------------------------------------------------

func TestRestartAbortsTheLiveProcess(t *testing.T) {
	proc := newFakeProcess()
	rec := &factoryRecorder{procs: []*fakeProcess{proc, newFakeProcess()}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := s.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if proc.aborts != 1 {
		t.Errorf("aborts = %d, want 1: a manual restart must actually end the wedged process", proc.aborts)
	}
	waitFor(t, "the replacement", func() bool { return rec.count() >= 2 })
}

func TestRestartDoesNotResetTheCrashBudget(t *testing.T) {
	// The escape hatch must not become the loop: a node an operator is
	// hand-restarting every minute should still trip the crash limit.
	rec := &factoryRecorder{}
	cfg := fastConfig(rec.next)
	cfg.MaxCrashAttempts = 3
	s := newTestSupervisor(t, cfg)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Hand-restarting is still crashing the process. Three of those must
	// trip the limit exactly as three unattended crashes would.
	//
	// Each round waits for a live process first. Restart on a process
	// that is already gone is a no-op by design - a respawn is already
	// pending, and queueing behind it is what turns a double click into a
	// crash nobody asked for. Waiting for a live process is what makes
	// this three restarts rather than three-and-sometimes-two.
	for i := 0; i < 3; i++ {
		if i > 0 {
			waitFor(t, "a live process", func() bool {
				_, err := s.Process()
				return err == nil
			})
		}
		_ = s.Restart()
	}
	waitFor(t, "degradation", func() bool { return s.Health().Degraded })
	if rec.count() > 3 {
		t.Errorf("factory called %d times, want at most 3: a hand-restart must not buy the agent a fresh crash allowance", rec.count())
	}
}

func TestRestartIsSafeToCallRepeatedly(t *testing.T) {
	// Clicking restart twice must not queue two respawns.
	proc := newFakeProcess()
	rec := &factoryRecorder{procs: []*fakeProcess{proc, newFakeProcess()}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Restart(); err != nil {
			t.Fatalf("Restart %d: %v", i, err)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if n := rec.count(); n > 3 {
		t.Errorf("factory called %d times for five restart clicks: a double click must not become a double respawn", n)
	}
}

func TestRestartBeforeStartIsRefused(t *testing.T) {
	rec := &factoryRecorder{procs: []*fakeProcess{newFakeProcess()}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Restart(); err == nil {
		t.Error("Restart before Start succeeded: there is no process to replace")
	}
}

// --- events -------------------------------------------------------------

func TestOnEventReachesTheProcessListener(t *testing.T) {
	proc := newFakeProcess()
	rec := &factoryRecorder{procs: []*fakeProcess{proc}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var mu sync.Mutex
	var got []ports.ProcessEvent
	unsubscribe := s.OnEvent(func(ev ports.ProcessEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	defer unsubscribe()

	proc.emit(ports.ProcessEvent{Type: "assistant_delta", Seq: 1})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Type != "assistant_delta" {
		t.Errorf("listener saw %+v, want the emitted frame", got)
	}
}

func TestOnEventBeforeStartIsSafe(t *testing.T) {
	rec := &factoryRecorder{procs: []*fakeProcess{newFakeProcess()}}
	s := newTestSupervisor(t, fastConfig(rec.next))
	// A console that connects before the agent is up must not panic; it
	// gets a no-op unsubscribe and connects again once the node is live.
	if unsubscribe := s.OnEvent(func(ports.ProcessEvent) {}); unsubscribe == nil {
		t.Error("OnEvent returned a nil unsubscribe function")
	} else {
		unsubscribe()
	}
}

func TestASubscriptionSurvivesARestart(t *testing.T) {
	// The node's bridge subscribes once, before anything is running, and
	// keeps relaying. If the subscription lived on the process, every
	// crash would silently end the console's view of a turn with no error
	// anywhere: the bridge would still hold an unsubscribe function for a
	// process nobody was watching.
	rec := &factoryRecorder{}
	s := newTestSupervisor(t, fastConfig(rec.next))

	seen := make(chan string, 8)
	s.OnEvent(func(ev ports.ProcessEvent) {
		select {
		case seen <- ev.SessionID:
		default:
		}
	})

	// Subscribing before the first start is the ordinary case: main builds
	// the bridge and the supervisor, and neither has a process yet.
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first, err := s.Process()
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	first.(*fakeProcess).emit(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1"})
	if got := drainEvent(seen); got != "s-1" {
		t.Fatalf("event before the crash = %q, want s-1", got)
	}

	// Now restart underneath the subscription.
	first.(*fakeProcess).Crash(errors.New("crash"))
	var second ports.AgentProcess
	waitFor(t, "a replacement process", func() bool {
		p, err := s.Process()
		if err != nil || p == first {
			return false
		}
		second = p
		return true
	})
	second.(*fakeProcess).emit(ports.ProcessEvent{Type: "turn_start", SessionID: "s-2"})
	if got := drainEvent(seen); got != "s-2" {
		t.Errorf("event after the crash = %q, want s-2: the subscription did not survive the restart", got)
	}
}

func TestAnUnsubscribedListenerStopsAfterARestart(t *testing.T) {
	// The other half of the contract: cancelling must actually cancel.
	// A console that closed its conversation and then received the rest of
	// a turn would be looking at a conversation nobody asked for.
	rec := &factoryRecorder{}
	s := newTestSupervisor(t, fastConfig(rec.next))

	seen := make(chan string, 8)
	unsubscribe := s.OnEvent(func(ev ports.ProcessEvent) {
		select {
		case seen <- ev.SessionID:
		default:
		}
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	unsubscribe()

	first, err := s.Process()
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	first.(*fakeProcess).emit(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1"})
	first.(*fakeProcess).Crash(errors.New("crash"))

	var second ports.AgentProcess
	waitFor(t, "a replacement process", func() bool {
		p, err := s.Process()
		if err != nil || p == first {
			return false
		}
		second = p
		return true
	})
	second.(*fakeProcess).emit(ports.ProcessEvent{Type: "turn_start", SessionID: "s-2"})
	select {
	case got := <-seen:
		t.Errorf("a cancelled listener still received %q", got)
	default:
	}
}

// drainEvent takes one event, failing the test if none arrives.
func drainEvent(ch chan string) string {
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		return ""
	}
}

func TestARestartHookFiresOnTheReplacementProcess(t *testing.T) {
	// The node's per-conversation event counters belong to the process
	// that held them. A replacement that inherits them would report a
	// sequence continuing through a turn the dead process never finished.
	rec := &factoryRecorder{}
	s := newTestSupervisor(t, fastConfig(rec.next))

	fired := make(chan struct{}, 4)
	s.OnRestart(func() { fired <- struct{}{} })
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-fired:
		t.Error("the restart hook fired on the first start: there was no restart")
	default:
	}

	first, err := s.Process()
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	first.(*fakeProcess).Crash(errors.New("crash"))
	// The replacement is up when the hook has run, so waiting for the hook
	// is waiting for the restart.
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the restart hook never fired on a replacement process")
	}
}

func TestAnUnregisteredRestartHookStopsFiring(t *testing.T) {
	rec := &factoryRecorder{}
	s := newTestSupervisor(t, fastConfig(rec.next))

	fired := make(chan struct{}, 4)
	off := s.OnRestart(func() { fired <- struct{}{} })
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	off()

	first, err := s.Process()
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	first.(*fakeProcess).Crash(errors.New("crash"))
	waitFor(t, "a replacement process", func() bool {
		p, err := s.Process()
		return err == nil && p != first
	})
	select {
	case <-fired:
		t.Error("an unregistered restart hook still fired")
	default:
	}
}

func TestAPanickingRestartHookDoesNotCostTheNodeItsAgent(t *testing.T) {
	// Restart bookkeeping is the least important thing happening the
	// moment a replacement comes up. A hook that panics must not stop the
	// supervisor respawning, or one bad hook becomes the crash loop it
	// was meant to help diagnose.
	rec := &factoryRecorder{}
	cfg := fastConfig(rec.next)
	// Restart forever, so a dead supervisor is the only thing that can
	// end this test early. A bounded budget would stop the supervisor for
	// a reason that has nothing to do with the hook.
	cfg.MaxCrashAttempts = 0
	s := newTestSupervisor(t, cfg)

	var fired atomic.Int32
	s.OnRestart(func() { fired.Add(1); panic("bad hook") })
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := 0; i < 3; i++ {
		var live *fakeProcess
		waitFor(t, "a live process", func() bool {
			p, err := s.Process()
			if err != nil {
				return false
			}
			fake, ok := p.(*fakeProcess)
			if !ok {
				return false
			}
			live = fake
			return true
		})
		live.Crash(errors.New("crash"))
		waitFor(t, "a replacement process", func() bool {
			p, err := s.Process()
			return err == nil && p != live
		})
	}
	if got := fired.Load(); got != 3 {
		t.Errorf("the hook ran %d times, want 3", got)
	}
	if s.Health().Degraded {
		t.Error("a panicking hook degraded the supervisor")
	}
}

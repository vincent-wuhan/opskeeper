// Package pigsupervisor owns the lifecycle of a node's agent process.
//
// The agent runs as a separate process, not in the edge agent's own
// process, and that separation is the whole point. The agent and the plugins
// it loads are the highest-privilege, highest-churn code on a node: a plugin
// can segfault, leak, or wedge, and a shell tool it exposes can do damage
// with no operator present. Confining that to one process means a bad plugin
// upgrade takes down a process the supervisor restarts in seconds, rather
// than the telemetry pipeline, the tunnel, and the upgrade path the operator
// would need in order to roll it back.
//
// The supervisor's second job is to know the difference between an agent
// that is healthy, an agent that is down, and an agent that is failing too
// fast to be worth restarting. Those are three different pages, and
// collapsing them into "up or down" is how a node ends up in a respawn loop
// that looks healthy in a dashboard while burning a core and flooding a log.
package pigsupervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Factory builds one agent process.
//
// It is a factory rather than a single client because a restart is a new
// process with a new session: an agent that died mid-turn cannot resume it,
// and pretending otherwise would silently drop the turn's output.
type Factory func() ports.AgentProcess

// ErrStopped is returned by operations issued after Stop.
var ErrStopped = errors.New("pigsupervisor: supervisor is stopped")

// CrashWindow and the restart policy.
//
// Defaults, chosen so a node recovers from a bad plugin within a minute and
// gives up before it can do real damage if the agent is simply broken.
const (
	// DefaultRestartBackoff is the delay before the first respawn.
	DefaultRestartBackoff = 2 * time.Second
	// DefaultRestartBackoffMax caps the exponential growth. Without a cap
	// a long-lived broken agent settles into a restart every few minutes
	// forever, which reads as "occasionally flaky" rather than "broken".
	DefaultRestartBackoffMax = time.Minute
	// DefaultCrashWindow is the rolling window crash attempts are counted
	// in. It is deliberately several times the maximum backoff so a slow
	// backoff does not itself trip the limit on a merely unlucky agent.
	DefaultCrashWindow = 10 * time.Minute
	// DefaultMaxCrashAttempts is how many crashes inside the window are
	// tolerated before the supervisor gives up restarting.
	DefaultMaxCrashAttempts = 5
)

// Config configures a Supervisor.
type Config struct {
	// Factory builds each agent process. Required.
	Factory Factory
	// Log receives lifecycle events. Optional; a nil logger discards.
	Log *slog.Logger
	// Now defaults to time.Now. Tests inject a clock so backoff and the
	// crash window are exercised without sleeping.
	Now func() time.Time
	// RestartBackoff defaults to DefaultRestartBackoff.
	RestartBackoff time.Duration
	// RestartBackoffMax defaults to DefaultRestartBackoffMax.
	RestartBackoffMax time.Duration
	// CrashWindow defaults to DefaultCrashWindow.
	CrashWindow time.Duration
	// MaxCrashAttempts defaults to DefaultMaxCrashAttempts. Zero means
	// restart forever, which is a legitimate choice for a node whose
	// agent is known-good and whose failure mode is transient.
	MaxCrashAttempts int
}

// Supervisor runs an agent process and keeps it running, up to a point.
//
// A Supervisor is safe for concurrent use. Its methods are the whole
// external surface: Start, Stop, Health, and the pass-throughs to the live
// process.
type Supervisor struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	proc     ports.AgentProcess
	running  bool
	stopped  bool
	started  bool
	degraded bool
	restarts int
	// crashes are the recent crash timestamps inside CrashWindow, used to
	// decide when the agent is failing faster than restarting helps.
	crashes []time.Time
	// backoff is the delay before the next respawn, grown on each
	// consecutive crash and reset on a process that stayed up.
	backoff time.Duration
	// lastStartAt, lastExit and lastError are the health report.
	lastStartAt time.Time
	lastExit    string
	lastError   string

	cancel context.CancelFunc
	done   chan struct{}
	// wake carries a manual restart request. Buffered to depth one so a
	// caller asking for a restart never blocks on a supervisor that is
	// already restarting.
	wake chan struct{}

	// listeners are the subscriptions that must outlive a restart. The
	// bridge subscribes once and keeps relaying; if the subscription
	// lived on the process, every crash would silently end the console's
	// view of a turn with no error anywhere.
	//
	// Each entry also holds its binding to the process currently live.
	// Rebinding happens on every launch, so a listener added while no
	// process is running is not attached until the next one starts - and
	// cancelling it in that window has to reach the binding that launch
	// then creates, or the console keeps receiving a conversation it
	// closed.
	listeners map[int]*subscription
	nextSub   int
	// restartHooks run after a replacement process is up, before the
	// supervisor goes back to waiting on it. They exist for state the
	// node keeps outside the process: an event translator's per-
	// conversation counters, for instance, belong to a process that has
	// just died and must not be carried across to its replacement.
	restartHooks []func()
}

// subscription is one listener and its current process binding.
type subscription struct {
	fn func(ports.ProcessEvent)
	// live releases this listener's binding on the process it is
	// currently attached to. nil while no process is running.
	live func()
}

// New returns a Supervisor. It does not start anything; call Start.
func New(cfg Config) (*Supervisor, error) {
	if cfg.Factory == nil {
		return nil, errors.New("pigsupervisor: Factory is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.RestartBackoff <= 0 {
		cfg.RestartBackoff = DefaultRestartBackoff
	}
	if cfg.RestartBackoffMax <= 0 {
		cfg.RestartBackoffMax = DefaultRestartBackoffMax
	}
	if cfg.CrashWindow <= 0 {
		cfg.CrashWindow = DefaultCrashWindow
	}
	// done starts closed: a supervisor that was never started has no loop
	// to join, and Stop must not wait for one. Initialising it open would
	// hang any shutdown path that stops a supervisor it failed to start —
	// which is exactly the path taken when the agent binary is missing.
	done := make(chan struct{})
	close(done)
	return &Supervisor{
		cfg:       cfg,
		log:       cfg.Log,
		done:      done,
		wake:      make(chan struct{}, 1),
		listeners: make(map[int]*subscription),
	}, nil
}

// Start brings the first agent process up and begins supervising it.
//
// It returns once the first process has been started or has definitively
// failed. A first start that fails is not retried here: the caller asked
// for a process, got none, and should hear about it now rather than find
// out from a health check later.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	// Stopped is checked first: a caller that stopped a supervisor and then
	// tries to start it again deserves to be told it was stopped, not that
	// it "already started" a supervisor that no longer exists.
	if s.stopped {
		s.mu.Unlock()
		return ErrStopped
	}
	if s.started {
		s.mu.Unlock()
		return errors.New("pigsupervisor: already started")
	}
	s.started = true
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()

	proc := s.cfg.Factory()
	if err := s.launch(proc); err != nil {
		// The loop was never started, so the closed channel New installed
		// is still the right one for Stop to wait on. Re-closing it here
		// would panic.
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		cancel()
		return err
	}

	go s.supervise(runCtx, proc)
	return nil
}

// launch starts one process and records its start.
func (s *Supervisor) launch(proc ports.AgentProcess) error {
	err := proc.Start(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		// Rebind every listener to the new process. The old bindings are
		// released first: a process that has ended will never emit again,
		// and holding its subscription keeps its listener reachable for as
		// long as the node is up.
		for _, sub := range s.listeners {
			if sub.live != nil {
				sub.live()
				sub.live = nil
			}
			sub.live = proc.OnEvent(sub.fn)
		}
	}
	s.proc = proc
	if err != nil {
		s.running = false
		s.lastError = err.Error()
		return err
	}
	s.running = true
	s.lastStartAt = s.cfg.Now()
	s.lastError = ""
	return nil
}

// supervise is the restart loop.
//
// It waits on the process's own exit signal rather than polling, so a crash
// is acted on as it happens, and a crash between two polls cannot be missed.
func (s *Supervisor) supervise(ctx context.Context, proc ports.AgentProcess) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-proc.Exited():
		}

		reason := exitReason(proc)

		s.mu.Lock()
		if s.stopped {
			// Asked to stop. The process ending is the stop completing,
			// not a crash, and restarting would ignore the operator.
			s.running = false
			s.lastExit = reason
			s.mu.Unlock()
			return
		}
		s.running = false
		s.restarts++
		s.lastExit = reason
		s.crashes = append(s.crashes, s.cfg.Now())
		s.crashes = trimCrashes(s.crashes, s.cfg.Now(), s.cfg.CrashWindow)
		attempts := len(s.crashes)
		exhausted := s.cfg.MaxCrashAttempts > 0 && attempts >= s.cfg.MaxCrashAttempts
		s.degraded = exhausted
		s.backoff = growBackoff(s.backoff, s.cfg.RestartBackoff, s.cfg.RestartBackoffMax)
		delay := s.backoff
		s.mu.Unlock()

		if exhausted {
			// Give up restarting, but stay up and keep reporting. A
			// supervisor that exits takes its health endpoint with it, and
			// a node that has vanished from the fleet is a worse outcome
			// than one that is visibly broken and awaiting a human.
			//
			// The explanation is written in the same critical section that
			// set the degraded flag. Setting them together is what makes
			// the flag trustworthy: a fleet poll landing between the two
			// would otherwise see a degraded node with nothing to say why,
			// which is the one state an operator cannot act on.
			s.mu.Lock()
			s.lastError = fmt.Sprintf("agent crashed %d times in %s: %s", attempts, s.cfg.CrashWindow, reason)
			s.mu.Unlock()
			s.log.Error("agent process is crash-looping; not restarting",
				"attempts", attempts, "window", s.cfg.CrashWindow, "last_exit", reason)
			return
		}

		s.log.Warn("agent process exited; restarting",
			"attempt", attempts, "in", delay, "last_exit", reason)

		if !sleepCtx(ctx, delay) {
			return
		}

		// A manual restart requested during the backoff replaces this
		// respawn rather than queueing behind it.
		if drained := drain(s.wake); drained {
			s.mu.Lock()
			s.backoff = 0
			s.mu.Unlock()
		}
		if ctx.Err() != nil {
			return
		}

		next := s.cfg.Factory()
		if err := s.launch(next); err != nil {
			s.log.Error("agent process failed to start", "error", err)
			// The fresh process has already closed its own exit channel, so
			// continuing to wait on it would spin at full speed. Wait out
			// the backoff instead, which is what makes a broken binary
			// cheap rather than a busy loop.
			if !sleepCtx(ctx, delay) {
				return
			}
			proc = next
			continue
		}
		proc = next
		// The replacement is up. Tell the node before going back to
		// waiting, so anything keyed on a live process is rebuilt against
		// this one rather than the one that died.
		s.runRestartHooks()
	}
}

// runRestartHooks invokes every restart hook, isolating a panicking one.
//
// A hook is diagnostic state cleanup, not part of keeping the agent
// running. One that fails must not take the supervisor down with it: the
// node would lose the agent it just recovered, over bookkeeping.
func (s *Supervisor) runRestartHooks() {
	s.mu.Lock()
	hooks := make([]func(), len(s.restartHooks))
	copy(hooks, s.restartHooks)
	s.mu.Unlock()
	for _, fn := range hooks {
		if fn == nil {
			continue
		}
		s.safeHook(fn)
	}
}

// safeHook runs one restart hook, containing a panic.
//
// Restart bookkeeping is the least important thing happening on a node at
// the moment a hook runs - the agent has just been recovered and the next
// crash is still minutes away. A hook that panics must not turn a
// recovered agent into a supervisor that will not respawn again, or one
// bad hook would become the crash loop it was meant to help diagnose.
func (s *Supervisor) safeHook(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("agent restart hook panicked", slog.Any("panic", r))
		}
	}()
	fn()
}

// OnRestart registers a hook to run after each replacement process starts.
//
// It is the counterpart to OnEvent for state that outlives a process. A
// listener follows the agent's events across a restart because the events
// belong to the node; a translator's counters must not, because they
// describe a turn the dead process was in the middle of.
//
// The returned function unregisters the hook. Hooks fire synchronously on
// the restart goroutine, so one that blocks delays the next restart - they
// are for clearing state, not for talking to the network.
func (s *Supervisor) OnRestart(fn func()) func() {
	if fn == nil {
		return func() {}
	}
	s.mu.Lock()
	s.restartHooks = append(s.restartHooks, fn)
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		for i, got := range s.restartHooks {
			if reflect.ValueOf(got).Pointer() == reflect.ValueOf(fn).Pointer() {
				s.restartHooks = append(s.restartHooks[:i], s.restartHooks[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}
}

// Restart asks the supervisor to replace the process now.
//
// It is the operator's "the agent is wedged" affordance, and it deliberately
// does not reset the crash budget: a manual restart of an agent that is
// crash-looping must not hand it a fresh allowance, or the escape hatch
// becomes the loop.
func (s *Supervisor) Restart() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return ErrStopped
	}
	live := s.proc
	s.mu.Unlock()
	if live == nil {
		return errors.New("pigsupervisor: no process to restart")
	}
	select {
	case s.wake <- struct{}{}:
	default:
		// A restart is already pending; the caller asked twice, not twice
		// as much work.
	}
	return live.Abort(context.Background())
}

// Stop shuts the process down and stops supervising it. It is idempotent.
func (s *Supervisor) Stop() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	proc := s.proc
	cancel := s.cancel
	done := s.done
	s.mu.Unlock()

	if proc != nil {
		_ = proc.Stop()
	}
	// Release the process bindings here, not on the next Start. A stopped
	// supervisor holds them for as long as the node is up otherwise, and
	// a Start after Stop rebinds anyway.
	s.mu.Lock()
	for _, sub := range s.listeners {
		if sub.live != nil {
			sub.live()
			sub.live = nil
		}
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Wait for the loop to retire so a Stop followed by a Start cannot race
	// two supervisors over one node. The channel is already closed when no
	// loop ever ran, so this returns immediately in that case rather than
	// blocking a shutdown that has nothing to join.
	<-done
	s.mu.Lock()
	s.running = false
	s.proc = nil
	s.mu.Unlock()
	return nil
}

// Health reports what the supervisor can see about its process.
func (s *Supervisor) Health() ports.ProcessHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := ports.ProcessHealth{
		Running:     s.running,
		Restarts:    s.restarts,
		LastStartAt: s.lastStartAt,
		LastExit:    s.lastExit,
		Degraded:    s.degraded,
		LastError:   s.lastError,
	}
	// The agent's own version is only knowable from a live process.
	if s.running && s.proc != nil {
		if state, err := s.proc.State(context.Background()); err == nil {
			h.Version = state.Version
		}
	}
	return h
}

// Running reports whether the supervised process is up.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Process returns the live process, or an error naming what to do instead.
//
// Callers reach the agent through this rather than holding a process
// reference of their own, so a call issued across a restart fails with one
// clear message instead of landing on a process that no longer exists.
//
// The running flag alone is not enough to answer that. supervise sets it
// false only once the restart loop has noticed the exit, so in the window
// between a process dying and the loop reacting, the flag still reads true
// and this would hand out a handle that fails every command. The exit
// channel is the process's own answer and does not have that window; the
// race that remains - a process ending immediately after this returns - is
// inherent to any process handle, and the caller gets a clear error rather
// than a wrong answer.
func (s *Supervisor) Process() (ports.AgentProcess, error) {
	s.mu.Lock()
	stopped := s.stopped
	proc := s.proc
	running := s.running
	s.mu.Unlock()

	if stopped {
		return nil, ErrStopped
	}
	if !running || proc == nil {
		return nil, errors.New("pigsupervisor: agent process is not running")
	}
	if exited(proc.Exited()) {
		return nil, errors.New("pigsupervisor: agent process is not running")
	}
	return proc, nil
}

// exited reports whether ch is already closed.
//
// A nil channel blocks forever and is therefore "not yet exited", which is
// the right answer for a process that publishes no exit signal: treating
// it as ended would refuse to hand out a process that is perfectly alive.
func exited(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// OnEvent subscribes to the agent's event stream, surviving restarts.
//
// The subscription is installed on the current process and re-installed by
// the supervisor on each replacement, so a crash does not leave the console
// watching a process that is gone.
func (s *Supervisor) OnEvent(fn func(ports.ProcessEvent)) func() {
	if fn == nil {
		return func() {}
	}
	s.mu.Lock()
	id := s.nextSub
	s.nextSub++
	sub := &subscription{fn: fn}
	if s.running && s.proc != nil {
		sub.live = s.proc.OnEvent(fn)
	}
	s.listeners[id] = sub
	s.mu.Unlock()

	return func() {
		s.mu.Lock()
		delete(s.listeners, id)
		live := sub.live
		sub.live = nil
		s.mu.Unlock()
		if live != nil {
			live()
		}
	}
}

// growBackoff returns the next delay, doubling from the floor up to the
// ceiling. The first restart waits the base delay rather than zero: a
// process that failed to start needs a moment before retrying, and an
// instant retry turns a configuration error into a spin.
func growBackoff(current, base, max time.Duration) time.Duration {
	if current <= 0 {
		return min(base, max)
	}
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// trimCrashes drops crash timestamps that have fallen out of the window.
//
// Trimming on write rather than on a ticker means the crash budget is
// evaluated against real events, and a supervisor that is never restarted
// costs nothing to keep.
func trimCrashes(crashes []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	kept := crashes[:0]
	for _, at := range crashes {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
}

// sleepCtx waits for d, reporting false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// drain takes a pending wake signal, if any.
func drain(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// exitReason renders why a process ended, for the health report and the log.
func exitReason(proc ports.AgentProcess) string {
	if err := proc.LastError(); err != nil {
		return err.Error()
	}
	return "process exited"
}

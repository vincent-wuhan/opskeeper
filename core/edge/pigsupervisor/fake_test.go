package pigsupervisor

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// errNotRunning is what the fake reports for a call against a process that
// has already ended, matching the adapter's own wording closely enough that
// a test asserting on the message is asserting on behaviour, not on a
// coincidence.
var errNotRunning = errors.New("pigrpc: cannot prompt: agent process is not running")

// fakeProcess is a scriptable AgentProcess.
//
// The supervisor's contract is about what it does when a process ends, so
// the fake has to be able to end on command and to fail to start on command.
// Everything else is a panic if touched, so a test that reaches for behaviour
// it did not set up fails loudly instead of passing on a default.
type fakeProcess struct {
	mu sync.Mutex

	// startErr, when set, makes Start fail. The process still opens and
	// closes its exit channel, because a process that never came up is
	// still over — the supervisor must not wait on it forever.
	startErr error
	// exitErr is what LastError reports once the process has ended.
	exitErr error
	// exitOnStart makes the process end immediately, standing in for a
	// binary that crashes on launch.
	exitOnStart bool

	started  bool
	stopped  bool
	ended    bool
	exited   chan struct{}
	prompts  []string
	steers   []string
	aborts   int
	state    ports.ProcessState
	stateErr error
	model    string
	// listeners is a set rather than one slot because the port fans out.
	// A single slot here would make the fake disagree with the real
	// adapter, and the disagreement would show up as a supervisor test
	// that passes while the node it stands for would starve a subscriber.
	listeners []func(ports.ProcessEvent)
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{exited: make(chan struct{})}
}

func (f *fakeProcess) Start(context.Context) error {
	f.mu.Lock()
	if f.startErr != nil {
		err := f.startErr
		f.started = false
		f.mu.Unlock()
		// A failed start is immediately a finished process.
		f.end()
		return err
	}
	f.started = true
	exitOnStart := f.exitOnStart
	f.mu.Unlock()
	if exitOnStart {
		f.end()
	}
	return nil
}

func (f *fakeProcess) end() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ended {
		return
	}
	f.ended = true
	close(f.exited)
}

func (f *fakeProcess) Stop() error {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.end()
	return nil
}

// Crash ends the process the way a segfault would: without being asked.
func (f *fakeProcess) Crash(err error) {
	f.mu.Lock()
	f.exitErr = err
	f.mu.Unlock()
	f.end()
}

func (f *fakeProcess) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started && !f.ended
}

func (f *fakeProcess) Exited() <-chan struct{} { return f.exited }

func (f *fakeProcess) LastError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exitErr
}

func (f *fakeProcess) Prompt(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started && f.ended {
		return errNotRunning
	}
	f.prompts = append(f.prompts, text)
	return nil
}

func (f *fakeProcess) Steer(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers = append(f.steers, text)
	return nil
}

func (f *fakeProcess) Abort(context.Context) error {
	f.mu.Lock()
	f.aborts++
	f.mu.Unlock()
	f.end()
	return nil
}

func (f *fakeProcess) SetModel(_ context.Context, provider, model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started && f.ended {
		return errNotRunning
	}
	f.model = provider + "/" + model
	return nil
}

func (f *fakeProcess) State(context.Context) (*ports.ProcessState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateErr != nil {
		return nil, f.stateErr
	}
	s := f.state
	return &s, nil
}

func (f *fakeProcess) OnEvent(fn func(ports.ProcessEvent)) func() {
	f.mu.Lock()
	f.listeners = append(f.listeners, fn)
	f.mu.Unlock()
	// The returned function has to actually detach, and it has to detach
	// only this subscription. A no-op makes every cancellation test pass
	// for the wrong reason: the supervisor would look correct while the
	// process stayed wired to a console that believes it closed the
	// conversation.
	return func() {
		f.mu.Lock()
		for i, got := range f.listeners {
			if sameListener(got, fn) {
				f.listeners = append(f.listeners[:i], f.listeners[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
	}
}

// sameListener identifies a subscription by its code pointer. Go forbids
// comparing funcs with ==, and reflect is the only way to compare identity.
func sameListener(a, b func(ports.ProcessEvent)) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func (f *fakeProcess) emit(ev ports.ProcessEvent) {
	f.mu.Lock()
	listeners := make([]func(ports.ProcessEvent), len(f.listeners))
	copy(listeners, f.listeners)
	f.mu.Unlock()
	for _, fn := range listeners {
		fn(ev)
	}
}

func (f *fakeProcess) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeProcess) wasStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

var _ ports.AgentProcess = (*fakeProcess)(nil)

// factoryOf hands out the given processes in order, and records how many it
// was asked for. A test that asserts on the count is asserting that the
// supervisor restarted exactly as many times as it should have.
type factoryRecorder struct {
	mu    sync.Mutex
	procs []*fakeProcess
	made  []*fakeProcess
	// exhausted makes the factory hand out a process that fails to start,
	// so a test can drive the "cannot start at all" path.
	exhausted *fakeProcess
}

func (f *factoryRecorder) next() ports.AgentProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exhausted != nil {
		// Still recorded, so a test asserting on the number of processes
		// built counts a failed start the same as a successful one.
		f.made = append(f.made, f.exhausted)
		return f.exhausted
	}
	var p *fakeProcess
	if len(f.made) < len(f.procs) {
		p = f.procs[len(f.made)]
	} else {
		p = newFakeProcess()
		f.procs = append(f.procs, p)
	}
	f.made = append(f.made, p)
	return p
}

func (f *factoryRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.made)
}

func (f *factoryRecorder) at(i int) *fakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[i]
}

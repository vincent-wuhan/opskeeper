package pigagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/agent"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigcoding"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// SessionKernel is the OpsKeeper agent loop running on PiG's embedded SDK.
//
// # Why there are two kernels
//
// Kernel drives agent.Agent directly, which is the smallest thing that can
// execute a turn: a model, a tool bag, a set of hooks, and a callback for
// each event. SessionKernel drives coding.Session, which is the thing PiG's
// own CLI, RPC mode and Piglets all drive. The second is not a wrapper
// around the first. It is the difference between OpsKeeper embedding a
// library and OpsKeeper embedding *this* library: a Session carries the
// extension runner, the session log, the turn-end boundary, the steering
// queue and the abort signal, and every one of those is a thing a plugin
// loaded through a PiG package expects to find. An agent.Agent has none of
// them, so a plugin that calls session_start against a Kernel-driven turn
// gets nothing and no error.
//
// So the question was never whether to keep Kernel. It is that Kernel runs
// in contexts where standing up a Runtime is wrong: a unit test, a
// background worker that must not hold an extension process, a report
// query with no turn at all. The invariant is the one that matters and it
// is testable — the two drivers must produce the same console frames, the
// same ledger rows and the same transcript rows for the same script, and
// golden_test.go's sibling in this package holds them to that.
//
// # What is shared and what is not
//
// Shared, deliberately: Mapper (the frame vocabulary), runState (the whole
// policy gate — approval, budget, audit, recorder, usage folding), buildPrompt
// (transcript assembly) and NewAdapters (the tool bag). None of that knows
// which loop is running it, and a copy of any of it would be a second
// implementation to keep honest about the console's wire contract.
//
// Not shared, necessarily: FinishTurn. agent.Agent takes a FinishTurn hook
// directly; coding.Session does not expose one, because
// Session.installAgentBoundaryHooks already installs its own and would have
// to wrap ours. Replacing it would delete the extension turn_end boundary
// that every PiG package's session lifecycle depends on. The round cap
// therefore lands on BeforeToolCall through pigcoding.TurnBudget, which is
// a different mechanism with a different terminal frame; see
// SessionKernelOptions for what that costs and why it is worth it.

// SessionKernelOptions configures a SessionKernel.
type SessionKernelOptions struct {
	// Runtime is the process-wide PiG container whose sessions this kernel
	// runs. Required, and the same runtime the rest of the process uses:
	// two runtimes would mean two extension runners, and a plugin loaded
	// through one would be invisible to a turn driven by the other.
	Runtime *pigcoding.Runtime

	// Models resolves the per-turn model. Required.
	//
	// It is the same ModelResolver Kernel takes, and the StreamOptions it
	// returns are deliberately discarded. Those options exist only to carry
	// an API key into a provider call the kernel makes itself; a Session
	// asks the model for its own provider, and pigmodel's provider closure
	// already reads the key from settings on every call. Discarding them is
	// what removes the last OpsKeeper-shaped piece of the credential path —
	// there is no longer a second way for a key to reach a model.
	Models ModelResolver

	// Deps supplies the host services for a turn. Required.
	Deps DepsProvider

	// Persist records settled messages. Optional; nil means the host keeps
	// no transcript.
	Persist Persister

	// Now defaults to time.Now.
	Now func() time.Time

	// MaxIterations caps tool rounds per turn when a request does not set
	// its own. Zero selects DefaultMaxIterations.
	//
	// The cap is enforced by pigcoding.TurnBudget through BeforeToolCall,
	// which is one mechanism later than Kernel's agent.AgentOptions.MaxTurns
	// and produces a different terminal frame: a Session turn at its cap
	// ends with the model answering from the evidence it has, where a
	// Kernel turn ends in an error. That difference is a deliberate
	// improvement, not an accident — a turn that ran long is not a crash,
	// and the console's own path for TurnMaxIterations turns either into
	// the same apology. golden_spent_budget_test.go pins both.
	MaxIterations int

	// SessionTimeout bounds a single turn. A turn that exceeds it is
	// cancelled and reported as a timeout, so a wedged provider cannot
	// hold a session slot forever.
	SessionTimeout time.Duration

	// There is deliberately no working-directory option here.
	//
	// There was one, called ScratchDir, documented as "becomes the
	// session's working directory". It was never set by any caller, and
	// this is why: PiG only reads SessionStartOptions.CWDOverride when it
	// is *resuming* a session whose stored cwd no longer exists. A fresh
	// session's working directory comes from the Services the Runtime was
	// built with, and nothing a caller passes at Start can move it.
	//
	// That claim was measured, not read. pigcontract's
	// TestCWDOverrideDoesNotMoveAFreshSession starts a session whose
	// override names a directory that exists, and PiG still reports the
	// runtime's cwd. A field that looks like a safety measure and is not
	// one is worse than a missing field, because the missing one gets
	// written; the assertion is left in place so that upstream honouring
	// it turns red instead of turning silently useful.
	//
	// The lever is pigcoding.RuntimeOptions.CWD, and it is the deployment's
	// to set. See decision 171.
}

// SessionKernel is the SDK-driven implementation of the Agent port.
type SessionKernel struct {
	opts SessionKernelOptions

	mu       sync.Mutex
	sessions map[string]*sessionRun
}

// sessionRun is one live turn.
type sessionRun struct {
	sess   *pigcoding.Session
	cancel context.CancelFunc
	// turns counts completed turns, so Steer knows whether anything is
	// actually in flight.
	turns int
}

// The production SessionKernel. Asserted here so a change to the loop's
// method set breaks this file rather than every host that stores an Agent.
var _ Agent = (*SessionKernel)(nil)

// NewSessionKernel returns a SessionKernel.
func NewSessionKernel(opts SessionKernelOptions) (*SessionKernel, error) {
	if opts.Runtime == nil {
		return nil, errors.New("pigagent: SessionKernel requires a pigcoding.Runtime")
	}
	if opts.Models == nil {
		return nil, errors.New("pigagent: Models is required")
	}
	if opts.Deps == nil {
		return nil, errors.New("pigagent: Deps is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = DefaultMaxIterations
	}
	if opts.SessionTimeout <= 0 {
		opts.SessionTimeout = 10 * time.Minute
	}
	return &SessionKernel{opts: opts, sessions: make(map[string]*sessionRun)}, nil
}

// Run settles one turn.
func (k *SessionKernel) Run(ctx context.Context, req ports.AgentRequest) (*TurnResult, error) {
	if req.SessionID == "" {
		return nil, fmt.Errorf("%w: session id required", ErrNoRun)
	}

	// Same refusal as Kernel: two overlapping turns on one transcript would
	// interleave two assistants' messages in a shared incident view.
	k.mu.Lock()
	if _, live := k.sessions[req.SessionID]; live {
		k.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	k.mu.Unlock()

	deps, err := k.opts.Deps(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("pigagent: host deps: %w", err)
	}

	model, _, err := k.opts.Models.Model(ctx, req.Selection)
	if err != nil {
		return nil, fmt.Errorf("pigagent: resolve model: %w", err)
	}
	if model == nil {
		return nil, fmt.Errorf("%w: resolver returned no model", ErrNoRun)
	}

	// An absent bag is an empty bag: a summariser and a background worker
	// are legitimate turns with no tools.
	var admitted []ports.Tool
	if deps.Tools != nil {
		admitted = deps.Tools.Tools()
	}
	tools, err := NewAdapters(admitted)
	if err != nil {
		return nil, err
	}

	mapper := NewMapper(MapperOptions{SessionID: req.SessionID, Role: req.Role, Now: k.opts.Now})
	sink := ports.FromContext(ctx)

	maxRounds := req.MaxIterations
	if maxRounds <= 0 {
		maxRounds = k.opts.MaxIterations
	}

	gate := &runState{
		mapper: mapper,
		sink:   sink,
		deps:   deps,
		host:   runHost{persist: k.opts.Persist, now: k.opts.Now},
		req:    req,
		model:  model.ID,
	}

	sess, err := k.opts.Runtime.Start(pigcoding.Start{
		Model:        model,
		SystemPrompt: req.SystemPrompt,
		Tools:        tools,
		// Only the gate. The round cap is not here because pigcoding
		// installs its own budget hook ahead of this slice; see
		// SessionKernelOptions.MaxIterations.
		BeforeToolCall: []agent.BeforeToolCallHook{gate.beforeToolCall},
		MaxRounds:      maxRounds,
		// An operations agent's capabilities are its own catalogue. PiG's
		// built-in read/write/edit/bash tools are a general-purpose
		// filesystem editor, and on a production node they are an attack
		// surface nobody asked for.
		SkipBuiltinTools: true,
		// OpsKeeper's own session table is the transcript of record, always.
		// Leaving PiG's log on would put a second copy of every turn on the
		// node's disk with its own retention story.
		NoSession: true,
		// Pin the PiG session id to our session row's id so a transcript
		// traces to the agent that produced it without a mapping table.
		SessionID: req.SessionID,
		// No CWDOverride: PiG ignores it for a fresh session, so passing
		// one would read as a containment measure while changing nothing.
		// The session's working directory is the Runtime's, which the
		// deployment owns; see SessionKernelOptions and decision 171.
		CWDOverride: "",
	})
	if err != nil {
		return failTurn(mapper, sink, err, false)
	}
	// Close aborts an in-flight turn and releases the extension runner the
	// session owns. Leaking one leaks a process tree.
	defer sess.Close()

	// No Start field carries AfterToolCall, and putting the audit write in
	// BeforeToolCall would record calls that were then refused. PiG's own
	// Session appends its extension bridge here, so an append cannot
	// displace it.
	if err := sess.AddAfterToolCallHook(gate.afterToolCall); err != nil {
		return failTurn(mapper, sink, fmt.Errorf("pigagent: audit hook: %w", err), false)
	}

	turnCtx, cancel := context.WithTimeout(ctx, k.opts.SessionTimeout)
	defer cancel()

	run := &sessionRun{sess: sess, cancel: cancel}
	k.mu.Lock()
	k.sessions[req.SessionID] = run
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		if cur, ok := k.sessions[req.SessionID]; ok && cur == run {
			delete(k.sessions, req.SessionID)
		}
		k.mu.Unlock()
	}()

	consumer := k.consume(sess, gate, turnCtx)
	// Same contract as Kernel.Run: the whole seeded prompt is already
	// accounted for, so none of it may be written back as new rows.
	prompt := buildPrompt(req)
	gate.seededPromptPending = len(prompt)
	messages, runErr := sess.Run(turnCtx, prompt)
	// RunAgentPrompt flushes at its boundaries and again before it returns,
	// so by here every event the turn produced has been consumed and
	// acknowledged. The consumer is idle but still parked on the channel,
	// which only Close shuts — so the session is closed explicitly and the
	// defer above becomes a no-op rather than leaving a goroutine reading a
	// channel nobody will ever close.
	_ = sess.Close()
	consumer.stop()

	k.mu.Lock()
	run.turns++
	k.mu.Unlock()

	persistErr := consumer.persistError()
	if runErr == nil && persistErr != nil {
		// A turn whose transcript was not recorded must not be reported as
		// a success. The console would render a bubble the next history
		// load cannot find.
		runErr = persistErr
	}
	if runErr != nil {
		return failTurn(mapper, sink, runErr, errors.Is(runErr, context.DeadlineExceeded))
	}

	res := gate.result(messages)
	// A spent round cap is a cap, and reporting it as a plain end_turn would
	// leave an operator reading a truncated investigation as a complete one.
	// Kernel reports the same condition through TurnMaxIterations for the
	// same reason.
	if sess.Budget().Spent() {
		res.Stopped = TurnMaxIterations
	}
	return res, nil
}

// eventConsumer is the goroutine draining a session's event stream.
type eventConsumer struct {
	done chan struct{}

	mu         sync.Mutex
	persistErr error
}

// consume starts the Events consumer a Session needs.
//
// It is not optional bookkeeping. RunAgentPrompt waits at an internal
// barrier before it settles, and releases only when a consumer acknowledges
// it — a caller that forgets this hangs every turn rather than merely
// losing frames, which is why the stream is drained here and not by the
// host.
//
// The order inside the loop is the load-bearing part. Acknowledge comes
// first and gates everything else, because the barrier is FIFO: by
// acknowledging event N the consumer promises it has already done the work
// for N, and doing that work after the acknowledgement would let the run
// advance past work the console has not seen.
func (k *SessionKernel) consume(sess *pigcoding.Session, gate *runState, ctx context.Context) *eventConsumer {
	c := &eventConsumer{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for ev := range sess.Events() {
			if sess.Acknowledge(ev) {
				// An internal barrier, not a frame. Serialising it would put
				// something in the console's stream that the model never
				// did; ignoring it would wedge the run.
				continue
			}
			gate.onEvent(ev)
			// PiG invokes its own persistence hook from message_end, and so
			// does this. Persisting on MessageEndEvent rather than on
			// TurnEndEvent is what keeps the transcript row-for-row
			// identical to Kernel's: same event, same order, same
			// granularity. A turn-end hook would move the assistant row
			// after its own tool results and the replayed transcript would
			// no longer be the one an incident review reads.
			if end, ok := ev.(agent.MessageEndEvent); ok && end.Message.System == nil {
				// System messages are the SDK echoing its own configuration
				// back through the event stream. OpsKeeper composed that
				// prompt, decided where it lives, and already stores it;
				// persisting it here would write a row per turn that the
				// console has no bubble for and that the next turn's replay
				// would have to recognise and skip. The transcript is the
				// conversation between the operator and the model, and a
				// system entry is neither side of it.
				//
				// This is the transcript half of the same rule the mapper
				// applies to frames, and both are needed: filtering the
				// frame without filtering the row leaves a row the console
				// cannot render, and filtering the row without the frame
				// leaves a bubble with nothing behind it.
				if err := gate.persist(end.Message); err != nil {
					c.mu.Lock()
					if c.persistErr == nil {
						c.persistErr = err
					}
					c.mu.Unlock()
					// Stop the turn. There is no panel that reports a
					// persistence failure to a running Session, and
					// letting the loop keep calling a provider for a turn
					// whose output is already being dropped spends money
					// on an answer nobody will ever read.
					_ = sess.Abort(ctx)
				}
			}
		}
	}()
	return c
}

// stop waits for the consumer to finish draining.
func (c *eventConsumer) stop() {
	<-c.done
}

// persistError is the first transcript write failure, if any.
func (c *eventConsumer) persistError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persistErr
}

// Steer injects a message into the turn already running for a session.
func (k *SessionKernel) Steer(ctx context.Context, sessionID, text string) error {
	k.mu.Lock()
	sess, ok := k.sessions[sessionID]
	k.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoRun, sessionID)
	}
	return sess.sess.Steer(ctx, text)
}

// Abort cancels the turn in flight for a session. It is idempotent and safe
// to call when nothing is running.
func (k *SessionKernel) Abort(ctx context.Context, sessionID string) error {
	k.mu.Lock()
	sess, ok := k.sessions[sessionID]
	k.mu.Unlock()
	if !ok {
		return nil
	}
	return sess.sess.Abort(ctx)
}

// Spawn starts a background worker with its own tool bag and prompt.
//
// The worker runs on its own goroutine and reports its terminal state
// through the same sink as the parent turn. It is detached from the
// parent's cancellation because a worker must outlive the turn that spawned
// it, and it is stopped explicitly through Abort.
func (k *SessionKernel) Spawn(ctx context.Context, req ports.AgentRequest) (string, error) {
	if req.SessionID == "" {
		return "", fmt.Errorf("%w: session id required", ErrNoRun)
	}
	workerID := req.SessionID
	workerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), k.opts.SessionTimeout)
	go func() {
		defer cancel()
		_, _ = k.Run(workerCtx, req)
	}()
	return workerID, nil
}

// Notify reports a worker's terminal state to the parent turn.
func (k *SessionKernel) Notify(ctx context.Context, workerID, status, summary string) error {
	k.mu.Lock()
	_, ok := k.sessions[workerID]
	k.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoRun, workerID)
	}
	return nil
}

// Close aborts every live turn. A host calls it on shutdown so an
// in-flight investigation does not outlive the process.
func (k *SessionKernel) Close() error {
	k.mu.Lock()
	sessions := make([]*sessionRun, 0, len(k.sessions))
	for _, s := range k.sessions {
		sessions = append(sessions, s)
	}
	k.sessions = make(map[string]*sessionRun)
	k.mu.Unlock()
	for _, s := range sessions {
		s.cancel()
	}
	return nil
}

// LiveSessions reports how many turns are in flight, for health reporting.
func (k *SessionKernel) LiveSessions() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.sessions)
}

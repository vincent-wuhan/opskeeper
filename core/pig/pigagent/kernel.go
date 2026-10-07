package pigagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// Errors a caller distinguishes. ErrNoRun means no turn is in flight for
// the session, which is not the same as a turn that failed.
var (
	ErrNoRun          = errors.New("pigagent: no run in flight for session")
	ErrAlreadyRunning = errors.New("pigagent: a run is already in flight for session")
)

// ModelResolver turns a model selection into a PiG model plus the stream
// options carrying its per-request credentials.
//
// It is the seam that keeps this package from depending on pigmodel
// directly: a host can route through the settings-backed registry, a test
// can return a stub, and neither drags the other into its build.
type ModelResolver interface {
	Model(ctx context.Context, sel domain.ModelSelection) (*ai.Model, ai.StreamOptions, error)
}

// Persister records a settled message. It is the host's write path; the
// kernel never touches storage itself.
//
// The message is the kernel-neutral transcript entry rather than PiG's own
// type. A host that took the PiG shape would have to import PiG to implement
// this one method, which is exactly the boundary core/pig exists to hold;
// the conversion is done here, once, and is covered by its own test.
type Persister interface {
	Persist(ctx context.Context, sessionID string, msg ports.AgentMessage) error
}

// KernelOptions configures a Kernel.
type KernelOptions struct {
	// Models resolves the per-turn model selection. Required.
	Models ModelResolver
	// Deps supplies the host services for a turn. Required.
	Deps DepsProvider
	// Persist records settled messages. Optional; a nil Persister means the
	// host is not keeping a transcript.
	Persist Persister
	// Now defaults to time.Now.
	Now func() time.Time
	// MaxIterations caps tool rounds per turn when a request does not set
	// its own. Zero selects the default below.
	MaxIterations int
	// SessionTimeout bounds a single turn. A turn that exceeds it is
	// cancelled and reported as a timeout, so a wedged provider cannot
	// hold a session slot forever.
	SessionTimeout time.Duration
}

// DefaultMaxIterations caps tool rounds per turn when nothing else does.
// It is deliberately modest: a turn that has not converged after this many
// round trips is a loop, not an investigation, and the budget checker is
// the better place to catch spend.
const DefaultMaxIterations = 12

// Kernel is the OpsKeeper implementation of the agent-loop port. It drives
// PiG's agent loop and owns nothing else: persistence, streaming, audit,
// and approval are all injected through ports, which is what lets the same
// kernel serve the control plane, a background investigator, and a
// per-node pig process with different policies and no code changes.
type Kernel struct {
	opts KernelOptions

	mu       sync.Mutex
	sessions map[string]*session
}

// session is one live or recently-finished turn.
type session struct {
	agent  *agent.Agent
	cancel context.CancelFunc
	// turn counts completed turns, so Steer knows whether anything is
	// actually in flight.
	turns int
}

// NewKernel returns a Kernel. Models and Deps are required: a kernel with
// no model could only fail at the first turn, and a kernel with no host
// services would run tools outside the audit and approval guarantees.
// The production Agent. Asserted here so a change to the loop's method set
// breaks this file rather than every host that stores an Agent.
var _ Agent = (*Kernel)(nil)

func NewKernel(opts KernelOptions) (*Kernel, error) {
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
	return &Kernel{opts: opts, sessions: make(map[string]*session)}, nil
}

// runHost is the kernel's contribution to a turn's policy state.
//
// It is a method rather than a field so the defaults are applied once, at
// construction, and both drivers read the same resolved values. A driver
// that passed k.opts straight through would hand the run state an
// un-defaulted clock on the day someone fills KernelOptions.Now with a nil
// to mean "give me the real one".
func (k *Kernel) runHost() runHost {
	return runHost{persist: k.opts.Persist, now: k.opts.Now}
}

// Run settles one turn.
func (k *Kernel) Run(ctx context.Context, req ports.AgentRequest) (*TurnResult, error) {
	if req.SessionID == "" {
		return nil, fmt.Errorf("%w: session id required", ErrNoRun)
	}

	// Refuse to start a second turn on a live session. Overlapping turns
	// on one transcript would interleave two assistants' messages in a
	// shared incident view.
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

	model, streamOpts, err := k.opts.Models.Model(ctx, req.Selection)
	if err != nil {
		return nil, fmt.Errorf("pigagent: resolve model: %w", err)
	}

	// A host may legitimately run a turn with no tools at all — a
	// summariser, a classifier, a background worker whose profile grants
	// nothing. An absent bag is an empty bag, not a nil dereference.
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

	maxTurns := req.MaxIterations
	if maxTurns <= 0 {
		maxTurns = k.opts.MaxIterations
	}

	// The stream options carry the per-request credential, so the same
	// StreamFn works for every provider and a key rotation between the
	// agent's construction and its first request is still observed.
	gate := &runState{mapper: mapper, sink: sink, deps: deps, host: k.runHost(), req: req}
	if model != nil {
		gate.model = model.ID
	}

	ag := agent.NewAgent(agent.AgentOptions{
		Model:           model,
		Tools:           tools,
		SystemPrompt:    req.SystemPrompt,
		MaxTurns:        maxTurns,
		SessionID:       req.SessionID,
		DefaultStreamFn: streamFnWith(streamOpts),
		OnEvent:         gate.onEvent,
		OnMessagePersist: func(msg agent.AgentMessage) error {
			return gate.persist(msg)
		},
		BeforeToolCall: []agent.BeforeToolCallHook{gate.beforeToolCall},
		AfterToolCall:  []agent.AfterToolCallHook{gate.afterToolCall},
		FinishTurn:     gate.finishTurn,
	})

	turnCtx, cancel := context.WithTimeout(ctx, k.opts.SessionTimeout)
	defer cancel()

	sess := &session{agent: ag, cancel: cancel}
	k.mu.Lock()
	k.sessions[req.SessionID] = sess
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		if cur, ok := k.sessions[req.SessionID]; ok && cur == sess {
			delete(k.sessions, req.SessionID)
		}
		k.mu.Unlock()
	}()

	prompt := buildPrompt(req)
	// PiG persists every message it is handed, and every message in this
	// prompt is already accounted for: history was stored when it was
	// produced, the runtime stored the operator's turn before calling us
	// (runtime.go, the "persist before LLM call" invariant), and the
	// per-turn reminder must never be stored at all. So the whole seeded
	// prompt is skipped and only what the model produces from here is
	// written. Counting len(prompt) rather than re-deriving the split means
	// the skip cannot drift from what was actually sent.
	gate.seededPromptPending = len(prompt)
	run, err := ag.BeginSendMessages(turnCtx, prompt)
	if err != nil {
		return failTurn(mapper, sink, fmt.Errorf("pigagent: begin turn: %w", err), true)
	}

	messages, runErr := run.Run()
	k.mu.Lock()
	sess.turns++
	k.mu.Unlock()

	if runErr != nil {
		return failTurn(mapper, sink, runErr, errors.Is(runErr, context.DeadlineExceeded))
	}

	return gate.result(messages), nil
}

// failTurn emits the terminal error frame and builds the matching result.
//
// The frame is emitted before returning so the console sees a failure even
// when the caller only inspects the returned error.
//
// It is a free function rather than a method because both drivers end a
// turn this way, and a method on Kernel would have made SessionKernel reach
// for a kernel it never built in order to report its own failure. It reads
// nothing from a kernel: the mapper holds the frame sequence and the sink
// is the console, and both belong to the turn rather than to the loop that
// happened to drive it.
func failTurn(m *Mapper, sink ports.EventSink, err error, retryable bool) (*TurnResult, error) {
	code := "agent_error"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "turn_timeout"
	} else if errors.Is(err, context.Canceled) {
		code = "turn_cancelled"
	}
	_ = sink.Emit(context.Background(), m.Error(code, err.Error(), retryable))
	return &TurnResult{Stopped: TurnError, Err: err}, err
}

// Steer injects a message into the turn already running for a session.
func (k *Kernel) Steer(ctx context.Context, sessionID, text string) error {
	k.mu.Lock()
	sess, ok := k.sessions[sessionID]
	k.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoRun, sessionID)
	}
	sess.agent.Steer(agent.AgentMessage{User: &agent.UserMessage{
		Role:      "user",
		Content:   agentUserContent(text),
		Timestamp: nowMillis(),
	}})
	return nil
}

// Abort cancels the turn in flight for a session. It is idempotent and safe
// to call when nothing is running.
func (k *Kernel) Abort(ctx context.Context, sessionID string) error {
	k.mu.Lock()
	sess, ok := k.sessions[sessionID]
	k.mu.Unlock()
	if !ok {
		return nil
	}
	sess.agent.Abort()
	return nil
}

// Spawn starts a background worker with its own tool bag and prompt.
//
// The worker runs on its own goroutine and reports its terminal state
// through the same sink as the parent turn, which renders it as an inline
// tile. Spawn returns as soon as the worker is running; it does not block
// the parent.
func (k *Kernel) Spawn(ctx context.Context, req ports.AgentRequest) (string, error) {
	if req.SessionID == "" {
		return "", fmt.Errorf("%w: session id required", ErrNoRun)
	}
	workerID := req.SessionID
	// Detach from the parent's cancellation: a worker must outlive the
	// turn that spawned it, and it is stopped explicitly through Abort.
	workerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), k.opts.SessionTimeout)

	go func() {
		defer cancel()
		_, _ = k.Run(workerCtx, req)
	}()
	return workerID, nil
}

// Notify reports a worker's terminal state to the parent turn.
func (k *Kernel) Notify(ctx context.Context, workerID, status, summary string) error {
	k.mu.Lock()
	parent := workerID
	_, ok := k.sessions[parent]
	k.mu.Unlock()
	if !ok {
		// The parent turn has already settled. There is no channel left to
		// report on, and inventing a detached notification would deliver a
		// frame to nobody.
		return fmt.Errorf("%w: %s", ErrNoRun, workerID)
	}
	return nil
}

// Close aborts every live turn. A host calls it on shutdown so an
// in-flight investigation does not outlive the process.
func (k *Kernel) Close() error {
	k.mu.Lock()
	sessions := make([]*session, 0, len(k.sessions))
	for _, s := range k.sessions {
		sessions = append(sessions, s)
	}
	k.sessions = make(map[string]*session)
	k.mu.Unlock()
	for _, s := range sessions {
		s.cancel()
	}
	return nil
}

// LiveSessions reports how many turns are in flight, for health reporting.
func (k *Kernel) LiveSessions() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.sessions)
}

// buildPrompt lives in prompt.go — it now also replays AgentRequest.History,
// which is enough logic (and enough ways to be wrong) to deserve its own
// file and its own tests.

func agentUserContent(s string) ai.UserText { return ai.UserText(s) }

func trim(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// streamFnWith pins a request's resolved credentials onto every provider
// call while leaving the request itself to the model's own provider.
//
// The credential snapshot is taken when the turn starts, not per round
// trip: reading settings on each request would let a mid-turn key rotation
// change the model identity underneath a conversation.
//
// The provider is the one the resolver already bound, not one rebuilt from
// the model's metadata. Rebuilding it would discard the credential closure
// the registry attached, and would fail outright for any provider outside
// the fixed OpenAI/Anthropic/Google/Bedrock/Mistral set — including a
// self-hosted gateway, which is the normal ops deployment.
func streamFnWith(opts ai.StreamOptions) agent.StreamFn {
	return func(ctx context.Context, model *ai.Model, transcript ai.TranscriptContext, callOpts ai.StreamOptions) (*ai.AssistantMessageEventStream, error) {
		if model == nil || model.Provider == nil {
			return nil, fmt.Errorf("pigagent: model has no provider")
		}
		// The per-request options are the base; the loop's own options win
		// on the fields it computes (model cost, tool choice, sampling).
		merged := opts
		merged.MaxTokens = callOpts.MaxTokens
		merged.Temperature = callOpts.Temperature
		merged.ModelCost = callOpts.ModelCost
		merged.ToolChoice = callOpts.ToolChoice
		merged.Thinking = callOpts.Thinking
		merged.IsReasoning = callOpts.IsReasoning
		merged.SessionID = callOpts.SessionID
		return model.Provider.Stream(ctx, transcript, merged)
	}
}

// unusedWireRef keeps the wire import meaningful for the frame types the
// mapper returns when this file is compiled without the event mapper.
var _ = wire.StreamDone

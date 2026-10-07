package pigcoding

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

// Start describes the session one OpsKeeper turn runs in.
//
// Every field is PiG's. There is no OpsKeeper-shaped session request, and
// the absence is the design: a caller that can express a turn in
// SessionStartOptions can express it in PiG's own terms, so a PiG feature
// that is not in this struct is still reachable by whoever needs it —
// which is the property a wrapper erases.
type Start struct {
	// Model is the resolved model for this turn. Required.
	Model *ai.Model

	// SystemPrompt is the fully assembled base prompt, persona and skill
	// text included. OpsKeeper composes it because it is policy: what the
	// agent is allowed to say is a control-plane decision, not an agent
	// runtime's.
	SystemPrompt string

	// SystemPromptSections carries a structured prompt when OpsKeeper has
	// one. It wins over SystemPrompt when both are set, because PiG treats
	// it as the same prompt in a richer form rather than as an addition.
	SystemPromptSections ai.OrderedSections

	// Tools are the turn's tool bag, already filtered by role and profile.
	// OpsKeeper does not let the session widen it.
	Tools []agent.AgentTool

	// BeforeToolCall is where OpsKeeper's policy lives. A hook that returns
	// a block reason stops the call and the model is told why, so the
	// refusal is visible to the turn rather than only to the audit log.
	BeforeToolCall []agent.BeforeToolCallHook

	// ThinkingLevel overrides the model default for this turn.
	ThinkingLevel ai.ThinkingLevel

	// ScopedModels is the model cycle the console can step through.
	// Nil means the session may use any configured model.
	ScopedModels []coding.ScopedModel

	// SessionLog selects the transcript log. Nil uses the runtime's
	// in-memory log.
	SessionLog *coding.SessionManager

	// SkipBuiltinTools omits PiG's read/write/edit/bash tools. OpsKeeper
	// sets it: an operations agent's capabilities are its own tool
	// catalogue, and a general-purpose filesystem editor on a production
	// node is an attack surface nobody asked for.
	SkipBuiltinTools bool

	// NoSession disables persistence for this turn. OpsKeeper sets it
	// whenever its own session table is the transcript of record, which is
	// always.
	NoSession bool

	// CWDOverride is the working directory tools run in. OpsKeeper sets it
	// to a per-session scratch directory so a tool with a relative path
	// cannot walk into the manager's own source tree.
	CWDOverride string

	// MaxRounds caps the tool rounds this turn may spend. Zero is
	// unbounded; see NewTurnBudget for why this package declines to pick a
	// default. OpsKeeper always sets it, because a session whose cap lives
	// in a caller's good intentions is a session whose cap is a comment.
	//
	// The cap is enforced through the BeforeToolCall panel rather than an
	// agent option, and Runtime.Start installs it ahead of the hooks below.
	// See TurnBudget for the ordering argument.
	MaxRounds int

	// SkipExtensionTools omits the tools the Runtime's extensions
	// contributed, leaving only Tools.
	//
	// It is a security knob, not a tidiness one. OpsKeeper registers its
	// policy extensions on the Runtime so a gate cannot be forgotten on one
	// construction path, which means every session also inherits every tool
	// those extensions carry. A turn that must run with an empty bag says so
	// here, rather than relying on the extension set happening to be empty.
	SkipExtensionTools bool

	// AllowedTools is an allowlist applied to the final tool set, built-in,
	// extension and caller tools alike. Nil means no allowlist.
	AllowedTools map[string]struct{}

	// ExcludedTools is a denylist applied after the allowlist. It exists
	// beside AllowedTools because the two answer different questions: an
	// allowlist says what this turn may do, a denylist says what must never
	// appear even though something else vouched for it.
	ExcludedTools map[string]struct{}

	// NoTools mirrors PiG's own shorthand: "all" exposes nothing, "builtin"
	// omits the built-in coding tools. Empty leaves the set as assembled.
	NoTools string

	// SessionID pins the PiG session id instead of letting PiG generate
	// one. OpsKeeper pins its own session row's id so a transcript can be
	// traced to the agent that produced it without a second lookup table
	// mapping one id onto the other.
	SessionID string
}

// Session is one live turn's PiG session.
//
// The wrapper is four methods and a Close. It exists so the close ordering
// and the "subscribe before you send" rule are enforced in one place, not
// so the type can be renamed.
type Session struct {
	sess   *coding.Session
	closed bool

	// budget is the round cap this turn is running under, or nil when the
	// caller asked for none. It is retained so a caller can ask a turn why
	// it stopped, which is a different question from whether it stopped.
	budget *TurnBudget
}

// Start opens a session.
//
// A nil Model is refused here rather than at the first prompt: a session
// with no model can only fail later, inside a turn that has already been
// persisted and billed to an operator's console.
func (r *Runtime) Start(start Start) (*Session, error) {
	if r == nil || r.closed || r.rt == nil {
		return nil, ErrClosed
	}
	if start.Model == nil {
		return nil, errors.New("pigcoding: Start requires a Model")
	}

	// A caller that named no cap gets no budget object at all, rather than a
	// budget reporting zero. The two would be indistinguishable at the call
	// site that asks why a turn stopped, which is the only place the answer
	// is read.
	var budget *TurnBudget
	if start.MaxRounds > 0 {
		budget = NewTurnBudget(start.MaxRounds)
	}

	// The budget goes first, ahead of the caller's hooks, because it counts
	// rounds and a round the policy gate refused is still a round the model
	// spent. TurnBudget documents why reversing this order would let a
	// refusing gate switch the cap off.
	hooks := composeBeforeToolCall(budget, start.BeforeToolCall)

	opts := coding.SessionStartOptions{
		Model:                start.Model,
		SystemPrompt:         start.SystemPrompt,
		SystemPromptSections: start.SystemPromptSections,
		// PiG calls this ExtraTools because it adds them on top of whatever
		// the extensions contributed. OpsKeeper skips extension tools
		// separately (below), so the two sets are disjoint and the name is
		// only a source-order detail.
		ExtraTools:         start.Tools,
		BeforeToolCall:     hooks,
		ThinkingLevel:      start.ThinkingLevel,
		ScopedModels:       start.ScopedModels,
		SessionManager:     start.SessionLog,
		SkipBuiltinTools:   start.SkipBuiltinTools,
		SkipExtensionTools: start.SkipExtensionTools,
		AllowedTools:       start.AllowedTools,
		ExcludedTools:      start.ExcludedTools,
		NoTools:            start.NoTools,
		SessionID:          start.SessionID,
		NoSession:          start.NoSession,
	}
	if start.CWDOverride != "" {
		cwd := start.CWDOverride
		opts.CWDOverride = &cwd
	}

	sess, err := r.rt.New(opts)
	if err != nil {
		return nil, fmt.Errorf("pigcoding: start session: %w", err)
	}
	return &Session{sess: sess, budget: budget}, nil
}

// Budget is the round cap this turn is running under, or nil when the caller
// asked for none.
//
// It answers "why did this investigation stop" with a number instead of an
// absence. A turn that ends because it ran out of rounds and a turn that ends
// because the model finished look identical in a transcript, and only one of
// them is a defect worth an operator's attention.
func (s *Session) Budget() *TurnBudget {
	if s == nil {
		return nil
	}
	return s.budget
}

// Send runs one turn to completion and returns the messages it produced.
//
// It blocks, exactly as PiG documents. A caller that needs the answer on
// another goroutine owns the goroutine and the context; wrapping Send in an
// unowned goroutine is how a turn outlives the HTTP request that asked for
// it and keeps billing after the operator has navigated away.
func (s *Session) Send(ctx context.Context, prompt string) ([]agent.AgentMessage, error) {
	if s == nil || s.sess == nil {
		return nil, ErrClosed
	}
	return s.sess.Send(ctx, prompt)
}

// Events is the agent loop's stream.
//
// Subscribe before Send. A subscription opened afterwards misses every
// frame of the first turn, which is the turn with the tool calls in it —
// so the console shows a reply that appears from nowhere.
func (s *Session) Events() <-chan agent.AgentEvent {
	if s == nil || s.sess == nil {
		return nil
	}
	return s.sess.Events()
}

// Steer injects a message into a turn already in flight.
func (s *Session) Steer(ctx context.Context, text string) error {
	if s == nil || s.sess == nil {
		return ErrClosed
	}
	// v0.4.0 added a QueuedInputDisposition return. Discarded for the same
	// reason pigrpc.Client.Steer discards it: every disposition means the
	// input was taken, and the difference shows up in the event stream.
	_, err := s.sess.Steer(ctx, text, nil, nil)
	return err
}

// Abort cancels the turn in flight. It is safe to call when nothing is
// running, and safe to call twice.
func (s *Session) Abort(ctx context.Context) error {
	if s == nil || s.sess == nil {
		return ErrClosed
	}
	return s.sess.Abort(ctx)
}

// Running reports whether a turn is in flight.
func (s *Session) Running() bool {
	if s == nil || s.sess == nil {
		return false
	}
	return s.sess.IsStreaming()
}

// WaitIdle blocks until the session has no turn in flight.
//
// A caller that abandons a turn without this is racing: the next prompt is
// queued behind a run the caller believes has finished, and the tool calls
// of both turns interleave in one transcript.
func (s *Session) WaitIdle(ctx context.Context) error {
	if s == nil || s.sess == nil {
		return ErrClosed
	}
	return s.sess.WaitForIdle(ctx)
}

// Messages is a snapshot of the loop's in-memory history.
func (s *Session) Messages() []agent.AgentMessage {
	if s == nil || s.sess == nil {
		return nil
	}
	return s.sess.Messages()
}

// ID is the PiG session id. OpsKeeper records it beside its own session
// row so a transcript can be traced back to the agent that produced it.
func (s *Session) ID() string {
	if s == nil || s.sess == nil {
		return ""
	}
	return s.sess.ID()
}

// Run drives one turn from a caller-supplied opening transcript.
//
// This is PiG's RunAgentPrompt, exposed rather than hidden because
// OpsKeeper's turn is not a string. The host composes the conversation
// itself — it applies the history window, drops superseded tool batches and
// redacts what a viewer may not read — so the messages the loop opens from
// are a control-plane decision that PiG must not be asked to re-derive. The
// obvious alternative, Send, takes a string and starts from an empty
// transcript, which would quietly move the transcript policy upstream of
// the audit and redaction code that owns it.
//
// PiG takes a callback here rather than a message slice, and this method
// keeps the callback inside the boundary rather than passing it through. The
// callback is not a hook — it is the low-level run that seeds the turn, and
// it has to be built against the session's own loop or the run never starts
// and the turn settles empty. A caller cannot build one without reaching
// through Session.Agent(), which is the one thing this package exists to
// stop them doing. Everything the callback form would let a caller vary —
// pre-run compaction checks, seeding from a custom message or from queued
// input — is something OpsKeeper does not do, because it runs with
// NoSession and therefore has no prior assistant to compact.
//
// The Events consumer must be draining and acknowledging while this blocks.
// RunAgentPrompt waits for that consumer at each turn boundary, so a caller
// that has not started one deadlocks rather than merely losing frames.
//
// An empty opening is a caller error and not a silent no-op: the loop would
// have nothing to answer, and a provider would bill for the attempt.
func (s *Session) Run(ctx context.Context, opening []AgentMessage) ([]AgentMessage, error) {
	if s == nil || s.sess == nil {
		return nil, ErrClosed
	}
	if len(opening) == 0 {
		return nil, errors.New("pigcoding: Run requires an opening message")
	}
	return s.sess.RunAgentPrompt(ctx, func(runCtx context.Context) ([]agent.AgentMessage, error) {
		run, err := s.sess.Agent().BeginSendMessages(runCtx, opening)
		if err != nil {
			return nil, err
		}
		return run.Run()
	})
}

// Acknowledge reports whether an event is PiG's internal barrier and, if
// so, releases the run that is waiting on it.
//
// A consumer calls this on every event and skips the ones it returns true
// for. The barrier is not a wire event: it carries no delta, no tool call
// and no usage, and serialising one would put a frame in the console's
// stream that corresponds to nothing the model did. Skipping the call
// instead is not survivable either — the run waits at that barrier before
// it settles, so a consumer that filters events without acknowledging
// them turns every turn into a hang.
//
// The method is nil-safe on a nil Session so a consumer loop can ack
// unconditionally and let the closed case surface as a closed channel.
func (s *Session) Acknowledge(ev AgentEvent) bool {
	if s == nil || s.sess == nil {
		return false
	}
	return coding.AcknowledgeEvent(ev)
}

// AddAfterToolCallHook appends a hook that sees every settled tool call.
//
// It exists because SessionStartOptions has no AfterToolCall panel — only
// BeforeToolCall. The panel it does have would work for neither purpose:
// running the audit write inside BeforeToolCall would record calls that
// were subsequently refused, and running it inside a tool wrapper would
// put a host concern inside the tool the console shows.
//
// The hook is appended, not installed. agent.Agent.SetFinishTurn and its
// siblings replace, and PiG's own Session already calls AddAfterToolCallHook
// for its extension bridge — so an append is the only operation here that
// cannot silently discard a hook somebody else depends on. It must be
// called before the first Send: PiG reads the hook list when the loop runs,
// and a hook added mid-turn applies to the rounds after that one.
func (s *Session) AddAfterToolCallHook(h AfterToolCallHook) error {
	if s == nil || s.sess == nil {
		return ErrClosed
	}
	if h == nil {
		return errors.New("pigcoding: AddAfterToolCallHook requires a hook")
	}
	// Session.Agent is PiG's own accessor for the loop this Session drives.
	// Reaching through it is the only way to add a hook after construction,
	// and it is why this method exists rather than a Start field: a field
	// would have been the cleaner design, and PiG does not have one.
	s.sess.Agent().AddAfterToolCallHook(h)
	return nil
}

// Close releases the session.
//
// Closing a session with a turn still in flight cancels it. That is the
// right default on a server: the alternative is holding a provider
// connection and an extension process alive for a turn whose operator is
// gone.
func (s *Session) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	// Bounded, so a provider that ignores cancellation cannot hold up
	// shutdown. PiG's Abort is already context-aware; this deadline is the
	// backstop for a tool that is not.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.sess.Abort(ctx)
	return s.sess.Close()
}

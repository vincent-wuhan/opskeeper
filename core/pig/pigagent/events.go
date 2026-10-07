package pigagent

import (
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// Mapper turns PiG's agent event stream into the SSE frames the web console
// already parses.
//
// This is the piece that makes the runtime swap invisible to the frontend.
// The console has always received assistant_start / assistant_delta /
// assistant_end / tool_start / tool_end / done / error, and it keeps
// receiving exactly those. Everything PiG-shaped is contained here.
//
// One Mapper serves one session, and it is safe for concurrent use because
// it has to be: PiG runs the sibling tool calls of one assistant turn in
// parallel, and each call's progress update reaches the mapper from its own
// goroutine. The counters below are what the frames carry, so two
// unsynchronised updates would both hand out the same sequence number and
// the console would read a gap as a dropped frame.
type Mapper struct {
	// mu guards every field below plus the frame counters, so a caller may
	// use one Mapper from any goroutine.
	mu        sync.Mutex
	sessionID string
	// iteration is the turn number within the session, 1-based.
	iteration int
	// seq is the per-session monotonic frame counter. A consumer that sees
	// a gap knows a frame was dropped rather than that the turn ended.
	seq int64
	// pending counts tool calls the model asked for that have not settled.
	pending int
	// toolCalls counts every tool invocation the session started, settled
	// or not. The done frame reports it so the console can show a turn's
	// total cost in fan-out without counting tool_end frames itself.
	toolCalls int
	// role is the caller role, echoed into nothing but kept so a future
	// frame can carry it without changing the Mapper.
	role string
	// usage accumulates across the session so the done frame can report a
	// turn total without the console summing assistant frames.
	usage wire.UsageFrame
	// now is injected so tests are not wall-clock dependent.
	now func() time.Time
}

// MapperOptions configures a Mapper.
type MapperOptions struct {
	SessionID string
	Role      string
	// Now defaults to time.Now.
	Now func() time.Time
}

// NewMapper returns a Mapper for one session.
func NewMapper(opts MapperOptions) *Mapper {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Mapper{
		sessionID: opts.SessionID,
		role:      opts.Role,
		now:       now,
	}
}

// Iteration reports the current turn number.
func (m *Mapper) Iteration() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.iteration
}

// SetUsage publishes the turn's accumulated token spend so the done frame
// can report a total.
//
// The mapper cannot compute this itself: usage is observed on the settled
// message, which the run state folds as the turn proceeds. The mapper is
// handed the running total rather than reading PiG types, which keeps the
// frame construction free of provider accounting.
func (m *Mapper) SetUsage(u ports.TranscriptUsage, model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usage = wire.UsageFrame{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens,
		// Cache-write tokens are part of the billable prompt half and the
		// frame has a column for them; the live counter used to stop at
		// cache reads, so a caching provider's turn looked cheaper while it
		// was running than it did after a reload.
		CacheWriteTokens: u.CacheWriteTokens,
		CostUSD:          u.CostUSD,
		Model:            model,
	}
}

// next stamps and returns the next sequence number. Callers hold m.mu.
func (m *Mapper) next() int64 {
	m.seq++
	return m.seq
}

// frameLocked builds a frame with its sequence number. The caller holds
// m.mu.
func (m *Mapper) frameLocked(t wire.StreamEventType) wire.StreamEvent {
	return wire.StreamEvent{
		Type:      t,
		SessionID: m.sessionID,
		Iteration: m.iteration,
		Seq:       m.next(),
	}
}

// frame builds a frame from outside the event mapper, taking the lock.
//
// The two forms exist because the mapper has two callers with different
// locking contracts. Map runs under m.mu for the whole switch, so it uses
// frameLocked. Approval, ApprovalResolved, Error and Notification are
// called by the run state — from a tool's own goroutine, at the moment the
// policy gate asks a human for a decision — and they reach the same counter
// and the same pending count.
//
// They are not the same kind of caller and the difference is not academic:
// the run state writes frames from every tool goroutine at once, while the
// agent's events arrive on the loop's. A single unsynchronised m.seq++ read
// and written by both produces two frames carrying the same number, and
// the console orders by that number — so the failure is a card that renders
// before the tool it belongs to, or not at all, with no error anywhere.
//
// This was a live race, not a theoretical one, and the race detector found
// it the moment a gated tool call ran under a Session driver.
func (m *Mapper) frame(t wire.StreamEventType) wire.StreamEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.frameLocked(t)
}

// TurnStarted advances the turn counter. It is called once per assistant
// round trip, so the console's iteration column matches the number of
// model calls the turn made.
func (m *Mapper) TurnStarted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.iteration++
}

// Map converts one PiG agent event into the frames it should produce.
//
// A nil return means the event carries nothing the console renders: PiG
// emits lifecycle and timing events that have no SSE counterpart, and
// inventing frames for them would change the wire contract.
func (m *Mapper) Map(ev agent.AgentEvent) []wire.StreamEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch e := ev.(type) {

	case agent.TurnStartEvent:
		// The console opens a bubble on assistant_start and leaves it open
		// through the deltas. PendingToolCalls is zero here: the model has
		// not asked for tools yet.
		f := m.frameLocked(wire.StreamAssistantStart)
		f.Assistant = &wire.AssistantFrame{Content: ""}
		return []wire.StreamEvent{f}

	case agent.MessageUpdateEvent:
		// Only text deltas are streamed. Thinking deltas are deliberately
		// dropped: the console renders the answer, and leaking reasoning
		// into a shared incident view is not something the operator asked
		// for.
		delta, ok := e.AssistantMessageEvent.(ai.TextDeltaEvent)
		if !ok || delta.Delta == "" {
			return nil
		}
		f := m.frameLocked(wire.StreamAssistantDelta)
		f.Assistant = &wire.AssistantFrame{Content: delta.Delta}
		return []wire.StreamEvent{f}

	case agent.MessageEndEvent:
		// A system message is not something the assistant said, and a frame
		// claiming otherwise is an empty bubble the console would open and
		// then have to supersede. Only the Session driver produces these:
		// coding.SessionStartOptions.SystemPrompt lands in the transcript as
		// a system entry, and the loop opens and closes its lifecycle like
		// any other message. The bare agent.Agent takes the same prompt as
		// configuration and never emits it, so this path was dead until the
		// SDK driver existed and would have been a difference between the two
		// that looked like a console regression.
		//
		// Tool results are a different case and are deliberately NOT
		// filtered here. PiG settles a tool result as a message of its own,
		// so suppressing those would remove the empty assistant_end frames
		// the console's kernelSink supersede rule is built on. The rule
		// needs them; a system message needs nothing.
		if e.Message.System != nil {
			return nil
		}
		// The assistant message has settled. Its tool calls become the
		// pending count the console renders next to the bubble.
		text, calls := summarize(e.Message)
		m.pending += calls
		f := m.frameLocked(wire.StreamAssistantEnd)
		f.Assistant = &wire.AssistantFrame{
			Content:          stripInlineThinking(text),
			PendingToolCalls: m.pending,
			CreatedAt:        m.now().UTC().Format(time.RFC3339Nano),
		}
		return []wire.StreamEvent{f}

	case agent.ToolExecutionStartEvent:
		if m.pending > 0 {
			m.pending--
		}
		m.toolCalls++
		f := m.frameLocked(wire.StreamToolStart)
		f.Tool = &wire.ToolFrame{
			ToolCallID: e.ToolCallID,
			Name:       e.ToolName,
			Status:     "", // not yet settled
			ArgsJSON:   string(e.Args),
			StartedAt:  m.now().UTC().Format(time.RFC3339Nano),
		}
		return []wire.StreamEvent{f}

	case agent.ToolExecutionUpdateEvent:
		f := m.frameLocked(wire.StreamToolUpdate)
		f.Tool = &wire.ToolFrame{
			ToolCallID: e.ToolCallID,
			Name:       e.ToolName,
			// v0.4.0 replaced the update's `Content` string with
			// `PartialResult`, an AgentToolResult whose Content blocks are a
			// complete-so-far snapshot rather than a delta. The wire field is
			// unchanged, so the mapping is the same read one level deeper,
			// joined by text exactly like the terminal frame below.
			ResultJSON: e.PartialResult.Text(),
		}
		return []wire.StreamEvent{f}

	case agent.ToolExecutionEndEvent:
		// A blocked call is a distinct terminal state from an error: the
		// host policy gate refused it before it ran. The console renders
		// the two differently, so a block must never be reported as an
		// error.
		// v0.4.0 made the event's own `IsError` authoritative (upstream's
		// `event.isError`): it also covers a thrown tool and an
		// afterToolCall hook, neither of which sets Result.IsError.
		status := wire.ToolSuccess
		if e.IsError || e.Result.IsError {
			status = wire.ToolError
		}
		if isBlocked(e.Result) {
			status = wire.ToolBlocked
		}
		ms := e.Duration.Milliseconds()
		if ms < 0 {
			ms = 0
		}
		f := m.frameLocked(wire.StreamToolEnd)
		f.Tool = &wire.ToolFrame{
			ToolCallID: e.ToolCallID,
			Name:       e.ToolName,
			Status:     status,
			ResultJSON: e.Result.Text(),
			Error:      toolErrorText(e.Result),
			EndedAt:    m.now().UTC().Format(time.RFC3339Nano),
			DurationMs: ms,
		}
		return []wire.StreamEvent{f}

	case agent.AgentEndEvent:
		// The run is over. Usage is accumulated across the turn so the
		// console does not have to sum per-message frames.
		usage := m.usage
		frame := m.frameLocked(wire.StreamDone)
		frame.Done = &wire.DoneFrame{
			Iterations: m.iteration,
			ToolCalls:  m.toolCalls,
			Usage:      &usage,
		}
		return []wire.StreamEvent{frame}

	default:
		// AgentStartEvent, AgentSettledEvent, queue updates, session info,
		// and timing events have no SSE counterpart. Returning nil keeps
		// the wire contract exactly as the console already expects it.
		return nil
	}
}

// Error builds the terminal failure frame. It is separate from Map because
// a run can fail without ever reaching AgentEndEvent.
func (m *Mapper) Error(code, message string, retryable bool) wire.StreamEvent {
	f := m.frame(wire.StreamError)
	f.Error = &wire.ErrorFrame{Code: code, Message: message, Retryable: retryable}
	return f
}

// Approval builds the frame that opens an approval affordance in the
// console, bound to the digest of the exact call being approved.
func (m *Mapper) Approval(req ApprovalProjection) wire.StreamEvent {
	f := m.frame(wire.StreamApprovalPending)
	f.Approval = &wire.ApprovalFrame{
		RequestID:   req.RequestID,
		Digest:      req.Digest,
		Tool:        req.Tool,
		Class:       req.Class,
		Summary:     req.Summary,
		BlastRadius: req.BlastRadius,
		Target:      req.Target,
		ExpiresAt:   req.ExpiresAt,
	}
	return f
}

// ApprovalResolved builds the frame that reports a human's decision landing.
func (m *Mapper) ApprovalResolved(requestID, decision, note string) wire.StreamEvent {
	f := m.frame(wire.StreamApprovalResolved)
	f.Approval = &wire.ApprovalFrame{RequestID: requestID, Decision: decision, Note: note}
	return f
}

// Notification builds the inline tile for a background worker reaching a
// terminal state.
func (m *Mapper) Notification(workerID, agentName, status, summary string) wire.StreamEvent {
	f := m.frame(wire.StreamTaskNotification)
	f.Task = &wire.TaskFrame{WorkerID: workerID, Agent: agentName, Status: status, Summary: summary}
	return f
}

// ApprovalProjection is the console-facing view of a pending approval. The
// kernel builds it from the gate's request plus the host-assessed blast
// radius, which the plugin never sets.
type ApprovalProjection struct {
	RequestID   string
	Digest      string
	Tool        string
	Class       string
	Summary     string
	BlastRadius string
	Target      string
	ExpiresAt   string
}

// summarize extracts the reply text and the tool-call count from a settled
// assistant message.
//
// Thinking blocks are excluded: the console renders the answer, and a
// shared incident view is not the place for a model's private reasoning.
func summarize(msg agent.AgentMessage) (string, int) {
	asst := msg.Assistant
	if asst == nil {
		return "", 0
	}
	var calls int
	for _, block := range asst.Content {
		if _, ok := block.(ai.ToolCall); ok {
			calls++
		}
	}
	return ai.ContentText(asst.Content), calls
}

// blockedMarker is the sentinel a host gate writes into a refused tool
// result. The console matches on the status field, not the text; this only
// distinguishes "policy refused" from "the tool failed" inside the kernel.
const blockedMarker = "opskeeper: tool blocked by host policy"

// MarkBlocked tags a tool result as policy-refused. The host gate calls it
// before returning a refusal so the end frame carries ToolBlocked rather
// than ToolError.
func MarkBlocked(result agent.AgentToolResult, reason string) agent.AgentToolResult {
	text := blockedMarker
	if reason != "" {
		text += ": " + reason
	}
	result.IsError = true
	result.Content = append([]ai.ToolResultMessageContent{ai.TextContent{Text: text}}, result.Content...)
	return result
}

func isBlocked(r agent.AgentToolResult) bool {
	return r.IsError && len(r.Content) > 0 && strings.Contains(r.Text(), blockedMarker)
}

// toolErrorText extracts a short error for the console, preferring the
// details payload the gate attached over the model's transcript text.
func toolErrorText(r agent.AgentToolResult) string {
	if !r.IsError {
		return ""
	}
	if s, ok := r.Details.(string); ok && s != "" {
		return s
	}
	text := r.Text()
	if len(text) > 512 {
		text = text[:512] + "…"
	}
	return text
}

package chatruntime

import (
	"context"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/alertdraft"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// kernelsink.go translates the kernel frame vocabulary into the console
// event vocabulary this package already emits.
//
// The two are nearly the same set — assistant / tool_start / tool_end /
// done / error / task_notification — but they are not the same TYPE, and
// the console adapter at the service layer is typed on the second. Doing
// the translation here means the kernel never learns a chatruntime name and
// the service layer never learns a wire frame.
//
// The mapping is deliberately lossy in four places, each for a reason
// stated inline rather than by omission.

// kernelSink implements ports.EventSink over an Emit callback.
//
// It is safe for concurrent use because it has to be: the kernel runs the
// sibling tool calls of one assistant turn in parallel, so two goroutines
// reach Emit at once. The emit callback itself is serialised under the same
// lock — it ultimately writes to an HTTP response, and two concurrent
// writes would interleave two SSE frames into one malformed record.
type kernelSink struct {
	emit Emit

	mu sync.Mutex
	// buffered holds an assistant_end frame whose message id is not yet
	// known. See Emit for why it is held rather than emitted.
	buffered map[string]ports.StreamEvent
	// lastAssistant remembers the most recent terminal assistant frame per
	// session, including the row id flushLocked assigned it.
	//
	// It is separate from buffered because buffered is drained by the
	// flush: the frame the caller wants to read back is precisely the one
	// the flush just released, so a read of buffered after the drain would
	// find nothing and the Reply would fall back to an id the console
	// cannot match.
	lastAssistant map[string]assistantSnapshot
	// toolStarts remembers the admission stamp of every in-flight tool call,
	// keyed by session then by tool call id. The wire contract for tool_end
	// deliberately omits started_at and args_json (see
	// core/pig/pigagent/testdata/mapper-session.golden): the producer emits
	// the admission frame once and the settle frame once, and it is the
	// consumer's job to join them. Without this memory the settled tile
	// carries a zero start time and the console's timeline reads 1970.
	//
	// Nested by session so the map can be dropped wholesale when a session
	// ends: a tool call that never settles would otherwise pin its entry for
	// the lifetime of the process, and a long-lived console process serves
	// every session.
	toolStarts map[string]map[string]toolStartInfo
	// guard decides whether a model-authored alert-rule draft may be shown as
	// a confirmable one. It is a second instance of the rule the transcript
	// write path applies, and it has to be here as well because the frames
	// are produced before the row is written: sanitising only on the write
	// path would show the operator a draft the transcript does not contain.
	// Both instances derive their state from the same ordered event stream.
	guard *alertdraft.Guard
}

// assistantSnapshot is a terminal assistant frame reduced to the three
// fields a caller needs to build the console Reply.
type assistantSnapshot struct {
	// Content is the full turn text the model produced.
	Content string
	// MessageID is the chat_messages row the turn was committed under. It
	// is empty when no row was written, and the console then falls back to
	// an iteration-keyed bubble.
	MessageID string
	// CreatedAt is the row's timestamp. A zero value means the frame
	// carried no parseable stamp.
	CreatedAt time.Time
}

// toolStartInfo is the part of an admission frame that the settle frame
// does not repeat.
type toolStartInfo struct {
	startedAt string
	argsJSON  string
}

// newKernelSink returns a sink over emit. A nil emit yields a sink that
// buffers and discards, so a blocking (non-streaming) call does not have to
// branch at every call site.
func newKernelSink(emit Emit) *kernelSink {
	return &kernelSink{
		emit:          emit,
		guard:         alertdraft.NewGuard(),
		buffered:      make(map[string]ports.StreamEvent),
		lastAssistant: make(map[string]assistantSnapshot),
		toolStarts:    make(map[string]map[string]toolStartInfo),
	}
}

// Emit implements ports.EventSink.
func (s *kernelSink) Emit(ctx context.Context, ev ports.StreamEvent) error {
	userText, _ := basetool.TurnUserTextFromContext(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	switch ev.Type {
	case wire.StreamAssistantEnd:
		// Rewritten before it is held: the alert-draft rule decides what the
		// operator may read, and the held frame is what the console renders.
		// The frame is copied rather than edited in place — the kernel may
		// hand the same value to a second consumer.
		if ev.Assistant != nil {
			sanitized := *ev.Assistant
			sanitized.Content = s.guard.Sanitize(ev.SessionID, userText, sanitized.Content)
			ev.Assistant = &sanitized
		}
		// Held, not emitted. The frame carries the persisted chat_messages
		// id, and the row is written one step later — the kernel emits the
		// event and only then hands the message to the persister. Emitting
		// now would ship an empty id and the console would key the bubble
		// on a synthetic iteration string, which stops matching the real id
		// as soon as history reloads, so the bubble is replaced rather
		// than updated. FlushAssistant emits it with the real id.
		//
		// Only the newest frame is kept: a second assistant_end for the
		// same session supersedes the first, which is exactly what the
		// console renders.
		s.buffered[ev.SessionID] = ev
		s.rememberAssistantLocked(ev)
		return nil

	case wire.StreamAssistantStart, wire.StreamAssistantDelta:
		// The console has no bubble to open on assistant_start (it renders
		// the settled turn) and does not yet consume token deltas. Emitting
		// either would show a phantom empty bubble. The frames still exist
		// on the wire for a future console that renders streaming text.
		return nil

	case wire.StreamToolUpdate:
		// The console tool tile fills in at tool_end. A progress frame
		// would be a second, unused path for the same content.
		return nil

	case wire.StreamToolStart:
		// Remembered, not just emitted: the matching tool_end omits the
		// admission stamp, so the join has to happen here.
		s.rememberToolStartLocked(ev)

	case wire.StreamToolEnd:
		// A settled draft_config_change is what makes the surrounding prose a
		// confirmable draft; observed before the settle frame is emitted so a
		// batch that produces the draft and the summary in one turn is
		// judged in order.
		if ev.Tool != nil {
			s.guard.ObserveTool(ev.SessionID, userText, ev.Tool.Name, ev.Tool.ResultJSON)
		}
		// Joined with the admission frame so the settled tile carries the
		// start time and arguments the console pairs with the result.
		ev = s.mergeToolEndLocked(ev)

	case wire.StreamDone:
		// Handle emits the terminal Done itself, so the caller gets a
		// single well-typed event carrying the full Reply. The kernel done
		// frame has no Reply, so relaying it would double-fire.
		//
		// The session is over, so its in-flight start records are dropped:
		// a tool whose settle frame never arrived is not going to settle,
		// and holding its stamp would leak one entry per abandoned call.
		delete(s.toolStarts, ev.SessionID)
		s.flushLocked(ev.SessionID, "")
		return nil

	case wire.StreamApprovalPending, wire.StreamApprovalResolved:
		// Approval frames are produced by the kernel own approval gate.
		// The manager path does not install one: the blocking write tools
		// (cloud_bash, apply_config_change) propose-and-confirm inside the
		// tool and surface their card through EmitFromContext with a
		// richer payload than this frame carries. Relaying both would show
		// two cards for one approval. When a manager-side gate is wired,
		// this is the branch to extend.
		return nil
	}

	// Any other frame arrives after the assistant turn it belongs to, so
	// flushing first preserves the console ordering.
	s.flushLocked(ev.SessionID, "")
	if s.emit == nil {
		return nil
	}
	s.emit(toConsoleEvent(ev))
	return nil
}

// FlushAssistant emits the held assistant_end frame with its committed row
// id. It is called by the persister once the row is written, and is a no-op
// when there is nothing held (persistence disabled, or the frame already
// flushed).
func (s *kernelSink) FlushAssistant(sessionID, messageID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked(sessionID, messageID)
}

// flushLocked emits the held frame for one session. Callers hold s.mu.
func (s *kernelSink) flushLocked(sessionID, messageID string) {
	ev, ok := s.buffered[sessionID]
	if !ok {
		return
	}
	delete(s.buffered, sessionID)
	if ev.Assistant != nil {
		// An empty id means no row was written — persistence is off, or
		// the write failed. The console falls back to an iteration-keyed
		// bubble, which is the documented behaviour for a session with no
		// transcript.
		ev.Assistant.MessageID = messageID
	}
	// The snapshot is the frame the caller reads back after the turn
	// settles, so it has to carry the same id the emitted frame does.
	if snap, ok := s.lastAssistant[sessionID]; ok {
		snap.MessageID = messageID
		s.lastAssistant[sessionID] = snap
	}
	if s.emit == nil {
		return
	}
	s.emit(toConsoleEvent(ev))
}

// LastAssistant returns the most recent terminal assistant frame for a
// session. ok is false when no assistant turn settled, which is what a
// turn that failed before the model answered looks like.
func (s *kernelSink) LastAssistant(sessionID string) (assistantSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.lastAssistant[sessionID]
	return snap, ok
}

// rememberAssistantLocked records the content and timestamp of a terminal
// assistant frame. Callers hold s.mu. The id is left as the frame carried
// it — empty until flushLocked assigns the committed row id.
func (s *kernelSink) rememberAssistantLocked(ev ports.StreamEvent) {
	if ev.Assistant == nil {
		return
	}
	s.lastAssistant[ev.SessionID] = assistantSnapshot{
		Content:   ev.Assistant.Content,
		MessageID: ev.Assistant.MessageID,
		CreatedAt: parseTime(ev.Assistant.CreatedAt),
	}
}

// rememberToolStartLocked records the admission stamp of one tool call.
// Callers hold s.mu.
func (s *kernelSink) rememberToolStartLocked(ev ports.StreamEvent) {
	if ev.Tool == nil {
		return
	}
	byCall := s.toolStarts[ev.SessionID]
	if byCall == nil {
		byCall = make(map[string]toolStartInfo)
		s.toolStarts[ev.SessionID] = byCall
	}
	byCall[ev.Tool.ToolCallID] = toolStartInfo{
		startedAt: ev.Tool.StartedAt,
		argsJSON:  ev.Tool.ArgsJSON,
	}
}

// mergeToolEndLocked returns ev with the fields the settle frame does not
// repeat filled in from the matching admission frame. Callers hold s.mu.
//
// The frame is copied rather than edited in place: the caller still owns
// the value it passed in, and the kernel may hand the same frame to a
// second consumer.
func (s *kernelSink) mergeToolEndLocked(ev ports.StreamEvent) ports.StreamEvent {
	if ev.Tool == nil {
		return ev
	}
	byCall := s.toolStarts[ev.SessionID]
	if byCall == nil {
		return ev
	}
	info, ok := byCall[ev.Tool.ToolCallID]
	if !ok {
		return ev
	}
	delete(byCall, ev.Tool.ToolCallID)
	if len(byCall) == 0 {
		delete(s.toolStarts, ev.SessionID)
	}

	merged := *ev.Tool
	// Only fill blanks: a frame that carries its own values is the better
	// source, and overwriting would silently discard a producer that
	// decided to send them.
	if merged.StartedAt == "" {
		merged.StartedAt = info.startedAt
	}
	if merged.ArgsJSON == "" {
		merged.ArgsJSON = info.argsJSON
	}
	ev.Tool = &merged
	return ev
}

// toConsoleEvent maps one frame. A frame with no console counterpart
// yields a zero Event, which the service-layer adapter drops.
func toConsoleEvent(ev ports.StreamEvent) Event {
	switch ev.Type {
	case wire.StreamAssistantEnd:
		out := Event{Type: EventAssistant}
		if ev.Assistant != nil {
			out.Assistant = &AssistantEvent{
				Iteration:        ev.Iteration,
				MessageID:        ev.Assistant.MessageID,
				Content:          ev.Assistant.Content,
				CreatedAt:        parseTime(ev.Assistant.CreatedAt),
				PendingToolCalls: ev.Assistant.PendingToolCalls,
			}
		}
		return out

	case wire.StreamToolStart, wire.StreamToolEnd:
		out := Event{Type: EventToolStart}
		if ev.Type == wire.StreamToolEnd {
			out.Type = EventToolEnd
		}
		if ev.Tool != nil {
			out.Tool = &ToolEvent{
				ToolCallID: ev.Tool.ToolCallID,
				Name:       ev.Tool.Name,
				DeviceID:   ev.Tool.DeviceID,
				Status:     string(ev.Tool.Status),
				StartedAt:  parseTime(ev.Tool.StartedAt),
				EndedAt:    parseOptionalTime(ev.Tool.EndedAt),
				DurationMs: ev.Tool.DurationMs,
				Error:      ev.Tool.Error,
				ArgsJSON:   ev.Tool.ArgsJSON,
				ResultJSON: ev.Tool.ResultJSON,
			}
		}
		return out

	case wire.StreamError:
		out := Event{Type: EventError}
		if ev.Error != nil {
			out.Error = ev.Error.Message
		}
		return out

	case wire.StreamTaskNotification:
		out := Event{Type: EventTaskNotification}
		if ev.Task != nil {
			out.Notification = &TaskNotification{
				TaskID:  ev.Task.WorkerID,
				Status:  WorkerStatus(ev.Task.Status),
				Summary: ev.Task.Summary,
			}
		}
		return out
	}
	return Event{}
}

// parseTime reads an RFC3339Nano stamp off a frame. An unparseable value
// becomes the zero time rather than "now": a fabricated timestamp would
// make a stale frame look fresh in the console.
func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseOptionalTime is parseTime for a field whose absence is meaningful:
// nil means the call has not ended, which the console renders as a running
// tile. Returning a zero time instead would render it as ended in 1970.
func parseOptionalTime(v string) *time.Time {
	if v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil
	}
	return &t
}

var _ ports.EventSink = (*kernelSink)(nil)

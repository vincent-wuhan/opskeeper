// Package pigwire translates a node agent's RPC event stream into the frame
// vocabulary the web console already parses.
//
// The node agent runs `pig --mode rpc` and speaks PiG's own event names —
// turn_start, message_update, tool_execution_end. The console has always
// received assistant_start, assistant_delta, tool_end, done. Translating on
// the node, before the event crosses the tunnel, is what lets that stay
// true: the tunnel carries a frame the manager and the console already
// understand, and neither of them learns that a node agent exists.
//
// Translating here rather than in the control plane also keeps the knowledge
// in the module built to absorb it. PiG is pre-stable and renames things;
// when it does, this file changes and the console, the manager, and every
// node binary do not.
package pigwire

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// blockedMarker is the sentinel a host policy gate writes into a refused
// tool result. It is recognised here so a refusal on a node is reported as
// ToolBlocked rather than ToolError — the console renders the two
// differently, and collapsing them would tell an operator their read-only
// tool is broken.
const blockedMarker = "opskeeper: tool blocked by host policy"

// Translator converts one session's events.
//
// It owns per-session counters, so it is not safe for concurrent use. One
// translator serves one conversation; a node running several conversations
// uses several translators.
type Translator struct {
	sessionID string
	iteration int
	seq       int64
	// pending counts tool calls the model asked for that have not settled,
	// and toolCalls counts every invocation started.
	pending   int
	toolCalls int
	// usage accumulates across the session so the done frame reports a
	// total without the console summing assistant frames.
	usage wire.UsageFrame
	now   func() time.Time
}

// Options configures a Translator.
type Options struct {
	// SessionID scopes every frame. Required.
	SessionID string
	// Now defaults to time.Now.
	Now func() time.Time
}

// New returns a Translator for one session.
func New(opts Options) *Translator {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Translator{sessionID: opts.SessionID, now: now}
}

// Iteration reports the current turn number.
func (t *Translator) Iteration() int { return t.iteration }

func (t *Translator) frame(kind wire.StreamEventType) wire.StreamEvent {
	t.seq++
	return wire.StreamEvent{
		Type:      kind,
		SessionID: t.sessionID,
		Iteration: t.iteration,
		Seq:       t.seq,
	}
}

// Translate converts one raw agent event into the frames it should produce.
//
// A nil return means the event carries nothing the console renders. PiG
// emits lifecycle, queue, and compaction events with no counterpart in the
// console's contract, and inventing frames for them would change that
// contract. An event whose shape cannot be read is treated the same way —
// see Unknown.
func (t *Translator) Translate(eventType string, raw []byte) []wire.StreamEvent {
	switch eventType {
	case "turn_start":
		t.iteration++
		f := t.frame(wire.StreamAssistantStart)
		f.Assistant = &wire.AssistantFrame{}
		return []wire.StreamEvent{f}

	case "message_update":
		return t.translateUpdate(raw)

	case "message_end":
		return t.translateMessageEnd(raw)

	case "tool_execution_start":
		if t.pending > 0 {
			t.pending--
		}
		t.toolCalls++
		var ev struct {
			ToolCallID string          `json:"toolCallId"`
			ToolName   string          `json:"toolName"`
			Args       json.RawMessage `json:"args"`
		}
		_ = json.Unmarshal(raw, &ev)
		f := t.frame(wire.StreamToolStart)
		f.Tool = &wire.ToolFrame{
			ToolCallID: ev.ToolCallID,
			Name:       ev.ToolName,
			ArgsJSON:   string(ev.Args),
			StartedAt:  t.now().UTC().Format(time.RFC3339Nano),
		}
		return []wire.StreamEvent{f}

	case "tool_execution_update":
		var ev struct {
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
			Partial    struct {
				Content []contentBlock `json:"content"`
			} `json:"partialResult"`
		}
		_ = json.Unmarshal(raw, &ev)
		f := t.frame(wire.StreamToolUpdate)
		f.Tool = &wire.ToolFrame{
			ToolCallID: ev.ToolCallID,
			Name:       ev.ToolName,
			ResultJSON: joinText(ev.Partial.Content),
		}
		return []wire.StreamEvent{f}

	case "tool_execution_end":
		return t.translateToolEnd(raw)

	case "agent_end":
		usage := t.usage
		f := t.frame(wire.StreamDone)
		f.Done = &wire.DoneFrame{Iterations: t.iteration, ToolCalls: t.toolCalls, Usage: &usage}
		return []wire.StreamEvent{f}

	default:
		// message_start, agent_settled, turn_end, entry_appended, the
		// summarization and bash events: all real, none renderable.
		return nil
	}
}

// translateUpdate handles message_update, the one event that carries a
// nested provider event.
//
// Only text deltas become frames. Thinking deltas are dropped on purpose:
// the console renders the answer, and leaking a model's private reasoning
// into a shared incident view is not something the operator asked for.
func (t *Translator) translateUpdate(raw []byte) []wire.StreamEvent {
	var ev struct {
		Assistant struct {
			Type         string `json:"type"`
			Delta        string `json:"delta"`
			ContentIndex int    `json:"contentIndex"`
		} `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil
	}
	if ev.Assistant.Type != "text_delta" || ev.Assistant.Delta == "" {
		return nil
	}
	f := t.frame(wire.StreamAssistantDelta)
	f.Assistant = &wire.AssistantFrame{Content: ev.Assistant.Delta}
	return []wire.StreamEvent{f}
}

// translateMessageEnd handles message_end, which carries the settled
// assistant message.
//
// Its tool-call blocks become the pending count the console renders next to
// the bubble. Thinking blocks are excluded: the console renders the answer,
// and a shared incident view is not the place for a model's reasoning.
func (t *Translator) translateMessageEnd(raw []byte) []wire.StreamEvent {
	var ev struct {
		Message struct {
			Content []contentBlock `json:"content"`
			// Model sits beside usage on the message, not inside it.
			Model string       `json:"model"`
			Usage *usageRecord `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil
	}

	var text, thinking strings.Builder
	var calls int
	for _, block := range ev.Message.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
			thinking.WriteString(block.Thinking)
		case "toolCall":
			calls++
		}
	}
	t.pending += calls
	t.foldUsage(ev.Message.Usage, ev.Message.Model)

	f := t.frame(wire.StreamAssistantEnd)
	f.Assistant = &wire.AssistantFrame{
		Content:          text.String(),
		PendingToolCalls: t.pending,
		CreatedAt:        t.now().UTC().Format(time.RFC3339Nano),
	}
	return []wire.StreamEvent{f}
}

// translateToolEnd handles tool_execution_end, the one event whose terminal
// classification the console branches on.
func (t *Translator) translateToolEnd(raw []byte) []wire.StreamEvent {
	var ev struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		IsError    bool   `json:"isError"`
		Result     struct {
			Content []contentBlock `json:"content"`
			Details any            `json:"details"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &ev)

	text := joinText(ev.Result.Content)
	status := wire.ToolSuccess
	if ev.IsError {
		status = wire.ToolError
	}
	// A refusal by a node-side host gate is a distinct terminal state from
	// a failure: the console renders the two differently.
	if ev.IsError && strings.Contains(text, blockedMarker) {
		status = wire.ToolBlocked
	}

	f := t.frame(wire.StreamToolEnd)
	f.Tool = &wire.ToolFrame{
		ToolCallID: ev.ToolCallID,
		Name:       ev.ToolName,
		Status:     status,
		ResultJSON: text,
		Error:      toolErrorText(ev, text, status),
		EndedAt:    t.now().UTC().Format(time.RFC3339Nano),
	}
	return []wire.StreamEvent{f}
}

// foldUsage accumulates a settled message's spend.
//
// The agent reports cumulative totals per message, so the running total is
// the last message's, not the sum: summing would multiply every token by
// the number of round trips.
func (t *Translator) foldUsage(usage *usageRecord, model string) {
	if usage == nil {
		// The model is still worth recording: a message can settle with
		// no usage block and a cost line that names no model is worse
		// than one that names a cheap one.
		t.usage.Model = model
		return
	}
	t.usage = wire.UsageFrame{
		InputTokens:      usage.Input,
		OutputTokens:     usage.Output,
		CacheReadTokens:  usage.CacheRead,
		CacheWriteTokens: usage.CacheWrite,
		CostUSD:          usage.Cost.Total,
		Model:            model,
	}
}

// SetModel records which model answered, for the done frame's cost line.
func (t *Translator) SetModel(model string) { t.usage.Model = model }

// contentBlock is the subset of an assistant or tool-result content block
// this translator reads. Decoding into a narrow struct rather than the
// agent's own types is deliberate: a block the translator does not use must
// not be a reason to fail, and the agent's block types carry streaming
// scratch fields that mean nothing here.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	ID       string          `json:"id"`
	Args     json.RawMessage `json:"arguments"`
}

// usageRecord is the agent's per-message usage report.
type usageRecord struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	CacheRead int `json:"cacheRead"`
	// CacheWrite mirrors PiG's ai.Usage.CacheWrite. It was missing here as
	// well as in the frame, so the loss happened twice on this path: the
	// unmarshalled record could not carry it even once the frame had a
	// column to put it in.
	CacheWrite int `json:"cacheWrite"`
	Cost       struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// joinText concatenates the text blocks of a content list, which is how a
// tool result's text is addressed.
func joinText(blocks []contentBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// toolErrorText extracts a short error for the console, preferring the
// agent's own details over the transcript text so the operator sees the
// real cause. It is truncated: a runaway tool must not be able to flood the
// console through this field.
func toolErrorText(ev struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
	Result     struct {
		Content []contentBlock `json:"content"`
		Details any            `json:"details"`
	} `json:"result"`
}, text string, status wire.ToolStatus) string {
	if status == wire.ToolSuccess {
		return ""
	}
	if s, ok := ev.Result.Details.(string); ok && s != "" {
		return truncate(s)
	}
	return truncate(text)
}

// truncate caps an error string at a length a console can render.
func truncate(s string) string {
	const limit = 512
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// Error builds the terminal failure frame.
//
// It is exported for the node's transport, which knows about failures the
// agent itself never reported: a tunnel that dropped, a process that died
// mid-turn. A turn that ended with no done frame and no error frame is a
// console spinner that never stops, so the transport supplies one.
func (t *Translator) Error(code, message string, retryable bool) wire.StreamEvent {
	f := t.frame(wire.StreamError)
	f.Error = &wire.ErrorFrame{Code: code, Message: message, Retryable: retryable}
	return f
}

// Unknown describes an event the translator could not place.
//
// It is a report, not a frame. Dropping a frame the console never received
// leaves a gap nobody can explain, so a node counts these and a manager can
// surface the count on the node's page. A growing count means the agent's
// vocabulary has moved and the translator is behind it.
type Unknown struct {
	// Type is the agent's event name.
	Type string
	// Reason is why it could not be placed: "no_console_counterpart" for
	// a lifecycle event, "unreadable" for one whose shape did not parse.
	Reason string
}

// Reasons an event can go unplaced.
const (
	ReasonNoCounterpart = "no_console_counterpart"
	ReasonUnreadable    = "unreadable"
)

// Classify reports whether an event type has a console counterpart at all.
//
// It is separated from Translate so a caller can account for the difference
// between "this event is not for the console" — most of them, and entirely
// normal — and "this event was for the console and we could not read it",
// which is a real fault.
func Classify(eventType string) (known bool, renders bool) {
	switch eventType {
	case "turn_start", "message_update", "message_end",
		"tool_execution_start", "tool_execution_update",
		"tool_execution_end", "agent_end":
		return true, true
	case "message_start", "agent_settled", "turn_end", "entry_appended":
		return true, false
	default:
		// An event name this build has never heard of. It may be a new
		// renderable event or a new lifecycle one; the honest answer is
		// neither.
		return false, false
	}
}

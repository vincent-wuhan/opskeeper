// chat.go is the one place OpsKeeper calls a model, and it speaks PiG's
// vocabulary because there is no second one.
//
// Before this file was rewritten, OpsKeeper carried a parallel set of model
// types — ports.LLMRequest, ports.LLMResponse, ports.Conversation,
// ports.LLMMessage — and converted them into ai.* on every call. That layer
// bought one thing: callers could name a request without importing PiG. It
// cost far more than that. Every field had to be declared twice, and every
// conversion was a place a tool_call id or a thinking block could be dropped
// silently: the symptom of a bad conversion is a model that quietly stops
// calling tools, not an error. There is no OpsKeeper-shaped request type here
// now. A caller that wants a completion builds ai.Message values, hands them
// over, and reads an *ai.AssistantMessage back.
//
// The type surface this file adds is therefore small on purpose: Request says
// which model and carries a transcript that is already PiG's; the Reply
// helpers read the two things every caller wants off a reply. Anything more
// would be a second vocabulary wearing a PiG hat.
package pigmodel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Request is one model call.
//
// Every field except Selection is already PiG's, so a caller with a
// transcript from a previous turn, from persistence, or from a tool result
// has nothing to translate. Selection stays an OpsKeeper value because
// "which of our configured providers" is a host question PiG has no answer
// to — PiG resolves a model from its own config, and OpsKeeper resolves
// models from the admin settings table.
type Request struct {
	// Selection pins the model. An empty selection resolves to the cluster
	// default provider's default model.
	Selection domain.ModelSelection
	// SystemPrompt is folded into a leading system message by
	// ai.NormalizeContext. Leave it empty when the caller already carries
	// the system turn in Messages; passing both produces two system
	// messages, which providers concatenate in an unspecified order.
	SystemPrompt string
	// Messages is the transcript, oldest first. It is ai.Context.Messages,
	// not a port type, and every value in it is a PiG message union member.
	Messages []ai.Message
	// Tools are the model-facing tool declarations. Host governance fields
	// — class, origin, when_to_use — are not here: see ToolSchemas for the
	// bridge from the host catalogue to this slice.
	Tools []ai.ToolSchema
	// SessionID is the opaque prompt-cache key forwarded to the provider.
	// It must never be a user or tenant identifier: providers key their
	// cache on it and it is echoed on the wire.
	SessionID string
	// Tune adjusts the resolved stream options after the registry has
	// filled in the per-request credential, the model cost and the default
	// session id. It is the seam for the knobs a single call needs —
	// temperature, thinking level, max tokens, tool choice — without every
	// caller having to learn the full ai.StreamOptions surface.
	//
	// Nil is the common case.
	Tune func(*ai.StreamOptions)
}

// Completer is the model port every other package takes by injection.
//
// It is one method on purpose. A judge, a translator and a phase worker all
// want exactly "this transcript, give me the reply", and the moment a port
// grows an Available or a Resolve alongside it, everything that only wanted
// a completion is also being asked to answer questions about provider
// configuration it has no use for. That is why the registry satisfies this
// interface and why nothing else has to know it is a registry.
type Completer interface {
	Complete(ctx context.Context, req Request) (*ai.AssistantMessage, error)
}

// Compile-time proof the registry is the completer every host wires.
var _ Completer = (*Registry)(nil)

// Complete resolves the request to a model, runs it, and returns PiG's own
// settled reply.
//
// A nil model, a provider that returns no stream, and a stream that settles
// with no choices are all errors rather than empty replies. Every caller of
// this function either bills the request or reports a turn to an operator,
// and all three are lies told about a call that never happened.
func (r *Registry) Complete(ctx context.Context, req Request) (*ai.AssistantMessage, error) {
	model, opts, err := r.Model(ctx, req.Selection)
	if err != nil {
		return nil, fmt.Errorf("pigmodel: resolve model: %w", err)
	}

	// A request that pinned its own cache key wins over the registry's
	// provider-scoped default: the caller is the only party that knows
	// whether two calls belong in the same cache bucket.
	if req.SessionID != "" {
		opts.SessionID = req.SessionID
	}
	if req.Tune != nil {
		req.Tune(&opts)
	}

	transcript, err := r.Transcript(req)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	stream, err := model.Provider.Stream(ctx, transcript, opts)
	if err != nil {
		r.observe(model, "error", time.Since(start).Seconds(), 0, 0)
		return nil, fmt.Errorf("pigmodel: chat completion: %w", err)
	}
	msg, err := Settle(stream, ctx)
	if err != nil {
		r.observe(model, "error", time.Since(start).Seconds(), 0, 0)
		return nil, err
	}
	// Usage is read through ReplyUsage rather than off the message type, so
	// a provider that grows a new component of its usage record is counted
	// here by construction instead of by a second implementation of the
	// same accessor.
	usage := ReplyUsage(msg)
	r.observe(model, "ok", time.Since(start).Seconds(), usage.Input, usage.Output)
	return msg, nil
}

// Transcript normalises a request into PiG's provider-facing transcript.
//
// It is exported because a caller that needs to inspect what would be sent —
// a dry run, an audit row, a test asserting the exact prompt — must be able
// to ask without issuing the call. Returning ai.TranscriptContext rather than
// a renderable string is deliberate: the type cannot be constructed outside
// PiG, so exporting a function that returns it hands out a value a caller can
// only forward, never fabricate.
func (r *Registry) Transcript(req Request) (ai.TranscriptContext, error) {
	return NewTranscript(req)
}

// NewTranscript builds a provider-facing transcript from a Request.
//
// ai.TranscriptContext keeps a validation error private — it has no Err
// accessor, by design, because a transcript either normalises or it does not
// exist. So the error is recovered here: NormalizeContext returns a context
// whose Messages() is nil, and a request that went in with messages and
// comes out with none failed validation. Catching it here means the caller
// learns which request was malformed instead of sending a provider a
// transcript the provider will reject with a 400 that names no message
// index.
func NewTranscript(req Request) (ai.TranscriptContext, error) {
	// Two system messages is not a shape PiG refuses — it prepends the
	// SystemPrompt and leaves the caller's own system turn in place, and
	// providers concatenate the two in an order none of them specify. The
	// result is a model that was told two personas and obeyed whichever one
	// landed second, which is the hardest class of bug to diagnose from a
	// transcript: nothing failed, and the system prompt in the log looks
	// exactly like the one that was sent. Rejecting it here costs one
	// branch; recovering from it costs an afternoon.
	if req.SystemPrompt != "" {
		for _, msg := range req.Messages {
			if _, ok := msg.(ai.SystemMessage); ok {
				return ai.TranscriptContext{}, errors.New(
					"pigmodel: Request carries both SystemPrompt and a system message; " +
						"keep the persona in exactly one of them")
			}
		}
	}

	transcript := ai.NormalizeContext(ai.Context{
		SystemPrompt: req.SystemPrompt,
		Messages:     req.Messages,
		Tools:        req.Tools,
	})
	if len(transcript.Messages()) == 0 && len(req.Messages) > 0 {
		return ai.TranscriptContext{}, fmt.Errorf(
			"pigmodel: transcript of %d message(s) was refused as malformed; "+
				"a tool result with no matching tool call, or tool arguments that are not a JSON object, "+
				"is the usual cause", len(req.Messages))
	}
	return transcript, nil
}

// Settle drains a PiG stream to its terminal message.
//
// Streaming has no consumer on this path: the contract is one complete
// assistant turn, so deltas are discarded rather than forwarded. PiG's stream
// already applies backpressure by dropping frames for a slow reader, so
// draining without work is cheap.
func Settle(stream *ai.AssistantMessageEventStream, ctx context.Context) (*ai.AssistantMessage, error) {
	if stream == nil {
		return nil, fmt.Errorf("pigmodel: provider returned no stream")
	}
	// The caller's context, not a fresh Background: a cancelled turn must
	// stop waiting on a provider that will never answer. Draining under
	// Background would keep the caller's goroutine alive until the provider's
	// own read timeout fires.
	if ctx == nil {
		ctx = context.Background()
	}
	settled, err := stream.ResultContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("pigmodel: chat completion: %w", err)
	}
	if settled == nil {
		return nil, fmt.Errorf("pigmodel: empty choices in response")
	}
	// ResultContext reports the stream's terminal value and nothing else:
	// a provider that refused the request settles into an AssistantMessage
	// carrying StopReasonError and the provider's own text, and
	// ResultContext hands that back with a nil error. So a 401, a 429 and
	// a 500 all used to reach every caller as a successful, empty turn --
	// the one shape that is hardest to diagnose, because the caller's
	// schema check then reports "the model did not return JSON" and points
	// at the prompt instead of at the key that expired an hour ago.
	//
	// This is the third of the three lies the function's own comment names
	// -- a stream that settles into an error rather than into a reply. The
	// other two are checked above; this one was checked nowhere.
	if err := ProviderTurnError(settled); err != nil {
		return nil, err
	}
	return settled, nil
}

// ProviderTurnError reports whether a settled turn produced a reply or an
// excuse, and is the whole of Settle's third check.
//
// It is exported and takes a message rather than a stream because the
// distinction is worth testing directly: a stream that ends in a tool call,
// in a truncated turn, or in a clean stop is a normal turn, and only the
// two reasons that mean "nothing was produced" are a provider failure.
// Reading the value rather than the stream is also what the upstream
// ResultContext forces -- it reports the terminal message and a nil error,
// so the reason on that message is the only signal left.
func ProviderTurnError(settled *ai.AssistantMessage) error {
	if settled == nil {
		return nil
	}
	if settled.StopReason != ai.StopReasonError && settled.StopReason != ai.StopReasonAborted {
		return nil
	}
	return fmt.Errorf("pigmodel: %s rejected the turn (%s/%s): %s",
		settled.StopReason, settled.Provider, settled.Model, settled.ErrorMessage)
}

// ReplyText is the reply's text, with every text block concatenated in the
// order the model emitted them.
//
// A reply that asked for tools and said nothing returns "", which is correct:
// a tool-only turn is the normal shape of an agent step, and treating its
// silence as an error would fail every turn that begins by acting.
func ReplyText(msg *ai.AssistantMessage) string {
	if msg == nil {
		return ""
	}
	var b []byte
	for _, block := range msg.Content {
		if text, ok := block.(ai.TextContent); ok {
			b = append(b, text.Text...)
		}
	}
	return string(b)
}

// MessageText renders any message in a transcript as plain text.
//
// It is here, once, because "what did this message say" has four different
// answers depending on which of PiG's four message types you are holding, and
// PiG's own ai.ContentText is generic over the *content* unions rather than
// over ai.Message. Every call site that grew its own type switch grew its own
// idea of what to do with a content shape it did not recognise — silently
// dropping it — which is exactly the failure a judge or a replay path cannot
// afford: the block it dropped is the one the model actually read.
//
// Text blocks join with newlines so a multi-block message stays readable in a
// log line. Non-text blocks (images) contribute nothing rather than a
// placeholder: this is for prompts and diagnostics, not for re-encoding.
func MessageText(msg ai.Message) string {
	switch m := msg.(type) {
	case nil:
		return ""
	case ai.SystemMessage:
		return systemContentText(m.Content)
	case ai.UserMessage:
		return userContentText(m.Content)
	case ai.AssistantMessage:
		return ReplyText(&m)
	case ai.ToolResultMessage:
		return toolResultText(m.Content)
	default:
		return ""
	}
}

func systemContentText(content ai.SystemContent) string {
	switch c := content.(type) {
	case ai.SystemText:
		return string(c)
	case ai.SystemTextBlocks:
		var out []string
		for _, block := range c {
			out = append(out, block.Text)
		}
		return strings.Join(out, "\n")
	default:
		return ""
	}
}

func userContentText(content ai.UserContent) string {
	switch c := content.(type) {
	case ai.UserText:
		return string(c)
	case ai.UserContentBlocks:
		var out []string
		for _, block := range c {
			if text, ok := block.(ai.TextContent); ok {
				out = append(out, text.Text)
			}
		}
		return strings.Join(out, "\n")
	default:
		return ""
	}
}

func toolResultText(content []ai.ToolResultMessageContent) string {
	var out []string
	for _, block := range content {
		if text, ok := block.(ai.TextContent); ok {
			out = append(out, text.Text)
		}
	}
	return strings.Join(out, "\n")
}

// ReplyToolCalls returns the tool invocations the model asked for.
//
// The returned slice is freshly allocated, so a caller may sort or filter it
// without disturbing the reply. A reply with no tool calls returns nil rather
// than an empty slice, so "the model did not ask for anything" is a nil
// check and not a length check against a zero that has to be told apart from
// a provider that returned nothing at all.
func ReplyToolCalls(msg *ai.AssistantMessage) []ai.ToolCall {
	if msg == nil {
		return nil
	}
	var calls []ai.ToolCall
	for _, block := range msg.Content {
		if call, ok := block.(ai.ToolCall); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

// ReplyUsage is the reply's token accounting, or a zero Usage when the
// provider reported none.
//
// A zero is honest rather than estimated. A caller that meters spend records
// what the provider said; inventing a number from a character count would put
// a fabricated figure in the same column as a billed one, and the two are
// then indistinguishable forever.
func ReplyUsage(msg *ai.AssistantMessage) ai.Usage {
	if msg == nil {
		return ai.Usage{}
	}
	return msg.ObserveUsage()
}

// UsageOf folds a settled reply into the stored-ledger shape.
//
// It is the ai-side twin of pigagent.UsageOf, and the two cannot be one
// function: a loop reply carries a streaming view that only
// agent.AssistantMessage knows how to observe, while a one-shot completion
// is a plain ai.AssistantMessage. Sharing the *mapping* rather than the
// function is what matters — the same six fields, the same choice of
// provider-reported total over the sum, the same refusal to estimate.
//
// The nil-message case is the same in both: a caller that got no reply bills
// nothing, and a zero row says exactly that.
func UsageOf(msg *ai.AssistantMessage) ports.TranscriptUsage {
	if msg == nil {
		return ports.TranscriptUsage{}
	}
	observed := msg.ObserveUsage()
	return ports.TranscriptUsage{
		InputTokens:      observed.Input,
		OutputTokens:     observed.Output,
		CacheReadTokens:  observed.CacheRead,
		CacheWriteTokens: observed.CacheWrite,
		ReportedTotal:    observed.TotalTokens,
		CostUSD:          observed.Cost.Total,
	}
}

// SystemTurn builds the system message a Request's transcript opens with.
//
// It is a constructor rather than a literal because the literal is
// three fields wide and the third — Timestamp — is not something a caller
// should be inventing. PiG stamps timestamps when it persists a message; a
// caller setting one by hand is copying a time it did not observe.
func SystemTurn(text string) ai.Message {
	return ai.SystemMessage{Content: ai.SystemText(text)}
}

// UserTurn builds one user message.
func UserTurn(text string) ai.Message {
	return ai.UserMessage{Content: ai.UserText(text)}
}

// AssistantTurn builds an assistant message carrying text and, optionally,
// the tool calls it requested.
//
// Keeping the tool calls on the same message is not a convenience. A
// transcript that replays an assistant's text without its tool calls and then
// carries the tool results anyway is an orphan: strict providers reject the
// whole turn, and lenient ones attribute the result to the wrong call.
func AssistantTurn(text string, calls ...ai.ToolCall) ai.AssistantMessage {
	msg := ai.AssistantMessage{}
	if text != "" {
		msg.Content = append(msg.Content, ai.TextContent{Text: text})
	}
	for _, call := range calls {
		msg.Content = append(msg.Content, call)
	}
	return msg
}

// ToolTurn builds the tool-result message that answers one tool call.
//
// callID is required and is not defaulted: a result whose call id does not
// match a call in the transcript is rejected by every provider, and the
// rejection names no message index. An empty id here would surface as a 400
// on a later turn, far from the line that produced it.
func ToolTurn(callID, name, text string) ai.Message {
	return ai.ToolResultMessage{
		ToolCallID: callID,
		ToolName:   name,
		Content:    []ai.ToolResultMessageContent{ai.TextContent{Text: text}},
	}
}

// ArgumentsJSON renders a tool call's arguments as the bytes a host stores or
// a log line shows.
//
// An absent argument set is a parameterless call, which is the empty object
// rather than the four bytes "null": a persisted null replays as a request
// the provider refuses, and a null in an audit row reads as "the model asked
// for something with no arguments" when it meant "the model asked for
// nothing at all".
func ArgumentsJSON(args ai.JsonObject) json.RawMessage {
	if len(args) == 0 {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	return json.RawMessage(raw)
}

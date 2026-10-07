package llmgw

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// The OpenAI chat-completions wire format, and the translation to and from
// PiG's own vocabulary.
//
// This layer exists for one reason and it is not convenience: the node's
// agent speaks the OpenAI protocol because PiG's OpenAI provider speaks it,
// and the manager holds provider credentials that must not leave. So the
// manager has to be the other end of a protocol it does not otherwise use.
//
// Which makes the risk here unusually concrete. Every field that is renamed
// or re-shaped is a place where a tool call can lose its id, and the symptom
// of a lost id is not an error — it is a model that quietly stops calling
// tools and starts answering in prose. That is the same failure
// pigmodel's own doc comment names when it describes the layer this one
// replaced, and it is why the translation is written out rather than
// reflected: a reflected struct cannot drop a field, and a hand-written one
// can, so the hand-written one is the one that gets read twice.

// chatRequest is the request half of POST /v1/chat/completions.
//
// Only the fields a node's agent actually sends are modelled. A field that is
// accepted and ignored is worse than one that is refused: the caller believes
// it asked for something, and the model does something else.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream,omitempty"`
	// MaxCompletionTokens is the caller's own output ceiling. It is honoured
	// rather than parsed-and-dropped: an agent that asked for 4k tokens
	// because its findings are read aloud in a call bridge is entitled to
	// that bound, and a gateway that silently answered without one both
	// overran the request and made the cost of an answer unforecastable.
	//
	// Zero means unset, which is not the same as "no limit": the registry
	// applies whatever the operator's own model configuration says. A
	// negative value is refused in tune, because it is a caller that
	// misread the field rather than a caller asking for a small answer.
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

// tune turns the request's output ceiling into a pigmodel adjustment.
//
// It is a Tune rather than a field on pigmodel.Request because max tokens is
// a per-request knob the registry fills in from the model configuration, and
// the caller's value has to be applied after that resolution — a request field
// would either be ignored or would have to duplicate the registry's own
// precedence rules.
func (r *chatRequest) tune() func(*pigai.StreamOptions) {
	if r.MaxCompletionTokens == 0 {
		return nil
	}
	limit := r.MaxCompletionTokens
	return func(opts *pigai.StreamOptions) { opts.MaxTokens = limit }
}

// chatMessage is one entry of the request's transcript.
//
// Content is a union: a bare string, or the parts array. Both shapes are
// accepted because a real agent sends the second one — a PiG user turn
// carries its text as a one-element parts array, and a gateway that modelled
// only the string refused every request a real node makes. The parts that are
// accepted are exactly the ones that survive the translation, and a part that
// would not is refused by name rather than dropped: see contentText.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    contentText    `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// chatToolCall is a tool call the model made, on the way back in.
type chatToolCall struct {
	// Index is the streaming delta's position, and it is a pointer so that
	// "no index" (the non-streaming message shape, where OpenAI omits the
	// field entirely) stays distinguishable from "index zero".
	//
	// That distinction is not cosmetic. PiG's own client reads streamed tool
	// calls from `delta.tool_calls` and keys the fragments by this field
	// (ai/openai.go: openai.go's stream loop indexes `partialsByWireIndex`),
	// so an `omitempty` integer would drop index 0 -- the first and most
	// common tool call -- and leave the client unable to place it.
	Index    *int           `json:"index,omitempty"`
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function chatToolCallFn `json:"function"`
}

// chatToolCallFn is the function half of a tool call.
type chatToolCallFn struct {
	Name string `json:"name"`
	// Arguments is a JSON *string* on the OpenAI wire, and a decoded object
	// in PiG. The string form is not a formatting quirk to be cleaned up:
	// providers stream it in fragments, and re-encoding an object that was
	// assembled piecewise produces the same bytes but breaks any signature
	// or id a caller tied to the exact text.
	Arguments string `json:"arguments"`
}

// chatTool is a tool the caller offers the model.
type chatTool struct {
	Type     string     `json:"type"`
	Function chatToolFn `json:"function"`
}

// chatToolFn describes one offered function.
type chatToolFn struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// toRequest turns a wire request into the shape pigmodel speaks.
//
// The tool declarations and the transcript are the two halves that have to
// agree with each other, and the failure when they do not is the one this
// whole file is written around: a transcript carrying tool results whose
// calls are missing is rejected by strict providers and misattributed by
// lenient ones. So a tool result whose call id was never declared is refused
// here, at the edge, where the message can name the index — rather than
// passed upstream to become a 400 that names nothing.
func (r *chatRequest) toRequest() (toolNames map[string]string, err error) {
	if r.MaxCompletionTokens < 0 {
		return nil, fmt.Errorf("%w: max_completion_tokens is %d; it is a ceiling, "+
			"and a negative one asks for no output at all", errs.ErrInvalid, r.MaxCompletionTokens)
	}
	toolNames = make(map[string]string, len(r.Tools))
	for i, tool := range r.Tools {
		if tool.Type != "" && tool.Type != "function" {
			return nil, fmt.Errorf("%w: tools[%d].type is %q; only \"function\" is served", errs.ErrInvalid, i, tool.Type)
		}
		if tool.Function.Name == "" {
			return nil, fmt.Errorf("%w: tools[%d] has no name", errs.ErrInvalid, i)
		}
		if _, dup := toolNames[tool.Function.Name]; dup {
			return nil, fmt.Errorf("%w: tools[%d] declares %q twice", errs.ErrInvalid, i, tool.Function.Name)
		}
		toolNames[tool.Function.Name] = tool.Function.Name
	}

	declared := make(map[string]string, len(r.Messages))
	for i, msg := range r.Messages {
		for _, call := range msg.ToolCalls {
			if call.ID == "" {
				return nil, fmt.Errorf("%w: messages[%d].tool_calls has an entry with no id; "+
					"the result that answers it could not be matched to it", errs.ErrInvalid, i)
			}
			declared[call.ID] = call.Function.Name
		}
	}

	for i, msg := range r.Messages {
		if msg.Role != roleTool {
			continue
		}
		if msg.ToolCallID == "" {
			return nil, fmt.Errorf("%w: messages[%d] is a tool result with no tool_call_id", errs.ErrInvalid, i)
		}
		if _, ok := declared[msg.ToolCallID]; !ok {
			return nil, fmt.Errorf("%w: messages[%d] answers tool call %q, which no earlier message made",
				errs.ErrInvalid, i, msg.ToolCallID)
		}
	}
	return toolNames, nil
}

// The three roles this gateway serves. They are named rather than used as
// bare strings so a typo is a compile error instead of a 400 from a provider
// that names no message index.
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// messages turns the wire transcript into PiG's.
//
// Every message becomes exactly one pigai.Message. An assistant message that
// carries both text and tool calls keeps both on the same message, because a
// transcript that replays the text without the calls and then carries the
// results anyway is an orphan — see pigmodel.AssistantTurn, which this calls
// rather than reimplementing.
func (r *chatRequest) messages() ([]pigai.Message, error) {
	out := make([]pigai.Message, 0, len(r.Messages))
	for i, msg := range r.Messages {
		switch msg.Role {
		case roleSystem:
			out = append(out, systemTurn(msg.Content.String()))
		case roleUser:
			out = append(out, userTurn(msg.Content.String()))
		case roleAssistant:
			calls := make([]pigai.ToolCall, 0, len(msg.ToolCalls))
			for _, call := range msg.ToolCalls {
				if call.Function.Name == "" {
					return nil, fmt.Errorf("%w: messages[%d].tool_calls has an entry with no function name", errs.ErrInvalid, i)
				}
				arguments, err := decodeArguments(call.Function.Arguments)
				if err != nil {
					return nil, fmt.Errorf("%w: messages[%d].tool_calls[%s].function.arguments: %v", errs.ErrInvalid, i, call.ID, err)
				}
				calls = append(calls, pigai.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
			}
			out = append(out, assistantTurn(msg.Content.String(), calls...))
		case roleTool:
			out = append(out, toolTurn(msg.ToolCallID, msg.Name, msg.Content.String()))
		default:
			return nil, fmt.Errorf("%w: messages[%d].role is %q", errs.ErrInvalid, i, msg.Role)
		}
	}
	return out, nil
}

// decodeArguments parses the JSON string the OpenAI wire carries.
//
// An empty string is a call with no arguments, which is a real thing models
// emit for a no-argument tool, and it decodes to an empty object rather than
// to nil — a provider that validates "arguments is required and must be an
// object" is right to refuse nil.
func decodeArguments(raw string) (pigai.JsonObject, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return pigai.JsonObject{}, nil
	}
	var out pigai.JsonObject
	// UseNumber, not the plain unmarshal.
	//
	// A plain unmarshal decodes every JSON number into a float64, which is
	// lossy above 2^53 — and the arguments a node's tools take are full of
	// values past that. A nanosecond timestamp, a byte count on a large
	// volume, a nanosecond duration in a log query: each of them is an
	// integer a tool compares against something, and each of them comes back
	// rounded. The tool then acts on a number nobody passed, and the failure
	// is a wrong answer rather than an error.
	//
	// json.Number keeps the exact text, so the value re-encodes byte for byte
	// and a strict provider sees the argument it was sent.
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if out == nil {
		return pigai.JsonObject{}, nil
	}
	return out, nil
}

// toolSchemas turns the wire tool declarations into PiG's.
//
// A declaration with no parameter schema gets an empty object rather than nil,
// because "this tool takes no arguments" is a fact the model needs to be told
// and nil says nothing at all.
func (r *chatRequest) toolSchemas() []pigai.ToolSchema {
	if len(r.Tools) == 0 {
		return nil
	}
	out := make([]pigai.ToolSchema, 0, len(r.Tools))
	for _, tool := range r.Tools {
		parameters := tool.Function.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, pigai.ToolSchema{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  parameters,
		})
	}
	return out
}

// The three constructors below exist so the translation in this file never
// builds a PiG message by hand. pigmodel already owns how a turn is shaped —
// in particular that an assistant's text and its tool calls travel on the same
// message — and a second implementation of that rule in this package would be
// free to drift from it. The symptom of the drift is a transcript that is
// valid to PiG and wrong to the provider, which is a 400 on the node and a
// stack trace in the manager.
func systemTurn(text string) pigai.Message { return pigmodel.SystemTurn(text) }
func userTurn(text string) pigai.Message   { return pigmodel.UserTurn(text) }
func assistantTurn(text string, calls ...pigai.ToolCall) pigai.Message {
	return pigmodel.AssistantTurn(text, calls...)
}
func toolTurn(callID, name, text string) pigai.Message { return pigmodel.ToolTurn(callID, name, text) }

// chatResponse is the non-streaming reply.
type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

// chatChunk is one streaming frame. It is the same shape as chatResponse with
// the message replaced by a delta, which is what the OpenAI wire does: a
// client reads the two interchangeably and a frame that is not this shape is
// dropped by the client rather than reported.
type chatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

// chatChoice is one candidate completion.
type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason"`
}

// chatUsage is the token accounting, in the names the OpenAI wire uses.
//
// The mapping is a rename and nothing else: this gateway does not estimate,
// does not fill in a total the provider did not report, and does not round.
// A metered number that was invented is indistinguishable from a billed one
// for ever after, which is the reason pigmodel.UsageOf refuses to estimate —
// and a rename is a thing that can be checked, an estimate cannot.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// reply renders a settled PiG reply as the OpenAI wire.
//
// finish_reason is derived from what the reply contains rather than passed
// through, because PiG does not carry the field: a reply with tool calls is
// "tool_calls" to every OpenAI client, and a client that reads "stop" will
// stop the loop and never execute anything the model asked for.
func reply(id, model string, created int64, msg *pigai.AssistantMessage) chatResponse {
	response := chatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      assistantWire(msg),
			FinishReason: finishReason(msg),
		}},
	}
	if usage, ok := usageOf(msg); ok {
		response.Usage = &usage
	}
	return response
}

// assistantWire renders a reply as the wire's assistant message.
func assistantWire(msg *pigai.AssistantMessage) *chatMessage {
	if msg == nil {
		return &chatMessage{Role: roleAssistant}
	}
	out := &chatMessage{Role: roleAssistant, Content: contentText(pigmodel.ReplyText(msg))}
	for _, call := range pigmodel.ReplyToolCalls(msg) {
		arguments, err := json.Marshal(call.Arguments)
		if err != nil {
			// Unreachable for a decoded object, and a failure here would
			// otherwise be dropped silently — which is how a tool call ends
			// up on the wire with empty arguments and a model that retries
			// it for ever. An empty object is the honest degradation.
			arguments = []byte("{}")
		}
		out.ToolCalls = append(out.ToolCalls, chatToolCall{
			ID:       call.ID,
			Type:     "function",
			Function: chatToolCallFn{Name: call.Name, Arguments: string(arguments)},
		})
	}
	return out
}

// finishReason is what an OpenAI client branches on.
func finishReason(msg *pigai.AssistantMessage) string {
	if msg != nil && len(pigmodel.ReplyToolCalls(msg)) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// usageOf maps a reply's accounting, reporting false when there is none.
//
// The distinction is preserved rather than smoothed over: a client that sees
// usage:null knows its provider reported nothing, and a client that sees
// zeros knows a number was asserted. Those are different facts and only one
// of them is safe to bill against.
func usageOf(msg *pigai.AssistantMessage) (chatUsage, bool) {
	if msg == nil {
		return chatUsage{}, false
	}
	observed := pigmodel.ReplyUsage(msg)
	if observed.TotalTokens == 0 && observed.Input == 0 && observed.Output == 0 {
		return chatUsage{}, false
	}
	usage := chatUsage{
		PromptTokens:     observed.Input,
		CompletionTokens: observed.Output,
		TotalTokens:      observed.TotalTokens,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage, true
}

// A streaming delta carries content only, and the content frame above is the
// whole of it.
//
// Tool calls are deliberately not streamed. A node's agent accumulates them
// from the final frame, which is where the ids are, and a delta carrying a
// half-assembled call is a shape every client has to special-case and none of
// them do identically — so the call arrives whole or not at all.

// finalChunk carries the complete reply and the finish reason.
//
// It repeats the text rather than only the tool calls, because a client that
// reassembles a turn from deltas discards a final frame whose content differs
// from what it already has would be non-conformant, and because a client that
// reconnects mid-stream gets everything from this one frame.
func finalChunk(id, model string, created int64, msg *pigai.AssistantMessage) chatChunk {
	chunk := chatChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      assistantWire(msg),
			FinishReason: finishReason(msg),
		}},
	}
	if usage, ok := usageOf(msg); ok {
		chunk.Usage = &usage
	}
	return chunk
}

// contentText is the wire's `content` field, which is not one type.
//
// Measured against a real `pig` on v0.3.0: a system turn sends a bare JSON
// string, and a user turn sends the content-parts array —
// [{"type":"text","text":"..."}]. Both are the same field, both appear in
// the same request, and which shape a turn gets is the client's choice rather
// than a property of the role. Modelling it as a plain string made this
// gateway reject every request a real agent sent, and no unit test in this
// package caught that, because every one of them was written by the same
// hand that wrote the string. The end-to-end test in tests/agentgateway is
// what found it, which is the argument for having one.
//
// A non-text part is refused rather than skipped. Dropping an image is how a
// transcript ends up describing something the model never saw, and the model
// then reasons confidently about a picture it was never sent.
type contentText string

// UnmarshalJSON accepts both shapes of the field.
func (c *contentText) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	switch {
	case trimmed == "" || trimmed == "null":
		*c = ""
		return nil
	case strings.HasPrefix(trimmed, `"`):
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*c = contentText(text)
		return nil
	case strings.HasPrefix(trimmed, "["):
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(data, &parts); err != nil {
			return err
		}
		var text strings.Builder
		for i, part := range parts {
			switch part.Type {
			case "text", "refusal", "":
				// A refusal is text the model declined to place in
				// `content`. On a node's diagnostics turn there is nothing
				// else for it to be, and dropping it would leave the agent
				// with an empty reply and no explanation.
				text.WriteString(part.Text)
			default:
				return fmt.Errorf("content[%d].type is %q; this gateway serves text turns only, "+
					"and silently dropping a part is how a transcript ends up describing something "+
					"the model never saw", i, part.Type)
			}
		}
		*c = contentText(text.String())
		return nil
	default:
		return fmt.Errorf("content is %s; expected a string or an array of content parts", trimmed)
	}
}

// String reads the field back.
func (c contentText) String() string { return string(c) }

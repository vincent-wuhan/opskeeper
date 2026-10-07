// Package pigai is how every OpsKeeper module outside core/pig names PiG's
// message and stream vocabulary.
//
// # Why this package exists
//
// core/pig is the only module permitted to import github.com/MichaelKinsy/PiG.
// That is not a style preference: PiG is pre-stable 0.x, and the boundary is
// what makes an upstream API move a one-module change instead of a
// monorepo-wide one. `make module-check` fails the build when anything else
// names a PiG import path.
//
// The alternative to this package is the one this package replaced: every
// host file that needed a message type reached for github.com/MichaelKinsy/PiG/ai
// directly and the gate failed. The tempting fix — widening the gate to
// permit PiG/ai outside core/pig — trades a real invariant for a quieter
// build, and the bill arrives the next time PiG renames a content block.
//
// # What an alias is and is not
//
// Every declaration below is a type alias (`=`) or a re-exported constant.
// There is no wrapper struct, no conversion, and no function that reshapes a
// value on the way through. A caller that builds a pigai.Message and hands it
// to PiG is handing PiG an ai.Message — the identical type, not a
// translation of it. That distinction is the whole reason this is acceptable
// inside a rule that otherwise exists to keep a second vocabulary from taking
// root: an alias adds a name, not a shape.
//
// A wrapper would be a different thing entirely, and it is the thing this
// refactor deleted. The retired ports.LLMRequest / ports.LLMResponse pair was
// a wrapper: every field declared twice, every call site a conversion, and
// every conversion a place a tool_call id could be dropped silently.
//
// # Adding to this list
//
// The list is deliberately short. A symbol belongs here when a host outside
// core/pig has to name it, not when it might be convenient — every entry is a
// claim that PiG's definition of the concept is OpsKeeper's definition too.
// A host that wants something not on this list is usually about to invent a
// shape PiG already has; the answer is to add the name here, not to define a
// parallel type at the call site.
package pigai

import "github.com/MichaelKinsy/PiG/ai"

// The message union. These four are the only shapes a transcript is made of,
// and every one of them is a distinct Go type rather than a struct with a
// role field — which is what makes an empty assistant turn and a tool-result
// turn statically impossible to confuse.
type (
	// Message is one entry in a transcript: a SystemMessage, UserMessage,
	// AssistantMessage or ToolResultMessage.
	Message = ai.Message
	// SystemMessage carries the persona and the tool declarations the
	// session starts with.
	SystemMessage = ai.SystemMessage
	// UserMessage carries what the operator typed.
	UserMessage = ai.UserMessage
	// AssistantMessage is a settled model reply: its text, its tool calls,
	// its thinking blocks, and the usage the provider reported.
	AssistantMessage = ai.AssistantMessage
	// ToolResultMessage answers exactly one ToolCall.
	ToolResultMessage = ai.ToolResultMessage
)

// The content blocks that appear inside the messages above.
type (
	// TextContent is a text span. On an assistant message several of them
	// are streaming deltas of one utterance, which is why they concatenate
	// with no separator — see pigmodel.ReplyText.
	TextContent = ai.TextContent
	// ToolCall is a model's request to run a tool.
	ToolCall = ai.ToolCall
	// JsonObject is a tool call's argument set. It is an object and not a
	// raw string so a parameterless call can be spelled `{}` rather than
	// the `null` a naive encoding produces.
	JsonObject = ai.JsonObject
)

// The request-side values a host adjusts on a single call.
type (
	// StreamOptions is the per-request knob set: temperature, thinking
	// level, max tokens, the prompt-cache session id, and the credential
	// the registry resolved. Reached through pigmodel.Request.Tune, which is
	// the supported seam — a caller should not be building one of these.
	StreamOptions = ai.StreamOptions
	// API names a provider wire format. OpsKeeper's settings table is
	// OpenAI-compatible for every provider except one; llmpig owns that
	// mapping.
	API = ai.API
)

// The provider-side values a host names when it resolves a model.
//
// Model is what pigmodel.Registry.Model hands back, and a host that wants to
// serve a model catalogue — the OpenAI-compatible gateway the node's agent
// talks to — has to be able to spell the return type without naming PiG. It is
// the same type, not a projection: the fields a host reads are the fields the
// provider was constructed with.
type (
	// Model is a resolved provider model: its identity, its limits and its
	// wire format.
	Model = ai.Model
	// ToolSchema is one tool declaration in a request. The gateway passes
	// these through from the agent's request to the provider unchanged, so a
	// parameter the agent declared is a parameter the model was shown.
	ToolSchema = ai.ToolSchema
)

// Usage is the provider's own accounting. It is not the stored ledger row:
// see ports.TranscriptUsage for why the two are separate types, and
// pigmodel.UsageOf for the fold between them.
type Usage = ai.Usage

// The wire formats OpsKeeper's provider table maps onto.
const (
	// APIOpenAICompletions is what every OpsKeeper provider speaks except
	// Anthropic.
	APIOpenAICompletions = ai.APIOpenAICompletions
	// APIAnthropicMessages is the one provider with its own protocol.
	APIAnthropicMessages = ai.APIAnthropicMessages
)

// Why a message stopped. A host that records this is recording the model's
// own reason, not the loop's — a provider can stop a message for a dozen
// reasons and only two of them are interesting to a turn.
const (
	// StopReasonStop is a finished message.
	StopReasonStop = ai.StopReasonStop
	// StopReasonToolUse is a message that only asked for tools.
	StopReasonToolUse = ai.StopReasonToolUse
)

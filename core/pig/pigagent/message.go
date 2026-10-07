// message.go converts a settled PiG message into the kernel-neutral
// transcript entry the host persists.
//
// It is a separate file rather than a few lines inside runstate.go because
// the conversion is the whole risk of the persistence path. A field dropped
// here does not fail loudly: it produces a transcript that replays as an
// orphaned tool result, which a strict provider rejects with an HTTP 400 on
// a LATER turn — the failure appears far from its cause and in a different
// request. So the mapping has its own tests, one per role.
package pigagent

import (
	"encoding/json"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// usagePtr returns a pointer to a copy, or nil when there is nothing to
// record. The indirection is what the stored row needs — a nil Usage is how
// the host tells "this assistant turn reported no accounting" apart from
// "this assistant turn reported zero tokens" — and returning a pointer to a
// shared zero value would collapse the two.
func usagePtr(u ports.TranscriptUsage) *ports.TranscriptUsage {
	if u == (ports.TranscriptUsage{}) {
		return nil
	}
	return &u
}

// toPortsMessage translates one settled PiG message. model is the resolved
// model id for the turn, stamped only onto assistant rows so the console can
// attribute an answer to the model that produced it.
//
// An unusable message reports an empty role, which the host skips. Skipping
// is deliberate: the entry came from the provider, and refusing to persist it
// would fail the turn over a message the operator cannot repair.
func toPortsMessage(msg agent.AgentMessage, model string) ports.AgentMessage {
	switch {
	case msg.User != nil:
		return ports.AgentMessage{Role: "user", Content: userContentText(msg.User.Content)}
	case msg.System != nil:
		return ports.AgentMessage{Role: "system", Content: systemContentText(msg.System.Content)}
	case msg.Assistant != nil:
		out := ports.AgentMessage{Role: "assistant", Model: model}
		var text string
		for _, block := range msg.Assistant.Content {
			switch b := block.(type) {
			case ai.TextContent:
				text += b.Text
			case ai.ToolCall:
				out.ToolCalls = append(out.ToolCalls, ports.AgentToolCall{
					ID:        b.ID,
					Name:      b.Name,
					Arguments: marshalArguments(b.Arguments),
				})
			}
		}
		out.Content = stripInlineThinking(text)
		out.Usage = usagePtr(UsageOf(msg.Assistant))
		if out.Content == "" && len(out.ToolCalls) == 0 {
			// Neither text nor a call: nothing to persist, and most
			// providers reject an empty assistant turn on replay.
			return ports.AgentMessage{}
		}
		return out
	case msg.ToolResult != nil:
		return ports.AgentMessage{
			Role:       "tool",
			Content:    ai.ContentText(msg.ToolResult.Content),
			ToolCallID: msg.ToolResult.ToolCallID,
			ToolName:   msg.ToolResult.ToolName,
		}
	}
	return ports.AgentMessage{}
}

// userContentText renders a user message body.
//
// Same interface-over-two-shapes problem as the system content below: the
// generic helper covers the concrete types but not the interface.
func userContentText(content ai.UserContent) string {
	switch c := content.(type) {
	case ai.UserText:
		return ai.ContentText(c)
	case ai.UserContentBlocks:
		return ai.ContentText(c)
	}
	return ""
}

// systemContentText renders a system message body.
//
// The system content is an interface over either a plain text value or a
// block list, and a type switch is the only way to read both: the generic
// ContentText helper covers the concrete types but not the interface, so
// passing the interface straight in does not compile. An unrecognised shape
// yields "", and the host drops an empty entry rather than writing a row
// with no content.
func systemContentText(content ai.SystemContent) string {
	switch c := content.(type) {
	case ai.SystemText:
		return ai.ContentText(c)
	case ai.SystemTextBlocks:
		return ai.ContentText(c)
	}
	return ""
}

// marshalArguments renders tool call arguments as the raw JSON the host
// stores. An absent object becomes an empty object, which is what a
// parameterless call means; a value that cannot be rendered becomes nil, and
// the host treats a nil argument set as unrepresentable rather than writing a
// malformed row.
func marshalArguments(obj ai.JsonObject) []byte {
	if len(obj) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return b
}

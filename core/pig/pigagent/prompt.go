// prompt.go turns a kernel-agnostic AgentRequest into the message list one
// PiG turn starts from.
//
// It is separate from kernel.go because the transcript is what a kernel swap
// is judged on: the same request must produce the same conversation under any
// kernel, and the only way to check that is for the translation to be a pure
// function with its own tests.
package pigagent

import (
	"encoding/json"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// buildPrompt assembles the messages for one turn: persisted history first,
// then the persona reminder, then the new user text.
//
// Order matters and is not arbitrary. History is oldest-first so a provider's
// prompt cache sees a stable prefix; the reminder sits immediately before the
// user turn so it is the most recent thing the model read, and it is a
// separate message rather than a system-prompt suffix because the system
// prompt is the cached prefix — a per-turn reminder placed there would be
// cached with it and fade out of attention, which is the opposite of what an
// anti-drift reminder is for.
func buildPrompt(req ports.AgentRequest) []agent.AgentMessage {
	out := make([]agent.AgentMessage, 0, len(req.History)+2)

	for _, m := range req.History {
		if msg, ok := toAgentMessage(m); ok {
			out = append(out, msg)
		}
	}

	if r := trim(req.CriticalReminder); r != "" {
		out = append(out, agent.AgentMessage{User: &agent.UserMessage{
			Role:      "user",
			Content:   agentUserContent(r),
			Timestamp: nowMillis(),
		}})
	}
	if t := trim(req.UserText); t != "" {
		out = append(out, agent.AgentMessage{User: &agent.UserMessage{
			Role:      "user",
			Content:   agentUserContent(t),
			Timestamp: nowMillis(),
		}})
	}
	if len(out) == 0 {
		// A turn with no prompt cannot run, and sending an empty user turn
		// would make the provider charge for a call with nothing to answer.
		out = append(out, agent.AgentMessage{User: &agent.UserMessage{
			Role:      "user",
			Content:   agentUserContent("(continue)"),
			Timestamp: nowMillis(),
		}})
	}
	return out
}

// toAgentMessage converts one prior turn. The bool reports whether the entry
// is usable; an unusable entry is skipped rather than failing the turn.
//
// Skipping is the deliberate choice here. History comes from persisted rows
// the operator cannot edit, so an entry this build cannot express (a role
// added by a newer version, a tool call whose id was lost) would otherwise
// turn every later turn in the session into a hard error, and the session
// would be permanently unusable with no way back. Dropping the entry costs
// the model some context; refusing costs it the conversation.
func toAgentMessage(m ports.AgentMessage) (agent.AgentMessage, bool) {
	switch m.Role {
	case "user":
		return agent.AgentMessage{User: &agent.UserMessage{
			Role:      "user",
			Content:   agentUserContent(m.Content),
			Timestamp: nowMillis(),
		}}, true

	case "assistant":
		msg := &agent.AssistantMessage{Role: "assistant", Timestamp: nowMillis()}
		if m.Content != "" {
			msg.Content = append(msg.Content, ai.TextContent{Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			args, ok := decodeArguments(tc.Arguments)
			if !ok {
				// An assistant turn whose arguments cannot be replayed is
				// not representable: the provider validates the JSON object
				// and would reject the whole transcript. Dropping the turn
				// loses one round trip; keeping it loses the conversation.
				return agent.AgentMessage{}, false
			}
			msg.Content = append(msg.Content, ai.ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: args,
			})
		}
		if len(msg.Content) == 0 {
			// An assistant entry with neither text nor tool calls carries
			// nothing. Most providers reject an empty assistant turn, so it
			// is dropped rather than sent.
			return agent.AgentMessage{}, false
		}
		return agent.AgentMessage{Assistant: msg}, true

	case "tool":
		if m.ToolCallID == "" {
			// Without the id the result cannot be attached to its request,
			// and a provider that receives an orphaned tool result rejects
			// the transcript outright.
			return agent.AgentMessage{}, false
		}
		return agent.AgentMessage{ToolResult: &agent.ToolResultMessage{
			Role:       "toolResult",
			ToolCallID: m.ToolCallID,
			ToolName:   m.ToolName,
			Content:    []ai.ToolResultMessageContent{ai.TextContent{Text: m.Content}},
			Timestamp:  nowMillis(),
		}}, true
	}
	return agent.AgentMessage{}, false
}

// decodeArguments turns the stored argument bytes into the JSON object PiG's
// tool call carries. Absent or empty becomes an empty object, which is what a
// parameterless call means; malformed reports false so the caller drops the
// turn instead of sending arguments the provider will reject.
func decodeArguments(raw []byte) (ai.JsonObject, bool) {
	if len(raw) == 0 {
		return ai.JsonObject{}, true
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	if out == nil {
		return ai.JsonObject{}, true
	}
	return ai.JsonObject(out), true
}

package agent

import (
	"encoding/json"
	"strconv"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
)

// This file is the small set of helpers the legacy loop needs to speak
// PiG's vocabulary directly. Each one exists because re-deriving it at the
// call site is how the same value ends up encoded two different ways in one
// turn.

// assistantTurnOf splits a settled reply into the turn to append to the
// transcript and the calls to execute.
//
// Both halves come from one walk of the content blocks, so they cannot
// disagree: a tool call the loop executes is always a tool call the next
// round can see, which is the pairing a strict provider rejects the whole
// request over when it is broken.
func assistantTurnOf(reply *pigai.AssistantMessage) (pigai.AssistantMessage, []pigai.ToolCall) {
	turn := pigai.AssistantMessage{}
	if reply == nil {
		return turn, nil
	}
	var calls []pigai.ToolCall
	for _, block := range reply.Content {
		switch b := block.(type) {
		case pigai.TextContent:
			turn.Content = append(turn.Content, pigai.TextContent{Text: b.Text})
		case pigai.ToolCall:
			calls = append(calls, b)
			turn.Content = append(turn.Content, b)
		}
		// Thinking blocks are deliberately not replayed. A reasoning
		// signature is model- and turn-specific, and replaying one the
		// provider did not issue is a rejection on the next round trip
		// with an error that names no message.
	}
	turn.Usage = reply.Usage
	turn.Model = reply.Model
	turn.Provider = reply.Provider
	turn.StopReason = reply.StopReason
	return turn, calls
}

// decodeReplayArgs turns stored argument bytes into PiG's JSON object.
//
// Malformed bytes become an empty object rather than an error. History is
// persisted data the operator cannot edit from the console, and a single
// unreplayable row must not make every later turn in the session fail; the
// tool will reject the call it cannot parse, and the model sees that and
// adapts. Aborting the whole turn instead costs the operator the
// conversation to protect one malformed argument.
func decodeReplayArgs(raw string) pigai.JsonObject {
	if raw == "" {
		return pigai.JsonObject{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return pigai.JsonObject{}
	}
	return pigai.JsonObject(out)
}

// cacheKeyFor builds the provider's prompt-cache key for a turn.
//
// The key must be stable for the whole tool loop and must not be a
// credential. It combines the caller with the session, which is exactly the
// granularity a provider cache wants — two turns of one conversation share
// a prefix, and two conversations by the same caller do not — while
// carrying nothing an operator could not already see in their own session
// list. It is never used as a metric label.
func cacheKeyFor(userID uint64, sessionID string) string {
	if sessionID == "" {
		return "u" + strconv.FormatUint(userID, 10)
	}
	return "u" + strconv.FormatUint(userID, 10) + "s" + sessionID
}

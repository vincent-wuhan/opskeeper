package pigcontract

import (
	"encoding/json"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file pins the parts of the PiG contract the type system cannot state.
//
// contract.go stops the build when a name moves. It cannot stop the build when
// a name keeps its type and changes its value, and those are the changes that
// reach production as a silently empty frame or as a policy hook that never
// fires. Three values matter to OpsKeeper that way, and each is asserted
// against the literal OpsKeeper's own code writes rather than against a copy
// of itself:
//
//  1. the thinking-level strings, because a value stored in a settings row is
//     compared against them by pigmodel;
//  2. the provider-stream discriminants, because `pig --mode rpc` carries an
//     assistant delta as a JSON envelope keyed by `type` and pigwire keeps
//     only the text_delta arm;
//  3. the tool-call hook name and the node wire event names, because the gate
//     extension and the node translator select on those strings.
//
// When one of these fails, the fix belongs in core/pig: teach pigmodel's
// parseThinkingLevel, or pigwire's translator, or the gate's hook name, about
// the new spelling, and then update the literal here. The point is that the
// failure is loud and local instead of being a frame nobody sees.

// TestThinkingLevelStringsAreStable pins the vocabulary the settings schema
// and the console share with the model registry. ThinkingNone is included
// separately because it is an alias, and an alias that stops tracking its
// target is a migration bug waiting for a saved settings row.
func TestThinkingLevelStringsAreStable(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  ai.ThinkingLevel
		want string
	}{
		{"off", ai.ThinkingOff, "off"},
		{"minimal", ai.ThinkingMinimal, "minimal"},
		{"low", ai.ThinkingLow, "low"},
		{"medium", ai.ThinkingMedium, "medium"},
		{"high", ai.ThinkingHigh, "high"},
		{"xhigh", ai.ThinkingXHigh, "xhigh"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("thinking level %s = %q, but pigmodel and the settings schema spell it %q", tc.name, tc.got, tc.want)
		}
	}

	if ai.ThinkingNone != ai.ThinkingOff {
		t.Errorf("ThinkingNone = %q, no longer an alias of ThinkingOff = %q; a saved \"none\" row would stop resolving", ai.ThinkingNone, ai.ThinkingOff)
	}
}

// TestProviderStreamDiscriminantsAreStable pins the `type` values on the
// assistant streaming envelope. They do not appear in a Go type that
// OpsKeeper names: `rpcAgentEvent` in cmd/pig turns a concrete event into
// `{"type": ai.EventTextDelta, ...}`, pigwire decodes the envelope, and a
// rename upstream turns every assistant token into a dropped frame instead of
// a compile error (see pigwire/translator.go, translateUpdate).
func TestProviderStreamDiscriminantsAreStable(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  ai.AssistantEventType
		want string
	}{
		{"start", ai.EventStart, "start"},
		{"text_start", ai.EventTextStart, "text_start"},
		{"text_delta", ai.EventTextDelta, "text_delta"},
		{"text_end", ai.EventTextEnd, "text_end"},
		{"thinking_delta", ai.EventThinkingDelta, "thinking_delta"},
		{"thinking_end", ai.EventThinkingEnd, "thinking_end"},
		{"toolcall_start", ai.EventToolCallStart, "toolcall_start"},
		{"toolcall_end", ai.EventToolCallEnd, "toolcall_end"},
		{"done", ai.EventDone, "done"},
		{"error", ai.EventError, "error"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("assistant stream discriminant %s = %q, but the RPC envelope and pigwire use %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestContentBlockJSONIsStable pins the content-block encoding the node
// translator decodes. pigwire reads `type`, `text` and `toolCall` out of a
// settled message, and the only thing that produces those keys is PiG's own
// MarshalJSON on the block types. A changed key would leave the console
// rendering an empty answer rather than failing.
func TestContentBlockJSONIsStable(t *testing.T) {
	text, err := json.Marshal(ai.TextContent{Text: "hello"})
	if err != nil {
		t.Fatalf("marshal text content: %v", err)
	}
	var textBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(text, &textBlock); err != nil {
		t.Fatalf("round-trip text content: %v", err)
	}
	if textBlock.Type != "text" || textBlock.Text != "hello" {
		t.Errorf("text content encodes as %s, but pigwire reads {\"type\":\"text\",\"text\":...}", text)
	}

	call, err := json.Marshal(ai.ToolCall{ID: "c-1", Name: "restart_service", Arguments: ai.JsonObject{"service": "api"}})
	if err != nil {
		t.Fatalf("marshal tool call: %v", err)
	}
	var callBlock struct {
		Type      string         `json:"type"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(call, &callBlock); err != nil {
		t.Fatalf("round-trip tool call: %v", err)
	}
	if callBlock.Type != "toolCall" || callBlock.ID != "c-1" || callBlock.Name != "restart_service" {
		t.Errorf("tool call encodes as %s, but pigwire counts pending calls by {\"type\":\"toolCall\"}", call)
	}
	if got := callBlock.Arguments["service"]; got != "api" {
		t.Errorf("tool call arguments = %#v, want the decoded JSON object map[string]any", got)
	}
}

// TestNodeWireEventNamesAreStable pins the seven event names pigwire's
// translator switches on. PiG exports the same names to extension authors, so
// the assertion is between two upstream constants and one literal rather than
// between a literal and itself: a rename fails here before it can become a
// node whose frames stopped flowing.
func TestNodeWireEventNamesAreStable(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"turn_start", sdk.EventTurnStart, "turn_start"},
		{"message_update", sdk.EventMessageUpdate, "message_update"},
		{"message_end", sdk.EventMessageEnd, "message_end"},
		{"tool_execution_start", sdk.EventToolExecutionStart, "tool_execution_start"},
		{"tool_execution_update", sdk.EventToolExecutionUpdate, "tool_execution_update"},
		{"tool_execution_end", sdk.EventToolExecutionEnd, "tool_execution_end"},
		{"agent_end", sdk.EventAgentEnd, "agent_end"},
	} {
		if tc.got != tc.want {
			t.Errorf("sdk event %s = %q, but pigwire's translator selects on %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestGateHookNameIsStable pins the one extension hook the gate registers on.
// The gate is a policy enforcement point; a hook name that stops matching
// upstream is a gate that never runs, which looks exactly like a gate that
// always allows.
func TestGateHookNameIsStable(t *testing.T) {
	if sdk.EventToolCall != "tool_call" {
		t.Errorf("sdk.EventToolCall = %q, but the gate registers its interceptor as %q", sdk.EventToolCall, "tool_call")
	}
}

// TestToolExecutionModesAreStable pins the parallelism mode every OpsKeeper
// tool adapter reports. A silent change to the spelling would make PiG treat
// the mode as unknown and fall back to its default rather than serializing the
// tools OpsKeeper asked it to serialize.
func TestToolExecutionModesAreStable(t *testing.T) {
	if string(agent.ToolModeParallel) != "parallel" {
		t.Errorf("agent.ToolModeParallel = %q, want %q", agent.ToolModeParallel, "parallel")
	}
	if string(agent.ToolModeSequential) != "sequential" {
		t.Errorf("agent.ToolModeSequential = %q, want %q", agent.ToolModeSequential, "sequential")
	}
	if agent.ToolModeParallel == agent.ToolModeSequential {
		t.Error("parallel and sequential tool execution modes are the same value")
	}
}

// TestReleaseVersionsAreReported pins that the two version strings a node
// reports are populated. An empty one is not a build failure but it is a node
// that cannot be matched against a min_pig_version during a rollout.
func TestReleaseVersionsAreReported(t *testing.T) {
	if coding.PigVersion == "" {
		t.Error("coding.PigVersion is empty; a node could not report which agent build it runs")
	}
	if coding.UpstreamVersion == "" {
		t.Error("coding.UpstreamVersion is empty; the Pi release a node tracks would be unreportable")
	}
}

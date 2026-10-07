package pigwire

import (
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

func fixedClock() func() time.Time {
	at := time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC)
	return func() time.Time { return at }
}

func newTranslator() *Translator {
	return New(Options{SessionID: "s-1", Now: fixedClock()})
}

// The payloads below are the shapes PiG's cmd/pig/rpc_events.go actually
// emits. They are written out rather than constructed from Go types on
// purpose: this package decodes a wire format it does not own, and a test
// that built its input from the same structs it decodes into would pass
// even if the wire changed underneath it.

const turnStartJSON = `{"type":"turn_start"}`

const textDeltaJSON = `{"type":"message_update","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"checking"}}`

const messageEndTextJSON = `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The node is healthy."}],"api":"openai-completions","provider":"openai","model":"gpt-5.6","usage":{"input":1200,"output":340,"cacheRead":800,"cacheWrite":0,"totalTokens":2340,"cost":{"input":0.01,"output":0.02,"cacheRead":0.001,"cacheWrite":0,"total":0.031}},"stopReason":"stop","timestamp":1759200000000}}`

const messageEndToolCallJSON = `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Let me look."},{"type":"toolCall","id":"tc-1","name":"get_topology","arguments":{"root":"prod"}}],"usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":15,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}`

const toolStartJSON = `{"type":"tool_execution_start","toolCallId":"tc-1","toolName":"get_topology","args":{"root":"prod"}}`

const toolUpdateJSON = `{"type":"tool_execution_update","toolCallId":"tc-1","toolName":"tail_file","args":{"path":"/var/log/syslog"},"partialResult":{"content":[{"type":"text","text":"line 1\n"}]}}`

const toolEndOKJSON = `{"type":"tool_execution_end","toolCallId":"tc-1","toolName":"get_topology","result":{"content":[{"type":"text","text":"three tiers"}]},"isError":false}`

const toolEndErrJSON = `{"type":"tool_execution_end","toolCallId":"tc-1","toolName":"get_topology","result":{"content":[{"type":"text","text":"connection refused"}]},"isError":true}`

const toolEndBlockedJSON = `{"type":"tool_execution_end","toolCallId":"tc-1","toolName":"restart_service","result":{"content":[{"type":"text","text":"opskeeper: tool blocked by host policy: needs approval"}]},"isError":true}`

const agentEndJSON = `{"type":"agent_end","messages":[],"willRetry":false}`

// --- frame mapping ------------------------------------------------------

func TestTurnStartOpensTheBubble(t *testing.T) {
	tr := newTranslator()
	frames := tr.Translate("turn_start", []byte(turnStartJSON))
	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.Type != wire.StreamAssistantStart {
		t.Errorf("type = %q", f.Type)
	}
	if f.Assistant == nil || f.Assistant.Content != "" {
		t.Errorf("assistant = %+v, want an empty bubble: the console opens it before any text", f.Assistant)
	}
	if f.Assistant.PendingToolCalls != 0 {
		t.Errorf("pending = %d, want 0", f.Assistant.PendingToolCalls)
	}
	// turn_start is also what advances the turn counter, so the console's
	// iteration column matches model round trips.
	if f.Iteration != 1 || f.Seq != 1 || f.SessionID != "s-1" {
		t.Errorf("envelope = %+v", f)
	}
}

func TestTextDeltaBecomesAnAssistantDelta(t *testing.T) {
	frames := newTranslator().Translate("message_update", []byte(textDeltaJSON))
	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	if frames[0].Type != wire.StreamAssistantDelta {
		t.Errorf("type = %q", frames[0].Type)
	}
	if frames[0].Assistant.Content != "checking" {
		t.Errorf("content = %q", frames[0].Assistant.Content)
	}
}

func TestThinkingDeltasAreDropped(t *testing.T) {
	// A shared incident view is not the place for a model's private
	// reasoning, and an operator did not ask for it.
	thinking := `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"the user wants me to..."}}`
	tr := newTranslator()
	if got := tr.Translate("message_update", []byte(thinking)); got != nil {
		t.Errorf("a thinking delta produced %d frames, want none", len(got))
	}
	if tr.seq != 0 {
		t.Errorf("seq advanced to %d while dropping a non-frameable event", tr.seq)
	}
}

func TestEmptyTextDeltaIsNotAFrame(t *testing.T) {
	// Emitting one would make the console append a no-op bubble.
	empty := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":""}}`
	if got := newTranslator().Translate("message_update", []byte(empty)); got != nil {
		t.Errorf("an empty delta produced %d frames, want none", len(got))
	}
}

func TestOtherUpdateKindsAreDropped(t *testing.T) {
	for _, kind := range []string{"start", "text_start", "text_end", "toolcall_start", "toolcall_delta", "toolcall_end", "done", "error"} {
		raw := `{"type":"message_update","assistantMessageEvent":{"type":"` + kind + `","contentIndex":0}}`
		if got := newTranslator().Translate("message_update", []byte(raw)); got != nil {
			t.Errorf("assistant event %q produced %d frames, want none", kind, len(got))
		}
	}
}

func TestMessageEndReportsTextAndPendingCalls(t *testing.T) {
	tr := newTranslator()
	tr.Translate("turn_start", []byte(turnStartJSON))
	frames := tr.Translate("message_end", []byte(messageEndToolCallJSON))

	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.Type != wire.StreamAssistantEnd {
		t.Errorf("type = %q", f.Type)
	}
	if f.Assistant.Content != "Let me look." {
		t.Errorf("content = %q", f.Assistant.Content)
	}
	if f.Assistant.PendingToolCalls != 1 {
		t.Errorf("pending = %d, want 1: the tool call has been asked for but not started", f.Assistant.PendingToolCalls)
	}
	if f.Assistant.CreatedAt == "" {
		t.Error("created_at is empty: the console timestamps the bubble from this")
	}
}

func TestMessageEndExcludesThinkingFromTheText(t *testing.T) {
	raw := `{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"internal"},{"type":"text","text":"visible"}]}}`
	frames := newTranslator().Translate("message_end", []byte(raw))
	if frames[0].Assistant.Content != "visible" {
		t.Errorf("content = %q, want the thinking block excluded", frames[0].Assistant.Content)
	}
}

func TestToolStartDrainsPending(t *testing.T) {
	tr := newTranslator()
	tr.Translate("message_end", []byte(messageEndToolCallJSON))
	if tr.pending != 1 {
		t.Fatalf("pending = %d, want 1", tr.pending)
	}
	frames := tr.Translate("tool_execution_start", []byte(toolStartJSON))
	if frames[0].Type != wire.StreamToolStart {
		t.Errorf("type = %q", frames[0].Type)
	}
	// The operator must be able to see what is about to run.
	if frames[0].Tool.ArgsJSON != `{"root":"prod"}` {
		t.Errorf("args_json = %q", frames[0].Tool.ArgsJSON)
	}
	if frames[0].Tool.Name != "get_topology" || frames[0].Tool.ToolCallID != "tc-1" {
		t.Errorf("tool = %+v", frames[0].Tool)
	}
	if tr.pending != 0 {
		t.Errorf("pending = %d, want 0", tr.pending)
	}
	if tr.toolCalls != 1 {
		t.Errorf("toolCalls = %d, want 1", tr.toolCalls)
	}
}

func TestToolUpdateCarriesPartialOutput(t *testing.T) {
	frames := newTranslator().Translate("tool_execution_update", []byte(toolUpdateJSON))
	if frames[0].Type != wire.StreamToolUpdate {
		t.Errorf("type = %q", frames[0].Type)
	}
	if frames[0].Tool.ResultJSON != "line 1\n" {
		t.Errorf("result_json = %q", frames[0].Tool.ResultJSON)
	}
}

func TestToolEndStatusesAreDistinct(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want wire.ToolStatus
	}{
		{"success", toolEndOKJSON, wire.ToolSuccess},
		{"error", toolEndErrJSON, wire.ToolError},
		// A refusal by a node-side host gate is not a broken tool. The
		// console renders the two differently, and collapsing them would
		// tell an operator their read-only tool is broken.
		{"blocked", toolEndBlockedJSON, wire.ToolBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := newTranslator().Translate("tool_execution_end", []byte(tc.raw))
			if frames[0].Type != wire.StreamToolEnd {
				t.Errorf("type = %q", frames[0].Type)
			}
			if frames[0].Tool.Status != tc.want {
				t.Errorf("status = %q, want %q", frames[0].Tool.Status, tc.want)
			}
		})
	}
}

func TestToolEndErrorTextIsTruncated(t *testing.T) {
	long := strings.Repeat("x", 900)
	raw := `{"type":"tool_execution_end","toolCallId":"tc-1","toolName":"t","result":{"content":[{"type":"text","text":"` + long + `"}]},"isError":true}`
	frames := newTranslator().Translate("tool_execution_end", []byte(raw))
	if got := len([]rune(frames[0].Tool.Error)); got != 513 {
		t.Errorf("error length = %d runes, want 513 (512 plus an ellipsis)", got)
	}
}

func TestAgentEndReportsUsageAndCounters(t *testing.T) {
	tr := newTranslator()
	tr.Translate("turn_start", []byte(turnStartJSON))
	tr.Translate("turn_start", []byte(turnStartJSON))
	tr.Translate("message_end", []byte(messageEndTextJSON))
	tr.Translate("tool_execution_start", []byte(toolStartJSON))

	frames := tr.Translate("agent_end", []byte(agentEndJSON))
	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.Type != wire.StreamDone {
		t.Errorf("type = %q", f.Type)
	}
	if f.Done.Iterations != 2 {
		t.Errorf("iterations = %d, want 2", f.Done.Iterations)
	}
	if f.Done.ToolCalls != 1 {
		t.Errorf("tool_calls = %d, want 1", f.Done.ToolCalls)
	}
	if f.Done.Usage == nil {
		t.Fatal("usage is missing: the console must not have to sum assistant frames for a cost")
	}
	if f.Done.Usage.InputTokens != 1200 || f.Done.Usage.OutputTokens != 340 || f.Done.Usage.CacheReadTokens != 800 {
		t.Errorf("usage = %+v", *f.Done.Usage)
	}
	if f.Done.Usage.CostUSD != 0.031 {
		t.Errorf("cost = %v, want 0.031", f.Done.Usage.CostUSD)
	}
	if f.Done.Usage.Model != "gpt-5.6" {
		t.Errorf("model = %q, want the model that actually incurred the cost", f.Done.Usage.Model)
	}
}

func TestUsageIsNotSummedAcrossMessages(t *testing.T) {
	// The agent reports cumulative totals per message. Summing them would
	// multiply every token by the number of round trips — the exact shape
	// of a cost report nobody trusts.
	tr := newTranslator()
	tr.Translate("message_end", []byte(messageEndTextJSON))
	tr.Translate("message_end", []byte(messageEndTextJSON))
	frames := tr.Translate("agent_end", []byte(agentEndJSON))
	if got := frames[0].Done.Usage.InputTokens; got != 1200 {
		t.Errorf("input tokens = %d, want 1200: the last report is the running total", got)
	}
}

// --- events with no counterpart ----------------------------------------

func TestLifecycleEventsProduceNoFrames(t *testing.T) {
	// Real events the console has never rendered. Inventing frames for
	// them would change the contract the console parses.
	for name, raw := range map[string]string{
		"message_start":   `{"type":"message_start","message":{"role":"assistant"}}`,
		"agent_settled":   `{"type":"agent_settled"}`,
		"turn_end":        `{"type":"turn_end","message":{},"toolResults":[]}`,
		"entry_appended":  `{"type":"entry_appended","entry":{}}`,
		"unknown_future":  `{"type":"something_new_in_a_later_version"}`,
		"bash_update":     `{"type":"bash_update","id":"b-1","content":"x"}`,
		"summarization_1": `{"type":"summarization_retry_scheduled","attempt":1,"maxAttempts":3,"delayMs":1000,"errorMessage":"x"}`,
	} {
		tr := newTranslator()
		if got := tr.Translate(name, []byte(raw)); got != nil {
			t.Errorf("%s produced %d frames, want none", name, len(got))
		}
		if tr.seq != 0 {
			t.Errorf("%s consumed sequence number %d, want 0", name, tr.seq)
		}
	}
}

func TestUnreadablePayloadsProduceNoFrames(t *testing.T) {
	// A frame the console never received leaves a gap nobody can explain,
	// so an unreadable event is counted rather than guessed at. The
	// translator does not emit a placeholder: a wrong frame is worse than
	// a missing one, because the console would render it as fact.
	tr := newTranslator()
	for _, raw := range []string{``, `not json`, `{"type":"message_end","message":"wrong shape"}`} {
		if got := tr.Translate("message_end", []byte(raw)); got != nil {
			t.Errorf("raw %q produced %d frames, want none", raw, len(got))
		}
	}
}

func TestClassifySeparatesMissingFromUnwanted(t *testing.T) {
	// The difference matters: "not for the console" is most events and is
	// entirely normal, while "for the console and unreadable" is a fault.
	cases := []struct {
		name    string
		known   bool
		renders bool
	}{
		{"turn_start", true, true},
		{"agent_end", true, true},
		{"message_start", true, false},
		{"agent_settled", true, false},
		// A name this build has never seen is honestly neither.
		{"invented_later", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		known, renders := Classify(tc.name)
		if known != tc.known || renders != tc.renders {
			t.Errorf("Classify(%q) = (%v, %v), want (%v, %v)", tc.name, known, renders, tc.known, tc.renders)
		}
	}
}

// --- a whole turn ------------------------------------------------------

func TestAFullTurnProducesTheSameFrameSequenceAsAnInProcessRun(t *testing.T) {
	// This is the property the whole file exists for: a conversation with
	// a node agent renders in the console exactly as one with the control
	// plane's own agent does. The sequence below is what the console has
	// always handled.
	tr := newTranslator()
	var got []wire.StreamEventType
	record := func(eventType, raw string) {
		for _, f := range tr.Translate(eventType, []byte(raw)) {
			got = append(got, f.Type)
		}
	}

	record("turn_start", turnStartJSON)
	record("message_update", textDeltaJSON)
	record("message_end", messageEndToolCallJSON)
	record("tool_execution_start", toolStartJSON)
	record("tool_execution_update", toolUpdateJSON)
	record("tool_execution_end", toolEndOKJSON)
	record("turn_start", turnStartJSON)
	record("message_end", messageEndTextJSON)
	record("agent_end", agentEndJSON)

	want := []wire.StreamEventType{
		wire.StreamAssistantStart,
		wire.StreamAssistantDelta,
		wire.StreamAssistantEnd,
		wire.StreamToolStart,
		wire.StreamToolUpdate,
		wire.StreamToolEnd,
		wire.StreamAssistantStart,
		wire.StreamAssistantEnd,
		wire.StreamDone,
	}
	if len(got) != len(want) {
		t.Fatalf("frame sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSequenceNumbersAreMonotonic(t *testing.T) {
	// A gap tells the console a frame was lost. The translator must never
	// manufacture one, including for events it drops.
	tr := newTranslator()
	var last int64
	record := func(eventType, raw string) {
		for _, f := range tr.Translate(eventType, []byte(raw)) {
			if f.Seq <= last {
				t.Fatalf("seq went %d -> %d", last, f.Seq)
			}
			last = f.Seq
		}
	}
	record("message_start", `{"type":"message_start"}`)
	record("turn_start", turnStartJSON)
	record("message_update", textDeltaJSON)
	record("message_update", `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"hidden"}}`)
	record("message_end", messageEndTextJSON)
	record("agent_settled", `{"type":"agent_settled"}`)
	record("agent_end", agentEndJSON)

	if last != 4 {
		t.Errorf("final seq = %d, want 4: the two dropped events must not consume numbers", last)
	}
}

func TestAProducedFrameDoesNotChangeWhenTheInputBufferIsReused(t *testing.T) {
	// The agent's record is reused by the transport after the call
	// returns. A frame that aliased it would show a later event's text
	// under an earlier frame's type.
	raw := []byte(textDeltaJSON)
	frames := newTranslator().Translate("message_update", raw)
	if len(frames) != 1 || frames[0].Assistant.Content != "checking" {
		t.Fatalf("frames = %+v", frames)
	}
	before := frames[0]

	for i := range raw {
		raw[i] = ' '
	}
	if before.Assistant.Content != "checking" {
		t.Errorf("the frame changed to %q after the input buffer was overwritten", before.Assistant.Content)
	}
}

func TestErrorFrameIsAvailableForTransportFailures(t *testing.T) {
	// A turn that ends with neither done nor error leaves a console
	// spinner that never stops. The transport supplies the error frame.
	tr := newTranslator()
	f := tr.Error("agent_transport_lost", "the tunnel to this node dropped", true)
	if f.Type != wire.StreamError {
		t.Errorf("type = %q", f.Type)
	}
	if !f.Error.Retryable {
		t.Error("retryable is false: a dropped tunnel is worth retrying")
	}
	if f.Seq != 1 {
		t.Errorf("seq = %d, want 1", f.Seq)
	}
}

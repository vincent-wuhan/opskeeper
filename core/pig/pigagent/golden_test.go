package pigagent

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigwire"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// The Phase B acceptance gate: "SSE frames are byte-identical frame for
// frame". The console is 52K lines of React that parses these frames and is
// not being rewritten, so the swap from the legacy translator to the PiG
// kernel must be invisible on the wire.
//
// Two tests carry that here, and they fail for different reasons:
//
//   - TestAgentGoldenMatchesTheConsoleContract pins the kernel's frame
//     stream against a checked-in golden, so any change in what the kernel
//     emits is a diff somebody has to read.
//   - TestAgentAndTranslatorAgreeFrameForFrame feeds one incident turn
//     through both adapters and requires identical bytes. That is the test
//     that makes the migration safe: the legacy path is still what the
//     fleet runs, so if the two ever diverge, whichever path is live
//     becomes the contract.

var updateGolden = flag.Bool("update-golden", false,
	"rewrite the .golden files from this run's output instead of asserting against them")

// agentScript reproduces the same incident turn as pigwire's golden script:
// the model looks at something (a read tool), the tool settles, the model
// answers. Every frame type the console renders appears, and the lifecycle
// events appear too, so the golden also pins that they stay silent.
func agentScript(t *testing.T) []wire.StreamEvent {
	t.Helper()
	m := newTestMapper()
	m.SetUsage(ports.TranscriptUsage{
		InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 800, CostUSD: 0.031,
	}, "gpt-5.6")

	var frames []wire.StreamEvent
	emit := func(ev agent.AgentEvent) { frames = append(frames, m.Map(ev)...) }

	m.TurnStarted()
	emit(agent.TurnStartEvent{})
	emit(agent.MessageUpdateEvent{AssistantMessageEvent: ai.TextDeltaEvent{Delta: "checking"}})
	emit(agent.MessageEndEvent{Message: assistantMessage("Let me look.", 1)})
	emit(agent.ToolExecutionStartEvent{ToolCallID: "tc-1", ToolName: "get_topology", Args: json.RawMessage(`{"root":"prod"}`)})
	emit(agent.ToolExecutionUpdateEvent{ToolCallID: "tc-1", ToolName: "tail_file", PartialResult: agent.AgentToolResult{
		Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "line 1\n"}},
	}})
	emit(agent.ToolExecutionEndEvent{ToolCallID: "tc-1", ToolName: "get_topology", Result: agent.AgentToolResult{
		Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "three tiers"}},
	}})
	// A refusal with no result of its own: the gate refused before the tool
	// ran, so the marker is the entire content. That is exactly what the
	// wire-side fixture carries, which is what makes the parity assertion
	// meaningful rather than a comparison of two different results.
	emit(agent.ToolExecutionEndEvent{ToolCallID: "tc-2", ToolName: "restart_service",
		Result: MarkBlocked(agent.AgentToolResult{}, "needs approval")})
	m.TurnStarted()
	emit(agent.TurnStartEvent{})
	emit(agent.MessageUpdateEvent{AssistantMessageEvent: ai.TextDeltaEvent{Delta: "checking"}})
	emit(agent.MessageEndEvent{Message: assistantMessage("The node is healthy.", 0)})
	emit(agent.AgentEndEvent{})

	// Lifecycle events the console has no frame for. A golden script that
	// omitted them would not notice if one started to render.
	emit(agent.AgentStartEvent{})
	emit(agent.MessageStartEvent{})
	emit(agent.TurnEndEvent{})

	return frames
}

func TestAgentGoldenMatchesTheConsoleContract(t *testing.T) {
	got := encodeGoldens(t, agentScript(t))
	path := filepath.Join("testdata", "mapper-session.golden")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update-golden to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the kernel's frame stream drifted from the golden the console renders:\n%s",
			firstDifference(want, got))
	}
}

// TestAgentAndTranslatorAgreeFrameForFrame is the migration's real safety
// net.
//
// The legacy node path (pigwire.Translator, fed raw RPC JSON) and the PiG
// kernel path (pigagent.Mapper, fed agent.AgentEvent) are two independent
// implementations of the same contract. While both exist, a fleet runs one
// or the other, and a console cannot tell which. If they disagree, the
// contract has quietly become "whatever this node happens to run".
//
// The inputs below are the same turn expressed twice — once as the JSON
// PiG puts on the wire, once as the Go events the kernel sees — so any
// difference in the frames is a difference in the adapters, not the input.
func TestAgentAndTranslatorAgreeFrameForFrame(t *testing.T) {
	legacy := pigwire.New(pigwire.Options{SessionID: "s-1", Now: fixedClock()})
	var fromTranslator []wire.StreamEvent
	for _, step := range translatorScript {
		fromTranslator = append(fromTranslator, legacy.Translate(step.event, []byte(step.body))...)
	}

	fromKernel := agentScript(t)

	if len(fromKernel) != len(fromTranslator) {
		t.Fatalf("kernel produced %d frames, translator produced %d:\n%s",
			len(fromKernel), len(fromTranslator), frameList(fromTranslator, fromKernel))
	}
	for i := range fromTranslator {
		a, err := json.Marshal(fromKernel[i])
		if err != nil {
			t.Fatalf("marshal kernel frame %d: %v", i, err)
		}
		b, err := json.Marshal(fromTranslator[i])
		if err != nil {
			t.Fatalf("marshal translator frame %d: %v", i, err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("frame %d differs between the kernel and the translator, so the wire contract depends on which adapter a node runs:\n  kernel:     %s\n  translator: %s",
				i, a, b)
		}
	}
}

// translatorScript mirrors pigwire's own golden script. It is duplicated
// here rather than exported from that package because the whole point is
// two independent inputs: a shared fixture would let one adapter's bug
// define the other's expectation.
var translatorScript = []struct {
	event string
	body  string
}{
	{"turn_start", `{"type":"turn_start"}`},
	{"message_update", `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"checking"}}`},
	{"message_end", `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Let me look."},{"type":"toolCall","id":"tc-1","name":"get_topology","arguments":{"root":"prod"}}],"usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":15,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}`},
	{"tool_execution_start", `{"type":"tool_execution_start","toolCallId":"tc-1","toolName":"get_topology","args":{"root":"prod"}}`},
	{"tool_execution_update", `{"type":"tool_execution_update","toolCallId":"tc-1","toolName":"tail_file","partialResult":{"content":[{"type":"text","text":"line 1\n"}]}}`},
	{"tool_execution_end", `{"type":"tool_execution_end","toolCallId":"tc-1","toolName":"get_topology","result":{"content":[{"type":"text","text":"three tiers"}]},"isError":false}`},
	{"tool_execution_end", `{"type":"tool_execution_end","toolCallId":"tc-2","toolName":"restart_service","result":{"content":[{"type":"text","text":"opskeeper: tool blocked by host policy: needs approval"}]},"isError":true}`},
	{"turn_start", `{"type":"turn_start"}`},
	{"message_update", `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"checking"}}`},
	{"message_end", `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The node is healthy."}],"model":"gpt-5.6","usage":{"input":1200,"output":340,"cacheRead":800,"cacheWrite":0,"totalTokens":2340,"cost":{"input":0.01,"output":0.02,"cacheRead":0.001,"cacheWrite":0,"total":0.031}}}}`},
	{"agent_end", `{"type":"agent_end","messages":[],"willRetry":false}`},
}

// TestAgentFramesAreGapFree pins seq separately from the bytes: a golden
// regenerated with a hole in it would satisfy the byte comparison and still
// trip the console's gap detector.
func TestAgentFramesAreGapFree(t *testing.T) {
	frames := agentScript(t)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	for i, f := range frames {
		if want := int64(i + 1); f.Seq != want {
			t.Fatalf("frame %d has seq %d, want %d: a gap tells the console a frame was lost", i, f.Seq, want)
		}
	}
}

// TestAgentGoldenCoversEveryRenderableType keeps the script honest. Without
// it, a script that stopped exercising a frame kind would still produce a
// passing byte comparison and the console would lose that frame silently.
func TestAgentGoldenCoversEveryRenderableType(t *testing.T) {
	seen := map[wire.StreamEventType]bool{}
	for _, f := range agentScript(t) {
		seen[f.Type] = true
	}
	for _, want := range []wire.StreamEventType{
		wire.StreamAssistantStart, wire.StreamAssistantDelta, wire.StreamAssistantEnd,
		wire.StreamToolStart, wire.StreamToolUpdate, wire.StreamToolEnd, wire.StreamDone,
	} {
		if !seen[want] {
			t.Errorf("the golden script never produces a %s frame; the console renders it, so it must be pinned", want)
		}
	}
}

// TestUnrenderableAgentEventsStaySilent is the negative half of the
// coverage check: the lifecycle events in the script must contribute no
// frames, so that "silence" is a pinned property and not an accident of
// which events the script happened to include.
func TestUnrenderableAgentEventsStaySilent(t *testing.T) {
	m := newTestMapper()
	for _, ev := range []agent.AgentEvent{
		agent.AgentStartEvent{},
		agent.MessageStartEvent{},
		agent.TurnEndEvent{},
	} {
		if got := m.Map(ev); got != nil {
			t.Errorf("%T produced %d frames, want none: the console has no renderer for it", ev, len(got))
		}
	}
	if m.seq != 0 {
		t.Errorf("seq advanced to %d while mapping unrenderable events", m.seq)
	}
}

func frameList(a, b []wire.StreamEvent) string {
	var sb strings.Builder
	sb.WriteString("translator:")
	for _, f := range a {
		sb.WriteString("\n  " + string(f.Type))
	}
	sb.WriteString("\nkernel:")
	for _, f := range b {
		sb.WriteString("\n  " + string(f.Type))
	}
	return sb.String()
}

func encodeGoldens(t *testing.T, frames []wire.StreamEvent) []byte {
	t.Helper()
	var b strings.Builder
	for _, f := range frames {
		line, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		b.Write(line)
		b.WriteByte(0x0a)
	}
	return []byte(b.String())
}

func firstDifference(want, got []byte) string {
	wl := bytes.Split(want, []byte{0x0a})
	gl := bytes.Split(got, []byte{0x0a})
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g []byte
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if !bytes.Equal(w, g) {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + string(w) + "\n  got:  " + string(g)
		}
	}
	return "no differing line found (length mismatch only)"
}

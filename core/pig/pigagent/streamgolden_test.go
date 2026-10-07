package pigagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// This file is the other half of the Phase B acceptance gate.
//
// golden_test.go pins the Mapper against a hand-written agent.AgentEvent
// script, and kernel_test.go runs real turns through PiG's faux provider.
// Neither file pins the two together, and the seam between them is the
// whole risk of the migration. The console's promise is that swapping the
// legacy translator for the PiG kernel is invisible on the wire, and the
// part of that promise a hand-fed script cannot check is exactly the part
// that changes: what PiG's stream actually emits. A hand-written script
// says nothing about whether a new event kind now lands between
// message_update and message_end, whether a tool call's arguments arrive
// as deltas the mapper never forwards, or whether usage rides on the same
// frame the console reads it from. Those are all questions about PiG's
// event stream, so the golden has to be driven by one.

// goldenChunkTokens pins PiG's text chunker to a fixed width.
//
// ai.splitByTokenSize draws its chunk size from math/rand between
// MinTokenSize and MaxTokenSize, so a golden over a real stream is
// impossible unless the two ends coincide — rand.IntN(1) is the only
// non-random draw available. A test that tried to pin deltas without this
// would be green on the run that generated the golden and red on the next
// one, which is the worst failure mode a golden has: it trains people to
// re-record instead of to read the diff.
const goldenChunkTokens = 1

// newGoldenFauxModel is newFauxModel with the chunker pinned. It is a
// separate constructor rather than a flag because the other kernel tests
// assert on settled content, where chunk boundaries are invisible, and
// silently making every one of them depend on this constant would spread a
// golden concern through tests that do not have one.
func newGoldenFauxModel(t *testing.T, steps ...ai.FauxResponseStep) *ai.Model {
	t.Helper()
	provider := ai.NewFauxProvider(ai.FauxConfig{
		Model:           "faux-1",
		Models:          []ai.FauxModelDefinition{{ID: "faux-1", Name: "Faux"}},
		TokensPerSecond: 1_000_000,
		MinTokenSize:    goldenChunkTokens,
		MaxTokenSize:    goldenChunkTokens,
	})
	provider.SetResponses(steps)
	model := provider.GetModel("faux-1")
	if model == nil {
		t.Fatal("faux provider did not register faux-1")
	}
	return model
}

// goldenTurnScript is one incident: the model asks for a read, then for a
// change that the host refuses, then answers.
//
// It is deliberately the turn shape an operator actually produces — read,
// blocked write, conclusion — because a golden built from the shapes that
// are convenient to write is a golden that never met production. The
// blocked call is the frame the console renders differently from an error,
// so it is the one most worth pinning.
func goldenTurnScript() []ai.FauxResponseStep {
	return []ai.FauxResponseStep{
		goldenToolStep("tc-topo", "get_topology", map[string]any{"root": "prod"}),
		goldenToolStep("tc-web", "restart_service", map[string]any{"service": "web"}),
		textStep("The topology has three tiers. I did not restart web: the change is frozen."),
	}
}

// goldenToolStep is toolStep with an explicit call id. The shared helper
// hardcodes tc-1, which is fine for a test that makes one call and wrong
// for a golden: the console binds tool frames to a call id, so a turn that
// reused one would pin an artifact of the test helper as if it were the
// contract, and a real provider never reuses an id inside a turn.
func goldenToolStep(id, name string, args map[string]any) ai.FauxResponseStep {
	return ai.FauxStaticStep(ai.FauxResponse{
		Content:    []ai.FauxContentBlock{ai.FauxToolCall(name, args, id)},
		StopReason: string(ai.StopReasonToolUse),
	})
}

// runGoldenStreamTurn drives the real Kernel over the real PiG event stream
// and returns what the console would have received.
func runGoldenStreamTurn(t *testing.T) []wire.StreamEvent {
	t.Helper()

	readonly := classTool("get_topology", domain.ClassRead)
	mutating := &fakeTool{schema: ports.ToolSchema{
		Name:       "restart_service",
		Class:      domain.ClassDestructive,
		Parameters: json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"}}}`),
	}, out: "restarted"}

	gate := &recordingGate{decide: func(req ports.ApprovalRequest) (ports.Decision, error) {
		if req.ToolName == "restart_service" {
			return ports.Decision{
				RequestID: req.ID,
				Digest:    CallDigest(req.ToolName, req.Arguments),
				Decision:  ports.ApprovalDenied,
				DecidedBy: "op-1",
				Note:      "change freeze",
			}, nil
		}
		return grantFor("")(req)
	}}

	model := newGoldenFauxModel(t, goldenTurnScript()...)
	sink := &collectSink{}
	k, err := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
			return Deps{Tools: staticBag{tools: []ports.Tool{readonly, mutating}}, Gate: gate}, nil
		},
		Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("NewKernel: %v", err)
	}
	if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{
		SessionID: "s-1", UserText: "map prod and restart web", Role: "admin",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sink.Frames()
}

// Read the golden before trusting it: frames 002, 007 and 014 are
// assistant_end frames carrying no text, and nothing in PiG produces them on
// purpose.
//
// PiG ends the lifecycle of every message it appends, not only the
// assistant's. agent_loop.go:112-116 walks the prompt messages and emits a
// MessageStartEvent/MessageEndEvent pair for each, and appendMessage does the
// same for the tool result. summarize() reads those as a message with no
// text and no tool calls, and the mapper renders that as a settled assistant
// bubble. So a tool round produces three assistant_end frames where one
// carries the answer.
//
// That is not a defect, and the reason matters more than the frames do: the
// kernel's frame stream is not the console's frame stream.
// chatruntime.kernelSink is the fold between them — it drops
// assistant_start and assistant_delta outright ("emitting either would show a
// phantom empty bubble") and keeps only the newest assistant_end per
// session, so the console receives exactly one settled bubble per turn
// (kernelsink.go, with TestASecondAssistantEndSupersedesTheFirst pinning the
// supersede rule). The empty frames are load-bearing input to that rule, not
// output the operator sees.
//
// So this golden pins a shape that only makes sense once you know about the
// fold, and that is exactly why it is worth having: the hand-written script
// in golden_test.go fed only assistant-shaped message_end events, so it
// never surfaced the real stream's shape at all, and a reader of this file
// without the fold in hand would file a bug against OpsKeeper that does not
// exist. Written down here so the next reader does not have to rediscover it.

// coalesceDeltas merges runs of assistant_delta frames into one.
//
// Delta boundaries are the provider's tokenizer, not our contract: the
// console concatenates whatever arrives and re-renders on assistant_end, so
// a provider that chunks by four bytes and one that streams a whole
// paragraph are both correct. Pinning the boundaries would make this golden
// a tripwire on PiG's chunker rather than on our mapper, and the failure it
// produced would be a diff nobody could act on.
//
// What is left after coalescing is the part that is ours — which frame
// kinds arrive, in what order, carrying what — and that is exactly what a
// wire contract is made of. Loss is not swept up by the merge: the deltas
// are concatenated rather than sampled, and TestStreamDeltasReassembleThe
// Answer checks the result against assistant_end, so a dropped chunk shows
// up as a content mismatch rather than as a quietly shorter golden.
func coalesceDeltas(frames []wire.StreamEvent) []wire.StreamEvent {
	out := make([]wire.StreamEvent, 0, len(frames))
	for _, f := range frames {
		if f.Type == wire.StreamAssistantDelta && len(out) > 0 {
			if last := &out[len(out)-1]; last.Type == wire.StreamAssistantDelta {
				last.Assistant.Content += f.Assistant.Content
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// renderStreamFrames writes one line per frame: its sequence number, its
// type, and the fields the console reads off that kind.
//
// The sequence number is printed rather than implied so that a frame the
// mapper started emitting and then stopped — the failure a migration
// actually produces — shows up as a changed line instead of a changed
// count somewhere below the diff.
func renderStreamFrames(frames []wire.StreamEvent) []byte {
	var b strings.Builder
	for _, f := range frames {
		fmt.Fprintf(&b, "%03d %s", f.Seq, f.Type)
		switch f.Type {
		case wire.StreamAssistantStart:
			fmt.Fprintf(&b, " pending=%d id=%q", f.Assistant.PendingToolCalls, f.Assistant.MessageID)
		case wire.StreamAssistantDelta:
			fmt.Fprintf(&b, " %q", f.Assistant.Content)
		case wire.StreamAssistantEnd:
			fmt.Fprintf(&b, " pending=%d id=%q %q",
				f.Assistant.PendingToolCalls, f.Assistant.MessageID, f.Assistant.Content)
		case wire.StreamToolStart:
			fmt.Fprintf(&b, " %s %s args=%s", f.Tool.ToolCallID, f.Tool.Name, f.Tool.ArgsJSON)
		case wire.StreamToolUpdate:
			fmt.Fprintf(&b, " %s %s %q", f.Tool.ToolCallID, f.Tool.Name, f.Tool.ResultJSON)
		case wire.StreamToolEnd:
			fmt.Fprintf(&b, " %s %s status=%s %q", f.Tool.ToolCallID, f.Tool.Name, f.Tool.Status, f.Tool.ResultJSON)
		case wire.StreamDone:
			fmt.Fprintf(&b, " iterations=%d tool_calls=%d", f.Done.Iterations, f.Done.ToolCalls)
		case wire.StreamError:
			fmt.Fprintf(&b, " code=%q retryable=%v %q", f.Error.Code, f.Error.Retryable, f.Error.Message)
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func TestStreamGoldenMatchesTheConsoleContract(t *testing.T) {
	got := renderStreamFrames(coalesceDeltas(runGoldenStreamTurn(t)))
	path := filepath.Join("testdata", "stream-turn.golden")
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
		t.Errorf("a real PiG stream no longer renders the frames the console was written against:\n%s",
			firstDifference(want, got))
	}
}

// TestStreamDeltasReassembleTheAnswer is the check that makes coalescing
// safe. The golden above merges deltas, so on its own it would still pass
// if the mapper dropped every other delta and the console rendered half a
// sentence. This asserts the merged text against the settled frame, which
// is the only place the full answer exists.
func TestStreamDeltasReassembleTheAnswer(t *testing.T) {
	var streamed, settled string
	for _, f := range runGoldenStreamTurn(t) {
		switch f.Type {
		case wire.StreamAssistantDelta:
			streamed += f.Assistant.Content
		case wire.StreamAssistantEnd:
			settled += f.Assistant.Content
		}
	}
	if streamed == "" {
		t.Fatal("the turn streamed no deltas: the console would render nothing until the turn ended")
	}
	if streamed != settled {
		t.Errorf("the deltas and the settled frame disagree:\n  streamed: %q\n  settled:  %q",
			streamed, settled)
	}
}

// TestStreamGoldenCoversTheFramesThatDiffer pins coverage rather than
// bytes: a golden that quietly stopped exercising the blocked tool frame
// would still compare equal to itself.
func TestStreamGoldenCoversTheFramesThatDiffer(t *testing.T) {
	frames := coalesceDeltas(runGoldenStreamTurn(t))
	seen := map[wire.StreamEventType]bool{}
	for _, f := range frames {
		seen[f.Type] = true
	}
	for _, want := range []wire.StreamEventType{
		wire.StreamAssistantStart, wire.StreamAssistantDelta, wire.StreamAssistantEnd,
		wire.StreamToolStart, wire.StreamToolEnd, wire.StreamDone,
	} {
		if !seen[want] {
			t.Errorf("the golden no longer produces a %s frame", want)
		}
	}
	if seen[wire.StreamError] {
		t.Error("a successful turn emitted an error frame")
	}

	// The blocked call must arrive as blocked, not as an error: the console
	// renders the two differently and collapsing them would train operators
	// to read a policy refusal as a failure.
	var blocked *wire.ToolFrame
	for _, f := range frames {
		if f.Type == wire.StreamToolEnd && f.Tool.Name == "restart_service" {
			blocked = f.Tool
		}
	}
	if blocked == nil {
		t.Fatal("the denied restart produced no tool_end frame")
	}
	if blocked.Status != wire.ToolBlocked {
		t.Errorf("the denied restart reported status %q, want %q", blocked.Status, wire.ToolBlocked)
	}
}

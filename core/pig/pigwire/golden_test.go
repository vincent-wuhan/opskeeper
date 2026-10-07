package pigwire

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// Golden frames for the node-side event stream.
//
// This is the Phase B acceptance gate "SSE frames are byte-identical frame
// for frame". It exists because the console has 52K lines of React that
// render these frames and is not being rewritten, so the only thing that
// may change under it is nothing. `Mapper`/`Translator` unit tests assert
// individual fields; this asserts the whole ordered stream, which is what a
// console actually consumes.
//
// The input is the raw event JSON PiG's `cmd/pig/rpc_events.go` puts on the
// wire, written out as text rather than built from Go structs. A golden
// test whose input came from the same types it decodes would keep passing
// after PiG renamed a field, and the failure it is meant to catch is
// exactly that.

// blockedEndJSON is a host-policy refusal. It is in the script because the
// console renders a refusal differently from a failure, so "blocked" is part
// of the contract and not merely a status the translator happens to support.
// Bumping the id keeps it from being confused with the earlier call.
const blockedEndJSON = `{"type":"tool_execution_end","toolCallId":"tc-2","toolName":"restart_service","result":{"content":[{"type":"text","text":"opskeeper: tool blocked by host policy: needs approval"}]},"isError":true}`

var updateGolden = flag.Bool("update-golden", false,
	"rewrite the .golden files from this run's output instead of asserting against them")

// sessionScript is one incident turn pair: the model asks for a tool, the
// tool settles, the model answers. Every renderable frame type appears at
// least once, and the unrenderable ones appear so the golden also pins that
// they produce nothing.
var sessionScript = []struct {
	event string
	body  string
}{
	{"turn_start", turnStartJSON},
	{"message_update", textDeltaJSON},
	{"message_end", messageEndToolCallJSON},
	{"tool_execution_start", toolStartJSON},
	{"tool_execution_update", toolUpdateJSON},
	{"tool_execution_end", toolEndOKJSON},
	{"tool_execution_end", blockedEndJSON},
	{"turn_start", turnStartJSON},
	{"message_update", textDeltaJSON},
	{"message_end", messageEndTextJSON},
	{"agent_end", agentEndJSON},
	// Lifecycle events with no console counterpart. They must contribute no
	// frame, and a golden that omitted them would not notice if one started
	// to.
	{"message_start", `{"type":"message_start"}`},
	{"turn_end", `{"type":"turn_end"}`},
	{"agent_settled", `{"type":"agent_settled"}`},
}

// TestGoldenFramesMatchTheConsoleContract replays the script and compares
// the frame stream to the golden file.
func TestGoldenFramesMatchTheConsoleContract(t *testing.T) {
	tr := newTranslator()

	var frames []wire.StreamEvent
	for _, step := range sessionScript {
		frames = append(frames, tr.Translate(step.event, []byte(step.body))...)
	}

	got := encodeGoldens(t, frames)
	path := filepath.Join("testdata", "translator-session.golden")
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
		t.Errorf("frame stream differs from the golden the console renders:\n%s",
			firstDifference(want, got))
	}
}

// TestGoldenFrameSequenceIsGapFreeIsThePointOfSeq pins the property the
// console's gap detection depends on, separately from the byte comparison:
// if the golden itself were regenerated with a hole in it, the byte test
// would happily agree.
func TestGoldenFrameSequenceIsGapFreeIsThePointOfSeq(t *testing.T) {
	tr := newTranslator()

	var frames []wire.StreamEvent
	for _, step := range sessionScript {
		frames = append(frames, tr.Translate(step.event, []byte(step.body))...)
	}
	if len(frames) == 0 {
		t.Fatal("script produced no frames")
	}
	for i, f := range frames {
		if want := int64(i + 1); f.Seq != want {
			t.Fatalf("frame %d has seq %d, want %d: a gap tells the console a frame was lost", i, f.Seq, want)
		}
		if f.SessionID != "s-1" {
			t.Fatalf("frame %d carries session %q, want s-1", i, f.SessionID)
		}
	}
}

// TestGoldenCoversEveryRenderableType guards the golden's own coverage.
//
// A script that stopped exercising one frame type would still produce a
// passing byte comparison, and the console would lose a frame kind with
// nobody noticing. This fails the moment the script stops covering the set
// the wire defines.
func TestGoldenCoversEveryRenderableType(t *testing.T) {
	tr := newTranslator()
	seen := map[wire.StreamEventType]bool{}
	for _, step := range sessionScript {
		for _, f := range tr.Translate(step.event, []byte(step.body)) {
			seen[f.Type] = true
		}
	}
	// Every type a node-side translator can emit. approval_pending and
	// approval_resolved are not here: they are built by the host gate from a
	// live approval, not from an agent event.
	for _, want := range []wire.StreamEventType{
		wire.StreamAssistantStart,
		wire.StreamAssistantDelta,
		wire.StreamAssistantEnd,
		wire.StreamToolStart,
		wire.StreamToolUpdate,
		wire.StreamToolEnd,
		wire.StreamDone,
	} {
		if !seen[want] {
			t.Errorf("the golden script never produces a %s frame; the console renders it, so it must be pinned", want)
		}
	}
}

// TestGoldenBlockedToolIsItsOwnTerminalState pins the one classification the
// console branches on: a policy refusal is not a broken tool.
func TestGoldenBlockedToolIsItsOwnTerminalState(t *testing.T) {
	tr := newTranslator()
	frames := tr.Translate("tool_execution_end", []byte(toolEndBlockedJSON))
	if len(frames) != 1 || frames[0].Tool == nil {
		t.Fatalf("blocked end produced %d frames", len(frames))
	}
	if frames[0].Tool.Status != wire.ToolBlocked {
		t.Fatalf("status = %q, want %q", frames[0].Tool.Status, wire.ToolBlocked)
	}
}

// writeGoldens renders one frame per line, which keeps a diff readable: a
// changed frame is one changed line, not a re-wrapped blob.
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

// firstDifference names the first line that differs, because "the stream
// differs" is not actionable and "frame 4 changed" is.
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
			return "line " + itoa(i+1) + ":\n  want: " + string(w) + "\n  got:  " + string(g)
		}
	}
	return "no differing line found (length mismatch only)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

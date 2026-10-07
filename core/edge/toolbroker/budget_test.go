package toolbroker

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// The output ceiling is enforced here, in the broker, and nowhere else —
// because this is the last point at which a tool's answer is still in host
// hands. The tests below are the ones that would fail if the bound moved
// into a tool, where a package could decline to honour it.

func replyOfBytes(n int) json.RawMessage {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'x'
	}
	return json.RawMessage(`{"matches":"` + string(out) + `"}`)
}

// The tool returned something enormous. What must not happen is that the
// model receives it: a gigabyte of grep hits does not fail, it just ends the
// investigation with a context window full of text nobody asked for.
func TestAnOversizedReplyIsReplacedRatherThanHandedBack(t *testing.T) {
	invoker := &fakeInvoker{result: replyOfBytes(4096)}
	conn, _ := serve(t, Options{
		Invoke:    invoker,
		SpillDir:  t.TempDir(),
		BudgetFor: func(string) Budget { return Budget{MaxOutputBytes: 4096} },
	})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s", ToolName: "host_grep_file", Arguments: map[string]any{}})

	if reply.Error != "" {
		t.Fatalf("an oversized reply became an error: %s", reply.Error)
	}
	if int64(len(reply.Result)) > 4096 {
		t.Fatalf("the reply is %d bytes; the declared ceiling was 4096", len(reply.Result))
	}
	var notice struct {
		Truncated   bool   `json:"truncated"`
		Tool        string `json:"tool"`
		Bytes       int    `json:"bytes"`
		LimitBytes  int64  `json:"limit_bytes"`
		Spilled     bool   `json:"spilled"`
		SpillPath   string `json:"spill_path"`
		Explanation string `json:"explanation"`
	}
	if err := json.Unmarshal(reply.Result, &notice); err != nil {
		t.Fatalf("the notice is not readable JSON, so a model cannot act on it: %v (%s)", err, reply.Result)
	}
	if !notice.Truncated || notice.Tool != "host_grep_file" {
		t.Errorf("the notice does not name what was cut: %+v", notice)
	}
	if notice.LimitBytes != 4096 || notice.Bytes != len(replyOfBytes(4096)) {
		t.Errorf("the notice reports bytes=%d limit=%d, want the real pair", notice.Bytes, notice.LimitBytes)
	}
	if !notice.Spilled || notice.SpillPath == "" {
		t.Fatalf("the full reply was not kept anywhere, so the model cannot ask for the rest: %+v", notice)
	}
	if !strings.Contains(notice.Explanation, notice.SpillPath) {
		t.Errorf("the explanation does not tell the model where the output went: %q", notice.Explanation)
	}
	if _, err := os.Stat(notice.SpillPath); err != nil {
		t.Errorf("the spill path does not exist: %v", err)
	}
}

// Cutting a JSON document at a byte boundary produces something a model
// cannot parse, and a parse failure reads as a broken tool rather than a
// truncated one. The replacement has to be a valid document every time.
func TestTheTruncationNoticeIsAlwaysAValidDocument(t *testing.T) {
	// 4097 bytes is the awkward length: it lands in the middle of the
	// closing brace of the payload.
	invoker := &fakeInvoker{result: replyOfBytes(4097)}
	conn, _ := serve(t, Options{
		Invoke:    invoker,
		SpillDir:  t.TempDir(),
		BudgetFor: func(string) Budget { return Budget{MaxOutputBytes: 4096} },
	})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s", ToolName: "host_grep_file"})

	if !json.Valid(reply.Result) {
		t.Errorf("the reply is not valid JSON: %s", reply.Result)
	}
}

// The notice has to fit inside the ceiling it is enforcing. A ceiling small
// enough that the notice cannot state the ceiling is answered as a refusal —
// the host's own sentence, not the tool's payload — rather than with a reply
// that breaks the promise the limit just made.
func TestACeilingTooSmallForItsOwnNoticeIsRefusedRatherThanBroken(t *testing.T) {
	invoker := &fakeInvoker{result: replyOfBytes(8192)}
	conn, _ := serve(t, Options{
		Invoke:    invoker,
		SpillDir:  t.TempDir(),
		BudgetFor: func(string) Budget { return Budget{MaxOutputBytes: 128} },
	})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s", ToolName: "host_grep_file"})

	if len(reply.Result) != 0 {
		t.Errorf("a result was returned against a ceiling too small to describe: %s", reply.Result)
	}
	if reply.Error == "" {
		t.Fatal("the oversized reply was neither truncated nor refused")
	}
	if !strings.Contains(reply.Error, "128") {
		t.Errorf("the refusal does not name the limit: %q", reply.Error)
	}
	if !strings.Contains(reply.Error, "narrow the query") {
		t.Errorf("the refusal tells the model nothing it can act on: %q", reply.Error)
	}
}

// A tool that declares nothing still gets a ceiling. A limit that only
// exists when a package opted into it is not a limit, and the tools that
// flood a context are exactly the ones nobody remembered to bound.
func TestAToolThatDeclaresNothingStillGetsTheHostDefault(t *testing.T) {
	invoker := &fakeInvoker{result: replyOfBytes(int(skill.DefaultMaxOutputBytes) + 1)}
	conn, _ := serve(t, Options{Invoke: invoker, SpillDir: t.TempDir()})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s", ToolName: "host_probe_dns"})

	var notice struct {
		Truncated  bool  `json:"truncated"`
		LimitBytes int64 `json:"limit_bytes"`
	}
	if err := json.Unmarshal(reply.Result, &notice); err != nil {
		t.Fatalf("the reply was not replaced with a notice: %v (%s)", err, reply.Result)
	}
	if !notice.Truncated || notice.LimitBytes != skill.DefaultMaxOutputBytes {
		t.Errorf("an undeclared tool got limit=%d, want the %d byte host default",
			notice.LimitBytes, skill.DefaultMaxOutputBytes)
	}
}

// The whole mechanism only matters for the replies that are large, so a
// reply inside the ceiling has to come back untouched — byte for byte, not
// re-serialised.
func TestAReplyInsideTheCeilingIsReturnedUntouched(t *testing.T) {
	original := json.RawMessage(`{"entries":[{"level":"err"}],"total":1}`)
	invoker := &fakeInvoker{result: original}
	conn, _ := serve(t, Options{
		Invoke:    invoker,
		SpillDir:  t.TempDir(),
		BudgetFor: func(string) Budget { return Budget{MaxOutputBytes: 1 << 20} },
	})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s", ToolName: "host_dmesg"})

	if string(reply.Result) != string(original) {
		t.Errorf("a reply inside the ceiling was rewritten:\n got %s\nwant %s", reply.Result, original)
	}
}

// A tool that ignores its context must not be able to hold a broker slot
// open for ever, and the per-tool ceiling is where that becomes possible:
// the global five minutes is right for a support bundle and wrong for
// everything else.
func TestAPerToolTimeoutReplacesTheGlobalOne(t *testing.T) {
	invoker := &fakeInvoker{
		result: json.RawMessage(`{"ok":true}`),
		block:  make(chan struct{}),
	}
	t.Cleanup(func() { close(invoker.block) })

	var deadlines []time.Duration
	conn, _ := serve(t, Options{
		Invoke: invoker,
		BudgetFor: func(toolName string) Budget {
			if toolName == "host_grep_file" {
				return Budget{MaxOutputBytes: 1 << 20, Timeout: 150 * time.Millisecond}
			}
			return Budget{}
		},
	})

	start := time.Now()
	reply := call(t, conn, wire.ToolRequest{SessionID: "s", ToolName: "host_grep_file"})
	elapsed := time.Since(start)
	deadlines = append(deadlines, elapsed)

	if reply.Error == "" {
		t.Error("a tool that ignores its deadline was allowed to run past it")
	}
	if elapsed > 5*time.Second {
		t.Errorf("the call took %s; the declared 150ms ceiling did not apply", elapsed)
	}
	if len(deadlines) == 0 {
		t.Fatal("no deadline was observed")
	}
}

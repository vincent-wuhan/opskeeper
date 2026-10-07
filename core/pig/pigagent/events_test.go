package pigagent

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// fixedClock gives every frame a stable timestamp so the assertions below
// can compare whole frames rather than string prefixes.
func fixedClock() func() time.Time {
	t := time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func newTestMapper() *Mapper {
	return NewMapper(MapperOptions{SessionID: "s-1", Role: "admin", Now: fixedClock()})
}

// assistantMessage builds a settled assistant message with the given text
// and tool calls, so tests can assert the summarisation without constructing
// a full transcript.
func assistantMessage(text string, toolCalls int) agent.AgentMessage {
	content := []ai.AssistantContentBlock{}
	if text != "" {
		content = append(content, ai.TextContent{Text: text})
	}
	for i := 0; i < toolCalls; i++ {
		content = append(content, ai.ToolCall{
			ID:        "tc-" + string(rune('a'+i)),
			Name:      "get_topology",
			Arguments: ai.JsonObject{},
		})
	}
	return agent.AgentMessage{Assistant: &agent.AssistantMessage{Content: content}}
}

func TestMapperTurnStartOpensTheBubble(t *testing.T) {
	m := newTestMapper()
	m.TurnStarted()

	frames := m.Map(agent.TurnStartEvent{})
	if len(frames) != 1 {
		t.Fatalf("TurnStartEvent produced %d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.Type != wire.StreamAssistantStart {
		t.Errorf("type = %q, want %q", f.Type, wire.StreamAssistantStart)
	}
	if f.Assistant == nil {
		t.Fatal("assistant frame missing")
	}
	if f.Assistant.Content != "" {
		t.Errorf("content = %q, want empty: the console opens the bubble before any text", f.Assistant.Content)
	}
	if f.Assistant.PendingToolCalls != 0 {
		t.Errorf("pending = %d, want 0: no tools have been requested yet", f.Assistant.PendingToolCalls)
	}
	if f.SessionID != "s-1" || f.Iteration != 1 || f.Seq != 1 {
		t.Errorf("envelope = %+v, want session s-1 iteration 1 seq 1", f)
	}
}

func TestMapperDeltasCarryOnlyText(t *testing.T) {
	m := newTestMapper()

	frames := m.Map(agent.MessageUpdateEvent{AssistantMessageEvent: ai.TextDeltaEvent{Delta: "partial"}})
	if len(frames) != 1 {
		t.Fatalf("text delta produced %d frames, want 1", len(frames))
	}
	if frames[0].Type != wire.StreamAssistantDelta {
		t.Errorf("type = %q", frames[0].Type)
	}
	if frames[0].Assistant.Content != "partial" {
		t.Errorf("content = %q", frames[0].Assistant.Content)
	}

	// An empty delta is not an event worth a frame; emitting one would
	// make the console append a no-op.
	if got := m.Map(agent.MessageUpdateEvent{AssistantMessageEvent: ai.TextDeltaEvent{Delta: ""}}); got != nil {
		t.Errorf("empty delta produced %d frames, want none", len(got))
	}
}

func TestMapperDropsNonTextDeltas(t *testing.T) {
	m := newTestMapper()

	// Thinking deltas are the load-bearing case: a shared incident view is
	// not the place for a model's private reasoning, so they must not
	// reach the console even when the provider streams them.
	events := []agent.MessageUpdateEvent{
		{AssistantMessageEvent: ai.ThinkingDeltaEvent{Delta: "reasoning…"}},
		{AssistantMessageEvent: ai.TextStartEvent{}},
		{AssistantMessageEvent: ai.DoneEvent{}},
		{AssistantMessageEvent: ai.ErrorEvent{}},
	}
	for i, ev := range events {
		if got := m.Map(ev); got != nil {
			t.Errorf("event %d produced %d frames, want none: %T", i, len(got), ev.AssistantMessageEvent)
		}
	}
	if m.seq != 0 {
		t.Errorf("seq advanced to %d while mapping no-op events", m.seq)
	}
}

func TestMapperMessageEndCountsPendingToolCalls(t *testing.T) {
	m := newTestMapper()
	m.TurnStarted()

	frames := m.Map(agent.MessageEndEvent{Message: assistantMessage("I will look", 2)})
	if len(frames) != 1 {
		t.Fatalf("MessageEndEvent produced %d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.Type != wire.StreamAssistantEnd {
		t.Errorf("type = %q", f.Type)
	}
	if f.Assistant.Content != "I will look" {
		t.Errorf("content = %q", f.Assistant.Content)
	}
	if f.Assistant.PendingToolCalls != 2 {
		t.Errorf("pending = %d, want 2", f.Assistant.PendingToolCalls)
	}
	if f.Assistant.CreatedAt == "" {
		t.Error("created_at empty: the console timestamps the bubble from this")
	}
}

func TestMapperMessageEndWithoutAssistantContent(t *testing.T) {
	m := newTestMapper()
	frames := m.Map(agent.MessageEndEvent{Message: agent.AgentMessage{}})
	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	if frames[0].Assistant.PendingToolCalls != 0 {
		t.Errorf("pending = %d, want 0", frames[0].Assistant.PendingToolCalls)
	}
}

func TestMapperToolStartDecrementsPending(t *testing.T) {
	m := newTestMapper()
	m.TurnStarted()
	m.Map(agent.MessageEndEvent{Message: assistantMessage("", 2)})

	frames := m.Map(agent.ToolExecutionStartEvent{ToolCallID: "tc-a", ToolName: "get_topology", Args: json.RawMessage(`{"root":1}`)})
	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.Type != wire.StreamToolStart {
		t.Errorf("type = %q", f.Type)
	}
	if f.Tool.ArgsJSON != `{"root":1}` {
		t.Errorf("args_json = %q: the console shows the operator what was about to run", f.Tool.ArgsJSON)
	}
	if f.Tool.Status != "" {
		t.Errorf("status = %q, want empty: a started tool has not settled", f.Tool.Status)
	}

	// The second start confirms the counter drains rather than sticking.
	frames = m.Map(agent.ToolExecutionStartEvent{ToolCallID: "tc-b", ToolName: "get_topology"})
	if frames[0].Type != wire.StreamToolStart {
		t.Errorf("type = %q", frames[0].Type)
	}
	if m.pending != 0 {
		t.Errorf("pending = %d, want 0", m.pending)
	}
}

func TestMapperToolEndStatusesAreDistinct(t *testing.T) {
	cases := []struct {
		name   string
		result agent.AgentToolResult
		want   wire.ToolStatus
	}{
		{
			name:   "success",
			result: agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "ok"}}},
			want:   wire.ToolSuccess,
		},
		{
			name:   "error",
			result: agent.AgentToolResult{IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "connection refused"}}},
			want:   wire.ToolError,
		},
		{
			// A refusal by the host policy is not a failure of the tool.
			// The console renders the two differently, so collapsing them
			// would tell an operator their read-only tool is broken.
			name:   "blocked",
			result: MarkBlocked(agent.AgentToolResult{}, "needs approval"),
			want:   wire.ToolBlocked,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestMapper()
			frames := m.Map(agent.ToolExecutionEndEvent{
				ToolCallID: "tc-a", ToolName: "restart_service", Result: tc.result, Duration: 1500 * time.Millisecond,
			})
			if len(frames) != 1 {
				t.Fatalf("produced %d frames, want 1", len(frames))
			}
			f := frames[0]
			if f.Type != wire.StreamToolEnd {
				t.Errorf("type = %q", f.Type)
			}
			if f.Tool.Status != tc.want {
				t.Errorf("status = %q, want %q", f.Tool.Status, tc.want)
			}
			if f.Tool.DurationMs != 1500 {
				t.Errorf("duration_ms = %d, want 1500", f.Tool.DurationMs)
			}
		})
	}
}

func TestMapperToolEndNegativeDurationIsClamped(t *testing.T) {
	m := newTestMapper()
	frames := m.Map(agent.ToolExecutionEndEvent{
		ToolCallID: "tc-a", ToolName: "t", Result: agent.AgentToolResult{}, Duration: -5 * time.Millisecond,
	})
	if frames[0].Tool.DurationMs != 0 {
		t.Errorf("duration_ms = %d, want 0: a negative duration is nonsense the console must not render", frames[0].Tool.DurationMs)
	}
}

func TestMapperToolEndBlockedErrorTextIsTheReason(t *testing.T) {
	m := newTestMapper()
	blocked := MarkBlocked(agent.AgentToolResult{Details: "host policy refused restart_service"}, "needs approval")
	frames := m.Map(agent.ToolExecutionEndEvent{ToolCallID: "tc-a", ToolName: "restart_service", Result: blocked})

	if frames[0].Tool.Status != wire.ToolBlocked {
		t.Errorf("status = %q", frames[0].Tool.Status)
	}
	if frames[0].Tool.Error != "host policy refused restart_service" {
		t.Errorf("error = %q, want the host-supplied reason", frames[0].Tool.Error)
	}
	if !contains(frames[0].Tool.ResultJSON, "needs approval") {
		t.Errorf("result_json = %q, want it to explain the refusal to the model", frames[0].Tool.ResultJSON)
	}
}

func TestMapperToolEndErrorTextIsTruncated(t *testing.T) {
	m := newTestMapper()
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'x'
	}
	frames := m.Map(agent.ToolExecutionEndEvent{
		ToolCallID: "tc-a", ToolName: "t",
		Result: agent.AgentToolResult{IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: string(long)}}},
	})
	// Counted in runes, not bytes: the ellipsis is three bytes but one
	// character, and the console renders characters.
	if got := utf8.RuneCountInString(frames[0].Tool.Error); got != 513 {
		t.Errorf("error rune length = %d, want 513 (512 plus an ellipsis) so a runaway tool cannot flood the console", got)
	}
}

func TestMapperToolUpdate(t *testing.T) {
	m := newTestMapper()
	frames := m.Map(agent.ToolExecutionUpdateEvent{ToolCallID: "tc-a", ToolName: "tail_file", PartialResult: agent.AgentToolResult{
		Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "line 1\n"}},
	}})
	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	if frames[0].Type != wire.StreamToolUpdate {
		t.Errorf("type = %q", frames[0].Type)
	}
	if frames[0].Tool.ResultJSON != "line 1\n" {
		t.Errorf("result_json = %q", frames[0].Tool.ResultJSON)
	}
}

// TestMapperToolUpdateJoinsTextBlocks pins the join rule v0.4.0 introduced:
// the update carries content blocks, and the wire field is one string. The
// same rule the terminal frame uses (AgentToolResult.Text) has to apply here,
// or a multi-block tool result would render differently mid-stream than it
// does once settled.
func TestMapperToolUpdateJoinsTextBlocks(t *testing.T) {
	m := newTestMapper()
	frames := m.Map(agent.ToolExecutionUpdateEvent{
		ToolCallID: "tc-a", ToolName: "tail_file",
		PartialResult: agent.AgentToolResult{Content: []ai.ToolResultMessageContent{
			ai.TextContent{Text: "line 1"},
			ai.ImageContent{Data: "AAAA", MimeType: "image/png"},
			ai.TextContent{Text: "line 2"},
		}},
	})
	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	if got := frames[0].Tool.ResultJSON; got != "line 1\nline 2" {
		t.Errorf("result_json = %q, want the text blocks joined and the image dropped", got)
	}
}

func TestMapperDoneCarriesUsageAndCounters(t *testing.T) {
	m := newTestMapper()
	m.TurnStarted()
	m.TurnStarted()
	m.Map(agent.MessageEndEvent{Message: assistantMessage("", 1)})
	m.Map(agent.ToolExecutionStartEvent{ToolCallID: "tc-a", ToolName: "get_topology"})
	m.SetUsage(ports.TranscriptUsage{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 800, CostUSD: 0.0123}, "test-model")

	frames := m.Map(agent.AgentEndEvent{})
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
		t.Fatal("usage missing: the console must not have to sum assistant frames for a cost")
	}
	if f.Done.Usage.InputTokens != 1200 || f.Done.Usage.OutputTokens != 340 || f.Done.Usage.CacheReadTokens != 800 {
		t.Errorf("usage = %+v", *f.Done.Usage)
	}
	if f.Done.Usage.Model != "test-model" {
		t.Errorf("model = %q, want test-model", f.Done.Usage.Model)
	}
}

func TestMapperIgnoresEventsWithNoConsoleCounterpart(t *testing.T) {
	m := newTestMapper()
	noFrames := []agent.AgentEvent{
		agent.AgentStartEvent{},
		agent.MessageStartEvent{},
		agent.AgentSettledEvent{},
	}
	for i, ev := range noFrames {
		if got := m.Map(ev); got != nil {
			t.Errorf("event %d (%T) produced %d frames, want none", i, ev, len(got))
		}
	}
	if m.seq != 0 {
		t.Errorf("seq = %d, want 0: an unmapped event must not consume a sequence number", m.seq)
	}
}

func TestMapperSeqIsMonotonicAcrossFrameTypes(t *testing.T) {
	m := newTestMapper()
	m.TurnStarted()
	var last int64
	emit := func(ev agent.AgentEvent) {
		for _, f := range m.Map(ev) {
			if f.Seq <= last {
				t.Fatalf("seq went %d -> %d on a %s frame", last, f.Seq, f.Type)
			}
			last = f.Seq
		}
	}
	emit(agent.TurnStartEvent{})
	emit(agent.MessageUpdateEvent{AssistantMessageEvent: ai.TextDeltaEvent{Delta: "a"}})
	emit(agent.MessageEndEvent{Message: assistantMessage("a", 1)})
	emit(agent.ToolExecutionStartEvent{ToolCallID: "tc-a", ToolName: "t"})
	emit(agent.ToolExecutionUpdateEvent{ToolCallID: "tc-a", ToolName: "t", PartialResult: agent.AgentToolResult{
		Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "x"}},
	}})
	emit(agent.ToolExecutionEndEvent{ToolCallID: "tc-a", ToolName: "t"})
	emit(agent.AgentEndEvent{})
	if last != 7 {
		t.Errorf("final seq = %d, want 7: a gap here would tell the console a frame was lost", last)
	}
}

func TestMapperAuxiliaryFrames(t *testing.T) {
	m := newTestMapper()

	errFrame := m.Error(CodeToolBlocked, "needs approval", false)
	if errFrame.Type != wire.StreamError || errFrame.Error.Code != CodeToolBlocked || errFrame.Error.Retryable {
		t.Errorf("error frame = %+v", errFrame)
	}

	pending := m.Approval(ApprovalProjection{
		RequestID: "r-1", Digest: "abc", Tool: "restart_service",
		Class: "write", Summary: "restart_service on web-1", BlastRadius: "pod", Target: "web-1",
	})
	if pending.Type != wire.StreamApprovalPending {
		t.Errorf("approval type = %q", pending.Type)
	}
	if pending.Approval.Digest != "abc" {
		t.Errorf("digest = %q: the console echoes this back and the host rejects a mismatch", pending.Approval.Digest)
	}
	if pending.Approval.BlastRadius != "pod" {
		t.Errorf("blast radius = %q", pending.Approval.BlastRadius)
	}

	resolved := m.ApprovalResolved("r-1", "denied", "change freeze")
	if resolved.Type != wire.StreamApprovalResolved || resolved.Approval.Decision != "denied" || resolved.Approval.Note != "change freeze" {
		t.Errorf("resolved frame = %+v", resolved)
	}

	note := m.Notification("w-1", "investigator", "completed", "found the cause")
	if note.Type != wire.StreamTaskNotification || note.Task.WorkerID != "w-1" || note.Task.Agent != "investigator" {
		t.Errorf("task frame = %+v", note)
	}
}

func TestMarkBlockedPrependsTheMarker(t *testing.T) {
	base := agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "original"}}}
	blocked := MarkBlocked(base, "approval denied")

	if !blocked.IsError {
		t.Error("a blocked result must still be an error upstream, or the loop would treat it as a usable result")
	}
	if !isBlocked(blocked) {
		t.Error("isBlocked did not recognise the marked result")
	}
	if !contains(blocked.Text(), "original") {
		t.Errorf("text = %q, want the original content preserved", blocked.Text())
	}

	// An unmarked error must not be mistaken for a policy refusal.
	if isBlocked(agent.AgentToolResult{IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "plain failure"}}}) {
		t.Error("a plain error was classified as blocked")
	}
}

func TestIsBlockedRequiresBothErrorAndMarker(t *testing.T) {
	// Marker without IsError: the loop would feed this back to the model as
	// a successful result, so it must not count as blocked.
	if isBlocked(agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: blockedMarker}}}) {
		t.Error("marker text alone was treated as blocked")
	}
	if isBlocked(agent.AgentToolResult{}) {
		t.Error("an empty result was treated as blocked")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestFramesFromTheRunStateAndTheMapperDoNotCollide is the regression test
// for a real data race, and it is written to fail without the race detector
// so it keeps failing on a build where -race is not enabled.
//
// The mapper has two callers. The event stream arrives on the agent's loop
// goroutine and takes the lock for the whole switch. The run state writes
// approval cards, error frames and worker tiles from a tool's own goroutine,
// while the loop is still emitting. Both increment the same sequence
// counter, because the console orders frames by it and a card that lands
// with the same number as the tool it belongs to renders in the wrong place
// with nothing to indicate why.
//
// The unsynchronised version passed every functional test in this package.
// Two goroutines doing a read-modify-write on one int64 do not reliably
// collide; they collide often enough to be found by `go test -race` and
// rarely enough that no assertion on a single turn would ever see it. So
// this test does what a functional test cannot: it puts the two callers in
// genuine contention, many times over, and requires the result to be a
// permutation of 1..n with no repeats and no holes.
func TestFramesFromTheRunStateAndTheMapperDoNotCollide(t *testing.T) {
	const rounds = 200
	const perRound = 64

	for round := 0; round < rounds; round++ {
		m := NewMapper(MapperOptions{SessionID: "s-1", Now: fixedClock()})
		var mu sync.Mutex
		var wg sync.WaitGroup
		seqs := make([]int64, 0, 3*perRound)

		// The event-stream caller, going through Map.
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int64, 0, perRound)
			for i := 0; i < perRound; i++ {
				m.TurnStarted()
				local = append(local, m.Map(agent.TurnStartEvent{})[0].Seq)
			}
			mu.Lock()
			seqs = append(seqs, local...)
			mu.Unlock()
		}()

		// The run-state caller, going straight to the frame builders.
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int64, 0, 2*perRound)
			for i := 0; i < perRound; i++ {
				local = append(local, m.Approval(ApprovalProjection{RequestID: "r"}).Seq)
				local = append(local, m.ApprovalResolved("r", "granted", "").Seq)
			}
			mu.Lock()
			seqs = append(seqs, local...)
			mu.Unlock()
		}()

		wg.Wait()

		if len(seqs) != 3*perRound {
			t.Fatalf("round %d: collected %d frames, want %d", round, len(seqs), 3*perRound)
		}
		seen := make(map[int64]bool, len(seqs))
		for _, s := range seqs {
			if s < 1 || s > int64(3*perRound) {
				t.Fatalf("round %d: sequence %d is outside 1..%d; the counter was read and written without synchronisation",
					round, s, 3*perRound)
			}
			if seen[s] {
				t.Fatalf("round %d: sequence %d was handed out twice; the console orders by it, so a duplicate renders out of place", round, s)
			}
			seen[s] = true
		}
	}
}

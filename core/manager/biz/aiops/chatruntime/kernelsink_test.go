package chatruntime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// recorderEmit keeps every console event a sink produced. It is mutex
// guarded because the sink emits from parallel tool goroutines.
type recorderEmit struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorderEmit) fn(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorderEmit) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func assistantEndFrame(sessionID, content string) ports.StreamEvent {
	return ports.StreamEvent{
		Type:      wire.StreamAssistantEnd,
		SessionID: sessionID,
		Iteration: 2,
		Assistant: &wire.AssistantFrame{Content: content, PendingToolCalls: 1,
			CreatedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)},
	}
}

func TestAssistantEndIsHeldUntilTheRowIdIsKnown(t *testing.T) {
	// The frame carries the persisted row id and the row is written one
	// step after the frame is produced. Shipping it early would make the
	// console key the bubble on a synthetic string that stops matching
	// the real id on the next history load.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)

	if err := s.Emit(context.Background(), assistantEndFrame("s-1", "the db is healthy")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("the frame escaped before the row id was known: %d events", len(got))
	}

	s.FlushAssistant("s-1", "msg-42")
	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Type != EventAssistant || got[0].Assistant == nil {
		t.Fatalf("event = %+v", got[0])
	}
	if got[0].Assistant.MessageID != "msg-42" {
		t.Fatalf("message id = %q, want the committed row id", got[0].Assistant.MessageID)
	}
	if got[0].Assistant.Content != "the db is healthy" {
		t.Fatalf("content = %q", got[0].Assistant.Content)
	}
	if got[0].Assistant.Iteration != 2 || got[0].Assistant.PendingToolCalls != 1 {
		t.Fatalf("iteration/pending = %d/%d", got[0].Assistant.Iteration, got[0].Assistant.PendingToolCalls)
	}
	if got[0].Assistant.CreatedAt.IsZero() {
		t.Fatal("created_at was dropped; the console sorts on it")
	}
}

func TestTheNextFrameFlushesTheHeldAssistant(t *testing.T) {
	// With persistence off nothing ever calls FlushAssistant, so the held
	// frame has to be released by the frame that follows it — otherwise a
	// text-only deployment would show no assistant bubble at all.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	ctx := context.Background()

	_ = s.Emit(ctx, assistantEndFrame("s-1", "answer"))
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamToolStart, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "query_promql"}})

	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	if got[0].Type != EventAssistant {
		t.Fatalf("first event = %v, want the assistant frame to precede the tool frame", got[0].Type)
	}
	if got[1].Type != EventToolStart {
		t.Fatalf("second event = %v", got[1].Type)
	}
}

func TestUnsupportedFramesProduceNoBubble(t *testing.T) {
	// assistant_start / delta / tool_update have no console rendering yet;
	// emitting them shows a phantom empty bubble or an unused duplicate
	// path.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	ctx := context.Background()

	for _, ev := range []ports.StreamEvent{
		{Type: wire.StreamAssistantStart, SessionID: "s-1", Assistant: &wire.AssistantFrame{}},
		{Type: wire.StreamAssistantDelta, SessionID: "s-1", Assistant: &wire.AssistantFrame{Content: "to"}},
		{Type: wire.StreamToolUpdate, SessionID: "s-1", Tool: &wire.ToolFrame{ToolCallID: "c1"}},
	} {
		if err := s.Emit(ctx, ev); err != nil {
			t.Fatalf("Emit(%s): %v", ev.Type, err)
		}
	}
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("events = %d, want 0: %+v", len(got), got)
	}
}

func TestDoneFrameIsDroppedButStillFlushes(t *testing.T) {
	// Handle emits its own Done with the full Reply; relaying the kernel
	// one would double-fire the terminal frame.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	ctx := context.Background()

	_ = s.Emit(ctx, assistantEndFrame("s-1", "answer"))
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamDone, SessionID: "s-1"})

	got := rec.all()
	if len(got) != 1 || got[0].Type != EventAssistant {
		t.Fatalf("events = %+v, want only the flushed assistant frame", got)
	}
}

func TestToolFramesKeepTheirTerminalDetail(t *testing.T) {
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	ctx := context.Background()
	started := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	ended := started.Add(250 * time.Millisecond)

	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamToolStart, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "host_bash", ArgsJSON: "{}",
			StartedAt: started.Format(time.RFC3339Nano)}})
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamToolEnd, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "host_bash", Status: wire.ToolBlocked,
			ResultJSON: "denied", Error: "no approval gate configured",
			EndedAt: ended.Format(time.RFC3339Nano), DurationMs: 250}})

	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	if got[0].Type != EventToolStart {
		t.Fatalf("first = %v", got[0].Type)
	}
	end := got[1]
	if end.Type != EventToolEnd || end.Tool == nil {
		t.Fatalf("second = %+v", end)
	}
	// The wire vocabulary distinguishes a policy refusal from a failure;
	// the console branches on it, so it must survive the translation.
	if end.Tool.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", end.Tool.Status)
	}
	if end.Tool.DurationMs != 250 {
		t.Fatalf("duration = %d", end.Tool.DurationMs)
	}
	if end.Tool.StartedAt.IsZero() || !end.Tool.StartedAt.Equal(started) {
		t.Fatalf("started_at = %v, want %v", end.Tool.StartedAt, started)
	}
	if end.Tool.EndedAt == nil || !end.Tool.EndedAt.Equal(ended) {
		t.Fatalf("ended_at = %v, want %v", end.Tool.EndedAt, ended)
	}
	if end.Tool.Error == "" {
		t.Fatal("the refusal reason was dropped; the console shows it to the operator")
	}
}

func TestToolStartHasNoEndedAt(t *testing.T) {
	// A nil end means the tile renders as running. A zero time would
	// render it as ended in 1970.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	_ = s.Emit(context.Background(), ports.StreamEvent{Type: wire.StreamToolStart, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "host_bash"}})
	got := rec.all()
	if len(got) != 1 || got[0].Tool == nil {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Tool.EndedAt != nil {
		t.Fatalf("ended_at = %v, want nil", got[0].Tool.EndedAt)
	}
}

func TestErrorFrameCarriesTheMessage(t *testing.T) {
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	_ = s.Emit(context.Background(), ports.StreamEvent{Type: wire.StreamError, SessionID: "s-1",
		Error: &wire.ErrorFrame{Code: "turn_timeout", Message: "the provider did not answer"}})
	got := rec.all()
	if len(got) != 1 || got[0].Type != EventError {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Error != "the provider did not answer" {
		t.Fatalf("error = %q", got[0].Error)
	}
}

func TestTaskNotificationIsForwardedAsATile(t *testing.T) {
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	_ = s.Emit(context.Background(), ports.StreamEvent{Type: wire.StreamTaskNotification, SessionID: "s-1",
		Task: &wire.TaskFrame{WorkerID: "w-1", Agent: "db-expert", Status: "completed", Summary: "all clear"}})
	got := rec.all()
	if len(got) != 1 || got[0].Type != EventTaskNotification {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Notification == nil || got[0].Notification.TaskID != "w-1" {
		t.Fatalf("notification = %+v", got[0].Notification)
	}
	if got[0].Notification.Status != WorkerStatusCompleted {
		t.Fatalf("status = %q", got[0].Notification.Status)
	}
}

func TestApprovalFramesAreNotRelayed(t *testing.T) {
	// The manager path surfaces approvals through the tool own propose
	// and confirm card. Relaying the kernel frame too would show two.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	_ = s.Emit(context.Background(), ports.StreamEvent{Type: wire.StreamApprovalPending, SessionID: "s-1",
		Approval: &wire.ApprovalFrame{RequestID: "r-1", Tool: "restart_service"}})
	_ = s.Emit(context.Background(), ports.StreamEvent{Type: wire.StreamApprovalResolved, SessionID: "s-1",
		Approval: &wire.ApprovalFrame{RequestID: "r-1", Decision: "grant"}})
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("events = %d, want 0", len(got))
	}
}

func TestASecondAssistantEndSupersedesTheFirst(t *testing.T) {
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	ctx := context.Background()
	_ = s.Emit(ctx, assistantEndFrame("s-1", "first"))
	_ = s.Emit(ctx, assistantEndFrame("s-1", "second"))
	s.FlushAssistant("s-1", "msg-1")

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Assistant.Content != "second" {
		t.Fatalf("content = %q, want the newest turn", got[0].Assistant.Content)
	}
}

func TestSessionsDoNotLeakAssistantFrames(t *testing.T) {
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	ctx := context.Background()
	_ = s.Emit(ctx, assistantEndFrame("coordinator", "dispatch"))
	_ = s.Emit(ctx, assistantEndFrame("worker", "probe"))
	s.FlushAssistant("worker", "msg-w")

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Assistant.Content != "probe" || got[0].Assistant.MessageID != "msg-w" {
		t.Fatalf("flushed the wrong session: %+v", got[0].Assistant)
	}
}

func TestFlushWithNothingHeldIsANoOp(t *testing.T) {
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	s.FlushAssistant("s-1", "msg-1")
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("events = %d, want 0", len(got))
	}
}

func TestSinkIsSafeUnderParallelToolFrames(t *testing.T) {
	// The kernel runs sibling tool calls in parallel, so two goroutines
	// reach Emit at once. Without the lock this loses frames and trips the
	// race detector.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Emit(context.Background(), ports.StreamEvent{Type: wire.StreamToolStart, SessionID: "s-1",
				Tool: &wire.ToolFrame{ToolCallID: "c", Name: "probe"}})
		}()
	}
	wg.Wait()
	if got := len(rec.all()); got != 32 {
		t.Fatalf("events = %d, want 32", got)
	}
}

func TestToolEndKeepsItsOwnStampWhenTheWireCarriesOne(t *testing.T) {
	// The join fills blanks only. A producer that decides to stamp the
	// settle frame itself is the better source, and overwriting it would
	// silently replace measured time with remembered time.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	ctx := context.Background()
	admitted := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	authoritative := admitted.Add(3 * time.Second)

	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamToolStart, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "host_bash", ArgsJSON: "{\"a\":1}",
			StartedAt: admitted.Format(time.RFC3339Nano)}})
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamToolEnd, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "host_bash", Status: wire.ToolSuccess,
			ArgsJSON: "{\"b\":2}", StartedAt: authoritative.Format(time.RFC3339Nano)}})

	got := rec.all()
	if len(got) != 2 || got[1].Tool == nil {
		t.Fatalf("events = %+v", got)
	}
	if !got[1].Tool.StartedAt.Equal(authoritative) {
		t.Fatalf("started_at = %v, want the settle frame's own %v", got[1].Tool.StartedAt, authoritative)
	}
	if got[1].Tool.ArgsJSON != "{\"b\":2}" {
		t.Fatalf("args_json = %q, want the settle frame's own", got[1].Tool.ArgsJSON)
	}
}

func TestToolEndWithoutAnAdmissionFrameInventsNothing(t *testing.T) {
	// A settle frame whose start frame was dropped (a reconnected console,
	// a truncated stream) must render as unknown rather than as 1970, and
	// must not panic on the missing entry.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	started := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	_ = s.Emit(context.Background(), ports.StreamEvent{Type: wire.StreamToolEnd, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "orphan", Name: "host_bash", Status: wire.ToolSuccess,
			EndedAt: started.Format(time.RFC3339Nano)}})

	got := rec.all()
	if len(got) != 1 || got[0].Tool == nil {
		t.Fatalf("events = %+v", got)
	}
	if !got[0].Tool.StartedAt.IsZero() {
		t.Fatalf("started_at = %v, want the zero time", got[0].Tool.StartedAt)
	}
}

func TestSettledAndFinishedSessionsDropTheirStartRecords(t *testing.T) {
	// The map would otherwise grow for the life of the process, one entry
	// per tool call whose settle frame never arrived, and the console
	// process serves every session.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	ctx := context.Background()

	started := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamToolStart, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "host_bash", StartedAt: started}})
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamToolEnd, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "host_bash", Status: wire.ToolSuccess}})
	if got := len(s.toolStarts); got != 0 {
		t.Fatalf("a settled call kept its record: %d sessions tracked", got)
	}
	// The record is gone, so replaying the settle frame must not resurrect
	// a start time the console would then render as a fresh 250ms tile.
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamToolEnd, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "host_bash", Status: wire.ToolSuccess}})

	// An abandoned call is dropped when the turn ends.
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamToolStart, SessionID: "s-2",
		Tool: &wire.ToolFrame{ToolCallID: "c2", Name: "host_bash", StartedAt: started}})
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamDone, SessionID: "s-2"})
	if got := len(s.toolStarts); got != 0 {
		t.Fatalf("a finished session kept its records: %d sessions tracked", got)
	}
}

func TestLastAssistantCarriesTheCommittedRowID(t *testing.T) {
	// The Reply the runtime hands back must name the row the turn was
	// committed under, because that is the bubble the console updates. The
	// snapshot therefore has to outlive the flush that drains the buffer:
	// reading the buffered frame after the drain returns nothing, and the
	// Reply would fall back to an id the next history load cannot match.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)

	_ = s.Emit(context.Background(), assistantEndFrame("s-1", "the db is healthy"))
	s.FlushAssistant("s-1", "msg-42")

	snap, ok := s.LastAssistant("s-1")
	if !ok {
		t.Fatal("the settled assistant frame was not readable after the flush")
	}
	if snap.MessageID != "msg-42" {
		t.Fatalf("message id = %q, want the committed row id", snap.MessageID)
	}
	if snap.Content != "the db is healthy" {
		t.Fatalf("content = %q", snap.Content)
	}
	if snap.CreatedAt.IsZero() {
		t.Fatal("created_at was dropped; the runtime stamps the Reply from it")
	}
}

func TestLastAssistantSaysNothingBeforeAnAssistantTurnSettles(t *testing.T) {
	// A turn that fails before the model answers has no assistant frame.
	// Reporting one anyway would make the runtime emit an empty bubble.
	s := newKernelSink(nil)
	if _, ok := s.LastAssistant("s-1"); ok {
		t.Fatal("a session with no assistant turn reported one")
	}
	_ = s.Emit(context.Background(), ports.StreamEvent{Type: wire.StreamToolStart, SessionID: "s-1",
		Tool: &wire.ToolFrame{ToolCallID: "c1", Name: "host_bash"}})
	if _, ok := s.LastAssistant("s-1"); ok {
		t.Fatal("a tool frame was mistaken for an assistant turn")
	}
}

func TestLastAssistantIsScopedToItsSession(t *testing.T) {
	// One sink serves the coordinator and every worker it spawns, so a
	// shared scalar would attribute a worker's answer to the coordinator
	// and the console would update the wrong bubble.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)

	_ = s.Emit(context.Background(), assistantEndFrame("s-1", "coordinator answer"))
	s.FlushAssistant("s-1", "msg-1")
	_ = s.Emit(context.Background(), assistantEndFrame("s-2", "worker answer"))
	s.FlushAssistant("s-2", "msg-2")

	first, _ := s.LastAssistant("s-1")
	second, _ := s.LastAssistant("s-2")
	if first.MessageID != "msg-1" || first.Content != "coordinator answer" {
		t.Fatalf("s-1 = %+v", first)
	}
	if second.MessageID != "msg-2" || second.Content != "worker answer" {
		t.Fatalf("s-2 = %+v", second)
	}
}

func TestLastAssistantKeepsTheFrameWhenNothingFlushedIt(t *testing.T) {
	// Persistence off means nothing ever calls FlushAssistant; the frame is
	// released by the terminal done frame instead. The snapshot has to
	// survive that path too, otherwise a transcript-less deployment shows
	// an answer with no id at all.
	rec := &recorderEmit{}
	s := newKernelSink(rec.fn)
	ctx := context.Background()

	_ = s.Emit(ctx, assistantEndFrame("s-1", "answer"))
	_ = s.Emit(ctx, ports.StreamEvent{Type: wire.StreamDone, SessionID: "s-1"})

	snap, ok := s.LastAssistant("s-1")
	if !ok {
		t.Fatal("the frame was dropped by the done-frame flush")
	}
	if snap.MessageID != "" {
		t.Fatalf("message id = %q, want empty when no row was written", snap.MessageID)
	}
	if snap.Content != "answer" {
		t.Fatalf("content = %q", snap.Content)
	}
}

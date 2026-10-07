package chatruntime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/alertdraft"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// The alert-draft rule has to hold on the kernel path, which is the only path
// left. It used to live in an eino callback that rewrote the assistant
// message before persistence; here it is asserted from both ends of the same
// turn — the frame the console renders and the content the Reply carries —
// because the console bubble and the terminal frame are read independently
// and a guard that reached only one of them is invisible.

// draftReply is a model turn that emits a fixed sequence of kernel frames and
// answers with content. The fake kernel has no loop, so the frames a real one
// would have produced are emitted here.
func draftReply(t *testing.T, content string, before func(ctx context.Context, sink ports.EventSink)) *scriptedKernel {
	t.Helper()
	k := newScriptedKernel(content)
	k.onRun = func(ctx context.Context, _ ports.AgentRequest) error {
		sink := ports.FromContext(ctx)
		if before != nil {
			before(ctx, sink)
		}
		if err := sink.Emit(ctx, ports.StreamEvent{
			Type:      wire.StreamAssistantEnd,
			SessionID: "s1",
			Assistant: &wire.AssistantFrame{Content: content, CreatedAt: "2026-01-01T00:00:00Z"},
		}); err != nil {
			return err
		}
		// The terminal frame is what releases the held assistant frame, the
		// same way a real turn's done frame does.
		return sink.Emit(ctx, ports.StreamEvent{Type: wire.StreamDone, SessionID: "s1"})
	}
	return k
}

const proseDraft = "规则 key: mysql_conn_high\n触发条件: conn_used > 80%\n草案哈希: sha256:abc\n需要确认后创建。"

func runDraftTurn(t *testing.T, kernel *scriptedKernel, userText string) ([]Event, *Reply) {
	t.Helper()
	store := newMemSessions(&model.Session{ID: "s1", UserID: 7})
	rt, err := NewRuntime(Config{Sessions: store, Kernel: kernel})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	var (
		mu     sync.Mutex
		events []Event
	)
	reply, err := rt.Handle(context.Background(), &Request{
		SessionID: "s1",
		UserID:    7,
		UserText:  userText,
		Emit: func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return events, reply
}

func assistantFrameOf(t *testing.T, events []Event) *AssistantEvent {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == EventAssistant && events[i].Assistant != nil {
			return events[i].Assistant
		}
	}
	t.Fatalf("no assistant frame in %+v", events)
	return nil
}

func TestAProseAlertDraftIsReplacedBeforeTheOperatorSeesIt(t *testing.T) {
	events, reply := runDraftTurn(t,
		draftReply(t, proseDraft, nil),
		"帮我创建一条 MySQL 连接数告警规则")

	frame := assistantFrameOf(t, events)
	if frame.Content != alertdraft.BlockedMessage {
		t.Fatalf("assistant frame = %q, want the blocked message", frame.Content)
	}
	if reply.Message == nil || reply.Message.Content == nil {
		t.Fatalf("reply message = %+v, want content", reply.Message)
	}
	if *reply.Message.Content != alertdraft.BlockedMessage {
		t.Fatalf("reply content = %q, want the blocked message", *reply.Message.Content)
	}
}

func TestARealDraftLetsTheModelDescribeIt(t *testing.T) {
	const result = `{"kind":"config_draft","draft_hash":"sha256:abc","payload":{"rule":{"kind":"metric_threshold"}}}`
	events, reply := runDraftTurn(t,
		draftReply(t, proseDraft, func(ctx context.Context, sink ports.EventSink) {
			_ = sink.Emit(ctx, ports.StreamEvent{
				Type:      wire.StreamToolEnd,
				SessionID: "s1",
				Tool: &wire.ToolFrame{
					ToolCallID: "call_draft",
					Name:       alertdraft.DraftConfigChangeToolName,
					ResultJSON: result,
					Status:     wire.ToolSuccess,
				},
			})
		}),
		"帮我创建一条 MySQL 连接数告警规则")

	frame := assistantFrameOf(t, events)
	if frame.Content != proseDraft {
		t.Fatalf("assistant frame = %q, want the real draft left alone", frame.Content)
	}
	if reply.Message == nil || reply.Message.Content == nil || *reply.Message.Content != proseDraft {
		t.Fatalf("reply content = %+v, want the real draft left alone", reply.Message)
	}
}

func TestATurnThatAskedForNoRuleIsUntouched(t *testing.T) {
	// The same words, asked as a question. A guard that answered this with
	// the blocked message would be refusing a question nobody asked twice.
	const answer = "这条规则是当连接占用超过 80% 持续 5 分钟时触发。"
	events, reply := runDraftTurn(t, draftReply(t, answer, nil), "解释一下这条告警规则为什么这么配")

	if got := assistantFrameOf(t, events).Content; got != answer {
		t.Fatalf("assistant frame = %q, want the answer left alone", got)
	}
	if reply.Message == nil || reply.Message.Content == nil || *reply.Message.Content != answer {
		t.Fatalf("reply content = %+v, want the answer left alone", reply.Message)
	}
}

// The guard must not fire on a kernel that failed: the failure path writes
// its own apology, and a sanitized apology would be a second, confusing
// message about drafts.
func TestAKernelFailureIsReportedAsItself(t *testing.T) {
	store := newMemSessions(&model.Session{ID: "s1", UserID: 7})
	kernel := newScriptedKernel()
	kernel.err = errors.New("provider unreachable")
	rt, err := NewRuntime(Config{Sessions: store, Kernel: kernel})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	var events []Event
	reply, err := rt.Handle(context.Background(), &Request{
		SessionID: "s1",
		UserID:    7,
		UserText:  "帮我创建一条 MySQL 连接数告警规则",
		Emit:      func(ev Event) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatalf("Handle returned an error instead of the apology: %v", err)
	}
	if reply.Message == nil || reply.Message.Content == nil {
		t.Fatalf("reply = %+v, want the apology message", reply.Message)
	}
	if *reply.Message.Content == alertdraft.BlockedMessage {
		t.Fatal("the failure apology was replaced by the draft block")
	}
}

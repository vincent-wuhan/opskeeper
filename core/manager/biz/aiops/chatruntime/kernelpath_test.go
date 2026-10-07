package chatruntime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigagent"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// scriptedKernel is a pigagent.Agent that answers with a canned result. It
// records the request so a test can prove the runtime handed it the turn the
// host composed — the history, the prompt, the selection — rather than
// letting the kernel re-derive any of it.
type scriptedKernel struct {
	mu       sync.Mutex
	requests []ports.AgentRequest
	// replies are returned in order, the last one repeating. It is the
	// scripted model currency turned into the kernel's: a test that used to
	// queue two schema.Messages now queues two strings.
	replies []string
	idx     int
	result  *pigagent.TurnResult
	err     error
	// onRun, when set, runs before the canned result is returned. The fake
	// has no loop, so a test that needs frames a loop would have produced
	// (a tool call, an approval) emits them here.
	onRun func(ctx context.Context, req ports.AgentRequest) error
}

// newScriptedKernel answers with the given contents in order, repeating the
// last. No replies = a turn that ends with empty content.
func newScriptedKernel(replies ...string) *scriptedKernel {
	return &scriptedKernel{replies: replies}
}

func (k *scriptedKernel) Run(ctx context.Context, req ports.AgentRequest) (*pigagent.TurnResult, error) {
	k.mu.Lock()
	k.requests = append(k.requests, req)
	k.mu.Unlock()
	if k.onRun != nil {
		if err := k.onRun(ctx, req); err != nil {
			return nil, err
		}
	}
	if k.err != nil {
		return nil, k.err
	}
	if k.result != nil {
		return k.result, nil
	}
	if len(k.replies) == 0 {
		return &pigagent.TurnResult{Stopped: pigagent.TurnEndTurn}, nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.replies[k.idx]
	if k.idx < len(k.replies)-1 {
		k.idx++
	}
	return &pigagent.TurnResult{Content: out, Stopped: pigagent.TurnEndTurn}, nil
}

func (k *scriptedKernel) Steer(context.Context, string, string) error { return nil }
func (k *scriptedKernel) Abort(context.Context, string) error         { return nil }
func (k *scriptedKernel) Spawn(context.Context, ports.AgentRequest) (string, error) {
	return "", nil
}
func (k *scriptedKernel) Notify(context.Context, string, string, string) error { return nil }

// runCount reports how many turns the fake was asked to run. It replaces the
// scripted model's generate-call counter: "the loop ran zero times" is the
// assertion that a deterministic path (a confirmed draft applied without the
// model) did not fall through to the kernel.
func (k *scriptedKernel) runCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.requests)
}

func (k *scriptedKernel) lastRequest(t *testing.T) ports.AgentRequest {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.requests) == 0 {
		t.Fatal("the kernel was never asked to run a turn")
	}
	return k.requests[len(k.requests)-1]
}

var _ pigagent.Agent = (*scriptedKernel)(nil)

// TestWithAKernelTheTurnRunsOnItAndNoChatModelIsRequired is the seam's whole
// point. ChatModel is deliberately nil: on this configuration the eino graph
// cannot be built at all, so a runtime that quietly fell through to it would
// fail here rather than in production.
func TestWithAKernelTheTurnRunsOnItAndNoChatModelIsRequired(t *testing.T) {
	sess := &model.Session{ID: "s1", UserID: 7}
	store := newMemSessions(sess)
	kernel := &scriptedKernel{result: &pigagent.TurnResult{
		Content:    "the replica lag is 4s",
		Iterations: 2,
		Usage:      ports.TranscriptUsage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3},
		Stopped:    pigagent.TurnEndTurn,
	}}

	rt, err := NewRuntime(Config{Sessions: store, Kernel: kernel})
	if err != nil {
		t.Fatalf("NewRuntime without a ChatModel but with a kernel: %v", err)
	}

	var (
		mu     sync.Mutex
		events []Event
	)
	emit := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	reply, err := rt.Handle(context.Background(), &Request{
		SessionID: "s1",
		UserID:    7,
		UserText:  "why is the db slow?",
		Provider:  "anthropic",
		Model:     "claude-sonnet-4-6",
		Locale:    "en",
		Emit:      emit,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply == nil || reply.Message == nil || reply.Message.Content == nil {
		t.Fatalf("reply = %+v, want an assistant message", reply)
	}
	if got := *reply.Message.Content; got != "the replica lag is 4s" {
		t.Fatalf("content = %q", got)
	}
	if reply.Iterations != 2 {
		t.Fatalf("iterations = %d, want the kernel's count", reply.Iterations)
	}
	// The runtime hands the ledger row through untouched: cache reads stay
	// a separate column rather than being folded into input here. The fold
	// into the console's single "prompt" number is the DTO layer's job
	// (server/aiops.usageDTOOf), because it is a display decision and
	// folding it here would make the stored row disagree with the column an
	// operator sums when reconciling an invoice.
	if reply.Usage.InputTokens != 10 || reply.Usage.OutputTokens != 5 || reply.Usage.CacheReadTokens != 3 {
		t.Fatalf("usage = %+v, want input 10 / output 5 / cache-read 3 kept apart", reply.Usage)
	}
	if reply.Usage.Total() != 18 {
		t.Fatalf("usage total = %d, want 18", reply.Usage.Total())
	}

	req := kernel.lastRequest(t)
	if req.SessionID != "s1" || req.UserID != 7 {
		t.Fatalf("kernel got session/user %q/%d", req.SessionID, req.UserID)
	}
	if req.UserText != "why is the db slow?" {
		t.Fatalf("user text = %q, want the live turn", req.UserText)
	}
	// The live turn is carried by UserText, and the history must NOT repeat
	// it: a transcript that contains the question twice makes the model
	// answer a conversation that never happened.
	if len(req.History) != 0 {
		t.Fatalf("history = %d entries, want the live turn trimmed out", len(req.History))
	}
	if req.SystemPrompt == "" {
		t.Fatal("no system prompt reached the kernel")
	}
	if string(req.Selection.Provider) != "anthropic" || req.Selection.Model != "claude-sonnet-4-6" {
		t.Fatalf("selection = %+v, want the request's picker choice", req.Selection)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 || events[len(events)-1].Type != EventDone {
		t.Fatalf("events = %+v, want a terminal done frame", events)
	}
	if events[len(events)-1].Done == nil {
		t.Fatal("the done frame carried no reply")
	}
}

// TestAKernelCapIsReportedAsAnApology mirrors the graph path: a turn that hit
// its cap is not reported as the partial answer it happened to produce. An
// operator cannot tell a half-finished exploration from a complete one, so
// the actionable message is the one that says the search did not converge.
func TestAKernelCapIsReportedAsAnApology(t *testing.T) {
	sess := &model.Session{ID: "s1", UserID: 7}
	store := newMemSessions(sess)
	kernel := &scriptedKernel{result: &pigagent.TurnResult{
		Content: "half an answer",
		Stopped: pigagent.TurnMaxIterations,
	}}
	rt, err := NewRuntime(Config{Sessions: store, Kernel: kernel})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	var (
		mu     sync.Mutex
		events []Event
	)
	emit := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	reply, err := rt.Handle(context.Background(), &Request{
		SessionID: "s1", UserID: 7, UserText: "dig deeper", Emit: emit,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply == nil || reply.Message == nil || reply.Message.Content == nil {
		t.Fatalf("reply = %+v, want an apology message", reply)
	}
	got := *reply.Message.Content
	if got == "half an answer" {
		t.Fatal("the partial answer was reported as the reply")
	}
	if !strings.Contains(got, "收敛") && !strings.Contains(got, "converge") && !strings.Contains(got, "工具调用") {
		t.Fatalf("apology = %q, want the non-convergence wording", got)
	}

	// The apology must be persisted: a reload has to show the same thing the
	// live stream did.
	store.mu.Lock()
	defer store.mu.Unlock()
	found := false
	for _, m := range store.messages {
		if m.Role == model.RoleAssistant && m.Content != nil && *m.Content == got {
			found = true
		}
	}
	if !found {
		t.Fatal("the apology was not persisted")
	}
}

// TestAKernelFailureIsReportedRatherThanReturned mirrors the graph path's
// behaviour: the console gets an explanation and the caller gets no error.
// Returning the error after emitting nothing leaves a stream that ended
// mid-turn, which a user reads as "the product is broken".
func TestAKernelFailureIsReportedRatherThanReturned(t *testing.T) {
	sess := &model.Session{ID: "s1", UserID: 7}
	store := newMemSessions(sess)
	kernel := &scriptedKernel{err: errors.New("provider said no")}
	rt, err := NewRuntime(Config{Sessions: store, Kernel: kernel})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	var (
		mu     sync.Mutex
		events []Event
	)
	emit := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	reply, err := rt.Handle(context.Background(), &Request{
		SessionID: "s1", UserID: 7, UserText: "check the db", Emit: emit,
	})
	if err != nil {
		t.Fatalf("Handle returned the kernel error instead of reporting it: %v", err)
	}
	if reply == nil || reply.Message == nil || reply.Message.Content == nil || *reply.Message.Content == "" {
		t.Fatalf("reply = %+v, want a stated failure", reply)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 {
		t.Fatalf("events = %+v, want an assistant frame and a done frame", events)
	}
}

// TestTheSinkRegistryFlushesOnlyTheLiveTurn covers the callback path that
// joins the assistant frame to its committed row id.
//
// The registry is keyed by session because the persister's row-id callback
// carries no context; a stale entry that outlived its turn would deliver a
// worker's row id to whichever session asked next and the console would
// update the wrong bubble.
func TestTheSinkRegistryFlushesOnlyTheLiveTurn(t *testing.T) {
	rec := &recorderEmit{}
	reg := &sinkRegistry{}
	live := newKernelSink(rec.fn)

	reg.add("s1", live)
	// The turn produced its terminal frame, which the sink holds until the
	// row id is known.
	_ = live.Emit(context.Background(), assistantEndFrame("s1", "the answer"))
	// A flush for a session with no live turn is a no-op, not a panic: the
	// row is written best-effort and can land after the turn returned.
	reg.flush("nobody", "msg-1")
	reg.flush("s1", "msg-9")

	got := rec.all()
	if len(got) != 1 || got[0].Assistant == nil || got[0].Assistant.MessageID != "msg-9" {
		t.Fatalf("events = %+v, want one frame carrying the committed id", got)
	}

	// Deregistering a turn that is no longer the live one must not remove
	// its successor's entry.
	successor := newKernelSink(rec.fn)
	reg.add("s1", successor)
	_ = successor.Emit(context.Background(), assistantEndFrame("s1", "the second answer"))
	reg.remove("s1", live)
	reg.flush("s1", "msg-10")

	got = rec.all()
	if len(got) != 2 || got[1].Assistant == nil || got[1].Assistant.MessageID != "msg-10" {
		t.Fatalf("events = %+v, want the successor still registered", got)
	}
	reg.remove("s1", successor)
	reg.flush("s1", "msg-11")
	if n := len(rec.all()); n != 2 {
		t.Fatalf("a removed turn still received a flush: %d events", n)
	}
}

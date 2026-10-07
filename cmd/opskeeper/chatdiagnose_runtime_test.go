package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	aiopschatruntime "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/chatruntime"
	managerbizchatdiagnose "github.com/vincent-wuhan/opskeeper/core/manager/biz/chatdiagnose"
	aiopsmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
)

// The adapter this file tests had no test, and it had no test for a reason
// worth recording: it held a concrete *chatruntime.Runtime, so there was no
// way to construct one without standing up the whole agent kernel — a session
// store, a kernel, a model registry and an LLM client.
//
// That is the same finding as decision 224's "a consumer that declares its own
// projection types leaves the port's return type unnegotiable and untestable",
// arriving from the other side. There the port was untestable; here the
// implementation was. Both are the cost of naming a concrete type where an
// interface would do, and both are invisible until someone tries to write the
// test that should have existed all along.
//
// What the test buys is not coverage for its own sake. The translation is
// where a field goes missing, and a field that goes missing does not fail —
// the diagnostic conversation runs, the operator gets a plausible answer, and
// the thing that stopped working is a root cause that never got cited.

// fakeChatRuntime records the request it was handed and answers with whatever
// the test put in reply.
type fakeChatRuntime struct {
	got   *aiopschatruntime.Request
	reply *aiopschatruntime.Reply
	err   error
}

func (f *fakeChatRuntime) Handle(_ context.Context, req *aiopschatruntime.Request) (*aiopschatruntime.Reply, error) {
	f.got = req
	return f.reply, f.err
}

func chatDiagnoseRequest() managerbizchatdiagnose.ChatRuntimeRequest {
	return managerbizchatdiagnose.ChatRuntimeRequest{
		ConversationID: "conv-7",
		UserID:         "42",
		Message:        "why is the pod flapping",
		ContextRefs:    []string{"alert:9", "device:node-a", "malformed-ref"},
	}
}

// The request direction, all four fields. The mapping is not one-to-one:
// ConversationID becomes SessionID and UserID is a string that has to become a
// uint64, so a copy-paste error here is silent in a way the field names do not
// suggest.
func TestTheRequestTranslationCarriesEveryField(t *testing.T) {
	fake := &fakeChatRuntime{reply: &aiopschatruntime.Reply{}}
	if _, err := (chatDiagnoseReAct{rt: fake}).ReAct(context.Background(), chatDiagnoseRequest()); err != nil {
		t.Fatalf("ReAct: %v", err)
	}
	if fake.got == nil {
		t.Fatal("the runtime was never called")
	}
	if fake.got.SessionID != "conv-7" {
		t.Errorf("SessionID = %q, want the ConversationID %q — a diagnostic conversation is "+
			"tied to its own row, not to a chat session the runtime would otherwise pick",
			fake.got.SessionID, "conv-7")
	}
	if fake.got.UserID != 42 {
		t.Errorf("UserID = %d, want 42 from the string %q", fake.got.UserID, "42")
	}
	if fake.got.UserText != "why is the pod flapping" {
		t.Errorf("UserText = %q", fake.got.UserText)
	}
	want := []aiopschatruntime.Mention{
		{Type: "alert", ID: "9", Label: "alert:9"},
		{Type: "device", ID: "node-a", Label: "device:node-a"},
		// A ref with no colon degrades to an empty type rather than being
		// dropped: the label still reaches the runtime, so the model can see
		// the raw text and ask about it. Dropping it would be a silent loss.
		{Type: "", ID: "", Label: "malformed-ref"},
	}
	if len(fake.got.Mentions) != len(want) {
		t.Fatalf("got %d mentions, want %d: %+v", len(fake.got.Mentions), len(want), fake.got.Mentions)
	}
	for i, w := range want {
		if fake.got.Mentions[i] != w {
			t.Errorf("mention %d = %+v, want %+v", i, fake.got.Mentions[i], w)
		}
	}
}

// An unparseable user id must not fail the turn. The session is attributed to
// nobody, which is what the runtime spells 0, and the diagnostic runs — the
// expensive part is the inference, not the ownership of its transcript.
func TestAnUnparseableUserIDBecomesSystemOwned(t *testing.T) {
	for _, id := range []string{"", "abc", "1.5", "-3"} {
		fake := &fakeChatRuntime{reply: &aiopschatruntime.Reply{}}
		req := chatDiagnoseRequest()
		req.UserID = id
		if _, err := (chatDiagnoseReAct{rt: fake}).ReAct(context.Background(), req); err != nil {
			t.Fatalf("UserID %q: ReAct: %v", id, err)
		}
		if fake.got.UserID != 0 {
			t.Errorf("UserID %q became %d, want 0", id, fake.got.UserID)
		}
	}
}

// The reply direction, and the reason the trace id is read from ctx rather
// than from the reply: the runtime does not propagate it, and a diagnostic
// that cannot be correlated with the agent's own trace is a diagnostic nobody
// can debug.
func TestTheReplyTranslationCarriesTextToolsAndTrace(t *testing.T) {
	content := "the node ran out of memory"
	fake := &fakeChatRuntime{reply: &aiopschatruntime.Reply{
		Message: &aiopsmodel.Message{Content: &content},
		ToolCalls: []*aiopsmodel.ToolCall{
			{ToolName: "k8s.describe_pod", ArgumentsJSON: `{"ns":"prod","pod":"api"}`, Status: "ok"},
			nil, // a hole in the slice must not become a zero-valued call
			{ToolName: "shell.dmesg", ArgumentsJSON: strings.Repeat("x", 200), Status: "ok"},
		},
		// Usage and Iterations are the runtime's, and this domain has no
		// column for either. Carrying them across would be carrying a
		// decision nobody here made.
		Iterations: 9,
	}}

	got, err := (chatDiagnoseReAct{rt: fake}).ReAct(context.Background(), chatDiagnoseRequest())
	if err != nil {
		t.Fatalf("ReAct: %v", err)
	}
	if got.Reply != content {
		t.Errorf("Reply = %q, want %q", got.Reply, content)
	}
	if len(got.ToolCalls) != 2 {
		t.Fatalf("got %d tool calls, want 2 (the nil entry dropped): %+v", len(got.ToolCalls), got.ToolCalls)
	}
	if got.ToolCalls[0].Name != "k8s.describe_pod" {
		t.Errorf("tool name = %q", got.ToolCalls[0].Name)
	}
	if got.ToolCalls[0].Status != "ok" {
		t.Errorf("tool status = %q — the status column is the only thing the SPA uses to "+
			"colour a failed call, and a dropped status renders every call as successful", got.ToolCalls[0].Status)
	}
	// Short arguments are passed through verbatim; the key=value digest is
	// only for arguments too long to put on a chip. Both branches matter —
	// rewriting the short case to always parse would change what the SPA
	// shows for every ordinary call.
	if got.ToolCalls[0].ArgsPreview != `{"ns":"prod","pod":"api"}` {
		t.Errorf("short args preview = %q, want it verbatim", got.ToolCalls[0].ArgsPreview)
	}
	if len(got.ToolCalls[1].ArgsPreview) > 83 {
		t.Errorf("long args preview is %d chars, want it truncated near 80", len(got.ToolCalls[1].ArgsPreview))
	}
	// No span in this context, so no trace id: the field has to stay empty
	// rather than pick up a zero-value span's id.
	if got.TraceID != "" {
		t.Errorf("TraceID = %q with no valid span in ctx", got.TraceID)
	}
}

// A nil reply is a valid answer — the runtime can hand one back with no
// error — and the old adapter turned it into an empty result rather than a
// failure. Keeping that matters because the service decides "not converged"
// from empty content, and an error here would instead read as a broken turn.
func TestANilReplyIsAnEmptyResultNotAnError(t *testing.T) {
	got, err := (chatDiagnoseReAct{rt: &fakeChatRuntime{reply: nil}}).ReAct(context.Background(), chatDiagnoseRequest())
	if err != nil {
		t.Fatalf("ReAct: %v", err)
	}
	if got == nil || got.Reply != "" {
		t.Fatalf("got %+v, want an empty result", got)
	}
}

func TestARuntimeErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("upstream refused")
	_, err := (chatDiagnoseReAct{rt: &fakeChatRuntime{err: sentinel}}).ReAct(context.Background(), chatDiagnoseRequest())
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v does not wrap the runtime's", err)
	}
	if !strings.Contains(err.Error(), "chatruntime handle") {
		t.Errorf("error %q does not say which call failed", err)
	}
}

// This is the test the move was for.
//
// The wiring in main.go is three lines where the old one was a line, and the
// extra two exist to stop a nil *chatruntime.Runtime from being stored in a
// non-nil interface — where it compares unequal to nil, walks past the guard
// in ReAct, and panics on the first diagnostic of a boot with no LLM
// configured. The compiler cannot see that, and neither can a test that
// constructs the adapter the convenient way.
//
// So the first assertion states the trap exists, and the second states the
// wiring does not fall into it. A future reader who "simplifies" the wiring
// back to one line gets a red test that names the boot it breaks.
func TestANilRuntimePointerIsNotANilInterface(t *testing.T) {
	var rt *aiopschatruntime.Runtime
	var iface chatDiagnoseRuntime = rt
	if iface == nil {
		t.Skip("this Go version compares a nil pointer in an interface to nil; the " +
			"wiring guard below is then unnecessary and this test has nothing to say")
	}
	t.Log("confirmed: a nil *chatruntime.Runtime stored in chatDiagnoseRuntime " +
		"compares unequal to nil, so ReAct's guard cannot be reached with a " +
		"one-line wiring")

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)
	needle := "chatDiagReAct := chatDiagnoseReAct{}"
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatal("main.go no longer builds chatDiagnoseReAct from a zero value. If the " +
			"assignment is unconditional, a nil runtime reaches ReAct as a non-nil " +
			"interface and panics on the first diagnostic of an LLM-less boot")
	}
	rest := body[i:]
	if !strings.Contains(rest[:strings.Index(rest, "chatDiagRuntime :=")], "if chatRT != nil {") {
		t.Error("the nil check on chatRT is gone from the chatDiagnoseReAct wiring")
	}
}

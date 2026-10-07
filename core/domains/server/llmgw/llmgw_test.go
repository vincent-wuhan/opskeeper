package llmgw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// stubAuth is the tunnel's authenticator, reduced to a map.
type stubAuth struct {
	edges map[string]uint64
	seen  []string
}

func (s *stubAuth) Authenticate(_ context.Context, accessKey, secretKey string) (tunnel.Session, error) {
	s.seen = append(s.seen, accessKey)
	id, ok := s.edges[accessKey+":"+secretKey]
	if !ok {
		return tunnel.Session{}, errs.ErrUnauthorized
	}
	return tunnel.Session{EdgeID: id}, nil
}

// stubCompleter records what it was asked and returns a fixed reply.
type stubCompleter struct {
	got   pigmodel.Request
	reply *pigai.AssistantMessage
	err   error
}

func (s *stubCompleter) Complete(_ context.Context, req pigmodel.Request) (*pigai.AssistantMessage, error) {
	s.got = req
	return s.reply, s.err
}

func assistantWithToolCall() *pigai.AssistantMessage {
	msg := pigai.AssistantMessage{}
	msg.Content = append(msg.Content, pigai.TextContent{Text: "let me look"})
	msg.Content = append(msg.Content, pigai.ToolCall{
		ID:   "call_abc123",
		Name: "host_dmesg",
		Arguments: pigai.JsonObject{
			"lines": json.Number("40"),
		},
	})
	return &msg
}

func newTestHandler(t *testing.T, auth EdgeAuthenticator, completer Completer) *Handler {
	t.Helper()
	handler, err := NewHandler(Options{Auth: auth, Completer: completer})
	if err != nil {
		t.Fatalf("build the handler: %v", err)
	}
	return handler
}

func post(t *testing.T, handler *Handler, credential, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	handler.Register(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// A gateway with no credential check is a key dispenser with a URL, and the
// mistake is easy because the handler is otherwise complete. This is the one
// construction that must not succeed.
func TestTheGatewayRefusesToBeBuiltWithoutACredentialCheck(t *testing.T) {
	if _, err := NewHandler(Options{Completer: &stubCompleter{}}); err == nil {
		t.Error("a gateway with no authenticator was built; it would serve model calls to anyone " +
			"who found the URL, spending the operator's credentials")
	}
	if _, err := NewHandler(Options{Auth: &stubAuth{}}); err == nil {
		t.Error("a gateway with no completer was built")
	}
}

// The node's credential is its existing tunnel pair, verified by the same
// function the tunnel dial uses. Every failure has to collapse to one answer:
// a gateway that distinguishes "no such node" from "wrong secret" is an
// oracle for enumerating the fleet.
func TestEveryCredentialFailureIsOneUnanswerableRefusal(t *testing.T) {
	auth := &stubAuth{edges: map[string]uint64{"ak-1:sk-good": 42}}
	handler := newTestHandler(t, auth, &stubCompleter{reply: &pigai.AssistantMessage{}})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	cases := []struct {
		name       string
		credential string
	}{
		{"no header at all", ""},
		{"not a bearer token", "Basic YWtrLTE6c2stZ29vZA=="},
		{"missing the secret half", "ak-1"},
		{"empty secret", "ak-1:"},
		{"unknown node", "ak-unknown:sk-whatever"},
		{"wrong secret", "ak-1:sk-wrong"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, handler, tc.credential, body)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status %d, want 401; the body is %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "authentication_error") {
				t.Errorf("the refusal is not in the shape a provider client parses: %s", rec.Body.String())
			}
		})
	}
}

// The tool-call id is the whole reason this translation is hand-written. An
// id that does not survive the round trip is not an error — it is a node whose
// model stops calling tools and starts answering in prose, which reads as a
// model problem and is a gateway problem.
func TestToolCallIdentitySurvivesTheRoundTripInBothDirections(t *testing.T) {
	completer := &stubCompleter{reply: assistantWithToolCall()}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)

	// In: an assistant turn that made a call, and the result answering it.
	body := `{"model":"m","messages":[
      {"role":"user","content":"why is the disk full"},
      {"role":"assistant","content":"","tool_calls":[
        {"id":"call_abc123","type":"function","function":{"name":"host_dmesg","arguments":"{\"lines\":40}"}}]},
      {"role":"tool","tool_call_id":"call_abc123","name":"host_dmesg","content":"no space left on device"}
    ]}`
	rec := post(t, handler, "ak:sk", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	// The transcript the model saw must still carry the id.
	if len(completer.got.Messages) != 3 {
		t.Fatalf("the transcript has %d messages, want 3", len(completer.got.Messages))
	}
	assistant, ok := completer.got.Messages[1].(pigai.AssistantMessage)
	if !ok {
		t.Fatalf("messages[1] is %T, want an assistant turn", completer.got.Messages[1])
	}
	calls := pigmodel.ReplyToolCalls(&assistant)
	if len(calls) != 1 || calls[0].ID != "call_abc123" {
		t.Fatalf("the transcript's tool call lost its id: %+v", calls)
	}
	if got := calls[0].Arguments["lines"]; got != json.Number("40") {
		t.Errorf("the tool call's arguments were re-encoded as %T(%v); a number that arrives as "+
			"a string makes a strict provider reject the whole turn", got, got)
	}
	result, ok := completer.got.Messages[2].(pigai.ToolResultMessage)
	if !ok || result.ToolCallID != "call_abc123" {
		t.Fatalf("the tool result lost its call id: %+v", completer.got.Messages[2])
	}

	// Out: the reply's tool call must carry the same id onto the wire, and
	// the client branches on finish_reason to decide to execute it.
	var response chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("the response is not a chat completion: %v", err)
	}
	if response.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason is %q; a client that reads \"stop\" here stops the loop and never "+
			"executes the call the model asked for", response.Choices[0].FinishReason)
	}
	out := response.Choices[0].Message.ToolCalls
	if len(out) != 1 || out[0].ID != "call_abc123" {
		t.Fatalf("the reply's tool call lost its id on the way out: %+v", out)
	}
	if out[0].Function.Name != "host_dmesg" {
		t.Errorf("the reply's tool call is named %q, want host_dmesg", out[0].Function.Name)
	}
	if !strings.Contains(out[0].Function.Arguments, "40") {
		t.Errorf("the reply's arguments are %q, which does not carry the value", out[0].Function.Arguments)
	}
}

// A tool result whose call was never made is refused here, where the message
// can name the index, rather than passed upstream to become a 400 that names
// nothing. This is the check that keeps an orphan out of a transcript.
func TestAToolResultWithNoMatchingCallIsRefusedAtTheEdge(t *testing.T) {
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)

	body := `{"model":"m","messages":[
      {"role":"user","content":"hi"},
      {"role":"tool","tool_call_id":"call_never_made","name":"host_dmesg","content":"output"}
    ]}`
	rec := post(t, handler, "ak:sk", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "call_never_made") {
		t.Errorf("the refusal does not name the orphan: %s", rec.Body.String())
	}
	if completer.got.Messages != nil {
		t.Error("the request reached the model despite failing validation")
	}
}

// A node names a model and never a provider. If it could name a provider it
// could spend a credential the operator never put in this cluster, which is
// the one thing the gateway exists to prevent.
func TestANodeCannotChooseWhichProviderPays(t *testing.T) {
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)

	body := `{"model":"some-model","messages":[{"role":"user","content":"hi"}]}`
	if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if completer.got.Selection.Provider != "" {
		t.Errorf("the request carried provider %q; a node must not be able to choose which "+
			"provider account the manager spends", completer.got.Selection.Provider)
	}
	if completer.got.Selection.Model != "some-model" {
		t.Errorf("the requested model was not carried through: %q", completer.got.Selection.Model)
	}
	if completer.got.SessionID != "" {
		t.Errorf("the request carried a provider-visible cache key %q supplied by the node; "+
			"providers key their cache on it and it is echoed on the wire", completer.got.SessionID)
	}
}

// Tool declarations have to survive with their schemas intact: a tool the
// model is offered with an empty parameter object is a tool it will call
// wrongly, and the wrong call reaches a host.
func TestToolDeclarationsKeepTheirSchemas(t *testing.T) {
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[
      {"type":"function","function":{"name":"host_dmesg","description":"read the ring buffer",
        "parameters":{"type":"object","properties":{"lines":{"type":"integer"}},"required":["lines"]}}},
      {"type":"function","function":{"name":"ping","description":"no arguments"}}
    ]}`
	if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(completer.got.Tools) != 2 {
		t.Fatalf("the model was offered %d tools, want 2", len(completer.got.Tools))
	}
	if completer.got.Tools[0].Name != "host_dmesg" {
		t.Errorf("tool[0] is %q", completer.got.Tools[0].Name)
	}
	props, ok := completer.got.Tools[0].Parameters["properties"].(map[string]any)
	if !ok || props["lines"] == nil {
		t.Errorf("tool[0] lost its parameter schema: %+v", completer.got.Tools[0].Parameters)
	}
	// A tool with no schema is told it takes no arguments, rather than being
	// offered nil, which says nothing at all.
	if completer.got.Tools[1].Parameters["type"] != "object" {
		t.Errorf("a no-argument tool was offered %+v; it must say it takes an object", completer.got.Tools[1].Parameters)
	}
}

// A streaming reply has to be a well-formed frame sequence ending in [DONE],
// and the tool calls have to arrive whole in the final frame — a client that
// is accumulating deltas has nowhere else to get an id from.
func TestAStreamingReplyIsAWellFormedFrameSequence(t *testing.T) {
	completer := &stubCompleter{reply: assistantWithToolCall()}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"why is the disk full"}]}`
	rec := post(t, handler, "ak:sk", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type is %q, want text/event-stream", ct)
	}

	raw := rec.Body.String()
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Errorf("the stream does not end with [DONE]; a client that waits for it hangs until "+
			"its own timeout:\n%q", raw)
	}

	var frames []chatChunk
	var sawRole, sawText bool
	for _, line := range strings.Split(raw, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a frame is not a chat.completion.chunk: %v\n%s", err, payload)
		}
		frames = append(frames, chunk)
		for _, choice := range chunk.Choices {
			if choice.Delta == nil {
				continue
			}
			if choice.Delta.Content == "" && choice.Delta.Role == roleAssistant {
				sawRole = true
			}
			if choice.Delta.Content != "" {
				sawText = true
			}
			for i, call := range choice.Delta.ToolCalls {
				// This assertion used to say the opposite -- that tool calls
				// are not streamed and arrive whole in the final frame --
				// and it was wrong, in a way the rest of the repository
				// could not catch. PiG's OpenAI client assembles a tool call
				// from `delta.tool_calls`, keyed by this index and the call
				// id, and never reads the final frame's `message` (see
				// ai/openai.go's stream loop). A gateway that put them only
				// there therefore answered every tool-using turn with a
				// well-formed stream the node read as silence.
				//
				// The index is checked as present rather than as a value,
				// because the field is a pointer upstream and `omitempty`
				// on a plain int would drop index 0 -- the first tool call,
				// and the most common one.
				if call.Index == nil {
					t.Errorf("a streamed tool call carried no index: %+v; the client keys "+
						"streamed fragments by it and cannot place one without it", call)
				} else if *call.Index != i {
					t.Errorf("tool call %d carries index %d; a client accumulating fragments "+
						"by index would attach this one to the wrong call", i, *call.Index)
				}
				if call.ID == "" || call.Function.Name == "" {
					t.Errorf("a streamed tool call is incomplete: %+v", call)
				}
			}
		}
	}
	if !sawRole {
		t.Error("the stream has no role frame; a client that accumulates a message from deltas " +
			"has nowhere to start")
	}
	if !sawText {
		t.Error("the stream has no content frame; the model's text never reached the caller")
	}
	last := frames[len(frames)-1]
	if last.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("the final frame's finish_reason is %q, want tool_calls", last.Choices[0].FinishReason)
	}
	final := last.Choices[0].Message.ToolCalls
	if len(final) != 1 || final[0].ID != "call_abc123" {
		t.Errorf("the final frame's tool calls are %+v; a streaming client gets the id from "+
			"nowhere else", final)
	}
}

// A provider that reports no usage gets usage:null, not zeros. A client that
// sees zeros has been told a number; one that sees null knows it was told
// nothing, and only one of those is safe to bill against.
func TestAbsentProviderUsageIsReportedAsAbsent(t *testing.T) {
	auth := &stubAuth{edges: map[string]uint64{"ak:sk": 7}}
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	rec := post(t, newTestHandler(t, auth, &stubCompleter{reply: &pigai.AssistantMessage{}}), "ak:sk", body)
	if !strings.Contains(rec.Body.String(), `"usage"`) && !strings.Contains(rec.Body.String(), `"choices"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	var response chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Usage != nil {
		t.Errorf("usage is %+v; a provider that reported nothing must not be reported as zero", response.Usage)
	}
}

// An upstream failure inside a stream cannot become a 500 — the status line
// is already written. It is reported as an OpenAI error object in the stream,
// because an empty choices array would read as "the model said nothing",
// which is the one reading a caller cannot tell from a real empty reply.
func TestAnUpstreamFailureBeforeTheFirstFrameIsAnHTTPFailure(t *testing.T) {
	completer := &stubCompleter{err: errors.New("provider is out of credit")}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, handler, "ak:sk", body)
	raw := rec.Body.String()
	if !strings.Contains(raw, `"error"`) || !strings.Contains(raw, "out of credit") {
		t.Errorf("the failure did not carry the error object shape:\n%s", raw)
	}
	// No frame was written, so there is no stream to terminate: [DONE] here
	// told a client a stream had started and finished, which is the reading
	// that turns a provider failure into a truncated answer. The status is
	// what a client acts on, and 200 was the wrong one (decision 356).
	if rec.Code == http.StatusOK {
		t.Errorf("a failure before the first frame was reported as 200:\n%s", raw)
	}
	if strings.Contains(raw, "[DONE]") {
		t.Errorf("a stream that never started was terminated by [DONE]:\n%s", raw)
	}
}

// A malformed request is refused before the model is touched, with an error
// in the shape a provider client parses.
func TestMalformedRequestsAreRefusedBeforeTheModelIsCalled(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json", `{`, "not chat completions"},
		{"no messages", `{"model":"m","messages":[]}`, "messages is empty"},
		{"unknown role", `{"model":"m","messages":[{"role":"wizard","content":"x"}]}`, "wizard"},
		{"tool result with no id", `{"model":"m","messages":[{"role":"tool","content":"x"}]}`, "tool_call_id"},
		{"a tool with no name", `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{}}]}`, "no name"},
		{"arguments that are not an object", `{"model":"m","messages":[
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"[1,2]"}}]}]}`, "not a JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
			handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)
			rec := post(t, handler, "ak:sk", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("the refusal does not mention %q: %s", tc.want, rec.Body.String())
			}
			if completer.got.Messages != nil {
				t.Error("the request reached the model despite failing validation")
			}
		})
	}
}

// An empty string of arguments is a call with no arguments, which models emit
// for a no-argument tool, and it decodes to an empty object rather than nil.
func TestEmptyToolArgumentsDecodeToAnEmptyObject(t *testing.T) {
	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)
	body := `{"model":"m","messages":[
      {"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ping","arguments":""}}]}]}`
	if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	assistant := completer.got.Messages[0].(pigai.AssistantMessage)
	calls := pigmodel.ReplyToolCalls(&assistant)
	if len(calls) != 1 {
		t.Fatalf("tool calls: %+v", calls)
	}
	if calls[0].Arguments == nil {
		t.Error("empty arguments decoded to nil; a provider validating \"arguments is required\" " +
			"is right to refuse nil, and \"this tool takes no arguments\" is a fact it needs told")
	}
}

// Numbers past 2^53 must survive verbatim.
//
// This is not a hypothetical: the tools a node's agent calls take
// nanosecond timestamps, byte counts on large volumes and nanosecond log
// offsets, and every one of them is an integer a tool compares against
// something. Decoded as float64 they come back rounded, the tool acts on a
// number nobody passed, and the result is a wrong answer rather than an
// error — the hardest class of bug to find during an incident.
func TestLargeIntegerArgumentsSurviveVerbatim(t *testing.T) {
	const timestamp = int64(1735689600123456789)

	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)

	body := `{"model":"m","messages":[
      {"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{
        "name":"loki_query","arguments":"{\"since\":1735689600123456789}"}}]}]}`
	if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	assistant := completer.got.Messages[0].(pigai.AssistantMessage)
	calls := pigmodel.ReplyToolCalls(&assistant)
	if len(calls) != 1 {
		t.Fatalf("tool calls: %+v", calls)
	}
	number, ok := calls[0].Arguments["since"].(json.Number)
	if !ok {
		t.Fatalf("the argument arrived as %T, not a number that can be re-encoded exactly",
			calls[0].Arguments["since"])
	}
	if got, err := number.Int64(); err != nil || got != timestamp {
		t.Errorf("the argument is %v, want %d", number, timestamp)
	}
	// The re-encoded form is what a provider will be sent, so it is the
	// string that has to be exact, not just the Go value.
	encoded, err := json.Marshal(calls[0].Arguments)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !strings.Contains(string(encoded), "1735689600123456789") {
		t.Errorf("re-encoding the arguments produced %s; the value a tool compares against "+
			"would be wrong", encoded)
	}
}

// The `content` field arrives in two shapes and which one is used is the
// client's choice, not a property of the role.
//
// This is pinned here rather than only in tests/agentgateway because the
// failure it guards is silent in the worst way: modelled as a plain string,
// this gateway rejected *every* request a real agent sent, and the unit tests
// did not notice because every one of them was written by the same hand that
// wrote the string. The shapes below are copied from a real `pig` on v0.3.0.
func TestTheContentFieldIsAcceptedInBothShapes(t *testing.T) {
	t.Run("system turns send a bare string", func(t *testing.T) {
		completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
		handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)
		body := `{"model":"m","messages":[
		  {"role":"system","content":"you are a node agent"},
		  {"role":"user","content":"why is the disk full"}]}`
		if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if len(completer.got.Messages) != 2 {
			t.Fatalf("messages: %d", len(completer.got.Messages))
		}
	})

	t.Run("user turns send the content-parts array", func(t *testing.T) {
		completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
		handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)
		body := `{"model":"m","messages":[
		  {"role":"user","content":[{"type":"text","text":"why is the disk full"}]}]}`
		if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		user, ok := completer.got.Messages[0].(pigai.UserMessage)
		if !ok {
			t.Fatalf("messages[0] is %T, want a user turn", completer.got.Messages[0])
		}
		if got := pigmodel.MessageText(user); got != "why is the disk full" {
			t.Errorf("the user turn reached the model as %q", got)
		}
	})

	t.Run("several text parts join", func(t *testing.T) {
		completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
		handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)
		body := `{"model":"m","messages":[
		  {"role":"user","content":[{"type":"text","text":"first "},{"type":"text","text":"second"}]}]}`
		if rec := post(t, handler, "ak:sk", body); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if got := pigmodel.MessageText(completer.got.Messages[0]); got != "first second" {
			t.Errorf("the parts joined as %q", got)
		}
	})

	// A part this gateway cannot carry is refused. Dropping it would leave
	// the model reasoning about something it was never shown, while the
	// transcript looks complete.
	t.Run("a non-text part is refused rather than dropped", func(t *testing.T) {
		completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
		handler := newTestHandler(t, &stubAuth{edges: map[string]uint64{"ak:sk": 7}}, completer)
		body := `{"model":"m","messages":[
		  {"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`
		rec := post(t, handler, "ak:sk", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "image_url") {
			t.Errorf("the refusal does not name the part it dropped: %s", rec.Body.String())
		}
		if completer.got.Messages != nil {
			t.Error("a transcript missing an image reached the model")
		}
	})
}

// The plan's security line is "a node's credential must not drive another
// node's inference", and it has been true here for a structural reason that
// no test names: chatRequest carries no node identity at all, so the only
// edge id in the system is the one the credential resolves to. That is worth
// a test, because the property is invisible in review and one field would
// undo it — an `edge_id` on the request, read the way `model` is read, would
// attribute a node's spend to somebody else and let one node exhaust another's
// budget without ever being wrong about a credential.
//
// So the test is behavioural rather than reflective: it spends node A's
// allowance down to nothing and then asks, with A's credential and a body
// that names B three different ways, whether B's inference was affected.
func TestANodesCredentialCannotDriveAnotherNodesInference(t *testing.T) {
	auth := &stubAuth{edges: map[string]uint64{"ak-a:sk": 11, "ak-b:sk": 22}}

	// One request per minute for node A: the second is over budget and the
	// first is not, which is the whole shape of the question.
	limiter := &countingLimiter{
		allow: map[uint64]int{11: 1, 22: 2},
		count: map[uint64]int{},
	}

	completer := &stubCompleter{reply: &pigai.AssistantMessage{}}
	handler, err := NewHandler(Options{Auth: auth, Completer: completer, Limiter: limiter})
	if err != nil {
		t.Fatalf("build the handler: %v", err)
	}

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if rec := post(t, handler, "ak-b:sk", body); rec.Code != http.StatusOK {
		t.Fatalf("node B's own request was refused: %d %s", rec.Code, rec.Body.String())
	}

	// Node B's id, carried by node A's credential, under every spelling a
	// caller might reach for. None of them is a field the request type has,
	// which is the point: they must all be inert rather than honoured.
	spoofed := `{"model":"m","edge_id":22,"node_id":22,"nodeId":22,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	if rec := postAt(t, handler, "ak-a:sk", "/v1/chat/completions?edge_id=22&node_id=22", spoofed); rec.Code != http.StatusOK {
		t.Fatalf("node A's own first request was refused: %d %s", rec.Code, rec.Body.String())
	}
	if len(limiter.saw) != 2 || limiter.saw[0] != 22 || limiter.saw[1] != 11 {
		t.Errorf("the limiter was consulted for %v, want [22 11]; one request spends exactly one "+
			"node's allowance, and it must be the one its credential belongs to", limiter.saw)
	}

	// Node A is now spent. Its second request is refused, and node B's second
	// one is not: a body able to name a node would have spent B's allowance
	// on A's first request and left B refused here, and A served past its own.
	if rec := postAt(t, handler, "ak-a:sk", "/v1/chat/completions?edge_id=22", spoofed); rec.Code != http.StatusTooManyRequests {
		t.Errorf("node A was served past its own limit: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(t, handler, "ak-b:sk", body); rec.Code != http.StatusOK {
		t.Errorf("node B was refused because node A spent its allowance: %d %s", rec.Code, rec.Body.String())
	}
}

// countingLimiter admits a fixed number of requests per node and records the
// order it was consulted in, so a test can see not only what was refused but
// whose budget paid for it.
type countingLimiter struct {
	allow map[uint64]int
	count map[uint64]int
	saw   []uint64
}

func (l *countingLimiter) Allow(_ context.Context, edgeID uint64) (bool, string) {
	l.saw = append(l.saw, edgeID)
	if l.count[edgeID] < l.allow[edgeID] {
		l.count[edgeID]++
		return true, ""
	}
	return false, "per-node request rate exceeded"
}

// postAt is post with a caller-chosen path, so a test can put an identity in
// the query string as well as in the body. Both are ways a future change
// would let one node speak for another, and a test that only exercises one of
// them is a test that catches half the bug.
func postAt(t *testing.T, handler *Handler, credential, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	handler.Register(router)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

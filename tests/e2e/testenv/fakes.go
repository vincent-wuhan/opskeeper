//go:build e2e

package testenv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/snappy"
)

// ─── Fake LLM ───────────────────────────────────────────────────────────
//
// The manager talks to LLM providers through a single HTTP base URL per
// provider, with OpenAI-compatible chat completions (Anthropic uses a
// different shape — we serve both off /v1/chat/completions and /v1/messages
// because the manager router only picks the right one). The fake doesn't
// implement anything real — it returns a fixed canned response so RCA /
// chat tests can assert "we got AN answer", not "we got THIS answer".
//
// Tests that need to assert a specific reply or token model can swap the
// canned response via SetLLMReply.

type FakeLLM struct {
	server *httptest.Server

	mu        sync.Mutex
	reply     string
	calls     int
	gotModels []string // model parameter from each request, in order

	// script is a queue of tool calls the fake asks for, one per turn,
	// consumed in order. Empty by default, which is why adding it changed
	// no existing test: a fake with no script answers text exactly as
	// before.
	script []LLMToolCall
	// gotTools records, per request, the tool names the caller advertised.
	// It is the evidence for "the agent was offered something to call" —
	// without it, a scripted tool call proves only that the fake can
	// produce JSON, not that the node's agent had a menu.
	gotTools [][]string
	// gotMessages records the message count per request, which is how a
	// test tells "the model was called again after the tool answered"
	// from "the model answered once and the tool never ran".
	gotMessages []int
	// gotToolResults records the content of every tool-role message, in
	// order. Message *counts* were enough for every assertion that existed
	// while nothing ever called a tool, but the one question a tool-using
	// turn raises is whether the result came back at all -- and a count
	// cannot tell "one more message" from "one more message, and it is the
	// answer to the call". Recording the content is what makes that
	// answerable, and it is deliberately not a parsed struct: a harness that
	// handed back a tidy object would be testing a convenience the wire does
	// not offer.
	gotToolResults []string
	// hold parks the next completion until the test releases it. See
	// HoldNextCall.
	hold *LLMHold
	// refusals records every request this fake rejected as one a real
	// provider would refuse. See Refusals.
	refusals []string
}

// invalidChatRequest returns why a chat request would be refused, or "".
//
// Each rule is one a provider enforces, and each exists because the failure it
// catches is invisible otherwise: an empty model and an empty conversation
// both produce a cheerful 200 from a permissive stub, and an agent that then
// "works" in tests and fails on the first real call.
func invalidChatRequest(model string, messages []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}, tools []struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}) string {
	if model == "" {
		return "model is required"
	}
	if len(messages) == 0 {
		return "messages must not be empty"
	}
	for i, message := range messages {
		if message.Role == "" {
			return fmt.Sprintf("messages[%d] has no role", i)
		}
	}
	for i, tool := range tools {
		if tool.Function.Name == "" {
			return fmt.Sprintf("tools[%d] has no function name", i)
		}
	}
	return ""
}

// Refusals returns the reasons this fake refused a request, in order.
//
// It exists so a test can assert the property that matters: not merely that
// the run passed, but that nothing the run sent was something a real provider
// would have rejected.
func (f *FakeLLM) Refusals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refusals...)
}

// refuse answers the way a provider would and remembers why.
//
// The body is OpenAI-shaped on purpose: a client that recovers from an error
// reads `error.message`, and a stub returning a bare string would let that
// recovery path go untested until the first real 400.
func (f *FakeLLM) refuse(w http.ResponseWriter, reason string) {
	f.mu.Lock()
	f.refusals = append(f.refusals, reason)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": reason,
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    nil,
		},
	})
}

// LLMHold parks one completion.
//
// It exists for a sequencing problem that is a property of the system
// rather than of the test: a node's autonomy only applies while the control
// plane is unreachable, so a tool call has to *arrive* after the node has
// noticed the outage. But the model call that produces the tool call is
// itself in flight, and the node reaches the model over its own HTTP path
// to the gateway rather than over the management tunnel — which is the real
// production shape, a node that lost its management link and still has
// outbound access to a provider.
//
// So the test starts the turn, parks the model mid-thought, cuts the link,
// waits out the offline threshold, and only then lets the model finish.
// Anything else either defers (the link is fine) or cannot happen at all
// (no tunnel, no turn).
type LLMHold struct {
	reached chan struct{}
	release chan struct{}
}

// HoldNextCall arms a hold on the next completion.
func (f *FakeLLM) HoldNextCall() *LLMHold {
	h := &LLMHold{reached: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	f.hold = h
	f.mu.Unlock()
	return h
}

// WaitReached blocks until the armed completion has actually parked.
func (h *LLMHold) WaitReached(t *testing.T) {
	t.Helper()
	select {
	case <-h.reached:
	case <-time.After(60 * time.Second):
		t.Fatal("the model was never called; the hold cannot be exercised")
	}
}

// Release lets the parked completion answer.
func (h *LLMHold) Release() { close(h.release) }

// LLMToolCall is one tool call the fake asks a model to make.
//
// Arguments is a JSON string because that is the shape the OpenAI wire
// carries for a streamed tool call's arguments: the client concatenates the
// fragments, and a harness that handed over a pre-parsed object would be
// testing a convenience the wire does not offer.
type LLMToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// NewFakeLLM starts an httptest.Server that speaks enough of the
// OpenAI/Anthropic completion shape to satisfy the manager's chatruntime.
func NewFakeLLM() *FakeLLM {
	f := &FakeLLM{
		reply: "PONG — fake LLM canned reply. Override with SetLLMReply for assertion tests.",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", f.openaiChat)
	mux.HandleFunc("/v1/messages", f.anthropicMessages)
	f.server = httptest.NewServer(mux)
	return f
}

// URL is the base URL to put in cfg.OpenAI.BaseURL / Anthropic.BaseURL.
func (f *FakeLLM) URL() string { return f.server.URL }

// Close tears down the fake server. Safe to call multiple times.
func (f *FakeLLM) Close() { f.server.Close() }

// SetLLMReply changes the canned assistant text for subsequent calls.
func (f *FakeLLM) SetLLMReply(s string) {
	f.mu.Lock()
	f.reply = s
	f.mu.Unlock()
}

// SetToolScript makes the fake ask for tool calls, one per turn, in order.
//
// It exists because the delivery acceptance is a conversation in which the
// agent *does* something, and a fake that can only answer text can only
// prove the pipe is open. The script is consumed turn by turn: the Nth
// request gets the Nth entry, and once the queue is empty the fake goes
// back to answering text. That shape is deliberate — it is what a real
// tool-using turn looks like, one call then one answer — and it means a
// test does not have to write a state machine to model a two-step turn.
func (f *FakeLLM) SetToolScript(calls ...LLMToolCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append([]LLMToolCall(nil), calls...)
}

// ToolsAdvertised returns the tool names each request carried, in order.
// ToolResults returns the content of every tool-role message this fake has
// been sent, in order.
//
// It is the accessor a tool-using turn needs and that a message count could
// never replace: the claim being made is "the agent ran something and its
// result came back", and only the content of the result carries that.
func (f *FakeLLM) ToolResults() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gotToolResults...)
}

func (f *FakeLLM) ToolsAdvertised() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.gotTools))
	copy(out, f.gotTools)
	return out
}

// MessagesPerRequest returns the message count of each request, in order.
func (f *FakeLLM) MessagesPerRequest() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.gotMessages))
	copy(out, f.gotMessages)
	return out
}

// CallCount returns how many completions have been served.
func (f *FakeLLM) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ModelsRequested returns the `model` parameter sent on each call, in
// order. Useful for asserting that a routing change really took effect.
func (f *FakeLLM) ModelsRequested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.gotModels))
	copy(out, f.gotModels)
	return out
}

func (f *FakeLLM) openaiChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	// The decode error used to be discarded, which made this fake accept a
	// body that no provider would serve: no model, no messages, a tool
	// without a function name, or something that is not JSON at all. A stub
	// that accepts everything proves the pipe is open and nothing about
	// whether our request would survive a real provider -- and the thing
	// that breaks is the translation, not the connection.
	//
	// So the rules below are the minimum a provider actually enforces. They
	// are deliberately not exhaustive: this fake is a gate on shape, not a
	// second implementation of OpenAI. Every refusal is recorded, so a test
	// that starts failing says which request was refused rather than just
	// that it was.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.refuse(w, fmt.Sprintf("request body is not a JSON object: %v", err))
		return
	}
	if reason := invalidChatRequest(req.Model, req.Messages, req.Tools); reason != "" {
		f.refuse(w, reason)
		return
	}

	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}

	f.mu.Lock()
	f.calls++
	f.gotModels = append(f.gotModels, req.Model)
	f.gotMessages = append(f.gotMessages, len(req.Messages))
	for _, message := range req.Messages {
		if message.Role == "tool" {
			f.gotToolResults = append(f.gotToolResults, message.Content)
		}
	}
	f.gotTools = append(f.gotTools, names)
	reply := f.reply
	var call *LLMToolCall
	if len(f.script) > 0 {
		head := f.script[0]
		f.script = f.script[1:]
		call = &head
	}
	hold := f.hold
	f.hold = nil
	f.mu.Unlock()

	// Park *after* the bookkeeping above, so a test waiting on the hold is
	// waiting on a request that has already been counted and whose reply is
	// fully decided. Releasing earlier would race the very accounting the
	// test is using to know the model was reached.
	if hold != nil {
		close(hold.reached)
		<-hold.release
	}

	message := map[string]any{"role": "assistant"}
	finish := "stop"
	if call != nil {
		// A tool call is content *and* a request: the assistant message
		// carries no text, and the finish reason is what tells the client
		// to go and run something. Getting finish_reason wrong here is
		// the classic way to produce a fake that looks like it works and
		// an agent that silently never calls a tool.
		message["content"] = nil
		message["tool_calls"] = []map[string]any{{
			"index": 0,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		}}
		finish = "tool_calls"
	} else {
		message["content"] = reply
	}

	resp := map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     42,
			"completion_tokens": 8,
			"total_tokens":      50,
		},
	}
	if req.Stream {
		writeOpenAIStream(w, req.Model, message, finish, resp["usage"])
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeOpenAIStream serves the same reply as an event stream.
//
// It exists because a fake that answers a `stream: true` request with a
// whole JSON body is a fake that no provider has ever been, and the failure
// it produces is the quietest kind: the client's SSE reader finds no `data:`
// lines, the turn settles with an empty message, and nothing anywhere
// reports an error. A delivery test that ran on that fake would have been
// asserting against a gateway that silently drops every reply, and it would
// have kept passing.
//
// The text is split into several deltas rather than sent whole, because the
// only way a test can tell a stream from a buffered replay is by seeing more
// than one frame arrive.
func writeOpenAIStream(
	w http.ResponseWriter, model string, message map[string]any,
	finish string, usage any,
) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func(payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, reason string, withUsage bool) map[string]any {
		out := map[string]any{
			"id":      "chatcmpl-fake",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": reason,
			}},
		}
		if withUsage {
			out["usage"] = usage
		}
		return out
	}

	send(chunk(map[string]any{"role": "assistant", "content": ""}, "", false))

	if calls, ok := message["tool_calls"].([]map[string]any); ok && len(calls) > 0 {
		// A tool call is a delta like any other, and the client only runs it
		// once it reads the finish reason.
		for i, call := range calls {
			fn, _ := call["function"].(map[string]any)
			piece := map[string]any{
				"index":    i,
				"id":       call["id"],
				"type":     "function",
				"function": map[string]any{"name": fn["name"], "arguments": fn["arguments"]},
			}
			send(chunk(map[string]any{"tool_calls": []map[string]any{piece}}, "", false))
		}
	} else if text, ok := message["content"].(string); ok && text != "" {
		for _, piece := range streamPieces(text) {
			send(chunk(map[string]any{"content": piece}, "", false))
		}
	}

	send(chunk(map[string]any{}, finish, true))
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// streamPieces cuts a reply into a few deltas at rune boundaries.
//
// A cut that split a multi-byte rune would produce a stream no real client
// could reassemble, and the failure would be blamed on the client.
func streamPieces(text string) []string {
	runes := []rune(text)
	const want = 4
	if len(runes) <= want {
		return []string{text}
	}
	size := (len(runes) + want - 1) / want
	var out []string
	for start := 0; start < len(runes); start += size {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
	}
	return out
}

func (f *FakeLLM) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.calls++
	f.gotModels = append(f.gotModels, req.Model)
	reply := f.reply
	f.mu.Unlock()
	resp := map[string]any{
		"id":          "msg_fake",
		"type":        "message",
		"role":        "assistant",
		"model":       req.Model,
		"content":     []map[string]any{{"type": "text", "text": reply}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 42, "output_tokens": 8},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ─── Fake Slack incoming webhook ───────────────────────────────────────
//
// Captures every POST so the test can assert payload shape (e.g. the
// attachments format from core/base/pkg/notify/webhook.go). The fake
// always returns 200 OK with body "ok", which is what real Slack does.

type FakeSlack struct {
	server *httptest.Server

	mu       sync.Mutex
	captures []SlackCapture
}

type SlackCapture struct {
	Path string
	// RawQuery is the URL-encoded query string of the request, captured
	// without modification so signing-via-URL providers (DingTalk:
	// ?timestamp=…&sign=…) can be asserted on. Empty for Slack/Feishu
	// proper (those sign in the JSON body), which is the existing G3
	// behaviour — additive only.
	RawQuery string
	Headers  http.Header
	Body     map[string]any // decoded JSON body
}

func NewFakeSlack() *FakeSlack {
	f := &FakeSlack{}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// URL is the host part. Tests usually append `/services/T.../B.../X` to
// get a webhook URL that looks like real Slack; the path is what Slack
// uses to identify the webhook, so it gets captured too.
func (f *FakeSlack) URL() string { return f.server.URL }

// WebhookURL returns the full URL with the conventional Slack path,
// suitable for storing in a notification_channels row.
func (f *FakeSlack) WebhookURL() string {
	return f.server.URL + "/services/T0FAKE/B0FAKE/abcdef"
}

func (f *FakeSlack) Close() { f.server.Close() }

// Captures returns a snapshot of every POST received so far, in order.
func (f *FakeSlack) Captures() []SlackCapture {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SlackCapture, len(f.captures))
	copy(out, f.captures)
	return out
}

func (f *FakeSlack) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.captures = append(f.captures, SlackCapture{
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
		Headers:  cloneHeader(r.Header),
		Body:     body,
	})
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ─── Fake Telegram Bot API ─────────────────────────────────────────────
//
// Serves /bot<TOKEN>/getUpdates + /bot<TOKEN>/sendMessage + /editMessageText.
// The test can inject inbound user messages via PushUpdate and they pop
// out the next getUpdates long-poll, so the bridge sees them just like
// real Telegram traffic. Outbound sendMessage / edits are captured.

type FakeTelegram struct {
	server *httptest.Server

	mu      sync.Mutex
	updates []map[string]any // queued inbound updates (FIFO)
	sent    []map[string]any // outbound sendMessage bodies
	edited  []map[string]any // outbound editMessageText bodies
	nextID  int
}

func NewFakeTelegram() *FakeTelegram {
	f := &FakeTelegram{nextID: 100}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *FakeTelegram) URL() string { return f.server.URL }
func (f *FakeTelegram) Close()      { f.server.Close() }

// PushUpdate queues a fake inbound message. text is the user's message;
// fromID is the Telegram numeric user id (must match allow_from). chatID
// defaults to fromID (DM) when zero.
func (f *FakeTelegram) PushUpdate(text string, fromID, chatID int64) {
	if chatID == 0 {
		chatID = fromID
	}
	f.mu.Lock()
	f.nextID++
	f.updates = append(f.updates, map[string]any{
		"update_id": f.nextID,
		"message": map[string]any{
			"message_id": f.nextID,
			"from":       map[string]any{"id": fromID, "first_name": "TestUser"},
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"text":       text,
		},
	})
	f.mu.Unlock()
}

// SentMessages returns a snapshot of outbound sendMessage payloads.
func (f *FakeTelegram) SentMessages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *FakeTelegram) handle(w http.ResponseWriter, r *http.Request) {
	// path = /bot<TOKEN>/<method>
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "bot") {
		http.NotFound(w, r)
		return
	}
	method := parts[1]
	switch method {
	case "getUpdates":
		f.mu.Lock()
		out := f.updates
		f.updates = nil
		f.mu.Unlock()
		writeOK(w, map[string]any{"ok": true, "result": out})
	case "sendMessage":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sent = append(f.sent, body)
		mid := f.nextID
		f.nextID++
		f.mu.Unlock()
		writeOK(w, map[string]any{"ok": true, "result": map[string]any{"message_id": mid}})
	case "editMessageText":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.edited = append(f.edited, body)
		f.mu.Unlock()
		writeOK(w, map[string]any{"ok": true, "result": true})
	default:
		writeOK(w, map[string]any{"ok": true, "result": map[string]any{}})
	}
}

// ─── Fake Prometheus query backend ─────────────────────────────────────
//
// Two endpoints with subtly different response shapes:
//   /api/v1/query        → resultType="vector", value=[ts, "v"]   (instant)
//   /api/v1/query_range  → resultType="matrix", values=[[ts,"v"]] (range)
//
// Alert evaluators use the instant variant (predicate baked into PromQL
// returns the firing rows). Range is wired for grafana-shaped panels.

type FakeProm struct {
	server *httptest.Server

	mu      sync.Mutex
	series  map[string][][2]any       // for query_range: query → samples
	instant map[string][]InstantEntry // for query: query → entries

	// written counts the label sets that arrived over remote_write, and
	// byDevice counts them per device. See remoteWrite for why the count
	// is taken from the label name rather than by decoding the payload.
	written  int
	byDevice map[uint64]int
	writes   int
}

// InstantEntry is one vector entry the FakeProm returns for /api/v1/query.
// Labels are emitted as the entry's "metric" map; Value is rendered as
// the Prom-canonical [unix_ts, "<float-as-string>"] pair.
type InstantEntry struct {
	Labels map[string]string
	Value  float64
}

func NewFakeProm() *FakeProm {
	f := &FakeProm{
		series:   map[string][][2]any{},
		instant:  map[string][]InstantEntry{},
		byDevice: map[uint64]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query_range", f.queryRange)
	mux.HandleFunc("/api/v1/query", f.queryInstant)
	mux.HandleFunc("/api/v1/write", f.remoteWrite)
	f.server = httptest.NewServer(mux)
	return f
}

func (f *FakeProm) URL() string { return f.server.URL }
func (f *FakeProm) Close()      { f.server.Close() }

// SetSeries lets a test inject a canned response for an exact query
// string on /api/v1/query_range. Unmatched queries return an empty
// matrix (success, no data).
func (f *FakeProm) SetSeries(query string, samples [][2]any) {
	f.mu.Lock()
	f.series[query] = samples
	f.mu.Unlock()
}

// SetInstant injects a canned vector response for /api/v1/query. Each
// entry maps to one "firing" series in the alert evaluator's view (the
// predicate is baked into the PromQL expression itself, so the very
// presence of an entry means "the rule fires for this label set").
//
//	SetInstant("up == 0", []InstantEntry{{Labels: nil, Value: 0}})
func (f *FakeProm) SetInstant(query string, entries []InstantEntry) {
	f.mu.Lock()
	f.instant[query] = entries
	f.mu.Unlock()
}

func (f *FakeProm) queryRange(w http.ResponseWriter, r *http.Request) {
	q := readQueryParam(r)
	f.mu.Lock()
	samples := f.series[q]
	f.mu.Unlock()
	resp := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "matrix",
			"result":     []any{},
		},
	}
	if len(samples) > 0 {
		resp["data"].(map[string]any)["result"] = []any{
			map[string]any{
				"metric": map[string]any{},
				"values": samples,
			},
		}
	}
	writeOK(w, resp)
}

func (f *FakeProm) queryInstant(w http.ResponseWriter, r *http.Request) {
	q := readQueryParam(r)
	f.mu.Lock()
	entries := f.instant[q]
	f.mu.Unlock()
	result := make([]any, 0, len(entries))
	ts := time.Now().Unix()
	for _, e := range entries {
		metric := map[string]any{}
		for k, v := range e.Labels {
			metric[k] = v
		}
		valStr := strconv.FormatFloat(e.Value, 'f', -1, 64)
		result = append(result, map[string]any{
			"metric": metric,
			"value":  []any{ts, valStr},
		})
	}
	writeOK(w, map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result":     result,
		},
	})
}

// readQueryParam returns the PromQL expression: Prom accepts it either
// on the query string (GET) or in form body (POST). The real client
// sends POST application/x-www-form-urlencoded, so we parse both.
// remoteWrite is the receive end of Prometheus remote_write.
//
// The harness previously served only the two query endpoints, so a node that
// pushed samples had nowhere to land: the manager's ingester POSTed to a URL
// that returned 404, the node's write-ahead log was told "nothing was
// accepted", and the log never drained. That is not a harmless gap — it means
// any test asserting on replayed telemetry was asserting on a node that could
// never finish replaying, and the failure would have been reported as a
// product defect.
//
// Counting rows is done by counting the device_id label rather than by
// decoding the protobuf. The manager's writer hand-rolls its protobuf
// encoding and this harness has no reason to take a dependency on a decoder
// to learn one number; the ingester attaches device_id to every single sample
// it forwards (see biz/promwrite), so one occurrence is exactly one series.
func (f *FakeProm) remoteWrite(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
		return
	}
	raw, err := snappy.Decode(nil, body)
	if err != nil {
		http.Error(w, "snappy: "+err.Error(), http.StatusBadRequest)
		return
	}
	rows := bytes.Count(raw, []byte("device_id"))
	devices := deviceIDsIn(raw)

	f.mu.Lock()
	f.writes++
	f.written += rows
	for _, id := range devices {
		f.byDevice[id]++
	}
	f.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// Written is how many labelled series have arrived over remote_write in
// total, how many of those carried each device id, and how many write
// requests it took.
func (f *FakeProm) Written() (total int, byDevice map[uint64]int, requests int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	perDevice := make(map[uint64]int, len(f.byDevice))
	for id, n := range f.byDevice {
		perDevice[id] = n
	}
	return f.written, perDevice, f.writes
}

// deviceIDsIn reads the numeric device_id label values out of a decoded
// remote_write body. It looks for the label name followed by its length
// prefix and decimal digits, which is enough to attribute a batch to a node
// without pulling in a protobuf decoder. Values it cannot read are simply not
// attributed; the total count above does not depend on this succeeding.
func deviceIDsIn(raw []byte) []uint64 {
	var out []uint64
	needle := []byte("device_id")
	for offset := 0; offset <= len(raw); {
		i := bytes.Index(raw[offset:], needle)
		if i < 0 {
			return out
		}
		at := offset + i + len(needle)
		if at < len(raw) && raw[at] < 0x20 {
			at++
			end := at
			var value uint64
			digits := 0
			for end < len(raw) && raw[end] >= '0' && raw[end] <= '9' && digits < 19 {
				value = value*10 + uint64(raw[end]-'0')
				end++
				digits++
			}
			if digits > 0 {
				out = append(out, value)
			}
		}
		offset = at
	}
	return out
}

func readQueryParam(r *http.Request) string {
	if v := r.URL.Query().Get("query"); v != "" {
		return v
	}
	_ = r.ParseForm()
	return r.Form.Get("query")
}

// ─── helpers ────────────────────────────────────────────────────────────

func writeOK(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

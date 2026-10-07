//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/tests/e2e/testenv"
)

// streamedText joins the assistant text out of an OpenAI-shaped SSE body.
//
// It parses rather than greps, and the reason is the assertion this file
// exists to make: a gateway that settles the provider reply with no content
// blocks writes a syntactically perfect stream containing nothing. Only a
// parse that asks "how many characters did the model actually say" can tell
// that apart from a real answer, and a substring search for a phrase the
// model was asked to produce cannot.
func streamedText(t *testing.T, body string) string {
	t.Helper()
	var text strings.Builder
	frames := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("a streamed frame is not JSON (%v): %s", err, payload)
		}
		frames++
		for _, choice := range frame.Choices {
			text.WriteString(choice.Delta.Content)
		}
	}
	if frames == 0 {
		t.Fatalf("the stream carried no data frames at all\nbody: %s", body)
	}
	return text.String()
}

// TestTheGatewayServesAStreamToARealModel is the one hop every delivery
// check has been substituting, run against something that is not a stub.
//
// Everything else in this suite proves the same thing about the same thing:
// that the delivery path carries a *fake* model's reply back to a node. That
// is worth having and it is not this. The fake accepts every request shape,
// so a translation that a real engine would reject — a malformed tool
// declaration, a content block typed as the wrong union member, a token
// count that overflows — passes every other test in this package and then
// fails on the first turn in production.
//
// So this test points the manager at a local inference engine (see
// testenv.RealLLMBaseURL for why loopback only) and asserts the two things a
// stub cannot fake: that the upstream was a real engine serving the model we
// asked for, and that the frames the gateway re-encoded carry real generated
// text rather than the fake's canned string.
//
// What a green run here does NOT establish is written in
// testenv.RealLLMLimits. Read it before quoting this test as evidence about
// a hosted provider.
func TestTheGatewayServesAStreamToARealModel(t *testing.T) {
	base, err := testenv.RealLLMBaseURL()
	if err != nil {
		t.Fatalf("real model: %v", err)
	}
	if base == "" {
		t.Skipf("SKIP: set %s to a loopback inference engine (ollama, llama.cpp, vLLM) "+
			"to run this; the default e2e run substitutes a fake model and says so", testenv.RealLLMEnv)
	}

	env := testenv.Start(t)
	login := env.LoginAdmin()
	_, access, secret := env.CreateEdge(t, login.AccessToken, "real-model-node")

	status, body, err := env.DoJSON("POST", "/v1/chat/completions", map[string]any{
		"model": testenv.RealLLMModel(),
		"messages": []map[string]any{
			{"role": "user", "content": "用一句话说明磁盘满了会发生什么。"},
		},
		"stream": true,
	}, access+":"+secret)
	if err != nil {
		t.Fatalf("real model stream: transport: %v", err)
	}
	if status != 200 {
		t.Fatalf("real model stream: status=%d body=%s\n%s",
			status, testenv.MustJSON(body), env.ManagerLogs())
	}

	stream, err := env.StreamBody("/v1/chat/completions", map[string]any{
		"model": testenv.RealLLMModel(),
		"messages": []map[string]any{
			{"role": "user", "content": "用一句话说明磁盘满了会发生什么。"},
		},
		"stream": true,
	}, access+":"+secret)
	if err != nil {
		t.Fatalf("real model stream body: transport: %v", err)
	}

	// The fake's canned string, verbatim. If it appears, the manager talked
	// to the stub despite being pointed at a real engine, and every claim
	// this test makes is false.
	if strings.Contains(stream, "PONG — fake LLM canned reply") {
		t.Fatalf("the manager answered from the fake model, not the real engine at %s\nbody: %s",
			base, stream)
	}
	// Real generated text, not merely a well-formed empty stream. A gateway
	// that settles the reply with no content blocks writes a valid stream
	// containing nothing, and a status check cannot tell those apart.
	text := streamedText(t, stream)
	if strings.TrimSpace(text) == "" {
		t.Fatalf("a real model answered but the stream carried no text\n%s\nbody: %s",
			testenv.RealLLMLimits, stream)
	}
	t.Logf("real inference: %d chars of generated text from %s", len([]rune(text)), base)
}

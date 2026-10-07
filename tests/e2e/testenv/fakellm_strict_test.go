//go:build e2e

package testenv

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The delivery e2e runs against this fake instead of a real provider, which is
// what makes it cheap enough to run on every push -- and also what makes it
// dangerous. A stub that accepts every request proves the pipe is open and
// says nothing about whether our request would survive a real provider's
// validation, and the part that breaks in production is the translation into
// that request, not the connection to it.
//
// So these tests pin the fake to refusing the shapes a provider refuses. If a
// change to the client makes it send one of these, the failure names the
// request instead of surfacing later as a 400 in someone's cluster.

func postChat(t *testing.T, fake *FakeLLM, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, fake.URL()+"/v1/chat/completions", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("posting to the fake: %v", err)
	}
	return response
}

func TestAWellFormedChatRequestIsServed(t *testing.T) {
	fake := NewFakeLLM()
	defer fake.Close()

	response := postChat(t, fake, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a well-formed request was refused with %d: %s", response.StatusCode, fake.Refusals())
	}
	if len(fake.Refusals()) != 0 {
		t.Fatalf("a served request was also recorded as refused: %v", fake.Refusals())
	}
}

func TestTheFakeRefusesWhatAProviderRefuses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json at all", `not json`, "not a JSON object"},
		{"a truncated body", `{"model":"m","messages":[`, "not a JSON object"},
		{"no model", `{"messages":[{"role":"user"}]}`, "model is required"},
		{"an empty model", `{"model":"","messages":[{"role":"user"}]}`, "model is required"},
		{"no messages", `{"model":"m","messages":[]}`, "messages must not be empty"},
		{"a message with no role", `{"model":"m","messages":[{"content":"hi"}]}`, "has no role"},
		{"a tool with no function", `{"model":"m","messages":[{"role":"user"}],"tools":[{"type":"function"}]}`, "has no function name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := NewFakeLLM()
			defer fake.Close()

			response := postChat(t, fake, tc.body)
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("the fake answered %d to a request a provider would refuse (%s)", response.StatusCode, tc.want)
			}
			var payload struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatalf("the refusal body is not JSON, so a client could not read it: %v", err)
			}
			if !strings.Contains(payload.Error.Message, tc.want) {
				t.Fatalf("refusal says %q, want it to mention %q", payload.Error.Message, tc.want)
			}
			if payload.Error.Type == "" {
				t.Fatal("the refusal carries no error.type, which is the field clients branch on")
			}
			refusals := fake.Refusals()
			if len(refusals) != 1 || !strings.Contains(refusals[0], tc.want) {
				t.Fatalf("the refusal was not recorded for a later assertion: %v", refusals)
			}
		})
	}
}

func TestARefusedRequestDoesNotCountAsAModelCall(t *testing.T) {
	// Otherwise a run can look like it exercised the model when nothing was
	// ever served -- the failure this fake previously could not produce,
	// because it never refused anything.
	fake := NewFakeLLM()
	defer fake.Close()

	bad := postChat(t, fake, `{"messages":[{"role":"user"}]}`)
	bad.Body.Close()
	good := postChat(t, fake, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	good.Body.Close()

	if got := fake.CallCount(); got != 1 {
		t.Fatalf("calls = %d, want 1: a refused request must not be counted as a served one", got)
	}
}

//go:build e2e

package testenv

import (
	"strings"
	"testing"
)

// TestTheRealLLMEndpointRefusesAnythingButLoopback guards the property the
// whole file rests on: this harness scrubs credential-shaped variables out of
// every child process, because one of the properties it must demonstrate is
// that a node's environment holds no cloud vendor key. Letting an operator
// point it at a hosted endpoint would break that promise in one of two ways —
// leak a key into the run, or start storing secrets in a file next to the
// tests — and neither is worth one extra assertion.
//
// These are cases rather than one test because the failure being prevented is
// silent: a run that quietly honoured a hosted URL would go on to report a
// green result about something it never tested.
func TestTheRealLLMEndpointRefusesAnythingButLoopback(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    string
		wantErr string
	}{
		{name: "unset keeps the fake", value: "", want: ""},
		{name: "whitespace is unset", value: "   ", want: ""},
		{name: "ipv4 loopback", value: "http://127.0.0.1:11434", want: "http://127.0.0.1:11434"},
		{name: "ipv6 loopback", value: "http://[::1]:11434", want: "http://[::1]:11434"},
		{name: "localhost by name", value: "http://localhost:11434", want: "http://localhost:11434"},
		{name: "trailing slash trimmed", value: "http://127.0.0.1:11434/", want: "http://127.0.0.1:11434"},
		{name: "a hosted provider", value: "https://api.openai.com/v1", wantErr: "not a loopback address"},
		{
			name:    "a loopback-looking name in a hosted host",
			value:   "https://localhost.evil.example/v1",
			wantErr: "not a loopback address",
		},
		{name: "a private lan address", value: "http://10.0.0.5:11434", wantErr: "not a loopback address"},
		// A malformed value fails in url.Parse before the loopback check ever
		// runs, so it is refused for a different reason -- and it still must
		// be refused, which is why it is a case here rather than an oversight.
		{name: "not a url", value: "://nope", wantErr: "is not a URL"},
		{name: "no host at all", value: "http://", wantErr: "not a loopback address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(RealLLMEnv, tc.value)
			got, err := RealLLMBaseURL()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("RealLLMBaseURL() = %q, want an error mentioning %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not say why; a refusal that does not explain "+
						"itself is a refusal the next operator argues with", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("RealLLMBaseURL() = error %v, want %q", err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("RealLLMBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheRealLLMModelDefaultsToSomethingSmall documents why the default is a
// small model: this path exists to prove the translation survives a real
// engine, and a default large enough to make the run slow is a default
// people stop running.
func TestTheRealLLMModelDefaultsToSomethingSmall(t *testing.T) {
	t.Setenv(RealLLMModelEnv, "")
	if got := RealLLMModel(); got != DefaultRealLLMModel {
		t.Fatalf("RealLLMModel() = %q, want the documented default %q", got, DefaultRealLLMModel)
	}
	t.Setenv(RealLLMModelEnv, "  qwen2.5:7b  ")
	if got := RealLLMModel(); got != "qwen2.5:7b" {
		t.Fatalf("RealLLMModel() = %q, want the operator's model trimmed of spaces", got)
	}
}

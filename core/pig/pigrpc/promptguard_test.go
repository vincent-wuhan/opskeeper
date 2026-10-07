package pigrpc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// collect subscribes to a client and returns a sink for the frames it emits.
//
// It goes through the public OnEvent rather than reading the relay's
// internals, because the property under test is the one the control plane
// depends on: a frame a console is subscribed to actually arrives.
func collect(t *testing.T, c *Client) *[]ports.ProcessEvent {
	t.Helper()
	var got []ports.ProcessEvent
	c.OnEvent(func(ev ports.ProcessEvent) { got = append(got, ev) })
	return &got
}

func TestSanitizePromptNeutralisesOnlyALeadingSlash(t *testing.T) {
	// The two columns are the whole contract. The left column is what a
	// control plane can legitimately forward; the right is what the agent
	// must receive. They differ only for the cases the agent's lexical
	// command test would otherwise capture.
	cases := []struct {
		name      string
		in        string
		want      string
		rewritten bool
	}{
		// Ordinary prose is untouched: the guard is not a filter, and a
		// turn that cannot possibly match must not be reported as one.
		{"ordinary prose", "why is checkout latency up", "why is checkout latency up", false},
		{"empty", "", "", false},
		{"leading whitespace is not a slash", "  /var/log filling", "  /var/log filling", false},

		// The collision set: each of these would have been read as a
		// command by PiG's catalog before the turn was ever admitted.
		{"bare command", "/compact", " /compact", true},
		{"command with args", "/restart api-gateway", " /restart api-gateway", true},
		{"skill expansion", "/skill:rca-loop", " /skill:rca-loop", true},
		{"unregistered name is still a slash", "/etc/nginx/nginx.conf is unreadable", " /etc/nginx/nginx.conf is unreadable", true},
		{"api path", "/api/v1/orders returns 502", " /api/v1/orders returns 502", true},
		{"lone slash", "/", " /", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rewritten := sanitizePrompt(tc.in)
			if got != tc.want {
				t.Errorf("sanitizePrompt(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if rewritten != tc.rewritten {
				t.Errorf("sanitizePrompt(%q) rewritten = %v, want %v", tc.in, rewritten, tc.rewritten)
			}
			// The operator's words must survive intact in every case. A
			// guard that edited the text would be trading a silent
			// misdispatch for a silent corruption, which is not a
			// better failure.
			if rewritten && strings.TrimPrefix(got, " ") != tc.in {
				t.Errorf("sanitizePrompt(%q) altered the text: %q", tc.in, got)
			}
		})
	}
}

func TestAnOrdinaryTurnEmitsNoNotice(t *testing.T) {
	// A console that renders every notice as a banner would drown in them
	// if the guard reported on turns it did not touch.
	c := New(Options{})
	got := collect(t, c)
	if sent := c.guardPrompt("prompt", "restart the stuck consumer"); sent != "restart the stuck consumer" {
		t.Errorf("guardPrompt sent %q, want the text unchanged", sent)
	}
	if len(*got) != 0 {
		t.Errorf("emitted %d frames for an untouched turn, want 0: %+v", len(*got), *got)
	}
}

func TestARewrittenTurnIsAnnouncedOnTheStream(t *testing.T) {
	// The notice is the difference between a guard and a silent rewrite.
	// An operator who pasted a log path deserves to learn the agent read
	// it as text rather than as a command, and an auditor needs the
	// original text in the recorded stream to tell a paste from an
	// attempt.
	c := New(Options{})
	got := collect(t, c)
	if sent := c.guardPrompt("prompt", "/compact the transcript"); sent != " /compact the transcript" {
		t.Errorf("guardPrompt sent %q, want the neutralised text", sent)
	}
	if len(*got) != 1 {
		t.Fatalf("emitted %d frames, want exactly 1: %+v", len(*got), *got)
	}
	ev := (*got)[0]
	if ev.Type != PromptRewriteNotice {
		t.Errorf("Type = %q, want %q", ev.Type, PromptRewriteNotice)
	}
	// A notice must not look like the end of a turn. A relay that read it
	// as terminal would close the conversation's bookkeeping while the
	// rewritten turn was still running.
	if ev.Terminal {
		t.Error("the notice is marked terminal: a relay would close the turn it belongs to")
	}

	var payload struct {
		Operation string `json:"operation"`
		Original  string `json:"original"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload is not decodable: %v", err)
	}
	if payload.Original != "/compact the transcript" {
		t.Errorf("Original = %q, want the operator's exact text", payload.Original)
	}
	if payload.Operation != "prompt" {
		t.Errorf("Operation = %q, want prompt", payload.Operation)
	}
	if payload.Reason == "" {
		t.Error("Reason is empty: a notice that does not say why is worse than no notice")
	}
}

func TestTheNoticeNamesTheOperationThatWasGuarded(t *testing.T) {
	// prompt and steer fail differently upstream — one is reinterpreted,
	// the other refused — so a notice that said only "a prompt was
	// rewritten" would misdescribe a steer.
	c := New(Options{})
	got := collect(t, c)
	c.guardPrompt("steer", "/status")
	if len(*got) != 1 {
		t.Fatalf("emitted %d frames, want 1", len(*got))
	}
	var payload struct {
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal((*got)[0].Payload, &payload); err != nil {
		t.Fatalf("payload is not decodable: %v", err)
	}
	if payload.Operation != "steer" {
		t.Errorf("Operation = %q, want steer", payload.Operation)
	}
}

func TestAnUnsubscribedClientStillRewrites(t *testing.T) {
	// The notice is a courtesy to whoever is watching, not a precondition
	// for correctness. A node with no console attached must still get a
	// turn that reaches the model.
	c := New(Options{})
	if sent := c.guardPrompt("prompt", "/deploy"); sent != " /deploy" {
		t.Errorf("guardPrompt sent %q, want the neutralised text", sent)
	}
}

package pigagent

import (
	"regexp"
	"strings"
)

// inlineThinkBlock matches the <think>…</think> run some providers fold into
// the assistant's text content rather than exposing as a separate reasoning
// channel. MiniMax's M-series is one: it answers with
// "<think>reasoning</think>\n\nanswer" in the same string.
//
// It matters because every other layer already treats reasoning as something
// the operator did not ask to read. The Mapper drops thinking *deltas* on
// purpose (see events.go) because leaking reasoning into a shared incident
// view is not the point of an AIOps console. A provider that inlines it
// defeats that by presenting it as ordinary text, and from there it lands in
// chat_messages, in the transcript the next turn replays, and in every
// generated report — where an operator would reasonably read it as the
// system's own conclusion.
var inlineThinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripInlineThinking removes inline reasoning runs and tidies the seam they
// leave behind.
//
// It is deliberately narrow: only complete <think>…</think> pairs are
// removed, so a partial stream that has emitted an opening tag but not yet
// its closing one is left alone rather than silently swallowing the rest of
// the answer. Leading blank lines introduced by the removal are trimmed.
//
// A message that was nothing but reasoning comes back EMPTY. It used to come
// back with the tags restored, on the theory that the turn should still
// "record that the model answered with no prose" — but that is what the tags
// are not: they are the prose. MiniMax-M3 spends whole turns thinking between
// tool calls ("<think>no such tool</think>" with no answer at all), and every
// one of those turns was therefore written to chat_messages verbatim and
// rendered as a bubble in the console, reasoning this package goes to some
// trouble to withhold everywhere else. Returning "" costs nothing that was
// worth keeping: toPortsMessage already drops an assistant turn that carries
// neither text nor a call, and an assistant turn that DOES carry calls is
// still persisted — with empty text — so tool-call pairing on replay is
// untouched. Verified in a live MiniMax-M3 session: 2 of 19 transcript rows
// were pure reasoning, and both rendered <think> tags in the browser.
func stripInlineThinking(s string) string {
	if !strings.Contains(s, "<think>") {
		return s
	}
	out := inlineThinkBlock.ReplaceAllString(s, "")
	out = strings.TrimLeft(out, "\n\r\t ")
	return out
}

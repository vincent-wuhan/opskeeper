package pigrpc

import (
	"encoding/json"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// PromptRewriteNotice is the event type OpsKeeper synthesises when a
// control-plane prompt would otherwise have been read by the agent as a
// command rather than as text.
//
// It is a real frame on the same stream the agent's own events arrive on,
// not a log line, because ports.ProcessEvent's contract is explicit that
// the vocabulary is open: unknown types are relayed and a frame the console
// cannot render is ignored. An operator whose turn was rewritten deserves
// to see it in the transcript where the turn happened, and an auditor
// deserves it in the stream that is already being recorded — not in a log
// file whose retention nobody agreed to.
const PromptRewriteNotice = "opskeeper_prompt_rewritten"

// sanitizePrompt keeps a control-plane prompt from being read as a command.
//
// # The hazard
//
// A node agent is a `pig --mode rpc` process, and its `prompt` command is
// not a way to submit text. Before admitting a turn, the agent asks its
// command catalog whether the message is actually a slash command
// (PiG cmd/pig/rpc_mode.go:634, resolved by extensionCommand at
// cmd/pig/rpc_mode.go:121-141). The test is purely lexical: a leading "/"
// followed by a first token that matches any loaded extension command's
// invocation name. A match executes the command and the turn never happens
// — the operator watches a command run instead of an investigation, and
// nothing in the response says the request was reinterpreted.
//
// A second, narrower gate sits behind it: ExpandSkillCommand
// (PiG internal/codingagent/skills.go:267-270) rewrites a leading
// "/skill:<name>" into that skill's body. Same failure mode, much smaller
// trigger surface.
//
// # Why this is reachable, not theoretical
//
// ports.AgentProcess.Prompt documents its argument as "a user turn", and
// the control plane honours that: the console's text travels unmodified
// from nodeagent.Service through nodefleet.Fleet to the agent
// (core/manager/biz/nodeagent/service.go:253). Nothing in that path is
// aware that the destination has a command dispatcher.
//
// The text is not only operator prose. Alerts, log excerpts and RCA context
// all begin with whatever the source system produced, and a leading slash
// is ordinary in operations data — a filesystem path, a URL, a label
// selector. Any of those, pasted or forwarded into a conversation, is one
// collision away from being executed as a plugin command.
//
// And the collision surface grows with the plugin ecosystem. Every command
// an extension registers is a name this can match, so the set of
// triggering prefixes is whatever third-party packages choose to ship.
// That is a phase-D concern leaking into phase C's data path, which is the
// wrong order to discover it in.
//
// # Why neutralise rather than refuse
//
// Refusing would be safer and wrong. The text is legitimate — "/var/log is
// filling up" is a real thing an operator types — and the failure this
// guards against is silence, not malice. Rejecting a valid message teaches
// operators to mangle their input, and the next thing they try is removing
// the slash, which destroys the evidence the investigation needed.
//
// Prefixing one space defeats both gates, because both test the very first
// byte. The model reads a line with a leading space, which no reasoning
// depends on, and the operator's actual words reach the agent intact.
//
// This is unconditional. It is not an option, because there is no
// legitimate OpsKeeper turn that is a slash command: the control plane has
// no feature that speaks the agent's command language, so a leading slash
// reaching this layer is by construction text that was not meant as a
// command. An option to disable the guard would only be a way to turn the
// hazard back on.
func sanitizePrompt(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return text, false
	}
	return " " + text, true
}

// guardPrompt sanitises a prompt and, when it had to, tells the stream.
//
// The rewrite is announced before the turn is submitted rather than after,
// so the notice is ordered ahead of the events the rewritten turn produces.
// A console that renders the transcript in order then shows the operator
// what happened before it shows the investigation that followed, which is
// the order that makes the notice explainable.
func (c *Client) guardPrompt(operation, text string) string {
	guarded, rewritten := sanitizePrompt(text)
	if !rewritten {
		return text
	}
	c.announceRewrite(operation, text)
	return guarded
}

// announceRewrite emits the notice frame to the current subscribers.
//
// The original text is carried in the payload rather than only described,
// because the operator's own words are the only way to tell a harmless
// paste from a third-party plugin attempting to capture a turn. A summary
// would describe the attempt; the text is the evidence.
func (c *Client) announceRewrite(operation, original string) {
	payload, err := json.Marshal(struct {
		Operation string `json:"operation"`
		Original  string `json:"original"`
		Reason    string `json:"reason"`
	}{
		Operation: operation,
		Original:  original,
		Reason:    "leading '/' would have been read as an agent command; neutralised so the text reaches the model as a turn",
	})
	if err != nil {
		// A struct of three strings cannot fail to marshal. If it somehow
		// does, the turn is still more important than the notice: send it.
		return
	}
	ev := ports.ProcessEvent{Type: PromptRewriteNotice, Payload: payload}
	c.mu.Lock()
	subs := make([]func(ports.ProcessEvent), 0, len(c.listeners))
	for _, fn := range c.listeners {
		subs = append(subs, fn)
	}
	c.mu.Unlock()
	for _, fn := range subs {
		fn(ev)
	}
}

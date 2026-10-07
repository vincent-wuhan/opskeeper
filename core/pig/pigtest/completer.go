// Package pigtest provides programmable stand-ins for the PiG model
// surface.
//
// It exists because a Completer is one method with a PiG-shaped argument,
// which makes it trivial to fake in a test — and trivially easy to fake
// badly. The fakes this package replaces returned a translated response
// struct, so a test could assert on lastUserPrompt but never on the
// transcript the model would actually have seen. A conversion bug in the
// fake and a conversion bug in production look identical from the test,
// which is why those fakes passed for as long as they did.
//
// A stub here records the Request the caller built. That is the whole
// contract: it is PiG's own shape, so a test asserts on ai.Message values
// and on the tool arguments the provider would actually have received.
package pigtest

import (
	"context"
	"sync"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// Reply is one programmed answer.
//
// Text and ToolCalls are compatible rather than exclusive: an agent step
// that says one sentence and then calls two tools is a normal turn, and a
// stub that made the caller choose would push that shape into production code
// as a special case.
type Reply struct {
	Text      string
	ToolCalls []ai.ToolCall
	// Usage is stamped onto the settled message. Leave it zero for a
	// provider that reported nothing.
	Usage ai.Usage
	// Err fails the call. A non-nil Err wins over Text and ToolCalls.
	Err error
}

// Completer answers from a fixed script.
//
// Replays are per call, not per session: a stub that returned its first
// reply forever could not exercise a multi-turn agent, and the multi-turn
// path is where transcripts break.
type Completer struct {
	mu      sync.Mutex
	replies []Reply
	// fallback answers once the script is exhausted. Its zero value
	// succeeds with empty text, which is the right default for a test that
	// cares about one call.
	fallback Reply
	calls    int
	last     pigmodel.Request
	lastOK   *ai.AssistantMessage
}

// NewCompleter returns a stub that answers with the given replies in order.
func NewCompleter(replies ...Reply) *Completer {
	return &Completer{replies: append([]Reply(nil), replies...)}
}

// NewFailingCompleter returns a stub whose every call fails with err.
func NewFailingCompleter(err error) *Completer {
	return &Completer{fallback: Reply{Err: err}}
}

// Complete implements pigmodel.Completer.
func (c *Completer) Complete(_ context.Context, req pigmodel.Request) (*ai.AssistantMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.last = req
	idx := c.calls
	c.calls++

	reply := c.fallback
	if idx < len(c.replies) {
		reply = c.replies[idx]
	}
	if reply.Err != nil {
		return nil, reply.Err
	}

	msg := pigmodel.AssistantTurn(reply.Text, reply.ToolCalls...)
	msg.Usage = reply.Usage
	msg.StopReason = ai.StopReasonStop
	if len(reply.ToolCalls) > 0 {
		msg.StopReason = ai.StopReasonToolUse
	}
	c.lastOK = &msg
	return &msg, nil
}

// Calls reports how many times the stub was asked.
func (c *Completer) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// LastRequest returns the most recent request, for assertions on the
// transcript, the tool bag and the model selection the caller built.
func (c *Completer) LastRequest() pigmodel.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// LastSelection returns the model selection of the most recent request.
func (c *Completer) LastSelection() domain.ModelSelection {
	return c.LastRequest().Selection
}

// LastReply returns the settled message of the most recent successful call,
// or nil when the last call failed.
func (c *Completer) LastReply() *ai.AssistantMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastOK
}

// SetFallback replaces the reply used once the script is exhausted.
func (c *Completer) SetFallback(r Reply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fallback = r
}

// Compile-time proof the stub satisfies the port every host injects.
var _ pigmodel.Completer = (*Completer)(nil)

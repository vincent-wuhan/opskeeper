package ports

import (
	"context"
	"encoding/json"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// ToolCallRecord is one tool call observed crossing the kernel dispatch
// point: once when it is admitted, once when it settles.
//
// It exists because the console tool table is not the audit ledger. The
// ledger records that a call happened and how it was classified; the table
// records the call identity, its arguments, its output, and how long it
// took — the four things an operator reads when asking what the agent
// actually ran on that host. Folding one into the other would either put
// unbounded result bodies into the ledger or lose the ledger
// classification, and the ledger whole value is that it is small enough to
// verify and complete enough to trust.
//
// The vocabulary is deliberately the kernel own, not any storage schema:
// an implementation decides what a row looks like, and a kernel that had to
// know would have to know one host table layout.
type ToolCallRecord struct {
	// SessionID scopes the record to one conversation. It is required:
	// the row a call belongs to is keyed by the assistant turn that
	// requested it, and that turn lives in one session. A recorder that
	// had to infer the session from the call id would have to keep a
	// global index of every in-flight call, which is exactly the state
	// that goes stale when a process restarts mid-turn.
	SessionID string
	// ID is the provider-assigned call id. It is the only key that pairs a
	// start with its own settle when several calls are in flight, which is
	// exactly the situation a parallel tool batch creates.
	ID string
	// Name is the tool wire name.
	Name string
	// Class is the blast-radius classification the host resolved from its
	// own tool bag. A recorder must not re-derive it from the name.
	Class domain.ToolClass
	// Args is the exact JSON the model produced.
	Args json.RawMessage
	// Status is set on the settle only. It uses the wire.ToolStatus
	// vocabulary so one console vocabulary covers every producer; an empty
	// status on a settle means the call ran and succeeded.
	Status string
	// Result is the settled call output text, truncated by the recorder
	// if the store has a bound. Empty on start.
	Result string
	// Err is the settled call failure text, empty when none. It is
	// separate from Result because a tool may return both — a partial
	// output and the error that stopped it.
	Err string
	// Duration is the settle measured wall clock, zero on start.
	Duration time.Duration
}

// ToolCallRecorder observes tool call lifecycles for a host that keeps a
// per-call record.
//
// Two properties are load-bearing:
//
//  1. It is advisory. A recorder that fails must not fail the turn: the
//     call already ran, and refusing its result because a row could not be
//     written would turn a storage hiccup into a failed investigation. An
//     implementation reports its own failures through its own metrics.
//  2. It sees every call, including the ones the gate refused. A refused
//     call never executes, so nothing else in the system observes it — and
//     the agent tried to restart that host and was refused is precisely
//     the event an incident review is looking for.
type ToolCallRecorder interface {
	// Started is called when the kernel admits a call, before any hook
	// decides whether it may run.
	Started(ctx context.Context, rec ToolCallRecord) error
	// Settled is called exactly once per admitted call: on completion, on
	// failure, on refusal, and on cancellation.
	Settled(ctx context.Context, rec ToolCallRecord) error
}

// The turn record: what a host stores, what a kernel replays.
//
// This file is the *persistence* half of the agent contract and nothing
// else. It holds no model vocabulary. OpsKeeper used to also declare
// ports.LLMRequest, ports.LLMResponse and ports.Conversation here — a second
// set of message, request and usage types sitting one package away from
// PiG's own. Every call went through a translation between them, and a
// translation that drops a tool_call id does not fail: the model simply
// stops calling tools, and the cause is a line of code that looked fine.
// Those types are gone. A caller that wants a model call builds
// ai.Message values and reads an *ai.AssistantMessage back, in
// core/pig/pigmodel. What is left here is the part PiG has no opinion
// about: which role may reach which tool, what the console was shown, and
// what the operator has to be able to read back out of the database
// tomorrow.
//
// The split is the reason the names here say "transcript". ports.AgentMessage
// is a row. ai.Message is a wire type. They are not the same thing and must
// never be made the same thing: the row outlives the provider, survives a
// migration, and is what an incident review reads.
package ports

import (
	"context"
	"errors"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// AgentRequest is one user turn handed to the agent kernel.
//
// The kernel does not compose the prompt from this; it is handed the pieces
// the host already decided, so a replacement kernel cannot change what an
// operator's persona or role policy says.
type AgentRequest struct {
	SessionID string
	UserID    uint64
	// Role is the caller's system role (admin | user | viewer). The kernel
	// filters the tool bag by it before the turn starts, so a viewer can
	// never reach a mutating tool no matter what a profile requests.
	Role string
	// UserText is the turn verbatim, after any mention rendering.
	UserText string
	// History is the conversation BEFORE this turn, oldest first.
	//
	// It exists on the request rather than being replayed out of the host's
	// session store by the kernel, because the transcript is policy, not
	// plumbing: the host has already applied history-window limits, dropped
	// superseded tool batches, and redacted what a viewer must not see. A
	// kernel that read the session itself would answer a different question
	// from the one the host composed.
	//
	// An empty History is a first turn, not an error.
	History []AgentMessage
	// Selection pins the model for this turn. An unpinned selection
	// resolves to the cluster default at call time.
	Selection domain.ModelSelection
	// WebSearchEnabled gates the search tool for this turn only.
	WebSearchEnabled bool
	// Locale is the console language the reply must use.
	Locale string
	// SystemPrompt is the fully assembled base prompt. The kernel does not
	// compose it; skill and persona assembly happen upstream so a
	// replacement kernel cannot change what the prompt says.
	SystemPrompt string
	// CriticalReminder is the persona's anti-drift text, injected ahead of
	// the turn rather than into the system prompt, so it is re-read each
	// turn instead of being cached with the prompt prefix.
	CriticalReminder string
	// MaxIterations caps tool rounds for the turn. Zero means the kernel
	// default applies.
	MaxIterations int
}

// AgentMessage is one row of a stored conversation.
//
// The shape is deliberately minimal. Tool arguments and results are carried
// as opaque JSON bytes rather than typed structures: the host stores what
// the provider said, and a kernel that needs a richer form decodes it.
// Typing them here would force the store to agree on a provider wire
// format, which is precisely the coupling this boundary exists to prevent —
// and it would mean re-writing every stored row the day PiG renames a field.
//
// Exactly one of the role-specific fields is set. A message with none set
// carries no information and a kernel should skip it — silently dropping a
// malformed entry is safer than failing a turn, because the entry came from
// persisted history the operator cannot repair from the console.
type AgentMessage struct {
	// Role is "user" | "assistant" | "tool".
	Role string
	// Content is the text of the message. Empty is legal for an assistant
	// turn that only requested tool calls.
	Content string
	// ToolCalls are the invocations an assistant turn requested.
	ToolCalls []AgentToolCall
	// ToolCallID identifies which assistant tool call a "tool" message
	// answers. Required for role "tool": without it the result cannot be
	// attached to its request and the provider rejects the transcript.
	ToolCallID string
	// ToolName is the tool a "tool" message reports on. Informational —
	// provider correlation uses ToolCallID.
	ToolName string

	// Model and Usage annotate an assistant message the kernel observed
	// accounting for. Both are optional and both are ignored on any other
	// role.
	//
	// They ride here rather than on the turn result because the console
	// shows provenance per message ("the answer above came from glm-4-plus")
	// and the usage ledger sums per row. A kernel that observed them and
	// could not hand them on would force the host to re-ask the provider or
	// to show every turn as unattributed.
	Model string
	Usage *TranscriptUsage
}

// AgentToolCall is one tool invocation an assistant asked for.
type AgentToolCall struct {
	// ID is the provider-assigned call id, echoed by the answering tool
	// message.
	ID string
	// Name is the tool's wire name.
	Name string
	// Arguments is the raw JSON object the model produced, passed through
	// unmodified.
	Arguments []byte
}

// TranscriptUsage is the token accounting written to a row.
//
// It is a storage record, not a model type, and the difference is not
// cosmetic. ai.Usage carries provider-specific extras — reasoning tokens,
// a one-hour cache-write counter, a cost breakdown in five currencies — and
// those are a property of the provider that produced them, not of the
// ledger. Pinning a row to them would mean re-writing stored history the day
// a provider is added, and a column set that grows a field per provider
// stops being a ledger and starts being a copy of a wire type.
//
// Only the four numbers an operator is actually charged on are kept.
type TranscriptUsage struct {
	InputTokens  int
	OutputTokens int
	// CacheReadTokens and CacheWriteTokens are stored separately from
	// InputTokens because providers bill them at different rates, and a
	// ledger that folds them in cannot answer "what did cache save".
	CacheReadTokens  int
	CacheWriteTokens int
	// ReportedTotal is the total the provider itself reported, when it
	// reported one. Zero means "the provider was silent".
	//
	// It exists because the sum above is not always the number that was
	// billed. Reasoning models bill reasoning tokens, and several providers
	// fold those into the total without naming them in either input or
	// output, so Input+Output under-reports. Recomputing the provider's own
	// number would quietly change the bill, which is the one number in this
	// struct a caller must not second-guess.
	ReportedTotal int
	// CostUSD is the reply's computed cost, as the provider priced it.
	//
	// It is advisory and is labelled as such wherever it is shown: OpsKeeper
	// does not own the price table, so a provider that misprices a model
	// writes a wrong number here. It is stored anyway because "which model
	// is expensive" is the question an operator actually asks, and token
	// counts alone do not answer it across a catalogue with different
	// per-token rates.
	CostUSD float64
}

// Total returns the number of tokens the row consumed, counting cache reads
// and writes as input.
//
// A provider-reported total wins over the sum. The sum is the fallback for
// providers that report no total at all, and is a lower bound rather than an
// estimate when the provider was silent about reasoning tokens.
func (u TranscriptUsage) Total() int {
	if u.ReportedTotal != 0 {
		return u.ReportedTotal
	}
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Add folds one provider reply's accounting into the running total.
//
// It is here rather than at each call site because getting it wrong is
// invisible: a turn total that under-reports by the cache component still
// looks like a number. The provider's own total is preferred whenever it
// reported one, for the same reason TranscriptUsage.Total does.
// Billed returns the token count this usage should be charged as.
//
// The provider's own total wins when it reported one, for the same reason
// Add sums reports rather than mixing them: a recomputed number belongs to
// neither the provider's bill nor the caller's own arithmetic. When the
// provider was silent the fallback is every component the ledger stores,
// because a turn that reports no usage is still tokens that were bought.
func (u TranscriptUsage) Billed() int {
	if u.ReportedTotal > 0 {
		return u.ReportedTotal
	}
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

func (u *TranscriptUsage) Add(next TranscriptUsage) {
	if u == nil {
		return
	}
	u.InputTokens += next.InputTokens
	u.OutputTokens += next.OutputTokens
	u.CacheReadTokens += next.CacheReadTokens
	u.CacheWriteTokens += next.CacheWriteTokens
	u.CostUSD += next.CostUSD
	if next.ReportedTotal != 0 {
		// A run of calls with a reported total reports the sum of the
		// reports, which is the bill. Mixing that with the fallback sum
		// for silent calls would produce a number belonging to neither.
		u.ReportedTotal += next.ReportedTotal
	}
}

// TokenRecorder charges settled usage to a budget ledger.
//
// It is the other half of BudgetChecker, and it is declared as a separate
// interface because the two answer different questions at different times:
// the checker asks "may this call happen" before the provider, the recorder
// asks "this cost N" after it settled. A cap that is only ever checked is
// not a cap — the ledger's running total stays at zero and every check
// passes, which is exactly the failure this interface exists to close.
type TokenRecorder interface {
	Record(ctx context.Context, sessionID string, tokens int) error
}

// BudgetChecker reports whether spend may continue.
//
// It is the query half of a budget, and on its own it is not a budget: a
// checker with no recorder beside it sees a running total that never moves.
type BudgetChecker interface {
	// Allow reports whether another model call is permitted. It is
	// consulted before each round trip so a turn that would blow the
	// budget stops cleanly rather than mid-flight.
	Allow(ctx context.Context, sessionID string) (allowed bool, reason string)
}

// ErrSinkClosed is what a sink reports once the consumer it was feeding has
// gone away.
//
// It is a named error rather than a bare one so a caller can tell "this
// console closed the tab" from "this turn failed": the first means stop
// relaying, the second means report it. A sink that invents its own error
// for the first case leaves the caller guessing which of the two it hit.
var ErrSinkClosed = errors.New("ports: event sink is closed")

// EventSink receives streaming frames for one turn.
//
// Implementations must not block indefinitely. A sink that is not reading
// causes the kernel to drop frames and bump the session's Seq gap rather
// than stall the loop: a stalled consumer must never hold a provider
// connection open.
//
// Emit may be called concurrently. A kernel runs the sibling tool calls of
// one assistant turn in parallel, so several goroutines reach the sink at
// once; an implementation that appends to a bare slice will lose frames or
// trip the race detector. Frame ORDER is the mapper's responsibility (it
// hands out the sequence numbers under its own lock), so a sink must not
// try to serialise for that reason — it only has to be safe to call.
type EventSink interface {
	// Emit delivers one frame. Returning an error ends the turn: a
	// consumer that has gone away cancels the work it asked for.
	Emit(ctx context.Context, ev StreamEvent) error
}

// StreamEvent is an alias for the wire frame so a kernel implementation
// needs to import only this package. It is the same type the console
// parses; there is no separate internal event shape to translate through.
type StreamEvent = wire.StreamEvent

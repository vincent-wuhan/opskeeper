package pigagent

import (
	"context"

	"github.com/MichaelKinsy/PiG/agent"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Deps are the host services one turn runs against.
//
// They live here rather than in core/ports for one reason, and it used to be
// written down wrongly. This struct used to carry a Model completer, and the
// comment said the type had to be in this module because of it. Nothing ever
// read that field: both kernels resolve the model from their own
// k.opts.Models against the turn's ModelSelection, and no assembly anywhere
// in the repository filled it in. A field that is never read is not a reason
// to move a type between modules, and the reason it is written here now is
// the one that is still true — a host that holds an Agent holds a
// *TurnResult whose Reply is a *ai.AssistantMessage, so the turn's own
// vocabulary cannot be expressed in a package that must not depend on PiG.
// Ports is stdlib-only by design, and it is left holding the two things that
// are genuinely the host's: the stored transcript and the streaming sink.
//
// What Deps is, precisely, is the turn's POLICY surface: which tools the
// turn may reach, who audits them, who may refuse one, what the day's budget
// is, and who watches a call from admission to settle. Every one of those is
// a decision the host makes and the kernel must not re-derive. The model is
// not on that list — it arrives on the request as a ModelSelection and is
// resolved through the kernel's own model registry, because the model is a
// property of the deployment rather than of the turn's permissions. Putting
// it in Deps as well would have been a second place for the same choice to
// be made, and a second place is a drift waiting for a reason.
//
// A kernel that reaches for anything outside this struct is bypassing the
// host's policy and audit guarantees.
type Deps struct {
	// Tools is the tool bag for this turn, already filtered by role and
	// profile. The kernel must not widen it.
	Tools ports.ToolBag
	// Audit records every tool call, block, and failure.
	Audit ports.AuditSink
	// Gate is the sole path for a gated call to execute. A kernel that
	// finds no gate must refuse every non-read tool rather than run it.
	Gate ports.ApprovalGate
	// Budget is consulted before each model call. Returning false ends the
	// turn with TurnToolBudget.
	Budget ports.BudgetChecker
	// Spender charges each settled model's usage to the budget ledger.
	// Optional: nil records nothing.
	//
	// It sits beside Budget rather than inside it because the two run at
	// different moments — the checker before the provider, the recorder after
	// it settles — and a ledger that is checked but never written sees a
	// running total of zero and lets everything through.
	Spender ports.TokenRecorder
	// Recorder observes each admitted tool call from admission to settle.
	// It is how the console's tool table is populated without the kernel
	// knowing its schema. Optional: nil records nothing.
	Recorder ports.ToolCallRecorder
}

// DepsProvider supplies the host services for one turn.
//
// It is a function rather than a struct so a host can vary the policy per
// turn — a viewer's tool bag, a worker's reduced scope, an investigator's
// audit sink — without constructing a different kernel.
type DepsProvider func(ctx context.Context, req ports.AgentRequest) (Deps, error)

// Agent is the host's view of the agent loop.
//
// It used to live in core/ports as ports.Agent, which made the host depend
// on an interface whose result type was a re-shaped copy of PiG's own
// assistant message. Moving it here closes the loop: a host that holds an
// Agent holds a *TurnResult whose Reply IS the PiG message, so nothing
// downstream has to translate between two vocabularies for the same turn.
//
// *Kernel is the production implementation. Implementations must be safe for
// concurrent use — one loop serves every session in the process.
type Agent interface {
	// Run settles one turn. It blocks until the turn completes, the context
	// is cancelled, or a cap is reached. Emitted frames go to the sink
	// supplied on the request context.
	Run(ctx context.Context, req ports.AgentRequest) (*TurnResult, error)
	// Steer injects a message into a turn already in flight, the way a
	// supervisor redirects a running investigation. It returns
	// ErrNotRunning when no turn is active for the session.
	Steer(ctx context.Context, sessionID, text string) error
	// Abort cancels the in-flight turn for a session. It is idempotent and
	// safe to call when nothing is running.
	Abort(ctx context.Context, sessionID string) error
	// Spawn starts a background worker with its own tool bag and system
	// prompt. The returned id is used with Steer, Abort, and Notify.
	Spawn(ctx context.Context, req ports.AgentRequest) (string, error)
	// Notify reports a worker's terminal state to the parent turn, which
	// renders it as a task_notification frame.
	Notify(ctx context.Context, workerID, status, summary string) error
}

// TurnResult is the settled outcome of one turn.
type TurnResult struct {
	// Reply is the PiG agent loop's own settled assistant message, not a
	// re-shaped copy of it. A host that wants the text calls
	// ai.ContentText on its content; a host that wants the thinking blocks,
	// the tool calls and the raw stop reason reads them off the message.
	// Re-shaping it here would be the dual vocabulary this kernel was
	// rebuilt to delete, one type higher up the stack.
	Reply *agent.AssistantMessage
	// Content is the final assistant text, carried alongside Reply because
	// "what did the agent say" is asked by every caller and re-deriving it
	// from content blocks at each of them is how two call sites end up
	// disagreeing about what the model said.
	Content string
	// Iterations counts how many model round trips the turn consumed.
	Iterations int
	// Usage aggregates the turn across every model call it made, in the
	// stored-ledger shape rather than the provider shape — see
	// ports.TranscriptUsage for why the two are different types.
	Usage ports.TranscriptUsage
	// Stopped reports why the loop ended, as one of the Turn* constants.
	Stopped string
	// Err is non-nil only when Stopped is TurnError.
	Err error
}

// Stop reasons for a turn. They describe the loop's own outcome, not the
// provider's: a provider can stop a message for a dozen reasons, and the
// console only ever asks one question about it — did the investigation
// finish, and if not, what stopped it.
const (
	TurnEndTurn       = "end_turn"
	TurnMaxIterations = "max_iterations"
	TurnToolBudget    = "tool_budget"
	TurnCancelled     = "cancelled"
	TurnError         = "error"
)

// UsageOf folds one settled loop message into the stored-ledger shape.
//
// It is exported, and it is the only place agent.AssistantMessage becomes a
// ports.TranscriptUsage, because getting the fold wrong is invisible: a turn
// total that under-reports by the cache component still looks like a number,
// and the discrepancy surfaces on an invoice weeks later. A second copy in a
// caller is a second chance to drop a field, and the two copies would then
// disagree about the same reply.
//
// The nil return from ObserveUsage is PiG's way of saying "this provider
// reported no usage" — a message with no Usage set, or a stream view that
// has not settled. It is the common case for a local model and a rare one
// for a hosted API, which is exactly why it survives to production: it only
// fires for the deployments nobody tests against. A zero row is the honest
// answer here; a character-count estimate in the same column as a billed
// figure is not, and the two become indistinguishable the moment anyone sums
// the column.
func UsageOf(msg *agent.AssistantMessage) ports.TranscriptUsage {
	if msg == nil {
		return ports.TranscriptUsage{}
	}
	observed := msg.ObserveUsage()
	if observed == nil {
		return ports.TranscriptUsage{}
	}
	return ports.TranscriptUsage{
		InputTokens:      observed.Input,
		OutputTokens:     observed.Output,
		CacheReadTokens:  observed.CacheRead,
		CacheWriteTokens: observed.CacheWrite,
		ReportedTotal:    observed.TotalTokens,
		CostUSD:          observed.Cost.Total,
	}
}

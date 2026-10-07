package pigcoding

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/MichaelKinsy/PiG/agent"
)

// DefaultMaxRounds is the round cap a caller gets when it asks for one and
// leaves the number to this package.
//
// It is deliberately below pigagent.Kernel's DefaultMaxIterations of 12
// rather than equal to it. The two numbers answer different questions. The
// kernel's cap bounded a loop this repository owned, where every round was a
// round through OpsKeeper's own tool adapter. A round here is a round through
// PiG's agent loop with whatever tools a plugin package contributed, and a
// plugin is code this repository did not write. The budget a control plane
// needs is therefore the one that survives its own tool catalogue growing.
const DefaultMaxRounds = 12

// TurnBudget is the per-turn round cap for a session running on the embedded
// SDK.
//
// # Why this exists at all
//
// agent.AgentOptions has MaxTurns, and pigagent.Kernel passes it, so the
// control plane has had a turn cap since the kernel landed. The SDK panel has
// no such field: coding.SessionStartOptions exposes BeforeToolCall,
// ExtraTools, SkipBuiltinTools and the allow/deny lists, and no iteration
// bound. That absence is what made an earlier round of this work conclude that
// the control plane could not move onto coding.Session without silently
// losing its cap (decision 75, option C). The conclusion was half right: the
// field really is missing. The inference was wrong, because the cap does not
// need a field.
//
// PiG already answers "how many tool rounds may this turn have" through the
// hook OpsKeeper owns anyway. BeforeToolCallHook returns a
// ToolCallHookResult, and that result carries Block, Reason and Terminate.
// A hook that counts calls and returns {Block, Terminate} at the cap makes the
// agent loop stop requesting new provider rounds: tool_execution.go turns the
// blocked call into an error tool result carrying Terminate, the batch
// terminator sees every finalized result asking to stop, and the run exits
// after settling the turn it is in. The model still receives a tool result
// explaining the refusal, so the turn ends with a reply rather than a wedge.
//
// The alternative — Session.RequestAbort() from inside the hook — was
// rejected because it reaches past the panel to end the run by cancellation.
// Cancellation is the wrong verb for a budget: it produces an aborted turn,
// which a console renders as a failure and an operator reads as "the agent
// crashed". A spent budget is not a failure, it is the cap doing its job.
//
// # Ordering, and why it is not negotiable
//
// Runtime.Start installs this hook ahead of every hook the caller passed. The
// order is load-bearing in the direction it is: the budget counts calls, and a
// call the policy gate refused was still a round the model spent. Were the
// gate first, a gate that refuses everything would let the model retry for
// free and the turn would never be bounded — the cap would be silently
// disabled by the very defence it sits behind. Running the budget outermost
// means the loop is bounded no matter what any inner hook decides, while the
// gate keeps its authority over what actually executes.
type TurnBudget struct {
	max int

	mu     sync.Mutex
	rounds int
}

// NewTurnBudget builds a budget of maxRounds tool rounds.
//
// A maxRounds below one means unbounded, and the caller is trusted to mean
// it: this package is a boundary, not a control plane, and a short
// summarisation turn has no business spending twelve provider round trips.
// The turn-shaped callers are the ones that must pass a number, and the
// console's own default lives with the console's configuration.
func NewTurnBudget(maxRounds int) *TurnBudget {
	if maxRounds < 0 {
		maxRounds = 0
	}
	return &TurnBudget{max: maxRounds}
}

// MaxRounds is the cap this budget enforces, or zero when unbounded.
func (b *TurnBudget) MaxRounds() int {
	if b == nil {
		return 0
	}
	return b.max
}

// Rounds is how many tool calls the turn has spent so far. It is exported for
// the turn result OpsKeeper persists, because "this investigation stopped
// because it ran out of budget" is an answer an operator needs and a bare
// truncation is not.
func (b *TurnBudget) Rounds() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rounds
}

// Spent reports whether the budget is exhausted and the cap has been applied.
func (b *TurnBudget) Spent() bool {
	if b == nil || b.max < 1 {
		return false
	}
	return b.Rounds() > b.max
}

// composeBeforeToolCall assembles the hook chain a session runs with, budget
// first.
//
// It is a named function rather than three lines inside Runtime.Start so that
// the ordering is something a test can assert directly. The order is the whole
// argument TurnBudget makes, and an invariant that lives only in a comment
// above a slice literal is an invariant the next edit reorders.
func composeBeforeToolCall(budget *TurnBudget, caller []agent.BeforeToolCallHook) []agent.BeforeToolCallHook {
	spend := budget.Hook()
	if spend == nil && len(caller) == 0 {
		return nil
	}
	hooks := make([]agent.BeforeToolCallHook, 0, len(caller)+1)
	if spend != nil {
		hooks = append(hooks, spend)
	}
	return append(hooks, caller...)
}

// Hook is the BeforeToolCallHook that spends the budget.
//
// The first call at the cap is refused softly: the model is told the budget
// is spent and asked to answer with what it has, which is the outcome an
// operator wants from an investigation that ran long — a real conclusion
// rather than a truncated one. Only a model that calls again after being told
// to stop is terminated, so the cap costs at most one extra provider request
// beyond the round it was reached on and is a hard bound in every case.
func (b *TurnBudget) Hook() agent.BeforeToolCallHook {
	if b == nil || b.max < 1 {
		return nil
	}
	return func(_ context.Context, _, _ string, _ json.RawMessage) agent.ToolCallHookResult {
		b.mu.Lock()
		b.rounds++
		round := b.rounds
		b.mu.Unlock()

		if round <= b.max {
			return agent.ToolCallHookResult{}
		}
		if round == b.max+1 {
			// The soft refusal. Terminate is left false so the model gets
			// one turn to land an answer with the evidence it gathered.
			return agent.ToolCallHookResult{
				Block:  true,
				Reason: fmt.Sprintf("turn budget of %d tool rounds is spent; answer now with the findings so far and do not call another tool", b.max),
			}
		}
		return agent.ToolCallHookResult{
			Block:     true,
			Terminate: true,
			Reason:    fmt.Sprintf("turn budget of %d tool rounds is spent and the turn did not conclude; stopping", b.max),
		}
	}
}

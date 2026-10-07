package agentkernel

import (
	"context"
	"errors"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigagent"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// ErrNoToolSource reports that the host was assembled without a way to
// resolve the turn's tool bag.
//
// It fails the turn rather than falling back to an empty bag. An empty bag
// looks like a model that decided not to use a tool: the turn succeeds, the
// answer is plausible, and nothing anywhere says the deployment forgot to
// pass its tools in. Refusing at assembly time makes the wiring bug visible
// at boot instead of as a quietly worse answer.
var ErrNoToolSource = errors.New("agentkernel: no tool source configured")

// Host binds the host services one turn runs against onto the kernel's deps
// port.
//
// Every field is a decision the host already made: the bag is filtered by
// role, profile and the write gate before it arrives here, the ledger is the
// host's, and the gate is the host's. Nothing in this struct inspects, widens
// or re-derives any of them — a binding layer that made a policy decision
// would become a second place to look when a call was refused, and the first
// place would be wrong.
type Host struct {
	// ToolsFor resolves the bag for one turn. Required: see ErrNoToolSource.
	//
	// It is a function rather than a bag because the bag is per turn — a
	// viewer gets a read-only view of the same registry an admin reaches in
	// full, and the write gate is consulted live.
	ToolsFor func(ctx context.Context, req ports.AgentRequest) (ports.ToolBag, error)

	// Audit receives every gate decision. Optional: nil records nothing, and
	// the kernel treats that as "this deployment does not keep a ledger".
	Audit ports.AuditSink
	// Gate decides whether a mutating call may run. Optional: a nil gate
	// makes the kernel refuse every non-read call, which is the documented
	// fail-closed behaviour rather than a licence to run it.
	Gate ports.ApprovalGate
	// Budget is consulted before each model call. Optional.
	Budget ports.BudgetChecker
	// Spender books settled usage against the same ledger. Optional, and
	// wired from the same adapter as Budget: a checker without a recorder
	// beside it is a cap that never moves.
	Spender ports.TokenRecorder
	// Recorder observes each call from admission to settle. Optional.
	Recorder ports.ToolCallRecorder
}

// Provider returns the kernel's deps callback, or an error when the host is
// not assembled enough to run a turn.
func (h Host) Provider() (pigagent.DepsProvider, error) {
	if h.ToolsFor == nil {
		return nil, ErrNoToolSource
	}
	// The bag is not resolved here: which tools a turn reaches depends on
	// the request (role, session, write gate), so resolving it eagerly would
	// hand every turn the first caller's view.
	return func(ctx context.Context, req ports.AgentRequest) (pigagent.Deps, error) {
		tools, err := h.ToolsFor(ctx, req)
		if err != nil {
			return pigagent.Deps{}, err
		}
		return pigagent.Deps{
			Tools:    tools,
			Audit:    h.Audit,
			Gate:     h.Gate,
			Budget:   h.Budget,
			Spender:  h.Spender,
			Recorder: h.Recorder,
		}, nil
	}, nil
}

// Budget adapts the pre-existing per-day token cap onto the kernel's budget
// port.
//
// The two are shaped differently and the difference is not cosmetic: the
// legacy checker takes an estimated prompt size, and the kernel's port asks a
// yes/no question without one. The estimate is therefore zero here, which
// makes the check *weaker* than the legacy path — it stops a turn once the
// cap is already crossed rather than before the crossing call. That is
// recorded rather than hidden: a silent stronger claim ("we check the same
// way on both paths") would be false, and the honest fix is to give the port
// the estimate, not to guess one at this layer.
type Budget struct {
	// Checker is the legacy per-day cap. Required for the adapter to do
	// anything; a nil checker yields a nil adapter so the deps field stays
	// nil and the kernel runs unbudgeted, which is what "not configured"
	// means here.
	Checker TokenBudget
	// UserFor maps a session to its budget bucket. A nil resolver puts every
	// session in the global bucket (0), which is what the single-tenant
	// deployment is.
	UserFor func(sessionID string) uint64
}

// TokenBudget is the narrow seam onto the legacy checker.
//
// It is one method wide on purpose. The legacy interface also records usage
// after a call; the kernel's port has no record half, so declaring it here
// would be a method nothing calls — and a seam that claims to keep the
// ledger in sync but never writes to it is worse than one that says what it
// actually does.
type TokenBudget interface {
	Check(ctx context.Context, userID uint64, estPromptTokens int) error
}

// TokenRecorder is the write half of the ledger behind TokenBudget.
//
// It is a separate interface on purpose, and this comment used to say the
// opposite: it argued that declaring it would be "a method nothing calls",
// which was true — and was the bug. A checker with no recorder never sees its
// running total move, so the console's daily cap passed every call it was
// written to bound. The seam existed and the operator's ceiling did not.
type TokenRecorder interface {
	Record(ctx context.Context, userID uint64, tokens int) error
}

// NewBudget wraps a checker. A nil checker yields nil: see Budget.Checker.
func NewBudget(checker TokenBudget, userFor func(sessionID string) uint64) *Budget {
	if checker == nil {
		return nil
	}
	return &Budget{Checker: checker, UserFor: userFor}
}

// Allow reports whether another model call may run.
//
// The reason is the checker's error text: it is what the operator sees in
// the turn's terminal frame, and inventing a reason here would replace the
// one that names the cap.
func (b *Budget) Allow(ctx context.Context, sessionID string) (bool, string) {
	if b == nil || b.Checker == nil {
		return true, ""
	}
	if err := b.Checker.Check(ctx, b.userFor(sessionID), 0); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// Record books a settled turn's tokens against the ledger of whoever
// owns the session.
//
// The user bucket is resolved the same way Allow resolves it, so the money a
// call was checked against and the money it is charged to are the same
// person's. A ledger whose two halves disagree is a cap that is not
// enforceable.
//
// A checker that cannot record is not an error here: an unbudgeted
// deployment still runs every turn, and the seam stays honest by reporting
// that it had nothing to say.
func (b *Budget) Record(ctx context.Context, sessionID string, tokens int) error {
	if b == nil || b.Checker == nil || tokens <= 0 {
		return nil
	}
	recorder, ok := b.Checker.(TokenRecorder)
	if !ok {
		return nil
	}
	return recorder.Record(ctx, b.userFor(sessionID), tokens)
}

func (b *Budget) userFor(sessionID string) uint64 {
	if b.UserFor == nil {
		return 0
	}
	return b.UserFor(sessionID)
}

var _ ports.BudgetChecker = (*Budget)(nil)
var _ ports.TokenRecorder = (*Budget)(nil)

// TurnToolsFromContext is the ToolSource for a deployment whose caller
// resolves the bag itself and stamps it on ctx (see ports.WithTurnTools).
//
// It is a named function rather than an inline closure in the assembly so the
// missing-bag case is impossible to lose: an assembly that inlined the type
// assertion would read a nil bag from a forgotten stamp and run the turn with
// no tools, which looks exactly like a model that chose not to use any.
//
// The request is accepted and ignored so this has the DepsProvider tool-source
// shape; a deployment that resolves per request instead writes its own
// function.
func TurnToolsFromContext(ctx context.Context, _ ports.AgentRequest) (ports.ToolBag, error) {
	if bag, ok := ports.TurnToolsFromContext(ctx); ok {
		return bag, nil
	}
	return nil, ErrNoToolSource
}

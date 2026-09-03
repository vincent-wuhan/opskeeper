// Package chatdiagnose — orchestrator_adapter.go
//
// Day 5 Task 5.7: bridge between chatdiagnose's local Orchestrator
// interface and the loop package's production Orchestrator. Lives
// in the chatdiagnose package (per monorepo boundary) so the
// chatdiagnose build does NOT pull the heavy loop import transitively
// — the adapter itself is the single seam.
//
// Why an adapter (vs. importing loop.Orchestrator directly):
//
//   - chatdiagnose's Orchestrator interface is narrow: Run(...) only.
//     The adapter hides the loop-specific RunOptions/RunResult
//     shape from the rest of the package.
//   - The conversion responsibility lives in one file; future
//     divergence (e.g. NewOrchestrator returns loop.Orchestrator by
//     pointer, or RunOptions adds new fields) is handled in one place.
//   - Tests swap the adapter for a mock without touching the loop
//     package (already in place in service_test.go).
//
// Type-conversion specifics:
//
//   - chatdiagnose.OrchestratorRunOptions.FromPhase is a string
//     (chatdiagnose deliberately does not import the loop package's
//     typed Phase). The adapter validates the string against the
//     loop.Phase allowlist before converting, surfacing an error
//     when the caller passes a typo.
//   - chatdiagnose.OrchestratorRunResult.FirstLoopEventID is read
//     from loop.RunResult.LoopEvents[0].ID; the loop-side phase
//     walker writes the phase_entered event with a synthesised
//     snowflake ID and the InMemoryEventRepo / production repo both
//     populate e.ID before returning.
//   - FinalPhase is converted from loop.Phase (typed string) to
//     plain string for the chatdiagnose-side DTO.

package chatdiagnose

import (
	"context"
	"errors"
	"fmt"
	"strings"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
)

// LoopOrchestrator is the production-side Orchestrator contract the
// adapter consumes. Defined here as a narrow interface so:
//
//   - Production wires *loop.Orchestrator (which satisfies it).
//   - Tests can swap in a stub without pulling the loop package's
//     full constructor (NewOrchestrator).
//
// The interface mirrors loop.Orchestrator.Run exactly so the
// production *loop.Orchestrator satisfies it for free.
type LoopOrchestrator interface {
	Run(ctx context.Context, opts loop.RunOptions) (*loop.RunResult, error)
}

// OrchestratorAdapter implements chatdiagnose.Orchestrator by
// wrapping a LoopOrchestrator. It is the production value passed
// into NewChatDiagnoseService (Day 5 wire-up; see cmd/opskeeper/main.go).
//
// The adapter is goroutine-safe; the underlying loop.Orchestrator
// serialises via its per-incident advisory lock, so concurrent
// PromoteToLoop calls for distinct incidents run in parallel while
// calls for the same incident serialise.
type OrchestratorAdapter struct {
	loop LoopOrchestrator
}

// NewOrchestratorAdapter constructs the adapter. Returns an error
// when loop is nil so the wire-up misconfiguration fails loudly.
func NewOrchestratorAdapter(loop LoopOrchestrator) (*OrchestratorAdapter, error) {
	if loop == nil {
		return nil, errors.New("chatdiagnose: LoopOrchestrator is required")
	}
	return &OrchestratorAdapter{loop: loop}, nil
}

// Run satisfies the chatdiagnose.Orchestrator interface. The
// implementation translates chatdiagnose.OrchestratorRunOptions
// (string-typed FromPhase) into loop.RunOptions (Phase-typed
// FromPhase) and projects the result back.
//
// Errors:
//
//   - empty IncidentID / TenantID → wrapped ErrInvalidInput.
//   - unknown FromPhase string → ErrUnknownPhase.
//   - loop-side failure → wrapped loop error.
func (a *OrchestratorAdapter) Run(ctx context.Context, opts OrchestratorRunOptions) (*OrchestratorRunResult, error) {
	if opts.IncidentID == "" {
		return nil, fmt.Errorf("chatdiagnose: orchestrator adapter: %w: incident_id is required", ErrInvalidInput)
	}
	if opts.TenantID == "" {
		return nil, fmt.Errorf("chatdiagnose: orchestrator adapter: %w: tenant_id is required", ErrInvalidInput)
	}

	phase, err := phaseFromString(opts.FromPhase)
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: orchestrator adapter: %w", err)
	}

	res, err := a.loop.Run(ctx, loop.RunOptions{
		IncidentID:  opts.IncidentID,
		FromPhase:   phase,
		TenantID:    opts.TenantID,
		TriggeredBy: opts.TriggeredBy,
		// IdempotentKey is omitted: chatdiagnose owns its own
		// idempotency via ConversationID + linked turn seq.
	})
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: orchestrator adapter run: %w", err)
	}

	firstLoopEventID := int64(0)
	if len(res.LoopEvents) > 0 {
		firstLoopEventID = res.LoopEvents[0].ID
	}
	return &OrchestratorRunResult{
		IncidentID:       res.IncidentID,
		FirstLoopEventID: firstLoopEventID,
		FinalPhase:       string(res.FinalPhase),
	}, nil
}

// phaseFromString maps a chatdiagnose string-typed FromPhase to the
// loop.Phase enum. Empty string defaults to PhaseDetected (alert
// entry); this matches the loop.Run default and keeps the call site
// short for the typical "promote from chat" path which passes
// PhaseCorrelated.
//
// Whitelist validation: an unknown phase string returns
// ErrUnknownPhase so the chat-side handler can return a clean 400
// instead of silently advancing to PhaseDetected.
func phaseFromString(s string) (loop.Phase, error) {
	switch strings.TrimSpace(s) {
	case "":
		return loop.PhaseDetected, nil
	case PhaseDetected:
		return loop.PhaseDetected, nil
	case PhaseCorrelated:
		return loop.PhaseCorrelated, nil
	case PhaseInvestigated:
		return loop.PhaseInvestigated, nil
	case PhaseApproved:
		return loop.PhaseApproved, nil
	case PhaseExecuted, PhaseVerified, PhasePostmortem, PhaseProposed:
		// PhaseExecuted/PhaseVerified/PhaseProposed are not part
		// of the loop package's canonical state machine — they
		// are chat-side aliases kept here for backward compat
		// with prior calls. Map them to the closest loop.Phase.
		// In practice no caller should pass these; the chat
		// promote path always passes PhaseCorrelated.
		switch s {
		case PhaseExecuted, PhaseVerified, PhasePostmortem:
			return loop.PhasePostmortem, nil
		case PhaseProposed:
			return loop.PhaseCritiqued, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownPhase, s)
}

// ErrUnknownPhase is returned when the caller passes a FromPhase
// string that does not match any loop.Phase value (or the chat-side
// aliases). The chat HTTP layer maps this to 400 + error_code=
// unknown_phase so the SPA can surface a clear error.
var ErrUnknownPhase = errors.New("chatdiagnose: unknown FromPhase")

// ErrInvalidInput is returned by the adapter for empty required
// fields (incident_id, tenant_id). HTTP layer maps to 400 +
// error_code=invalid_input.
var ErrInvalidInput = errors.New("chatdiagnose: invalid orchestrator input")

// chatdiagnose/orchestrator_adapter_test.go — unit tests for the
// loop-orchestrator adapter (Task 5.7).
//
// Test matrix:
//
//   - happy path: chat promote runs through the adapter and the
//     loop.RunResult is projected into the chatdiagnose DTO with
//     FirstLoopEventID pulled from LoopEvents[0].ID.
//   - FromPhase string → loop.Phase conversion covers the 4
//     canonical phases (detected / correlated / investigated /
//     approved) + the chat-side aliases (executed / verified /
//     postmortem / proposed).
//   - Empty FromPhase defaults to PhaseDetected (loop behaviour).
//   - Empty IncidentID / TenantID → ErrInvalidInput.
//   - Unknown FromPhase → ErrUnknownPhase.
//   - Adapter's loop-side error is wrapped (errors.Is works
//     transitively via %w).

package chatdiagnose

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

type stubLoopOrchestrator struct {
	mu       sync.Mutex
	lastOpts loop.RunOptions
	result   *loop.RunResult
	err      error
}

func (s *stubLoopOrchestrator) Run(_ context.Context, opts loop.RunOptions) (*loop.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastOpts = opts
	if s.err != nil {
		return nil, s.err
	}
	if s.result != nil {
		cp := *s.result
		return &cp, nil
	}
	return &loop.RunResult{
		IncidentID: opts.IncidentID,
		FinalPhase: opts.FromPhase,
		LoopEvents: []loopmodel.Event{{
			ID:        4242,
			CreatedAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		}},
	}, nil
}

func TestOrchestratorAdapter_Run_HappyPath(t *testing.T) {
	t.Parallel()
	stub := &stubLoopOrchestrator{}
	a, err := NewOrchestratorAdapter(stub)
	if err != nil {
		t.Fatalf("NewOrchestratorAdapter err: %v", err)
	}
	res, err := a.Run(context.Background(), OrchestratorRunOptions{
		IncidentID:  "inc-chat-1",
		TenantID:    "t1",
		FromPhase:   PhaseCorrelated,
		TriggeredBy: "chat",
	})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.IncidentID != "inc-chat-1" {
		t.Errorf("res.IncidentID = %q, want %q", res.IncidentID, "inc-chat-1")
	}
	if res.FirstLoopEventID != 4242 {
		t.Errorf("res.FirstLoopEventID = %d, want 4242", res.FirstLoopEventID)
	}
	if res.FinalPhase != PhaseCorrelated {
		t.Errorf("res.FinalPhase = %q, want %q", res.FinalPhase, PhaseCorrelated)
	}
	// Confirm the loop-side received the right Phase (typed).
	if stub.lastOpts.FromPhase != loop.PhaseCorrelated {
		t.Errorf("loop.Run received FromPhase = %s, want %s", stub.lastOpts.FromPhase, loop.PhaseCorrelated)
	}
}

func TestOrchestratorAdapter_Run_DefaultFromPhaseDetected(t *testing.T) {
	t.Parallel()
	stub := &stubLoopOrchestrator{}
	a, err := NewOrchestratorAdapter(stub)
	if err != nil {
		t.Fatalf("NewOrchestratorAdapter err: %v", err)
	}
	_, err = a.Run(context.Background(), OrchestratorRunOptions{
		IncidentID: "inc-2",
		TenantID:   "t1",
		// FromPhase empty → loop.PhaseDetected
	})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if stub.lastOpts.FromPhase != loop.PhaseDetected {
		t.Errorf("loop.Run received FromPhase = %s, want %s", stub.lastOpts.FromPhase, loop.PhaseDetected)
	}
}

func TestOrchestratorAdapter_Run_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	a, _ := NewOrchestratorAdapter(&stubLoopOrchestrator{})
	// Missing incident_id
	_, err := a.Run(context.Background(), OrchestratorRunOptions{TenantID: "t1"})
	if err == nil || !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
	// Missing tenant_id
	_, err = a.Run(context.Background(), OrchestratorRunOptions{IncidentID: "i1"})
	if err == nil || !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestOrchestratorAdapter_Run_UnknownPhase(t *testing.T) {
	t.Parallel()
	a, _ := NewOrchestratorAdapter(&stubLoopOrchestrator{})
	_, err := a.Run(context.Background(), OrchestratorRunOptions{
		IncidentID: "i1",
		TenantID:   "t1",
		FromPhase:  "totally_made_up",
	})
	if err == nil || !errors.Is(err, ErrUnknownPhase) {
		t.Errorf("expected ErrUnknownPhase, got %v", err)
	}
}

func TestOrchestratorAdapter_Run_ChatAliases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		chatPhase string
		loopPhase loop.Phase
	}{
		{PhaseDetected, loop.PhaseDetected},
		{PhaseCorrelated, loop.PhaseCorrelated},
		{PhaseInvestigated, loop.PhaseInvestigated},
		{PhaseApproved, loop.PhaseApproved},
		{PhaseExecuted, loop.PhasePostmortem},
		{PhaseVerified, loop.PhasePostmortem},
		{PhasePostmortem, loop.PhasePostmortem},
		{PhaseProposed, loop.PhaseCritiqued},
	}
	for _, tc := range cases {
		stub := &stubLoopOrchestrator{}
		a, _ := NewOrchestratorAdapter(stub)
		_, err := a.Run(context.Background(), OrchestratorRunOptions{
			IncidentID: "i1",
			TenantID:   "t1",
			FromPhase:  tc.chatPhase,
		})
		if err != nil {
			t.Errorf("Run(%q) err: %v", tc.chatPhase, err)
			continue
		}
		if stub.lastOpts.FromPhase != tc.loopPhase {
			t.Errorf("Run(%q) loop.FromPhase = %s, want %s", tc.chatPhase, stub.lastOpts.FromPhase, tc.loopPhase)
		}
	}
}

func TestOrchestratorAdapter_Run_LoopErrorWrapped(t *testing.T) {
	t.Parallel()
	stub := &stubLoopOrchestrator{err: errors.New("boom")}
	a, _ := NewOrchestratorAdapter(stub)
	_, err := a.Run(context.Background(), OrchestratorRunOptions{
		IncidentID: "i1",
		TenantID:   "t1",
		FromPhase:  PhaseDetected,
	})
	if err == nil {
		t.Fatalf("expected error from loop stub")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err chain should contain 'boom', got %v", err)
	}
}

func TestNewOrchestratorAdapter_NilLoopErrors(t *testing.T) {
	t.Parallel()
	if _, err := NewOrchestratorAdapter(nil); err == nil {
		t.Errorf("NewOrchestratorAdapter(nil) should error")
	}
}

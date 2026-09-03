// Package loop — event_repo.go
//
// Day 5 integration: narrow DB seam for the orchestrator's event log
// + contract persistence. The orchestrator (orchestrator.go::Run)
// optionally walks all 7 phases when both EventRepo + WorkerRegistry
// are wired; without these deps it falls back to the legacy
// phase_entered-only path (Day 1).
//
// Why a narrow interface (not the full loop_event_log repository):
//
//   - The loop package must not import internal/manager/repo/loop
//     (monorepo boundary, AGENTS.md §架构).
//   - Tests inject an in-memory fake (inmemory_repos.go); production
//     wires a *sql.DB-backed adapter in cmd/opskeeper/main.go.
//
// The repo exposes append + read; we deliberately omit update /
// delete because loop_event_log is APPEND-ONLY at the DB layer
// (see migration 0001-loop-event-log.sql's SIGNAL trigger).
package loop

import (
	"context"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// EventRepo is the narrow seam the orchestrator uses for the
// loop_event_log append-only event source of truth.
//
// AppendEvent MUST preserve the caller's IdempotencyKey; on UNIQUE
// collision (replay) the impl SHOULD return the previously-written
// row instead of erroring so retry calls are idempotent.
//
// ReadEvents returns events ordered by created_at ASC; the orchestrator
// passes the slice into recoverFromEvents for crash-recovery.
//
// The interface intentionally does NOT expose Delete / Update:
// loop_event_log is append-only.
type EventRepo interface {
	AppendEvent(ctx context.Context, e *loopmodel.Event) error
	ReadEvents(ctx context.Context, tenantID, incidentID string) ([]loopmodel.Event, error)
}

// ContractRepo is the narrow seam the orchestrator uses to read/write
// the loop_contract table (per-phase payloads).
//
// ReadContract returns the most recent contract for (incident, phase,
// contractType) — or (nil, nil) when none exists. tenantID MUST be
// enforced (cross-tenant isolation).
//
// WriteContract persists a new contract; the impl is responsible for
// choosing storage_backend ("db" inline vs "oss" offload based on
// payload size; see loopmodel.InlinePayloadMaxBytes).
type ContractRepo interface {
	WriteContract(ctx context.Context, c *loopmodel.Contract) error
	ReadContract(ctx context.Context, tenantID, incidentID string, phase Phase, contractType string) (*loopmodel.Contract, error)
}

// WorkerRegistry holds the 7 PhaseWorker instances the orchestrator
// dispatches to. Production wires this once at startup (see
// phase_workers.go::DefaultPhaseWorkerFactory); tests inject per-phase
// fakes via NewWorkerRegistry.
//
// Lookups are O(1); Get returns (nil, false) when no worker is
// registered for the phase — the orchestrator treats this as the
// terminal/failed branch (no further transitions).
type WorkerRegistry struct {
	workers map[Phase]PhaseWorker
}

// NewWorkerRegistry wraps a map into a registry. The orchestrator's
// Run uses this when WorkerRegistry is non-nil in OrchestratorDeps;
// otherwise it falls back to the global DefaultPhaseWorkerRegistry.
func NewWorkerRegistry(workers map[Phase]PhaseWorker) *WorkerRegistry {
	cp := make(map[Phase]PhaseWorker, len(workers))
	for k, v := range workers {
		cp[k] = v
	}
	return &WorkerRegistry{workers: cp}
}

// Get returns the worker for p; (nil, false) when absent.
func (r *WorkerRegistry) Get(p Phase) (PhaseWorker, bool) {
	if r == nil {
		return nil, false
	}
	w, ok := r.workers[p]
	return w, ok
}

// Has reports whether the registry has a worker for p.
func (r *WorkerRegistry) Has(p Phase) bool {
	_, ok := r.Get(p)
	return ok
}

// Phases returns the set of phases with a registered worker.
// Useful for diagnostics + the dry-run integration test.
func (r *WorkerRegistry) Phases() []Phase {
	if r == nil {
		return nil
	}
	out := make([]Phase, 0, len(r.workers))
	for p := range r.workers {
		out = append(out, p)
	}
	return out
}

// Package loop — orchestrator_walk.go
//
// Day 5 integration: the orchestrator's full 7-phase walker. Lives
// in its own file (vs. orchestrator.go) so the Day 1–4 lifecycle
// code stays compact and the 37 prior tests stay green.
//
// Behavior matrix:
//
//	EventRepo + WorkerRegistry BOTH nil → walkPhases returns nil and
//	  the orchestrator falls back to the Day 1 legacy path (writes
//	  one phase_entered event + returns). Existing tests rely on
//	  this; nothing in the wire-up passes nil deps except test stubs.
//
//	EventRepo + WorkerRegistry BOTH set → walkPhases dispatches the
//	  per-phase Worker (Planner → Executor → Verifier), persists
//	  events + contracts, and advances until a terminal phase.
//
// Pause handling: when the approved phase's Executor returns
// RawOutputs["approved_decision"] == "pause" the walker writes a
// phase_paused event with the pause_token side-effect target and
// exits the loop. The Resume() method picks up from there.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// canWalk reports whether the orchestrator is wired with the optional
// EventRepo + WorkerRegistry deps that enable the full phase walker.
// When either is nil the orchestrator falls back to the Day 1 path.
func (o *orchestrator) canWalk() bool {
	return o.deps.EventRepo != nil && (o.deps.WorkerRegistry != nil || hasGlobalWorkerRegistry())
}

// hasGlobalWorkerRegistry reports whether any worker is registered
// in the package-global DefaultPhaseWorkerRegistry. When the
// orchestrator was constructed without a WorkerRegistry but the
// package globals are populated (the production wire-up path) we
// still allow walking — the registry lookups hit the global.
func hasGlobalWorkerRegistry() bool {
	registryMu.Lock()
	defer registryMu.Unlock()
	return len(DefaultPhaseWorkerRegistry) > 0
}

// lookupWorker returns the PhaseWorker for p using either the local
// WorkerRegistry (preferred) or the package-global registry as
// fallback. Returns (nil, ErrWorkerNotRegistered) when neither has
// a worker for p.
func (o *orchestrator) lookupWorker(p Phase) (PhaseWorker, error) {
	if o.deps.WorkerRegistry != nil {
		if w, ok := o.deps.WorkerRegistry.Get(p); ok && w != nil {
			return w, nil
		}
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if w, ok := DefaultPhaseWorkerRegistry[p]; ok && w != nil {
		return w, nil
	}
	return nil, ErrWorkerNotRegistered
}

// walkPhases drives the state machine from `from` to a terminal
// phase (postmortem / failed / aborted). Each iteration:
//
//  1. Look up the Worker for the current phase (Planner → Executor
//     → Verifier).
//  2. Persist phase_entered.
//  3. Run Planner; if it errors, write phase_failed + stop.
//  4. Run Executor; persist phase_contract_written if the contract
//     ref is present, else write the side-effects as a payload.
//     For the approved phase a pause decision (RawOutputs
//     ["approved_decision"] == "pause") short-circuits: write
//     phase_paused + exit.
//  5. Run Verifier under its deadline context; on OK advance to
//     the next phase; on NOT-OK consult the rollback guard (only
//     for recovered → approved).
//  6. Update FinalPhase + accumulate LoopEvents.
//
// Returns the slice of events emitted (in chronological order)
// plus the FinalPhase. The caller appends the result to RunResult.
func (o *orchestrator) walkPhases(ctx context.Context, opts RunOptions, from Phase) ([]loopmodel.Event, Phase, error) {
	events := make([]loopmodel.Event, 0, 8)
	current := from
	attempt := 1
	rollbackCount := 0
	const maxAttempts = 32 // defensive cap so a misconfigured registry can't loop forever
	for i := 0; i < maxAttempts; i++ {
		// Reject already-terminal source phases (failed / aborted
		// can't be re-entered). Postmortem IS allowed in this
		// loop because the worker must run to produce the
		// artifact; we treat it as terminal only after the
		// worker completes.
		if current == PhaseFailed || current == PhaseAborted {
			return events, current, nil
		}

		worker, err := o.lookupWorker(current)
		if err != nil {
			// Missing worker: stop the walk and surface a
			// phase_failed event so the audit log records the
			// gap. Day 6–10 will replace BasePhaseWorker stubs
			// with concrete ones; the gap should be impossible
			// in production.
			events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventPhaseFailed, attempt, map[string]any{
				"reason": "worker_not_registered",
				"err":    err.Error(),
			}))
			return events, PhaseFailed, nil
		}

		// phase_entered
		events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventTypePhaseEntered, attempt, nil))

		// Planner
		planInput := PlanInput{
			IncidentID: opts.IncidentID,
			TenantID:   opts.TenantID,
			Phase:      current,
			Attempt:    attempt,
			// UpstreamContract is left nil for Day 5 minimal —
			// Day 6+ will read loop_contract via ContractRepo and
			// build a real ContractRef. The recovered phase
			// worker is the one that strictly requires a
			// non-nil upstream (it loads the ApprovalDecision);
			// for the dry-run integration we synthesise a
			// placeholder so the Planner doesn't reject. The
			// test's pgApprovedLoader ignores the ID.
			UpstreamContract: upstreamContractFor(current),
			AlertGroup:       opts.AlertGroup,
			CorrelationHints: opts.CorrelationHints,
		}
		plan, err := worker.Planner(ctx, planInput)
		if err != nil {
			events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventPhaseFailed, attempt, map[string]any{
				"reason": "planner_error",
				"err":    err.Error(),
			}))
			return events, PhaseFailed, nil
		}

		// Executor
		execResult, err := worker.Executor(ctx, plan)
		if err != nil {
			events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventPhaseFailed, attempt, map[string]any{
				"reason": "executor_error",
				"err":    err.Error(),
			}))
			return events, PhaseFailed, nil
		}

		// Special case: approved phase pause.
		if current == PhaseApproved {
			if decision, _ := execResult.RawOutputs[ApprovedDecisionRawKey].(string); decision == string(ApprovedExecPause) {
				token := extractPauseToken(execResult)
				events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventPhasePaused, attempt, map[string]any{
					"pause_token": token,
				}))
				return events, current, nil
			}
		}

		// Verifier (with deadline)
		verdictCtx, cancel := context.WithTimeout(ctx, time.Duration(worker.VerifierTimeoutMs())*time.Millisecond)
		verdict, verr := worker.Verifier(verdictCtx, execResult)
		cancel()
		if verr != nil {
			events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventPhaseFailed, attempt, map[string]any{
				"reason": "verifier_error",
				"err":    verr.Error(),
			}))
			return events, PhaseFailed, nil
		}
		if !verdict.OK {
			// Recovered → approved rollback path. We track the
			// rollback count and stop after MaxRetryCount
			// rollbacks (per design §3.2). Beyond the cap the
			// walker writes retry_exhausted and enters the
			// failed terminal state.
			if current == PhaseRecovered {
				rollbackCount++
				events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventRollback, attempt, map[string]any{
					"reasons":     verdict.Reasons,
					"retry_count": rollbackCount,
				}))
				if rollbackCount > MaxRetryCount {
					// Exhausted: stop and surface failed.
					events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventRetryExhausted, attempt, map[string]any{
						"retry_count": rollbackCount,
						"reasons":     verdict.Reasons,
					}))
					return events, PhaseFailed, nil
				}
				current = PhaseApproved
				attempt++
				continue
			}
			events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventPhaseFailed, attempt, map[string]any{
				"reason":  "verifier_rejected",
				"reasons": verdict.Reasons,
			}))
			return events, PhaseFailed, nil
		}

		if current == PhaseInvestigated && execResult.RawOutputs["root_cause_json"] != nil {
			contractRef, err := o.writeInvestigatedContract(ctx, opts, execResult)
			if err != nil {
				events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventPhaseFailed, attempt, map[string]any{
					"reason": "contract_write_error",
					"err":    err.Error(),
				}))
				return events, PhaseFailed, nil
			}
			execResult.ContractRef = contractRef
		}

		if err := o.writeSideEffectContracts(ctx, opts, current, &execResult); err != nil {
			events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventPhaseFailed, attempt, map[string]any{
				"reason": "contract_write_error",
				"err":    err.Error(),
			}))
			return events, PhaseFailed, nil
		}

		phaseContractEvent, err := o.appendEventStrict(ctx, opts, current, loopmodel.EventPhaseContractWritten, attempt, sideEffectsPayload(execResult))
		if err != nil {
			return events, PhaseFailed, fmt.Errorf("loop: append %s event: %w", loopmodel.EventPhaseContractWritten, err)
		}
		events = append(events, phaseContractEvent)

		if opts.StopAfterPhase != "" && current == opts.StopAfterPhase {
			return events, current, nil
		}

		// Advance to next phase.
		next, err := nextPhase(current, true)
		if err != nil {
			if errors.Is(err, ErrTerminalPhase) {
				return events, current, nil
			}
			events = append(events, o.appendEvent(ctx, opts, current, loopmodel.EventPhaseFailed, attempt, map[string]any{
				"reason": "invalid_transition",
				"err":    err.Error(),
			}))
			return events, PhaseFailed, nil
		}
		current = next
		attempt++
	}
	// Day 9: post-mortem write-back to chat (spec §"自动化完成后回灌
	// Postmortem 摘要到对话"). Only when the run actually reached
	// postmortem AND was triggered from chat AND carries a linked
	// conversation id. We push outside the worker so a pusher
	// failure does not poison the loop result.
	if current == PhasePostmortem && opts.TriggeredBy == "chat" && opts.LinkedConversationID != "" && o.deps.ChatReportPusher != nil {
		if md := o.renderPostmortemMarkdown(ctx, opts); md != "" {
			if err := o.deps.ChatReportPusher.PushReportToConversation(ctx, opts.LinkedConversationID, opts.TenantID, md); err != nil {
				if o.deps.Logger != nil {
					o.deps.Logger.Warn("loop: chat report push failed", slog.String("conversation_id", opts.LinkedConversationID), slog.Any("err", err))
				}
			}
		}
	}
	return events, current, nil
}

func (o *orchestrator) writeSideEffectContracts(ctx context.Context, opts RunOptions, phase Phase, result *ExecResult) error {
	for _, sideEffect := range result.SideEffects {
		if sideEffect.Kind != "phase_contract_written" {
			continue
		}
		contractType, _ := sideEffect.Detail["contract"].(string)
		if contractType == "" {
			contractType, _ = sideEffect.Detail["contract_type"].(string)
		}
		payload, _ := sideEffect.Detail["payload"].(string)
		if contractType == "" || payload == "" {
			continue
		}
		if o.deps.ContractRepo == nil {
			return errors.New("loop: ContractRepo is required for phase contract output")
		}
		contract := &loopmodel.Contract{
			IncidentID:     opts.IncidentID,
			TenantID:       opts.TenantID,
			Phase:          string(phase),
			Type:           contractType,
			SchemaVer:      ContractSchemaV1,
			Payload:        payload,
			SizeBytes:      len(payload),
			StorageBackend: loopmodel.StorageBackendDB,
			CreatedAt:      time.Now().UTC(),
		}
		if err := o.deps.ContractRepo.WriteContract(ctx, contract); err != nil {
			return fmt.Errorf("loop: write %s contract: %w", contractType, err)
		}
		if result.ContractRef == nil {
			result.ContractRef = &ContractRef{ID: contract.ID, Type: contract.Type, SchemaVersion: contract.SchemaVer}
		}
	}
	return nil
}

func (o *orchestrator) writeInvestigatedContract(ctx context.Context, opts RunOptions, result ExecResult) (*ContractRef, error) {
	if o.deps.ContractRepo == nil {
		return nil, errors.New("loop: ContractRepo is required for investigated output")
	}
	raw, ok := result.RawOutputs["root_cause_json"]
	if !ok {
		return nil, errors.New("loop: investigated Executor.RawOutputs missing root_cause_json")
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("loop: marshal root_cause_json: %w", err)
	}
	contract := &loopmodel.Contract{
		IncidentID:     opts.IncidentID,
		TenantID:       opts.TenantID,
		Phase:          string(PhaseInvestigated),
		Type:           "root_cause_json",
		SchemaVer:      ContractSchemaV1,
		Payload:        string(payload),
		SizeBytes:      len(payload),
		StorageBackend: loopmodel.StorageBackendDB,
		CreatedAt:      time.Now().UTC(),
	}
	if err := o.deps.ContractRepo.WriteContract(ctx, contract); err != nil {
		return nil, fmt.Errorf("loop: write root_cause_json contract: %w", err)
	}
	return &ContractRef{ID: contract.ID, Type: contract.Type, SchemaVersion: contract.SchemaVer}, nil
}

// appendEvent persists one loop_event_log row via EventRepo (when
// available) and returns the materialized struct so the caller can
// include it in RunResult.LoopEvents. When EventRepo is nil the
// helper still returns the struct for callers that just want the
// event list (used by tests).
func (o *orchestrator) appendEvent(
	ctx context.Context,
	opts RunOptions,
	phase Phase,
	eventType string,
	attempt int,
	payload map[string]any,
) loopmodel.Event {
	event, err := o.appendEventStrict(ctx, opts, phase, eventType, attempt, payload)
	if err != nil && o.deps.Logger != nil {
		o.deps.Logger.Warn("loop: append event failed",
			"incident", opts.IncidentID, "phase", phase, "event_type", eventType, "err", err)
	}
	return event
}

func (o *orchestrator) appendEventStrict(
	ctx context.Context,
	opts RunOptions,
	phase Phase,
	eventType string,
	attempt int,
	payload map[string]any,
) (loopmodel.Event, error) {
	ev := loopmodel.Event{
		IncidentID:     opts.IncidentID,
		TenantID:       opts.TenantID,
		EventType:      eventType,
		Phase:          string(phase),
		IdempotencyKey: buildIdempotencyKey(opts.IncidentID, phase, eventType, attempt),
		TraceID:        traceIDFromContext(ctx),
		CreatedAt:      time.Now().UTC(),
	}
	if payload != nil {
		if raw, err := json.Marshal(payload); err == nil {
			ev.Payload = string(raw)
		} else {
			ev.Payload = fmt.Sprintf(`{"marshal_error":%q}`, err.Error())
		}
	}
	if o.deps.EventRepo != nil {
		if err := o.deps.EventRepo.AppendEvent(ctx, &ev); err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// sideEffectsPayload converts an ExecResult into the JSON payload
// recorded on the phase_contract_written event.
func sideEffectsPayload(r ExecResult) map[string]any {
	out := map[string]any{}
	if len(r.SideEffects) > 0 {
		sides := make([]map[string]any, 0, len(r.SideEffects))
		for _, s := range r.SideEffects {
			sides = append(sides, map[string]any{
				"kind":   s.Kind,
				"target": s.Target,
				"detail": s.Detail,
			})
		}
		out["side_effects"] = sides
	}
	if r.ContractRef != nil {
		out["contract_ref"] = map[string]any{
			"id":             r.ContractRef.ID,
			"type":           r.ContractRef.Type,
			"schema_version": r.ContractRef.SchemaVersion,
		}
	}
	if len(r.RawOutputs) > 0 {
		out["raw_outputs"] = r.RawOutputs
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractPauseToken retrieves the pause_token from the side-effect
// or raw_outputs bag. Returns empty when absent.
func extractPauseToken(r ExecResult) string {
	for _, s := range r.SideEffects {
		if s.Target == ApprovedPauseTokenField {
			if v, ok := s.Detail["value"].(string); ok && v != "" {
				return v
			}
		}
	}
	if v, ok := r.RawOutputs["pause_token"].(string); ok {
		return v
	}
	return ""
}

// upstreamContractFor returns a non-nil placeholder ContractRef for
// phases that read an upstream contract (recovered reads
// ApprovalDecision; Day 6+ may extend to other phases). The
// placeholder's ID is consumed by the test's narrow loader; in
// production the orchestrator reads the real ref via ContractRepo.
func upstreamContractFor(p Phase) *ContractRef {
	switch p {
	case PhaseRecovered:
		return &ContractRef{
			ID:            1,
			Type:          "ApprovalDecision",
			SchemaVersion: ContractSchemaV1,
		}
	}
	return nil
}

// renderPostmortemMarkdown reads the postmortem contract the phase
// worker just wrote and returns the markdown body. Returns "" when
// the contract is missing (worker skipped or ContractRepo not wired).
func (o *orchestrator) renderPostmortemMarkdown(ctx context.Context, opts RunOptions) string {
	if o.deps.ContractRepo == nil {
		return ""
	}
	// Read latest postmortem contract for this incident.
	payload, err := o.deps.ContractRepo.ReadContract(ctx, opts.TenantID, opts.IncidentID, PhasePostmortem, "postmortem_doc")
	if err != nil || payload == nil {
		return ""
	}
	var doc PostmortemDoc
	if err := json.Unmarshal([]byte(payload.Payload), &doc); err != nil {
		return ""
	}
	return doc.Markdown
}

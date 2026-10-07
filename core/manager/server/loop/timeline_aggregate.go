// Package loop — timeline_aggregate.go
//
// Day 11: trace aggregation by incident ID. The closed-loop
// timeline view consumed by ClosedLoopTimeline.tsx expected a JSON
// shape with `phases` and `rubric`, but the timeline endpoint was
// only returning the raw event log. This file bridges that gap:
//
//   - BuildTimelinePhases collapses an ordered event slice into the
//     7-stage phase list the SPA renders, attaching per-phase tool
//     calls, approval/execution/validation evidence, agent identity,
//     and skill versions.
//   - BuildRubric computes the four spec-defined metrics
//     (rca_accuracy / time_to_remediate / approval_rate /
//     recovery_pass_rate) so the SPA does not need to recompute them.
//
// All computation is pure (no DB I/O) so tests cover the contract
// without a database. The handler (http.go::timeline) calls these
// helpers after ReadEvents; the wire format is backwards-compatible
// (the existing `events` field is still returned).
package loop

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	loopbiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	loopmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/loop"
)

// forwardPhaseOrder is the canonical 7-phase list the timeline view
// renders in stable order. It must match allForwardPhases in
// core/manager/biz/loop/orchestrator.go so phase_entered event
// rows render against the same phase names. The string values come
// from loopbiz.Phase constants so the wire format stays in sync
// with the orchestrator.
var forwardPhaseOrder = []string{
	string(loopbiz.PhaseDetected),
	string(loopbiz.PhaseCorrelated),
	string(loopbiz.PhaseInvestigated),
	string(loopbiz.PhaseCritiqued),
	string(loopbiz.PhaseApproved),
	string(loopbiz.PhaseRecovered),
	string(loopbiz.PhasePostmortem),
}

// terminalPhases lists the loop states the timeline view treats as
// a final state rather than a forward step. These are the only
// values the orchestrator can leave the loop in after a terminal
// transition.
var terminalPhases = map[string]bool{
	"failed":  true,
	"aborted": true,
}

// skillVersions encodes the per-Worker skill version the
// opskeeper-teamharness plugin ships in this build. The numbers are
// sourced from plugins/opskeeper-teamharness/skills/agent/*/skill_meta.yaml
// and the dashboard plugin.json. Surfacing them in the timeline view
// is what lets demo reviewers verify which skill set
// actually handled each phase (rather than guessing from prompts).
var skillVersions = map[string]string{
	"opskeeper-alerter":      "1.0.0",
	"opskeeper-investigator": "1.1.0",
	"opskeeper-critic":       "1.0.0",
	"opskeeper-reviewer":     "1.0.0",
	"opskeeper-repairer":     "1.0.0",
	"opskeeper-verifier":     "1.0.0",
	"opskeeper-postmortem":   "1.0.0",
}

// agentRolesByPhase maps a phase name to the Worker role the
// opskeeper-coordination SKILL declares is responsible for that
// phase. The same map drives both the actor-role default and the
// approval-binding audit row.
var agentRolesByPhase = map[string]string{
	string(loopbiz.PhaseDetected):     "opskeeper-alerter",
	string(loopbiz.PhaseCorrelated):   "opskeeper-investigator",
	string(loopbiz.PhaseInvestigated): "opskeeper-investigator",
	string(loopbiz.PhaseCritiqued):    "opskeeper-critic",
	string(loopbiz.PhaseApproved):     "opskeeper-reviewer",
	string(loopbiz.PhaseRecovered):    "opskeeper-repairer",
	string(loopbiz.PhasePostmortem):   "opskeeper-postmortem",
}

// phaseToPhaseLabel maps the 7 internal phase names to the display
// labels the Element side panel uses. Keeping this here (vs. in the
// SPA) means the same string is used by admin audit exports and
// by the human-readable EvidenceChain.
var phaseToPhaseLabel = map[string]string{
	string(loopbiz.PhaseDetected):     "detect",
	string(loopbiz.PhaseCorrelated):   "correlate",
	string(loopbiz.PhaseInvestigated): "diagnose",
	string(loopbiz.PhaseCritiqued):    "critique",
	string(loopbiz.PhaseApproved):     "approve",
	string(loopbiz.PhaseRecovered):    "act",
	string(loopbiz.PhasePostmortem):   "report",
}

// TimelinePhase is the wire shape ProcessTimeline.tsx renders. The
// fields match the existing TimelinePhase interface in the SPA so
// the new server-side aggregation is a drop-in for the existing
// render path.
type TimelinePhase struct {
	Phase            string             `json:"phase"`
	PhaseLabel       string             `json:"phase_label"`
	Status           string             `json:"status"`
	Duration         string             `json:"duration,omitempty"`
	StartedAt        string             `json:"started_at,omitempty"`
	EndedAt          string             `json:"ended_at,omitempty"`
	ContractSummary  string             `json:"contract_summary,omitempty"`
	ContractDetail   map[string]any     `json:"contract_detail,omitempty"`
	ToolCalls        []TimelineToolCall `json:"tool_calls,omitempty"`
	Audit            []TimelineAuditRow `json:"audit,omitempty"`
	WorkerRole       string             `json:"worker_role,omitempty"`
	WorkerSkillVer   string             `json:"worker_skill_version,omitempty"`
	ContextKnowledge []string           `json:"context_knowledge,omitempty"`
	SubPhases        []TimelineSubPhase `json:"sub_phases,omitempty"`
}

// TimelineToolCall is one MCP / tool invocation recorded by a Worker
// during the phase. The renderer shows it in the expanded view; the
// audit row also links to the matching entry.
type TimelineToolCall struct {
	Name   string `json:"name"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	// Argv is the literal vector the tool executed, when it ran one. It is
	// the field a reviewer reads to see exactly what the platform did, and
	// the field the crystalliser promotes into a declaration; Args is the
	// resolved argument bag, which is a different answer.
	Argv        []string `json:"argv,omitempty"`
	Status      string   `json:"status"`
	LatencyMs   int64    `json:"latency_ms,omitempty"`
	Actor       string   `json:"actor,omitempty"`
	SkillVer    string   `json:"skill_version,omitempty"`
	EvidenceRef string   `json:"evidence_ref,omitempty"`
}

// TimelineAuditRow is one decision-point row the timeline shows
// below the tool calls: dispatch / approval / execution /
// verification. The router decodes the row's SourceEventType to
// pick the chip colour.
type TimelineAuditRow struct {
	Kind          string `json:"kind"` // dispatch / approval / execution / verification / close
	Actor         string `json:"actor,omitempty"`
	ActorRole     string `json:"actor_role,omitempty"`
	BoundTarget   string `json:"bound_target,omitempty"`
	BoundParams   string `json:"bound_params,omitempty"`
	BoundScope    string `json:"bound_scope,omitempty"`
	Action        string `json:"action,omitempty"`
	Fallback      string `json:"fallback,omitempty"`
	FallbackCause string `json:"fallback_cause,omitempty"`
	At            string `json:"at,omitempty"`
	TraceID       string `json:"trace_id,omitempty"`
	EvidenceRef   string `json:"evidence_ref,omitempty"`
	Note          string `json:"note,omitempty"`
}

// TimelineSubPhase is a sub-step within a phase (e.g. critic's
// replay / depth / consistency). The renderer shows them as chips
// inline next to the phase name.
type TimelineSubPhase struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// TimelineRubric is the four-metric summary card at the top of
// ClosedLoopTimeline. The numbers come from the EventRepo contents
// alone (no DB joins), keeping the helper pure.
type TimelineRubric struct {
	RCAAccuracy       float64 `json:"rca_accuracy"`
	TimeToRemediate   string  `json:"time_to_remediate"`
	ApprovalRate      float64 `json:"approval_rate"`
	RecoveryPassRate  float64 `json:"recovery_pass_rate"`
	PhaseCount        int     `json:"phase_count"`
	EventCount        int     `json:"event_count"`
	HasRecoverySignal bool    `json:"has_recovery_signal"`
	HasClosure        bool    `json:"has_closure"`
}

// BuildTimelinePhases converts a chronological event slice into the
// TimelinePhase list. The function is intentionally tolerant: it
// always returns one entry per forward phase (filling pending
// placeholders for phases with no events) so the renderer never has
// to special-case "no data yet".
//
// The events slice MUST be sorted ascending by CreatedAt (the same
// order EventRepo.ReadEvents returns).
func BuildTimelinePhases(events []loopmodel.Event) []TimelinePhase {
	// Index events by phase name for stable order. The first
	// phase_entered marks the start; the matching phase_contract_written
	// marks the end. Anything in between is a sub-event.
	byPhase := make(map[string][]loopmodel.Event, len(forwardPhaseOrder))
	for _, ev := range events {
		byPhase[ev.Phase] = append(byPhase[ev.Phase], ev)
	}

	out := make([]TimelinePhase, 0, len(forwardPhaseOrder)+1)
	for _, phaseName := range forwardPhaseOrder {
		phase := buildPhase(phaseName, byPhase[phaseName])
		out = append(out, phase)
	}

	// If the latest event landed in a terminal phase (failed / aborted),
	// surface that as a trailing summary row so the renderer shows
	// "loop closed by failure" instead of a misleading "postmortem pending".
	if len(events) > 0 {
		latest := events[len(events)-1]
		if terminalPhases[latest.Phase] {
			out = append(out, TimelinePhase{
				Phase:      latest.Phase,
				PhaseLabel: latest.Phase,
				Status:     "failed",
				EndedAt:    latest.CreatedAt.UTC().Format(time.RFC3339Nano),
				ContractSummary: fmt.Sprintf(
					"loop entered terminal state %s; see event log for failure cause",
					latest.Phase,
				),
			})
		}
	}
	return out
}

// buildPhase assembles one TimelinePhase entry from the events that
// belong to that phase. When no events exist for a forward phase
// the function returns a `pending` placeholder so the renderer can
// still draw the row.
func buildPhase(phaseName string, events []loopmodel.Event) TimelinePhase {
	role := agentRolesByPhase[phaseName]
	phase := TimelinePhase{
		Phase:          phaseName,
		PhaseLabel:     phaseToPhaseLabel[phaseName],
		Status:         "pending",
		WorkerRole:     role,
		WorkerSkillVer: skillVersions[role],
	}

	if len(events) == 0 {
		return phase
	}

	// Walk the per-phase events. The first phase_entered starts the
	// clock; the first phase_contract_written closes it and yields the
	// contract summary; phase_failed / paused mark the failure.
	var startedAt, endedAt time.Time
	var toolCalls []TimelineToolCall
	var auditRows []TimelineAuditRow
	var knowledgeRefs []string

	for _, ev := range events {
		switch ev.EventType {
		case loopmodel.EventTypePhaseEntered:
			if startedAt.IsZero() {
				startedAt = ev.CreatedAt
				phase.StartedAt = startedAt.UTC().Format(time.RFC3339Nano)
			}
		case loopmodel.EventPhaseContractWritten:
			endedAt = ev.CreatedAt
			phase.EndedAt = endedAt.UTC().Format(time.RFC3339Nano)
			phase.Status = "success"
			summary, detail := parseContractPayload(ev.Payload)
			phase.ContractSummary = summary
			phase.ContractDetail = detail
		case loopmodel.EventPhaseFailed:
			endedAt = ev.CreatedAt
			phase.EndedAt = endedAt.UTC().Format(time.RFC3339Nano)
			phase.Status = "failed"
			phase.ContractSummary = phaseFailureSummary(ev.Payload)
		case loopmodel.EventPhasePaused:
			phase.Status = "running"
			phase.ContractSummary = "paused: " + phaseFailureSummary(ev.Payload)
		case loopmodel.EventPhaseResumed:
			phase.Status = "running"
		case loopmodel.EventRollback:
			phase.Status = "skipped"
			phase.ContractSummary = "rollback recorded: " + phaseFailureSummary(ev.Payload)
		case loopmodel.EventRetryExhausted:
			phase.Status = "failed"
			phase.ContractSummary = "retry budget exhausted"
		case loopmodel.EventCorrection:
			phase.ContractSummary = appendIfMissing(phase.ContractSummary, "correction event recorded")
		}

		// Mine tool calls + audit rows from the payload. Both the
		// per-phase events and the agent_teams incident.record entries
		// can carry this information; we surface whatever the event
		// contains so the renderer doesn't have to dual-source.
		if tc, ok := parseToolCall(ev.Payload, role); ok {
			toolCalls = append(toolCalls, tc)
		}
		if row, ok := parseAuditRow(ev.Payload, phaseName, ev); ok {
			auditRows = append(auditRows, row)
		}
		for _, tc := range parseToolReplay(ev.Payload, role) {
			toolCalls = append(toolCalls, tc)
		}
		for _, ref := range parseKnowledgeRefs(ev.Payload) {
			knowledgeRefs = append(knowledgeRefs, ref)
		}
	}

	// Sub-phases for the critiqued phase. The critic SKILL declares
	// three sub-checks: replay / depth / consistency; we surface
	// them as chips when the payload names them.
	if phaseName == string(loopbiz.PhaseCritiqued) {
		phase.SubPhases = parseCritiqueSubPhases(events)
	}

	if !startedAt.IsZero() && !endedAt.IsZero() {
		phase.Duration = formatDuration(endedAt.Sub(startedAt))
	} else if !startedAt.IsZero() && phase.Status == "running" {
		phase.Duration = formatDuration(time.Since(startedAt))
	}

	// Last-known sub-step becomes the row's toolCalls even when
	// no phase_contract_written was recorded yet (e.g. phase paused).
	if len(toolCalls) > 0 {
		phase.ToolCalls = toolCalls
	}
	if len(auditRows) > 0 {
		phase.Audit = auditRows
	}
	if len(knowledgeRefs) > 0 {
		phase.ContextKnowledge = dedupeStrings(knowledgeRefs)
	}
	return phase
}

// parseContractPayload extracts the summary string + JSON detail
// object from a contract-written payload. Unknown / empty payloads
// return a best-effort summary so the renderer still has something
// to show.
func parseContractPayload(raw string) (string, map[string]any) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return "", nil
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		// Non-JSON: surface as a raw summary.
		return truncateForSummary(raw), nil
	}
	// Prefer a "summary" / "root_cause_object.summary" field for the
	// single-line summary; fall back to a type-based heuristic.
	if s, ok := detail["summary"].(string); ok && s != "" {
		return s, detail
	}
	if rco, ok := detail["root_cause_object"].(map[string]any); ok {
		if s, ok := rco["summary"].(string); ok && s != "" {
			return s, detail
		}
	}
	if s, ok := detail["passed"].(bool); ok {
		return fmt.Sprintf("verified=%v", s), detail
	}
	return truncateForSummary(raw), detail
}

// phaseFailureSummary renders a short, human-readable summary of a
// phase_failed payload. Falls back to "phase failed" when the
// payload is empty.
func phaseFailureSummary(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "phase failed"
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return truncateForSummary(raw)
	}
	if s, ok := detail["error"].(string); ok && s != "" {
		return "phase failed: " + s
	}
	if s, ok := detail["reason"].(string); ok && s != "" {
		return "phase failed: " + s
	}
	return "phase failed"
}

// parseToolReplay extracts the tool calls an execute wrote into the
// event's "tool_replay" array. The orchestrator records this on the
// phase_contract_written event (the success path) as well as on
// phase_failed (the failure path), so reading only phase_failed — which
// is what the payload shape used to imply — would hide every action a
// *successful* run took.
//
// The array entries carry the same fields the agent-authored tool call
// path uses (name/args/result/status/latency_ms), so both sources land
// in the same TimelineToolCall shape and the renderer stays single-source.
// Unknown / malformed payloads return nil rather than a zero entry.
func parseToolReplay(raw, role string) []TimelineToolCall {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil
	}
	var detail struct {
		ToolReplay []struct {
			Name       string   `json:"Name"`
			ArgsJSON   string   `json:"ArgsJSON"`
			ResultJSON string   `json:"ResultJSON"`
			Argv       []string `json:"Argv"`
			Status     string   `json:"Status"`
			LatencyMs  int64    `json:"LatencyMs"`
		} `json:"tool_replay"`
	}
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return nil
	}
	out := make([]TimelineToolCall, 0, len(detail.ToolReplay))
	for _, r := range detail.ToolReplay {
		if r.Name == "" {
			continue
		}
		status := r.Status
		if status == "" {
			status = "success"
		}
		out = append(out, TimelineToolCall{
			Name:      r.Name,
			Args:      truncateForSummary(r.ArgsJSON),
			Result:    truncateForSummary(r.ResultJSON),
			Argv:      append([]string(nil), r.Argv...),
			Status:    status,
			LatencyMs: r.LatencyMs,
			Actor:     role,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseToolCall extracts a TimelineToolCall from the event payload
// when the event encodes a tool invocation. The function returns
// ok=false when the payload does not name a tool so the caller
// can leave the phase's toolCalls slice untouched.
func parseToolCall(raw, role string) (TimelineToolCall, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return TimelineToolCall{}, false
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return TimelineToolCall{}, false
	}
	toolName, _ := detail["tool"].(string)
	if toolName == "" {
		toolName, _ = detail["name"].(string)
	}
	if toolName == "" {
		return TimelineToolCall{}, false
	}
	args, _ := detail["args"].(string)
	if args == "" {
		if rawArgs, ok := detail["args"].(map[string]any); ok {
			b, _ := json.Marshal(rawArgs)
			args = string(b)
		}
	}
	result, _ := detail["result"].(string)
	status, _ := detail["status"].(string)
	if status == "" {
		status = "success"
	}
	var latency int64
	switch v := detail["latency_ms"].(type) {
	case float64:
		latency = int64(v)
	case int64:
		latency = v
	}
	actor, _ := detail["actor"].(string)
	if actor == "" {
		actor = role
	}
	ev, _ := detail["evidence_ref"].(string)
	return TimelineToolCall{
		Name:        toolName,
		Args:        truncateForSummary(args),
		Result:      truncateForSummary(result),
		Status:      status,
		LatencyMs:   latency,
		Actor:       actor,
		SkillVer:    skillVersions[role],
		EvidenceRef: ev,
	}, true
}

// approvedBoundTargetPrefixes is the whitelist of allowed
// `bound_target` prefixes for approval-kind audit rows. Anything
// that does not match this set is flagged as a reject so the
// Element timeline surfaces "wrong target" rather than silently
// rendering an out-of-policy approval. The set is intentionally
// narrow — only resource / scope identifiers the review pipeline
// knows how to bind. Adding a new prefix is an explicit policy
// decision; do not relax without the docs team's review.
var approvedBoundTargetPrefixes = []string{
	"pg:",
	"incident:",
	"opskeeper:",
	"app:",
	"host:",
}

// approvedBoundTargetAllowed returns true when target matches one
// of the approved prefixes. Empty / blank targets are NOT
// considered allowed — the audit row must carry an explicit
// target so the reviewer can verify the binding.
func approvedBoundTargetAllowed(target string) bool {
	t := strings.TrimSpace(target)
	if t == "" {
		return false
	}
	for _, p := range approvedBoundTargetPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// auditKindRejectsBoundTarget reports whether the audit kind is
// the kind that must be bound to a whitelisted target. Dispatch /
// execution rows carry their own bound targets but the on-error
// reason is the same: surface a reject annotation when the target
// is out of policy.
func auditKindRejectsBoundTarget(kind string) bool {
	switch kind {
	case "approval", "execution", "dispatch":
		return true
	}
	return false
}

// parseAuditRow converts an event payload into one TimelineAuditRow
// when the event represents a decision point (dispatch / approval /
// execution / verification). The function is conservative: it
// returns ok=false when the payload doesn't carry an audit
// discriminator, so empty payloads never produce empty audit rows.
//
// The bound_target / fallback / fallback_cause fields are validated
// here so the timeline view can show "wrong target" / "missing
// fallback cause" instead of a silently bad audit row:
//
//   - On approval / execution / dispatch, an empty or off-whitelist
//     bound_target is rewritten to "<original>:REJECTED:<reason>"
//     and the FallbackCause slot carries the reason. The original
//     target stays visible for audit replay.
//   - On any row, when fallback is set to a non-empty value
//     fallback_cause must also be non-empty (and vice versa);
//     the missing slot is filled with "<unset>:<kind>" so the
//     reviewer can see the inconsistency.
func parseAuditRow(raw, phaseName string, ev loopmodel.Event) (TimelineAuditRow, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return TimelineAuditRow{}, false
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return TimelineAuditRow{}, false
	}
	kind, _ := detail["audit_kind"].(string)
	if kind == "" {
		// Fall back to event_type → audit_kind mapping.
		switch ev.EventType {
		case loopmodel.EventTypePhaseEntered:
			kind = "dispatch"
		case loopmodel.EventPhaseContractWritten:
			switch phaseName {
			case string(loopbiz.PhaseApproved):
				kind = "approval"
			case string(loopbiz.PhaseRecovered):
				kind = "execution"
			case string(loopbiz.PhasePostmortem):
				kind = "close"
			default:
				kind = "verification"
			}
		case loopmodel.EventRollback:
			kind = "rollback"
		case loopmodel.EventPhaseFailed:
			kind = "failure"
		}
	}
	if kind == "" {
		return TimelineAuditRow{}, false
	}
	row := TimelineAuditRow{
		Kind:          kind,
		BoundTarget:   stringOr(detail, "bound_target"),
		BoundParams:   stringOr(detail, "bound_params"),
		BoundScope:    stringOr(detail, "bound_scope"),
		Action:        stringOr(detail, "action"),
		Fallback:      stringOr(detail, "fallback"),
		FallbackCause: stringOr(detail, "fallback_cause"),
		Actor:         stringOr(detail, "actor"),
		ActorRole:     stringOr(detail, "actor_role"),
		EvidenceRef:   stringOr(detail, "evidence_ref"),
		Note:          stringOr(detail, "note"),
		TraceID:       ev.TraceID,
		At:            ev.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if row.ActorRole == "" {
		row.ActorRole = agentRolesByPhase[phaseName]
	}
	if row.Actor == "" {
		row.Actor = row.ActorRole
	}

	// bound_target whitelist enforcement for the kinds that must
	// bind to a known resource. Empty target → reject with
	// "missing" reason; off-whitelist → reject with "wrong_target"
	// reason. The original target is preserved verbatim so the
	// audit replay can show what was proposed vs what was accepted.
	if auditKindRejectsBoundTarget(kind) && !approvedBoundTargetAllowed(row.BoundTarget) {
		original := row.BoundTarget
		reason := "wrong_target"
		if strings.TrimSpace(original) == "" {
			reason = "missing"
		}
		row.BoundTarget = original + ":REJECTED:" + reason
		if strings.TrimSpace(row.FallbackCause) == "" {
			row.FallbackCause = "bound_target_rejected:" + reason
		} else {
			row.FallbackCause = row.FallbackCause + ";bound_target_rejected:" + reason
		}
	}

	// fallback / fallback_cause pair validation. The contract
	// requires both halves to be present together (you cannot
	// name a fallback without naming the cause, and vice versa).
	// An unset half is filled with an "<unset>:<kind>" marker so
	// the reviewer can see the inconsistency instead of the row
	// silently looking well-formed.
	fallback := strings.TrimSpace(row.Fallback)
	cause := strings.TrimSpace(row.FallbackCause)
	if fallback != "" && cause == "" {
		row.FallbackCause = "<unset>:fallback_without_cause"
	}
	if cause != "" && fallback == "" {
		row.Fallback = "<unset>:cause_without_fallback"
	}

	return row, true
}

// parseKnowledgeRefs extracts any knowledge-vault references the
// payload names. Used to surface the closed-loop "context +
// knowledge" requirement in the timeline view.
func parseKnowledgeRefs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return nil
	}
	out := []string{}
	if v, ok := detail["knowledge_refs"].([]any); ok {
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	if v, ok := detail["knowledge_ref"].(string); ok && v != "" {
		out = append(out, v)
	}
	return out
}

// parseCritiqueSubPhases surfaces the three sub-checks the
// opskeeper-critic SKILL performs: replay / depth / consistency.
// The chips appear inline next to the phase name in the renderer.
func parseCritiqueSubPhases(events []loopmodel.Event) []TimelineSubPhase {
	statusByName := map[string]string{
		"replay":      "pending",
		"depth":       "pending",
		"consistency": "pending",
	}
	// Use the most recent contract payload (or final event payload)
	// to discover which sub-checks actually ran.
	for i := len(events) - 1; i >= 0; i-- {
		raw := strings.TrimSpace(events[i].Payload)
		if raw == "" {
			continue
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			continue
		}
		for name := range statusByName {
			if s, ok := detail[name].(string); ok {
				statusByName[name] = s
			}
		}
		// Only the most recent payload informs the chip state.
		break
	}
	out := make([]TimelineSubPhase, 0, len(statusByName))
	for _, name := range []string{"replay", "depth", "consistency"} {
		out = append(out, TimelineSubPhase{Name: name, Status: statusByName[name]})
	}
	return out
}

// BuildRubric computes the four-metric summary card from the raw
// event slice. All four metrics are defined in the
// loop-harness-rubric spec:
//
//   - rca_accuracy:    ratio of phases that wrote a non-empty
//     RootCauseJSON contract to total forward phases.
//   - time_to_remediate: wall-clock between the first detected event
//     and the recovered-phase contract write.
//   - approval_rate:   ratio of approved-phase events to total
//     approval attempts (1.0 when no human approval
//     was needed).
//   - recovery_pass_rate: ratio of phases whose VerifiedDelta.passed
//     is true to total recovery attempts.
//
// Empty / partial event sets return zero-value metrics with a
// pending status; the SPA renders "—" rather than "0.00" in that case.
func BuildRubric(events []loopmodel.Event) TimelineRubric {
	rubric := TimelineRubric{
		RCAAccuracy:      0,
		TimeToRemediate:  "—",
		ApprovalRate:     0,
		RecoveryPassRate: 0,
		PhaseCount:       len(forwardPhaseOrder),
		EventCount:       len(events),
	}

	if len(events) == 0 {
		return rubric
	}

	// rca_accuracy: count phases that wrote a non-empty contract.
	written := 0
	for _, phaseName := range forwardPhaseOrder {
		for _, ev := range events {
			if ev.Phase != phaseName {
				continue
			}
			if ev.EventType == loopmodel.EventPhaseContractWritten && strings.TrimSpace(ev.Payload) != "" && strings.TrimSpace(ev.Payload) != "{}" {
				written++
				break
			}
		}
	}
	rubric.RCAAccuracy = float64(written) / float64(len(forwardPhaseOrder))

	// time_to_remediate: first detected to recovered-contract.
	var firstDetected, recoveredAt time.Time
	for _, ev := range events {
		switch {
		case ev.EventType == loopmodel.EventTypePhaseEntered && ev.Phase == string(loopbiz.PhaseDetected) && firstDetected.IsZero():
			firstDetected = ev.CreatedAt
		case ev.EventType == loopmodel.EventPhaseContractWritten && ev.Phase == string(loopbiz.PhaseRecovered) && recoveredAt.IsZero():
			recoveredAt = ev.CreatedAt
		}
	}
	if !firstDetected.IsZero() && !recoveredAt.IsZero() && recoveredAt.After(firstDetected) {
		rubric.TimeToRemediate = formatDuration(recoveredAt.Sub(firstDetected))
	}

	// approval_rate: count approved-phase events vs total approval
	// attempts. We treat any phase_entered on approved as an
	// attempt; phase_contract_written on approved as a success.
	approvalAttempts := 0
	approvalSuccesses := 0
	for _, ev := range events {
		if ev.Phase != string(loopbiz.PhaseApproved) {
			continue
		}
		switch ev.EventType {
		case loopmodel.EventTypePhaseEntered:
			approvalAttempts++
		case loopmodel.EventPhaseContractWritten:
			approvalSuccesses++
		}
	}
	if approvalAttempts == 0 {
		rubric.ApprovalRate = 1.0 // no human approval needed → trivially passing
	} else {
		rubric.ApprovalRate = float64(approvalSuccesses) / float64(approvalAttempts)
	}

	// recovery_pass_rate: weighted by VerifiedDelta.sample_size so a
	// 60s observation window that produced 60 observations
	// contributes 60 samples to the denominator instead of just 1.
	// The VerifyRecovery worker emits one contract per observation
	// window (typically sample_size >= 3 per design §D5), and the
	// rubric must reflect the multi-sample reality rather than
	// counting each contract as a single +/-1 boolean. When a
	// contract omits sample_size we fall back to counting it as one
	// attempt so the metric stays defined for legacy/partial event
	// logs.
	recoveryAttempts := 0
	recoveryPasses := 0
	for _, ev := range events {
		if ev.Phase != string(loopbiz.PhaseRecovered) {
			continue
		}
		if ev.EventType != loopmodel.EventPhaseContractWritten {
			continue
		}
		weight, hasWeight := verifiedSampleSize(ev.Payload)
		if !hasWeight || weight <= 0 {
			weight = 1
		}
		recoveryAttempts += weight
		if verifiedPassed(ev.Payload) {
			recoveryPasses += weight
		}
	}
	if recoveryAttempts > 0 {
		rubric.RecoveryPassRate = float64(recoveryPasses) / float64(recoveryAttempts)
	}

	// has_recovery_signal: any event tagged with recovery_signal=true.
	for _, ev := range events {
		if strings.Contains(ev.Payload, `"recovery_signal":true`) || strings.Contains(ev.Payload, `"recovery_signal": true`) {
			rubric.HasRecoverySignal = true
			break
		}
	}

	// has_closure: any phase_entered or contract_written on postmortem
	// that is not in error.
	for _, ev := range events {
		if ev.Phase == string(loopbiz.PhasePostmortem) {
			rubric.HasClosure = true
			break
		}
	}

	return rubric
}

// verifiedPassed returns true when the contract payload encodes a
// VerifiedDelta with passed=true. The function tolerates both the
// canonical {"passed":true} form and the schema_v1.1 nested form.
func verifiedPassed(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return false
	}
	if v, ok := detail["passed"].(bool); ok {
		return v
	}
	if v, ok := detail["verified"].(bool); ok {
		return v
	}
	return false
}

// verifiedSampleSize extracts the VerifiedDelta.sample_size value
// from a contract payload. The function returns hasWeight=false
// when the field is missing, zero, or negative so the caller can
// fall back to counting the contract as a single attempt. A
// successful multi-sample observation window (e.g. 60s × 1Hz → 60
// samples) propagates to the rubric as 60 sample-weight.
func verifiedSampleSize(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return 0, false
	}
	v, ok := detail["sample_size"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if n <= 0 {
			return 0, false
		}
		return int(n), true
	case int64:
		if n <= 0 {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// formatDuration renders a duration in a compact "1m23s" / "1h05m"
// form. The renderer never sees a "0s" placeholder: zero durations
// return "" so the cell can omit the column.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		minutes := int(d / time.Minute)
		seconds := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	hours := int(d / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	return fmt.Sprintf("%dh%02dm", hours, minutes)
}

// truncateForSummary bounds a JSON-encoded payload to a single-line
// summary the timeline can show in the collapsed card. The 240-char
// cap keeps the layout stable for long RCA documents.
func truncateForSummary(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) <= 240 {
		return s
	}
	return s[:237] + "..."
}

// appendIfMissing adds note to summary when summary does not
// already contain note. Used to mark the contract summary when
// later events (correction / paused) amend the phase state.
func appendIfMissing(summary, note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return summary
	}
	if summary == "" {
		return note
	}
	if strings.Contains(summary, note) {
		return summary
	}
	return summary + "; " + note
}

// dedupeStrings returns the unique-preserving-order subset of in.
// Used to coalesce knowledge refs the same doc may appear in
// multiple payloads.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// stringOr returns detail[key] when it is a non-empty string.
func stringOr(detail map[string]any, key string) string {
	if v, ok := detail[key].(string); ok {
		return v
	}
	return ""
}

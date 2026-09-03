// Package loop — loaders.go
//
// Narrow interfaces used by the postmortem phase (zero-manual-ops-loop
// Day 4) to read prior-phase contracts WITHOUT importing the
// cross-domain contract-repository implementation.
//
// Why narrow interfaces (vs. importing loop_contract directly):
//
//   - The report package must not import internal/manager/repo/loop
//     (monorepo boundary).
//   - Each loader is a 1-method seam that the integration PR (Day 5)
//     wires by adapting the loop_contract repository. The narrow
//     surface keeps test doubles trivial (a map[string]any is enough
//     to satisfy the interface).
//
// Loaders declared here:
//
//   - RootCauseLoader  — load RootCauseJSON for an incident.
//   - CritiqueLoader   — load CritiqueScore for an incident.
//   - VerifiedDeltaLoader — load VerifiedDelta for an incident.
//   - TimelineLoader   — load the chronological event list for an
//     incident (for the Timeline section of the postmortem Markdown).
//
// All loaders take a tenantID argument so the cross-domain impl can
// enforce multi-tenant isolation; the loop-side contract ref is
// irrelevant at the postmortem layer (we only need the payload).
package loop

import "context"

// RootCauseLoader loads the RootCauseJSON contract for an incident.
//
// Implementation contract:
//
//   - Returns the most recent contract whose schema_version is in
//     {v1, v1.1}. Older / unrecognised versions are skipped.
//   - Returns (nil, nil) when the incident has no RootCauseJSON
//     recorded yet (the orchestrator has not reached the
//     investigated phase).
//   - tenantID is required: a cross-tenant incident id must return
//     (nil, nil) so the postmortem cannot leak across tenants.
type RootCauseLoader interface {
	LoadRootCause(ctx context.Context, tenantID, incidentID string) (*RootCauseJSON, error)
}

// CritiqueLoader loads the CritiqueScore contract for an incident.
//
// Implementation contract mirrors RootCauseLoader:
//
//   - Returns the most recent contract with schema_version=v1.
//   - Returns (nil, nil) when the incident has no CritiqueScore
//     recorded (e.g. it never reached the critiqued phase, or the
//     score is still pending). The postmortem renderer treats the
//     nil case as "critic 未评审" and renders a placeholder section
//     (per spec 4.6 test "critic 评分缺").
//   - tenantID enforces multi-tenant isolation.
type CritiqueLoader interface {
	LoadCritique(ctx context.Context, tenantID, incidentID string) (*CritiqueScore, error)
}

// VerifiedDeltaLoader loads the VerifiedDelta contract for an incident.
//
// Same return contract as the others: (nil, nil) when no recovery
// verification has run yet (the orchestrator never reached
// recovered). The postmortem still renders a "deferred to human
// review" placeholder in that case.
type VerifiedDeltaLoader interface {
	LoadVerifiedDelta(ctx context.Context, tenantID, incidentID string) (*VerifiedDelta, error)
}

// TimelineEvent is a single loop event row read for the postmortem's
// Timeline section. We do not expose the full model.Event (DB
// column shape); the postmortem only needs (phase, event_type,
// created_at, summary) so we keep the type lean and decoupled.
type TimelineEvent struct {
	Phase     string
	EventType string
	CreatedAt string // RFC3339
	Summary   string // human-readable one-liner; empty if no payload
}

// TimelineLoader loads the chronological event list for an incident.
//
// Implementation contract:
//
//   - Returns events ordered by created_at ASC.
//   - Empty slice (not nil) when the incident has no events yet.
//   - The postmortem caps the timeline at TimelineMaxRows (most
//     recent); the loader does NOT cap.
type TimelineLoader interface {
	LoadTimeline(ctx context.Context, tenantID, incidentID string) ([]TimelineEvent, error)
}

// TimelineMaxRows is the cap applied by the postmortem renderer on
// the timeline section. Tuned so the Markdown stays readable for
// long-running incidents.
const TimelineMaxRows = 32

// --- Composite loader (production convenience) -------------------------

// Loaders bundles the four loaders so the report package can hold a
// single dependency instead of four. Each field is required; the
// report constructor returns an error when any is nil. Kept as a
// struct (not an interface) because the report package always wants
// the full set.
type Loaders struct {
	RootCause RootCauseLoader
	Critique  CritiqueLoader
	Verified  VerifiedDeltaLoader
	Timeline  TimelineLoader
}

// Validate returns an error if any required loader is nil.
func (l Loaders) Validate() error {
	if l.RootCause == nil {
		return errLoaderMissing("RootCause")
	}
	if l.Critique == nil {
		return errLoaderMissing("Critique")
	}
	if l.Verified == nil {
		return errLoaderMissing("Verified")
	}
	if l.Timeline == nil {
		return errLoaderMissing("Timeline")
	}
	return nil
}

type loaderMissingError string

func (e loaderMissingError) Error() string {
	return "loop: required loader missing: " + string(e)
}

func errLoaderMissing(name string) error {
	return loaderMissingError(name)
}

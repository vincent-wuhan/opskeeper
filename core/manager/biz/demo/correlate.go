package demo

import (
	"context"

	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// FiringCorrelationRepository is the storage half of "is this firing part of
// a running scenario?". It is a separate one-method port rather than another
// method on ScenarioRepository so that everything already faking the
// scenarios surface does not have to learn about correlation.
type FiringCorrelationRepository interface {
	// CorrelateFiring finds the active scenario this firing belongs to (by
	// fingerprint, else by the scenario's own label triple), advances it to
	// alert_correlated, and returns the incident the scenario pre-opened.
	// matched=false means no active scenario owns this firing.
	CorrelateFiring(ctx context.Context, fingerprint string, labels map[string]string) (*alertmodel.Incident, bool, error)
}

// CorrelateFiring satisfies biz/alert's FiringCorrelator structurally — the
// signature is expressed with model/alert types only, so this package never
// imports biz/alert and the two can be connected by main.go without either
// side naming the other.
//
// It is the demo's half of decision 113: the alert ingest path used to carry
// the recognition logic itself (a hardcoded label triple plus a transaction
// that advanced demo_scenario_runs), so the production alert store imported
// the demo model. The behaviour is unchanged — same fingerprints, same label
// fallback, same state transition, same conflict on a missing incident — but
// the knowledge now lives with the story that owns it.
func (u *Usecase) CorrelateFiring(ctx context.Context, fingerprint string, labels map[string]string) (*alertmodel.Incident, bool, error) {
	if u == nil || u.correlations == nil {
		return nil, false, nil
	}
	return u.correlations.CorrelateFiring(ctx, fingerprint, labels)
}

// SetFiringCorrelationRepository wires the scenario storage used for
// correlation. main.go calls it; safe to leave unset, in which case no firing
// is ever claimed by a scenario and the alert path ingests them all normally.
func (u *Usecase) SetFiringCorrelationRepository(r FiringCorrelationRepository) {
	u.correlations = r
}

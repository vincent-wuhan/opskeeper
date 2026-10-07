package alert

import (
	"context"
	"errors"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// ErrUsecaseNotWired is what the zero resolver returns. It is a sentinel
// because it is a wiring mistake rather than a query failure, and a caller
// that retries on it will retry forever.
var ErrUsecaseNotWired = errors.New("alert: the open-alert resolver has no usecase to ask")

// OpenAlertResolver is how the alert domain answers the one question
// agentteams asks of it, in the vocabulary that question needs.
//
// It is a type in this package rather than a method on Usecase on purpose.
// Usecase.ListIncidents takes a filter and returns the entity, which is the
// right shape for this domain and the wrong one across the boundary: the
// consumer needs open incidents and two columns, and giving it the general
// filter-plus-entity shape is what made `agentteams -> alert` a three-type
// edge at all. The adapter is where that narrowing happens, so it happens
// once, next to the entity it projects from.
//
// The other reason it lives here rather than in the consumer is the
// dependency direction. agentteams declares the port; this type satisfies it
// structurally; cmd/opskeeper hands one to the other. Neither domain names
// the other, so neither has to move when the port's shape is wrong.

// OpenAlertResolver projects open incidents for a caller that only closes
// them. A nil Usecase is refused at the method, not at construction, so the
// zero value is a valid thing to hold and an error to use.
type OpenAlertResolver struct {
	UC *Usecase
}

// ListOpenAlerts returns the open incidents, newest first, as the projection.
//
// The status filter is the whole point of this method and the only place the
// word "open" survives the cut. The consumer has no filter to set — for it,
// "open" was a precondition of the question, not something it chose — so if
// this line goes, the method answers a different question than its name and
// a recovery closure will "close" an incident an operator already signed
// off on.
func (r OpenAlertResolver) ListOpenAlerts(ctx context.Context, limit int) ([]domain.OpenAlert, error) {
	if r.UC == nil {
		return nil, ErrUsecaseNotWired
	}
	rows, err := r.UC.ListIncidents(ctx, IncidentFilter{
		Status: model.IncidentStatusOpen,
		Limit:  limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]domain.OpenAlert, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			// The slice is []*Incident and a nil element is representable
			// from the store. Skipping it here rather than handing back a
			// zero OpenAlert, which would read as a real incident with an
			// empty dedupe key and no labels.
			continue
		}
		out = append(out, domain.OpenAlert{
			DedupeKey:  row.DedupeKey,
			LabelsJSON: row.LabelsJSON,
		})
	}
	return out, nil
}

// SystemResolveIncident is passed through unchanged: the key is the key, and
// a projection of a resolve call would only hide the arguments a caller has
// to get right.
func (r OpenAlertResolver) SystemResolveIncident(ctx context.Context, dedupeKey, reason string, occurredAt time.Time) (bool, error) {
	if r.UC == nil {
		return false, ErrUsecaseNotWired
	}
	return r.UC.SystemResolveIncident(ctx, dedupeKey, reason, occurredAt)
}

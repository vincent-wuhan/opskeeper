package loop

// AlertLabelsAdapter reads the subject of an alert from the alert store.
//
// It exists because of one specific dependency: the loop addresses an alert
// by the string id that incidentToDetectionEvent produced, which is the
// alert_incidents primary key in decimal. Everything downstream — the
// evidence chain's subject item, and through it every write action's
// arguments — needs the labels that alert fired with, and the loop package
// must not reach into the alert model to get them.
//
// The failure modes are deliberately loud about what they cost. An id that
// is not a number, an incident that does not exist, and labels that are not
// valid JSON all return an error rather than an empty map, because an empty
// map reads downstream as "this alert genuinely named no object" and the
// resolvers would refuse with a message pointing at the evidence rather than
// at the lookup that failed.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// AlertLabelsAdapter adapts the alert repository to the loop's subject
// lookup.
type AlertLabelsAdapter struct {
	alerts AlertReader
}

// NewAlertLabelsAdapter constructs the adapter. repo must not be nil.
func NewAlertLabelsAdapter(alerts AlertReader) *AlertLabelsAdapter {
	if alerts == nil {
		panic("loop: NewAlertLabelsAdapter: alerts is nil")
	}
	return &AlertLabelsAdapter{alerts: alerts}
}

// AlertLabels implements investigatorreal.AlertLabelsProvider structurally.
func (a *AlertLabelsAdapter) AlertLabels(ctx context.Context, alertID string) (map[string]string, error) {
	idText := strings.TrimSpace(alertID)
	if idText == "" {
		return nil, fmt.Errorf("loop: cannot read the subject of an alert with no id")
	}
	id, err := strconv.ParseUint(idText, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("loop: alert id %q is not an alert_incidents id: %w", idText, err)
	}
	incident, err := a.alerts.GetIncidentByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("loop: read alert incident %d: %w", id, err)
	}
	if incident == nil {
		return nil, fmt.Errorf("loop: alert incident %d does not exist, so it names no object", id)
	}
	labels, err := parseIncidentLabels(incident.LabelsJSON)
	if err != nil {
		return nil, fmt.Errorf("loop: alert incident %d has unreadable labels: %w", id, err)
	}
	return labels, nil
}

package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// The three alert adapters in this package read the alert domain through
// this port rather than through its types, so the loop domain keeps no
// compile-time dependency on alert (decision 279).
//
// One port rather than three narrow ones. Each adapter uses a different
// third of it, and a reader could argue for three interfaces — but the
// wiring constructs exactly one adapter either way, and three interfaces
// would mean three adapters over the same repository for no gain. What
// would be worth arguing for is a narrower *port*; the honest answer there
// is that the trigger adapter needs the rule, the labels adapter needs the
// labels, and the detection adapter needs neither rule nor labels, so the
// union is still small.
//
// Two shapes cross the boundary as raw JSON: LabelsJSON and
// ConditionsJSON. Carrying the parsed values instead would move alert's
// storage format into loop, and carrying the columns would move alert's
// table into loop. Carrying the strings and parsing them here keeps the
// bytes owned by alert and the error messages owned by loop — which is the
// split the adapters' own comments argue for.

// AlertIncident is the part of one alert incident that this package reads.
type AlertIncident struct {
	ID           uint64
	Severity     string
	Scope        string
	Rule         string
	RuleID       *uint64
	FirstFiredAt time.Time
	UpdatedAt    time.Time
	LabelsJSON   string
}

// AlertRule is the part of one rule that this package reads.
type AlertRule struct {
	ConditionsJSON string
}

// AlertIncidentFilter narrows a listing. Status, Severity, DeviceID and
// Offset are absent because no adapter in this package sets them; adding
// one is a compile-safe change whose only other home is the adapter.
type AlertIncidentFilter struct {
	RuleKey string
	Limit   int
}

// AlertReader reads alert incidents and rules. Pointers rather than values
// because "the row is absent" is a real answer here — the alert repository
// returns (nil, nil) for it — and a value type would have to invent a
// sentinel to say the same thing.
type AlertReader interface {
	GetIncidentByID(ctx context.Context, id uint64) (*AlertIncident, error)
	ListIncidents(ctx context.Context, f AlertIncidentFilter) ([]*AlertIncident, error)
	GetRuleByID(ctx context.Context, id uint64) (*AlertRule, error)
}

// parseIncidentLabels reads the labels column.
//
// The one rule that is not obvious, and the reason this function is pinned
// against alert's own rather than assumed to match: an empty column is an
// empty map, not nil and not an error. Downstream, an empty map reads as
// "this alert genuinely named no object" and the resolvers refuse with a
// message pointing at the evidence, so the difference between an empty map
// and a parse failure is the difference between a useful refusal and a
// misleading one. alertport_labels_test.go holds the two against each other
// on a fixture set that includes that case.
func parseIncidentLabels(raw string) (map[string]string, error) {
	if raw == "" {
		return map[string]string{}, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("unreadable labels column: %w", err)
	}
	return out, nil
}

// ruleCondition is the part of one stored rule condition the trigger
// adapter reads. The stored column carries six fields; the three declared
// here are the three it reads, and the other three are ignored the way
// encoding/json ignores unknown keys — so a rule that also declares a
// window still parses here exactly as it did before.
//
// That leniency is also the risk this declaration creates, which is why
// alertport_shape_test.go holds the JSON tags of these three against
// alert's own by reflection. A tag renamed on either side does not fail to
// compile and does not fail to parse: it parses, reads as the zero value,
// and the adapter declines to fire. That is a silent loss of a
// self-healing trigger.
type ruleCondition struct {
	Metric    string  `json:"metric"`
	Operator  string  `json:"operator"`
	Threshold float64 `json:"threshold"`
}

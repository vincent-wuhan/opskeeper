// identity.go — the wire identity of the four alerting tools, and the one
// seam they all read through.
//
// The split rule for this cluster is: everything a tool declares about
// itself on the wire (registered name, description, JSON schema, typed
// args, response row, call timeout) lives here, and everything that
// *executes* it lives next to it as a BaseTool. The Registry methods in
// the parent tools package cannot move — a method is welded to its
// receiver — so they stayed behind and now read their identity from
// here. That asymmetry is the price of lifting a cluster out while the
// node-side pig agent still reaches these tools through Registry
// upcalls; it is deliberate, not a leftover.
package alerting

import (
	"context"
	"encoding/json"
	"time"

	alertbiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/alert"
	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// AlertUsecase is the narrow surface the alert-flavoured tools need from
// the manager/alert biz layer. *alertbiz.Usecase satisfies it; tests
// inject a fake.
//
// It lives with the cluster rather than in toolcore because toolcore must
// not depend on any biz package, and because every one of its four methods
// is only ever needed here.
type AlertUsecase interface {
	GetIncident(ctx context.Context, id uint64) (*alertmodel.Incident, error)
	ListIncidents(ctx context.Context, f alertbiz.IncidentFilter) ([]*alertmodel.Incident, error)
	ListEvents(ctx context.Context, incidentID uint64, limit int) ([]*alertmodel.Event, error)
	ListRules(ctx context.Context, scopeType string) ([]*alertmodel.Rule, error)
}

// ---- query_incidents ---------------------------------------------------

// ToolNameQueryIncidents is the stable wire name the LLM sees.
const ToolNameQueryIncidents = "query_incidents"

// QueryIncidentsDescription pushes the model toward this tool whenever
// the question is "how many / which incidents matched X".
const QueryIncidentsDescription = "List OpsKeeper alert incidents filtered by severity, status, edge, rule_key and a since-window (in minutes). " +
	"Use this for questions like '过去 24h 有几条 critical incident' or 'show open incidents on edge X'. " +
	"Returns array of {id, title, severity, status, rule, edge_id, first_fired_at, last_fired_at}."

// QueryIncidentsSchema is the JSON Schema of the tool's argument object.
var QueryIncidentsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "severity": {
      "type": "string",
      "enum": ["info", "warning", "critical"],
      "description": "Filter by severity. Optional."
    },
    "status": {
      "type": "string",
      "enum": ["open", "acknowledged", "silenced", "resolved"],
      "description": "Filter by lifecycle status. Optional."
    },
    "since_minutes": {
      "type": "integer",
      "minimum": 1,
      "description": "Only return incidents whose last_fired_at is within the last N minutes. Default 1440 (24h)."
    },
    "edge_id": {
      "type": "integer",
      "description": "Filter to incidents on a specific edge. Optional."
    },
    "rule_key": {
      "type": "string",
      "description": "Filter by rule key (e.g. 'cpu_high'). Optional."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 500,
      "description": "Max rows returned (default 50)."
    }
  }
}`)

// QueryIncidentsArgs is the typed form of QueryIncidentsSchema.
type QueryIncidentsArgs struct {
	Severity     string `json:"severity,omitempty"`
	Status       string `json:"status,omitempty"`
	SinceMinutes int    `json:"since_minutes,omitempty"`
	// DeviceID accepts both the new "device_id" key and the legacy
	// "edge_id" so existing prompts keep working.
	DeviceID uint64 `json:"device_id,omitempty"`
	EdgeID   uint64 `json:"edge_id,omitempty"`
	RuleKey  string `json:"rule_key,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// IncidentRow is the trimmed incident envelope returned by query_incidents.
type IncidentRow struct {
	ID             uint64     `json:"id"`
	Title          string     `json:"title"`
	Severity       string     `json:"severity"`
	Status         string     `json:"status"`
	Rule           string     `json:"rule"`
	RuleName       string     `json:"rule_name"`
	DeviceID       *uint64    `json:"device_id,omitempty"`
	ScopeType      string     `json:"scope_type"`
	FirstFiredAt   time.Time  `json:"first_fired_at"`
	LastFiredAt    time.Time  `json:"last_fired_at"`
	EventCount     uint64     `json:"event_count"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
}

// QueryIncidentsCallTimeout caps the biz call. Exported because the
// Registry method in the parent package honours the same budget.
const QueryIncidentsCallTimeout = 15 * time.Second

// ---- get_incident_detail ----------------------------------------------

// ToolNameGetIncidentDetail is the stable wire name the LLM sees.
const ToolNameGetIncidentDetail = "get_incident_detail"

// GetIncidentDetailDescription pushes the model toward this tool when the
// question is about a specific incident's history / timeline.
const GetIncidentDetailDescription = "Return the full incident row plus its event timeline (firing, ack, resolve, notification_sent/failed). " +
	"Use this whenever the question is about what happened on a specific incident id."

// GetIncidentDetailSchema is the JSON Schema of the single-id argument
// object. The BaseTool advertises a batched variant of its own; this one
// is what the node-side Registry path sends.
var GetIncidentDetailSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "incident_id": {
      "type": "integer",
      "minimum": 1,
      "description": "Numeric incident id from query_incidents."
    }
  },
  "required": ["incident_id"]
}`)

// GetIncidentDetailArgs is the typed form of GetIncidentDetailSchema.
type GetIncidentDetailArgs struct {
	IncidentID uint64 `json:"incident_id"`
}

// IncidentEventRow is the trimmed event envelope embedded in the
// incident detail timeline.
type IncidentEventRow struct {
	ID          uint64    `json:"id"`
	EventType   string    `json:"event_type"`
	StatusAfter string    `json:"status_after"`
	Severity    string    `json:"severity"`
	Title       string    `json:"title"`
	Message     *string   `json:"message,omitempty"`
	ActorType   string    `json:"actor_type"`
	ActorID     *uint64   `json:"actor_id,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// IncidentDetailCallTimeout caps the get + events pair.
const IncidentDetailCallTimeout = 10 * time.Second

// ---- query_alert_rules -------------------------------------------------

// ToolNameQueryAlertRules is the stable wire name the LLM sees.
const ToolNameQueryAlertRules = "query_alert_rules"

// QueryAlertRulesDescription pushes the model toward this tool when the
// question is "which rules exist / who's using rule X".
const QueryAlertRulesDescription = "List OpsKeeper alert rules filtered by kind, enabled flag, or a name substring. " +
	"Use this for questions like '这条规则是谁在用' or 'show all metric_threshold rules'. " +
	"Returns array of {id, rule_key, kind, name, scope_type, severity, enabled}."

// QueryAlertRulesSchema is the JSON Schema of the tool's argument object.
var QueryAlertRulesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "kind": {
      "type": "string",
      "description": "Filter by rule kind (metric_threshold | metric_anomaly | metric_forecast | metric_burn_rate | metric_raw | log_match | log_volume | trace_latency | trace_error_rate)."
    },
    "enabled": {
      "type": "boolean",
      "description": "Filter by enabled flag. Optional."
    },
    "name_contains": {
      "type": "string",
      "description": "Substring filter against rule name OR rule_key."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 500,
      "description": "Max rows returned (default 100)."
    }
  }
}`)

// QueryAlertRulesArgs is the typed form of QueryAlertRulesSchema.
type QueryAlertRulesArgs struct {
	Kind         string `json:"kind,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
	NameContains string `json:"name_contains,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

// AlertRuleRow is the trimmed rule envelope returned by query_alert_rules.
type AlertRuleRow struct {
	ID         uint64    `json:"id"`
	RuleKey    string    `json:"rule_key"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	ScopeType  string    `json:"scope_type"`
	Severity   string    `json:"severity"`
	Enabled    bool      `json:"enabled"`
	SourceType string    `json:"source_type"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// QueryAlertRulesCallTimeout caps the biz call.
const QueryAlertRulesCallTimeout = 10 * time.Second

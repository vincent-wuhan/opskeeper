package opskeeperrepair

// toolSpec is one tool this package contributes to the agent.
//
// The table is data, not code: every entry is the same shape and every
// entry does the same thing, which is hand the call to the host. Nothing
// here interprets an argument or touches the machine, because everything
// that does lives in the control plane and on the edge, where it is
// permissioned, tested and — for the mutating tools — reachable only
// through a reviewer a human can see.
//
// The descriptions and schemas are copied from the host implementations
// rather than rewritten. A tool whose schema the model sees and whose
// schema the executor parses are two different objects, and a hand-edited
// copy of one is a lie the model finds out about at run time. The drift
// test in core/floor/pluginmanifest is what keeps the copy honest.
//
// The descriptions say what will happen before the tool runs, because on
// this side of the socket the model is the only thing that can tell the
// operator what it is about to do. A tool whose description omits the
// approval step produces a conversation where the approval prompt appears
// to be a surprise, and a human surprised by an approval prompt is a
// human who approves it.
type toolSpec struct {
	// Name is what the model calls. It is also the name the host looks
	// up, so it is one string end to end rather than a translation
	// somewhere in the middle.
	Name string
	// Label is the human-readable name the console shows.
	Label string
	// Description is what the model reads to decide whether to call this.
	Description string
	// Parameters is the tool's JSON Schema, as a raw literal so a schema
	// change shows up in review as a schema change.
	Parameters string
}

// tools is the whole inventory, sorted by name.
//
// Sorted because this list is the review surface: a reviewer comparing two
// versions of this package is reading a diff, and a reordered list makes
// that diff lie. It is also short. Every name here is a way to change
// something, so the list is a list of things to argue about, and a package
// of thirty would be a package nobody argues about properly.
var tools = []toolSpec{

	{
		Name:        "apply_config_change",
		Label:       "apply_config_change",
		Description: "Apply a previously confirmed new alert-rule configuration draft. MUTATING: this changes live alerting configuration, and the host requires an operator to confirm the exact draft hash before it is applied. Call draft_config_change first, disclose the draft scope, wait for the user's explicit confirmation, then call this with the unchanged draft_hash and payload. Never invent a draft_hash.",
		Parameters: `{
  "type": "object",
  "required": ["domain", "action", "confirmed", "payload", "draft_hash"],
  "properties": {
    "domain": {
      "type": "string",
      "enum": ["alert_rule"],
      "description": "Configuration domain from the config_draft."
    },
    "action": {"type": "string", "enum": ["create"]},
    "confirmed": {"type": "boolean", "description": "Must be true after explicit user confirmation."},
    "draft_id": {"type": "string", "description": "Optional top-level copy of payload.draft_id; the exact payload remains the source of truth."},
    "draft_hash": {"type": "string", "description": "Exact draft_hash returned by draft_config_change. The payload is rejected if this hash does not match."},
    "confirmation_text": {"type": "string"},
    "payload": {
      "type": "object",
      "description": "Exact payload object returned by draft_config_change; it is the source of truth for action/rule."
    },
    "rule": {"type": "object", "additionalProperties": true}
  }
}`,
	},
	{
		Name:        "draft_config_change",
		Label:       "draft_config_change",
		Description: "Create and validate a read-only configuration draft for a new alert rule across all supported alert rule kinds. It never persists business config, so it needs no approval and is safe to call while you are still working out what the user wants. It validates the candidate and returns either config_validation_failed with issues to fix, or config_draft with draft_hash. Only config_draft is confirmable; after one successful draft, stop tool calls, disclose the draft scope from scope.label/type, and ask the user to confirm, cancel, or request a scope change.",
		Parameters: `{
  "type": "object",
  "required": ["domain", "action", "request_text", "rule"],
  "properties": {
    "domain": {
      "type": "string",
      "enum": ["alert_rule"],
      "description": "Only alert_rule is supported in v1."
    },
    "action": {"type": "string", "enum": ["create"]},
    "request_text": {
      "type": "string",
      "description": "Exact current user request text that triggered this draft. Always copy the user's latest natural-language request verbatim; backend normalization uses it to verify scope labels such as log level/unit and explicit database source intent."
    },
    "lookback_seconds": {
      "type": "integer",
      "minimum": 60,
      "maximum": 604800,
      "description": "alert_rule preview lookback window; default 86400."
    },
    "rule": {"$ref": "#/$defs/rule", "description": "Required for creating an alert rule."}
  },
  "$defs": {
    "rule": {
      "type": "object",
      "properties": {
        "rule_key": {"type": "string", "description": "Required for create. Lower snake case."},
        "kind": {
          "type": "string",
          "enum": ["metric_threshold", "metric_raw", "metric_anomaly", "metric_forecast", "metric_burn_rate", "log_match", "log_volume", "trace_latency", "trace_error_rate"],
          "description": "Choose the existing alert creation mode. metric_threshold is only for host closed-set metrics. metric_raw is for arbitrary PromQL predicates, database metrics, custommetrics, and any exact collected metric name."
        },
        "name": {"type": "string"},
        "scope_type": {"type": "string", "enum": ["global", "host", "monitoring_pipeline"], "description": "Use host when the alert should be associated with a specific machine or device-collected instance, such as CPU, memory, disk/filesystem, load, network, system/journald logs, database/Redis/MongoDB metrics, or when the final PromQL/LogQL result keeps a device_id label. Use global only for service, SLO, trace, or intentionally aggregated fleet-wide rules where no single host/device should own the incident."},
        "join_mode": {"type": "string", "enum": ["all", "any"]},
        "window": {"type": "string", "description": "Compatibility alias. Prefer kind-specific spec.window or condition.window; backend normalizes this field into the correct place."},
        "for": {"type": "string", "description": "Compatibility alias for sustained duration. Prefer spec.for for metric_raw or condition.for for metric_threshold; backend normalizes this field into the correct place."},
        "severity": {"type": "string", "enum": ["info", "warning", "critical"]},
        "enabled": {"type": "boolean"},
        "conditions": {
          "type": "array",
          "description": "Only for kind=metric_threshold host rules. Canonical host metrics: cpu_pct, mem_pct, disk_used_pct, disk_avail_bytes, load1, load5, load15, net_rx_bps, net_tx_bps. Do not put MySQL/PostgreSQL/Redis/MongoDB/custom metrics here; use metric_raw instead.",
          "items": {"$ref": "#/$defs/condition"}
        },
        "spec": {
          "type": "object",
          "additionalProperties": true,
          "description": "Kind-specific spec. metric_raw accepts expr/promql/query as a full boolean PromQL predicate, or metric plus operator and threshold for one exact Prometheus metric. Use metric names and label keys from list_metric_catalog when available. Only scope selectors when the user explicitly asked for that source/device/job/service/instance; mark that with source_explicit=true. Other kinds use their natural fields: metric_anomaly, metric_forecast, metric_burn_rate, log_match, log_volume, trace_latency, trace_error_rate."
        },
        "labels": {"type": "object", "additionalProperties": {"type": "string"}},
        "runbook_url": {"type": "string"},
        "notify_channel_ids": {"type": "array", "items": {"type": "integer"}},
        "notify_window_seconds": {"type": "integer", "minimum": 0},
        "notify_min_fires": {"type": "integer", "minimum": 0}
      }
    },
    "condition": {
      "type": "object",
      "properties": {
        "metric": {"type": "string"},
        "operator": {"type": "string"},
        "threshold": {"type": "number"},
        "window": {"type": "string"},
        "for": {"type": "string"},
        "aggregator": {"type": "string"}
      }
    }
  }
}`,
	},
	{
		Name:        "host_restart_service",
		Label:       "重启 systemd 服务",
		Description: "Restart an allowlisted systemd service on one managed host. MUTATING: the service is down for the duration of the restart, and a second restart of a service that is already restarting is a second outage. The host requires an operator to approve this exact call first, and refuses it if nobody has. Use it when evidence points at the service itself — NOT to fix a symptom you have not explained, and NOT for a crash-looping service (that needs the cause, not more restarts).",
		Parameters: `{
  "type": "object",
  "required": ["device_id", "service"],
  "properties": {
    "device_id": {
      "type": "integer",
      "description": "Target device id. The same id as the @-mention chip and the device_id label on Prom metrics."
    },
    "service": {
      "type": "string",
      "description": "systemd short name, without the .service suffix, e.g. nginx / redis / prometheus."
    },
    "reason": {
      "type": "string",
      "description": "Why this restart is the right action. Written into the audit row, so write it for the person reading it in six months."
    }
  }
}`,
	},
	{
		Name:        "recovery.execute",
		Label:       "recovery.execute",
		Description: "Execute a closed-loop recovery action for an incident after verify_recovery returned a passed=false verdict. MUTATING — it atomically reserves an exact approved HITL proposal before dispatching, so a proposal_id is mandatory and cannot be reused. The proposal must already be approved by a human; this tool enforces that a repair corresponds to a decision somebody made, rather than to a model's confidence.",
		Parameters: `{
  "type": "object",
  "properties": {
    "incident_id":      {"type": "string", "minLength": 1, "description": "Incident id the recovery action targets. Used as the proposal session binding."},
    "proposal_id":      {"type": "string", "pattern": "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$", "description": "Exact approved HITL proposal reserved for this action."},
    "skill_id":         {"type": "string", "minLength": 1, "description": "Skill id selected by investigator (matches verify_recovery.skill_id)."},
    "target":           {"type": "string", "minLength": 1, "description": "Resource locator (host-1 / pg-cluster-x / k8s-deploy-y). Echoed into the audit envelope."},
    "resource_type":    {"type": "string", "enum": ["host","pg","redis","k8s","app"], "description": "Target resource type — host dispatches to restart_service or the case-owned fixture; other types are drill-only."},
    "baseline_window":  {"type": "string", "pattern": "^[1-9][0-9]*(m|h|s)$", "description": "Echo from verify_recovery's baseline_window; not re-validated here (verify already ran the gate)."},
    "compare_window":   {"type": "string", "pattern": "^[1-9][0-9]*(m|h|s)$", "description": "Echo from verify_recovery's compare_window."},
    "tolerance":        {"type": "number", "minimum": 0, "maximum": 1, "description": "Echo from verify_recovery's tolerance."},
    "parameters": {
      "type": "object",
      "additionalProperties": false,
      "description": "Action-specific payload. restart_service binds device_id/service; kill_process binds incident_id/fixture_manifest_id; resize_pool binds incident_id/pool_manifest_id; skip_audit is forbidden for AgentTeams.",
      "properties": {
        "command":        {"type": "string", "enum": ["restart_service", "kill_process", "resize_pool", "noop"]},
        "device_id":      {"type": "integer", "minimum": 1},
        "service":        {"type": "string", "minLength": 1, "maxLength": 255},
        "incident_id":        {"type": "string", "minLength": 1, "maxLength": 64},
        "fixture_manifest_id":{"type": "string", "minLength": 8, "maxLength": 128},
        "pool_manifest_id":   {"type": "string", "minLength": 8, "maxLength": 128},
        "preview_run_id":     {"type": "string", "minLength": 8, "maxLength": 128},
        "preview_candidate_id": {"type": "string", "minLength": 3, "maxLength": 128},
        "reason":         {"type": "string", "minLength": 1, "maxLength": 512},
        "skip_audit":  {"type": "boolean", "default": false}
      }
    }
  },
  "required": ["incident_id","proposal_id","skill_id","target","resource_type","parameters"],
  "additionalProperties": false
}`,
	},
	{
		Name:        "verify_recovery",
		Label:       "verify_recovery",
		Description: "Verify that a repair action pulled the target metric back to its pre-alert baseline, returning VerifiedDelta (passed / failed_metrics / deltas / retry_count). READ-ONLY, so it needs no approval and is the one call in this package you should reach for before proposing anything. Only allowlisted metrics are checked; any other resource type is refused outright. A repair that is not verified is a hope, and this is what tells the two apart.",
		Parameters: `{
  "type": "object",
  "properties": {
    "skill_id":        {"type": "string",  "description": "关联 skill id（来自 investigator 根因）"},
    "target":          {"type": "string",  "description": "host-1 / pg-cluster-x / k8s-deploy-y 等资源定位符"},
    "resource_type":   {"type": "string",  "enum": ["host","pg","redis","k8s","app"], "description": "目标资源类型，决定 per-resource metric 子集"},
    "baseline_window": {"type": "string",  "description": "baseline 窗口（Go duration 语法，如 5m），默认 5m，范围 (0, 1h]"},
    "compare_window":  {"type": "string",  "description": "compare 窗口（Go duration 语法，如 2m），默认 2m，范围 (0, 1h]"},
    "tolerance":       {"type": "number",  "minimum": 0, "maximum": 1, "default": 0.15, "description": "相对偏差阈值（绝对值），<= 该值算 pass"},
    "metrics":         {"type": "array",   "minItems": 1, "items": {"type": "string", "enum": ["cpu_usage","mem_usage","qps","latency_p99"]}, "description": "待校验 metric 子集，必须是 allowlist 内且当前 resource_type 允许"},
    "sensitivity":     {"type": "string",  "description": "data-guard-classification 分类（public/internal/confidential/restricted），可选"}
  },
  "required": ["skill_id","target","resource_type","metrics"],
  "additionalProperties": false
}`,
	},
}

// ToolNames returns the inventory in order, for the host-side drift check
// and for diagnostics.
func ToolNames() []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

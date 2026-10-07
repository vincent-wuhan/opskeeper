package opskeeperobservability

// GENERATED FILE — do not edit.
//
// Produced from the live Info() of the tools in
// core/manager/biz/aiops/tools by that package's tests. The registry
// is the authority: it is what executes the call, so its schema is the one
// that has to be correct, and this file is the copy the node's agent reads.
//
// Regenerate with:
//
//	OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/biz/aiops/tools/ -run Toolset
//
// then run scripts/sync-pig-ops.sh to copy the extension into the package.
// TestTheObservabilityToolsetMatchesTheRegistry fails if this file and the
// registry disagree, so editing it by hand fails a test rather than
// shipping a menu the executor cannot parse.

// toolSpec is one tool this package contributes to the agent.
//
// The table is data, not code: every entry is the same shape and every
// entry does the same thing, which is hand the call to the host. Nothing
// here interprets an argument or touches a system, because everything that
// does lives in the control plane, where it is permissioned, covered and
// — for the mutating tools elsewhere in this repository — reachable only
// through a reviewer a human can see.
//
// These tools are all served by an upcall. The observability stack and the
// database sources are the manager's: the Prometheus and Loki endpoints and
// the registered repositories are configured there, and a subprocess on a
// node has no path to any of them. Inventing one would be a second,
// unaudited route into the control plane.
type toolSpec struct {
	// Name is what the model calls. It is also the name the host looks
	// up, so it is one string end to end.
	Name string
	// Label is the human-readable name the console shows.
	Label string
	// Description is what the model reads to decide whether to call this.
	Description string
	// Parameters is the tool's JSON Schema, copied from the registry.
	Parameters string
}

// tools is the whole inventory, sorted by name.
var tools = []toolSpec{

	{
		Name:        "analyze_database_status",
		Label:       "analyze_database_status",
		Description: "Analyze database health and performance from OpsKeeper database metrics sources. Use this as the first tool for any MySQL, PostgreSQL, Redis, or MongoDB question that exporter metrics can cover, before raw query_promql, query_knowledge, or AgentTool. This includes health, performance, logical database/schema/table/collection/key counts from metrics, cluster/replication, advanced/optional exporter collectors, and metric coverage. For database alert-rule creation, list_metric_catalog must discover the current metric names first; use this afterwards only if source, device, or capability context is still needed before drafting. It can answer typed object-inventory questions such as how many MySQL schemas, PostgreSQL databases/tables, Redis logical DBs/keys, or MongoDB databases/collections exist from collected exporter metrics; if object inventory metrics are not collected, the response says which capability is missing. The response includes discovered metric names, unmapped metric names, and a capability matrix; unavailable capabilities mean the exporter has not collected the needed metrics.",
		Parameters: `{
  "type": "object",
  "properties": {
    "device_ids": {
      "type": "array",
      "items": {"type": "integer"},
      "minItems": 1,
      "maxItems": 16,
      "description": "Optional device ids to analyze. Omit to analyze discovered database metric sources across all devices."
    },
    "db_types": {
      "type": "array",
      "items": {"type": "string", "enum": ["mysql", "postgresql", "postgres", "pg", "redis", "mongodb", "mongo"]},
      "description": "Optional database type filter."
    },
    "source_ids": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional databasemetrics/custommetrics source ids to analyze."
    },
    "lookback_seconds": {
      "type": "integer",
      "minimum": 300,
      "maximum": 86400,
      "description": "Analysis window in seconds. Default 3600."
    },
    "include_custommetrics": {
      "type": "boolean",
      "description": "Include custommetrics targets with resource.category=database and resource.type set to mysql/postgresql/redis/mongodb. Default true."
    },
    "include_disabled": {
      "type": "boolean",
      "description": "Include disabled plugin rows or disabled sources. Default false."
    }
  }
}`,
	},
	{
		Name:        "get_edge_summary",
		Label:       "get_edge_summary",
		Description: "Return a one-shot snapshot for an edge: registration metadata, current host load (cpu/mem/load), and recent incidents in the last 24h (any status, severity >= warning). Use this whenever the question is about a single named host's overall state.",
		Parameters: `{
  "type": "object",
  "properties": {
    "device_ids": {
      "type": "array",
      "items": {"type": "integer"},
      "minItems": 0,
      "maxItems": 16,
      "description": "设备 id 列表，一次最多 16 个，把它们的 metadata + host_load + 24h incidents 一次性全拿回来（省得逐台单独调）。【省略或留空 = 汇总全部边端设备（最多 16 个）】，适合「巡检 / 体检所有设备」这类不指定具体 id 的请求，无需先查设备清单。"
    }
  }
}`,
	},
	{
		Name:        "get_host_load",
		Label:       "get_host_load",
		Description: "Return current CPU percent, memory percent, and 1/5/15-minute load averages of the named edge host.",
		Parameters: `{
  "type": "object",
  "properties": {
    "device_ids": {
      "type": "array",
      "items": {"type": "integer"},
      "minItems": 1,
      "maxItems": 16,
      "description": "设备 id 列表，一次最多 16 个。fleet 视角问题（'哪台 cpu 最高'/'对比这几台 mem'）用此一次性拉，避免单设备多次调用。"
    }
  },
  "required": ["device_ids"]
}`,
	},
	{
		Name:        "grep_source",
		Label:       "grep_source",
		Description: "Search a registered git repo's tracked source for a regex (function/type names, error strings). Returns path:line:text hits. Binary files skipped.",
		Parameters: `{
  "type": "object",
  "properties": {
    "repo": {"type": "string", "description": "Which registered repo: URL / unique substring / numeric id."},
    "pattern": {"type": "string", "description": "git-grep basic regex. e.g. a function name \"func ResolveEdgeID\" or an error string \"connection refused\"."},
    "path_glob": {"type": "string", "description": "Optional pathspec to narrow the search, e.g. \"*.go\" or \"core/manager/\". Empty = whole repo."},
    "max_results": {"type": "integer", "description": "Cap on hits returned. Default 50, max 200.", "default": 50, "minimum": 1, "maximum": 200}
  },
  "required": ["repo", "pattern"]
}`,
	},
	{
		Name:        "list_database_sources",
		Label:       "list_database_sources",
		Description: "List configured database metrics sources without querying Prometheus. Use this only for configured-source lists, source counts, generic database inventory questions like 'how many databases do I have', where a database source is located, and simple device/edge/plugin/source relationships when the user asks about configuration rather than current database state or collected database objects. Not for health, performance, or metric coverage analysis; use analyze_database_status for those.",
		Parameters: `{
  "type": "object",
  "properties": {
    "device_ids": {
      "type": "array",
      "items": {"type": "integer"},
      "minItems": 1,
      "maxItems": 64,
      "description": "Optional device ids to list. Omit to list discovered database metric sources across all devices."
    },
    "db_types": {
      "type": "array",
      "items": {"type": "string", "enum": ["mysql", "postgresql", "postgres", "pg", "redis", "mongodb", "mongo"]},
      "description": "Optional database type filter."
    },
    "source_ids": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional databasemetrics/custommetrics source ids to list."
    },
    "include_custommetrics": {
      "type": "boolean",
      "description": "Include custommetrics targets with resource.category=database and resource.type set to mysql/postgresql/redis/mongodb. Default true."
    },
    "include_disabled": {
      "type": "boolean",
      "description": "Include disabled plugin rows or disabled sources. Default false."
    }
  }
}`,
	},
	{
		Name:        "list_metric_catalog",
		Label:       "list_metric_catalog",
		Description: "List currently scraped Prometheus metric names with representative labels. Use this before drafting metric-based alert rules so the draft can use current metric names and label keys. sample_labels are examples for understanding available labels; do not copy sample label values into selectors unless the user explicitly requested that exact source.",
		Parameters: `{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Natural-language or keyword hint, e.g. 'MySQL connection usage', 'Redis memory usage', 'exporter down', 'http p95 latency', 'queue depth'. The tool uses it to rank returned metric names."
    },
    "prefixes": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional metric-name prefixes to restrict discovery, e.g. ['mysql_'], ['pg_'], ['redis_'], ['mongodb_'], ['http_', 'custom_', 'node_']."
    },
    "metric_regex": {
      "type": "string",
      "description": "Optional RE2 regex for __name__. It must not match the empty string. Omit to list all current metric names."
    },
    "selector": {
      "type": "string",
      "description": "Optional Prometheus label selector fragment, with or without braces, e.g. 'job=\"api\"' or '{device_id=\"5\"}'."
    },
    "labels": {
      "type": "object",
      "additionalProperties": true,
      "description": "Optional exact label matchers to combine with selector, e.g. {'device_id': 5, 'opskeeper_source': 'custom:api'}."
    },
    "max_metrics": {
      "type": "integer",
      "minimum": 1,
      "maximum": 200,
      "description": "Maximum metric names to return. Default 80, cap 200."
    },
    "include_label_samples": {
      "type": "boolean",
      "description": "Include up to three representative label sets per metric. Default true."
    }
  }
}`,
	},
	{
		Name:        "list_repo_sources",
		Label:       "list_repo_sources",
		Description: "List one directory level of a registered git repo's source tree (dirs first, then files with sizes). Use to discover structure before reading.",
		Parameters: `{
  "type": "object",
  "properties": {
    "repo": {"type": "string", "description": "Which registered repo: its URL (or a unique substring like \"liaison-cloud\") or numeric id."},
    "subpath": {"type": "string", "description": "Directory inside the repo to list (e.g. \"core/manager\"). Empty = repo root."}
  },
  "required": ["repo"]
}`,
	},
	{
		Name:        "query_change_events",
		Label:       "query_change_events",
		Description: "查询 audit log 里某时间窗内的「变更事件」——谁通过 OpsKeeper 改了什么（告警规则 / 设备 / 设置·LLM key / 通知通道 / 仓库 / 技能 / 用户）。RCA 溯源时回答「症状发生前后改了什么」，把变更当根因候选。只覆盖经 OpsKeeper 产品发起的变更，外部变更（SSH / 带外部署 / 容器 churn / package change）由边缘 changewatcher 上报；缺失时通常表示边缘 agent 离线。",
		Parameters: `{
  "type": "object",
  "properties": {
    "around_ts": {"type": "string", "description": "可选锚点时间 RFC3339（通常用 incident 的 fired_at）；省略时默认当前时间，围绕它取前后窗口。"},
    "window_minutes": {"type": "integer", "minimum": 1, "maximum": 1440, "description": "半窗口分钟数（默认 30，即锚点前后各 30 分钟）。"},
    "resource_type": {"type": "string", "description": "可选，缩小到某类资源：rule/device/setting/channel/repo/skill/user/llm/grafana。"},
    "action": {"type": "string", "description": "可选，缩小到某动作：rule_update/setting_update/device_update/repo_sync/..."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 200, "description": "返回条数上限（默认 50）。"}
  },
  "required": []
}`,
	},
	{
		Name:        "query_logql",
		Label:       "query_logql",
		Description: "Run a LogQL range query against Loki. Use this to investigate log patterns, error counts, or pipe into per-edge filters. Returns the raw Loki response (streams or matrix).",
		Parameters: `{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "LogQL expression. Example: \"{edge_id=\\\"1\\\"} |= \\\"error\\\"\"."
    },
    "start": {
      "type": "string",
      "description": "RFC3339 start time. Defaults to now-1h."
    },
    "end": {
      "type": "string",
      "description": "RFC3339 end time. Defaults to now."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 5000,
      "description": "Max number of result rows (default 200)."
    },
    "direction": {
      "type": "string",
      "enum": ["backward", "forward"],
      "description": "Order of results, default \"backward\" (newest first)."
    }
  },
  "required": ["query"]
}`,
	},
	{
		Name:        "query_promql",
		Label:       "query_promql",
		Description: "Run a PromQL range query against the cluster's Prometheus. Use this when you need any host or container metric beyond the few host-level fields the basic tools return. For fleet or multi-device questions, write one vectorized PromQL expression with by(device_id, ...) / regex selectors / topk instead of one query per device or metric. Returns the raw Prom HTTP API response.",
		Parameters: `{
  "type": "object",
  "properties": {
    "expr": {
      "type": "string",
      "description": "PromQL expression. Prefer one vectorized expression for multiple devices/labels. Example: \"avg by (device_id) (rate(node_cpu_seconds_total{mode!=\\\"idle\\\"}[5m]))\". For filesystem percent, combine numerator/denominator in one expression instead of separate used/size queries."
    },
    "lookback_seconds": {
      "type": "integer",
      "minimum": 60,
      "maximum": 604800,
      "description": "How far back to query in seconds (default 300 = 5 minutes; max 604800 = 7d). Use one 7d range query for weekly trends instead of repeating short lookbacks."
    }
  },
  "required": ["expr"]
}`,
	},
	{
		Name:        "query_traceql",
		Label:       "query_traceql",
		Description: "Run a TraceQL search against Tempo. Use this to find traces by service / operation / latency / status. Returns trace summaries (id, service, root span name, duration, span count).",
		Parameters: `{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "TraceQL expression. May be empty if service/operation/duration filters are given."
    },
    "service": {
      "type": "string",
      "description": "Filter by resource.service.name (tag mode)."
    },
    "operation": {
      "type": "string",
      "description": "Filter by span name (tag mode)."
    },
    "start": {
      "type": "string",
      "description": "RFC3339 start time. Defaults to now-1h."
    },
    "end": {
      "type": "string",
      "description": "RFC3339 end time. Defaults to now."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 1000,
      "description": "Max trace summaries to return (default 50)."
    },
    "min_duration": {
      "type": "string",
      "description": "Minimum trace duration as a Go duration string (e.g. \"100ms\", \"2s\")."
    },
    "max_duration": {
      "type": "string",
      "description": "Maximum trace duration as a Go duration string."
    }
  }
}`,
	},
	{
		Name:        "read_source",
		Label:       "read_source",
		Description: "Read a source file (or a 1-indexed [start_line,end_line] window) from a registered git repo. Binary files are refused; large files are capped.",
		Parameters: `{
  "type": "object",
  "properties": {
    "repo": {"type": "string", "description": "Which registered repo: URL / unique substring / numeric id."},
    "path": {"type": "string", "description": "File path relative to repo root, e.g. \"core/floor/tunnel/messages.go\"."},
    "start_line": {"type": "integer", "description": "1-indexed first line to return. Omit/0 = whole file. Set this to the line from a stack trace.", "minimum": 1},
    "end_line": {"type": "integer", "description": "Inclusive last line. Omit/0 = to EOF (or a sensible window around start_line)."}
  },
  "required": ["repo", "path"]
}`,
	},
}

// ToolNames returns the inventory in order, for the host-side drift
// check and for diagnostics.
func ToolNames() []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

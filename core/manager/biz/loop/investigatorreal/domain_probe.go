package investigatorreal

// Domain evidence: asking the domain what state it is in, instead of
// inferring it from a metric.
//
// Why this file exists. The remediation axis of the evaluation sat at 3/20,
// and the reason was not a missing adapter — every tool the closed loop
// dispatches to is implemented. The reason was that the investigator could
// not tell one incident from another. It queried Prometheus for a single
// scalar and Loki for a log-line count, and every PostgreSQL incident
// produced the same shape: `max(pg_stat_activity_max_tx_duration_seconds)`
// and a number. A table that has bloated, a vacuum that has stopped making
// progress, and a query that got slow are three different faults with three
// different fixes, and at that resolution they are indistinguishable.
//
// The temptation was to add thresholds. That is the same guess the
// resolvers refuse: a threshold that happens to separate the golden cases
// separates them by coincidence, and the first production incident it meets
// that does not fit proposes a VACUUM for a slow query.
//
// So the investigator now asks. `pg.table_bloat` reports dead-tuple ratios
// per table; `pg.vacuum_status` reports whether a vacuum is actually
// progressing; `k8s.node_list` reports which nodes are NotReady. Each is a
// recorded observation, and a proposal gated on one says why it is being
// made. The same rule the rest of this codebase follows: evidence, never
// inference; ambiguity, refusal.
//
// The read plan below is deliberately small. It names the few questions that
// discriminate between the faults whose fixes differ, not every tool the
// adapters offer — an investigation that calls everything is an
// investigation whose latency nobody will accept during an incident.

import (
	"context"
	"fmt"
	"strings"
	"time"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// DomainProbe is one read-only call the investigator makes to observe the
// domain it is investigating.
type DomainProbe struct {
	// Tool is the registered tool name.
	Tool string

	// Args are forwarded to the tool unchanged. Every probe in the plan
	// takes only optional arguments, so a plan entry never invents a
	// filter that would decide the answer.
	Args map[string]any

	// ForEach expands this entry into one call per row of an earlier
	// entry's result, filling the named arguments from named columns.
	//
	// It exists because some questions cannot be asked in one call.
	// "How full is this PersistentVolume?" is one question about one disk,
	// and the disk is not known until the cluster has been asked which
	// claims exist. The two obvious alternatives are both worse: a single
	// "measure everything" tool has to pick which volumes to measure, and
	// whichever way it picks — first N, biggest N, alphabetically — the
	// full one is missing from the answer more often than not, which is
	// the one case the investigation was for. Guessing the volume name
	// instead is the same failure with a different disguise.
	//
	// So the plan states the dependency and the probe runner follows it.
	// The expansion is bounded by the source probe's own limit, and each
	// call still passes the read-only check below.
	ForEach *ForEachRow
}

// ForEachRow describes how one probe entry is expanded over another entry's
// rows.
type ForEachRow struct {
	// Source is the tool whose recorded rows to iterate. It must appear
	// EARLIER in the same resource type's plan; a forward reference is
	// refused rather than skipped, because a plan that silently loses a
	// probe reports a thinner investigation as a complete one.
	Source string

	// Columns maps an argument name to the row column that supplies it.
	// Two or more are ordinary — a volume's name is not unique across
	// namespaces, so the namespace comes from the same row that named the
	// claim rather than from the credential's default.
	Columns map[string]string
}

// domainProbePlan is the read plan per resource type.
//
// The keys are the resource types the loop routes on, matching
// ListRemediationsWithEvidence. A resource type absent here is simply not
// probed, which is the same as the domain being unreachable.
var domainProbePlan = map[string][]DomainProbe{
	"pg": {
		// dead_pct per table is what separates "this table is bloating"
		// from "this database is slow"; vacuum_status says whether the
		// automatic path is already working on it, which is the
		// difference between proposing a manual VACUUM and not.
		{Tool: "pg.table_bloat", Args: map[string]any{"limit": 20}},
		{Tool: "pg.vacuum_status", Args: map[string]any{"limit": 20}},
		{Tool: "pg.slow_log", Args: map[string]any{"limit": 10}},
	},
	"k8s": {
		// rollout_status with only_stuck answers "is a rollout wedged",
		// which is the only evidence that makes a rollback the right
		// proposal rather than a forward fix.
		{Tool: "k8s.rollout_status", Args: map[string]any{"only_stuck": true, "limit": 20}},
		// Not left filtered to unhealthy: the evidence records what the
		// cluster looks like, and the proposal below is what narrows it
		// to the NotReady nodes. A filtered read would make the chain say
		// "these are the nodes" when it means "these are the nodes I
		// asked about".
		{Tool: "k8s.node_list", Args: map[string]any{"limit": 100}},
		// A full volume is invisible to every read above: pod_list says a
		// pod is running, node_list says the node has room, and neither
		// mentions the filesystem the pod is out of. pvc_list is what
		// names the claims, and pvc_usage is what measures each one — the
		// API server cannot do the second, so it is a separate call per
		// claim rather than a column on the first.
		{Tool: "k8s.pvc_list", Args: map[string]any{"limit": 20}},
		{Tool: "k8s.pvc_usage", ForEach: &ForEachRow{
			Source:  "k8s.pvc_list",
			Columns: map[string]string{"pvc": "name", "namespace": "namespace"},
		}},
	},
	"redis": {
		{Tool: "redis.big_keys", Args: map[string]any{"limit": 10}},
		{Tool: "redis.slow_log", Args: map[string]any{"limit": 10}},
		// INFO is what distinguishes "the instance is full" from "the
		// instance is full *and will start refusing writes*". The
		// eviction policy decides whether high memory turns into failed
		// commands or into a full keyspace, and that difference is what
		// decides which remedies are even on the table.
		{Tool: "redis.info", Args: map[string]any{}},
	},
	"mq": {
		{Tool: "mq.inspect_consumer_lag", Args: map[string]any{"limit": 20}},
		// A hot partition is invisible to a per-group lag total: a group can
		// be falling behind because one partition is carrying everything or
		// because all of them are carrying a share, and those have the same
		// number and completely different fixes. The topic is not known
		// until the lag read above has named the groups that are behind, so
		// the skew read is expanded over exactly those rows.
		//
		// The tool is registered by the kafka-namespaced adapter, so a
		// deployment with only the neutral `mq.` DSN does not register it;
		// the probe then makes no call and the proposals below stay absent,
		// which is the same state as a cluster with no skewed partitions.
		{Tool: "kafka.partition_skew", ForEach: &ForEachRow{
			Source:  "mq.inspect_consumer_lag",
			Columns: map[string]string{"topic": "topic"},
		}},
	},
	"host": {
		// CPU and memory alerts reach the loop as load averages and free
		// memory, and neither names a process or a file. These two are
		// what turn "the box is hot" into "pid 4211 is", which is the
		// difference between a proposal and a shrug.
		{Tool: "host.top_processes", Args: map[string]any{"limit": 10}},
		{Tool: "host.old_log_files", Args: map[string]any{"path": "/var/log", "older_than_days": 30}},
	},
}

// probeRiskCeiling is the highest risk level the investigator will call.
//
// L0 and L1 are read-only and diagnostic. An investigation runs before
// anyone has approved anything, so a probe at L2 or above would be a write
// performed as a side effect of looking. The check is here rather than in a
// comment because the plan is data: a future edit that adds a probe must
// pass this, and the test that enforces it lives with the plan.
const probeRiskCeiling = "L1"

// probe runs the plan for one resource type and returns the results as
// evidence items.
//
// A probe that fails is logged and skipped. A database that is refusing
// connections is exactly the incident worth investigating, and refusing to
// record anything because one of four questions went unanswered would make
// the tool least useful when it is most needed. The chain is then thinner,
// and the proposals that needed that evidence are not made.
func probe(ctx context.Context, tools loop.ToolCaller, resourceType string, at time.Time) []loop.EvidenceItem {
	plan := domainProbePlan[resourceType]
	if tools == nil || len(plan) == 0 {
		return nil
	}
	items := make([]loop.EvidenceItem, 0, len(plan))
	// rowsByTool records what each earlier probe returned, so a later entry
	// that declares ForEach can iterate it. Kept per resource type and per
	// call: evidence does not leak between investigations.
	rowsByTool := make(map[string][]map[string]any, len(plan))
	for _, entry := range plan {
		if !probeIsReadOnly(tools, entry.Tool) {
			continue
		}
		for _, call := range expandProbe(entry, rowsByTool) {
			result, err := tools.CallTool(ctx, call.tool, call.args)
			if err != nil {
				continue
			}
			// The Tool field matters: ProbeRows keys on it, so a synthetic
			// item without one would match nothing and the expansion would
			// silently produce zero calls.
			rows := loop.ProbeRows([]loop.EvidenceItem{{Tool: call.tool, Value: result}}, call.tool)
			rowsByTool[call.tool] = append(rowsByTool[call.tool], rows...)
			items = append(items, loop.EvidenceItem{
				Tool:      call.tool,
				Query:     call.tool + " (domain probe)",
				Value:     result,
				Count:     probeRowCount(result),
				Timestamp: at,
			})
		}
	}
	return items
}

// probeCall is one concrete call: a tool and the arguments it is made with.
type probeCall struct {
	tool string
	args map[string]any
}

// maxForEachCalls bounds a ForEach expansion.
//
// The bound is the source probe's own limit in every current use, and this
// is the backstop for a future plan entry whose source is unbounded. An
// investigation that makes one API call per row of an unbounded list is not
// an investigation anybody accepts during an incident, and the failure mode
// of not bounding it is a wall of exec calls rather than a visible error.
const maxForEachCalls = 50

// expandProbe turns one plan entry into the calls it actually makes.
//
// An entry with no ForEach is itself. An entry with one is expanded over the
// source's recorded rows, and each expansion is a copy: the plan's own Args
// map is never written to, so two entries sharing a literal map cannot see
// each other's arguments.
func expandProbe(entry DomainProbe, rowsByTool map[string][]map[string]any) []probeCall {
	if entry.ForEach == nil {
		return []probeCall{{tool: entry.Tool, args: entry.Args}}
	}
	rows, sourceRan := rowsByTool[entry.ForEach.Source]
	if !sourceRan {
		// The source either was not read-only, was not registered, or
		// failed. All three mean this entry has nothing to iterate, and
		// guessing a target here is precisely what the whole file refuses
		// to do — so no call is made, and the proposals that would have
		// needed it stay absent.
		return nil
	}
	if len(rows) > maxForEachCalls {
		rows = rows[:maxForEachCalls]
	}
	calls := make([]probeCall, 0, len(rows))
	for _, row := range rows {
		args := make(map[string]any, len(entry.Args)+len(entry.ForEach.Columns))
		for key, value := range entry.Args {
			args[key] = value
		}
		// A row that cannot supply a named column is skipped rather than
		// called with a blank argument: `k8s.pvc_usage` with an empty pvc
		// would measure whichever namespace the credential defaults to,
		// and its answer would be filed under this claim's name.
		complete := true
		for arg, column := range entry.ForEach.Columns {
			value, ok := probeCell(row, column)
			if !ok || value == "" {
				complete = false
				break
			}
			args[arg] = value
		}
		if !complete {
			continue
		}
		calls = append(calls, probeCall{tool: entry.Tool, args: args})
	}
	return calls
}

// probeCell reads one column of a probe row as a string, for the argument
// values a ForEach expansion needs. Numbers and booleans are rendered rather
// than dropped, so a column that arrives as a JSON number still names a
// resource.
func probeCell(row map[string]any, column string) (string, bool) {
	value, present := row[column]
	if !present || value == nil {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed), true
	case float64, int, int64, bool:
		return fmt.Sprint(typed), true
	default:
		return "", false
	}
}

// probeIsReadOnly refuses a probe whose tool grades itself above the
// ceiling, and one that is not registered at all — the adapter for this
// domain was never wired, which is a fact about the deployment rather than
// about the incident.
func probeIsReadOnly(tools loop.ToolCaller, name string) bool {
	spec, ok := tools.LookupTool(name)
	if !ok {
		return false
	}
	return !riskExceeds(strings.TrimSpace(spec.RiskLevel), probeRiskCeiling)
}

// riskExceeds compares an "L<n>" grade against the ceiling. An unparseable
// grade is treated as exceeding: a tool that will not say what it does is
// not one to call unprompted.
func riskExceeds(grade, ceiling string) bool {
	level, ok := strings.CutPrefix(grade, "L")
	if !ok {
		return true
	}
	got := 0
	for _, r := range level {
		if r < '0' || r > '9' {
			return true
		}
		got = got*10 + int(r-'0')
	}
	ceilingLevel := 0
	for _, r := range strings.TrimPrefix(ceiling, "L") {
		if r < '0' || r > '9' {
			return true
		}
		ceilingLevel = ceilingLevel*10 + int(r-'0')
	}
	return got > ceilingLevel
}

// probeRowCount reads the row count an adapter reported, so the evidence
// item carries the same Count the promql and logql items do.
func probeRowCount(result any) int {
	rows, ok := result.(map[string]any)
	if !ok {
		return 0
	}
	switch count := rows["count"].(type) {
	case int:
		return count
	case int64:
		return int(count)
	case float64:
		return int(count)
	default:
		return 0
	}
}

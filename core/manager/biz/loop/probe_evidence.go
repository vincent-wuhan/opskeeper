package loop

// Reading the rows a domain probe recorded.
//
// A probe stores what the adapter returned verbatim, which for every read
// tool in this codebase is the same envelope: {"rows": [...], "count": n,
// "summary": "..."}. That envelope is an adapter convention, not a contract,
// so it is decoded leniently and a shape that does not fit yields no rows
// rather than a guess.
//
// The rule for every reader here is the one the pg resolvers already follow:
// one candidate resolves, several refuse, none refuses. A proposal that
// names "the slow query" when the log recorded four is not a proposal.

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ProbeRows returns every row the evidence chain records for one tool.
//
// The tool name is matched exactly, the way every other vocabulary in this
// package is keyed: a probe that drifts to a name stops resolving rather
// than resolving against the wrong evidence.
//
// Rows from every matching item are concatenated, not just the first item's.
// A probe may legitimately be called more than once — `k8s.pvc_usage` runs
// once per PersistentVolumeClaim the cluster reported — and each of those
// calls is a separate recorded observation. Returning only the first would
// mean a chain measuring five volumes answers questions about one of them,
// silently: the one volume it happened to measure would not be a worse
// answer than a wrong one, it would just be a different question from the
// one being asked. When a probe was called once, which is every case before
// the per-row expansion existed, this returns exactly what it always did.
func ProbeRows(evidence []EvidenceItem, tool string) []map[string]any {
	var out []map[string]any
	for _, item := range evidence {
		if item.Tool != tool {
			continue
		}
		out = append(out, probeRowsFrom(item.Value)...)
	}
	return out
}

func probeRowsFrom(value any) []map[string]any {
	switch v := value.(type) {
	case map[string]any:
		rows, ok := v["rows"]
		if !ok {
			return nil
		}
		return probeRowsFrom(rows)
	case map[string]string:
		return nil
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, entry := range v {
			if row, ok := entry.(map[string]any); ok {
				out = append(out, row)
			}
		}
		return out
	case string:
		var rows []map[string]any
		if err := json.Unmarshal([]byte(v), &rows); err != nil {
			return nil
		}
		return rows
	case json.RawMessage:
		return probeRowsFrom(string(v))
	default:
		return nil
	}
}

// SingleCandidate returns the one value a set of rows agrees on.
//
// It is the shared shape of every "which one does this act on" question:
// the table that is bloating, the node that is NotReady, the rollout that
// is wedged. When the rows name exactly one, that is the answer. When they
// name several, the caller gets a refusal naming them rather than a pick —
// because choosing among a table that is 60% dead and one that is 25% dead
// is a judgement about blast radius, and blast radius is what the approval
// step is for.
//
// describe is used to render each candidate in the refusal, so the operator
// reading it sees the values they would have weighed.
func SingleCandidate(rows []map[string]any, key string, describe func(map[string]any) string) (string, bool) {
	values := map[string]struct{}{}
	var order []string
	for _, row := range rows {
		value, ok := rowString(row, key)
		if !ok || value == "" {
			continue
		}
		if _, seen := values[value]; seen {
			continue
		}
		values[value] = struct{}{}
		order = append(order, value)
	}
	switch len(values) {
	case 0:
		return "", false
	case 1:
		for value := range values {
			return value, true
		}
	}
	return "", false
}

// RowFloat reads a numeric field, accepting the shapes a JSON round trip
// produces.
func RowFloat(row map[string]any, key string) (float64, bool) {
	switch v := row[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// RowBool reads a boolean field, tolerating the string form some adapters
// and JSON round trips produce.
func RowBool(row map[string]any, key string) (bool, bool) {
	switch v := row[key].(type) {
	case bool:
		return v, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return b, err == nil
	default:
		return false, false
	}
}

// SortedValues returns a map's keys in a stable order, for refusal
// messages that must not shuffle between runs.
func SortedValues(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

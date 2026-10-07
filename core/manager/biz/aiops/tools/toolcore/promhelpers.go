package toolcore

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
)

// promhelpers.go — the PromQL value plumbing that more than one tool asks
// for.
//
// These five lived in analyze_database_status.go, which is where the first
// caller was. The metric catalog needs exactly the same three: read an
// instant vector out of a promquery result, and build the label selector
// that asks for one series. That made the database analyzer a dependency
// of the metric catalog for no reason beyond file order — neither tool is
// the other one's implementation, and neither would notice if the other
// were deleted.
//
// So they sit beside PromQuerier, which is the thing they operate on.

// PromInstantValue is one labelled sample of an instant vector.
type PromInstantValue struct {
	Metric map[string]string
	Value  float64
}

// InstantValues flattens an instant vector into its samples, skipping anything
// whose value is not a number. This is the only place that shape is read.
func InstantValues(res *promquery.InstantResult) []PromInstantValue {
	if res == nil || len(res.Result) == 0 {
		return nil
	}
	switch res.ResultType {
	case "vector", "":
		var rows []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		}
		if err := json.Unmarshal(res.Result, &rows); err != nil {
			return nil
		}
		out := make([]PromInstantValue, 0, len(rows))
		for _, row := range rows {
			v, ok := parsePromValue(row.Value)
			if !ok {
				continue
			}
			out = append(out, PromInstantValue{Metric: row.Metric, Value: v})
		}
		return out
	case "scalar":
		var row []interface{}
		if err := json.Unmarshal(res.Result, &row); err != nil {
			return nil
		}
		v, ok := parsePromValue(row)
		if !ok {
			return nil
		}
		return []PromInstantValue{{Value: v}}
	default:
		return nil
	}
}

func parsePromValue(raw []interface{}) (float64, bool) {
	if len(raw) < 2 {
		return 0, false
	}
	switch v := raw[1].(type) {
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	case float64:
		return v, true
	default:
		return 0, false
	}
}

// LabelSelector renders a label map as a PromQL matcher, sorted so the same
// labels always produce the same string (a selector that reorders is a
// cache miss on the Prometheus side).
func LabelSelector(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, EscapePromLabelValue(labels[k])))
	}
	return strings.Join(parts, ",")
}

// EscapePromLabelValue quotes a label value for a PromQL matcher.
func EscapePromLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

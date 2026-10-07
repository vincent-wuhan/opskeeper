package topology

// promql.go is the query vocabulary rank_edges and find_outlier_edges
// share.
//
// The closed set of metrics, the z-score expression, and the decoding of a
// Prometheus range into per-edge numbers were written once for the upcall
// executors and are used identically by the batch BaseTools. Splitting the
// package without splitting these would have left both halves reaching
// sideways for the other's helpers, which is the coupling this extraction
// exists to remove.

import (
	"encoding/json"
	"strconv"
)

// RankMetricExpr returns the PromQL fragment that yields a per-edge
// scalar for the requested rank-by name. The closed set must stay in
// sync with manager/biz/alert.metricExprFor so an LLM-driven rank uses
// the same vocabulary as alert thresholds.
func RankMetricExpr(by string) (expr, label string, ok bool) {
	switch by {
	case "cpu":
		return `100 * (1 - avg by (edge_id) (rate(node_cpu_seconds_total{mode="idle"}[5m])))`, "cpu_pct", true
	case "mem":
		return `100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`, "mem_pct", true
	case "disk":
		return `100 * (1 - node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"})`, "disk_used_pct", true
	case "load":
		return `node_load1`, "load1", true
	case "composite":
		// Mean of cpu/mem/disk in percent. Wrapped with avg by(edge_id)
		// because the underlying series carry differing label shapes
		// (cpu has a mode label aggregated away; mem has none; disk
		// carries a mountpoint filter). Aligning on edge_id collapses
		// to one row per host.
		return `(` +
			`avg by (edge_id) (100 * (1 - avg by (edge_id, instance) (rate(node_cpu_seconds_total{mode="idle"}[5m]))))` +
			` + avg by (edge_id) (100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes))` +
			` + avg by (edge_id) (100 * (1 - node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"}))` +
			`) / 3`, "composite_pct", true
	}
	return "", "", false
}

// promRangeSeries is the per-series envelope inside a Prom matrix
// response. Decoded only as much as we need.
type promRangeSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
}

// DecodeRankSeries pulls (edge_id, last-value) out of a matrix Prom
// response. Series without a numeric edge_id label are skipped: we have
// no way to map them back to an Edge row.
func DecodeRankSeries(res interface { /* satisfied by *promquery.InstantResult */
}, metricLabel string) ([]RankEdgeRow, error) {
	type irShape struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	// The PromQuerier returns a *promquery.InstantResult; rather than
	// hard-binding to that concrete type here (which would create a
	// dependency cycle issue if the package layout shifts), re-encode
	// and re-decode through json. This costs one extra marshal; the
	// payload is small (<= 50 rows).
	blob, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	var ir irShape
	if err := json.Unmarshal(blob, &ir); err != nil {
		return nil, err
	}
	switch ir.ResultType {
	case "matrix":
		var series []promRangeSeries
		if err := json.Unmarshal(ir.Result, &series); err != nil {
			return nil, err
		}
		rows := make([]RankEdgeRow, 0, len(series))
		for _, s := range series {
			eid, ok := numericLabel(s.Metric, "edge_id")
			if !ok {
				continue
			}
			val, ok := lastSampleValue(s.Values)
			if !ok {
				continue
			}
			rows = append(rows, RankEdgeRow{EdgeID: eid, Value: val, Metric: metricLabel})
		}
		return rows, nil
	case "vector":
		var series []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		if err := json.Unmarshal(ir.Result, &series); err != nil {
			return nil, err
		}
		rows := make([]RankEdgeRow, 0, len(series))
		for _, s := range series {
			eid, ok := numericLabel(s.Metric, "edge_id")
			if !ok {
				continue
			}
			val, ok := promSampleFloat(s.Value[1])
			if !ok {
				continue
			}
			rows = append(rows, RankEdgeRow{EdgeID: eid, Value: val, Metric: metricLabel})
		}
		return rows, nil
	}
	return nil, nil
}

// numericLabel pulls a uint64-ish label out of the Prom metric map. A
// non-numeric or missing label returns ok=false.
func numericLabel(m map[string]string, key string) (uint64, bool) {
	v, ok := m[key]
	if !ok || v == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// lastSampleValue returns the last numeric sample of a Prom matrix series.
// Each sample is [unix_ts, "<value>"]; the value comes back as a string
// inside JSON.
func lastSampleValue(values [][2]any) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	last := values[len(values)-1]
	return promSampleFloat(last[1])
}

func promSampleFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return x, true
	}
	return 0, false
}

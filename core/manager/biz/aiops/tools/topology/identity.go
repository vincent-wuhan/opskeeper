package topology

// identity.go is what these two tools are called and what they accept.
//
// rank_edges and find_outlier_edges each have an in-process BaseTool and
// an upcall executor in the parent package, and both surfaces answer to
// the same wire name. Keeping one declaration here is what stops the two
// halves from drifting: a model that was told one shape and gets another
// has no way to tell it is the same tool.
//
// The PromQL that backs both lives in promql.go for the same reason.

import (
	"encoding/json"
	"time"
)

// ToolNameFindOutlierEdges is the stable wire name the LLM sees.
const ToolNameFindOutlierEdges = "find_outlier_edges"

// FindOutlierEdgesDescription pushes the model toward this tool when the
// question is about which edges deviate from the fleet baseline.
const FindOutlierEdgesDescription = "Find edges whose metric value is more than N standard deviations above the fleet mean. " +
	"Use this for 'who's an outlier on cpu/mem/disk' style questions. Default sigma is 2."

// FindOutlierEdgesSchema is the JSON Schema of the tool's argument object.
var FindOutlierEdgesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "metric": {
      "type": "string",
      "enum": ["cpu", "mem", "disk"],
      "description": "Which closed-set host metric to compare across the fleet."
    },
    "sigma": {
      "type": "number",
      "minimum": 0.5,
      "maximum": 10,
      "description": "z-score threshold (default 2). Edges with z > sigma are returned."
    }
  },
  "required": ["metric"]
}`)

// FindOutlierEdgesArgs is the typed form of FindOutlierEdgesSchema.
type FindOutlierEdgesArgs struct {
	Metric string  `json:"metric"`
	Sigma  float64 `json:"sigma,omitempty"`
}

// OutlierEdgeRow is one outlier hit.
type OutlierEdgeRow struct {
	EdgeID   uint64  `json:"edge_id"`
	EdgeName string  `json:"edge_name"`
	ZScore   float64 `json:"z_score"`
	Metric   string  `json:"metric"`
}

const OutlierCallTimeout = 30 * time.Second

// ToolNameRankEdges is the stable wire name the LLM sees.
const ToolNameRankEdges = "rank_edges"

// RankEdgesDescription pushes the model toward this tool whenever the
// question is "which top/bottom N machines by a host metric".
const RankEdgesDescription = "Rank edges by a closed-set host metric (cpu, mem, disk, load, composite) and return top-N or bottom-N. " +
	"Use this for 'find the most/least loaded N machines' style questions. " +
	"Composite is the unweighted mean of cpu_pct + mem_pct + disk_used_pct."

// RankEdgesSchema is the JSON Schema of the tool's argument object.
var RankEdgesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "by": {
      "type": "string",
      "enum": ["cpu", "mem", "disk", "load", "composite"],
      "description": "Which metric to rank on. cpu/mem/disk are the same closed-set the alert evaluator uses."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 50,
      "description": "How many edges to return (default 5)."
    },
    "direction": {
      "type": "string",
      "enum": ["top", "bottom"],
      "description": "top = highest values; bottom = lowest values (default top)."
    }
  },
  "required": ["by"]
}`)

// RankEdgesArgs is the typed form of RankEdgesSchema.
type RankEdgesArgs struct {
	By        string `json:"by"`
	Limit     int    `json:"limit,omitempty"`
	Direction string `json:"direction,omitempty"`
}

// RankEdgeRow is one ranked entry. EdgeName is best-effort: when the
// label set on the Prom result doesn't contain a numeric edge_id we
// can decode, Name stays empty.
type RankEdgeRow struct {
	EdgeID   uint64  `json:"edge_id"`
	EdgeName string  `json:"edge_name"`
	Value    float64 `json:"value"`
	Metric   string  `json:"metric"`
}

const RankEdgesCallTimeout = 30 * time.Second

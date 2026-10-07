package host

// identity.go is what a tool is, as opposed to what it does.
//
// get_host_load and get_host_processes each have two execution surfaces:
// the batch-first BaseTool the in-process agent loop presents, and the
// single-shot executor the node agent reaches through the upcall. They
// answer to the same wire name, so the name, the description and the
// single-shot schema are one declaration, not two that have to be kept in
// step. When the two surfaces disagree about what a tool is called, the
// model is the one that finds out.
//
// The batch schema lives next to the batch tool that serves it, in
// host_load_basetool.go and host_processes_basetool.go.

import (
	"encoding/json"
	"time"
)

// ToolNameGetHostLoad is the stable wire name the LLM sees for this tool.
const ToolNameGetHostLoad = "get_host_load"

// GetHostLoadDescription is the single-sentence description the model reads
// when deciding whether to call this tool. Accuracy here directly affects
// dispatch quality; keep it concrete.
const GetHostLoadDescription = "Return current CPU percent, memory percent, and 1/5/15-minute load averages of the named edge host."

// GetHostLoadSchema is the JSON Schema of the tool's argument object.
var GetHostLoadSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "edge_name": {
      "type": "string",
      "description": "Name of the edge as set when the edge was created."
    }
  },
  "required": ["edge_name"]
}`)

// GetHostLoadArgs is the typed form of GetHostLoadSchema.
type GetHostLoadArgs struct {
	EdgeName string `json:"edge_name"`
}

// HostLoadCallTimeout caps how long a single tool dispatch may wait on
// the frontier round-trip. We derive a child ctx with this deadline so
// long-running edge calls cannot wedge the agent loop.
const HostLoadCallTimeout = 15 * time.Second

// ToolNameGetProcessList is the stable wire name the LLM sees for this tool.
const ToolNameGetProcessList = "get_host_processes"

// GetProcessListDescription is the single-sentence description the model
// reads when deciding whether to call this tool.
const GetProcessListDescription = "Return the top-N processes on the named edge host, sorted by CPU or memory usage."

// GetProcessListSchema is the JSON Schema of the tool's argument object.
var GetProcessListSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "edge_name": {
      "type": "string",
      "description": "Name of the edge as set when the edge was created."
    },
    "top_n": {
      "type": "integer",
      "minimum": 1,
      "maximum": 100,
      "description": "How many processes to return (default 10)."
    },
    "sort_by": {
      "type": "string",
      "enum": ["cpu", "mem"],
      "description": "Sort key: cpu or mem (default cpu)."
    }
  },
  "required": ["edge_name"]
}`)

// GetProcessListArgs is the typed form of GetProcessListSchema.
type GetProcessListArgs struct {
	EdgeName string `json:"edge_name"`
	TopN     uint32 `json:"top_n"`
	SortBy   string `json:"sort_by"`
}

// ProcessListCallTimeout caps how long a single dispatch may wait. Same
// rationale as HostLoadCallTimeout.
const ProcessListCallTimeout = 15 * time.Second

package toolcore

import (
	"context"
	"encoding/json"
)

// Caller is the narrow seam a tool needs from the frontierbound SDK
// wrapper. Declaring it here lets tests inject a fake without standing up
// a real Client. Any frontierbound.Client value satisfies it via its own
// Call method.
type Caller interface {
	Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error)
}

// ExecuteResult is what a tool executor returns: the JSON payload to feed
// back into the LLM plus (optionally) the device id the call targeted,
// which the agent uses to populate the chat_tool_calls.device_id audit
// column.
//
// Post-split (May 2026): renamed EdgeID -> DeviceID. Numerically the values
// are the same — the legacy chat_tool_calls.edge_id column is kept as the
// storage column name (audit-only) but the semantic interpretation is the
// host device id.
type ExecuteResult struct {
	ResultJSON json.RawMessage
	DeviceID   *uint64
}

// Tool is one exposed action: its name, description, JSON Schema of its
// argument object, and the executor that fulfils a call.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Execute     func(ctx context.Context, args json.RawMessage) (ExecuteResult, error)
}

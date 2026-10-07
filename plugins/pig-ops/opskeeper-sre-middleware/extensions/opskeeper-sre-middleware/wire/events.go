// Package wire holds the transport-level DTOs that cross a process or
// network boundary. These are contract surface: the web console parses
// them, and node agents emit them.
//
// Nothing in this package may change without a console change in the same
// change. Adding a field is additive; renaming or retyping one is breaking.
package wire

// StreamEventType is the discriminant on a streaming frame.
//
// The set is closed. A node agent or an adapter that needs a new frame adds
// a type here and a renderer in the console; it does not invent a string.
type StreamEventType string

const (
	// StreamAssistantStart fires before the model produces any content.
	StreamAssistantStart StreamEventType = "assistant_start"
	// StreamAssistantDelta carries one incremental content chunk. This is
	// the token-level frame; a non-streaming producer may omit it entirely
	// and the console renders the same result from assistant_end.
	StreamAssistantDelta StreamEventType = "assistant_delta"
	// StreamAssistantEnd carries the full turn content plus the turn
	// number and the count of tool calls still outstanding.
	StreamAssistantEnd StreamEventType = "assistant_end"
	// StreamToolStart fires when a tool is admitted and begins.
	StreamToolStart StreamEventType = "tool_start"
	// StreamToolUpdate carries incremental progress from a long-running
	// tool. Optional; a tool that reports no progress never emits it.
	StreamToolUpdate StreamEventType = "tool_update"
	// StreamToolEnd fires when a tool settles, with its terminal status.
	StreamToolEnd StreamEventType = "tool_end"
	// StreamDone fires once on terminal success, carrying the turn's usage.
	StreamDone StreamEventType = "done"
	// StreamError fires on terminal failure.
	StreamError StreamEventType = "error"
	// StreamTaskNotification reports a background worker reaching a
	// terminal state, so the console can render its result inline.
	StreamTaskNotification StreamEventType = "task_notification"
	// StreamApprovalPending reports a gated call awaiting a human. The
	// console renders an approval affordance bound to the request id.
	StreamApprovalPending StreamEventType = "approval_pending"
	// StreamApprovalResolved reports a human's decision landing, so an
	// in-flight call can resume or surface its refusal.
	StreamApprovalResolved StreamEventType = "approval_resolved"
)

// ToolStatus is a tool call's terminal classification.
type ToolStatus string

const (
	ToolSuccess ToolStatus = "success"
	ToolError   ToolStatus = "error"
	ToolTimeout ToolStatus = "timeout"
	// ToolBlocked is a distinct terminal state from error: the host policy
	// gate refused the call before it ran. The console shows these
	// differently, so a blocked call must never be reported as an error.
	ToolBlocked ToolStatus = "blocked"
)

// StreamEvent is the envelope every streaming frame uses.
type StreamEvent struct {
	Type StreamEventType `json:"type"`
	// SessionID scopes the frame to one conversation. Required on every
	// frame so a multiplexed connection can demultiplex.
	SessionID string `json:"session_id"`
	// Iteration is the turn number within the session, 1-based.
	Iteration int `json:"iteration,omitempty"`
	// Seq is a per-session monotonic counter. A consumer that sees a gap
	// knows a frame was dropped rather than that the turn ended.
	Seq int64 `json:"seq,omitempty"`

	Assistant *AssistantFrame `json:"assistant,omitempty"`
	Tool      *ToolFrame      `json:"tool,omitempty"`
	Done      *DoneFrame      `json:"done,omitempty"`
	Error     *ErrorFrame     `json:"error,omitempty"`
	Task      *TaskFrame      `json:"task,omitempty"`
	Approval  *ApprovalFrame  `json:"approval,omitempty"`
}

// AssistantFrame carries assistant turn content.
type AssistantFrame struct {
	// Content is the full turn text on assistant_end, and the incremental
	// chunk on assistant_delta.
	Content string `json:"content"`
	// MessageID identifies the persisted row. Empty on a delta.
	MessageID string `json:"message_id,omitempty"`
	// PendingToolCalls is the number of tool calls the model requested that
	// have not yet settled.
	PendingToolCalls int    `json:"pending_tool_calls,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
}

// ToolFrame carries a tool call's lifecycle state.
type ToolFrame struct {
	ToolCallID string     `json:"tool_call_id"`
	Name       string     `json:"name"`
	Status     ToolStatus `json:"status,omitempty"`
	Class      string     `json:"class,omitempty"`
	DeviceID   *uint64    `json:"device_id,omitempty"`
	ArgsJSON   string     `json:"args_json,omitempty"`
	ResultJSON string     `json:"result_json,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  string     `json:"started_at,omitempty"`
	EndedAt    string     `json:"ended_at,omitempty"`
	DurationMs int64      `json:"duration_ms,omitempty"`
}

// DoneFrame carries the terminal success payload.
type DoneFrame struct {
	Iterations int         `json:"iterations"`
	ToolCalls  int         `json:"tool_calls"`
	Usage      *UsageFrame `json:"usage,omitempty"`
}

// UsageFrame is the token accounting echoed to the console. It is
// aggregate-per-turn: a console must not need to sum assistant frames to
// show a cost.
type UsageFrame struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens,omitempty"`
	// CacheWriteTokens is the prompt half a provider wrote into its cache
	// during this turn. It is billable prompt work, so a frame that omits it
	// under-reports the turn's cost for exactly the providers that cache
	// (Anthropic, DeepSeek) — the ones where it is largest.
	//
	// It was absent while ports.TranscriptUsage already carried it and the
	// stored transcript already reported it, so the live frame and the
	// reloaded history disagreed for the same turn.
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	Model            string  `json:"model,omitempty"`
}

// ErrorFrame carries a terminal failure.
type ErrorFrame struct {
	// Code is stable and machine-readable. Message is for humans and may
	// change; branch on Code.
	Code    string `json:"code"`
	Message string `json:"message"`
	// Retryable tells the console whether to offer a retry affordance.
	Retryable bool `json:"retryable"`
}

// TaskFrame reports a background worker reaching a terminal state.
type TaskFrame struct {
	WorkerID string `json:"worker_id"`
	Agent    string `json:"agent,omitempty"`
	Status   string `json:"status"`
	Summary  string `json:"summary,omitempty"`
}

// ApprovalFrame carries a gated call awaiting or resolving a human.
type ApprovalFrame struct {
	RequestID string `json:"request_id"`
	// Digest binds the decision to this exact call. The console echoes it
	// back with its decision; the host recomputes and rejects a mismatch.
	Digest string `json:"digest"`
	Tool   string `json:"tool"`
	Class  string `json:"class,omitempty"`
	// Summary is the operator-facing one-liner.
	Summary string `json:"summary,omitempty"`
	// BlastRadius is the host-assessed reach, shown so the operator can
	// judge it.
	BlastRadius string `json:"blast_radius,omitempty"`
	Target      string `json:"target,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	// Decision is set on approval_resolved only. Its vocabulary is the
	// closed pair "grant" and "deny" — nothing else. A console branches on
	// it, so every refusal path, whatever the underlying cause, resolves to
	// "deny" and reports the cause in Note. Emitting the gate's own reason
	// vocabulary here instead would make the console special-case three
	// spellings of the same outcome.
	Decision string `json:"decision,omitempty"`
	// Note explains a denial in the gate's own vocabulary ("expired",
	// "digest mismatch") for the operator and the audit trail.
	Note string `json:"note,omitempty"`
}

package domain

import (
	"context"
	"time"
)

// This file is the answer to a measurement, and the measurement is the whole
// reason it exists.
//
// The tool-call audit seam is two event shapes and a two-method interface. It
// is declared in the decorators package inside the aiops domain, and until
// decision 238 exactly one thing outside that domain needed it: the MCP
// server's audit sink, which turns an MCP tool invocation into the same
// pending/success pair a native call writes to `chat_tool_calls`.
//
// That is a package-shaped boundary wearing an interface's clothes. The sink
// held `decorators.ToolStartEvent` in its signature, so satisfying the seam
// meant importing the entire decorators package — governance, review gates,
// rate limiters, untrusted-output marking, all of it — to name two structs
// with no behaviour in them.
//
// So the shapes moved here, where the plan already puts event contracts
// (plan §2.1: "wire DTO, event contracts"). Nothing about the fields changed:
// these are the same two structs, field for field, so every call site and
// every test fake keeps compiling against the alias the decorators package
// leaves behind.
//
// They carry no GORM tag and no entity identity, for the same reason
// `EdgePresence` does not: what a sink receives about a call is a receipt,
// not a row.

// ToolStartEvent is what the audit sink sees at the start of a call —
// captures the inputs needed to write the pending chat_tool_calls row.
//
// `DeviceID` is a pointer because the column is nullable and a call made
// outside a device's context has no device to name. It is the one field whose
// zero value would be a lie if it were flattened to 0.
type ToolStartEvent struct {
	ToolName  string
	ArgsJSON  string
	Tenant    string
	UserID    uint64
	DeviceID  *uint64
	StartedAt time.Time
}

// ToolEndEvent is what the audit sink sees at the end of a call —
// captures the result/error needed to update the chat_tool_calls row to
// status=success/error/timeout.
type ToolEndEvent struct {
	ResultJSON string
	Err        error // nil on success
	EndedAt    time.Time
	Duration   time.Duration
}

// ToolCallAuditSink is the seam the audit decorator writes through.
//
// It is not named `AuditSink` on purpose. That name is already spoken for six
// times in this repository — `ports.AuditSink` (the host's write path to the
// ledger), `skill.AuditSink`, `middleware/adapter/decorator.AuditSink`,
// `decorators.AuditSink` (the alias this interface replaces), the MCP sink
// struct of the same name, and the agentteams HTTP middleware's own. Six
// declarations, five different meanings, and a seventh would have made the
// trap list in the ledger a list of names rather than a list of shapes.
//
// Two methods, and the second one's error is explicitly not the call's
// outcome: an audit write that fails must not turn a successful tool call into
// a failed one. That rule lives in the interface's documentation rather than
// in its signature because no signature can say it.
type ToolCallAuditSink interface {
	// OnToolStart records the start of a tool invocation. id is an opaque
	// correlation token returned to the caller and passed back to OnToolEnd;
	// implementations typically map it to the chat_tool_calls.id (UUID).
	// When the sink wants to short-circuit the call (e.g. quota exceeded)
	// it returns a non-nil error and the decorator skips InvokableRun
	// entirely, surfacing the error.
	OnToolStart(ctx context.Context, ev ToolStartEvent) (id string, err error)

	// OnToolEnd records the end of a tool invocation. id is the value
	// returned from OnToolStart. Errors here are best-effort and do NOT
	// override the tool's own outcome.
	OnToolEnd(ctx context.Context, id string, ev ToolEndEvent) error
}

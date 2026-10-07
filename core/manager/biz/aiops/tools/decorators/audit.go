package decorators

import (
	"context"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// The three names below are aliases, not declarations.
//
// They used to be declared here, and one consumer outside this package needed
// them: the MCP server's audit sink, which writes the same pending/success
// pair a native call writes. Holding `decorators.ToolStartEvent` in its method
// signature meant satisfying a two-struct seam by importing the whole
// decorators package — governance, review gates, rate limiters and all — so
// the `mcp -> aiops` edge existed to carry two structs with no behaviour in
// them (decision 238).
//
// The shapes moved to core/domain, which the plan already gives event
// contracts to (plan §2.1). The field lists did not change, and because a Go
// alias *is* the type it names rather than a copy of it, every call site and
// every test fake in this repository keeps compiling unchanged — including
// the ones that were never part of the edge being cut.

// AuditSink is the interface-only seam that the audit decorator writes
// through. The production binding (later PR) implements this against
// the chat_tool_calls repo; tests inject a fake. The seam is here
// (instead of importing biz/aiops.SessionRepo) so decorators stay free
// of biz repo coupling — 不变量 (模块边界).
//
// Callback 链 spec for OnToolStart / OnToolEnd — this
// decorator is the synchronous, in-process equivalent for the parallel
// BaseTool path. When the agent loop migrates to eino + ToolsNode the
// implementation may switch to eino callbacks; the AuditSink contract
// stays the same.
//
// It is an alias rather than a second declaration, which is the point:
// two identical interfaces with two identical method sets are two types, and
// a sink satisfying one would not satisfy the other.
type AuditSink = domain.ToolCallAuditSink

// ToolStartEvent is what the audit sink sees at the start of a call —
// captures the inputs needed to write the pending chat_tool_calls row.
type ToolStartEvent = domain.ToolStartEvent

// ToolEndEvent is what the audit sink sees at the end of a call —
// captures the result/error needed to update the chat_tool_calls row to
// status=success/error/timeout.
type ToolEndEvent = domain.ToolEndEvent

// AuditTool wraps inner so OnToolStart fires before InvokableRun and
// OnToolEnd fires after (regardless of inner error). —
// AuditTool 装饰器层。
type AuditTool struct {
	inner basetool.BaseTool
	sink  AuditSink
}

// WithAudit returns inner wrapped to emit start + end events through
// sink. A nil sink is a no-op pass-through (so production wiring can
// disable audit in tests/dev without conditional decorators at the
// call site).
func WithAudit(inner basetool.BaseTool, sink AuditSink) basetool.BaseTool {
	if sink == nil {
		return inner
	}
	return &AuditTool{inner: inner, sink: sink}
}

// Info passes through — auditing is invocation-only, the schema is
// public.
func (a *AuditTool) Info(ctx context.Context) (*basetool.ToolInfo, error) {
	return a.inner.Info(ctx)
}

// InvokableRun emits OnToolStart, runs the inner tool, then emits
// OnToolEnd. OnToolStart errors abort the call (returned as-is so
// quota / preflight failures surface unmangled). OnToolEnd errors are
// silently swallowed to honour the "audit must not fail the tool"
// invariant — they reach observability via the sink's own logging.
func (a *AuditTool) InvokableRun(ctx context.Context, argsJSON string, opts ...basetool.InvokeOption) (string, error) {
	resolved := basetool.ResolveOptions(opts)

	// Resolve tool name once (Info must be cheap; the closure-style
	// tools in registry.go return constant ToolInfo without I/O).
	name := ""
	if info, err := a.inner.Info(ctx); err == nil && info != nil {
		name = info.Name
	}

	startedAt := time.Now().UTC()
	id, err := a.sink.OnToolStart(ctx, ToolStartEvent{
		ToolName:  name,
		ArgsJSON:  argsJSON,
		Tenant:    resolved.Tenant,
		UserID:    resolved.UserID,
		DeviceID:  resolved.DeviceID,
		StartedAt: startedAt,
	})
	if err != nil {
		return "", err
	}

	out, runErr := a.inner.InvokableRun(ctx, argsJSON, opts...)
	endedAt := time.Now().UTC()
	_ = a.sink.OnToolEnd(ctx, id, ToolEndEvent{
		ResultJSON: out,
		Err:        runErr,
		EndedAt:    endedAt,
		Duration:   endedAt.Sub(startedAt),
	})
	return out, runErr
}

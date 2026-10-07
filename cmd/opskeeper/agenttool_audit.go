package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// agentToolAuditEmitter is the slice of biz/audit.Usecase this file needs.
// Declared as an interface so the recorder can be tested without a
// repository, and so the composition root stays the only place that knows
// which writer it got.
type agentToolAuditEmitter interface {
	Emit(ctx context.Context, ev auditport.Event)
}

// agentToolAudit writes one row per agent.tool upcall.
//
// Why one row for the whole call rather than a start/end pair like the MCP
// surface: this channel's calls are dispatched synchronously inside a
// single RPC, and the node learns the outcome from the RPC response — it
// has no second message to correlate against. A start row without an end
// row is the case an node ledger cannot self-heal, and a node that loses
// the link mid-call would leave it permanently half-written. The cost is
// that a call still in flight is not visible; for a read-only proxy whose
// worst outcome is a hung query that the per-call timeout already bounds,
// that is the right trade.
type agentToolAudit struct {
	emitter agentToolAuditEmitter
}

// agentToolCall is everything the recorder needs about one upcall. It is
// filled in by RunAgentTool at the single point where both surfaces have
// already converged, so neither branch can forget to record.
type agentToolCall struct {
	EdgeID    uint64
	SessionID string
	Tool      string
	Surface   string // "aiops" or "middleware"
	Args      json.RawMessage
	Result    json.RawMessage
	Started   time.Time
	Duration  time.Duration

	// Denied separates "the call was refused before it ran" from "the
	// call ran and failed". Both are failures to the caller, but only one
	// of them is an attempt worth reading about on the console, and
	// folding them together would make a node probing for a write tool
	// look like a node whose database query broke.
	Denied bool
	Reason string // why it was denied; empty unless Denied
	Err    error
}

// record emits the row. It is a no-op when no emitter was wired, because
// a deployment that runs without an audit repository should still be able
// to serve tool calls — refusing them would trade a bookkeeping gap for an
// outage.
func (a *agentToolAudit) record(ctx context.Context, call agentToolCall) {
	if a == nil || a.emitter == nil {
		return
	}
	status := auditport.StatusSuccess
	switch {
	case call.Denied:
		status = auditport.StatusDenied
	case call.Err != nil:
		status = auditport.StatusFailure
	}

	payload := map[string]any{
		"surface":          call.Surface,
		"edge_id":          call.EdgeID,
		"session_id":       call.SessionID,
		"tool":             call.Tool,
		"arguments_sha256": hashAgentToolValue(call.Args),
		"result_sha256":    hashAgentToolValue(call.Result),
		"duration_ms":      call.Duration.Milliseconds(),
		"started_at":       call.Started,
	}
	if call.Denied {
		payload["denied_reason"] = call.Reason
	}
	if call.Err != nil {
		payload["error"] = call.Err.Error()
	}

	a.emitter.Emit(ctx, auditport.Event{
		Action:       auditport.ActionAgentToolCall,
		ResourceType: auditport.ResourceAgentTool,
		ResourceID:   call.Tool,
		ResourceName: call.Tool,
		Status:       status,
		RequestID:    newAgentToolCorrelationID(),
		ErrorMessage: agentToolErrorMessage(call),
		Payload:      payload,
	})
}

func agentToolErrorMessage(call agentToolCall) string {
	if call.Denied {
		return call.Reason
	}
	if call.Err != nil {
		return call.Err.Error()
	}
	return ""
}

// hashAgentToolValue fingerprints a value instead of storing it.
//
// The MCP sink hashes its arguments and results for the same reason, and
// the reason is stronger here: a middleware tool's result is a slice of a
// customer's database, and a PromQL result is a window into their
// traffic. An audit row is not the place for either. What an investigator
// actually needs is "did this node's agent ask the control plane to read
// pg.lock_waits at 03:12, and did it come back" — the answer to which is
// the same whether the payload is stored or fingerprinted, and the
// fingerprint is what lets two rows be compared for equality.
func hashAgentToolValue(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func newAgentToolCorrelationID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(buffer)
}

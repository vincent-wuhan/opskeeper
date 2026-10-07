package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The node plane's rows, on their way into the chain (决策 126).
//
// RecordAutonomyReplay above already writes a node's decisions here, so the
// shape of this problem is not new: the node holds rows on a machine that
// may be offline, the center appends them to one ordered chain, and neither
// side may invent order or content. What is new is the row. An autonomy row
// is always the same thirteen fields about one decision, written by one
// arbiter that cannot run without a phase. A node ledger row is written by
// three different components (the policy gate, the PiG run state, the plugin
// installer) against a vocabulary that lives in core/ports and that this
// package could not interpret — so the work here is a translation, and a
// translation is exactly where a silently dropped field becomes a lie.

// NodeLedgerRow is one row as a node wrote it. The shape moved to
// core/base/pkg/audit in decision 272 — a caller cannot hand this package a
// batch without naming the row, so the row is part of what the port has to
// carry. The alias keeps every spelling in this package unchanged.
//
// The two chain fields are deliberately absent: the node cannot compute a
// hash and is not asked to. A link is the center's to write, and a node that
// could sign one could forge a chain.
type NodeLedgerRow = auditport.NodeLedgerRow

// nodeActionMap is the whole translation, and it is a map rather than a
// fallthrough switch so that "unmapped" is a value the shape pass can test.
//
// It is total over core/ports' thirteen actions on purpose. Two of them
// (proposal_created, recovery_applied) have no writer on the node today, and
// mapping them anyway means the day a proposal is written on a node it
// reaches the chain instead of being refused as a string nobody recognises —
// a refusal the node counts and passes over, permanently.
var nodeActionMap = map[ports.AuditAction]struct {
	action string
	status string
}{
	ports.ActionToolCall:        {model.ActionNodeToolCall, model.StatusSuccess},
	ports.ActionToolBlocked:     {model.ActionNodeToolBlocked, model.StatusDenied},
	ports.ActionToolFailed:      {model.ActionNodeToolFailed, model.StatusFailure},
	ports.ActionApprovalRequest: {model.ActionNodeApprovalRequest, model.StatusSuccess},
	ports.ActionApprovalGrant:   {model.ActionNodeApprovalGrant, model.StatusSuccess},
	ports.ActionApprovalDeny:    {model.ActionNodeApprovalDeny, model.StatusDenied},
	ports.ActionAgentTurn:       {model.ActionNodeAgentTurn, model.StatusSuccess},
	ports.ActionModelCall:       {model.ActionNodeModelCall, model.StatusSuccess},
	ports.ActionPluginInstall:   {model.ActionNodePluginInstall, model.StatusSuccess},
	ports.ActionPluginRemove:    {model.ActionNodePluginRemove, model.StatusSuccess},
	ports.ActionPluginLoad:      {model.ActionNodePluginLoad, model.StatusSuccess},
	ports.ActionProposalCreate:  {model.ActionNodeProposalCreate, model.StatusSuccess},
	ports.ActionRecoveryApply:   {model.ActionNodeRecoveryApply, model.StatusSuccess},
}

// NodeLedgerResult reports how a batch was taken.
type NodeLedgerResult = auditport.NodeLedgerResult

// RecordNodeEntries writes a node's own ledger rows into the chain.
//
// The whole batch is shape-checked before the first row is written, and the
// storage then writes all of it or none of it, for the same reason
// RecordAutonomyReplay does: the chain is append-only with no dedupe key, so
// a prefix written and then failed would be written again by the node's
// all-or-nothing pump, and a duplicated tool call in a ledger of tool calls
// is worse than a late one.
//
// The two shape rules are a mapped action and a non-zero time, and both are
// about interpretation rather than completeness. An action this package
// cannot name is a row an operator could not filter or read; a row with no
// time is not evidence of when anything happened. Nothing else is
// required, because a rule the node cannot satisfy is a rule that refuses a
// legitimate batch and the node then counts it and moves on — the refusal
// is silent by construction, which is the worst property a ledger check can
// have. A missing actor or an empty target is carried in the payload as
// what it is.
//
// A caller with no chain configured still records, exactly as the autonomy
// path does: the rows land unchained rather than being lost because nobody
// configured a key.
func (u *Usecase) RecordNodeEntries(ctx context.Context, edgeID uint64, rows []NodeLedgerRow) (NodeLedgerResult, error) {
	if len(rows) == 0 {
		return NodeLedgerResult{}, nil
	}
	converted := make([]struct {
		row    NodeLedgerRow
		action string
		status string
	}, 0, len(rows))
	for _, r := range rows {
		mapped, ok := nodeActionMap[r.Action]
		if !ok {
			u.log.Warn("audit: node ledger batch refused; an action this build cannot name",
				slog.Uint64("edge_id", edgeID),
				slog.String("action", string(r.Action)),
				slog.Int("rows", len(rows)))
			return NodeLedgerResult{Rejected: len(rows)}, nil
		}
		if r.At.IsZero() {
			// Same reasoning, quieter: no clock is not a malformed row
			// worth dropping a whole batch over, but a row whose time is
			// the zero time is a row that claims something happened at the
			// beginning of time, and it is refused for that rather than
			// filed.
			u.log.Warn("audit: node ledger batch refused; a row with no time",
				slog.Uint64("edge_id", edgeID),
				slog.String("action", string(r.Action)),
				slog.Int("rows", len(rows)))
			return NodeLedgerResult{Rejected: len(rows)}, nil
		}
		converted = append(converted, struct {
			row    NodeLedgerRow
			action string
			status string
		}{row: r, action: mapped.action, status: mapped.status})
	}

	var res NodeLedgerResult
	for _, c := range converted {
		r := c.row
		payload := map[string]any{
			"at":     r.At.UTC().Format(time.RFC3339Nano),
			"action": string(r.Action),
			// The origin is explicit for the same reason the autonomy
			// path's is: nobody at the console pressed a button for this
			// row, and a reader who does not know that goes looking for
			// the operator who approved a tool call.
			"origin": "node_audit",
		}
		if r.Actor != "" {
			payload["actor"] = r.Actor
		}
		if r.Outcome != "" {
			payload["outcome"] = r.Outcome
		}
		if r.Class != "" {
			payload["class"] = r.Class
		}
		if len(r.Detail) > 0 {
			payload["detail"] = json.RawMessage(r.Detail)
		}
		_, err := u.EmitWithID(ctx, Event{
			Role:         "edge",
			Action:       c.action,
			ResourceType: model.ResourceEdge,
			ResourceID:   strconv.FormatUint(edgeID, 10),
			// The target is the tool or the plugin, and it is the thing
			// an operator searches for; the edge id stays in ResourceID so
			// that "everything this node did" is one filter.
			ResourceName: r.Target,
			Status:       c.status,
			Payload:      payload,
		})
		if err != nil {
			return res, fmt.Errorf("audit: record a node ledger row for edge %d: %w", edgeID, err)
		}
		res.Accepted++
	}
	return res, nil
}

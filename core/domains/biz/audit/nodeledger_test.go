package audit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	store "github.com/vincent-wuhan/opskeeper/core/domains/data/audit/store"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

func nodeRow(action ports.AuditAction) NodeLedgerRow {
	return NodeLedgerRow{
		At:      time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		Actor:   "operator@example.invalid",
		Action:  action,
		Target:  "host_bash",
		Outcome: "allowed",
		Class:   "write",
	}
}

// TestRecordNodeEntries_LandsInTheSameChainTheConsoleWrites is the floor
// for the whole route: a node's tool call has to end up in the same ledger,
// with a seal on it, as a release an operator pushed. A separate store
// would be a second kind of truth and no kind of evidence.
func TestRecordNodeEntries_LandsInTheSameChainTheConsoleWrites(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	res, err := uc.RecordNodeEntries(context.Background(), 42, []NodeLedgerRow{
		nodeRow(ports.ActionToolCall),
		nodeRow(ports.ActionToolBlocked),
	})
	if err != nil {
		t.Fatalf("RecordNodeEntries: %v", err)
	}
	if res.Accepted != 2 || res.Rejected != 0 {
		t.Fatalf("accepted %d rejected %d, want 2/0", res.Accepted, res.Rejected)
	}
	stored := allRows(t, db)
	if len(stored) != 2 {
		t.Fatalf("stored %d rows, want 2", len(stored))
	}
	for _, r := range stored {
		if r.ResourceType != model.ResourceEdge || r.ResourceID != "42" {
			t.Errorf("row is filed under %s/%s, want edge/42", r.ResourceType, r.ResourceID)
		}
		if r.Role != "edge" {
			t.Errorf("role = %q, want edge: nobody at the console did this", r.Role)
		}
		if r.ResourceName != "host_bash" {
			t.Errorf("resource_name = %q, want the tool", r.ResourceName)
		}
	}
	// The two verdicts have to be two rows an operator can ask for
	// separately; folding them into a status would make "what was blocked
	// on this host" a payload scan, which is what the mcp_tool_* entries
	// already refused to do.
	if stored[0].Action != model.ActionNodeToolCall || stored[0].Status != model.StatusSuccess {
		t.Errorf("an allowed call landed as %s/%s", stored[0].Action, stored[0].Status)
	}
	if stored[1].Action != model.ActionNodeToolBlocked || stored[1].Status != model.StatusDenied {
		t.Errorf("a blocked call landed as %s/%s", stored[1].Action, stored[1].Status)
	}
	if got := countChained(t, db); got != 2 {
		t.Fatalf("%d rows carry a chain seal, want 2", got)
	}
}

// The origin is the field that stops a reader hunting for the operator who
// approved a tool call on a host they were not sitting at.
func TestRecordNodeEntries_SaysWhereTheRowCameFrom(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	row := nodeRow(ports.ActionToolCall)
	row.Detail = json.RawMessage(`{"reason":"the disk was full"}`)
	if _, err := uc.RecordNodeEntries(context.Background(), 7, []NodeLedgerRow{row}); err != nil {
		t.Fatalf("RecordNodeEntries: %v", err)
	}
	stored := allRows(t, db)
	if len(stored) != 1 {
		t.Fatalf("stored %d rows, want 1", len(stored))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stored[0].PayloadJSON), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["origin"] != "node_audit" {
		t.Errorf("origin = %v, want node_audit", payload["origin"])
	}
	if payload["action"] != string(ports.ActionToolCall) {
		t.Errorf("the node's own action is %v, want it kept verbatim", payload["action"])
	}
	if payload["actor"] != "operator@example.invalid" {
		t.Errorf("actor = %v, want the node's claim carried", payload["actor"])
	}
	if _, ok := payload["detail"]; !ok {
		t.Error("the node's detail was dropped on the way in")
	}
}

// An empty actor or an empty target is carried as what it is. A rule the
// node cannot satisfy is a rule that refuses a legitimate batch, and the
// node counts the refusal and moves on — silently.
func TestRecordNodeEntries_KeepsARowWithNoActorOrTarget(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	row := nodeRow(ports.ActionPluginInstall)
	row.Actor, row.Target, row.Outcome, row.Class = "", "", "", ""
	if _, err := uc.RecordNodeEntries(context.Background(), 1, []NodeLedgerRow{row}); err != nil {
		t.Fatalf("RecordNodeEntries: %v", err)
	}
	stored := allRows(t, db)
	if len(stored) != 1 {
		t.Fatalf("stored %d rows, want 1", len(stored))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stored[0].PayloadJSON), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for _, k := range []string{"actor", "outcome", "class"} {
		if _, ok := payload[k]; ok {
			t.Errorf("payload carries %s: %v; an absent field should stay absent", k, payload[k])
		}
	}
}

// The whole-batch rule, for the same reason the autonomy path has it: the
// node's pump is all-or-nothing, so a partial write here is a duplicated
// prefix on the next attempt.
func TestRecordNodeEntries_RefusesTheWholeBatchForShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []NodeLedgerRow
	}{
		{"an action this build cannot name", []NodeLedgerRow{
			nodeRow(ports.ActionToolCall),
			{At: time.Now(), Action: "did_a_thing", Target: "host_bash"},
		}},
		{"a row with no time", []NodeLedgerRow{
			nodeRow(ports.ActionToolCall),
			{Action: ports.ActionToolCall, Target: "host_bash"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uc, db := newChainedUC(t, testKey)
			res, err := uc.RecordNodeEntries(context.Background(), 42, tc.rows)
			if err != nil {
				t.Fatalf("a shape refusal became an error: %v", err)
			}
			if res.Accepted != 0 || res.Rejected != 2 {
				t.Fatalf("accepted %d rejected %d, want 0/2", res.Accepted, res.Rejected)
			}
			if n := len(allRows(t, db)); n != 0 {
				t.Fatalf("%d rows were written by a refused batch", n)
			}
		})
	}
}

// The map is total on purpose: an action nobody has implemented yet must be
// refused as a string this build cannot interpret, never filed under a
// neighbour that would make the ledger say something else happened.
func TestEveryActionTheNodeCanWriteIsMapped(t *testing.T) {
	for _, action := range []ports.AuditAction{
		ports.ActionToolCall, ports.ActionToolBlocked, ports.ActionToolFailed,
		ports.ActionApprovalRequest, ports.ActionApprovalGrant, ports.ActionApprovalDeny,
		ports.ActionAgentTurn, ports.ActionModelCall,
		ports.ActionPluginInstall, ports.ActionPluginRemove, ports.ActionPluginLoad,
		ports.ActionProposalCreate, ports.ActionRecoveryApply,
	} {
		if _, ok := nodeActionMap[action]; !ok {
			t.Errorf("%q has no manager action; a node writing it would have its batch refused", action)
		}
	}
	// And the table has no entry the port does not declare, so a stale
	// row in the map cannot outlive the vocabulary it mirrors.
	if len(nodeActionMap) != 13 {
		t.Errorf("the map holds %d entries; core/ports declares 13 node-side actions "+
			"(the twelve tool/approval/agent/plugin/proposal/recovery ones plus plugin_removed)", len(nodeActionMap))
	}
}

func TestRecordNodeEntries_RecordsWithoutAChain(t *testing.T) {
	db := newChainDB(t)
	uc := New(store.New(db), nil)
	res, err := uc.RecordNodeEntries(context.Background(), 9, []NodeLedgerRow{nodeRow(ports.ActionToolCall)})
	if err != nil {
		t.Fatalf("RecordNodeEntries: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("accepted %d, want 1", res.Accepted)
	}
	if got := len(allRows(t, db)); got != 1 {
		t.Fatalf("stored %d rows, want 1: a node's history must not depend on a key", got)
	}
}

func TestRecordNodeEntries_AnEmptyBatchIsANoOp(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	res, err := uc.RecordNodeEntries(context.Background(), 3, nil)
	if err != nil {
		t.Fatalf("RecordNodeEntries: %v", err)
	}
	if res.Accepted != 0 || res.Rejected != 0 {
		t.Fatalf("accepted %d rejected %d on an empty batch", res.Accepted, res.Rejected)
	}
	if got := len(allRows(t, db)); got != 0 {
		t.Fatalf("an empty batch wrote %d rows", got)
	}
}

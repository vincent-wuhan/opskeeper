package audit

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	store "github.com/vincent-wuhan/opskeeper/core/domains/data/audit/store"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
)

// allRows reads every audit row, chained or not, oldest first.
func allRows(t *testing.T, db *gorm.DB) []model.Log {
	t.Helper()
	var rows []model.Log
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read rows: %v", err)
	}
	return rows
}

func replayRow(action, phase, verdict string) AutonomyReplayRow {
	return AutonomyReplayRow{
		At:      time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Action:  action,
		Package: "opskeeper-sre-autonomy",
		Tool:    "host_restart_service",
		Target:  "orders-api",
		Argv:    []string{"systemctl", "restart", "orders-api"},
		Kind:    "metric_above",
		Metric:  "node_disk_used_ratio",
		// Threshold and the rest are only carried into the payload, so a
		// zero here is a value rather than a hole.
		Threshold: 0.92,
		Key:       "restart-orders:orders-api:win-1",
		Verdict:   verdict,
		Reason:    "the disk was full and nobody was answering",
		Phase:     phase,
	}
}

// TestRecordAutonomyReplay_WritesEveryRowIntoTheChain is the floor: a
// replay that lands is a replay whose rows are in the same ledger as the
// rows the console writes.
func TestRecordAutonomyReplay_WritesEveryRowIntoTheChain(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	rows := []AutonomyReplayRow{
		replayRow("restart-orders-on-disk-full", "decided", "run"),
		replayRow("restart-orders-on-disk-full", "completed", "run"),
		replayRow("restart-orders-on-disk-full", "decided", "defer"),
	}
	res, err := uc.RecordAutonomyReplay(context.Background(), 42, rows)
	if err != nil {
		t.Fatalf("RecordAutonomyReplay: %v", err)
	}
	if res.Accepted != 3 || res.Rejected != 0 {
		t.Fatalf("accepted %d rejected %d, want 3/0", res.Accepted, res.Rejected)
	}
	stored := allRows(t, db)
	if len(stored) != 3 {
		t.Fatalf("stored %d rows, want 3", len(stored))
	}
	for _, r := range stored {
		if r.Action != model.ActionAutonomyExecute {
			t.Errorf("action = %q, want %q", r.Action, model.ActionAutonomyExecute)
		}
		if r.ResourceType != model.ResourceEdge {
			t.Errorf("resource_type = %q, want %q", r.ResourceType, model.ResourceEdge)
		}
		if r.ResourceID != "42" {
			t.Errorf("resource_id = %q, want the edge id", r.ResourceID)
		}
		if r.Role != "edge" {
			t.Errorf("role = %q, want edge", r.Role)
		}
		if r.RequestID != "restart-orders:orders-api:win-1" {
			t.Errorf("request_id = %q, want the node's idempotency key", r.RequestID)
		}
	}
	// The chain is the point of the whole route: unchained rows would mean
	// the replay took a shortcut the console's own writes do not.
	if got := countChained(t, db); got != 3 {
		t.Fatalf("%d rows carry a chain seal, want 3", got)
	}
}

func countChained(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.Log{}).Where("seq > 0").Count(&n).Error; err != nil {
		t.Fatalf("count chained: %v", err)
	}
	return n
}

// TestRecordAutonomyReplay_RefusesTheWholeBatchForShape is the property the
// node depends on: a batch the chain cannot store whole leaves the chain
// exactly as it was, so the retry the node makes is not a duplicate write.
func TestRecordAutonomyReplay_RefusesTheWholeBatchForShape(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	rows := []AutonomyReplayRow{
		replayRow("restart-orders-on-disk-full", "decided", "run"), // good
		replayRow("restart-orders-on-disk-full", "", "run"),        // no phase
		replayRow("", "completed", "run"),                          // no action
	}
	res, err := uc.RecordAutonomyReplay(context.Background(), 42, rows)
	if err != nil {
		t.Fatalf("a shape refusal became an error: %v", err)
	}
	if res.Accepted != 0 || res.Rejected != 3 {
		t.Fatalf("accepted %d rejected %d, want 0/3 — a batch is taken whole or refused whole", res.Accepted, res.Rejected)
	}
	if n := len(allRows(t, db)); n != 0 {
		t.Fatalf("%d rows were written by a batch that was refused; a partial write is what makes a retry duplicate the prefix", n)
	}
}

// TestRecordAutonomyReplay_RecordsARefusalAsDenied: "the node considered
// self-healing and decided not to" is the row an investigator reads after
// an outage that was not mitigated.
func TestRecordAutonomyReplay_RecordsARefusalAsDenied(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	rows := []AutonomyReplayRow{
		replayRow("restart-orders-on-disk-full", "decided", "refuse"),
		replayRow("restart-orders-on-disk-full", "decided", "run"),
	}
	if _, err := uc.RecordAutonomyReplay(context.Background(), 7, rows); err != nil {
		t.Fatalf("RecordAutonomyReplay: %v", err)
	}
	stored := allRows(t, db)
	if len(stored) != 2 {
		t.Fatalf("stored %d rows, want 2", len(stored))
	}
	if stored[0].Status != model.StatusDenied {
		t.Errorf("refusal status = %q, want %q", stored[0].Status, model.StatusDenied)
	}
	if stored[1].Status != model.StatusSuccess {
		t.Errorf("run status = %q, want %q", stored[1].Status, model.StatusSuccess)
	}
}

// TestRecordAutonomyReplay_RecordsWithoutAChain: a deployment with no HMAC
// key still gets the node's history. Losing self-heal evidence because
// nobody configured a key is the worse failure.
func TestRecordAutonomyReplay_RecordsWithoutAChain(t *testing.T) {
	db := newChainDB(t)
	uc := New(store.New(db), nil)
	res, err := uc.RecordAutonomyReplay(context.Background(), 9,
		[]AutonomyReplayRow{replayRow("restart-orders-on-disk-full", "decided", "run")})
	if err != nil {
		t.Fatalf("RecordAutonomyReplay: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("accepted %d, want 1", res.Accepted)
	}
	if got := len(allRows(t, db)); got != 1 {
		t.Fatalf("stored %d rows, want 1", got)
	}
}

// TestRecordAutonomyReplay_AnEmptyBatchIsANoOp: a node with nothing to
// replay must not produce an audit row saying it replayed nothing.
func TestRecordAutonomyReplay_AnEmptyBatchIsANoOp(t *testing.T) {
	uc, db := newChainedUC(t, testKey)
	res, err := uc.RecordAutonomyReplay(context.Background(), 3, nil)
	if err != nil {
		t.Fatalf("RecordAutonomyReplay: %v", err)
	}
	if res.Accepted != 0 || res.Rejected != 0 {
		t.Fatalf("accepted %d rejected %d on an empty batch", res.Accepted, res.Rejected)
	}
	if got := len(allRows(t, db)); got != 0 {
		t.Fatalf("an empty batch wrote %d rows", got)
	}
}

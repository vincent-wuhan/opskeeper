package frontierbound

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/singchia/geminio"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	changeeventbiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge/changeevent"
	edgestore "github.com/vincent-wuhan/opskeeper/core/manager/data/edge/store"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
)

// installChangeEvents wires push_change_events to a real usecase over a real
// SQLite schema, and returns the handler plus a way to read the table.
//
// The wiring is real on purpose. The dedup contract has a step that only
// exists in this file — turning the wire's seq into the row's — and a test
// that stops at the usecase cannot see it. That is not hypothetical: with
// this file absent, deleting the seq handoff left every other test green
// while the feature was dead in production.
func installChangeEvents(t *testing.T) (geminio.RPC, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := edgestore.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	uc := changeeventbiz.New(edgestore.NewChangeEventRepo(db), slog.Default())
	fs, _ := installAndDispatch(t, Wiring{ChangeEventUC: uc, Log: slog.Default()})
	return rpcFor(t, fs, tunnel.MethodPushChangeEvents), db
}

func changeEventBody(t *testing.T, edgeID uint64, events []tunnel.ChangeEventWire) []byte {
	t.Helper()
	b, err := json.Marshal(tunnel.PushChangeEventsRequest{EdgeID: edgeID, Events: events})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func pushChangeEvents(t *testing.T, rpc geminio.RPC, edgeID uint64, events []tunnel.ChangeEventWire) tunnel.PushChangeEventsResponse {
	t.Helper()
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: changeEventBody(t, edgeID, events), clientID: edgeID}, rsp)
	if rsp.err != nil {
		t.Fatalf("push_change_events: %v", rsp.err)
	}
	var out tunnel.PushChangeEventsResponse
	if err := json.Unmarshal(rsp.data, &out); err != nil {
		t.Fatalf("decode response %q: %v", rsp.data, err)
	}
	return out
}

func wireEvent(seq uint64) tunnel.ChangeEventWire {
	return tunnel.ChangeEventWire{
		Source:    "journald",
		Kind:      "service_restart",
		Subject:   "nginx.service",
		Action:    "restart",
		Timestamp: time.Date(2026, 4, 23, 3, 12, 0, 0, time.UTC),
		Severity:  "notice",
		Seq:       seq,
	}
}

func storedRows(t *testing.T, db *gorm.DB) []edgemodel.ChangeEventRow {
	t.Helper()
	var rows []edgemodel.ChangeEventRow
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("select: %v", err)
	}
	return rows
}

// TestInstall_PushChangeEvents_StoresTheSequenceOnTheRow is the handoff the
// whole replay contract rests on: the node's log number has to survive JSON
// decoding and land on the row, or the center has nothing to match a
// replayed event against.
func TestInstall_PushChangeEvents_StoresTheSequenceOnTheRow(t *testing.T) {
	rpc, db := installChangeEvents(t)

	if out := pushChangeEvents(t, rpc, 42, []tunnel.ChangeEventWire{wireEvent(9)}); out.Accepted != 1 {
		t.Fatalf("Accepted = %d, want 1", out.Accepted)
	}
	rows := storedRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("stored %d rows, want 1", len(rows))
	}
	if rows[0].Seq == nil {
		t.Fatal("the row has no sequence: a replay of it can never be recognised")
	}
	if *rows[0].Seq != 9 {
		t.Errorf("row seq = %d, want 9", *rows[0].Seq)
	}
	if rows[0].EdgeID != 42 {
		t.Errorf("row edge_id = %d, want 42", rows[0].EdgeID)
	}
}

// TestInstall_PushChangeEvents_ALostAckDoesNotDuplicateHistory: the node
// never heard the answer, so it sends the identical batch again. The
// operator's timeline must not show the same restart twice, and the node
// must be told the truth about how much is new.
func TestInstall_PushChangeEvents_ALostAckDoesNotDuplicateHistory(t *testing.T) {
	rpc, db := installChangeEvents(t)
	batch := []tunnel.ChangeEventWire{wireEvent(1), wireEvent(2), wireEvent(3)}

	if out := pushChangeEvents(t, rpc, 42, batch); out.Accepted != 3 {
		t.Fatalf("first push Accepted = %d, want 3", out.Accepted)
	}
	out := pushChangeEvents(t, rpc, 42, batch)
	if out.Accepted != 0 {
		t.Errorf("replay Accepted = %d, want 0: the node would think it had three fresh events", out.Accepted)
	}
	if rows := storedRows(t, db); len(rows) != 3 {
		t.Fatalf("stored %d rows after a replay, want 3", len(rows))
	}
}

// TestInstall_PushChangeEvents_SeqZeroIsStoredAsNoSequence guards the
// sentinel. 0 means "this node never logged it", and the row must say so
// with NULL rather than 0 — a unique index over (edge_id, 0) would make
// every ordinary event on a node collide with every other one.
func TestInstall_PushChangeEvents_SeqZeroIsStoredAsNoSequence(t *testing.T) {
	rpc, db := installChangeEvents(t)
	plain := wireEvent(0)

	for i := 0; i < 2; i++ {
		if out := pushChangeEvents(t, rpc, 42, []tunnel.ChangeEventWire{plain}); out.Accepted != 1 {
			t.Fatalf("push %d Accepted = %d, want 1: a seq-less event must never be dropped", i+1, out.Accepted)
		}
	}
	rows := storedRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("stored %d rows, want 2", len(rows))
	}
	for i, r := range rows {
		if r.Seq != nil {
			t.Errorf("row %d has seq %d, want NULL", i, *r.Seq)
		}
	}
}

// TestInstall_PushChangeEvents_MixedBatchKeepsTheNewOnes is the shape a
// reconnect produces: the log drains oldest-first, so a batch is some rows
// the center has and some it has never seen.
func TestInstall_PushChangeEvents_MixedBatchKeepsTheNewOnes(t *testing.T) {
	rpc, db := installChangeEvents(t)
	if out := pushChangeEvents(t, rpc, 42, []tunnel.ChangeEventWire{wireEvent(1), wireEvent(2)}); out.Accepted != 2 {
		t.Fatalf("first push Accepted = %d, want 2", out.Accepted)
	}
	out := pushChangeEvents(t, rpc, 42, []tunnel.ChangeEventWire{wireEvent(1), wireEvent(2), wireEvent(3)})
	if out.Accepted != 1 {
		t.Errorf("mixed push Accepted = %d, want 1", out.Accepted)
	}
	if rows := storedRows(t, db); len(rows) != 3 {
		t.Fatalf("stored %d rows, want 3", len(rows))
	}
}

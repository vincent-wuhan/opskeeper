package changewatcher

import (
	"context"
	"testing"
	"time"

	"encoding/json"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The center can only recognise a replayed change event if the node tells it
// which row of the log it is re-sending (decision 122). These tests pin the
// node half of that handoff; the center half lives in
// core/manager/service/frontierbound/changeevent_replay_test.go.
//
// Both halves have to exist. The node half alone is a field nobody reads,
// and the center half alone is a dedup key that never arrives — which is
// exactly the state a test that only covers one side leaves behind.

// wireSeqs reads the sequence numbers of every event that went out, in
// order, across all recorded calls.
func wireSeqs(fc *fakeClient) []uint64 {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var out []uint64
	for _, c := range fc.calls {
		for _, e := range c.Events {
			out = append(out, e.Seq)
		}
	}
	return out
}

// TestALostAckReplaysTheSameSequence is the scenario the whole mechanism
// exists for: the center stored the batch, the answer did not come back, and
// the node sends it again. The second send has to carry the number the first
// one did, or the center is looking at two different events.
func TestALostAckReplaysTheSameSequence(t *testing.T) {
	fc := newFakeClient()
	sink := newWALSink(t, fc, TunnelSinkSinkConfig{})
	ctx := context.Background()

	if err := sink.Push(ctx, changeEvent("orders-api")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// The batch reaches the center and the answer is lost on the way back.
	fc.lostAck.Store(true)
	sink.flushBatch(ctx)

	if n, err := sink.Pending(); err != nil || n != 1 {
		t.Fatalf("pending = %d, %v; want the event still on disk after a lost ack", n, err)
	}
	// The next tick replays it. The center will now hold this row twice
	// unless the sequence number travels with it.
	sink.flushBatch(ctx)

	seqs := wireSeqs(fc)
	if len(seqs) != 2 {
		t.Fatalf("the event went out %d times, want 2: the replay never happened", len(seqs))
	}
	if seqs[0] == 0 {
		t.Fatal("the first send carried no sequence: the center has nothing to match on")
	}
	if seqs[0] != seqs[1] {
		t.Errorf("the replay carried seq %d, the original %d: the center will store it twice", seqs[1], seqs[0])
	}
	if n, err := sink.Pending(); err != nil || n != 0 {
		t.Fatalf("pending = %d, %v after a successful replay; want the log drained", n, err)
	}
}

// TestEveryEventInABatchGetsItsOwnSequence: the log numbers rows, not
// batches. Two events sharing a number would make the center store one and
// drop the other as a duplicate.
func TestEveryEventInABatchGetsItsOwnSequence(t *testing.T) {
	fc := newFakeClient()
	sink := newWALSink(t, fc, TunnelSinkSinkConfig{BatchSize: 10})
	ctx := context.Background()

	for _, subject := range []string{"nginx", "redis", "postgres"} {
		if err := sink.Push(ctx, changeEvent(subject)); err != nil {
			t.Fatalf("Push %s: %v", subject, err)
		}
	}
	sink.flushBatch(ctx)

	seqs := wireSeqs(fc)
	if len(seqs) != 3 {
		t.Fatalf("sent %d events, want 3", len(seqs))
	}
	seen := map[uint64]bool{}
	for i, s := range seqs {
		if s == 0 {
			t.Fatalf("event %d went out with no sequence", i)
		}
		if seen[s] {
			t.Fatalf("events %d and %d share sequence %d", i, s, s)
		}
		seen[s] = true
	}
}

// TestANodeWithoutALogSendsNoSequence covers the sentinel. A node with no
// write-ahead log has nothing to replay, so there is no number to report —
// and reporting zero is correct, because zero is what the center stores as
// "no key" rather than as a real sequence.
func TestANodeWithoutALogSendsNoSequence(t *testing.T) {
	fc := newFakeClient()
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{
		BatchSize: 10, FlushInterval: time.Hour,
	})
	ctx := context.Background()

	if err := sink.Push(ctx, changeEvent("orders-api")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	sink.flushBatch(ctx)

	seqs := wireSeqs(fc)
	if len(seqs) != 1 {
		t.Fatalf("sent %d events, want 1", len(seqs))
	}
	if seqs[0] != 0 {
		t.Errorf("a node with no log sent seq %d; 0 is the documented 'no key' value", seqs[0])
	}
}

// TestTheSequenceSurvivesTheJSONRoundTrip: the number goes over the wire as
// JSON, and omitempty must not swallow it. A sequence of 0 never legitimately
// occurs, so the field cannot be quietly absent for a real event.
func TestTheSequenceSurvivesTheJSONRoundTrip(t *testing.T) {
	ev := tunnel.ChangeEventWire{Source: "journald", Kind: "service_restart", Seq: 7}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"seq":7`) {
		t.Errorf("wire form %s lost the sequence", b)
	}
}

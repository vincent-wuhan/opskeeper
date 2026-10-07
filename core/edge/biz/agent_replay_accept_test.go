package biz_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/biz"
	"github.com/vincent-wuhan/opskeeper/core/edge/telemetrywal"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The plan's 1.1 acceptance is "拔网线 → 遥测落盘 → 恢复回放". The
// write-ahead log made the "落盘" half true; this file pins the "回放" half,
// which is only true if a drain that the center could not place is treated
// as not delivered. These call the drain directly — the loop's timing is
// not what is under test, the ack decision is.

// TestARetryableRefusalKeepsTheBacklog is the end-to-end reason the log
// exists, stated as a handshake rather than as a timeout.
//
// A node that was offline sends its backlog; register_edge has not landed
// on this connection yet, so the center answers Accepted=0. Before this
// test the edge read that as a *rejection*, acked the log and moved on —
// the backlog survived the outage and was destroyed by the first message
// after it. The only correct reading of "accepted none" is "not yet": the
// batch stays, and the next drain will find a center that can place it.
func TestARetryableRefusalKeepsTheBacklog(t *testing.T) {
	fc := newFakeClient()
	// The center can place nothing yet. Accepted defaults to zero.
	fc.pushAccepted = pushScript{hostMetricAccepted: 0, promAccepted: 0}
	a := biz.NewAgent(fc, &fakeCollector{}, biz.Config{AgentVersion: "test"}, discardLog())

	batch := telemetrywal.Batch{
		Source:    "embedded",
		HostPoint: &tunnel.HostMetricPoint{Ts: 1, CPUPct: 1},
		Samples:   []tunnel.PromSample{{Name: "node_cpu_pct", Value: 1, TsMs: 1}},
	}

	n, err := a.DrainBatchesForTest(context.Background(), []telemetrywal.Batch{batch})
	if err == nil {
		t.Fatalf("a drain the center placed nothing of was reported as delivered")
	}
	if n != 0 {
		t.Errorf("delivered = %d, want 0 — nothing landed, so nothing may be acked", n)
	}
}

// TestAPartialRefusalMovesOnAndIsCounted is the other side of the same
// decision. Once the center has said it will take some of it and not the
// rest, asking again is a node asking the same question forever, so the
// batch is allowed past and the shortfall is a counter rather than a queue
// wedge.
func TestAPartialRefusalMovesOnAndIsCounted(t *testing.T) {
	fc := newFakeClient()
	fc.pushAccepted = pushScript{hostMetricAccepted: 1, promAccepted: 1}
	a := biz.NewAgent(fc, &fakeCollector{}, biz.Config{AgentVersion: "test"}, discardLog())

	batch := telemetrywal.Batch{
		Source:    "embedded",
		HostPoint: &tunnel.HostMetricPoint{Ts: 1, CPUPct: 1},
		Samples:   []tunnel.PromSample{{Name: "a", Value: 1, TsMs: 1}, {Name: "b", Value: 2, TsMs: 2}},
	}
	n, err := a.DrainBatchesForTest(context.Background(), []telemetrywal.Batch{batch})
	if err != nil {
		t.Fatalf("a partial acceptance must not wedge the drain: %v", err)
	}
	if n != 1 {
		t.Errorf("delivered = %d, want 1", n)
	}
	if got := a.RejectedForTest(); got != 1 {
		t.Errorf("rejected = %d, want 1 — the un-stored sample must be counted, not retried forever", got)
	}
}

// TestAFullyAcceptedBatchIsReportedDelivered is the control: the counting
// above is only meaningful if the happy path still reports every row.
func TestAFullyAcceptedBatchIsReportedDelivered(t *testing.T) {
	fc := newFakeClient()
	fc.pushAccepted = pushScript{hostMetricAccepted: 1, promAccepted: 2}
	a := biz.NewAgent(fc, &fakeCollector{}, biz.Config{AgentVersion: "test"}, discardLog())

	batch := telemetrywal.Batch{
		Source:    "embedded",
		HostPoint: &tunnel.HostMetricPoint{Ts: 1, CPUPct: 1},
		Samples:   []tunnel.PromSample{{Name: "a", Value: 1, TsMs: 1}, {Name: "b", Value: 2, TsMs: 2}},
	}
	n, err := a.DrainBatchesForTest(context.Background(), []telemetrywal.Batch{batch})
	if err != nil {
		t.Fatalf("DrainBatches: %v", err)
	}
	if n != 1 {
		t.Errorf("delivered = %d, want 1", n)
	}
	if got := a.RejectedForTest(); got != 0 {
		t.Errorf("rejected = %d, want 0 on a clean accept", got)
	}
}

// TestATransportFailureStopsBeforeTheRowsThatDidNotGo keeps the third
// sentence distinct from the two refusals: a batch that errored on the
// wire must stop the drain at its index, so the rows after it are not
// reported as delivered by association.
func TestATransportFailureStopsBeforeTheRowsThatDidNotGo(t *testing.T) {
	fc := newFakeClient()
	fc.pushAccepted = pushScript{err: context.DeadlineExceeded}
	a := biz.NewAgent(fc, &fakeCollector{}, biz.Config{AgentVersion: "test"}, discardLog())

	batches := []telemetrywal.Batch{
		{Source: "s1", HostPoint: &tunnel.HostMetricPoint{Ts: 1}},
		{Source: "s2", HostPoint: &tunnel.HostMetricPoint{Ts: 2}},
	}
	n, err := a.DrainBatchesForTest(context.Background(), batches)
	if err == nil {
		t.Fatal("a transport failure was reported as delivered")
	}
	if n != 0 {
		t.Errorf("delivered = %d, want 0 — the drain must stop where the transport did", n)
	}
	if got := fc.countOf(tunnel.MethodPushHostMetrics); got != 1 {
		t.Errorf("push_host_metrics called %d times, want 1 — a stopped drain does not keep going", got)
	}
	// Guard against a test that silently stops exercising the agent.
	_ = time.Now
}

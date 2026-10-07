package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/autonomy"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// replayTunnel is the control plane as far as the replay sender is
// concerned: it records what was pushed and answers with a scripted count.
//
// It exists instead of reusing fakeTunnel because that one models the
// tool-call reply shape, and the property under test here is precisely the
// three different answers the replay route can give. Collapsing them into
// one "reply" field is how the difference gets lost in the test as well as
// in the code.
type replayTunnel struct {
	mu     sync.Mutex
	method string
	req    tunnel.AutonomyAuditReplayRequest
	calls  int
	// resp is returned as-is; err wins over it.
	resp tunnel.AutonomyAuditReplayResponse
	err  error
	// hold blocks Call until closed, for the shutdown test.
	hold chan struct{}
}

func (t *replayTunnel) Dial(context.Context) error             { return nil }
func (t *replayTunnel) RegisterHandler(string, tunnel.Handler) {}
func (t *replayTunnel) OnReconnect(func())                     {}
func (t *replayTunnel) AcceptStream() (tunnel.StreamConn, error) {
	return nil, errors.New("not used")
}
func (t *replayTunnel) Close() error { return nil }

func (t *replayTunnel) Call(ctx context.Context, method string, req, resp any) error {
	if t.hold != nil {
		select {
		case <-t.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	t.method = method
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &t.req); err != nil {
		return err
	}
	if t.err != nil {
		return t.err
	}
	out, err := json.Marshal(t.resp)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, resp)
}

func (t *replayTunnel) snapshot() (string, tunnel.AutonomyAuditReplayRequest, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.method, t.req, t.calls
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func replayTestRow() autonomy.Row {
	return autonomy.Row{
		At:      time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Action:  "restart-orders-on-disk-full",
		Package: "opskeeper-sre-autonomy",
		Tool:    "host_restart_service",
		Target:  "orders-api",
		Argv:    []string{"systemctl", "restart", "orders-api"},
		Trigger: domain.AutonomyTrigger{
			Kind: domain.TriggerMetricAbove, Metric: "node_disk_used_ratio", Threshold: 0.92,
		},
		Key:     "restart-orders:orders-api:win-1",
		Verdict: "run",
		Phase:   autonomy.PhaseDecided,
	}
}

// TestAutonomyReplaySender_AllRowsTaken: the ordinary case. All of it
// landed, so the pump may forget the batch.
func TestAutonomyReplaySender_AllRowsTaken(t *testing.T) {
	tun := &replayTunnel{resp: tunnel.AutonomyAuditReplayResponse{Accepted: 2}}
	s := autonomyReplaySender{client: tun, edgeID: func() uint64 { return 42 }, log: discardLog(), refused: &atomic.Uint64{}}
	if err := s.Send(context.Background(), []autonomy.Row{replayTestRow(), replayTestRow()}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	method, req, _ := tun.snapshot()
	if method != tunnel.MethodAgentAuditReplay {
		t.Errorf("method = %q, want %q", method, tunnel.MethodAgentAuditReplay)
	}
	if req.EdgeID != 42 {
		t.Errorf("edge_id = %d, want 42", req.EdgeID)
	}
	if len(req.Rows) != 2 {
		t.Fatalf("sent %d rows, want 2", len(req.Rows))
	}
	// The trigger is flattened on the wire, and the fields have to arrive
	// under the names the center reads.
	got := req.Rows[0]
	if got.Kind != "metric_above" || got.Metric != "node_disk_used_ratio" || got.Threshold != 0.92 {
		t.Errorf("trigger = %q/%q/%v, want it flattened onto the row", got.Kind, got.Metric, got.Threshold)
	}
	if got.Key != "restart-orders:orders-api:win-1" || got.Phase != autonomy.PhaseDecided {
		t.Errorf("row = %+v, want the arbiter's own fields", got)
	}
	if got.Verdict != "run" {
		t.Errorf("verdict = %q, want run", got.Verdict)
	}
}

// TestAutonomyReplaySender_TookNoneMeansKeepThem is decision 100 applied to
// the audit route: the center answering "none" without an error means it
// could not place the rows yet, and reading that as "refused" is how a
// backlog dies in the first message after a reconnect.
func TestAutonomyReplaySender_TookNoneMeansKeepThem(t *testing.T) {
	tun := &replayTunnel{resp: tunnel.AutonomyAuditReplayResponse{Accepted: 0, Rejected: 0}}
	s := autonomyReplaySender{client: tun, edgeID: func() uint64 { return 0 }, log: discardLog(), refused: &atomic.Uint64{}}
	if err := s.Send(context.Background(), []autonomy.Row{replayTestRow()}); err == nil {
		t.Fatal("the center taking none of the batch was reported as delivered")
	}
}

// TestAutonomyReplaySender_ShapeRefusalIsCountedNotRetried: rows the center
// will never take must not wedge a queue that is otherwise fine.
func TestAutonomyReplaySender_ShapeRefusalIsCountedNotRetried(t *testing.T) {
	tun := &replayTunnel{resp: tunnel.AutonomyAuditReplayResponse{Accepted: 0, Rejected: 3, Reason: "missing phase"}}
	refused := &atomic.Uint64{}
	s := autonomyReplaySender{client: tun, edgeID: func() uint64 { return 42 }, log: discardLog(), refused: refused}
	if err := s.Send(context.Background(), []autonomy.Row{replayTestRow(), replayTestRow(), replayTestRow()}); err != nil {
		t.Fatalf("a shape refusal was retried: %v", err)
	}
	if got := refused.Load(); got != 3 {
		t.Fatalf("refused = %d, want 3", got)
	}
}

// TestAutonomyReplaySender_ATransportFailureKeepsTheBatch: the tunnel being
// down again is not a decision about the rows.
func TestAutonomyReplaySender_ATransportFailureKeepsTheBatch(t *testing.T) {
	tun := &replayTunnel{err: errors.New("tunnel is closed")}
	s := autonomyReplaySender{client: tun, edgeID: func() uint64 { return 42 }, log: discardLog(), refused: &atomic.Uint64{}}
	if err := s.Send(context.Background(), []autonomy.Row{replayTestRow()}); err == nil {
		t.Fatal("a transport failure was reported as delivered")
	}
}

// TestAutonomyReplaySender_ACountItCannotExplainIsRetried: a center this
// build does not understand must not be guessed at — guessing which half it
// took is how rows are lost.
//
// The case under test is a count that does not add up to the batch. A
// (1, 1) answer to two rows is *not* that case: every row is accounted for,
// one written and one refused, and acking it is correct. The dangerous
// answer is a prefix — some rows acknowledged and no word on the rest.
func TestAutonomyReplaySender_ACountItCannotExplainIsRetried(t *testing.T) {
	tun := &replayTunnel{resp: tunnel.AutonomyAuditReplayResponse{Accepted: 1}}
	s := autonomyReplaySender{client: tun, edgeID: func() uint64 { return 42 }, log: discardLog(), refused: &atomic.Uint64{}}
	if err := s.Send(context.Background(), []autonomy.Row{replayTestRow(), replayTestRow()}); err == nil {
		t.Fatal("a partial count was accepted and the unaccounted rows discarded")
	}
}

// TestAutonomyReplaySender_ACountLargerThanTheBatchIsNotAClaimOnIt: a
// center that reports more rows than were sent is not describing this batch,
// and a guard written as ">= len(rows)" would read its answer as "all taken"
// and ack rows nobody acknowledged. The node's rule has to be "the center
// accounted for exactly this batch", not "its number was big enough".
func TestAutonomyReplaySender_ACountLargerThanTheBatchIsNotAClaimOnIt(t *testing.T) {
	tun := &replayTunnel{resp: tunnel.AutonomyAuditReplayResponse{Accepted: 3}}
	s := autonomyReplaySender{client: tun, edgeID: func() uint64 { return 42 }, log: discardLog(), refused: &atomic.Uint64{}}
	if err := s.Send(context.Background(), []autonomy.Row{replayTestRow(), replayTestRow()}); err == nil {
		t.Fatal("a count larger than the batch was read as 'all taken'; the rows stay on disk")
	}
}

// TestAutonomyReplaySender_ACountThatAddsUpIsAckd: the same pair, adding to
// the batch size, is a complete answer — accepted rows written and refused
// rows permanently so.
func TestAutonomyReplaySender_ACountThatAddsUpIsAckd(t *testing.T) {
	tun := &replayTunnel{resp: tunnel.AutonomyAuditReplayResponse{Accepted: 1, Rejected: 1}}
	refused := &atomic.Uint64{}
	s := autonomyReplaySender{client: tun, edgeID: func() uint64 { return 42 }, log: discardLog(), refused: refused}
	if err := s.Send(context.Background(), []autonomy.Row{replayTestRow(), replayTestRow()}); err != nil {
		t.Fatalf("a fully-accounted batch was retried: %v", err)
	}
	if got := refused.Load(); got != 1 {
		t.Fatalf("refused = %d, want 1", got)
	}
}

// TestAutonomyReplaySender_AnEmptyBatchIsFree: a drain of nothing must not
// be a tunnel call, or a healthy idle node would chatter every interval.
func TestAutonomyReplaySender_AnEmptyBatchIsFree(t *testing.T) {
	tun := &replayTunnel{}
	s := autonomyReplaySender{client: tun, edgeID: func() uint64 { return 42 }, log: discardLog(), refused: &atomic.Uint64{}}
	if err := s.Send(context.Background(), nil); err != nil {
		t.Fatalf("Send(nil): %v", err)
	}
	if _, _, calls := tun.snapshot(); calls != 0 {
		t.Fatalf("an empty batch made %d tunnel calls", calls)
	}
}

// TestBuildAutonomy_InstallsAStartedReplayLoop is the wiring the plan asked
// for and the previous decision deliberately deferred: the stack a declared
// package produces is no longer local-only.
func TestBuildAutonomy_InstallsAStartedReplayLoop(t *testing.T) {
	obs := &fakeObservations{
		online: false, since: time.Now().Add(-10 * time.Minute),
		values: map[string]float64{"node_disk_used_ratio": 0.99},
		edgeID: 42,
	}
	runner := &recordingRunner{}
	stack := newAutonomyStack(t, []pluginmanifest.Plugin{autonomyPlugin()}, obs, runner)
	if stack == nil {
		t.Fatal("a declared package produced no autonomy stack")
	}
	if stack.pump == nil {
		t.Fatal("the stack has no replay pump; the spool is local-only again")
	}

	// Run one action while the center is away, so the spool holds the two
	// phases the arbiter writes.
	if out := callAutonomyRun(t, "restart-orders-on-disk-full", "orders-api", "win-1"); out.Verdict != autonomy.Run.String() {
		t.Fatalf("verdict = %q, want %q", out.Verdict, autonomy.Run.String())
	}

	pending, err := stack.pump.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 2 {
		t.Fatalf("spooled %d rows, want the decided+completed pair", pending)
	}

	// The rows the arbiter actually wrote are what has to survive the hop.
	// Read them through the spool (the same read the pump makes) and push
	// them through the sender the assembly root built, so the conversion
	// is checked against real rows rather than a hand-written fixture.
	rows, err := stack.spool.Peek(2)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("peeked %d rows, want 2", len(rows))
	}
	tun := &replayTunnel{resp: tunnel.AutonomyAuditReplayResponse{Accepted: 2}}
	sender := autonomyReplaySender{
		client: tun, edgeID: obs.EdgeID, log: discardLog(), refused: stack.refused,
	}
	if err := sender.Send(context.Background(), rows); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, req, _ := tun.snapshot()
	if len(req.Rows) != 2 {
		t.Fatalf("sent %d rows, want 2", len(req.Rows))
	}
	// Peek is oldest-first and the arbiter writes decided before completed,
	// so the order on the wire is the order the decision was made in —
	// which is the whole reason the audit route is not allowed to reorder.
	if req.Rows[0].Phase != autonomy.PhaseDecided || req.Rows[1].Phase != autonomy.PhaseCompleted {
		t.Fatalf("phases on the wire = %q then %q, want decided then completed",
			req.Rows[0].Phase, req.Rows[1].Phase)
	}
	if req.Rows[0].Action != "restart-orders-on-disk-full" {
		t.Errorf("action = %q, want the declared one", req.Rows[0].Action)
	}
	if req.Rows[1].Result == "" {
		t.Errorf("the completion row lost its result on the way out: %+v", req.Rows[1])
	}
}

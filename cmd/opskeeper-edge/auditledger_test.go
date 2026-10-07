package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/auditlog"
	"github.com/vincent-wuhan/opskeeper/core/edge/auditwire"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// ledgerTunnel is the control plane as far as the ledger sender is
// concerned. It is a second fake rather than a reuse of replayTunnel
// because the property under test is the three different answers this
// route can give, and a fake shared with another route would be one more
// place for the difference to be lost.
type ledgerTunnel struct {
	mu     sync.Mutex
	method string
	req    tunnel.AuditEntriesRequest
	calls  int
	resp   tunnel.AuditEntriesResponse
	err    error
}

func (t *ledgerTunnel) Dial(context.Context) error             { return nil }
func (t *ledgerTunnel) RegisterHandler(string, tunnel.Handler) {}
func (t *ledgerTunnel) OnReconnect(func())                     {}
func (t *ledgerTunnel) AcceptStream() (tunnel.StreamConn, error) {
	return nil, errors.New("not used")
}
func (t *ledgerTunnel) Close() error { return nil }

func (t *ledgerTunnel) Call(_ context.Context, method string, req, resp any) error {
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

func (t *ledgerTunnel) snapshot() (string, tunnel.AuditEntriesRequest, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.method, t.req, t.calls
}

func ledgerEntry(target string) ports.AuditEntry {
	return ports.AuditEntry{
		At:      time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		Actor:   "operator@example.invalid",
		Action:  ports.ActionToolCall,
		Target:  target,
		Outcome: "allowed",
		Class:   "write",
	}
}

func newLedgerSender(t *ledgerTunnel, refused *atomic.Uint64) *auditwire.Sender {
	return auditwire.NewSender(t, func() uint64 { return 42 }, discardLog(), refused)
}

// The ordinary case: all of it landed, so the pump may forget the batch.
func TestAuditEntriesSender_AllRowsTaken(t *testing.T) {
	tun := &ledgerTunnel{resp: tunnel.AuditEntriesResponse{Accepted: 2}}
	if err := newLedgerSender(tun, &atomic.Uint64{}).Send(context.Background(),
		[]ports.AuditEntry{ledgerEntry("host_bash"), ledgerEntry("host_tail_file")}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	method, req, calls := tun.snapshot()
	if calls != 1 {
		t.Fatalf("called %d times, want 1", calls)
	}
	if method != tunnel.MethodAgentAuditEntries {
		t.Errorf("method = %q, want %q", method, tunnel.MethodAgentAuditEntries)
	}
	if req.EdgeID != 42 {
		t.Errorf("edge_id = %d, want the node's own id for the first-connect case", req.EdgeID)
	}
	if len(req.Entries) != 2 {
		t.Fatalf("sent %d entries, want 2", len(req.Entries))
	}
	// The wire type does not share the port type, so the conversion is
	// where a field could be dropped. The node's own clock is the fact the
	// row is about, so it is the field that has to survive.
	if !req.Entries[0].At.Equal(ledgerEntry("host_bash").At) {
		t.Errorf("the row's time came back as %v", req.Entries[0].At)
	}
	if req.Entries[0].Action != "tool_call" || req.Entries[1].Target != "host_tail_file" {
		t.Errorf("entries = %+v, want the ones that were sent in order", req.Entries)
	}
}

// An empty batch must not become a call. A node with nothing to say is not
// a node that should wake the center up to be told so.
func TestAuditEntriesSender_AnEmptyBatchIsNotACall(t *testing.T) {
	tun := &ledgerTunnel{}
	if err := newLedgerSender(tun, &atomic.Uint64{}).Send(context.Background(), nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, _, calls := tun.snapshot(); calls != 0 {
		t.Fatalf("an empty batch made %d calls", calls)
	}
}

// The three answers, and the reason they must stay three. "Took none of it"
// is not "refused all of it": the first means the node is not registered
// yet and must keep the rows, the second means the rows will never be
// accepted and retrying asks the same question forever.
func TestAuditEntriesSender_TellsRefusalFromNotYetReady(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resp       tunnel.AuditEntriesResponse
		wantErr    bool
		wantRefuse uint64
	}{
		{"all taken", tunnel.AuditEntriesResponse{Accepted: 2}, false, 0},
		{"all refused for shape", tunnel.AuditEntriesResponse{Rejected: 2, Reason: "unknown action"}, false, 2},
		{"every row accounted for, some refused", tunnel.AuditEntriesResponse{Accepted: 1, Rejected: 1}, false, 1},
		{"not ready yet", tunnel.AuditEntriesResponse{Accepted: 0, Rejected: 0}, true, 0},
		// One accepted of two and no refusal accounted for the rest: a
		// count this build cannot read. Guessing which half it took is how
		// rows are lost, so the whole batch stays.
		{"a count this build does not understand", tunnel.AuditEntriesResponse{Accepted: 1}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tun := &ledgerTunnel{resp: tc.resp}
			refused := &atomic.Uint64{}
			err := newLedgerSender(tun, refused).Send(context.Background(),
				[]ports.AuditEntry{ledgerEntry("a"), ledgerEntry("b")})
			if tc.wantErr && err == nil {
				t.Fatal("Send returned nil; the pump would forget rows the center has not taken")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Send: %v", err)
			}
			if got := refused.Load(); got != tc.wantRefuse {
				t.Errorf("refused = %d, want %d", got, tc.wantRefuse)
			}
		})
	}
}

// A transport failure is the other half of "keep the rows", and it must
// not collapse into the refusal branch: one is retried, the other is not.
func TestAuditEntriesSender_ATransportFailureKeepsTheBatch(t *testing.T) {
	boom := errors.New("the tunnel is down")
	tun := &ledgerTunnel{err: boom}
	refused := &atomic.Uint64{}
	if err := newLedgerSender(tun, refused).Send(context.Background(), []ports.AuditEntry{ledgerEntry("a")}); err == nil {
		t.Fatal("a transport failure came back as success")
	}
	if got := refused.Load(); got != 0 {
		t.Errorf("a transport failure counted %d refused rows; it is not a refusal", got)
	}
}

// The whole node path in one test: the gate writes through the port, the
// rows land on disk, the pump drains them when the link is up, and the
// batch is only forgotten after the center says it took all of it. This is
// the route that did not exist, and the assertion that matters is the last
// one — the rows are gone from the node only when they are in the chain.
func TestTheNodeLedgerReachesTheCenterAndOnlyThenIsForgotten(t *testing.T) {
	sink, err := auditlog.Open(t.TempDir()+"/audit-ledger.jsonl", 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sink.Close()
	ctx := context.Background()
	for _, target := range []string{"host_bash", "host_tail_file", "host_lsof"} {
		if err := sink.Record(ctx, ledgerEntry(target)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// The gate blocks, so nothing is sent and nothing is lost.
	down := &ledgerTunnel{resp: tunnel.AuditEntriesResponse{Accepted: 3}}
	refused := &atomic.Uint64{}
	pump, err := auditlog.NewPump(auditlog.PumpOptions{
		Sink:      sink,
		Sender:    newLedgerSender(down, refused),
		Reachable: func() bool { return false },
		Log:       discardLog(),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	if n, err := pump.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("a down link still drained: (%d, %v)", n, err)
	}
	if pending, _ := pump.Pending(); pending != 3 {
		t.Fatalf("%d rows pending while the link was down, want 3", pending)
	}

	// The link comes back and the center takes all of it.
	down.mu.Lock()
	down.resp = tunnel.AuditEntriesResponse{Accepted: 3}
	down.mu.Unlock()
	up, err := auditlog.NewPump(auditlog.PumpOptions{
		Sink:      sink,
		Sender:    newLedgerSender(down, refused),
		Reachable: func() bool { return true },
		Log:       discardLog(),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	n, err := up.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("drained %d rows, want 3", n)
	}
	if pending, _ := up.Pending(); pending != 0 {
		t.Errorf("%d rows still on the node after the center took them", pending)
	}
	_, req, calls := down.snapshot()
	if calls != 1 {
		t.Fatalf("the center was called %d times, want 1", calls)
	}
	want := []string{"host_bash", "host_tail_file", "host_lsof"}
	if len(req.Entries) != len(want) {
		t.Fatalf("sent %d entries, want %d", len(req.Entries), len(want))
	}
	for i, target := range want {
		if req.Entries[i].Target != target {
			t.Errorf("entry %d = %q, want %q: the order the gate wrote them in is the order the chain stores",
				i, req.Entries[i].Target, target)
		}
	}
}

// A node that has never registered must keep its rows, and the pump is what
// makes that true: "took none of it" is an error, so nothing is acked.
func TestANodeThatIsNotRegisteredYetKeepsEveryRow(t *testing.T) {
	sink, err := auditlog.Open(t.TempDir()+"/audit-ledger.jsonl", 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sink.Close()
	ctx := context.Background()
	if err := sink.Record(ctx, ledgerEntry("host_bash")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Zero edge id is what a node reports before register_edge lands, and
	// the center answers "took none" until it does.
	tun := &ledgerTunnel{resp: tunnel.AuditEntriesResponse{Accepted: 0}}
	pump, err := auditlog.NewPump(auditlog.PumpOptions{
		Sink:      sink,
		Sender:    auditwire.NewSender(tun, func() uint64 { return 0 }, discardLog(), &atomic.Uint64{}),
		Reachable: func() bool { return true },
		Log:       discardLog(),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	if n, err := pump.DrainOnce(ctx); err == nil || n != 0 {
		t.Fatalf("DrainOnce = (%d, %v), want (0, an error)", n, err)
	}
	if pending, _ := pump.Pending(); pending != 1 {
		t.Errorf("%d rows pending; an unregistered node must not lose its ledger", pending)
	}
	if _, req, _ := tun.snapshot(); req.EdgeID != 0 {
		t.Errorf("edge_id = %d, want the node's own zero rather than an invented one", req.EdgeID)
	}
}

package frontierbound

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
)

// fakeAutonomyReplay stands in for the audit chain. It records the batch it
// was handed and answers with whatever the test told it to.
type fakeAutonomyReplay struct {
	mu       sync.Mutex
	gotEdge  uint64
	gotRows  []tunnel.AutonomyAuditRow
	accepted int
	rejected int
	wantErr  error
	calls    int
}

func (f *fakeAutonomyReplay) RecordAutonomyReplay(_ context.Context, edgeID uint64, rows []tunnel.AutonomyAuditRow) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotEdge = edgeID
	f.gotRows = append([]tunnel.AutonomyAuditRow(nil), rows...)
	return f.accepted, f.rejected, f.wantErr
}

// snapshot reads the recorder under its lock, for the same reason every
// other fake here does: the handler may run on the geminio goroutine.
func (f *fakeAutonomyReplay) snapshot() (uint64, []tunnel.AutonomyAuditRow, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotEdge, append([]tunnel.AutonomyAuditRow(nil), f.gotRows...), f.calls
}

func installAutonomyReplay(t *testing.T, rec AutonomyReplayRecorder) *fakeService {
	t.Helper()
	fs := newFakeService()
	c := newWithService(fs, slog.Default())
	err := Install(context.Background(), c, Wiring{
		EdgeAuthn:      &edgebiz.AccessKeyAuthenticator{},
		EdgeUC:         &edgebiz.Usecase{},
		MetricIngester: &fakeMetricIngester{},
		AutonomyReplay: rec,
		Log:            slog.Default(),
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	return fs
}

func replayBody(t *testing.T, edgeID uint64, rows []tunnel.AutonomyAuditRow) []byte {
	t.Helper()
	b, err := json.Marshal(tunnel.AutonomyAuditReplayRequest{EdgeID: edgeID, Rows: rows})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func decodeReplay(t *testing.T, data []byte) tunnel.AutonomyAuditReplayResponse {
	t.Helper()
	var out tunnel.AutonomyAuditReplayResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode replay response %q: %v", data, err)
	}
	return out
}

func sampleRow() tunnel.AutonomyAuditRow {
	return tunnel.AutonomyAuditRow{
		Action: "restart-orders-on-disk-full",
		Phase:  "decided", Verdict: "run", Package: "opskeeper-sre-autonomy",
		Key: "restart-orders:orders-api:win-1",
	}
}

// TestInstall_AutonomyReplay_IsNotRegisteredWithoutARecorder: a manager
// with no audit chain must not answer "took none of it", which the node
// reads as "keep trying" and replays forever into nothing.
func TestInstall_AutonomyReplay_IsNotRegisteredWithoutARecorder(t *testing.T) {
	fs := installAutonomyReplay(t, nil)
	if _, ok := fs.rpcs[tunnel.MethodAgentAuditReplay]; ok {
		t.Error("agent.audit.replay was registered with no chain behind it")
	}
}

// TestInstall_AutonomyReplay_DeferredUntilTheEdgeIsBound: the node hands
// rows over before register_edge has landed on a fresh transport. The
// answer has to be "took none" — an error would be logged as a fault, and
// an accept would discard the rows.
func TestInstall_AutonomyReplay_DeferredUntilTheEdgeIsBound(t *testing.T) {
	rec := &fakeAutonomyReplay{accepted: 1}
	fs := installAutonomyReplay(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditReplay)

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: replayBody(t, 0, []tunnel.AutonomyAuditRow{sampleRow()}), clientID: 555}, rsp)
	if rsp.err != nil {
		t.Fatalf("a deferred replay became an RPC error: %v", rsp.err)
	}
	out := decodeReplay(t, rsp.data)
	if out.Accepted != 0 {
		t.Errorf("accepted = %d, want 0 so the node keeps the rows", out.Accepted)
	}
	if _, _, calls := rec.snapshot(); calls != 0 {
		t.Error("an unbound replay reached the chain")
	}
}

// TestInstall_AutonomyReplay_BodyEdgeIDBindsTheTransport: the node names
// its edge id, which is what register_edge would have told the manager; the
// handler accepts that as the binding, exactly as the metrics push does.
func TestInstall_AutonomyReplay_BodyEdgeIDBindsTheTransport(t *testing.T) {
	rec := &fakeAutonomyReplay{accepted: 1}
	fs := installAutonomyReplay(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditReplay)

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: replayBody(t, 42, []tunnel.AutonomyAuditRow{sampleRow()}), clientID: 900}, rsp)
	if rsp.err != nil {
		t.Fatalf("replay returned an error: %v", rsp.err)
	}
	edge, rows, calls := rec.snapshot()
	if calls != 1 {
		t.Fatalf("chain called %d times, want 1", calls)
	}
	if edge != 42 {
		t.Errorf("edge = %d, want the bound 42", edge)
	}
	if len(rows) != 1 || rows[0].Action != "restart-orders-on-disk-full" {
		t.Errorf("rows = %+v, want the one that was sent", rows)
	}
	out := decodeReplay(t, rsp.data)
	if out.Accepted != 1 {
		t.Errorf("accepted = %d, want the chain's count", out.Accepted)
	}
}

// TestInstall_AutonomyReplay_TrustsTheTransportEdgeID: once the transport
// is bound, a body naming another edge must not move the write onto that
// edge's ledger entry.
func TestInstall_AutonomyReplay_TrustsTheTransportEdgeID(t *testing.T) {
	rec := &fakeAutonomyReplay{accepted: 1}
	fs := installAutonomyReplay(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditReplay)

	// Bind transport 900 to edge 42 first, the way register_edge does.
	rpc(context.Background(), &fakeReq{data: replayBody(t, 42, nil), clientID: 900}, &fakeResp{})
	// Now a batch from the same transport claiming a different edge id.
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: replayBody(t, 7, []tunnel.AutonomyAuditRow{sampleRow()}), clientID: 900}, rsp)

	edge, _, _ := rec.snapshot()
	if edge != 42 {
		t.Errorf("the body's claimed edge id moved the write: edge = %d, want 42", edge)
	}
}

// TestInstall_AutonomyReplay_ACountPassesThroughUnchanged: the node acks
// exactly what the chain reports, so the numbers must survive the hop.
func TestInstall_AutonomyReplay_ACountPassesThroughUnchanged(t *testing.T) {
	rec := &fakeAutonomyReplay{accepted: 0, rejected: 3}
	fs := installAutonomyReplay(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditReplay)

	rows := []tunnel.AutonomyAuditRow{sampleRow(), sampleRow(), sampleRow()}
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: replayBody(t, 42, rows), clientID: 900}, rsp)
	out := decodeReplay(t, rsp.data)
	if out.Accepted != 0 || out.Rejected != 3 {
		t.Errorf("accepted/rejected = %d/%d, want 0/3", out.Accepted, out.Rejected)
	}
}

// TestInstall_AutonomyReplay_AChainFailureIsAnRPCError: the node has to be
// able to tell "the ledger is unreachable, keep everything and try again"
// from "the ledger took none of it".
func TestInstall_AutonomyReplay_AChainFailureIsAnRPCError(t *testing.T) {
	rec := &fakeAutonomyReplay{wantErr: errors.New("database is down")}
	fs := installAutonomyReplay(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditReplay)

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: replayBody(t, 42, []tunnel.AutonomyAuditRow{sampleRow()}), clientID: 900}, rsp)
	if rsp.err == nil {
		t.Fatal("an unreachable chain was reported as a successful replay")
	}
}

// TestInstall_AutonomyReplay_AnUnreadableBodyIsNotRetriedForever: a body
// this build cannot parse will be the same bytes next time, so the answer
// has to be a countable refusal rather than a fault the node retries until
// its spool fills the disk.
func TestInstall_AutonomyReplay_AnUnreadableBodyIsNotRetriedForever(t *testing.T) {
	rec := &fakeAutonomyReplay{accepted: 1}
	fs := installAutonomyReplay(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditReplay)

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: []byte("{not json"), clientID: 900}, rsp)
	if rsp.err != nil {
		t.Fatalf("an unreadable body became an RPC error: %v", rsp.err)
	}
	out := decodeReplay(t, rsp.data)
	if out.Accepted != 0 {
		t.Errorf("accepted = %d, want 0", out.Accepted)
	}
	if _, _, calls := rec.snapshot(); calls != 0 {
		t.Error("an unreadable body reached the chain")
	}
}

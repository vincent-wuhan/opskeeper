package frontierbound

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
)

// fakeNodeLedger stands in for the chain. It records the batch it was
// handed and answers with whatever the test told it to.
type fakeNodeLedger struct {
	mu       sync.Mutex
	gotEdge  uint64
	gotRows  []tunnel.AuditEntry
	accepted int
	rejected int
	wantErr  error
	calls    int
}

func (f *fakeNodeLedger) RecordNodeEntries(_ context.Context, edgeID uint64, rows []tunnel.AuditEntry) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotEdge = edgeID
	f.gotRows = append([]tunnel.AuditEntry(nil), rows...)
	return f.accepted, f.rejected, f.wantErr
}

func (f *fakeNodeLedger) snapshot() (uint64, []tunnel.AuditEntry, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotEdge, append([]tunnel.AuditEntry(nil), f.gotRows...), f.calls
}

func installNodeLedger(t *testing.T, rec NodeLedgerRecorder) *fakeService {
	t.Helper()
	fs := newFakeService()
	c := newWithService(fs, slog.Default())
	if err := Install(context.Background(), c, Wiring{
		EdgeAuthn:      &edgebiz.AccessKeyAuthenticator{},
		EdgeUC:         &edgebiz.Usecase{},
		MetricIngester: &fakeMetricIngester{},
		AutonomyReplay: &fakeAutonomyReplay{},
		NodeLedger:     rec,
		Log:            slog.Default(),
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	return fs
}

func ledgerBody(t *testing.T, edgeID uint64, rows []tunnel.AuditEntry) []byte {
	t.Helper()
	b, err := json.Marshal(tunnel.AuditEntriesRequest{EdgeID: edgeID, Entries: rows})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func decodeLedger(t *testing.T, data []byte) tunnel.AuditEntriesResponse {
	t.Helper()
	var out tunnel.AuditEntriesResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode ledger response %q: %v", data, err)
	}
	return out
}

func sampleEntry() tunnel.AuditEntry {
	return tunnel.AuditEntry{
		At:      time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		Actor:   "operator@example.invalid",
		Action:  "tool_call",
		Target:  "host_bash",
		Outcome: "allowed",
		Class:   "write",
	}
}

// A manager with no chain behind the method must not answer "took none of
// it": the node reads that as "keep trying" and replays forever into
// nothing.
func TestInstall_NodeLedger_IsNotRegisteredWithoutARecorder(t *testing.T) {
	fs := installNodeLedger(t, nil)
	if _, ok := fs.rpcs[tunnel.MethodAgentAuditEntries]; ok {
		t.Error("agent.audit.entries was registered with no chain behind it")
	}
}

// The two node methods are independent. A manager that wired the autonomy
// chain and not this one is a manager that can take a self-heal and not a
// tool call, and the node has to be able to see which half is missing.
func TestInstall_NodeLedger_IsIndependentOfTheAutonomyReplay(t *testing.T) {
	fs := installNodeLedger(t, nil)
	if _, ok := fs.rpcs[tunnel.MethodAgentAuditReplay]; !ok {
		t.Error("the autonomy replay went missing because this one is unwired")
	}
}

func TestInstall_NodeLedger_DeferredUntilTheEdgeIsBound(t *testing.T) {
	rec := &fakeNodeLedger{accepted: 1}
	fs := installNodeLedger(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditEntries)

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: ledgerBody(t, 0, []tunnel.AuditEntry{sampleEntry()}), clientID: 555}, rsp)
	if rsp.err != nil {
		t.Fatalf("a deferred batch became an RPC error: %v", rsp.err)
	}
	if out := decodeLedger(t, rsp.data); out.Accepted != 0 {
		t.Errorf("accepted = %d, want 0 so the node keeps its rows", out.Accepted)
	}
	if _, _, calls := rec.snapshot(); calls != 0 {
		t.Error("an unbound batch reached the chain")
	}
}

func TestInstall_NodeLedger_BodyEdgeIDBindsTheTransport(t *testing.T) {
	rec := &fakeNodeLedger{accepted: 1}
	fs := installNodeLedger(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditEntries)

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: ledgerBody(t, 42, []tunnel.AuditEntry{sampleEntry()}), clientID: 900}, rsp)
	if rsp.err != nil {
		t.Fatalf("agent.audit.entries returned an error: %v", rsp.err)
	}
	edge, rows, calls := rec.snapshot()
	if calls != 1 {
		t.Fatalf("chain called %d times, want 1", calls)
	}
	if edge != 42 {
		t.Errorf("edge = %d, want the bound 42", edge)
	}
	if len(rows) != 1 || rows[0].Action != "tool_call" || rows[0].Target != "host_bash" {
		t.Errorf("rows = %+v, want the one that was sent", rows)
	}
	if !rows[0].At.Equal(sampleEntry().At) {
		t.Errorf("the row's time came back as %v; the node's clock is the fact", rows[0].At)
	}
	if out := decodeLedger(t, rsp.data); out.Accepted != 1 {
		t.Errorf("accepted = %d, want the chain's count", out.Accepted)
	}
}

// The one that must not be got wrong here. A node's rows are its testimony
// about a host; a node that could file another host's testimony would make
// the chain say something false about a machine it does not own.
func TestInstall_NodeLedger_TrustsTheTransportEdgeID(t *testing.T) {
	rec := &fakeNodeLedger{accepted: 1}
	fs := installNodeLedger(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditEntries)

	rpc(context.Background(), &fakeReq{data: ledgerBody(t, 42, nil), clientID: 900}, &fakeResp{})
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: ledgerBody(t, 7, []tunnel.AuditEntry{sampleEntry()}), clientID: 900}, rsp)

	edge, _, _ := rec.snapshot()
	if edge != 42 {
		t.Errorf("the body's claimed edge id moved the write: edge = %d, want 42", edge)
	}
}

// One malformed batch must not wedge a backlog that is otherwise fine, so
// the answer is a refusal count rather than an RPC error.
func TestInstall_NodeLedger_AMalformedBodyIsARefusalNotAnError(t *testing.T) {
	rec := &fakeNodeLedger{accepted: 5}
	fs := installNodeLedger(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditEntries)

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: []byte("{not json"), clientID: 900}, rsp)
	if rsp.err != nil {
		t.Fatalf("a malformed body became an RPC error: %v", rsp.err)
	}
	out := decodeLedger(t, rsp.data)
	if out.Accepted != 0 || out.Rejected != 0 {
		t.Errorf("got %d/%d, want 0/0 — neither taken nor refused, so the node retries", out.Accepted, out.Rejected)
	}
	if out.Reason == "" {
		t.Error("the refusal does not say why")
	}
	if _, _, calls := rec.snapshot(); calls != 0 {
		t.Error("an undecodable body reached the chain")
	}
}

func TestInstall_NodeLedger_ACountPassesThroughUnchanged(t *testing.T) {
	rec := &fakeNodeLedger{accepted: 0, rejected: 2}
	fs := installNodeLedger(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditEntries)

	rows := []tunnel.AuditEntry{sampleEntry(), sampleEntry()}
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: ledgerBody(t, 42, rows), clientID: 900}, rsp)
	out := decodeLedger(t, rsp.data)
	if out.Accepted != 0 || out.Rejected != 2 {
		t.Fatalf("got %d/%d, want 0/2", out.Accepted, out.Rejected)
	}
}

// A chain that is unreachable has to reach the node as an error, because
// that is the one answer the node retries. Anything else loses the rows.
func TestInstall_NodeLedger_AChainFailureIsAnErrorSoTheNodeRetries(t *testing.T) {
	rec := &fakeNodeLedger{wantErr: errors.New("database is down")}
	fs := installNodeLedger(t, rec)
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditEntries)

	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: ledgerBody(t, 42, []tunnel.AuditEntry{sampleEntry()}), clientID: 900}, rsp)
	if rsp.err == nil {
		t.Fatal("a chain failure came back as a success; the node would drop rows that are still only on disk")
	}
}

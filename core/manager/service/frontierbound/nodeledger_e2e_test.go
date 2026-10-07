package frontierbound

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	auditstore "github.com/vincent-wuhan/opskeeper/core/domains/data/audit/store"
	auditmodel "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
	"github.com/vincent-wuhan/opskeeper/core/edge/auditlog"
	"github.com/vincent-wuhan/opskeeper/core/edge/auditwire"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The end-to-end proof that a node's ledger row survives the whole route.
//
// Decision 126 closed the node plane's path to the chain and wrote in this
// ledger that it was "全线贯通". Every hop of it has a test: the gate records
// (core/edge/policygate), the sink and the pump (core/edge/auditlog), the
// three refusals (cmd/opskeeper-edge), the handler and its identity rules
// (this package), and the chain write (core/domains/biz/audit). And none of
// those tests could have failed if the route were broken, because each one
// stops at a fake the next hop is standing in for. **A route whose hops are
// all tested and which has never been walked is not tested.**
//
// So here nothing is stubbed but the socket. The real gate decides, the real
// sink appends to a real file, the real pump drains it, the real sender
// builds the batch and interprets the answer, the body crosses as JSON
// exactly as it crosses the wire, the handler installed by Install records
// it, the real NodeLedger converts it, and the real Usecase writes it to a
// real SQLite chain. The assertion is that the row is readable afterwards
// and that it is *in the chain* — linked to the row before it — because a
// row that arrived without a seal is a row nothing can prove later.
//
// This is the shape decision 324 used for the gateway's sink, and it is the
// only shape that catches wiring loss: delete the sender, the pump's link or
// the handler's registration and this test fails while every hop test in the
// tree stays green.

// loopbackTunnel is the one fake: a tunnel client that hands the real
// request body to a real registered handler, in-process.
//
// It marshals and unmarshals rather than passing structs, because the JSON
// round trip is where a renamed field would hide — a Go-struct hand-off
// would keep working with a field the wire never carries.
type loopbackTunnel struct {
	mu   sync.Mutex
	rpc  func(ctx context.Context, req *fakeReq, rsp *fakeResp)
	edge uint64
}

func (l *loopbackTunnel) Call(ctx context.Context, method string, req, resp any) error {
	l.mu.Lock()
	rpc := l.rpc
	l.mu.Unlock()
	if rpc == nil {
		return errNoLoopbackHandler
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	rsp2 := &fakeResp{}
	rpc(ctx, &fakeReq{data: body, clientID: l.edge}, rsp2)
	if rsp2.err != nil {
		return rsp2.err
	}
	return json.Unmarshal(rsp2.data, resp)
}

func (l *loopbackTunnel) Dial(context.Context) error             { return nil }
func (l *loopbackTunnel) RegisterHandler(string, tunnel.Handler) {}
func (l *loopbackTunnel) AcceptStream() (tunnel.StreamConn, error) {
	return nil, errNoLoopbackHandler
}
func (l *loopbackTunnel) OnReconnect(func()) {}
func (l *loopbackTunnel) Close() error       { return nil }

var errNoLoopbackHandler = errLoopback("no handler is registered on the loopback")

type errLoopback string

func (e errLoopback) Error() string { return string(e) }

// readOnly permits exactly one tool and refuses the rest, so the ledger
// receives one allowed call and one refusal.
//
// The first version of this file allowed everything and still named the test
// after a blocked call. It passed, and it was asserting nothing it said: a
// ledger that only ever records successes is the failure this whole route
// exists to prevent, and a test that cannot produce one will go on passing
// if the gate quietly stops writing refusals. **一个测不出「拒绝」这条路的测试，
// 等于没有测那条路。**
type readOnly struct{}

func (readOnly) Permitted(c policygate.Call) (bool, string) {
	if c.ToolName == "host_lsof" {
		return true, ""
	}
	return false, "not in the read-only allow-list"
}
func (readOnly) NeedsApproval(policygate.Call) bool              { return false }
func (readOnly) EffectiveClass(policygate.Call) domain.ToolClass { return domain.ClassRead }
func (readOnly) MaxRadius(policygate.Call) domain.BlastRadius    { return domain.RadiusPod }

func newChainedAuditUC(t *testing.T) (*audit.Usecase, *gorm.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	db, err := gorm.Open(sqlite.Open("file:"+path+"?_pragma=busy_timeout(5000)"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := auditstore.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cs := auditstore.NewChainStore(db)
	return audit.New(auditstore.New(db), nil, audit.WithChain("e2e-key", cs)), db
}

func TestAToolCallAndARefusalLandInTheRealChain(t *testing.T) {
	ctx := context.Background()
	uc, db := newChainedAuditUC(t)

	// The manager half: the real handler, installed by the real Install,
	// writing into the real chain.
	fs := installNodeLedger(t, NewNodeLedger(uc))
	rpc := rpcFor(t, fs, tunnel.MethodAgentAuditEntries)

	// The node half: a real gate deciding, into a real file.
	dir := t.TempDir()
	sink, err := auditlog.Open(filepath.Join(dir, "audit-ledger.jsonl"), 0)
	if err != nil {
		t.Fatalf("auditlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	gate, err := policygate.New(policygate.Options{Policy: readOnly{}, Audit: sink})
	if err != nil {
		t.Fatalf("policygate.New: %v", err)
	}

	// The wire: the real sender, over the loopback.
	lo := &loopbackTunnel{edge: 42}
	lo.rpc = func(ctx context.Context, req *fakeReq, rsp *fakeResp) { rpc(ctx, req, rsp) }
	pump, err := auditlog.NewPump(auditlog.PumpOptions{
		Sink:      sink,
		Sender:    auditwire.NewSender(lo, func() uint64 { return 42 }, slog.New(slog.NewTextHandler(io.Discard, nil)), nil),
		Reachable: func() bool { return true },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}

	// One allowed and one refused, so both of the rows an operator asks
	// this ledger for are actually produced.
	for _, tool := range []string{"host_lsof", "host_bash"} {
		if _, _, err := gate.Admit(ctx, policygate.Call{
			SessionID: "s-1", ToolName: tool, Target: "host-01",
			Summary: "inspect " + tool,
		}); err != nil {
			t.Fatalf("Admit(%s): %v", tool, err)
		}
	}
	if n, err := pump.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	} else if n != 2 {
		t.Fatalf("drained %d rows, want 2", n)
	}
	if pending, _ := pump.Pending(); pending != 0 {
		t.Errorf("%d rows left on the node after the center took them", pending)
	}

	// And now the claim: the rows are in the chain, linked, filed under the
	// node, and marked as the node's own testimony.
	chained, err := auditstore.NewChainStore(db).ListChained(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListChained: %v", err)
	}
	if len(chained) != 2 {
		t.Fatalf("%d chained rows, want 2", len(chained))
	}
	prev := ""
	for i, r := range chained {
		if r.Role != "edge" {
			t.Errorf("row %d role = %q, want edge", i, r.Role)
		}
		if r.ResourceType != auditmodel.ResourceEdge || r.ResourceID != "42" {
			t.Errorf("row %d filed under %s/%s, want edge/42", i, r.ResourceType, r.ResourceID)
		}
		if r.ResourceName != "host-01" {
			t.Errorf("row %d resource_name = %q", i, r.ResourceName)
		}
		if r.Hash == "" {
			t.Errorf("row %d has no seal: it arrived, it is not evidence", i)
		}
		if r.PrevHash != prev {
			t.Errorf("row %d prev_hash = %q, want %q: the node's rows must be inside the chain, not beside it", i, r.PrevHash, prev)
		}
		prev = r.Hash
	}
	if chained[0].Action != auditmodel.ActionNodeToolCall || chained[0].Status != auditmodel.StatusSuccess {
		t.Errorf("the allowed call landed as %s/%s", chained[0].Action, chained[0].Status)
	}
	// The refusal is the row that matters most and the one most likely to go
	// missing: a gate that stops recording what it stopped is still a gate.
	if chained[1].Action != auditmodel.ActionNodeToolBlocked || chained[1].Status != auditmodel.StatusDenied {
		t.Errorf("the refusal landed as %s/%s", chained[1].Action, chained[1].Status)
	}
}

// The chain link is the whole point of the route, so it gets its own
// assertion against a row that did not come from a node: two node rows and
// one console row have to form ONE chain, not two. That is the claim
// decision 126 made in prose — "链上现在各有一条" — and this is the first
// test that could have said otherwise.
func TestANodeRowAndAConsoleRowShareOneChain(t *testing.T) {
	ctx := context.Background()
	uc, db := newChainedAuditUC(t)

	uc.Emit(ctx, audit.Event{
		UserEmail: "op@example.invalid", Role: "admin",
		Action: auditmodel.ActionPluginReleaseStart, ResourceType: "plugin",
		ResourceID: "p-1", ResourceName: "kube-audit", Status: auditmodel.StatusSuccess,
	})
	res, err := uc.RecordNodeEntries(ctx, 42, []audit.NodeLedgerRow{{
		At: time.Now().UTC(), Actor: "node-42", Action: ports.ActionToolCall,
		Target: "host_bash", Outcome: "allowed", Class: "write",
	}})
	if err != nil {
		t.Fatalf("RecordNodeEntries: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("accepted %d, want 1", res.Accepted)
	}
	chained, err := auditstore.NewChainStore(db).ListChained(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListChained: %v", err)
	}
	if len(chained) != 2 {
		t.Fatalf("%d chained rows, want 2", len(chained))
	}
	if chained[0].PrevHash != "" {
		t.Errorf("the first row claims a predecessor: %q", chained[0].PrevHash)
	}
	if chained[1].PrevHash != chained[0].Hash {
		t.Errorf("the node row does not link to the console row: %q vs %q",
			chained[1].PrevHash, chained[0].Hash)
	}
}

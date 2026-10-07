package agentkernel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The interface this binding exists to satisfy, declared at the definition
// site so a contract change breaks here rather than at the assembly in cmd.
var _ ports.AuditSink = (*AuditLedger)(nil)

// recordingLedger captures the rows the adapter emitted. It records rather
// than asserts so a test can inspect the mapping the adapter chose.
type recordingLedger struct {
	events []auditport.Event
	err    error
}

func (l *recordingLedger) EmitWithID(_ context.Context, ev auditport.Event) (uint64, error) {
	l.events = append(l.events, ev)
	if l.err != nil {
		return 0, l.err
	}
	return uint64(len(l.events)), nil
}

func TestAuditLedgerKeepsARefusalDistinctFromAFailure(t *testing.T) {
	// The kernel's outcome vocabulary separates a policy refusal from a
	// broken tool, and the audit table's status set does not. The row keeps
	// the kernel's word in error_code so the console can still tell them
	// apart; collapsing both into "failure" would make a refused restart
	// read as a crashed one.
	w := &recordingLedger{}
	l := NewAuditLedger(w, nil)
	_ = l.Record(context.Background(), ports.AuditEntry{
		Action: ports.ActionToolBlocked, Target: "host_restart_service", Outcome: "blocked"})
	_ = l.Record(context.Background(), ports.AuditEntry{
		Action: ports.ActionToolFailed, Target: "query_promql", Outcome: "error"})
	_ = l.Record(context.Background(), ports.AuditEntry{
		Action: ports.ActionToolCall, Target: "get_topology", Outcome: "success"})

	if len(w.events) != 3 {
		t.Fatalf("rows = %d, want 3", len(w.events))
	}
	if w.events[0].Status != "denied" || w.events[0].ErrorCode != "blocked" {
		t.Fatalf("blocked row = %q/%q, want denied/blocked", w.events[0].Status, w.events[0].ErrorCode)
	}
	if w.events[1].Status != "failure" || w.events[1].ErrorCode != "error" {
		t.Fatalf("failed row = %q/%q, want failure/error", w.events[1].Status, w.events[1].ErrorCode)
	}
	if w.events[2].Status != "success" || w.events[2].ErrorCode != "" {
		t.Fatalf("success row = %q/%q, want success/empty", w.events[2].Status, w.events[2].ErrorCode)
	}
}

func TestAuditLedgerNeverReadsAnUnknownOutcomeAsSuccess(t *testing.T) {
	// An outcome this build does not know was chosen by a newer build. The
	// one interpretation an incident review must never be handed is "we do
	// not know what happened, so it worked".
	w := &recordingLedger{}
	l := NewAuditLedger(w, nil)
	_ = l.Record(context.Background(), ports.AuditEntry{
		Action: ports.ActionToolCall, Target: "x", Outcome: "quarantined"})

	if w.events[0].Status != "failure" {
		t.Fatalf("status = %q, want failure", w.events[0].Status)
	}
	if w.events[0].ErrorCode != "quarantined" {
		t.Fatalf("error_code = %q, want the original word preserved", w.events[0].ErrorCode)
	}
}

func TestAuditLedgerCarriesActorClassAndDetail(t *testing.T) {
	// The row's columns hold the action vocabulary the audit table already
	// owns; the gate's own context rides in the payload, because a ledger
	// that dropped the actor could not answer "who asked for this" and one
	// that dropped the class could not answer "how dangerous was it".
	w := &recordingLedger{}
	l := NewAuditLedger(w, nil)
	at := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	_ = l.Record(context.Background(), ports.AuditEntry{
		At:      at,
		Actor:   "agent:sess-7",
		Action:  ports.ActionToolBlocked,
		Target:  "cloud_bash",
		Class:   "destructive",
		Outcome: "blocked",
		Detail:  json.RawMessage(`{"reason":"no approval gate configured"}`),
	})

	ev := w.events[0]
	if ev.ResourceType != "tool" || ev.ResourceName != "cloud_bash" {
		t.Fatalf("resource = %q/%q, want tool/cloud_bash", ev.ResourceType, ev.ResourceName)
	}
	raw, err := json.Marshal(ev.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got["actor"] != "agent:sess-7" {
		t.Fatalf("actor = %v", got["actor"])
	}
	if got["class"] != "destructive" {
		t.Fatalf("class = %v", got["class"])
	}
	if !strings.HasPrefix(got["at"].(string), "2026-05-01T10:00:00") {
		t.Fatalf("at = %v", got["at"])
	}
	if _, ok := got["detail"].(map[string]any); !ok {
		t.Fatalf("detail was not kept as structured JSON: %T", got["detail"])
	}
}

func TestAuditLedgerKeepsAnUndecodableDetailAsText(t *testing.T) {
	// Dropping it would lose the only description of a call that may have
	// changed a production system.
	w := &recordingLedger{}
	l := NewAuditLedger(w, nil)
	_ = l.Record(context.Background(), ports.AuditEntry{
		Action: ports.ActionToolCall, Target: "x", Outcome: "success",
		Detail: []byte("not json")})

	got := w.events[0].Payload.(map[string]any)
	if got["detail_raw"] != "not json" {
		t.Fatalf("detail_raw = %v, want the raw text", got["detail_raw"])
	}
}

func TestAuditLedgerRecordPropagatesAWriteFailure(t *testing.T) {
	// The kernel ignores this error on purpose (a storage hiccup must not
	// fail an investigation), so the adapter must not also swallow it —
	// otherwise the two decisions compound into a silent drop.
	w := &recordingLedger{err: errors.New("ledger is down")}
	l := NewAuditLedger(w, nil)
	err := l.Record(context.Background(), ports.AuditEntry{Action: ports.ActionToolCall, Outcome: "success"})
	if err == nil {
		t.Fatal("a failed write was reported as success")
	}
}

func TestAuditLedgerVerifyRefusesToClaimTheLedgerIsIntact(t *testing.T) {
	// nil means "the chain is intact". A ledger with no verifier returning
	// nil would hand an operator asking about tampering a clean bill of
	// health from an absence.
	l := NewAuditLedger(&recordingLedger{}, nil)
	if err := l.Verify(context.Background()); !errors.Is(err, ErrNoChainVerifier) {
		t.Fatalf("Verify = %v, want ErrNoChainVerifier", err)
	}
}

// stubVerifier lets a test decide what the host's chain walk reports.
type stubVerifier struct{ err error }

func (s stubVerifier) VerifyChain(context.Context) error { return s.err }

func TestAuditLedgerVerifyDelegatesToTheHostChain(t *testing.T) {
	// The chain is host-wide: it also covers the middleware's rows, so the
	// binding must hand the question to the host rather than check the two
	// entries it happens to have written.
	broken := errors.New("audit: chain broken at seq 9")
	l := NewAuditLedger(&recordingLedger{}, stubVerifier{err: broken})
	if err := l.Verify(context.Background()); !errors.Is(err, broken) {
		t.Fatalf("Verify = %v, want the host verifier's error", err)
	}
	intact := NewAuditLedger(&recordingLedger{}, stubVerifier{})
	if err := intact.Verify(context.Background()); err != nil {
		t.Fatalf("Verify on an intact chain = %v, want nil", err)
	}
}

func TestNewAuditLedgerRefusesToWrapNothing(t *testing.T) {
	// A sink that accepted nil would look wired and record nothing, which
	// is the failure mode the operator cannot see.
	if l := NewAuditLedger(nil, nil); l != nil {
		t.Fatalf("ledger = %v, want nil", l)
	}
	var l *AuditLedger
	if err := l.Record(context.Background(), ports.AuditEntry{}); err != nil {
		t.Fatalf("a nil ledger must be inert, not panic: %v", err)
	}
}

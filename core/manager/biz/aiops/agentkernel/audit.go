package agentkernel

import (
	"context"
	"encoding/json"
	"errors"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// LedgerWriter and LedgerVerifier used to be declared here, one method
// each, and decision 272 deleted both in favour of the two port types that
// say the same thing in the package that owns the row shape. The split is
// preserved rather than merged into one interface, and the reason is the one
// the old comment on LedgerVerifier gave and is worth keeping verbatim: "a
// binding that could both write and check its own writes is a binding whose
// Verify result means nothing". The host's verifier walks the table rather
// than any in-memory state the writer kept.
//
// Deleting them rather than aliasing them is the point. An alias would have
// left two names for one interface, and the next reader would have had to
// work out which one a new binding should satisfy.
type (
	// LedgerWriter is the narrow seam onto the host's durable audit row
	// writer — the port's IDSink, named for the kernel's own vocabulary.
	LedgerWriter = auditport.IDSink
	// LedgerVerifier is the seam onto the host's chain verification.
	LedgerVerifier = auditport.Verifier
)

// AuditLedger adapts the host's audit_logs writer to the kernel's ledger
// port.
//
// The kernel derives every entry from its own gate decisions (it sees the
// refusal, the failure, and the settlement), so this adapter maps fields
// and nothing else: it decides no policy, classifies no tool, and never
// invents an outcome. That division is the point of the port — a plugin or
// a model cannot reach this sink, and the sink itself has nothing to decide.
type AuditLedger struct {
	writer   LedgerWriter
	verifier LedgerVerifier
}

// NewAuditLedger wraps a writer. A nil writer yields nil rather than a
// ledger that silently drops every row: the caller wires Audit: nil to mean
// "this deployment does not record", and a non-nil sink that records
// nothing would hide the misconfiguration behind a successful return.
//
// verifier may be nil, in which case Verify reports that this binding
// cannot verify. That is a different answer from "the chain is intact",
// and it is the answer a wiring mistake deserves.
func NewAuditLedger(w LedgerWriter, v LedgerVerifier) *AuditLedger {
	if w == nil {
		return nil
	}
	return &AuditLedger{writer: w, verifier: v}
}

// Record writes one entry.
//
// A write failure is returned rather than swallowed so the kernel's caller
// can see it; the kernel itself ignores the error because refusing a tool
// because a log row could not be written would turn a storage hiccup into a
// failed investigation. The row is the record of what the gate decided, and
// it is written before the outcome is reported.
func (l *AuditLedger) Record(ctx context.Context, entry ports.AuditEntry) error {
	if l == nil || l.writer == nil {
		return nil
	}
	payload := map[string]any{}
	if entry.Actor != "" {
		payload["actor"] = entry.Actor
	}
	if entry.Class != "" {
		payload["class"] = entry.Class
	}
	if !entry.At.IsZero() {
		payload["at"] = entry.At.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	}
	if len(entry.Detail) > 0 {
		var detail any
		if err := json.Unmarshal(entry.Detail, &detail); err == nil {
			payload["detail"] = detail
		} else {
			// An undecodable detail is stored as its raw text: dropping it
			// would lose the only description of a call that may have
			// changed a production system.
			payload["detail_raw"] = string(entry.Detail)
		}
	}
	_, err := l.writer.EmitWithID(ctx, auditport.Event{
		Action:       string(entry.Action),
		ResourceType: "tool",
		ResourceName: entry.Target,
		Status:       ledgerStatus(entry.Outcome),
		ErrorCode:    ledgerErrorCode(entry.Outcome),
		Payload:      payload,
	})
	return err
}

// Verify walks the host's hash chain and reports the first entry that
// does not check out.
//
// The chain is host-wide, not per-kernel: it covers every audit row the
// manager writes, so a break reported here may be in a row this binding
// never wrote. That is the point — the kernel's entries are only
// trustworthy for as long as the chain they sit in is, and an operator
// asking "was this tool call tampered with" needs an answer that covers
// everything between the entries too.
//
// With no verifier wired this returns ErrNoChainVerifier rather than nil.
// A ledger that reports an intact chain because nobody gave it a way to
// check is the exact failure this method exists to prevent.
func (l *AuditLedger) Verify(ctx context.Context) error {
	if l == nil || l.verifier == nil {
		return ErrNoChainVerifier
	}
	return l.verifier.VerifyChain(ctx)
}

// ErrNoChainVerifier reports that this binding was constructed without a
// chain verifier. It is an error rather than a nil return because nil
// means "the chain is intact", and that is the one thing an unverified
// ledger must never appear to say.
var ErrNoChainVerifier = errors.New("agentkernel: audit ledger has no chain verifier wired")

// ledgerStatus maps the kernel's outcome vocabulary onto the audit table's
// bounded status set.
//
// The mapping is deliberately lossy in one direction only: an unrecognised
// outcome becomes a failure, never a success. The kernel's outcomes are
// derived from a gate decision the host made, so a value this build does not
// know is a value a newer build chose — and reading "we do not know what
// happened" as "it worked" is the one interpretation an incident review must
// never be handed.
func ledgerStatus(outcome string) string {
	switch outcome {
	case "success":
		return "success"
	case "blocked":
		return "denied"
	default:
		return "failure"
	}
}

// ledgerErrorCode keeps the kernel's own outcome word on the row for the
// cases where the table's status set is coarser than the decision. Without
// it a blocked call and a crashed call both read as a non-success row and
// the console cannot tell a policy refusal from a broken tool.
func ledgerErrorCode(outcome string) string {
	switch outcome {
	case "success":
		return ""
	default:
		return outcome
	}
}

var _ ports.AuditSink = (*AuditLedger)(nil)

// Package projection turns a production root-cause contract into the shape
// the judge scores.
//
// This is the last hop of the evaluation loop: the investigated phase
// writes a RootCauseJSON, and `opskeeper-eval judge` scores a
// judge.AgentResponse. Nothing connected the two, so the judge could only
// ever score a hand-written response file — the corpus was never actually
// held to anything the system produced.
//
// The projection is deliberately partial. Three vocabularies meet here and
// only one of them lines up:
//
//   - remediation_options[].action is `<family>.<method>` (pg.terminate_long_tx),
//     the same shape the golden cases expect, so it maps directly;
//   - root_cause_object.kind is a closed enum (pg_lock), a different namespace
//     from anything a case can name;
//   - evidence_chain[].tool is a bare, cross-family name (query_promql) that
//     names no resource at all.
//
// Only the first is mapped. The second is resolved through a caller-supplied
// Resolver or reported as unmapped — it is never guessed. A projection that
// emitted pg_lock into a field the judge reads as "the root causes that
// matched" would manufacture a guaranteed zero and dress it up as a verdict,
// which is the specific failure this package exists to prevent. The third is
// carried through as tool calls, where the judge uses it for reasoning rather
// than for exact symbol matching.
//
// The Doc type mirrors the wire form of the investigated phase's contract.
// It is a mirror rather than an import because the evaluation plane must not
// depend on the control plane's implementation (see the harness module rule
// in scripts/modulecheck). Mirrors drift, so TestTheMirrorStillMatchesThe
// Contract round-trips a real contract through the wire format and is the
// thing that catches it when one side changes.
package projection

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/harness/judge"
)

// Doc is the wire form of the investigated phase's RootCauseJSON.
type Doc struct {
	SchemaVersion      string              `json:"schema_version"`
	RootCauseObject    *RootCauseObject    `json:"root_cause_object"`
	Confidence         float64             `json:"confidence"`
	EvidenceChain      []EvidenceItem      `json:"evidence_chain"`
	TimeWindow         TimeWindow          `json:"time_window"`
	RemediationOptions []RemediationOption `json:"remediation_options"`
}

// RootCauseObject is the inner typed root-cause descriptor.
type RootCauseObject struct {
	Kind    string         `json:"kind"`
	Summary string         `json:"summary"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// EvidenceItem is one piece of evidence supporting the root cause.
type EvidenceItem struct {
	Tool      string    `json:"tool"`
	Query     string    `json:"query,omitempty"`
	Value     any       `json:"value,omitempty"`
	Count     int       `json:"count,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// TimeWindow is the search interval the investigator used.
type TimeWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// RemediationOption is one candidate fix action.
type RemediationOption struct {
	Action      string `json:"action"`
	Target      string `json:"target"`
	Risk        string `json:"risk"`
	AutoApprove bool   `json:"auto_approve"`
}

// Timing carries the wall-clock marks the judge scores time_efficiency
// against. A zero mark means "not observed", and is treated as such rather
// than as an instant at the zero time — a response that never recorded when
// it recovered is not one that recovered in zero milliseconds.
type Timing struct {
	// DetectedAt is when the alert fired.
	DetectedAt time.Time
	// InvestigatedAt is when the investigated phase produced its contract.
	InvestigatedAt time.Time
	// RecoveredAt is when recovery was verified.
	RecoveredAt time.Time
}

// Resolver maps a contract root-cause kind onto the symbols the judge can
// score. Returning ok=false means this build has no expression for that kind,
// which is reported rather than guessed at.
//
// The seam is a parameter because the mapping is a judgement about meaning,
// not a fact about either vocabulary. pg_lock and pg.lock_waits may or may
// not be the same thing; only someone who knows the domain can say, and
// hard-coding an answer here would make that decision invisibly and forever.
type Resolver func(kind string) (symbols []string, ok bool)

// Result is a projection plus what it could not carry across.
type Result struct {
	// Response is ready to be scored.
	Response *judge.AgentResponse
	// UnmappedKind is the contract's root-cause kind that no Resolver
	// could express, empty when the root cause mapped cleanly or the
	// contract had none.
	UnmappedKind string
	// UnmappedReason explains UnmappedKind in one line.
	UnmappedReason string
}

// RootCauseIsScorable reports whether the projection carries a root cause
// the judge can compare against the case. A false here means rca_accuracy
// for this response is structurally zero, and the reason is the projection
// rather than the agent.
func (r *Result) RootCauseIsScorable() bool {
	return r.UnmappedKind == "" && len(r.Response.RootCause) > 0
}

// ErrEmptyContract is returned for a document that carries no conclusion.
var ErrEmptyContract = errors.New("root-cause contract is empty")

// FromContract projects a RootCauseJSON document onto a judge.AgentResponse.
//
// resolve may be nil, which means no kind can be expressed — the projection
// then reports the kind as unmapped rather than dropping it silently, because
// a response with no root cause and no explanation of why is the one output
// that reads as a diagnosis failure.
func FromContract(doc *Doc, timing Timing, resolve Resolver) (*Result, error) {
	if doc == nil || doc.RootCauseObject == nil {
		return nil, ErrEmptyContract
	}
	kind := doc.RootCauseObject.Kind
	if kind == "" {
		return nil, fmt.Errorf("%w: root_cause_object.kind is empty", ErrEmptyContract)
	}
	if len(doc.RemediationOptions) == 0 {
		return nil, fmt.Errorf("%w: %s proposes no remediation, so there is nothing to have done",
			ErrEmptyContract, kind)
	}

	resp := &judge.AgentResponse{
		ToolCalls:    toolCalls(doc.EvidenceChain),
		Remediations: actions(doc.RemediationOptions),
		DetectMs:     elapsed(timing.DetectedAt, timing.InvestigatedAt),
		RemediateMs:  elapsed(timing.InvestigatedAt, timing.RecoveredAt),
	}

	res := &Result{Response: resp}
	if resolve != nil {
		if syms, ok := resolve(kind); ok && len(syms) > 0 {
			resp.RootCause = append([]string(nil), syms...)
		}
	}
	if len(resp.RootCause) == 0 {
		res.UnmappedKind = kind
		res.UnmappedReason = fmt.Sprintf(
			"the contract names its root cause as %q, which is not a symbol the judge can "+
				"compare against a case; supply a Resolver that maps contract kinds onto "+
				"case vocabulary rather than reading this as a wrong diagnosis", kind)
	}
	resp.ResponseHash = judge.ComputeResponseHash(resp)
	return res, nil
}

// toolCalls turns the evidence chain into the judge tool-call shape.
//
// The query travels as a single-key args object. That is an encoding choice,
// not a claim: the contract has one free-text field per evidence item and the
// judge's args field is free-form JSON, so wrapping the string preserves it
// without inventing a parameter structure the investigator never emitted.
func toolCalls(items []EvidenceItem) []judge.ToolCall {
	out := make([]judge.ToolCall, 0, len(items))
	for _, it := range items {
		call := judge.ToolCall{Name: it.Tool}
		if it.Query != "" {
			if args, err := json.Marshal(map[string]string{"query": it.Query}); err == nil {
				call.Args = args
			}
		}
		if it.Value != nil {
			if v, err := json.Marshal(it.Value); err == nil {
				call.Result = v
			}
		}
		out = append(out, call)
	}
	return out
}

// actions lists the proposed remediation actions, deduplicated in order.
func actions(opts []RemediationOption) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		if o.Action == "" || seen[o.Action] {
			continue
		}
		seen[o.Action] = true
		out = append(out, o.Action)
	}
	return out
}

// elapsed returns the gap between two marks in milliseconds, or 0 when
// either mark is missing or the order is wrong. A negative or absurd gap
// would be scored as excellent efficiency, so an unobserved interval reports
// as zero rather than as a fast one.
func elapsed(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return 0
	}
	return to.Sub(from).Milliseconds()
}

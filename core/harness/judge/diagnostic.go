// diagnostic.go implements the three diagnostic axes an outcome-only score
// cannot separate: Localization, Identification and Reason.
//
// The reference is arXiv:2606.29193, whose whole argument is that a benchmark
// which scores the final answer cannot see whether the agent reasoned: "they
// score only the final answer and fail to assess the systematic reasoning
// process in failure diagnosis", and which separates that process into
// exactly three questions —
//
//	Localization   where the fault occurs
//	Identification what type of fault it is
//	Reason         whether the reasoning trace is grounded in relevant evidence
//
// The three are added as dimensions on top of the existing four process dims
// (rca_accuracy / time_efficiency / remediation_quality / collateral_safety),
// not as a replacement, and Overall is deliberately left alone. Reweighting
// the headline number would silently invalidate every score already stored and
// every leaderboard comparison drawn from them, and the corpus has not yet
// produced the distribution that would justify a new weight set. What the
// axes change *today* is the verdict: a run whose outcome is good while its
// trace is not grounded is flagged for human review, which is the same remedy
// this package already applies to a judge it does not trust.
//
// They are computed mechanically rather than asked of a model. The axes are
// grounding checks against what the case declared — the model would be asked
// to judge whether an answer contains a resource name, and a model's answer to
// "did it say pg" is strictly worse than a substring test. The LLM judge
// therefore carries the same numbers as the heuristic one; what differs
// between them stays the judgement-heavy rca_accuracy.
//
// Unmeasured is not zero. A case that declares no locus has no localization
// dimension at all rather than a 0, because 0 reads as "the agent localized
// wrongly" and the truth is "nobody asked". The map expresses that natively by
// omitting the key, which is why the axes live there and not in a struct whose
// zero value would have to be interpreted.
package judge

import (
	"fmt"
	"strings"
)

// Dimension keys for the three axes. Exported because a leaderboard that
// wants to chart them should not spell them twice.
const (
	DimensionLocalization   = "localization"
	DimensionIdentification = "identification"
	DimensionReason         = "reason"
)

// Flag thresholds for the one behavioural consequence of the axes.
//
// Both have to hold before a run is flagged, and each floor is a statement
// about a different thing:
//
//   - DiagnosticOutcomeFloor is "the outcome was good". Below it, an
//     ungrounded trace is not a surprise — a wrong answer usually comes with
//     thin evidence, and flagging every one of those would drown the reviewer
//     queue it is meant to protect.
//   - DiagnosticReasonFloor is "the trace did not carry the evidence the case
//     requires". Half the required observations missing is the point where the
//     conclusion is no longer supported by what the agent actually looked at.
const (
	DiagnosticOutcomeFloor = 0.7
	DiagnosticReasonFloor  = 0.5
)

// DiagnosticAxes scores the three questions the case declares.
//
// Only declared axes come back. An absent key means the case said nothing
// about that question, and the caller must read it as "not measured" rather
// than as a zero — see the package comment.
func DiagnosticAxes(c *Case, r *AgentResponse) map[string]float64 {
	if c == nil || r == nil {
		return nil
	}
	answer := answerSurface(r)
	out := make(map[string]float64, 3)
	if len(c.ExpectedLocus) > 0 {
		out[DimensionLocalization] = tokenCoverage(answer, c.ExpectedLocus)
	}
	if len(c.ExpectedFaultType) > 0 {
		out[DimensionIdentification] = tokenCoverage(answer, c.ExpectedFaultType)
	}
	if len(c.ExpectedRootCause) > 0 {
		out[DimensionReason] = symbolCoverage(traceSurface(r), c.ExpectedRootCause)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyDiagnostic folds the axes into a score and applies the one verdict that
// follows from them.
//
// Called by every judge, including both paths of the LLM judge, so the three
// numbers are present whichever scorer produced the rest of the score — a
// dimension that only exists on one path is a dimension a leaderboard silently
// averages over half its rows.
func applyDiagnostic(s *Score, c *Case, r *AgentResponse) {
	if s == nil {
		return
	}
	axes := DiagnosticAxes(c, r)
	if len(axes) == 0 {
		return
	}
	if s.Dimensions == nil {
		s.Dimensions = make(map[string]float64, len(axes))
	}
	for key, value := range axes {
		s.Dimensions[key] = value
	}

	reason, measured := axes[DimensionReason]
	if !measured || reason > DiagnosticReasonFloor || s.Overall < DiagnosticOutcomeFloor {
		return
	}
	note := fmt.Sprintf(
		"the answer scores %.2f but the reasoning trace is ungrounded (reason=%.2f): the case's required observations are not in the agent's tool calls, so the conclusion may be right for the wrong reasons",
		s.Overall, reason)
	if s.FlagReason != "" {
		s.FlagReason += "; " + note
	} else {
		s.FlagReason = note
	}
	s.Flagged = true
}

// answerSurface is what the agent concluded: the root causes it claims and the
// remediations it proposes. Localization and Identification are questions about
// that conclusion — an operator reads the conclusion, not the tool traffic.
func answerSurface(r *AgentResponse) string {
	var b strings.Builder
	for _, line := range r.RootCause {
		b.WriteString(line)
		b.WriteByte(' ')
	}
	for _, line := range r.Remediations {
		b.WriteString(line)
		b.WriteByte(' ')
	}
	return strings.ToLower(b.String())
}

// traceSurface is what the agent actually did: the tool calls it made and the
// results it saw. Reason asks whether the trace is grounded in evidence, so it
// reads this and not the conclusion — that separation is the whole point of
// the axis. An agent that states the case's observations without ever having
// looked at them scores 0 here while rca_accuracy stays high, and that
// disagreement is the finding.
func traceSurface(r *AgentResponse) string {
	var b strings.Builder
	for _, call := range r.ToolCalls {
		b.WriteString(call.Name)
		b.WriteByte(' ')
		b.Write(call.Args)
		b.WriteByte(' ')
		b.Write(call.Result)
		b.WriteByte(' ')
	}
	return strings.ToLower(b.String())
}

// tokenCoverage is the fraction of tokens present in the surface.
//
// Substring rather than word equality because both sides are prose fragments:
// the case declares "oom" and the agent writes "OOMKilled", and a comparison
// that missed that would score a correct identification as a miss.
func tokenCoverage(surface string, tokens []string) float64 {
	if len(tokens) == 0 {
		return 0
	}
	hit := 0
	for _, token := range tokens {
		if strings.Contains(surface, strings.ToLower(token)) {
			hit++
		}
	}
	return float64(hit) / float64(len(tokens))
}

// symbolCoverage is the fraction of the case's required symbols found in the
// trace.
//
// A symbol is a vocabulary entry like "pg.lock_waits". Two forms count as a
// hit: the whole symbol (a trace that quotes the vocabulary directly, which is
// what the investigator tools return), or its tail with every underscore-word
// present (a trace that says "lock waits" in its own words). The tail form
// splits on underscores and requires *every* word, so "active_sessions" is not
// satisfied by a result that only mentions sessions — half a symbol is not the
// observation the case asked for.
func symbolCoverage(surface string, symbols []string) float64 {
	if len(symbols) == 0 {
		return 0
	}
	hit := 0
	for _, symbol := range symbols {
		if symbolInSurface(surface, symbol) {
			hit++
		}
	}
	return float64(hit) / float64(len(symbols))
}

func symbolInSurface(surface, symbol string) bool {
	lowered := strings.ToLower(symbol)
	if strings.Contains(surface, lowered) {
		return true
	}
	tail := lowered
	if dot := strings.IndexByte(lowered, '.'); dot >= 0 {
		tail = lowered[dot+1:]
	}
	words := strings.Split(tail, "_")
	for _, word := range words {
		if word == "" {
			continue
		}
		if !strings.Contains(surface, word) {
			return false
		}
	}
	return tail != ""
}

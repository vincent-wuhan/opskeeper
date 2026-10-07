// Package critic implements the post-ReAct audit loop (D.2).
//
// A critic worker reviews the output of a primary ReAct agent (coordinator
// / incident-investigator / specialist) and flags three classes of
// issues:
//
//   - unevidenced_claim: a root_cause or causal_chain node has no
//     matching tool_call evidence
//   - missed_tool: a key query (e.g. query_change_events for any
//     incident) was not called
//   - broken_chain: causal_chain has a non-causal jump or stops at the
//     symptom instead of the 0 号病人
//
// The loop runs at most MaxRounds times (default 2). If the primary
// agent's first correction still has issues, the loop gives up and
// surfaces the remaining issues alongside the latest report — the
// coordinator decides whether to ask a human to intervene.
//
// Design note: this package deliberately does NOT import chatruntime
// to avoid the cycle (chatruntime already imports basetool, which is
// used by aiops/tools). The Spawner interface is the narrow seam —
// the wiring site (cmd/opskeeper/main.go) provides a thin shim that
// delegates to *chatruntime.Runtime.
package critic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// IssueType enumerates the three audit categories. The critic persona
// prompt (agents/critic.md) emits these literal strings; we keep the
// vocabulary tight so downstream filtering (e.g. only surface
// "critical" issues to humans) is trivial.
type IssueType string

const (
	IssueUnevidencedClaim IssueType = "unevidenced_claim"
	IssueMissedTool       IssueType = "missed_tool"
	IssueBrokenChain      IssueType = "broken_chain"
)

// IssueSeverity ranks how blocking an issue is. "minor" is logged but
// not surfaced; "major" surfaces in the chat; "critical" forces a
// human-in-the-loop.
type IssueSeverity string

const (
	SeverityMinor    IssueSeverity = "minor"
	SeverityMajor    IssueSeverity = "major"
	SeverityCritical IssueSeverity = "critical"
)

// Issue is one critic finding.
type Issue struct {
	Type       IssueType     `json:"type"`
	Location   string        `json:"location"`
	Severity   IssueSeverity `json:"severity"`
	Evidence   string        `json:"evidence"`
	Suggestion string        `json:"suggestion"`
}

// CriticAudit is the structured verdict from one critic round.
type CriticAudit struct {
	Issues          []Issue `json:"issues"`
	NeedsCorrection bool    `json:"needs_correction"`
	Summary         string  `json:"summary"`
}

// PrimaryReport is the input the critic audits. The shape mirrors what
// the coordinator emits after a ReAct loop completes; we don't reach
// into the actual chatruntime types to keep the seam narrow.
type PrimaryReport struct {
	SessionID   string         `json:"session_id"`
	IncidentID  string         `json:"incident_id"`
	RootCause   string         `json:"root_cause"`
	CausalChain []string       `json:"causal_chain"`
	Symptom     string         `json:"symptom"`
	Confidence  float64        `json:"confidence"`
	ToolCalls   []ToolCallStub `json:"tool_calls"`
	Severity    string         `json:"severity"` // "critical" etc. — gates the loop on
}

// ToolCallStub is the minimum the critic needs about a tool invocation.
type ToolCallStub struct {
	Tool          string `json:"tool"`
	Args          string `json:"args,omitempty"`
	ResultSummary string `json:"result_summary,omitempty"`
}

// HasCriticalIssue reports whether the audit contains a critical-severity
// issue. Critical issues block the report from being delivered to the
// user even if NeedsCorrection is false (the coordinator surfaces the
// issue and asks the operator).
func (a *CriticAudit) HasCriticalIssue() bool {
	for i := range a.Issues {
		if a.Issues[i].Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// SpawnRequest is the seam-side spawn shape, parallel to the worker
// pattern in tools/agent_tool.go. The wiring shim translates this
// into the chatruntime-specific shape (Locale / Provider / Model / etc.).
type SpawnRequest struct {
	AgentName  string
	Prompt     string
	Background bool
}

// Spawner is the narrow interface the loop depends on. The concrete
// *chatruntime.Runtime satisfies this; tests inject a fake.
type Spawner interface {
	SpawnWorker(ctx context.Context, req SpawnRequest) (string, error) // returns workerID
	GetWorkerResult(ctx context.Context, workerID string, timeout time.Duration) (string, error)
}

// ParseAudit is a small helper used by tests and the loop: the critic
// agent emits its JSON verdict as the last complete JSON object in
// its transcript. Returns the parsed audit or an error if the
// transcript has no parseable JSON object.
//
// Strategy: find the LAST "}" in the transcript, then walk backward
// trying to find a matching "{" that yields a valid CriticAudit.
// This handles transcripts with prose + multiple JSON examples
// (common when the critic shows its reasoning).
func ParseAudit(transcript string) (*CriticAudit, error) {
	if transcript == "" {
		return nil, fmt.Errorf("critic: empty transcript")
	}
	// Find all positions of '}' in reverse order.
	for end := len(transcript) - 1; end >= 0; end-- {
		if transcript[end] != '}' {
			continue
		}
		// Walk backward to find the matching '{'.
		depth := 1
		start := -1
		for i := end - 1; i >= 0; i-- {
			switch transcript[i] {
			case '}':
				depth++
			case '{':
				depth--
				if depth == 0 {
					start = i
				}
			}
			if start >= 0 {
				break
			}
		}
		if start < 0 {
			continue
		}
		candidate := transcript[start : end+1]
		var a CriticAudit
		if err := json.Unmarshal([]byte(candidate), &a); err != nil {
			continue
		}
		if a.Summary != "" || len(a.Issues) > 0 {
			return &a, nil
		}
	}
	return nil, fmt.Errorf("critic: no valid audit JSON in transcript (len=%d)", len(transcript))
}

// ShouldTrigger returns whether the critic loop should run for the
// given incident severity. The ROADMAP D.2 gate is "severity >=
// critical" — we hard-code the comparison here so callers don't drift
// from the documented contract.
//
// Severities lower than critical skip the loop entirely (saves the
// 2x token cost noted in the ROADMAP entry).
func ShouldTrigger(severity string) bool {
	s := strings.ToLower(strings.TrimSpace(severity))
	return s == "critical" || s == "p0" || s == "p1"
}

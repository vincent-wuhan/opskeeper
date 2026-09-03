package critic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DefaultMaxRounds is the ROADMAP D.2 spec value. The loop gives up
// after this many corrective rounds to bound token cost.
const DefaultMaxRounds = 2

// DefaultRoundTimeout caps each critic round-trip. The persona
// advertises max_turns=10, but the LLM may take 30-60s on Opus 4.7
// for a complex audit. We give it 90s and surface a timeout to the
// caller.
const DefaultRoundTimeout = 90 * time.Second

// Loop runs the critic-and-correct cycle. It is the public entry
// point the coordinator calls after a primary ReAct completes for a
// severity >= critical incident.
type Loop struct {
	Spawner      Spawner
	MaxRounds    int
	RoundTimeout time.Duration
}

// New returns a Loop with sensible defaults. Callers can mutate
// fields after construction (e.g. tests inject a short timeout).
func New(spawner Spawner) *Loop {
	return &Loop{
		Spawner:      spawner,
		MaxRounds:    DefaultMaxRounds,
		RoundTimeout: DefaultRoundTimeout,
	}
}

// Result is the loop's output. The coordinator uses this to decide
// whether to deliver the report as-is, surface the issues, or escalate
// to a human.
type Result struct {
	FinalReport *PrimaryReport `json:"final_report"`
	Audit       *CriticAudit   `json:"audit"`
	Rounds      int            `json:"rounds"`
	Corrected   bool           `json:"corrected"`
	GaveUp      bool           `json:"gave_up"`
	AbortReason string         `json:"abort_reason,omitempty"`
}

// Corrector is the seam that asks the primary agent to fix the issues
// raised by the critic. The default is no-op (the loop just audits;
// the coordinator re-spawns the primary upstream). Tests wire a fake.
//
// Returning nil signals "no corrector wired" — the loop continues
// auditing the same report (useful for tests and for the common
// case where the coordinator handles correction upstream).
type Corrector func(ctx context.Context, report *PrimaryReport, audit *CriticAudit) (*PrimaryReport, error)

// CorrectFunc optionally lets the caller wire a correction hook. nil
// = the loop only audits, never corrects. The coordinator's wiring
// sets this so the loop becomes a closed audit-and-correct cycle.
var CorrectFunc Corrector

// Run executes the critic-and-correct loop.
//
// Behavior:
//   - If !ShouldTrigger(report.Severity) → return immediately, no audit.
//   - For round in 1..MaxRounds: spawn critic, parse audit, record.
//   - After each round, if NeedsCorrection=false AND no critical
//     issue → stop (report is acceptable).
//   - Otherwise, if a corrector is wired, ask for a correction and
//     audit the corrected report on the next round.
//   - After MaxRounds with persistent issues → Result.GaveUp=true.
func (l *Loop) Run(ctx context.Context, report *PrimaryReport) (*Result, error) {
	if report == nil {
		return nil, fmt.Errorf("critic: nil report")
	}
	if l.MaxRounds <= 0 {
		l.MaxRounds = DefaultMaxRounds
	}
	if l.RoundTimeout <= 0 {
		l.RoundTimeout = DefaultRoundTimeout
	}

	res := &Result{
		FinalReport: report,
		Rounds:      0,
	}

	// Severity gate. Lower than critical skips the loop — saves 2x
	// tokens per ROADMAP D.2 spec.
	if !ShouldTrigger(report.Severity) {
		res.Audit = &CriticAudit{Summary: "critic skipped: severity below threshold"}
		return res, nil
	}

	current := report
	for round := 1; round <= l.MaxRounds; round++ {
		res.Rounds = round

		audit, err := l.runOneRound(ctx, current, round)
		if err != nil {
			res.AbortReason = err.Error()
			res.Audit = &CriticAudit{Summary: fmt.Sprintf("critic aborted: %v", err)}
			return res, nil
		}
		res.Audit = audit

		// Done? Two reasons to stop early:
		//   1. NeedsCorrection=false → primary report is acceptable
		//   2. Critical issue found AND we have no corrector wired →
		//      surface to human, don't keep auditing the same report
		auditOK := !audit.NeedsCorrection && !audit.HasCriticalIssue()
		if auditOK {
			return res, nil
		}

		// Last round — stop regardless. We don't correct after the
		// last audit (there'd be no point auditing the correction).
		if round == l.MaxRounds {
			res.GaveUp = true
			return res, nil
		}

		// Ask the corrector to fix the issues. If no corrector is
		// wired (CorrectFunc==nil), requestCorrection returns nil;
		// we re-audit the same report on the next round (useful for
		// tests and for the common case where the coordinator handles
		// correction upstream and only wants the audit).
		corrected, err := requestCorrection(ctx, current, audit)
		if err != nil {
			res.AbortReason = fmt.Sprintf("correction failed: %v", err)
			return res, nil
		}
		if corrected != nil {
			res.Corrected = true
			res.FinalReport = corrected
			current = corrected
		}
	}

	return res, nil
}

func requestCorrection(ctx context.Context, report *PrimaryReport, audit *CriticAudit) (*PrimaryReport, error) {
	if CorrectFunc == nil {
		return nil, nil
	}
	return CorrectFunc(ctx, report, audit)
}

// runOneRound spawns the critic worker, waits for the result, parses
// the audit JSON out of the transcript, and returns the parsed audit.
// Errors are returned only on infrastructure problems (spawn failed,
// timeout, malformed JSON). A clean "no issues" audit is a non-error.
func (l *Loop) runOneRound(ctx context.Context, report *PrimaryReport, round int) (*CriticAudit, error) {
	prompt := buildCriticPrompt(report, round)

	workerID, err := l.Spawner.SpawnWorker(ctx, SpawnRequest{
		AgentName:  "critic",
		Prompt:     prompt,
		Background: false,
	})
	if err != nil {
		return nil, fmt.Errorf("spawn critic: %w", err)
	}

	transcript, err := l.Spawner.GetWorkerResult(ctx, workerID, l.RoundTimeout)
	if err != nil {
		return nil, fmt.Errorf("get critic result: %w", err)
	}

	audit, err := ParseAudit(transcript)
	if err != nil {
		return nil, fmt.Errorf("parse audit: %w", err)
	}
	return audit, nil
}

// buildCriticPrompt is the prompt the coordinator sends to the critic
// worker. It includes the primary report as JSON plus a brief
// instruction block referencing the persona (agents/critic.md).
//
// Round number is included so the critic knows whether this is a
// re-audit (after a correction) — on round 2+ the critic should focus
// on issues raised in round 1 and check whether the corrected report
// resolves them.
func buildCriticPrompt(report *PrimaryReport, round int) string {
	payload, _ := json.MarshalIndent(report, "", "  ")
	var b strings.Builder
	b.WriteString("You are running critic round ")
	b.WriteString(fmt.Sprintf("%d", round))
	b.WriteString(". Audit the following primary ReAct report and respond with the JSON verdict shape from your persona instructions.\n\n")
	if round > 1 {
		b.WriteString("This is a re-audit. Focus on whether the issues from round 1 are resolved; do not re-raise previously rejected suggestions.\n\n")
	}
	b.WriteString("```json\n")
	b.Write(payload)
	b.WriteString("\n```\n")
	return b.String()
}

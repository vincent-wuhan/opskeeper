package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// recordingCompleter stands in for a provider. It keeps what it was asked
// so a test can assert the judge was shown the run's own conclusions rather
// than a constant — the failure this file exists to rule out.
type recordingCompleter struct {
	content string
	err     error
	seen    string
	calls   int
}

func (c *recordingCompleter) Complete(_ context.Context, req pigmodel.Request) (*pigai.AssistantMessage, error) {
	c.calls++
	// Every message, not just the user turn: a judge that was handed the
	// run's evidence as tool-result blocks and ignored them would still
	// leave a plausible-looking transcript here.
	for _, m := range req.Messages {
		c.seen += pigmodel.MessageText(m) + "\n"
	}
	if c.err != nil {
		return nil, c.err
	}
	reply := pigmodel.AssistantTurn(c.content)
	reply.StopReason = pigai.StopReasonStop
	return &reply, nil
}

var _ pigmodel.Completer = (*recordingCompleter)(nil)

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// TestRunLoop_RealAgentTeams_ScoresThePostmortemNotTheTerminalPhase is the
// test for the claim that this mode is evaluated. Before, rca_accuracy was
// synthesized from whether the run reached "postmortem" — a statement about
// the state machine, not about the agent.
func TestRunLoop_RealAgentTeams_ScoresThePostmortemNotTheTerminalPhase(t *testing.T) {
	ctx := context.Background()
	result, err := RunLoop(ctx, testRealRunnerOptions(t, validRealAgentTeamsBundle(t)), LoopDeps{})
	if err != nil {
		t.Fatalf("RunLoop real-agentteams: %v", err)
	}
	if len(result.JudgeScores.JudgesUsed) == 0 {
		t.Fatal("no judge ran: the rubric is synthesized from the terminal phase again")
	}
	if result.Rubric.RCAAccuracy == nil {
		t.Fatal("rca_accuracy is null even though a judge ran")
	}
	if strings.Contains(result.Rubric.RCAAccuracyReason, "synthesized") {
		t.Errorf("rca_accuracy is still synthesized: %q", result.Rubric.RCAAccuracyReason)
	}
	// The heuristic judge compares symbolic ids by exact string, so on a
	// real postmortem's prose its near-zero score is a fact about the
	// matcher. It has to say so, or a reader takes 0.0 as a verdict on the
	// agent.
	if !hasFlag(result.Flags, "judge_heuristic_on_free_text") {
		t.Errorf("flags = %v, want the heuristic-on-prose warning", result.Flags)
	}
}

func TestRunLoop_RealAgentTeams_UsesTheInjectedModelAndSeesTheRealRootCause(t *testing.T) {
	ctx := context.Background()
	c := &recordingCompleter{content: `{"rca_accuracy": 0.75}`}
	result, err := RunLoop(ctx, testRealRunnerOptions(t, validRealAgentTeamsBundle(t)), LoopDeps{LLMClient: c})
	if err != nil {
		t.Fatalf("RunLoop real-agentteams: %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("the model was called %d times, want 1", c.calls)
	}
	// The prompt must carry the postmortem's own sentence. If the response
	// were still the hardcoded constant, this is where it would show.
	if !strings.Contains(c.seen, "case-owned CPU processes saturated the host") {
		t.Errorf("the judge was not shown the run's root cause; prompt was:\n%s", c.seen)
	}
	if len(result.JudgeScores.JudgesUsed) != 1 || result.JudgeScores.JudgesUsed[0] != "llm" {
		t.Errorf("judges_used = %v, want the model judge", result.JudgeScores.JudgesUsed)
	}
	if hasFlag(result.Flags, "judge_heuristic_on_free_text") {
		t.Error("the heuristic warning was raised for a run scored by the model")
	}
	got, ok := result.JudgeScores.Dimensions["rca_accuracy"]
	if !ok || got != 0.75 {
		t.Errorf("rca_accuracy = %v (present=%t), want the model's 0.75", got, ok)
	}
}

// A model that fails must not become a silent clean-looking score. The
// fallback exists, and it is recorded as a fallback.
func TestRunLoop_RealAgentTeams_RecordsTheFallbackWhenTheModelFails(t *testing.T) {
	ctx := context.Background()
	c := &recordingCompleter{err: context.DeadlineExceeded}
	result, err := RunLoop(ctx, testRealRunnerOptions(t, validRealAgentTeamsBundle(t)), LoopDeps{LLMClient: c})
	if err != nil {
		t.Fatalf("RunLoop real-agentteams: %v", err)
	}
	used := strings.Join(result.JudgeScores.JudgesUsed, ",")
	if !strings.Contains(used, "fallback") {
		t.Errorf("judges_used = %q, want the degraded path to be visible in the artefact", used)
	}
}

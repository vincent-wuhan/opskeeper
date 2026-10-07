package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	"github.com/vincent-wuhan/opskeeper/core/harness/runner"
	"github.com/vincent-wuhan/opskeeper/core/harness/vocabulary"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/llm"
)

// clearProviderEnv removes every provider credential for the duration of a
// test, so a developer machine with a real key in its environment cannot
// turn "nothing is configured" into "something is".
func clearProviderEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OPSKEEPER_OPENAI_API_KEY", "")
	t.Setenv("OPSKEEPER_ANTHROPIC_API_KEY", "")
	t.Setenv("OPSKEEPER_ZHIPU_API_KEY", "")
	t.Setenv("OPSKEEPER_GEMINI_API_KEY", "")
	t.Setenv("OPSKEEPER_DEEPSEEK_API_KEY", "")
	t.Setenv("OPSKEEPER_KIMI_API_KEY", "")
	t.Setenv("OPSKEEPER_LLM_DEFAULT_PROVIDER", "")
}

func mustConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestTheHeuristicJudgeNeedsNoCredentials(t *testing.T) {
	clearProviderEnv(t)
	impl, label, err := buildJudge(judgeHeuristic, "", "")
	if err != nil {
		t.Fatalf("the default judge path errored: %v", err)
	}
	if label != judgeHeuristic {
		t.Errorf("label = %q, want %q", label, judgeHeuristic)
	}
	if impl == nil {
		t.Fatal("no judge was built")
	}
}

func TestAnUnreachableLLMJudgeIsAnErrorRatherThanAFallback(t *testing.T) {
	clearProviderEnv(t)
	// The failure this prevents: asking for a model judgement, silently
	// getting a heuristic number, and filing it under the model judge's
	// name. Nothing downstream could tell.
	_, _, err := buildJudge(judgeLLM, "", "")
	if err == nil {
		t.Fatal("the LLM judge was built with no provider configured")
	}
	if !strings.Contains(err.Error(), "no LLM provider is configured") {
		t.Errorf("error = %v, want a statement that nothing is configured", err)
	}
	for _, want := range []string{"OPSKEEPER_ANTHROPIC_API_KEY", "OPSKEEPER_OPENAI_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestANamedButUnconfiguredProviderIsNamed(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPSKEEPER_KIMI_API_KEY", "sk-kimi")
	_, _, err := buildJudge(judgeLLM, "anthropic", "")
	if err == nil {
		t.Fatal("an unconfigured provider was accepted")
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("error = %v, want it to name the provider that was asked for", err)
	}
}

func TestAnUnknownJudgeModeIsRejected(t *testing.T) {
	_, _, err := buildJudge("vibes", "", "")
	if err == nil || !strings.Contains(err.Error(), "--judge must be") {
		t.Fatalf("error = %v, want the flag's own contract restated", err)
	}
}

func TestTheDefaultProviderWinsAndOtherwiseTheFirstConfiguredOneDoes(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPSKEEPER_ANTHROPIC_API_KEY", "sk-anthropic")
	t.Setenv("OPSKEEPER_KIMI_API_KEY", "sk-kimi")

	// No default configured: the first configured id in catalog order.
	// anthropic sorts before kimi, so the choice is deterministic rather
	// than "whichever env var happened to be read first".
	got, err := pickProvider(mustConfig(t), "")
	if err != nil {
		t.Fatalf("pickProvider: %v", err)
	}
	if got != llm.ProviderAnthropic {
		t.Errorf("provider = %q, want %q", got, llm.ProviderAnthropic)
	}

	t.Setenv("OPSKEEPER_LLM_DEFAULT_PROVIDER", "kimi")
	got, err = pickProvider(mustConfig(t), "")
	if err != nil {
		t.Fatalf("pickProvider: %v", err)
	}
	if got != llm.ProviderKimi {
		t.Errorf("provider = %q, want the configured default %q", got, llm.ProviderKimi)
	}

	got, err = pickProvider(mustConfig(t), "anthropic")
	if err != nil {
		t.Fatalf("pickProvider: %v", err)
	}
	if got != llm.ProviderAnthropic {
		t.Errorf("provider = %q, want the explicit request", got)
	}
}

func TestADefaultNamingAnUnconfiguredProviderFallsBackToAConfiguredOne(t *testing.T) {
	// A default left over from a deployment whose credentials were removed
	// must not turn every scoring run into a hard failure. The fallback is
	// to another configured provider, not to the heuristic: the caller
	// asked for a model judge and still gets one.
	clearProviderEnv(t)
	t.Setenv("OPSKEEPER_LLM_DEFAULT_PROVIDER", "openai")
	t.Setenv("OPSKEEPER_KIMI_API_KEY", "sk-kimi")

	got, err := pickProvider(mustConfig(t), "")
	if err != nil {
		t.Fatalf("pickProvider: %v", err)
	}
	if got != llm.ProviderKimi {
		t.Errorf("provider = %q, want the only configured one", got)
	}
}

func TestAnEmptyResponseIsRejectedRatherThanScoredAsATotalFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadAgentResponse(path)
	if err == nil {
		t.Fatal("an empty response was accepted as something to score")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error = %v, want it to say the response is empty", err)
	}
}

func TestAResponseThatCannotBeParsedSaysWhatItExpected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wrong.json")
	if err := os.WriteFile(path, []byte(`{"tool_calls":"not a list"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadAgentResponse(path)
	if err == nil || !strings.Contains(err.Error(), "judge AgentResponse") {
		t.Fatalf("error = %v, want it to name the expected shape", err)
	}
}

func TestAResponseHashIsFilledInWhenTheProducerOmittedIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resp.json")
	raw, _ := json.Marshal(map[string]any{
		"tool_calls":         []map[string]string{{"name": "query_promql"}},
		"root_cause_matched": []string{"connection pool exhausted"},
	})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadAgentResponse(path)
	if err != nil {
		t.Fatalf("loadAgentResponse: %v", err)
	}
	if got.ResponseHash == "" {
		t.Error("the response hash was left empty; it is the key the judge dedupes on")
	}
}

// TestTheCommandScoresARealCaseAgainstARealResponse is the end-to-end check
// that the command works at all, with no network: the heuristic judge, one
// of the repository's own golden cases, and a response describing what an
// agent did.
func TestTheCommandScoresARealCaseAgainstARealResponse(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	respPath := filepath.Join(dir, "resp.json")
	raw, _ := json.Marshal(map[string]any{
		"tool_calls":         []map[string]any{{"name": "query_promql", "args": map[string]string{"expr": "pg_lock_waits"}}},
		"root_cause_matched": []string{"pg.lock_waits", "pg.active_sessions"},
	})
	if err := os.WriteFile(respPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "score.json")

	// No --allow-unservable here on purpose: pg/lock-waits expects symbols
	// the middleware adapter really does register (pg.lock_waits,
	// pg.active_sessions, pg.kill_session), so this case is servable and a
	// refusal would be the gate being wrong.
	err := cmdJudge(context.Background(), []string{
		"--case", "pg/lock-waits",
		"--response", respPath,
		"--cases-dir", filepath.Join("..", "..", "core", "harness", "cases"),
		"--plugins-dir", filepath.Join("..", "..", "plugins", "pig-ops"),
		"--out", outPath,
	})
	if err != nil {
		t.Fatalf("cmdJudge: %v", err)
	}

	doc, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		CaseID string `json:"case_id"`
		Score  struct {
			Overall    float64            `json:"overall"`
			Dimensions map[string]float64 `json:"dimensions"`
			JudgesUsed []string           `json:"judges_used"`
		} `json:"score"`
		Summary struct {
			RCAAccuracy *float64 `json:"rca_accuracy"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatalf("the result is not the documented shape: %v\n%s", err, doc)
	}
	if got.CaseID != "pg/lock-waits" {
		t.Errorf("case_id = %q, want the case that was asked for", got.CaseID)
	}
	// The judge that ran is recorded in the artifact. A score with no
	// provenance is a number nobody can act on.
	if len(got.Score.JudgesUsed) == 0 {
		t.Error("the score does not say which judge produced it")
	}
	if got.Score.Overall < 0 || got.Score.Overall > 1 {
		t.Errorf("overall = %v, want a value in [0,1]", got.Score.Overall)
	}
	// The response names exactly the two root causes this golden case
	// expects. A score of 0 here would mean the response was not actually
	// read, so this is the assertion that makes the test worth running.
	if got.Summary.RCAAccuracy == nil {
		t.Fatal("the result does not report rca_accuracy at all")
	}
	if *got.Summary.RCAAccuracy != 1 {
		t.Errorf("rca_accuracy = %v, want 1: the response matches both expected root causes exactly",
			*got.Summary.RCAAccuracy)
	}
}

// A misspelled field is a mistake worth surfacing, not a response that
// happens to be missing its root cause. Silent acceptance turned one into a
// confident zero the first time round.
func TestAMisspelledFieldIsRejectedRatherThanScoredAsAnEmptyAnswer(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "typo.json")
	if err := os.WriteFile(path, []byte(`{"root_cause":["pg.lock_waits"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadAgentResponse(path)
	if err == nil {
		t.Fatal(`a response using "root_cause" was accepted; the field is "root_cause_matched"`)
	}
	if !strings.Contains(err.Error(), "root_cause_matched") {
		t.Errorf("error = %v, want it to name the field the reader should have used", err)
	}
}

func TestTheCommandRefusesAMissingCaseRatherThanScoringSomethingElse(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	respPath := filepath.Join(dir, "resp.json")
	if err := os.WriteFile(respPath, []byte(`{"root_cause_matched":["x"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := cmdJudge(context.Background(), []string{
		"--case", "pg/does-not-exist",
		"--response", respPath,
		"--cases-dir", filepath.Join("..", "..", "core", "harness", "cases"),
	})
	if err == nil {
		t.Fatal("an unknown case was scored")
	}
}

func TestTheRunLoopJudgeIsOffByDefault(t *testing.T) {
	clearProviderEnv(t)
	deps, err := buildLoopDeps(judgeHeuristic, "", "", runner.ExecutionModeRealAgentTeams)
	if err != nil {
		t.Fatalf("the default path errored: %v", err)
	}
	if deps.LLMClient != nil {
		t.Error("a model client was injected with no credentials and no request for one")
	}
}

func TestAModelJudgeIsRefusedInAModeThatHasNothingToScore(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPSKEEPER_KIMI_API_KEY", "sk-kimi")
	// dry-run synthesizes its own timeline, so the judge's input would be a
	// constant. Paying a model for a number that is thrown away is worse
	// than refusing.
	for _, mode := range []runner.LoopExecutionMode{runner.ExecutionModeDryRun, runner.ExecutionModeOrchestrator} {
		_, err := buildLoopDeps(judgeLLM, "", "", mode)
		if err == nil {
			t.Fatalf("mode %q accepted --judge=llm", mode)
		}
		if !strings.Contains(err.Error(), "real-agentteams") {
			t.Errorf("mode %q: error = %v, want it to name the mode that does work", mode, err)
		}
	}
}

func TestAModelJudgeInARealModeStillNeedsCredentials(t *testing.T) {
	clearProviderEnv(t)
	_, err := buildLoopDeps(judgeLLM, "", "", runner.ExecutionModeRealAgentTeams)
	if err == nil {
		t.Fatal("a model judge was wired with nothing configured")
	}
}

func TestAModelJudgeInARealModeResolvesItsClient(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPSKEEPER_KIMI_API_KEY", "sk-kimi")
	deps, err := buildLoopDeps(judgeLLM, "", "", runner.ExecutionModeRealAgentTeams)
	if err != nil {
		t.Fatalf("buildLoopDeps: %v", err)
	}
	if deps.LLMClient == nil {
		t.Fatal("no model client was injected")
	}
}

func TestAnUnknownRunLoopJudgeModeIsRejected(t *testing.T) {
	if _, err := buildLoopDeps("vibes", "", "", runner.ExecutionModeRealAgentTeams); err == nil {
		t.Fatal("an unknown judge mode was accepted")
	}
}

// The refusal is the point. A golden case naming a symbol the build cannot
// emit scores zero however well the agent reasons, and a zero in an
// artifact is read as a verdict on the agent.
func TestTheCommandRefusesToScoreACaseTheBuildCannotSatisfy(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	respPath := filepath.Join(dir, "resp.json")
	raw, _ := json.Marshal(map[string]any{
		"tool_calls":           []map[string]string{{"name": "query_promql"}},
		"root_cause_matched":   []string{"pg.lock_waits"},
		"remediations_matched": []string{"zookeeper.restart_quorum"},
	})
	if err := os.WriteFile(respPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// The corpus is synthetic because every case the repository ships is
	// servable now — that is what the plugin fleet bought. This one expects
	// a "zookeeper" family no package declares, and only on the remediation
	// axis: the root cause is pg.lock_waits, which the pg adapter registers
	// by name. A run scoring zero on it would be saying something about the
	// corpus, not about the agent.
	err := cmdJudge(context.Background(), []string{
		"--case", "zookeeper/session-timeout",
		"--response", respPath,
		"--cases-dir", writeUnservableCorpus(t),
		"--plugins-dir", filepath.Join("..", "..", "plugins", "pig-ops"),
	})
	if err == nil {
		t.Fatal("a case this build cannot satisfy was scored anyway")
	}
	// The message has to be actionable: which case, which symbols, and
	// what to do about it.
	for _, want := range []string{"zookeeper/session-timeout", "zookeeper.restart_quorum", "opskeeper-eval vocabulary", "--allow-unservable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// The axis that is fine is not mentioned. "missing root causes:" with
	// nothing after it reads as a claim that the root causes were checked
	// and found acceptable, which is not what happened.
	if strings.Contains(err.Error(), "missing root causes") {
		t.Errorf("error names the root-cause axis although only the remediation is short:\n%v", err)
	}
}

// Opting out is allowed, but the artifact must carry the reason — otherwise
// a stored score is indistinguishable from one earned on a servable case,
// which is the failure the refusal exists to prevent.
func TestOptingOutOfTheRefusalStampsTheReasonIntoTheArtifact(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	respPath := filepath.Join(dir, "resp.json")
	raw, _ := json.Marshal(map[string]any{
		"tool_calls":           []map[string]string{{"name": "query_promql"}},
		"root_cause_matched":   []string{"pg.lock_waits"},
		"remediations_matched": []string{"zookeeper.restart_quorum"},
	})
	if err := os.WriteFile(respPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "score.json")
	if err := cmdJudge(context.Background(), []string{
		"--case", "zookeeper/session-timeout",
		"--response", respPath,
		"--cases-dir", writeUnservableCorpus(t),
		"--allow-unservable",
		"--plugins-dir", filepath.Join("..", "..", "plugins", "pig-ops"),
		"--out", outPath,
	}); err != nil {
		t.Fatalf("cmdJudge: %v", err)
	}
	doc, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		UnservableCase string `json:"unservable_case"`
	}
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatal(err)
	}
	if got.UnservableCase == "" {
		t.Errorf("the score was written with no record of being unservable:\n%s", doc)
	}
}

// The refusal above is only trustworthy if the capability behind it is real.
// An empty or mis-wired provider set would make every case look unservable
// (refusing everything) or servable (refusing nothing), and both failures
// look like a healthy gate from the outside.
func TestTheProductionCapabilityIsReadFromTheRealRegistries(t *testing.T) {
	cap, _, err := productionCapability(filepath.Join("..", "..", "plugins", "pig-ops"))
	if err != nil {
		t.Fatalf("productionCapability: %v", err)
	}
	byName := map[string]vocabulary.Provider{}
	for _, p := range cap.Providers {
		if _, dup := byName[p.Name]; dup {
			t.Errorf("provider %q is listed twice", p.Name)
		}
		byName[p.Name] = p
	}
	for _, want := range []string{"middleware-adapter", "loop-investigator"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("no %q provider; the gate is not consulting %s", want, want)
		}
	}
	adapter, ok := byName["middleware-adapter"]
	if !ok || len(adapter.Symbols) == 0 {
		t.Fatal("the middleware adapter registered no tools; the gate would refuse everything")
	}
	// Read the real registry rather than a constant: pg.kill_session is
	// registered by the pg adapter, and the corpus expects it.
	if _, cov, ok := cap.ProviderOf("pg.kill_session"); !ok || cov != vocabulary.CoverageExact {
		t.Errorf("pg.kill_session is covered by %q (%v); the pg adapter registers it", cov, ok)
	}
	// A symbol in a family no provider declares must still be a miss, or
	// the gate is accepting everything.
	if _, cov, ok := cap.ProviderOf("zookeeper.session_timeout"); ok {
		t.Errorf("an unknown symbol was covered by %q", cov)
	}
	// Inside a family some package does declare, the join is by family, not
	// by method: a package declares the capabilities it serves, not every
	// method of them (see pluginmanifest.CoverageOf, which documents the
	// choice). Pinned down here so that a move to per-method claims is a
	// deliberate change rather than a silent one — and so the limit is
	// written down rather than discovered: the middleware package cannot
	// dispatch pg.definitely_not_a_tool, and the boolean alone does not say
	// so. The dispatch axis that does say so is loopActionExecutability,
	// which counts exact symbols only.
	if _, cov, ok := cap.ProviderOf("pg.definitely_not_a_tool"); !ok || cov != vocabulary.CoverageFamily {
		t.Errorf("a method inside a declared family resolved as (%q, %v); the join is family-level by design", cov, ok)
	}
	// The loop's own remediation vocabulary is a separate provider: the
	// closed loop proposes pg.kill_backend where the adapter offers
	// pg.kill_session, and conflating the two would hide that.
	loopP, ok := byName["loop-investigator"]
	if !ok || len(loopP.Symbols) == 0 {
		t.Error("the loop investigator contributes no symbols")
	}
}

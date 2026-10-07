// The judge subcommand: score one agent response against one golden case.
//
// This command exists because the LLM judge was reachable from nowhere.
// judge.LLMJudge had tests, LoopDeps had an LLMClient field, and the
// runner would have used it — but cmd/opskeeper-eval passed an empty
// LoopDeps, and the only execution mode the CLI can reach (dry-run)
// deliberately skips the judge because there is no real agent response to
// score. So the judge had never run outside its own tests.
//
// The input is a judge.AgentResponse document: the harness's own type, not
// a new schema invented here. Whoever produces an agent's tool calls, root
// cause and remediation writes that JSON, and this command scores it. That
// keeps the judge's input contract the one the judge already validates,
// instead of a parallel format that drifts from it.
//
// The default judge is the heuristic one. A judge that silently started
// billing a provider because a flag flipped is a bad default for a tool
// whose whole job is to be re-run in a loop, and the substitution would be
// invisible in the output — the score would simply be a different number.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/harness/axes"
	"github.com/vincent-wuhan/opskeeper/core/harness/judge"
	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
	"github.com/vincent-wuhan/opskeeper/core/harness/vocabulary"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/llm"
)

// judgeHeuristic and judgeLLM are the two scoring paths.
const (
	judgeHeuristic = "heuristic"
	judgeLLM       = "llm"
)

func cmdJudge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("judge", flag.ExitOnError)
	caseID := fs.String("case", "", "case ID（必填），对应 core/harness/cases 下的黄金事故")
	responsePath := fs.String("response", "", "AgentResponse JSON 路径（必填）")
	casesDir := fs.String("cases-dir", "core/harness/cases", "cases 目录")
	mode := fs.String("judge", judgeHeuristic, "评分器：heuristic（不联网）/ llm（真实模型评分）")
	provider := fs.String("provider", "", "LLM provider（llm 模式；默认取部署的默认 provider）")
	model := fs.String("model", "", "模型（llm 模式；默认取该 provider 的默认模型）")
	out := fs.String("out", "", "评分结果 JSON 输出路径（默认打印到 stdout）")
	allowUnservable := fs.Bool("allow-unservable", false,
		"给本构建结构上无法满足的 case 打分（结果会标注 unservable_case）")
	pluginsDir := fs.String("plugins-dir", defaultPluginsDir, "插件包目录（能力覆盖检查用）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *caseID == "" {
		return errors.New("--case required")
	}
	if *responsePath == "" {
		return errors.New("--response required")
	}

	impl, label, err := buildJudge(*mode, *provider, *model)
	if err != nil {
		return err
	}

	caseObj, err := schema.NewLoader(*casesDir).LoadByID(*caseID)
	if err != nil {
		return fmt.Errorf("load case %q: %w", *caseID, err)
	}
	resp, err := loadAgentResponse(*responsePath)
	if err != nil {
		return err
	}

	// A case this build cannot satisfy produces a score that is a statement
	// about the corpus, not about the agent. No shipped case is in that
	// state today — the plugin fleet serves all 20 — but the check is not
	// vacuous: a case added tomorrow that names a family nobody packaged
	// would otherwise score a zero that reads as "the agent reasoned
	// badly", a false statement about a question the agent was never asked.
	// Emitting a number anyway is what this refuses.
	cap, _, err := productionCapability(*pluginsDir)
	if err != nil {
		return fmt.Errorf("read the production vocabulary: %w", err)
	}
	gap := vocabulary.Check(caseObj, cap)
	if !gap.Servable() && !*allowUnservable {
		var b strings.Builder
		fmt.Fprintf(&b, "refusing to score %s: this build cannot produce what the case asks for.", caseObj.ID)
		// Only the axes that are actually short are printed. A blank
		// "missing root causes:" line reads as a claim that the root
		// causes were checked and found fine, which is not what happened —
		// nothing was wrong with them, so they are not mentioned at all.
		if len(gap.UnservableRootCauses) > 0 {
			fmt.Fprintf(&b, "\n  missing root causes: %s", strings.Join(gap.UnservableRootCauses, " "))
		}
		if len(gap.UnservableRemediations) > 0 {
			fmt.Fprintf(&b, "\n  missing remediations: %s", strings.Join(gap.UnservableRemediations, " "))
		}
		b.WriteString("\n  run `opskeeper-eval vocabulary` for the full report, or pass\n" +
			"  --allow-unservable to score it anyway (the artifact will say so)")
		return errors.New(b.String())
	}

	score, err := impl.Score(ctx, judgeCaseOf(caseObj), resp)
	if err != nil {
		return fmt.Errorf("judge: %w", err)
	}
	if err := score.IsValid(); err != nil {
		// A score outside [0,1] is not a low score, it is a broken judge.
		// Reporting it as a number would let it into a leaderboard.
		return fmt.Errorf("judge returned an unusable score: %w", err)
	}
	// The judge records which path actually produced the number, including
	// the case where an LLM call failed and the heuristic took over. That
	// field is the only way a reader of a stored score can tell an LLM
	// judgement from a fallback, so it is printed and stored, not assumed.
	score.JudgesUsed = append([]string(nil), score.JudgesUsed...)
	if len(score.JudgesUsed) == 0 {
		score.JudgesUsed = []string{label}
	}

	doc, err := json.MarshalIndent(struct {
		CaseID  string       `json:"case_id"`
		Model   string       `json:"model,omitempty"`
		Score   *judge.Score `json:"score"`
		Summary scoreSummary `json:"summary"`
		// UnservableCase travels with the number. Without it a stored
		// score cannot be told apart from one earned on a case the
		// system could actually express, and that is the whole failure
		// mode this check exists to prevent.
		UnservableCase string `json:"unservable_case,omitempty"`
	}{CaseID: *caseID, Model: *model, Score: score, Summary: summarize(score), UnservableCase: gap.Reason()}, "", "  ")
	if err != nil {
		return err
	}
	if *out == "" {
		fmt.Println(string(doc))
		return nil
	}
	if err := os.WriteFile(*out, append(doc, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Printf("judge: case=%s judges_used=%s overall=%.3f → %s\n",
		*caseID, strings.Join(score.JudgesUsed, ","), score.Overall, *out)
	return nil
}

type scoreSummary struct {
	RCAAccuracy *float64 `json:"rca_accuracy,omitempty"`
	// 三个诊断轴进摘要，是因为它们回答的问题和 rca_accuracy 不同：一个
	// outcome 高而 trace 不落地的 run，在只看 rca 的摘要里和真的诊断长得
	// 一模一样。缺省（omitempty）表示这个 case 没声明该轴，不是 0 分。
	Localization   *float64 `json:"localization,omitempty"`
	Identification *float64 `json:"identification,omitempty"`
	Reason         *float64 `json:"reason,omitempty"`
	Flagged        bool     `json:"flagged"`
	FlagReason     string   `json:"flag_reason,omitempty"`
}

func summarize(s *judge.Score) scoreSummary {
	out := scoreSummary{Flagged: s.Flagged, FlagReason: s.FlagReason}
	for key, target := range map[string]**float64{
		"rca_accuracy":                &out.RCAAccuracy,
		judge.DimensionLocalization:   &out.Localization,
		judge.DimensionIdentification: &out.Identification,
		judge.DimensionReason:         &out.Reason,
	} {
		if v, ok := s.Dimensions[key]; ok {
			value := v
			*target = &value
		}
	}
	return out
}

// buildJudge resolves the scoring path.
//
// An LLM judge that cannot reach a provider is an error, not a fallback.
// Falling back here would produce a heuristic score labelled as if it came
// from the requested judge, and the person reading the leaderboard would
// have no way to tell. The per-call fallback inside LLMJudge is a
// different thing and is recorded: that is a model that answered badly,
// which is a fact about one score rather than a mislabelled run.
func buildJudge(mode, provider, model string) (judge.Judge, string, error) {
	switch mode {
	case judgeHeuristic:
		return judge.NewHeuristicJudge(), judgeHeuristic, nil
	case judgeLLM:
		c, err := resolveCompleter(provider, model)
		if err != nil {
			return nil, "", err
		}
		return judge.NewLLMJudge(c, judge.NewHeuristicJudge(), nil), judgeLLM, nil
	default:
		return nil, "", fmt.Errorf("--judge must be %q or %q, got %q", judgeHeuristic, judgeLLM, mode)
	}
}

// resolveCompleter builds the model client the judge will call through.
//
// The provider catalog comes from llm.ProviderConfigFor rather than from a
// table written here, so the eval tool cannot end up pointed at a
// different endpoint or default model than the server under evaluation.
func resolveCompleter(provider, model string) (pigmodel.Completer, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	id, err := pickProvider(cfg, provider)
	if err != nil {
		return nil, err
	}
	p, ok := llm.ProviderConfigFor(id, cfg)
	if !ok {
		return nil, fmt.Errorf("provider %q is not configured", id)
	}
	if model != "" {
		p.Model = model
	}
	// Exactly one provider is registered, and it is the default. The eval
	// tool has no settings table to read, so this is the static source
	// rather than the catalog adapter the server uses — but the resolution
	// above it is PiG's, and so is the wire call, so a score produced here
	// is comparable with one the server's own judge would produce.
	return pigmodel.NewRegistry(pigmodel.StaticSettings(
		[]pigmodel.ProviderConfig{{
			ID:           domain.ProviderID(p.ID),
			APIKey:       p.APIKey,
			BaseURL:      p.BaseURL,
			Models:       append([]string(nil), p.Models...),
			DefaultModel: p.Model,
		}},
		domain.ProviderID(p.ID),
	)), nil
}

// pickProvider resolves which provider to score with.
func pickProvider(cfg *config.Config, want string) (string, error) {
	if want != "" {
		return want, nil
	}
	if def := strings.TrimSpace(cfg.LLM.Default); def != "" {
		if _, ok := llm.ProviderConfigFor(def, cfg); ok {
			return def, nil
		}
	}
	for _, id := range llm.ProviderIDs() {
		if _, ok := llm.ProviderConfigFor(id, cfg); ok {
			return id, nil
		}
	}
	return "", errors.New("no LLM provider is configured: set one of " +
		strings.Join(envKeyNames(), ", ") +
		", or pass --provider explicitly")
}

func envKeyNames() []string {
	out := make([]string, 0, 6)
	for _, id := range llm.ProviderIDs() {
		out = append(out, "OPSKEEPER_"+strings.ToUpper(id)+"_API_KEY")
	}
	sort.Strings(out)
	return out
}

func loadAgentResponse(path string) (*judge.AgentResponse, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp judge.AgentResponse
	// Unknown fields are rejected rather than ignored. This document is
	// written by whatever produced the agent's run, and the judge scores it
	// against a fixed vocabulary — a producer that spells a field
	// differently has a document the judge cannot read. Accepting it would
	// decode the field as absent, and "absent root cause" is a score of
	// zero, so a rename on the producing side would quietly turn every
	// correct answer into a wrong one. A loud parse error is the only
	// reading that cannot be mistaken for a verdict.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		// The stock decoder error names the field it did not recognise but
		// not the spelling it wanted, and the two are one character apart in
		// the most common case. Saying so here costs a line and saves the
		// reader a trip to the source.
		return nil, fmt.Errorf("parse %s as a judge AgentResponse: %w "+
			"(the root cause and remediation lists are spelled %q and %q, not %q and %q)",
			path, err, "root_cause_matched", "remediations_matched", "root_cause", "remediations")
	}
	// An empty response is not a bad response, it is a missing one. The
	// heuristic judge would score it as a total failure and the LLM judge
	// would score it as a total failure, both of which read like a verdict
	// on an agent that never ran.
	if len(resp.ToolCalls) == 0 && len(resp.RootCause) == 0 && len(resp.Remediations) == 0 {
		return nil, fmt.Errorf("%s has no tool calls, no root cause and no remediations: "+
			"that is an empty response, not a scored one", path)
	}
	if resp.ResponseHash == "" {
		resp.ResponseHash = judge.ComputeResponseHash(&resp)
	}
	return &resp, nil
}

func judgeCaseOf(c *schema.Case) *judge.Case {
	// The three diagnostic axes travel with the judge case rather than being
	// computed here, because both the heuristic and the LLM judge have to
	// score them and a number only one path produces is a number a
	// leaderboard averages over half its rows. The derivation itself is in
	// core/harness/axes, which is the single place "where the fault is" is
	// defined — and it lives in a library rather than in this command so the
	// loop harness can reach it too (it did not, for as long as the
	// derivation sat here, and scored every case without the three axes).
	expectations := axes.Of(c)
	return &judge.Case{
		ID:                   c.ID,
		ExpectedRootCause:    c.Expect.RootCauseLines,
		ExpectedRemediations: c.Expect.RemediationOptions,
		ExpectedDetectSec:    c.Expect.TimeToDetect,
		ExpectedRemediateSec: c.Expect.TimeToRemediate,
		RCAThreshold:         c.Rubric.RCAAccuracy,
		NoCollateralDamage:   c.Rubric.NoCollateralDamage,
		ExpectedLocus:        expectations.Locus,
		ExpectedFaultType:    expectations.FaultType,
	}
}

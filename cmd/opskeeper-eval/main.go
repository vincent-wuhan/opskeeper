// Command opskeeper-eval 是 Harness 评测平台的 CLI 入口（路径 A 阶段 1 任务 1.5）。
//
// 子命令：
//
//	run        执行单个 case 或 suite
//	inject     手动触发 fault-injector（仅 staging；prod 需 --confirm-prod）
//	judge      对已有 incident 报告做 LLM 评分（重跑 judge）
//	leaderboard 显示排行榜 + 回归基线
//	list-cases 列出所有 golden case
//
// 设计依据：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.2.3
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/harness-eval-platform/spec.md
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	harnessleaderboard "github.com/vincent-wuhan/opskeeper/internal/harness/leaderboard"
	"github.com/vincent-wuhan/opskeeper/internal/harness/runner"
)

// version 由 build 阶段注入
const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	// 顶层 --version / --help
	if sub == "--version" || sub == "-v" {
		fmt.Printf("opskeeper-eval %s\n", version)
		return
	}
	if sub == "--help" || sub == "-h" {
		printUsage()
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var err error
	switch sub {
	case "run":
		err = cmdRun(ctx, args)
	case "inject":
		err = cmdInject(ctx, args)
	case "judge":
		err = cmdJudge(ctx, args)
	case "leaderboard":
		err = cmdLeaderboard(ctx, args)
	case "run-loop":
		err = cmdRunLoop(ctx, args)
	case "list-cases":
		err = cmdListCases(ctx, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`opskeeper-eval — Harness 评测平台 CLI

USAGE:
  opskeeper-eval <subcommand> [flags]

SUBCOMMANDS:
  run          执行单个 case 或 suite
  inject       手动触发 fault-injector（仅 staging）
  judge        对已有 incident 报告重跑 judge
  leaderboard  显示排行榜 + 回归基线
  run-loop     harness loop-mode 跑闭环（Day 6+）
               run-loop --execution-mode=real-agentteams 需要 --incident-id/--trace-id 与六类证据文件
  list-cases   列出所有 golden case

FLAGS:
  --version    输出版本
  --help       显示帮助

Use "opskeeper-eval <subcommand> --help" for subcommand-specific flags.

EXAMPLE:
  opskeeper-eval run --case pg/long-running-tx --env staging
  opskeeper-eval run --suite middleware-baseline --concurrency 4
  opskeeper-eval inject --case k8s/pod-oom --target ns=test deploy=order-svc
  opskeeper-eval judge --incident report-20260713-001.json --model claude-sonnet-4
  opskeeper-eval leaderboard --show --period 30d
  opskeeper-eval list-cases --filter pg

`)
}

// runFlags 定义 `run` 子命令的参数。
type runFlags struct {
	caseID      string
	suiteName   string
	env         string
	judgeModel  string
	concurrency int
	output      string
	reportDir   string
}

// cmdRun 执行单个 case 或 suite。
func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	f := &runFlags{}
	fs.StringVar(&f.caseID, "case", "", "case ID（如 pg/long-running-tx）")
	fs.StringVar(&f.suiteName, "suite", "", "suite 名（如 middleware-baseline）")
	fs.StringVar(&f.env, "env", "staging", "目标环境")
	fs.StringVar(&f.judgeModel, "judge-model", "claude-sonnet-4,gpt-4o", "judge 模型（逗号分隔）")
	fs.IntVar(&f.concurrency, "concurrency", 1, "并发数")
	fs.StringVar(&f.output, "output", "", "输出报告路径（JSON）")
	fs.StringVar(&f.reportDir, "report-dir", "", "报告目录（suite 模式）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.caseID == "" && f.suiteName == "" {
		return fmt.Errorf("either --case or --suite required")
	}
	// 骨架实现：参数校验 + 报告最小结构
	report := map[string]any{
		"subcommand":  "run",
		"case_id":     f.caseID,
		"suite_name":  f.suiteName,
		"env":         f.env,
		"judge_model": f.judgeModel,
		"concurrency": f.concurrency,
		"version":     version,
	}
	if f.output != "" {
		data, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(f.output, data, 0o644); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
		fmt.Printf("report written: %s\n", f.output)
	} else {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
	}
	return nil
}

// cmdInject 手动触发 fault-injector（仅 staging）。
func cmdInject(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inject", flag.ExitOnError)
	caseID := fs.String("case", "", "case ID")
	confirmProd := fs.Bool("confirm-prod", false, "确认在 prod 环境注入（需 double approval）")
	fs.String("target", "", "target spec（key=value 列表）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *caseID == "" {
		return fmt.Errorf("--case required")
	}
	fmt.Printf("inject: case=%s confirm_prod=%t\n", *caseID, *confirmProd)
	fmt.Println("(skeleton) — full implementation in Task 2.6")
	return nil
}

// cmdJudge 重跑 judge。
func cmdJudge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("judge", flag.ExitOnError)
	incidentPath := fs.String("incident", "", "incident 报告路径")
	model := fs.String("model", "claude-sonnet-4", "judge 模型")
	rubricPath := fs.String("rubric", "", "rubric JSON 路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *incidentPath == "" {
		return fmt.Errorf("--incident required")
	}
	fmt.Printf("judge: incident=%s model=%s rubric=%s\n", *incidentPath, *model, *rubricPath)
	fmt.Println("(skeleton) — full implementation in Task 2.7")
	return nil
}

// cmdLeaderboard — Day 6+ harness loop leaderboard.
//
// Aggregates harness/result/loop/*.json → harness/result/leaderboard-<date>.md。
// Renders 4-metric rubric + chat-mode 6-metric extension when present.
// Cases with recovery_pass_rate < --threshold (default 0.5) are flagged
// NOT QUALIFIED per spec loop-harness-rubric §"门槛不通过不入榜".
func cmdLeaderboard(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leaderboard", flag.ExitOnError)
	dir := fs.String("dir", "harness/result/loop", "LoopResult JSON 目录")
	outDir := fs.String("out-dir", "harness/result", "Markdown 报告输出目录")
	threshold := fs.Float64("threshold", 0.5, "recovery_pass_rate 门槛（低于则 NOT QUALIFIED）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := harnessleaderboard.NewLoopBoard(*dir)
	if err != nil {
		return fmt.Errorf("leaderboard: %w", err)
	}
	b.RecoveryPassRateThreshold = *threshold
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return fmt.Errorf("leaderboard: mkdir %s: %w", *outDir, err)
	}
	out := filepath.Join(*outDir, fmt.Sprintf("leaderboard-%s.md", time.Now().UTC().Format("2006-01-02")))
	if err := b.RenderToFile(out); err != nil {
		return fmt.Errorf("leaderboard: write %s: %w", out, err)
	}
	fmt.Printf("leaderboard written: %s\n", out)
	fmt.Printf("  cases: %d, qualified: %d, not_qualified: %d\n",
		len(b.Entries),
		countQualified(b.Entries),
		len(b.Entries)-countQualified(b.Entries))
	return nil
}

func countQualified(es []*harnessleaderboard.LoopBoardEntry) int {
	n := 0
	for _, e := range es {
		if e.Qualified {
			n++
		}
	}
	return n
}

// Subcommand: `opskeeper-eval run-loop --case=pg/long-running-tx --mode=loop`
// (or --mode=chat) drives the seven-phase orchestrator end-to-end via
// the harness runner. Output: harness/result/loop/<file-safe-case>.json.
//
// The default remains the legacy dry-run smoke mode. Use
// --execution-mode=real-agentteams for the fail-closed post-hoc evidence
// gate: it requires one incident/trace ID and the complete real evidence set,
// and never falls back to the synthetic timeline.
func cmdRunLoop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run-loop", flag.ExitOnError)
	caseID := fs.String("case", "", "case ID（必填）")
	modeStr := fs.String("mode", "loop", "运行模式：loop / chat / tool")
	executionModeStr := fs.String("execution-mode", "dry-run", "执行模式：dry-run / orchestrator / real-agentteams")
	envStr := fs.String("env", "staging", "目标环境（staging / test / prod）")
	tenantID := fs.String("tenant", "harness-default", "tenant ID")
	incidentID := fs.String("incident-id", "", "real-agentteams 模式必填，必须贯穿全部证据")
	traceID := fs.String("trace-id", "", "real-agentteams 模式必填，32 位小写 hex trace ID")
	casesDir := fs.String("cases-dir", "internal/harness/cases", "cases 目录")
	outDir := fs.String("out-dir", "harness/result/loop", "LoopResult JSON 输出目录")
	stateEvidence := fs.String("state-evidence", "", "real-agentteams state.json 路径")
	hitlEvidence := fs.String("hitl-evidence", "", "real-agentteams Matrix HITL/proposal evidence 路径")
	mcpEvidence := fs.String("mcp-evidence", "", "real-agentteams MCP role-call/audit evidence 路径")
	fixtureBeforeEvidence := fs.String("fixture-before-evidence", "", "real-agentteams fixture before evidence 路径")
	fixtureAfterEvidence := fs.String("fixture-after-evidence", "", "real-agentteams fixture after evidence 路径")
	postmortemEvidence := fs.String("postmortem-evidence", "", "real-agentteams postmortem evidence 路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *caseID == "" {
		return fmt.Errorf("--case required")
	}
	mode, err := runner.ParseMode(*modeStr)
	if err != nil {
		return err
	}
	executionMode, err := runner.ParseLoopExecutionMode(*executionModeStr)
	if err != nil {
		return err
	}
	res, err := runner.RunLoop(ctx, runner.LoopOptions{
		CaseID:        *caseID,
		Mode:          mode,
		ExecutionMode: executionMode,
		Env:           runner.Env(*envStr),
		TenantID:      *tenantID,
		IncidentID:    *incidentID,
		TraceID:       *traceID,
		CasesDir:      *casesDir,
		OutDir:        *outDir,
		RealAgentTeamsEvidence: runner.RealAgentTeamsEvidencePaths{
			State:         *stateEvidence,
			HITL:          *hitlEvidence,
			MCP:           *mcpEvidence,
			FixtureBefore: *fixtureBeforeEvidence,
			FixtureAfter:  *fixtureAfterEvidence,
			Postmortem:    *postmortemEvidence,
		},
	}, runner.LoopDeps{})
	if err != nil {
		return fmt.Errorf("run-loop: %w", err)
	}
	fmt.Printf("LoopResult: case=%s mode=%s execution_mode=%s passed=%t final=%s duration=%dms\n",
		res.CaseID, res.Mode, res.ExecutionMode, res.Passed, res.FinalPhase, res.DurationMs)
	fmt.Printf("  rca_accuracy: %v\n", fmtFloatPtr(res.Rubric.RCAAccuracy))
	fmt.Printf("  time_to_remediate: %s\n", res.Rubric.TimeToRemediate)
	fmt.Printf("  approval_rate: %v\n", fmtFloatPtr(res.Rubric.ApprovalRate))
	fmt.Printf("  recovery_pass_rate: %v\n", fmtFloatPtr(res.Rubric.RecoveryPassRate))
	return nil
}

func fmtFloatPtr(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%.3f", *p)
}

// cmdListCases 列出所有 golden case。
func cmdListCases(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list-cases", flag.ExitOnError)
	casesDir := fs.String("cases-dir", "internal/harness/cases", "cases 目录")
	filter := fs.String("filter", "", "过滤关键字（如 pg/redis/k8s）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cases, err := scanCases(*casesDir, *filter)
	if err != nil {
		return fmt.Errorf("scan cases: %w", err)
	}
	if len(cases) == 0 {
		fmt.Println("(no cases found)")
		return nil
	}
	fmt.Printf("Found %d case(s):\n", len(cases))
	for _, c := range cases {
		fmt.Printf("  %s\n", c)
	}
	return nil
}

// scanCases 扫描 cases 目录下的所有 case.yaml 文件。
func scanCases(dir, filter string) ([]string, error) {
	var cases []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Base(path) != "case.yaml" {
			return nil
		}
		// path 形如 internal/harness/cases/pg/long-running-tx/case.yaml
		// 提取 pg/long-running-tx
		rel, _ := filepath.Rel(dir, filepath.Dir(path))
		if filter != "" && !contains(rel, filter) {
			return nil
		}
		cases = append(cases, rel)
		return nil
	})
	return cases, err
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(sub) > 0 && (s[:len(sub)] == sub || contains(s[1:], sub))))
}

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
	"sort"
	"strings"
	"syscall"
	"time"

	harnessleaderboard "github.com/vincent-wuhan/opskeeper/core/harness/leaderboard"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
	hostinjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/host"
	k8sinjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/k8s"
	kafkainjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/mq/kafka"
	rabbitmqinjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/mq/rabbitmq"
	pginjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/pg"
	redisinjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/redis"
	"github.com/vincent-wuhan/opskeeper/core/harness/runner"
	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
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
	case "approve":
		err = cmdApprove(args)
	case "judge":
		err = cmdJudge(ctx, args)
	case "leaderboard":
		err = cmdLeaderboard(ctx, args)
	case "run-loop":
		err = cmdRunLoop(ctx, args)
	case "list-cases":
		err = cmdListCases(ctx, args)
	case "plugin-coverage":
		err = cmdPluginCoverage(ctx, args)
	case "vocabulary":
		err = cmdVocabulary(ctx, args)
	case "project":
		err = cmdProject(ctx, args)
	case "axes":
		err = cmdAxes(ctx, args)
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
  approve      签一条双人审批记录（用审批人自己的密钥；不碰任何环境）
               approve --case <id> --env prod --request-by alice --approve-as bob --out ok.json
  inject       读 case、路由到注入器、真的把它注入目标环境
               inject --env prod 还需要 --approval <record.json>（决策 303：--confirm-prod 必要但不充分）
               inject --case <id> --dry-run    只列出这个 case 会注入什么
               inject --case <id> --hold 5m    按住故障 5 分钟再撤销（默认进程退出即撤销）
               inject --max-duration 10m       单个故障的时间窗上限（staging 默认 30m，prod 默认 10m）
                                              超过就拒绝执行，不截断
  judge        对已有 incident 报告重跑 judge
  leaderboard  显示排行榜 + 回归基线
               leaderboard --lock-baseline     把当前分数锁成基线
               leaderboard --baselines         打印基线表
               leaderboard --check-regression  对照基线检查回归（block 时非零退出）
  run-loop     harness loop-mode 跑闭环（Day 6+）
               run-loop --execution-mode=real-agentteams 需要 --incident-id/--trace-id 与六类证据文件
  list-cases   列出所有 golden case
  plugin-coverage  把 golden case 的能力期望与插件包能力做对照（哪些 case 结构上不可能通过）
  vocabulary       把 golden case 的能力期望与本构建真实能产出的词表做对照
                   （哪些 case 连结构上都无法满足——这类 case 的分数不是 agent 的成绩）
  project          把生产的 RootCauseJSON 契约投影成 judge.AgentResponse
                   project --contract rc.json --kind-map kinds.json --out resp.json
  axes             列出每个 case 声明的三个诊断轴（Localization × Identification
                   × Reason），并报出哪些 case 的 locus 退化成资源族
                   opskeeper-eval axes --fail-on-unmeasured-axis

FLAGS:
  --version    输出版本
  --help       显示帮助

Use "opskeeper-eval <subcommand> --help" for subcommand-specific flags.

EXAMPLE:
  opskeeper-eval run --case pg/long-running-tx --env staging
  opskeeper-eval run --suite middleware-baseline --concurrency 4
  opskeeper-eval inject --case k8s/pod-oom --target ns=test deploy=order-svc
  opskeeper-eval judge --case pg/long-running-tx --response agent-response.json
  opskeeper-eval judge --case pg/long-running-tx --response agent-response.json --judge llm --provider anthropic
  opskeeper-eval run-loop --case host/cpu-spike --execution-mode=real-agentteams \
      --incident-id host-cpu-spike-real --trace-id <32 hex> --postmortem-evidence pm.json --judge llm
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
	casesDir := fs.String("cases-dir", "core/harness/cases", "golden case 目录")
	env := fs.String("env", "staging", "目标环境（prod 需 --confirm-prod + 双人审批记录）")
	confirmProd := fs.Bool("confirm-prod", false, "确认在 prod 环境注入（**必要但不充分**：仍需 --approval）")
	approvalPath := fs.String("approval", "", "双人审批记录（JSON）；prod 必填，用 `approve` 子命令生成")
	approvalKeys := fs.String("approval-keys", "", "审批密钥目录（默认读 "+approvalKeysEnv+"）")
	approvalMaxAge := fs.Duration("approval-max-age", 0, "一条审批记录最多算多新（0 = 默认 1h）")
	target := fs.String("target", "", "target spec（key=value 空格分隔，如 ns=test deploy=order-svc）")
	dryRun := fs.Bool("dry-run", false, "只列出这个 case 会注入什么，不真注入")
	hold := fs.Duration("hold", 0, "注入后把这个故障按住多久（0 = 进程退出即消失）")
	maxDuration := fs.Duration("max-duration", 0, "单个故障允许活多久的上限（0 = 用该环境的默认上限）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *caseID == "" {
		return fmt.Errorf("--case required")
	}
	if *env == "prod" {
		if !*confirmProd {
			return fmt.Errorf("refusing to inject in prod without --confirm-prod")
		}
		// 双人审批（决策 303）。它在读 case 之前跑，因为它的全部输入就是
		// 两个身份与一个文件——而"在碰目标环境之前拒绝"这条在决策 302 里
		// 已经定过一次规矩了。
		if *approvalMaxAge <= 0 {
			*approvalMaxAge = defaultApprovalMaxAge
		}
		if err := checkProdApproval(approvalRequest{
			caseID:   *caseID,
			env:      *env,
			operator: os.Getenv(operatorEnv),
		}, *approvalPath, *approvalKeys, time.Now(), *approvalMaxAge); err != nil {
			return fmt.Errorf("refusing to inject in prod: %w", err)
		}
	}
	c, err := schema.NewLoader(*casesDir).LoadByID(*caseID)
	if err != nil {
		return fmt.Errorf("inject: load case %s: %w", *caseID, err)
	}
	if len(c.Inject) == 0 {
		return fmt.Errorf("inject: case %s declares no inject step; there is nothing to inject", *caseID)
	}
	override, err := parseTarget(*target)
	if err != nil {
		return err
	}

	// 时间窗闸门在**碰目标环境之前**跑。
	//
	// 顺序不是风格问题：注入是有副作用的，而这一步的判断只需要读 case 文件
	// 与两个 flag。先注入再拒绝，等于"报了错但环境已经被改了"——而一个刚被
	// 写满的磁盘不会因为命令行返回非零就自己恢复。
	if *maxDuration <= 0 {
		*maxDuration = defaultInjectCeiling(*env)
	}
	if err := checkInjectCeiling(*maxDuration, *hold, c.Inject); err != nil {
		return err
	}

	reg := newInjectorRegistry()

	fmt.Printf("inject: case=%s env=%s steps=%d max_duration=%s\n",
		*caseID, *env, len(c.Inject), *maxDuration)
	var blocked []string
	var staged []stagedFault
	for i, step := range c.Inject {
		spec, serr := injector.FromSchemaStep(step, 0, *env)
		if serr != nil {
			return fmt.Errorf("inject: step %d: %w", i+1, serr)
		}
		if len(override) > 0 {
			if spec.Params == nil {
				spec.Params = map[string]interface{}{}
			}
			spec.Params["target"] = override
		}
		impl, action, ok := reg.Route(spec.Type)
		if !ok {
			blocked = append(blocked, fmt.Sprintf("  step %d  %s: 没有注册任何处理 %s. 前缀的注入器（已注册：%s）",
				i+1, spec.Type, prefixOf(spec.Type), strings.Join(reg.Prefixes(), ", ")))
			continue
		}
		line := fmt.Sprintf("  step %d  %-32s action=%-24s duration=%s", i+1, spec.Type, action, spec.Duration)
		if *dryRun {
			fmt.Println(line)
			continue
		}
		if aerr := impl.CheckAvailable(ctx); aerr != nil {
			blocked = append(blocked, fmt.Sprintf("%s\n      %v", line, aerr))
			continue
		}
		res, ierr := impl.Inject(ctx, spec)
		if ierr != nil {
			blocked = append(blocked, fmt.Sprintf("%s\n      %v", line, ierr))
			continue
		}
		fmt.Printf("%s  inject_id=%s\n", line, res.InjectID)
		staged = append(staged, stagedFault{impl: impl, res: res, until: spec.Duration})
	}
	if len(staged) > 0 && *hold == 0 {
		// 一条锁链的"存在"就是那几条还攥着行锁的连接。
		// 它们属于本进程，所以本进程一退出，故障就撤销了——
		// 而一次在诊断开始之前就自己好了的故障，诊断结论是关于空气的。
		// 不默认按住，是因为默认挂 3 分钟会让 CI 里的这条命令变成一个陷阱；
		// 但必须把这件事说出来，而不是让人以为注入还生效着。
		fmt.Fprintf(os.Stderr, "\ninject: 注意：这 %d 步是**真的**改了目标环境，但故障活在"+
			"本进程的连接上，进程退出即撤销。要让它活到诊断结束，请加 --hold。\n", len(staged))
	}
	if len(blocked) > 0 && len(staged) == 0 {
		fmt.Fprintf(os.Stderr, "\ninject: 以下 %d 步没有执行：\n", len(blocked))
		for _, b := range blocked {
			fmt.Fprintln(os.Stderr, b)
		}
		return fmt.Errorf("inject: %d of %d step(s) not executed", len(blocked), len(c.Inject))
	}
	holdFaults(ctx, staged, *hold, blocked)
	return nil
}

// defaultInjectCeiling 是这个命令在每个环境下的默认时间窗上限。
//
// prod 严一档，不是"prod 更危险"这种修辞：`host.fill_disk` 在 prod 上
// 写满的是别人的节点，在 staging 上写满的是一块一次性磁盘。
func defaultInjectCeiling(env string) time.Duration {
	if env == "prod" {
		return 10 * time.Minute
	}
	return 30 * time.Minute
}

// checkInjectCeiling 拒绝任何超过时间窗的注入。
//
// 它**拒绝而不截断**。截断看起来更"好用"，但它会让一条 600s 的 case
// 悄悄变成一个 300s 的故障，而 harness 会拿这个 300s 的结果去判一份
// 诊断结论——报告出来的时间与真实发生的时间不一样，这比直接报错坏得多。
//
// 它查两样：`--hold`（进程按住多久）与每一步 case 自带的 `duration`
// （注入器自己的自撤销时限）。两者取的是**故障真正活着的时间**——
// 进程跑着的时候，故障活 `max(duration, hold)`。
func checkInjectCeiling(ceiling, hold time.Duration, steps []schema.InjectStep) error {
	if hold > ceiling {
		return fmt.Errorf("refusing to inject: --hold %s is over the %s time window for this "+
			"environment; lower --hold or raise --max-duration deliberately (a fault left "+
			"running for hours can outlive the process that created it: files on disk and "+
			"messages in a broker are not undone by the injector going away)",
			hold, ceiling)
	}
	for i, step := range steps {
		d, err := time.ParseDuration(step.Duration)
		if err != nil {
			return fmt.Errorf("inject: step %d: invalid duration %q: %w", i+1, step.Duration, err)
		}
		if d > ceiling {
			return fmt.Errorf("refusing to inject: step %d (%s) asks for %s, over the %s time "+
				"window for this environment; raise --max-duration deliberately if that is "+
				"really what you want, otherwise this is the wrong case for this run",
				i+1, step.Type, d, ceiling)
		}
	}
	return nil
}

// cmdApprove 签一条双人审批记录。
//
// 没有它，"双人审批"就是一道谁也过不去的门：记录是审批人和发起人之间
// **唯一的**传递物，而它必须由**审批人自己的密钥**签出来——所以签这一步
// 只能在审批人自己手上做。
//
// 它不碰任何环境，只读一个密钥文件、写一个 JSON。
func cmdApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	caseID := fs.String("case", "", "要批准的 case ID")
	env := fs.String("env", "prod", "目标环境")
	requestBy := fs.String("request-by", "", "发起人（必须与运行时 OPSKEEPER_HARNESS_OPERATOR 一致）")
	approveAs := fs.String("approve-as", "", "审批人；密钥从 --approval-keys/<identity>.key 读")
	keys := fs.String("approval-keys", "", "审批密钥目录（默认读 "+approvalKeysEnv+"）")
	note := fs.String("note", "", "审批备注（会被签进记录，改一个字签名就失效）")
	validFor := fs.Duration("valid-for", 0, "这条记录多久之后失效（0 = 默认 1h）")
	out := fs.String("out", "", "记录写到哪个文件（默认打到 stdout）")
	now := time.Now()
	approvedAt := fs.String("approved-at", "",
		"把 approved_at 钉成这个时刻（RFC3339）；只给测试与事后复核用")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *caseID == "" {
		return fmt.Errorf("--case required")
	}
	if *requestBy == "" {
		return fmt.Errorf("--request-by required")
	}
	if *approveAs == "" {
		return fmt.Errorf("--approve-as required")
	}
	if *approveAs == *requestBy {
		return fmt.Errorf("%q cannot both request and approve; that is not two-person approval, "+
			"it is --confirm-prod with a JSON file attached", *approveAs)
	}
	key, err := loadApprovalKey(approvalKeysDir(*keys), *approveAs)
	if err != nil {
		return err
	}
	at := now
	if *approvedAt != "" {
		parsed, perr := time.Parse(time.RFC3339, *approvedAt)
		if perr != nil {
			return fmt.Errorf("--approved-at %q: %w", *approvedAt, perr)
		}
		at = parsed
	}
	valid := defaultApprovalAge
	if *validFor > 0 {
		valid = *validFor
	}
	rec := ApprovalRecord{
		Case:        *caseID,
		Env:         *env,
		RequestedBy: *requestBy,
		ApprovedBy:  *approveAs,
		ApprovedAt:  at,
		ExpiresAt:   at.Add(valid),
		Note:        *note,
	}
	if err := rec.Sign(key); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	blob = append(blob, '\n')
	if *out == "" {
		_, err = os.Stdout.Write(blob)
		return err
	}
	if err := os.WriteFile(*out, blob, 0o600); err != nil {
		return fmt.Errorf("write approval record: %w", err)
	}
	fmt.Printf("approve: 已签一条 %s/%s 的审批（%s 批给 %s），有效期到 %s，写到 %s\n",
		rec.Env, rec.Case, rec.ApprovedBy, rec.RequestedBy,
		rec.ExpiresAt.Format(time.RFC3339), *out)
	return nil
}

// stagedFault 是已经落到目标环境上、还等着被撤销的一次注入。
type stagedFault struct {
	impl  injector.Injector
	res   *injector.InjectResult
	until time.Duration
}

// holdFaults 把已注入的故障按住，然后逆序撤销。
//
// 这是"注入"这个动作的另一半。注入器把故障的寿命定义成"从 Inject 到
// Cleanup 之间的这段时间"，而 Cleanup 的触发者只能是还活着的那个进程。
// 所以一个必须活过诊断的故障，得由一个不退出进程来按住——
// 顺手也把撤销做掉：故障留在一台真库上比从未注入过更难收拾。
//
// 有几步没注入成功时照样按住成功的那几步：已经落下去的故障
// 不能因为邻居失败就不管了。
func holdFaults(ctx context.Context, staged []stagedFault, hold time.Duration, blocked []string) {
	if len(staged) == 0 {
		return
	}
	if hold < 0 {
		hold = 0
	}
	if hold > 0 {
		fmt.Printf("inject: 按住 %d 步故障 %s（Ctrl-C 提前结束并撤销）\n", len(staged), hold)
		if len(blocked) > 0 {
			fmt.Fprintf(os.Stderr, "\ninject: 以下 %d 步没有执行：\n", len(blocked))
			for _, b := range blocked {
				fmt.Fprintln(os.Stderr, b)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(hold):
		}
	}
	for i := len(staged) - 1; i >= 0; i-- {
		if err := staged[i].impl.Cleanup(context.Background(), staged[i].res.InjectID); err != nil {
			fmt.Fprintf(os.Stderr, "inject: 撤销 %s 失败: %v\n", staged[i].res.InjectID, err)
		}
	}
	if hold > 0 {
		fmt.Printf("inject: 已撤销 %d 步\n", len(staged))
	}
}

// newInjectorRegistry 装上全部六个注入器。
//
// 这是本文件第一次真的用上这个注册表：此前 cmdInject 只打印一行
// "inject: case=... confirm_prod=..." 就返回 0，六个骨架一次都没被调用过，
// 而 printUsage 把它写成"手动触发 fault-injector"。
func newInjectorRegistry() *injector.Registry {
	reg := injector.NewRegistry()
	// 连接参数在**装配根**读，不让注入器自己读全局环境。
	// 两边都读的话，"这台机器上有没有配库"就变成一个藏在
	// pgx.Connect / go-redis 的 Dial 里的事实：命令行看不到它，
	// 测试也没法用 t.Setenv 钉住它。
	for _, impl := range []injector.Injector{
		hostinjector.New(hostinjector.WithRoot(os.Getenv(hostinjector.RootEnv))),
		pginjector.New(pginjector.WithDSN(os.Getenv(pginjector.DSNEnv))),
		redisinjector.New(
			redisinjector.WithAddr(os.Getenv(redisinjector.AddrEnv)),
			redisinjector.WithPassword(os.Getenv(redisinjector.PasswordEnv)),
		),
		kafkainjector.New(kafkainjector.WithBrokers(splitBrokers(os.Getenv(kafkainjector.BrokersEnv))...)),
		rabbitmqinjector.New(rabbitmqinjector.WithURL(os.Getenv(rabbitmqinjector.URLEnv))),
		k8sinjector.New(k8sinjector.WithKubeconfig(os.Getenv(k8sinjector.KubeconfigEnv))),
	} {
		if err := reg.Register(impl); err != nil {
			// 注册冲突是编程错误，不是运行时状态：拼错前缀会在这里炸，
			// 而不是等到某个 case 路由不到时被读成"没有这个注入器"。
			panic(fmt.Sprintf("inject: register %s: %v", impl.Type(), err))
		}
	}
	return reg
}

// splitBrokers 把逗号分隔的 broker 列表拆开，顺手丢掉空项与空白。
//
// 放在装配根而不是让注入器自己拆，是同一条理由：连接参数在命令行可见，
// 就该在命令行可见。
func splitBrokers(raw string) []string {
	var out []string
	for _, b := range strings.Split(raw, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// parseTarget 把 "k=v k2=v2" 解析成 map。
func parseTarget(spec string) (map[string]string, error) {
	if strings.TrimSpace(spec) == "" {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	for _, field := range strings.Fields(spec) {
		k, v, ok := strings.Cut(field, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--target %q: expected key=value, got %q", spec, field)
		}
		out[k] = v
	}
	return out, nil
}

func prefixOf(injectType string) string {
	if i := strings.IndexByte(injectType, '.'); i >= 0 {
		return injectType[:i+1]
	}
	return injectType
}

func cmdLeaderboard(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leaderboard", flag.ExitOnError)
	dir := fs.String("dir", "harness/result/loop", "LoopResult JSON 目录")
	outDir := fs.String("out-dir", "harness/result", "Markdown 报告输出目录")
	threshold := fs.Float64("threshold", 0.5, "recovery_pass_rate 门槛（低于则 NOT QUALIFIED）")
	baselinePath := fs.String("baseline-file", "harness/result/baseline.json", "回归基线文件（lock / check 都读写它）")
	lockBaseline := fs.Bool("lock-baseline", false, "把当前分数写成本次基线")
	showBaselines := fs.Bool("baselines", false, "打印基线表后退出")
	checkRegression := fs.Bool("check-regression", false, "对照基线检查回归；有 block 时非零退出")
	failOnWarn := fs.Bool("fail-on-warn", false, "--check-regression 下 warn 也非零退出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := harnessleaderboard.NewLoopBoard(*dir)
	if err != nil {
		return fmt.Errorf("leaderboard: %w", err)
	}
	b.RecoveryPassRateThreshold = *threshold

	if *lockBaseline {
		by := os.Getenv("GIT_AUTHOR_NAME")
		if by == "" {
			by = os.Getenv("USER")
		}
		locked, unmeasured := harnessleaderboard.LockBaseline(b, by)
		if err := harnessleaderboard.SaveBaseline(*baselinePath, locked); err != nil {
			return err
		}
		fmt.Printf("baseline locked: %s (%d cases, metrics %s)\n",
			*baselinePath, len(locked.Scores), strings.Join(locked.Metrics, "+"))
		if len(unmeasured) > 0 {
			// 说出来，而不是让它们安静地不出现：下一次这些 case 有分数了，
			// 它们会被当成新 case，而这与"基线里本来就有"不是一回事。
			fmt.Printf("  not locked (no metric measured): %s\n", strings.Join(unmeasured, ", "))
		}
	}
	if *showBaselines {
		if err := printBaselines(*baselinePath); err != nil {
			return err
		}
	}
	if *checkRegression {
		// 基线读不出来就是非零退出。把它当成"零回归"是这条命令最容易犯的错，
		// 而且发生在最需要它说实话的时刻：一次刚引入回归的 CI。
		if err := reportRegressions(b, *baselinePath, *failOnWarn); err != nil {
			return err
		}
	}
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

// printBaselines 打印基线表。
func printBaselines(path string) error {
	base, err := harnessleaderboard.LoadBaseline(path)
	if err != nil {
		return err
	}
	fmt.Printf("baseline %s\n  locked_at: %s\n  locked_by: %s\n  metrics: %s\n",
		path, base.LockedAt.Format(time.RFC3339), base.LockedBy, strings.Join(base.Metrics, " + "))
	ids := make([]string, 0, len(base.Scores))
	for id := range base.Scores {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Printf("  %-44s %.3f\n", id, base.Scores[id])
	}
	fmt.Printf("  (%d cases)\n", len(ids))
	return nil
}

// reportRegressions 对照基线并按严重程度决定退出码。
func reportRegressions(b *harnessleaderboard.LoopBoard, path string, failOnWarn bool) error {
	base, err := harnessleaderboard.LoadBaseline(path)
	if err != nil {
		return err
	}
	rep := harnessleaderboard.CheckBoard(b, base)
	for _, r := range rep.Regressions {
		fmt.Printf("  [%s] %-44s %s\n", r.Severity, r.CaseID, r.Message)
	}
	fmt.Printf("regressions: %d (warn %d / block %d)  improved: %d  "+
		"new: %d  missing: %d  unmeasured: %d\n",
		len(rep.Regressions), countSeverity(rep, harnessleaderboard.SeverityWarn),
		countSeverity(rep, harnessleaderboard.SeverityBlock),
		len(rep.Improved), len(rep.New), len(rep.Missing), len(rep.Unmeasured))
	// 这三类都不是回归，但它们是"这次没被判定过"。全部为零时这句话才有意义，
	// 所以它在总数里被单独说出来，而不是被并进"无回归"。
	if n := rep.Unaccounted(); n > 0 {
		fmt.Printf("  note: %d case(s) were not judged this run — new: %s | missing: %s | unmeasured: %s\n",
			n, joinOrNone(rep.New), joinOrNone(rep.Missing), joinOrNone(rep.Unmeasured))
	}
	switch rep.Worst() {
	case harnessleaderboard.SeverityBlock:
		return fmt.Errorf("regression check: %d regression(s), %d of them blocking",
			len(rep.Regressions), countSeverity(rep, harnessleaderboard.SeverityBlock))
	case harnessleaderboard.SeverityWarn:
		if failOnWarn {
			return fmt.Errorf("regression check: %d regression(s) at or above the warn threshold", len(rep.Regressions))
		}
	}
	return nil
}

func countSeverity(rep *harnessleaderboard.RegressionReport, sev harnessleaderboard.Severity) int {
	n := 0
	for _, r := range rep.Regressions {
		if r.Severity == sev {
			n++
		}
	}
	return n
}

func joinOrNone(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
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
	casesDir := fs.String("cases-dir", "core/harness/cases", "cases 目录")
	outDir := fs.String("out-dir", "harness/result/loop", "LoopResult JSON 输出目录")
	stateEvidence := fs.String("state-evidence", "", "real-agentteams state.json 路径")
	hitlEvidence := fs.String("hitl-evidence", "", "real-agentteams Matrix HITL/proposal evidence 路径")
	mcpEvidence := fs.String("mcp-evidence", "", "real-agentteams MCP role-call/audit evidence 路径")
	fixtureBeforeEvidence := fs.String("fixture-before-evidence", "", "real-agentteams fixture before evidence 路径")
	fixtureAfterEvidence := fs.String("fixture-after-evidence", "", "real-agentteams fixture after evidence 路径")
	postmortemEvidence := fs.String("postmortem-evidence", "", "real-agentteams postmortem evidence 路径")
	judgeMode := fs.String("judge", judgeHeuristic, "评分器：heuristic（不联网）/ llm（真实模型评分，仅 real-agentteams 模式有意义）")
	judgeProvider := fs.String("judge-provider", "", "LLM provider（llm 模式）")
	judgeModel := fs.String("judge-model", "", "模型（llm 模式）")
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
	// Resolved before the run so an unusable judge configuration is a
	// refusal to start rather than a run that produces an unscored result
	// and reports success.
	deps, err := buildLoopDeps(*judgeMode, *judgeProvider, *judgeModel, executionMode)
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
	}, deps)
	if err != nil {
		return fmt.Errorf("run-loop: %w", err)
	}
	fmt.Printf("LoopResult: case=%s mode=%s execution_mode=%s passed=%t final=%s duration=%dms\n",
		res.CaseID, res.Mode, res.ExecutionMode, res.Passed, res.FinalPhase, res.DurationMs)
	fmt.Printf("  rca_accuracy: %v\n", fmtFloatPtr(res.Rubric.RCAAccuracy))
	fmt.Printf("  time_to_remediate: %s\n", res.Rubric.TimeToRemediate)
	fmt.Printf("  approval_rate: %v\n", fmtFloatPtr(res.Rubric.ApprovalRate))
	fmt.Printf("  recovery_pass_rate: %v\n", fmtFloatPtr(res.Rubric.RecoveryPassRate))
	// Provenance, printed next to the numbers it describes. A reader who
	// sees four rubric figures and no judge cannot tell a measurement from
	// a synthesis, and the synthesis is the default in every mode but one.
	if len(res.JudgeScores.JudgesUsed) == 0 {
		fmt.Printf("  judge: none — rca_accuracy is synthesized from the terminal phase, not judged\n")
	} else {
		fmt.Printf("  judge: %s (overall=%.3f)\n",
			strings.Join(res.JudgeScores.JudgesUsed, ","), res.JudgeScores.Overall)
	}
	for _, f := range res.Flags {
		if f == "judge_heuristic_on_free_text" {
			fmt.Printf("  warning: the heuristic judge matches symbolic root-cause ids by exact\n")
			fmt.Printf("           string, so it cannot score a real postmortem's prose. This\n")
			fmt.Printf("           rca_accuracy reflects the matcher, not the agent. Re-run with\n")
			fmt.Printf("           --judge=llm for a score that means something.\n")
		}
	}
	return nil
}

// buildLoopDeps resolves the judge wiring for one run.
//
// An LLM judge is only offered in real-agentteams mode. The other modes
// either have no agent output to score (dry-run) or need an orchestrator
// the CLI does not build (orchestrator), and injecting a paid model into a
// run whose score is then discarded is a cost with no result.
func buildLoopDeps(mode, provider, model string, executionMode runner.LoopExecutionMode) (runner.LoopDeps, error) {
	if mode == judgeHeuristic {
		return runner.LoopDeps{}, nil
	}
	if mode != judgeLLM {
		return runner.LoopDeps{}, fmt.Errorf("--judge must be %q or %q, got %q", judgeHeuristic, judgeLLM, mode)
	}
	if executionMode != runner.ExecutionModeRealAgentTeams {
		return runner.LoopDeps{}, fmt.Errorf(
			"--judge=llm requires --execution-mode=real-agentteams: that is the only mode whose result carries an agent's own root cause to score")
	}
	c, err := resolveCompleter(provider, model)
	if err != nil {
		return runner.LoopDeps{}, err
	}
	return runner.LoopDeps{LLMClient: c}, nil
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
	casesDir := fs.String("cases-dir", "core/harness/cases", "cases 目录")
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
		// path 形如 core/harness/cases/pg/long-running-tx/case.yaml
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

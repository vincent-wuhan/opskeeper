// Command audit is the completion audit: which open items this repository can
// decide, and which ones it cannot.
//
// The problem it answers is one the ledger made visible. After the diagnosis
// axis reached 20/20, the remaining open items stopped being the same KIND
// of thing as the ones that came before. Most of the last ones needed a
// capability. What is left mostly needs evidence this repository cannot
// produce — a real provider key, a real Docker daemon, a real broker — and
// those were being carried in prose, where they look exactly like the
// capability items did.
//
// So every open item is declared here with a class:
//
//	repo      — this repository can decide it, right now, with a command.
//	external  — the deciding evidence lives outside this tree. Declared with
//	            what that evidence is and who holds it, so "we could not test
//	            it" stops being a shrug.
//	judgment  — a claim that is a decision, not a measurement. Never counted
//	            as closed by a machine, because a machine counting its own
//	            author would be the one thing an audit cannot be.
//
// The score is repo items closed over repo items. External items are printed,
// never scored: scoring them zero would say the platform is 90% done, and
// scoring them full would say the audit passed. Neither is true.
//
// Usage:
//
//	go run ./scripts/audit [repo-root]
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type class string

const (
	classRepo     class = "repo"
	classExternal class = "external"
	classJudgment class = "judgment"
)

type item struct {
	id      string
	subject string
	class   class
	// done reports whether a repo item is satisfied. Nil for the classes
	// this command refuses to score.
	done func(root string) (bool, string, error)
	// evidence and holder are required for external items and ignored
	// otherwise: "cannot test" is only honest when it says what would.
	evidence string
	holder   string
	// closer names a command in this repository that runs the evidence.
	// An external item with no command behind it is a wish; one with a
	// command is a task somebody can pick up. It is printed, never run,
	// and never scored.
	closer string
}

// repoItems are the claims this repository can settle today.
//
// Each names the symbol or the file that would exist if the item were
// closed. That naming is a contract: whoever implements one has to write the
// ledger entry naming what they added, and the audit follows the entry rather
// than the author's memory. A gate that inferred intent from filenames would
// be guessing, and a gate that guessed would be trusted.
var repoItems = []item{
	{
		id:      "A1",
		subject: "诊断轴 20/20 且 GAP 表为空",
		class:   classRepo,
		done: func(root string) (bool, string, error) {
			test, err := read(root, "core/floor/pluginmanifest/coverage_test.go")
			if err != nil {
				return false, "", err
			}
			if !strings.Contains(test, "const want = 20") {
				return false, "棘轮不是 20：诊断轴掉过能力，或这条读数过期", nil
			}
			src, err := read(root, "core/floor/pluginmanifest/coverage.go")
			if err != nil {
				return false, "", err
			}
			if gaps := countGaps(src); gaps != 0 {
				return false, fmt.Sprintf("DiagnosisGaps 还有 %d 条未关记录", gaps), nil
			}
			return true, "", nil
		},
	},
	{
		id:      "A2",
		subject: "五个已发布包都声明两条 host 下限",
		class:   classRepo,
		done: func(root string) (bool, string, error) {
			dir := filepath.Join(root, "plugins", "pig-ops")
			entries, err := os.ReadDir(dir)
			if err != nil {
				return false, "", err
			}
			var missing []string
			packages := 0
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				body, err := read(filepath.Join(root, "plugins", "pig-ops"), filepath.Join(e.Name(), "pig-ops.yaml"))
				if err != nil {
					return false, "", err
				}
				packages++
				if !strings.Contains(body, "min_edge_version") || !strings.Contains(body, "min_pig_version") {
					missing = append(missing, e.Name())
				}
			}
			if len(missing) > 0 {
				return false, "未声明下限：" + strings.Join(missing, ", "), nil
			}
			return true, "", nil
		},
	},
	{
		id:      "A3",
		subject: "面向调用者的拒绝不把一条策略说成一项缺失的功能",
		class:   classRepo,
		done:    noMisleadingRefusal,
	},
	{
		id:      "B1",
		subject: "重平衡历史跨控制面重启保留",
		class:   classRepo,
		done: func(root string) (bool, string, error) {
			return declared(root, "core/manager/middleware/adapter/mq",
				"func NewRebalanceHistoryStore(", "history is in memory only: a control-plane restart loses the window")
		},
	},
	{
		id:      "B2",
		subject: "重平衡采样由定时器驱动，而非只挂在读调用上",
		class:   classRepo,
		done: func(root string) (bool, string, error) {
			return declared(root, "core/manager/middleware/adapter/mq",
				"func (c *kafkaClient) StartRebalanceSampler(", "sampling hangs off the read path, so a group nobody asks about has no history")
		},
	},
	{
		id:      "B3",
		subject: "市场索引覆盖本租户安装根之外的来源",
		class:   classRepo,
		done: func(root string) (bool, string, error) {
			return declared(root, "core/floor/pluginmanifest",
				"func LoadCatalogSources(", "the index reads one directory: the cluster-wide and image-baked roots a tenant can also install from are not in it")
		},
	},
}

// noMisleadingRefusal looks for one specific way a caller is misled.
//
// A refusal that says "not implemented" describes a capability that is
// missing. A refusal that is really a policy — "denied until X lands" — has
// the same effect on the reader and the opposite meaning: one produces a bug
// report against a decision nobody is going to revisit, the other produces the
// question the reader actually has, which is under what condition the answer
// changes.
//
// This is a separate item rather than a clause inside A1 or A2 because those
// read a registry and this walks the tree, and because it is a different kind
// of thing: every other repo item checks that something exists, and this one
// checks that something says the truth.
//
// The scan is over core/ and cmd/ rather than the whole tree, deliberately.
// plugins/pig-ops carries vendored copies of the extensions and web/ carries a
// build of somebody else's stylesheets; a rule whose reach is wider than its
// evidence is a rule that gets switched off. The baseline is zero, so no
// allowlist is needed and a new offender is unambiguous.
//
// What it does not check: whether the policy the message describes is the
// policy the code implements. That is what the unit tests beside the gate are
// for; this only insists that the message does not lie about the *kind* of
// thing it is.
func noMisleadingRefusal(root string) (bool, string, error) {
	phrases := []string{"not implemented", "unimplemented", "not yet implemented"}
	for _, dir := range []string{"core", "cmd"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			// A tree without that root is a fixture, not a failure. An item
			// that cannot be evaluated says so by finding nothing to say.
			continue
		}
		offender, err := misleadingRefusalIn(base)
		if err != nil {
			return false, "", err
		}
		if offender == "" {
			continue
		}
		for _, phrase := range phrases {
			if !strings.Contains(offender, phrase) {
				continue
			}
			return false, fmt.Sprintf("%s refuses with %q: a refusal that names a missing feature reads "+
				"as a bug rather than as the policy it is — say under what condition the answer changes",
				dir, phrase), nil
		}
	}
	return true, "", nil
}

// misleadingRefusalIn returns the first offending line under base, or "".
//
// The scan is line-based on purpose: a file-level match would report a 400-line
// file as "the offender", and a rule that names a file instead of a line is a
// rule people stop reading. The fallback to a file-level match exists because a
// multi-line Errorf can put ErrForbidden on one line and the phrase on another
// — finding both in one file and refusing to report it would be a false pass,
// which is the one failure mode a gate must not have.
func misleadingRefusalIn(base string) (string, error) {
	phrases := []string{"not implemented", "unimplemented", "not yet implemented"}
	fileHasBoth := ""
	hit := ""

	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || (strings.HasPrefix(d.Name(), ".") && d.Name() != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// Comment lines are removed first, and this is not a detail: the
		// first version of this rule flagged a file because a *comment*
		// explaining why the message must not say "not implemented" contained
		// the phrase. A rule that punishes its own documentation is a rule
		// whose fix is to delete the documentation.
		//
		// Only whole-line comments are dropped, by their leading "//". A
		// line that merely contains "//" — a URL, say — is kept, because
		// dropping it would create exactly the false pass this item exists to
		// avoid. Block comments are not handled; none of the refusals in
		// this tree sit inside one, and adding a parser here would be a
		// second Go front end for a rule about strings.
		code := make([]string, 0, 64)
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			code = append(code, strings.ToLower(line))
		}
		lower := strings.Join(code, "\n")
		if !strings.Contains(lower, "errforbidden") {
			return nil
		}
		for _, line := range code {
			if !strings.Contains(line, "errforbidden") {
				continue
			}
			for _, phrase := range phrases {
				if strings.Contains(line, phrase) {
					hit = line
					return filepath.SkipAll
				}
			}
		}
		if fileHasBoth == "" {
			for _, phrase := range phrases {
				if strings.Contains(lower, phrase) {
					fileHasBoth = phrase
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if hit != "" {
		return hit, nil
	}
	return fileHasBoth, nil
}

// externalItems are the ones a real environment decides.
//
// They are listed with the evidence that would close them and who holds it.
// The reason for the format is that "cannot be tested here" is the sentence
// this repository has used most often and verified least.
var externalItems = []item{
	{
		id:       "E1",
		subject:  "阶段 0 端到端验收：一台 edge 用真实模型凭据完成一次对话",
		class:    classExternal,
		evidence: "一个真实 provider 密钥 + 一次 docker compose 起 edge + 一次 SSE 流式回包",
		holder:   "部署方（密钥持有人）",
		closer:   "bash scripts/acceptance/run.sh —— 退出码 3 即表示缺输入，并逐条点名缺什么",
	},
	{
		id:       "E2",
		subject:  "中间件适配器与真实 broker / 实例的联调",
		class:    classExternal,
		evidence: "一个 Kafka / PostgreSQL / Redis / K8s 实例接一次真实读写",
		holder:   "部署方",
		closer:   "make test-e2e-live —— 需要 tests/e2e/secrets.local.env，缺哪项会 SKIP 并写明",
	},
	{
		id:       "E4",
		subject:  "远程来源自身的索引在真实部署中被读回",
		evidence: "一个远程 registry 把索引服务出去，且 GET /v1/marketplace/catalog 把它并进索引并据此完成一次安装",
		class:    classExternal,
		holder:   "部署方（registry 侧与控制面侧各一次真实部署）",
		closer: "仓内两端已齐（决策 466）：产出 go run ./scripts/registryindex，消费 " +
			"OPSKEEPER_MARKETPLACE_REGISTRIES。剩下的只是部署时把前者服务出去、后者指过去 —— " +
			"这一条现在的诚实说法是「本仓没有可调的远端」指的不是缺代码，而是缺一次真实部署",
	},
	{
		id:       "E5",
		subject:  "节点交付闭环（make e2e-delivery-check）从未在 2.0 代码上执行过一次",
		class:    classExternal,
		evidence: "一次真的跑起来：节点进程环境与安装目录经审计无云厂商密钥，且四个用例经 frontier broker 往返",
		holder:   "部署方（一次带 broker 容器的真实运行）",
		closer: "make e2e-delivery-check —— 命令在本仓、测试在 tests/e2e/、断言是真的（" +
			"node_agent_delivery_test.go:214/233 分别审安装目录与进程环境）。它没跑过的原因是 " +
			"承载它的 job 挂在 schedule 上，而 GitHub 只对**默认分支**的 workflow 文件发 schedule：" +
			"默认分支是 main，main 落后本分支 529 个提交，其 .github/workflows/ci.yml 只有 72 行 " +
			"且根本没有 delivery job。所以这不是「等一个时钟」，是「那个 job 在本分支上永远不会被时钟点到」。" +
			"本仓能做的都做了；剩下的要么把 main 与本分支合流，要么由部署方手动 dispatch",
	},
	{
		id:       "E3",
		subject:  "多集群联邦的真实子集群",
		class:    classExternal,
		evidence: "第二个控制面进程 + 一次真实策略投递",
		holder:   "部署方",
		closer:   "bash scripts/acceptance/run.sh --chain=federation --root-env=… --child-env=… —— 五条离线检查随时可跑，三条要一份真的双控制面部署；F8 读子集群的 live 链接，那是策略抵达执行点的证据",
	},
}

// judgmentItems are claims no machine should close.
//
// They are printed so a reader can see they exist. Counting them would put
// the author of the claim in the position of grading it.
var judgmentItems = []item{
	{
		id:       "J1",
		subject:  "manager 是否还应该更小",
		class:    classJudgment,
		done:     nil,
		evidence: "这是一次取舍，不是一次测量；分界线由部署形态（一起扩缩容 / 一起故障）决定",
		holder:   "架构评审",
	},
}

func read(root, rel string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// declared reports whether the symbol that would close an item exists.
func declared(root, dir, symbol, why string) (bool, string, error) {
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return false, "", err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		body, err := read(filepath.Join(root, dir), e.Name())
		if err != nil {
			return false, "", err
		}
		if strings.Contains(body, symbol) {
			return true, "", nil
		}
	}
	return false, why, nil
}

// countGaps counts the entries of the DiagnosisGaps literal.
//
// It counts keys rather than trusting a comment, because the comment is
// exactly the thing that has been wrong twice in this repository's history.
//
// The first version counted the substring ": GapReason{" and was vacuous: Go
// lets a composite literal elide its type, and every entry in this map is
// written `"name": {`, so the pattern matched nothing and a map full of
// entries counted as empty. **A gate that cannot fail is worse than no gate**,
// because it is read as a check. So the count is per line, on the shape an
// entry actually has: a quoted key, a colon, then a brace.
func countGaps(src string) int {
	start := strings.Index(src, "var DiagnosisGaps = map[string]GapReason{")
	if start < 0 {
		return -1
	}
	entries := 0
	for _, line := range strings.Split(src[start:], "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "\"") {
			continue
		}
		colon := strings.Index(trimmed, "\":")
		if colon < 0 {
			continue
		}
		// colon indexes the COLON, and the quote that closes the key sits
		// one before it, so the value starts at colon+2. Reading from
		// colon+1 leaves the colon in the value and the test below never
		// matches — which is how a map full of entries counted as empty.
		rest := strings.TrimSpace(trimmed[colon+2:])
		if strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, "GapReason{") {
			entries++
		}
	}
	return entries
}

// all is the declared list, in the order it is reported.
func all() []item {
	return append(append(append([]item{}, repoItems...), externalItems...), judgmentItems...)
}

// score evaluates the repo items and returns the closed/total counts.
//
// It is a function so the denominator can be asserted: an external item that
// leaked into it would make "3 of 5 closed" mean something nobody intended.
func score(root string) (closed, total int, failures []string) {
	for _, it := range all() {
		if it.class != classRepo {
			continue
		}
		total++
		ok, _, err := it.done(root)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s cannot be evaluated: %v", it.id, err))
			continue
		}
		if ok {
			closed++
		}
	}
	return closed, total, failures
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	// The numbers come from score() rather than from this loop's counters,
	// because a report whose tally is maintained twice is a report that can
	// disagree with its own verdict — and the first version of this file
	// printed "2/0 closed" for exactly that reason.
	closed, total, failures := score(root)
	for _, it := range all() {
		switch it.class {
		case classRepo:
			ok, why, err := it.done(root)
			switch {
			case err != nil:
				fmt.Printf("  broken  %-3s %s\n            %v\n", it.id, it.subject, err)
			case ok:
				fmt.Printf("  closed  %-3s %s\n", it.id, it.subject)
			default:
				fmt.Printf("  open    %-3s %s\n            %s\n", it.id, it.subject, why)
			}
		case classExternal:
			fmt.Printf("  outside %-3s %s\n            evidence: %s\n            held by:   %s\n            closer:    %s\n",
				it.id, it.subject, it.evidence, it.holder, it.closer)
		case classJudgment:
			fmt.Printf("  call it %-3s %s\n            %s\n", it.id, it.subject, it.evidence)
		}
	}

	fmt.Printf("\naudit: repo items %d/%d closed\n", closed, total)
	fmt.Printf("       %d external item(s) are decided by evidence outside this tree, listed above with what would close them\n", len(externalItems))
	fmt.Printf("       %d judgment item(s) are decisions, not measurements, and are never scored\n", len(judgmentItems))
	sort.Strings(failures)
	for _, f := range failures {
		fmt.Fprintln(os.Stderr, "audit: "+f)
	}
	if len(failures) > 0 {
		os.Exit(1)
	}
}

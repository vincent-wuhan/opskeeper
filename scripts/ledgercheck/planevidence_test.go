package ledgercheck

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This is the twenty-first gate, and it exists because the seventeenth gate's
// own stated reason applies to far more of the plan than it was extended to.
//
// Gate 17 says it well: "a capability the plan names and no table tracks has an
// invisible progress value, and an invisible value cannot go red". Its list has
// two entries — the two stage-3 pillars. The plan's section 4 names roughly
// fifteen deliverables across stages 0, 1 and 2, and section 6 names four
// security requirements and three acceptance commands. None of those twenty-odd
// items is in that list, so for all of them the progress value is, by gate 17's
// own argument, invisible.
//
// The plan lives outside this repository, so a check cannot diff the two
// documents and the list below is hand-maintained. That is the honest shape of
// the problem and gate 17 already says so. What this gate adds is the half
// gate 17 does not have: an **evidence anchor** per item.
//
// "The progress section mentions it" is a weak claim — a word in prose is not a
// delivered capability. So every item here carries the file that implements it,
// and the security items carry the *name of a test function* that has to exist
// in that file. A path that exists proves the code is there; a named test
// proves there is something executable about it, and for the four items where
// the plan says "必须进 CI" that is the difference between a claim and a check.
//
// What this still does not prove, stated plainly so the next reader does not
// over-read it: that the anchor is *wired in*, that the test still passes, or
// that the item is complete. Those are the gates that already exist elsewhere
// in this repository, and this one is a cross-reference that keeps a renamed or
// deleted item from quietly becoming untracked.

// planCheck is one piece of executable evidence: a test function that has to be
// defined in a file that has to exist.
type planCheck struct {
	file string
	fn   string
}

type planItem struct {
	// name must appear in the progress section, for the reason gate 17 gives.
	name string
	// what is delivered, in the words the plan uses.
	what string
	// anchors are files that implement it. A missing one means the item was
	// renamed, moved, or removed and the list is now lying.
	anchors []string
	// checks are the named tests, for the items where "a file exists" is the
	// weakest possible evidence.
	checks []planCheck
}

// planItems is every item the plan names that this repository can point at.
// The section 4 stage numbers and the section 6 requirement numbers are in the
// comments so a reader holding the plan can find the row.
var planItems = []planItem{
	// --- 阶段 0 ---
	{
		name:    "LLM 网关",
		what:    "0.1 manager 侧 OpenAI 兼容端点，按节点令牌桶限流并返回 429",
		anchors: []string{"core/domains/server/llmgw/spend.go"},
	},
	{
		name:    "边缘接入",
		what:    "0.2 edge 把网关地址与节点令牌写进 agent 的进程环境",
		anchors: []string{"cmd/opskeeper-edge/agent.go"},
	},
	{
		name: "pig 二进制",
		what: "0.3 交叉编译并进 bundle 与镜像，安装时自检 --version",
		anchors: []string{
			"deploy/install/edge/build-edge-bundle.sh",
			"deploy/Dockerfile.opskeeper-edge",
		},
	},
	{
		name:    "节点 Agent 交付闭环",
		what:    "0.4 一台 edge 完成真实对话、独立进程、配置目录无云厂商密钥",
		anchors: []string{"tests/e2e/node_agent_delivery_test.go"},
	},

	// --- 阶段 1 ---
	{
		name: "遥测 spool",
		what: "1.1 断连落盘、按可丢弃度分级丢弃、恢复按序回放并限流",
		anchors: []string{
			"core/edge/spool/spool.go",
			// policy.go is where the drop order lives, and the order is the
			// part the plan actually specified: the plan wrote "traces 优先于
			// metrics 丢弃" and the code generalised it into four levels
			// with a stated reason. A gate that only pointed at spool.go
			// would survive losing the policy entirely.
			"core/edge/spool/policy.go",
		},
	},
	{
		name: "自治",
		what: "1.2 中心失联后由 edge 仲裁器按签名声明放行，写本地审计并回传",
		anchors: []string{
			"core/edge/autonomy/autonomy.go",
			"core/edge/autonomy/execute.go",
		},
	},
	{
		name:    "幂等与栅栏",
		what:    "1.3 计划点名的三条栅栏语义用例，逐条对应到三个测试",
		anchors: []string{"core/edge/policygate/fence.go"},
		checks: []planCheck{
			// 同一 idempotency_key 重复提交只执行一次
			{"core/edge/policygate/fence_test.go", "TestTheSameCallSubmittedEightTimesIsOneQuestionAndOneExecution"},
			// 批准后在执行租约内可领取，过期后不可
			{"core/edge/policygate/fence_test.go", "TestAGrantIsCollectableInsideItsLeaseAndNotAfterIt"},
			// 审批挂起期间并发的兄弟调用一起等，而不是只有被审批的那条
			{"core/edge/policygate/fence_test.go", "TestASecondMutatingCallInOneConversationWaitsRatherThanQueuing"},
		},
	},

	// --- 阶段 2 ---
	{
		name:    "工具注册表",
		what:    "2 工具注册与相关性检索，让插件变多之后仍能选对工具",
		anchors: []string{"core/manager/biz/aiops/tools"},
	},
	{
		name:    "per-tool 资源配额",
		what:    "2 声明并强制工具级输出上限与墙钟（内存上限刻意未声明，理由见台账 4.34.3）",
		anchors: []string{"core/floor/skill/types.go"},
	},
	{
		name:    "MCP",
		what:    "2 对外用 MCP 协议，对内自建网关做授权与审计",
		anchors: []string{"core/base/pkg/mcpclient"},
	},
	{
		name:    "结晶",
		what:    "2 被反复验证的修复晋升为确定性 runbook，证据驱动升降级",
		anchors: []string{"core/manager/biz/aiops/crystallize"},
	},
	{
		name:    "prompt injection",
		what:    "2 外来文本进模型前带 nonce 围栏，参数级授权不放松",
		anchors: []string{"core/pig/pigagent"},
	},

	// --- 阶段 3 ---
	{
		name: "审计端口",
		what: "3 抽出审计端口，解开 iam 反向依赖 manager 的那条边",
		// The port itself is core/base/pkg/audit (decision 109) and the
		// single throat that writes through it is the audit domain, which
		// moved to the release floor with the rest of the chain (decision
		// 226). Both are named: the port is what let iam stop importing
		// the writer, and the throat is what still holds it.
		anchors: []string{"core/base/pkg/audit", "core/domains/biz/audit"},
	},
	{
		name:    "联邦",
		what:    "3 中心下发签名策略，子集群可独立运行",
		anchors: []string{"core/floor/federation"},
	},
	{
		name:    "多租户",
		what:    "3 阶段 3 两根支柱之一（决策 197 登记，决策 198 定方向）",
		anchors: []string{"core/manager/iam"},
	},

	// --- 计划 §六 安全专项（必须进 CI）---
	{
		name:    "节点令牌越权",
		what:    "§六 安全 2：节点 A 的令牌不能用于节点 B 的推理",
		anchors: []string{"core/domains/server/llmgw/nodeidentity_test.go"},
		checks: []planCheck{
			{"core/domains/server/llmgw/nodeidentity_test.go", "TestOneNodesAllowanceCannotBeSpentOnAnothers"},
			{"core/domains/server/llmgw/nodeidentity_test.go", "TestAMixedPairIsRefusedBeforeTheModelIsReached"},
		},
	},
	{
		name:    "自治动作逃逸",
		what:    "§六 安全 3：篡改 argv、超 blast_radius、重放已执行幂等键均须被拒",
		anchors: []string{"core/edge/autonomy/escape_test.go"},
		checks: []planCheck{
			{"core/edge/autonomy/escape_test.go", "TestADeclarationWiderThanTheNodesCeilingIsRefused"},
			{"core/edge/autonomy/escape_test.go", "TestOnlyDeclaredRadiiRun"},
			// 重放已执行的幂等键
			{"core/edge/autonomy/autonomy_test.go", "TestAReplayIsRefused"},
			// 篡改 argv：runner 拿到的是声明里的那份，不是调用方声称的那份
			{"core/edge/autonomy/execute_test.go", "TestTheRunnerIsGivenTheDeclaredArgvAndNotTheClaimed"},
		},
	},
	{
		name:    "只读边界",
		what:    "§六 安全 4：确认写通道仍然关闭，防止有人为凑覆盖率而开放它",
		anchors: []string{"core/manager/middleware/toolset/toolset_gen_test.go"},
		checks: []planCheck{
			{"core/manager/middleware/toolset/toolset_gen_test.go", "TestTheMiddlewareToolsetIsReadOnly"},
			{"core/floor/pluginmanifest/middleware_profile_test.go", "TestEveryToolInTheMiddlewareProfileIsReadOnly"},
			{"core/floor/pluginmanifest/middleware_profile_test.go", "TestTheMiddlewareProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel"},
		},
	},
}

// TestEveryPlanItemThePlanNamesIsTrackedInTheProgressSection is the tracking
// half, and it is gate 17's test with a longer list. The quotes are stripped
// first for gate 17's reason: a name satisfied only by being quoted back from
// this file says nothing.
func TestEveryPlanItemThePlanNamesIsTrackedInTheProgressSection(t *testing.T) {
	progress := quotedRE.ReplaceAllString(progressSection(t), "")

	var missing []string
	for _, item := range planItems {
		if !strings.Contains(progress, item.name) {
			missing = append(missing, fmt.Sprintf(
				"the plan names %q (%s) and the progress section never mentions it; "+
					"a capability no table tracks has an invisible progress value",
				item.name, item.what))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("plan items the progress section does not track:\n  %s", strings.Join(missing, "\n  "))
	}
}

// TestEveryPlanItemsEvidenceStillExists is the half gate 17 does not have.
//
// "The progress section mentions it" is a word in prose. This asks where the
// code is, and for the items the plan says must be in CI it asks for the name
// of the test — so a renamed package, a moved file, or a deleted test all turn
// into a red run instead of a slowly-wrong table.
func TestEveryPlanItemsEvidenceStillExists(t *testing.T) {
	var missing []string

	for _, item := range planItems {
		if len(item.anchors) == 0 && len(item.checks) == 0 {
			t.Errorf("%q has no evidence anchor at all; an item nothing points at cannot be "+
				"distinguished from an item that was never built", item.name)
			continue
		}
		for _, anchor := range item.anchors {
			// The path is repository-relative and this test runs from
			// scripts/ledgercheck, so it needs the same ../../ the other
			// gates in this package use. Getting that wrong makes every
			// anchor look missing at once, which reads like a total
			// regression rather than a path bug.
			if _, err := os.Stat(filepath.FromSlash("../../" + anchor)); err != nil {
				missing = append(missing, fmt.Sprintf(
					"%q (%s) — its evidence %s is gone; the plan item was renamed, moved, or "+
						"removed, and this list now describes a repository that does not exist",
					item.name, item.what, anchor))
			}
		}
		for _, chk := range item.checks {
			path := filepath.FromSlash("../../" + chk.file)
			if _, err := os.Stat(path); err != nil {
				missing = append(missing, fmt.Sprintf(
					"%q (%s) — the file its named check lives in, %s, is gone",
					item.name, item.what, chk.file))
				continue
			}
			body := repoFile(t, "../../"+chk.file)
			decl := "func " + chk.fn + "("
			if !strings.Contains(body, decl) {
				missing = append(missing, fmt.Sprintf(
					"%q (%s) — %s no longer defines %s. The plan calls this one a CI "+
						"requirement, so a check that no longer exists is the item silently "+
						"becoming a claim",
					item.name, item.what, chk.file, chk.fn))
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("plan items whose evidence is no longer where this list says it is:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestThePlanItemListItselfIsNotEmpty is the precondition, and it is here for
// the same reason every other gate in this repository carries one.
//
// A list that has been emptied — by a bad merge, by a refactor that "cleaned
// it up" — leaves both tests above passing and green, because an empty list
// has no missing names and no missing files. The failure is silent and total,
// which is worse than a red run.
func TestThePlanItemListItselfIsNotEmpty(t *testing.T) {
	if len(planItems) < 15 {
		t.Fatalf("the plan names around twenty items across its four stages and its test plan; "+
			"this list has %d. Either the plan grew and the list did not, or the list was "+
			"emptied by accident — and an empty list makes both tests in this file pass "+
			"vacuously, which is the one outcome a gate must never have", len(planItems))
	}
	var withChecks int
	for _, item := range planItems {
		if len(item.checks) > 0 {
			withChecks++
		}
	}
	if withChecks == 0 {
		t.Fatal("no plan item carries a named check; the evidence half of this gate has " +
			"degenerated into \"a file exists\", which is the weakest evidence available")
	}
}

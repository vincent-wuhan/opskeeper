package dataguard

// This file is the honest answer to a question the vocabulary kept answering
// wrong: what does Data-Guard actually enforce today?
//
// The sensitivity constants and the compliance tag both promise controls in
// their own doc comments — a reader role, an override, two approvers, an
// audit-retention period injected into the sandbox. Those promises were the
// kind that survive for years precisely because they live in comments and in
// a JSON column: nothing reads `enforced: true` and changes its behaviour, and
// nothing reports that it did not. So they are written down here, one row per
// promise, with the status each one actually has today.
//
// The status vocabulary is three words, not two, because "the code exists" and
// "the code runs" are different facts and only two words cannot tell them
// apart:
//
//   - StatusEnforced  the control runs on some path a request can take.
//   - StatusInert     the control is written and wired to nothing. Tests reach
//     it; production does not. This is the state that reads as "implemented"
//     from inside the package and as "absent" from outside it.
//   - StatusDeclared  there is no code at all. The promise lives in a comment
//     and in a persisted flag.
//
// Only StatusEnforced may be described as enforced anywhere else in the tree.
// Everything else is a declaration the console may show and nothing may rely
// on.

// Status is what a promised control actually does today.
type Status string

const (
	// StatusEnforced means the control runs on a path a request can take.
	StatusEnforced Status = "enforced"
	// StatusInert means the control is implemented and wired to nothing:
	// only tests reach it.
	StatusInert Status = "inert"
	// StatusDeclared means the promise has no implementation at all.
	StatusDeclared Status = "declared"
)

// ControlClaim is one promise, and what is true about it today.
//
// Probe is the symbol whose presence or absence in non-test code decides the
// row's status, and it is required for every row except a pure declaration —
// a registry that asserts reachability without naming what it is asserting
// reachability of is a registry of opinions.
type ControlClaim struct {
	// ID is stable and is what the gate and the ledger refer to.
	ID string
	// Claim is the promise, in the words the vocabulary uses.
	Claim string
	// Status is what is true today.
	Status Status
	// Probe is the symbol that proves the status: named for enforced and
	// inert rows, empty for declared ones.
	Probe string
	// Note says what stands in the control's place while it is not enforced,
	// or why nothing does.
	Note string
}

// Claims is the registry. It is a function rather than a package variable so
// a caller cannot mutate the shared slice and hand itself a cleaner report.
func Claims() []ControlClaim {
	return []ControlClaim{
		{
			ID: "compliance.enforced-tag",
			Claim: "a compliance tag with Enforced=true injects hard constraints into the cmdpolicy " +
				"sandbox: encryption-at-rest, audit-log-retention-1y, audit-log-retention-6mo, " +
				"audit-log-retention-6y, mfa-on-write, geo-eu-only, phi-encryption, " +
				"access-control-rbac, incident-response-24h, subject-erasure, purpose-limitation, " +
				"data-minimization, minimum-necessary, change-management, access-review-quarterly " +
				"and logical-access-logging",
			Status: StatusDeclared,
			Note: "nothing reads the flag: the sandbox has no compliance notion, and " +
				"retention and encryption controls have no implementation to inject. The flag " +
				"is a label the console may display and nothing may rely on.",
		},
		{
			ID:     "compliance.control-catalog",
			Claim:  "the five frameworks ship a recommended control list",
			Status: StatusEnforced,
			Probe:  "ControlCatalog",
			Note: "the row was declared because the catalog function was called by this registry " +
				"and nothing else: a one-click list had no route behind it. Decision 366 added " +
				"GET /v1/data-guard/compliance/frameworks, and the reason it is not simply the " +
				"directory served as JSON is that **an unqualified catalog is the same lie in " +
				"an API costume** — and an API costume is harder to argue with than a comment. " +
				"Of the sixteen names the five frameworks recommend, fourteen appear nowhere " +
				"but the catalog function and this registry, one appears only in a form " +
				"round-trip test, and one only on the public site. So every entry the endpoint " +
				"returns carries the status this build actually has, computed from this " +
				"registry rather than from a second list written beside it: a control that is " +
				"recommended here and enforced nowhere says so in the same response that " +
				"offers it. The catalog's status is `declared` for all sixteen, which is what " +
				"the compliance.enforced-tag row says and what the endpoint now says too.",
		},
		{
			ID:     "sensitivity.escalates-severity",
			Claim:  "TopSecret and Restricted raise a call to dangerous, Confidential to mutating",
			Status: StatusEnforced,
			Probe:  "ClassFor",
			Note: "the row was inert because PausePolicyImpl implements the mapping and nothing " +
				"constructs it in production. Decision 363 replaced that with a path that runs: " +
				"approval.Propose looks the action's target up in the label store before the row " +
				"exists and stores the raised class, so the signatures a reviewer is asked for " +
				"are decided by the label rather than by the producer's risk_class. Two failure " +
				"directions are closed deliberately — a lookup that errors refuses to create the " +
				"row rather than defaulting to unlabelled, and a label can only raise a class, " +
				"so labelling a database Internal is not a way to buy a single signature. A call " +
				"reaching several resources is judged by the strictest label in the set. See also " +
				"the read half, which has run since decision 361.",
		},
		{
			ID:     "sensitivity.confidential-reader-role",
			Claim:  "Confidential data requires the confidential-reader role",
			Status: StatusEnforced,
			Probe:  "AllowWithSensitivity",
			Note: "every console tool call now passes a reader-tier gate before the tool runs. " +
				"cmd/opskeeper assembles it from the label store and the iam enforcer, and " +
				"decorators.WithSensitivity wraps the tool bag on both the coordinator and the " +
				"worker path, so a caller whose tier does not reach the resource's label is " +
				"refused by name. The row was inert until decision 361, which was the first " +
				"thing in the tree to call the check that had been written for it.",
		},
		{
			ID:     "sensitivity.restricted-reader-role",
			Claim:  "Restricted data requires the restricted-reader role",
			Status: StatusEnforced,
			Probe:  "AllowWithSensitivity",
			Note: "the same gate as the confidential row, and it is a read gate: it asks the " +
				"enforcer about the read action, which is all the tier table can answer. A call " +
				"naming several resources is decided by the strictest label in the set, so the " +
				"order the caller wrote the list in does not decide the answer.",
		},
		{
			ID:     "sensitivity.restricted-write-override",
			Claim:  "writing to a Restricted resource requires an override",
			Status: StatusDeclared,
			Note: "this half was split out of the restricted-reader row because the reader gate " +
				"that now runs does not implement it. The reader gate asks about the read " +
				"action, and an override is a grant that lets a write through anyway — which is " +
				"not what decision 363 added. That change raises an approval's class so the " +
				"write costs more signatures, a different control on a different side of the " +
				"queue, and it leaves the override this row names with no code behind it. A row " +
				"that said enforced because the escalation landed would be the exact lie this " +
				"registry exists to prevent, in the shape this registry was written to catch.",
		},
		{
			ID:     "redaction.claim-matches-content",
			Claim:  "an artifact is only marked redacted when the redactor would find nothing left to change",
			Status: StatusEnforced,
			Probe:  "isRedacted",
			Note: "决策 379。**这一行拆的是上一行的一半**：值侧「替换」仍然不接线，因为它要" +
				"吃掉事故号、端口、耗时和主机 IP，那是人要拍板的代价；而「这份工件可以声称自己" +
				"已脱敏」不是取舍，是对的。原来的判据是「正文里有没有 <redacted: 标记」——" +
				"而实测证明：一遍确实跑了（所以标记在），也确实没抓到散文里的邮箱与手机号。" +
				"于是 sink 把一份仍然带着个人信息的工件报成 redacted: true，**把一个没人验证过" +
				"的缺口，换成了一个看起来已经验证过的结论**。现在判据是「没有标记，或者" +
				"脱敏器还能改动什么」，并把第三种状态（脱敏跑了但没跑完）单列为 " +
				"redaction_partial，因为「没脱敏」与「脱敏了但漏了」该有的应对不一样。" +
				"**检测可以激进，替换不能**：检测只会让工件不敢声称自己干净，一个字都不会丢；" +
				"替换会毁掉文档。所以值侧匹配以窄形状（邮箱、11 位手机号）进的是检测这条路，" +
				"并有「不得对事故号/端口/耗时/版本号误报」的用例钉住——**一个会误报的检查，" +
				"是一个会被关掉的检查**。",
		},
		{
			ID:     "sensitivity.top-secret-read-gated",
			Claim:  "TopSecret data is readable by nobody",
			Status: StatusEnforced,
			Probe:  "AllowWithSensitivity",
			Note: "**这一行改写了它自己的措辞，因为原措辞从来不是真的。** TopSecret 不是" +
				"「无人可读」，而是「持有 topsecret_reader tier 的人可读」——TierForSensitivity " +
				"一直这么定义，决策 361 把它接到了每一次控制台工具调用上。改写成可实现的那句之后 " +
				"它是真的：读 TopSecret 需要 tier，而 grant 这个 tier 是一条独立的、有记录的 " +
				"管理动作。**「无人可读」不是被实现了，是被撤回了**——一个能兑现的弱承诺比一个 " +
				"兑现不了的强承诺有用。",
		},
		{
			ID:     "redaction.depth-by-sensitivity",
			Claim:  "how deeply a value is redacted is decided by the sensitivity of the resource it came from",
			Status: StatusDeclared,
			Note: "这一行的原措辞是「Redactor 的文档说生产代码在 cmd/main.go 里接 " +
				"NewRedactor(mode)」，而那句话是假的——决策 368 把它改成了它真正是的东西。" +
				"整条脱敏链今天没有被生产代码走到过：非测试代码里唯一构造 Redactor 的地方是 " +
				"postmortem.go 的 nil 兜底，而那个兜底是 RedactModeNone；它所属的 " +
				"NewPostmortemService 在测试之外也没有调用方（scripts/deadcode 早就记着这一条，" +
				"报告不闸门，所以它一直没红）。因此 Sensitivity→RedactMode 的映射没有任何生产 " +
				"消费者，本轮连同 ModeForSensitivity 与 NewRedactorForSensitivity 一起删除——" +
				"**留着它们只会让下一个人以为这条链已经接好了**。" +
				"写这一行不是为了给缺口留个位置，而是为了让「没有接线」这件事出现在一个会被 " +
				"读到的地方，而不是只出现在一个 git 历史里。" +
				"**这一行同时记下一个闸门洞**：scripts/deadcode 的 " +
				"TestNoSymbolClaimsProductionWiringWhileBeingUnreachableFromIt 只在符号" +
				"**不可达**时才报，而 Redactor 这个类型是可达的（postmortem.go 用它做参数），" +
				"于是「生产代码在 cmd/main.go 里构造它」这句假话一路绿灯。本轮试过给它补两条短语" +
				"（「from cmd/main.go」/「from main.go」），放回那句假注释重跑仍然是绿的——" +
				"**理由与符号是否可达有关，与措辞无关**。补短语是投机，投机性的闸门增强比" +
				"没有更坏，所以撤回了。真正的洞是：这道闸门分不清「可达」与「按注释说的那样接线」，" +
				"要补的是判据，不是词表。" +
				"**这一行后来又被实测收紧了一次，而收紧它的不是读代码，是跑代码**：" +
				"`GitArtifactSink.Save` 是每一份 postmortem 正文必经的唯一咽喉，它今天只**检查** " +
				"正文里有没有 `<redacted:` 标记来填 metadata，从不真的脱敏。把 Redactor 接进去" +
				"看起来是显然的修法，本轮拿一份真实形状的复盘正文实测过，结果否掉了它：" +
				"要求「敏感词 + 分隔符」的键值构造，而正文是散文。同一份正文里 " +
				"`api_key=sk-live-9f2a7c` 被脱敏，**而出现两次的 alice@example.com 与 " +
				"13800138000 一个都没动**——因为它们左边没有键。" +
				"更要命的是标记确实出现了，于是 sink 的标记检查会把这份仍然带着用户邮箱与手机号的" +
				"工件报成 `redacted: true`。**部分脱敏被标成已脱敏，比不脱敏更坏**：它把一个" +
				"没人验证过的缺口，换成了一个看起来已经验证过的结论。" +
				"所以本轮不接线。前置条件是给脱敏器补一个**值侧**匹配（不看键、只认值形状），" +
				"而那件事自带一个真实的代价需要人拍板：值侧匹配会吃掉合法的数字——事故号 " +
				"`PG-20261006-001`、端口、耗时、主机 IP 长得都和手机号一样。**这一行的状态是 " +
				"declared，而且它缺的不是一个调用方，是匹配能力。**",
		},
		{
			ID:     "approval.dual-sign",
			Claim:  "a destructive or cluster-scope approval needs two different administrators",
			Status: StatusEnforced,
			Probe:  "WithDualSignGate",
			Note: "决策 362 接上的。存储侧：approvals 行新增 signers_json，能放下 N 个签名人；" +
				"闸门侧：Sign 累积签名、缺口未补齐就保持 pending（HTTP 202），补齐才 Decide + 执行；" +
				"规则侧：policy/opskeeper/casbin/tenant_wide.json 从一份**匹配不到任何东西**的" +
				"配置（resource 写 tenant_wide 而没有一行审批是这么标的；role 写 opskeeper-admin " +
				"而系统里没有这个角色）改写成系统真实产出的词表。验证器本身也修了两个洞：按 " +
				"UserID 去重（此前同一个人签两次算数），以及真的去检查 rule.Role（此前该字段" +
				"从未被读过）。**仍有一处已知缺口**：生产者没有声明分类的行（目前是 mcp_call）" +
				"按单签放行，闸门不替它猜风险等级。",
		},
	}
}

// Claim returns the registry row with this id.
func Claim(id string) (ControlClaim, bool) {
	for _, claim := range Claims() {
		if claim.ID == id {
			return claim, true
		}
	}
	return ControlClaim{}, false
}

// DeclaredControls returns every control name the built-in frameworks mention
// without saying which of them this build enforces.
//
// The list is what an operator reading a label cannot tell apart, and it is
// exported so the gate that guards the registry is comparing against the same
// source the labels come from rather than a copy of it.
func DeclaredControls() []string {
	var out []string
	for _, framework := range AllFrameworks {
		out = append(out, DefaultFrameworkControls()[framework]...)
	}
	return out
}

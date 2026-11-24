# §6.5 安全审查收口报告

- 基线：`main@f4b76a36`（detached HEAD，`git status` 干净）
- 方法：一手读数。所有引用的文件与测试名在基线上重读，所有闸门与测试在干净检出上本次复跑。
- 缺口分档：**S 级 = 正式版阻塞**（生产默认配置下成立，且影响凭据泄露 / 越权执行 / 跨租户外泄）；**F 级 = 后续版本**（需错误配置、或影响面有限、或已有缓解）。

## 0. 复跑的命令与结果（全部本次在干净检出上执行）

| 命令 | 结果 |
|---|---|
| `python3 scripts/audit_open_source.py` | `open-source gate passed: 3551 text files audited in the tracked tree` |
| `bash scripts/plansecurity.sh` | 三组命名断言全部执行（fence 3 条 / node credential 1 条 / autonomy 3 条） |
| `make module-check` | `modulecheck: all module boundaries hold` |
| `make pi-plugin-guard` | `1 plugin(s) valid (upup)`；workspace 侧 `no Pi plugins declared; workspace contract ready` |
| `go test ./scripts/ledgercheck`（含 `TestTheTenantKeyDerivationsAreTheOnesTheLedgerRegisters`） | ok |
| `GOWORK=off go run ./scripts/routeaudit ../..`（scripts/routeaudit 内执行） | `audited: 138, settled exemption: 53, acknowledged gap: 0` |
| `core/base`: `go test ./pkg/promptguard/ ./pkg/secretbox/ ./pkg/audit/ ./pkg/auth/` | 全 ok |
| `core/manager`: `go test ./biz/aiops/tools/decorators/ ./biz/hitl/ ./biz/pigruntime/ ./dataguard/ ./domains… ./biz/approval/... ./server/approval/... ./server/chatdiagnose/ ./biz/marketplace/ ./server/imbridge/ ./biz/imbridge/...` | 全 ok |
| `core/domains`: `go test ./server/secret/ ./server/llmgw/` | ok |
| `core/edge`: `go test ./cmdpolicy/ ./autonomy/ ./policygate/` | 全 ok |
| `core/floor`: `go test ./pluginmanifest/ ./federation/` | ok |
| `go test -tags e2e ./tests/e2e/ -run 'TestCredentials_VaultAndApprovalSurface_HLD017|TestTheGatewayServesAStreamToANodeCredential'` | ok |
| `go test -tags e2e ./tests/e2e/ -run 'TestAuth_RBAC_ThreeTier_B3' -count=1 -v` | `--- PASS` |

## 1. Secret 管理

### 已覆盖

- **静态加密（vault）**：`core/base/pkg/secretbox/secretbox.go` — AES-256-GCM，密钥来自 `OPSKEEPER_SECRET_KEY`，密钥在 DB 外（DB dump 单独拿不到明文）。弱密钥有显式告警：`cmd/opskeeper/main.go:3689` 在 `KeyIsWeak()` 时打 WARN（未 unset 才用内置回退密钥）。`secretbox_test.go` 往返测试。
- **凭据库写路由不入审计明文**：`core/domains/server/secret/audit_test.go` `TestTheChainNeverHoldsTheCredential`（序列化整个事件后找明文，而非逐字段），`TestTheDigestAnswersWasItRotated` / `TestTheDigestRespectsFieldBoundaries`（摘要可回答轮换且尊重字段边界），`TestUpdateAndDeleteAreAudited`、`TestANonAdminWriteLeavesAFailureRow`。审计只落 `field_names` + `fields_digest`。
- **节点零凭据**：provider 密钥不落节点——`core/domains/server/llmgw` 的 `TestANodesCredentialCannotDriveAnotherNodesInference`（plansecurity 断言之一）；E2E `tests/e2e/node_agent_delivery_test.go` 用 sentinel `sk-decoy-openai-must-not-reach-a-node` 断言密钥不到节点进程。
- **仓库内容零秘密**：`scripts/audit_open_source.py` 的 `FORBIDDEN_PATTERNS` 覆盖 `ghp_`/`sk-ant-`/`sk-`/`AKIA`/私钥块/带凭据 URL/内网 IP/私路径/内部任务号；`DECOY_CREDENTIALS` 仅按精确串豁免哨兵值。配套 `tests/test_audit_open_source.py`。CI 有 `.github/workflows/audit-open-source.yml`。
- **manifest 拒绝凭据字段**：`sdk.Decode` 的 `KnownFields(true)` 使插件包无法声明密钥（provider-plugin-plan §6.1 的 I-2 不变量，本次核对 `sdk/manifest.go` 属实）。
- **PiG 子进程零凭据继承**：`core/manager/biz/pigruntime/sandbox_enforcement_test.go` `TestProcessDriverEnvironmentDropsInjectedHostSecrets` + `TestProcessDriverRealSubprocessSeesOnlyPIGEnvironment`——`command.Env` 从零构造（非 append `os.Environ()`），宿主注入的 `DB_PASSWORD`/`AWS_SECRET_ACCESS_KEY`/`VAULT_TOKEN` 等不进子进程。
- **edge 凭据文件落盘权限**：`core/edge/plugins/databasemetrics/secrets.go:543` `Chmod(0o600)`，且一次性写 RPC 只接受 secrets 目录下路径（不能变成通用文件写原语）。
- **MCP server 注册的凭据是「vault 名」而非值**：`core/manager/model/mcp/model.go` — `Credential` 字段存凭据库名，`HeaderTemplateJSON` 用 `{{field}}` 占位符在调用时解析。值本身不进 mcp_servers 表。

### 缺口

- **F-1（后续）`chat_tool_calls.arguments_json / result_json` 落原文。** `core/manager/biz/aiops/agent/agent.go:491`（`ArgumentsJSON: string(rawArgs)`）与 `agentkernel/persist.go:230`（`ArgumentsJSON: string(rec.Args)`）直接落库。模型在 args 里塞进敏感值（用户贴的口令、日志里带出的 DSN）会原样持久化。对照：MCP 面（`server/mcp/audit_sink.go`）与 agent.tool 上呼面（`cmd/opskeeper/agenttool_audit.go` 的 `hashAgentToolValue`）都只落 SHA-256；PiG 网关面（`runtimemgmt_pig_tool_calls`）也只落哈希——chat 面是唯一落原文的面。**建议**：短期加 `dataguard.Redactor`（`RedactModeAll`）入参出参再落库；长期对齐哈希化（该表同时承担 UI 回放，需求不同，需单独裁决）。归后续版本的理由：该表为 admin-only 读面 + 平台自持 DB，非默认外泄路径。
- **F-2（后续）`OPSKEEPER_SECRET_KEY` 弱密钥只 WARN 不拒启。** 对照 `OPSKEEPER_JWT_SECRET` 用默认值时 `os.Exit(1)`（`cmd/opskeeper/main.go:511-512`），secretbox 弱密钥只打日志。`deploy/install/docker-compose.yml:73` 把它列为可空默认。**建议**：对齐 JWT 的 fail-closed，或至少在 compose 里 `:?` 必填。

## 2. 日志脱敏

### 已覆盖

- **redaction 原语**：`core/manager/dataguard/redact.go` — 13 个敏感字段名（password/token/api_key/secret/email/phone/credit_card/ssn/id_card/passport/tax_id 等）case-insensitive 子串匹配，per-sensitivity 三档（none/summary/all），TopSecret 额外剥数字；幂等性是显式设计（`ResidualSensitive` 的注释记录了曾经的翻倍 bug）。测试 `redact_test.go` 五组（`TestRedactor_RedactFieldName` / `_NoneMode_NoOp` / `_AllMode_KeyValueRedaction` / `_SummaryMode_StableHash` / `_TopSecret_StripsDigits`）。
- **聊天诊断正文不进审计链**：`core/manager/server/chatdiagnose/audit_test.go` `TestDiagnose_MessageTextNeverReachesTheChain`——用 `ghp_…` 形态的串断言提问正文不出现在审计行，行上只有 `message_len` + `message_digest`。
- **MCP / agent.tool 审计只落哈希**：`server/mcp/audit_sink.go` 与 `cmd/opskeeper/agenttool_audit.go`，注释明说理由（「a middleware tool's result is a slice of a customer's database… the fingerprint is what lets two rows be compared」）。
- **postmortem 出口有残敏检查**：`core/manager/biz/report/postmortem_sink.go` `residualRedactor()` nil-safe 回退 `RedactModeAll`，落库前跑 `ResidualSensitive`。

### 缺口

- **F-3（后续）redaction 的生产消费面窄。** `dataguard.Redactor` 的非测试消费方目前只有 postmortem sink 一处（`redact.go` 头注自己也承认：`NewPostmortemService` 测试外无调用方，`dataguard/enforcement.go:188` 把这条记进了 Claims 登记表——防线对此是诚实的）。日志输出层（slog 路径）没有统一 redaction 中间件。**建议**：给 slog 加一个 `dataguard` handler（按字段名模式替换），或至少给已知携带用户输入的 logger 点位加显式脱敏。与 F-1 同源，可一并处理。
- **F-4（后续）PiG 网关错误消息进审计行的截断不脱敏。** `gateway.go:320-336` `recordEnd` 把 `err.Error()` 截 512 字节落 `ErrorMessage`。错误串里若嵌了参数值（连接串错误常见）会带出。**建议**：截断前过一遍 `RedactString`。

## 3. 越权（认证与授权）

### 已覆盖

- **JWT 认证中间件**：`core/base/pkg/auth/middleware.go` — Bearer/query token，验签失败 401 不进 handler；AgentTeams service token 限定 `/v1/mcp` 路径；旧 token 的 `Role=="admin"` 显式升级为 superuser 有注释说明升级语义。`jwt_test.go` + `middleware_test.go`。
- **JWT 弱密钥拒启**：`cmd/opskeeper/main.go:510-512` — 内置默认值直接 `os.Exit(1)`。
- **变更面路由 admin 闸**：`hitl/http.go`、`dataguard/http.go`、`mcp/http.go`、`marketplace/http.go`、`imbridge/http.go` 均有各自 `requireAdmin`（模式一致：`tenantctx.From` + `Role != "admin" && !IsSuperuser` → 403）。marketplace 的 install/uninstall/import/upload 四写路由 + secret reveal 走 admin；`revealAppSecret` 对**非 admin 的尝试也入账**（`imbridge/http.go:541-545` 注释明说「没有权限的人试图读出明文密钥是最该被看见的一行」）。
- **webshell 有独立 authz 面**：`server/webshell/http.go` — `authz.Require("device:shell", "exec"/"read")` 按Casbin 对象细分。
- **RBAC E2E**：`tests/e2e/auth_rbac_test.go` `TestAuth_RBAC_ThreeTier_B3`（真实登录 + 三层角色断言，本次复跑 PASS，249s）。
- **AgentTeams 角色工具白名单**：`server/mcp/middleware/auth.go` — service token 校验 `AllowedTools` 非空且每条都被 `AgentTeamsRoleAllows(role, tool)` 允许，否则 403。
- **敏感度闸**：`biz/aiops/tools/decorators/sensitivity.go` — 每次工具调用前问 `ports.SensitivityGate`（IAM reader tier vs 资源敏感度），生产装配在 `chatruntime/worker.go:576`、`runtime.go:708`、`cmd/opskeeper/main.go:3534/5988`。
- **升级路径独立**：`pig/pigrpc/promptguard.go` 的 `sanitizePrompt` 防节点代理把控制面文本当 slash 命令执行（`TestSanitizePromptNeutralisesOnlyALeadingSlash` 等 5 条）。

### 缺口

- **S-2（正式版阻塞）IM 入站消息以超管身份运行。** `cmd/opskeeper/main.go:2331` `const imBridgeServiceUserID uint64 = 1`，注释自认「superuser admin (id=1 on every install thanks to bootstrap)」，且「S3 will replace that with per-IM-user binding」——S3 未落地。后果：任何能向已配置 IM bot 发消息的人（飞书群任何成员、Telegram/Slack 白名单内用户）触发的 agent 会话以 superuser 身份创建与执行，绕过 RBAC 三层模型。Telegram/Slack 有 sender 白名单缓解（见 §4），但**飞书群聊内任何成员**（含外部协作成员，若被拉群）都能用该身份。**建议（最小整改）**：为 IM 桥建一个专用非特权 service user（如 `imbridge-svc`，普通 user 角色），会话归属它而非超管；per-IM-user 绑定仍留 S3。这不需要动 per-user 映射，只改一个常量与 bootstrap 一行。

## 4. 跨租户

### 已覆盖

- **租户推导单一登记**：`scripts/ledgercheck/tenantkey_test.go` `TestTheTenantKeyDerivationsAreTheOnesTheLedgerRegisters`——把「`tenantFromContext` 生产定义共 N 处」变成台账可持有的断言（第三处出现会被闸门抓住，gitartifact 的 uint64 版已如实登记）。本次复跑 PASS。
- **PiG 网关 tenant 硬闸**：`biz/pigruntime/gateway.go:353-356` — `request.Scope.TenantID == "" || g.tenantID != request.Scope.TenantID` → `DenyReasonTenantMismatch`，测试 `TestGatewayDeniesTenantMismatch`。
- **MCP AgentTeams 租户一致性**：`server/mcp/middleware/auth.go:399-401` — 请求租户与 token 租户不一致拒；`TestAuthenticator_RejectsAgentTeamsTenantMismatch`。
- **数据层租户隔离测试**：`data/chatdiagnose/store/conversation_repo_test.go`（`GetConversationForTenant` 跨租户 → `ErrConversationTenantMismatch`）、`data/loop/store/event_repo_test.go` `TestContractRepoDB_ReadContractByID_TenantIsolation`（双向反例：T1 读 T2 静默 nil、T2 读 T1 同拒）。
- **marketplace 存储租户列**：`data/marketplace/store/repo.go` 全部查询带 `tenant_id = ?` 谓词（Create 唯一键是 `PackID + TenantID`）。
- **tenant_bind 装饰器**：`decorators/tenant_bind.go` — 从 ctx 注入 tenant_id 进 args，链序在 audit/ratelimit 之外层。`TestTenantBind_InjectsTenantID` / `_DoesNotOverwriteExisting` / `_NoTenantOnCtxIsPassThrough` / `_SkipsWhenSchemaLacksTenantID` / `_UsesAgentTeamsTenant`。
- **IM 公开可达 bot 的白名单**：Telegram 与 Slack 强制非空 `allow_from`（`biz/imbridge/usecase.go:134-180`，空表单直接校验错误），运行期 `provider/telegram/stream.go:105` 与 `provider/slack/stream.go:229` 对非白名单 sender 静默丢弃。`TestValidateTelegramRequiresAllowFrom` / `TestValidateSlackRequiresAllowFrom` / `TestParseAllowFrom`。

### 缺口

- **S-2 的跨租户面**（同 §3）：IM 会话归属超管用户，意味着 IM 侧不存在按 IM 用户隔离的租户边界——群内所有人共享同一个 superuser 会话（`biz/imbridge/bridge.go` 会话键是 `(im_app_id, chat_id, thread_id)`，无用户维度）。修复 S-2 后该面收敛为「群内共享一个受限 service user」，仍非 per-user，但至少不再叠加超管权限。
- **F-5（后续）单租户 MVP 的兜底身份。** `server/loop/http.go:537` `tenantFromContext` 在 `UserID==0` 时返回 `"default"`，注释自认「Single-tenant MVP… Day 6+ will replace with the real multi-tenant resolver」。runtimemgmt 的 `tenantFromRequest`（`tasks_http.go:497-504`）同型。真实多租户启用前这是已知设计债，台账已登记（tenantkey 闸门正是为它存在的）。**建议**：多租户启用时把 `"default"` 兜底改为显式拒绝。
- **F-6（后续）飞书无 sender 白名单。** 模型注释写「Unused by Feishu/DingTalk, which are gated by enterprise-tenant membership」——但 webhook/事件路径上平台并不校验「发消息的人在飞书企业内是否对应本系统租户」，企业成员资格 ≠ 本系统授权。飞书群被拉入外部成员时该假设失效。**建议**：给 Feishu 也接 `allow_from`（按 open_id），或至少文档化这个信任边界。

## 5. Prompt injection

### 已覆盖

- **promptguard 围栏原语**：`core/base/pkg/promptguard/promptguard.go` — nonce 闭合标记（每次渲染随机 id，攻击者无法闭合未见的块）+ 标记转义双层防御；`Instruction()` 与 `Tag` 常量同源生成（模型被告知的 tag 即它将看到的 tag）。测试含 `TestTheFenceCannotReachThePlatform`（对抗样本不能伪造闭合）。
- **不可信工具输出围栏（闭集表驱动）**：`biz/aiops/tools/untrusted_sources.go` — 17 个工具名 → kind 映射表，键是工具包自己的名字常量（重命名即编译失败）。`untrusted_sources_test.go` `TestEveryNameInTheTableIsFencedInTheShippedBag` 双向钉住（表内有名必被围栏、被围栏必在表内）+ `TestTheShippedBagFencesRatherThanJustWraps`。装饰器 `decorators/untrusted.go` 是最内层包裹——audit/metric/console 看到的就是模型看到的（围栏外应用会让审计记录模型没见过的正文）。
- **调查 worker 的系统提示注入 Instruction**：`biz/loop/investigated_worker.go:658` — 同一 prompt 内 correlated group / evidence chain / remediation options 三块全部 `fence.Fence(...)` 包裹。
- **对抗闭合测试**：`decorators/untrusted_test.go` `TestAnAdversarialResultCannotCloseTheFence`。
- **升级命令净化**：`core/pig/pigrpc/promptguard.go` — 节点代理 `prompt` 命令不被当 slash 命令执行；重写发生时在**同一事件流**上发 `opskeeper_prompt_rewritten` 帧（操作者与审计都能看到，不是只在日志里）。

### 缺口

- **F-7（后续）chat 主链路（agent kernel / chatruntime）的系统提示未带 Instruction。** `Instruction()` 的唯一生产调用方是 `biz/loop/investigated_worker.go:658`（investigator 摘要 worker）。`chatruntime/system_prompt.go` 的 `ComposeSystemPrompt` 拼persona/skills/路由提示但不拼 `promptguard.Instruction()`。虽然工具输出已被装饰器围栏（模型看到 fence 标记），但系统提示没告诉模型这些标记的含义——围栏的语义学一半缺位。**建议**：`ComposeSystemPrompt` 追加 `promptguard.Instruction()` 一行（该函数设计上就是为此导出的）。归后续的理由：围栏 + 转义本身已构成主要防线，Instruction 是语义加固。
- **F-8（后续）`web_search` 输出不在围栏表内。** `untrusted_outputs` 表覆盖 Loki/Tempo/告警/仓库/知识/Bash/host 文件，但 `web_search`（`core/floor/skill/builtin/web_search.go`，经 `RegisterSafeSkills` 进 registry）不在表内——而公网搜索结果是典型的外部可写文本。缓解：默认 `WebSearchEnabled=false`（`agent/agent.go:196`，需 SPA 显式开），且双闸（schema 过滤 + dispatch 本地拒绝，`agent.go:506`）。**建议**：把 `web_search` 加进 `untrusted_outputs`（kind=`tool`）并在 `untrusted_sources_test.go` 的必检清单同步。这是表驱动设计下的一行改动。

## 6. 审批绕过

### 已覆盖

- **双签策略与闸门**：`biz/hitl/dual_sign.go` — ADR-019 实现。按 UserID 去重（`TestValidate_DualSign_SameUserTwiceIsOneSigner`）、真的读 `rule.Role`（历史洞已修）、角色组覆盖校验（`_SameRoleGroup` / `_SameUserDifferentRole`）。装配在 `cmd/opskeeper/dualsigngate.go`：`scopeKeys` 取 risk_class / blast_radius / kind 三键并集，全部满足才放行；未分类行如实登记为单签（`TestAnUnclassifiedRowStillDecidesOnOneSignature`——诚实记录而非伪装成有闸）。行形状测试 `dualsign_rowshape_test.go`。
- **单签不执行 / 双人执行 / 拒绝后续签**：`server/approval/dualsign_test.go` `TestOneSignatureOnADestructiveRowRunsNothing` / `TestTwoDistinctSignersRunTheAction` / `TestOnePersonSigningTwiceDoesNotRunTheAction` / `TestARejectedRowRefusesFurtherSignatures`。
- **禁用路径可见**：`dualsigngate.go:52-63` — 显式 opt-out 每次放行都打 WARN（不是 boot 时一行）。
- **PiG 只读硬闸不进审批**：`biz/pigruntime/gateway.go:376-382` — `ReadOnly=true` 时 write/destructive 直接拒，**不产生待审批项**（只读发布里的变更请求本身就是策略违规）；`TestReadOnlyHardGateProducesNoProposal` 断言 proposal sink 零写入。可写发布走 `AdmitProposalRequired` → `chat_mutating_proposals` pending → 既有 exact-payload 人审路径，网关不内置执行（`TestProposalOnlyPathForWriteUnderMutableRelease`）。
- **恢复执行绑定已批准提案（精确载荷）**：`data/hitl/store/recovery_audit.go` `ReserveApprovedForRecovery` — 七维全等（Kind/Action/Resource/SessionID/IncidentID/Execution 参数 canonical JSON 字节比对）+ 过期拒 + 5 分钟租约。
- **审批执行幂等防重放**：edge `policygate` 三断言（plansecurity 闸门固化）：`TestTheSameCallSubmittedEightTimesIsOneQuestionAndOneExecution`（8 次提交 = 1 问 1 执行）、`TestAGrantIsCollectableInsideItsLeaseAndNotAfterIt`（租约外不可收）、`TestASecondMutatingCallInOneConversationWaitsRatherThanQueuing`。gate_test 另有跨调用授权拒绝（`TestAGrantForADifferentCallDoesNotAuthoriseThisOne`）、无 digest 授权拒（`_WithNoDigestIsRefused`）等 16 条。
- **旧内核变更工具按名拒绝**：`biz/aiops/agent/agent.go:232` `legacyKernelMutatingTools` deny-list（当前 `host_restart_service` 一项）——旧闭包内核没有 ReviewGate 装饰链，按名拒绝是显式补偿；PiG 内核（生产默认）走完整装饰链。

### 缺口

- **F-9（后续）`legacyKernelMutatingTools` 是名单不是机制。** 注释自认「every PR-N addition of a write/destructive tool MUST add its wire name here」——新增变更工具忘了登记就是旁路。无测试钉住「每个 Class=write/destructive 的注册工具都在名单或都已过 ReviewGate」。**建议**：加一条 registry 级测试遍历所有注册工具，断言 write/destructive 类要么在名单、要么确认运行在带 ReviewGate 的内核上；或直接下线旧内核。归后续：PiG 内核是生产默认（`OPSKEEPER_AGENT_KERNEL=pig`），旧内核是遗留路径。
- **未分类行的单签放行是已登记的设计选择**（非缺口，但值得正式版知晓）：`dualsigngate.go` 明确写了「producer did not say how far this action reaches」时单签放行的理由（发明风险等级比记录缺口更糟）。生产端应确保 destructive 写动作的生产者都填 risk_class/blast_radius——建议给这两个字段加生产者侧必填校验。

## 7. 审计缺失

### 已覆盖

- **变更路由审计全覆盖（裁决表机制）**：`scripts/routeaudit` 把「core/manager/server 等五个根下每条变更路由要么在有 SetAuditEvent 的文件里、要么带书面豁免理由」固化成闸门。本次复跑：`verdicts recorded: 191, of which backlog: 53`，`audited: 138, settled exemption: 53, acknowledged gap: 0`；stale/orphaned 裁决都会被报出（表不能对树撒谎）。豁免理由本身可审计（如 `/v1/im/feishu/events` 的「inbound webhook authenticated by platform signature rather than by a tenant, so a failure row would name nobody」）。
- **PiG 工具调用双证据面**：`biz/pigruntime/gateway.go` — 每次裁决落 `runtimemgmt_pig_tool_calls` 表行 + `ActionPigToolCall` 审计行，两证据面独立（`recordDenied` 注释：表写失败时审计行仍发，「审计行是表不可写时的最后防线」）。denied 也落表（失败闭合矩阵的持久化断言）。`gateway_fail_closed_test.go` 七类矩阵 + `TestGatewayFailsClosedWhenLedgerUnwritable` / `TestGatewayConstructionFailsClosed`。
- **agent.tool 上呼审计**：`cmd/opskeeper/agenttool_audit.go` — 每次上呼一行，denied 与 ran-and-failed 分开记（「node probing for a write tool」不应看起来像「database query broke」），参数/结果只落 SHA-256。
- **MCP 面审计**：`server/mcp/audit_sink.go` — pending/success 成对行 + arguments/results 哈希。
- **凭据库、聊天诊断、IM app 增删改、secret reveal、飞书入站**均有专门审计测试（见 §1/§2 引用的测试名）。
- **runtime task 七动词审计**：`server/runtimemgmt/tasks_http.go` — create/claim/start/result/failure/cancel/recover 全动词入账，三条高频存活路由（lease-renewal/checkpoint/progress）的「settled 无行」裁决写进 routeaudit 表且有理由。
- **升级路径审计可见**：`pig/pigrpc/promptguard.go` 的重写通知是流上的真实帧而非日志行（保留策略未约定的地方不留证据）。

### 缺口

- **F-10（后续）MCP server 注册/更新路由在豁免表而非审计。** `scripts/routeaudit/main.go:478` 起：`/v1/mcp/servers` 的 Backlog 理由是「MCP server registration carries credentials; decision 311 found this table had wrongly claimed it was audited」。即：注册一个携带凭据引用的 MCP server 是无审计行的（admin 闸有，审计行无）。**建议**：给 create/update/delete 加审计行（载荷只落 name/transport/endpoint + credential 名，不落值——与 secret 域同型）。这是 53 条 backlog 里最值得转正的一条。
- **chat 面审计的原文落库**与 F-1 同条（见 §1），不重复登记。

## 8. 网络出口

### 已覆盖

- **PiG 子进程网络闭集**：`biz/pigruntime/process_driver.go:281-285` — `SandboxProfile.AllowedNetworkHosts` 以 `PIG_ALLOWED_HOSTS` 下发，**空列表下发空值**（「无边界声明」与「无网络」同义，不允许把空解释成不受限）；`TestProcessDriverEnvironmentCarriesAllowedHosts` / `_OmitsAllowedHostsWhenUnset`。
- **PiG 沙箱四禁 + 只读**：`biz/pigruntime/sandbox.go` `Validate()` — `NoAmbientDiscovery/NoProviderOverride/NoHostWrite/NoDirectCredential/ReadOnly` 五项全真才过；`config.go:33` `Validate()` 同样把五个开关为真当非法契约。装配双闸：`cmd/opskeeper/pigruntime_wiring.go` 默认零环境变量零行为，显式启用但工件三元组（路径+SHA-256+RuntimeID）缺失或摘要漂移 fail closed（启动错误不降级）。`TestProcessDriverLaunchRevalidatesSandbox` / `_RevalidatesConfig`。
- **edge 命令执行网络白名单**：`core/edge/cmdpolicy/sandbox.go:134` — ClassNetwork 二进制（curl/ping/dig 等）解析目标 host 后必须命中 allowlist，**空表 = 全拒**；条目支持 CIDR / 主机名后缀 / 字面量。`TestSandbox_NetworkHostAllowlist` / `_CIDR` / `_Suffix`。
- **子进程环境净化**：cmdpolicy 管道每个 child 用净化 env（仅 PATH/LANG/LC_ALL）——凭据不经环境进 shell。
- **autonomy 三断言（plansecurity 固化）**：`TestTheRunnerIsGivenTheDeclaredArgvAndNotTheClaimed`（argv 篡改）、`TestOnlyDeclaredRadiiRun`（越权半径）、`TestTheCeilingIsEnforcedHereAndNotOnlyAtAdmission`（上限在执行处强制而非只在准入处）。
- **节点凭据隔离**：`TestANodesCredentialCannotDriveAnotherNodesInference`（llmgw）。
- **联邦策略防回滚重放**：`core/floor/federation` — 版本号 = 决策序号（单调），Apply 拒绝已见版本（含已拒绝的），签名包永不过期也因此不构成回滚武器；`TestAReplayedBundleCannotRollAClusterBack`。
- **插件树签名**：`core/floor/pluginmanifest/signing.go` — ed25519 整树签名（manifest 与源码不可分离），KeyID 解析而非随包携带公钥（自验自的包不存在）；domain 前缀防跨用途重放。七条测试含 `TestAModifiedFileIsCaughtByTheSignature` / `_ModifiedManifest_` / `_AddedFile_` / `_RemovedFile_` / `_SignedByAnUntrustedKey_` / `_RevokedKey_`。
- **marketplace 源白名单 + 官方源强制签名 + 公钥钉扎**：`biz/marketplace/usecase.go` `checkSourceAllowed`（DevMode 关闭时源必须在 `AllowedSources`，生产装配 `["opskeeper-official","local"]`）；`RequireSignedSources=["opskeeper-official"]`（官方源无有效签名硬拒）；`SignaturePinnedKey`（`OPSKEEPER_MARKETPLACE_PINNED_PUBKEY`，DER 级比对）。签名测试九条（`TestVerifySignature_Happy/_Missing/_Tampered/_MalformedManifest/_MalformedPubKey/_PinnedKeyMismatch` 等）。`TestInstall_SourceAllowlist` / `TestInstall_UnsignedSourceNotGated`。
- **入站 webhook 有令牌常量时间比对**：`server/alert/http.go:536` `subtle.ConstantTimeCompare` + 1MB body 上限 + 双重 JSON 解析防尾随内容。
- **git clone 无 shell 注入面**：`biz/marketplace/usecase.go:718` `exec.CommandContext(gitCmd, args...)` 参数数组直传（无 shell 解释），`expandShorthandGitURL` 对无 scheme 无 @ 的 `owner/repo` 收敛到 `https://github.com/...`。

### 缺口

- **S-1（正式版阻塞）marketplace 生产默认 DevMode=true，源白名单与签名闸整体旁路。** `cmd/opskeeper/main.go:3148-3149`：`mpDevMode := …=="true" || os.Getenv(…DEVMODE) == ""` —— **环境变量未设置（空）时 DevMode 为 true**。`deploy/install/docker-compose.yml` 与 `install.sh` 全文无 `OPSKEEPER_MARKETPLACE_DEVMODE` 设置（本次 grep 确认），故生产部署默认走 DevMode：`checkSourceAllowed` 直接 return nil（任意 git/tarball URL 的源都放行），`requiresSigned` 检查也被跳过。marketplace install 是 admin-only（这层闸在），但「任意 URL 的 git clone + tarball 解包 + 技能装载」在默认部署下对 admin 是无源约束的——一个被钓鱼的 admin 或被劫持的 admin 会话可以把任意远端内容装进技能目录（技能 PromptBody 会进系统提示，即 prompt injection 的直通面）。归 S 级理由：生产默认即成立 + 影响面是任意内容进入受信任执行面。**建议（最小整改）**：把空值默认翻转为 false（DevMode 必须显式 `=true` 才开），或至少在 compose/install.sh 里显式设 `OPSKEEPER_MARKETPLACE_DEVMODE=false` 并把 `OPSKEEPER_MARKETPLACE_PINNED_PUBKEY` 写进部署文档。翻转默认值需检查仓内测试是否依赖空值=dev（`usecase_test.go` 有显式设 DevMode 的用例，风险可控）。
- **S-3（正式版阻塞）飞书明文事件模式无任何校验。** `server/imbridge/http.go:161-174`：签名校验**仅在 `app.EncryptKey != ""` 时执行**；未配 encrypt_key 的 app（明文模式）下，任何能到达该端点的人都可以伪造 `im.message.receive_v1` 事件。payload 里的 `header.token`（verify_token）**从未被校验**——模型有 `VerifyToken` 字段、API 有 reveal 端点能读它，但事件路径不用它（grep 全文无一处对入站 token 做比对；`routeaudit` 的 backlog 理由「audited by delivery」也没有兑现为代码）。结合 S-2（超管身份），伪造事件 = 以超管身份驱动 agent 会话。归 S 级理由：明文模式是飞书文档认可的模式（代码注释自认「weaker security, but matches what Feishu docs call non-encrypted mode」）、生产无强制 encrypt_key、影响面是越权执行。**建议（最小整改）**：明文模式下强制校验 payload `header.token` == `app.VerifyToken`（字段已存在，一行比对 + 一条测试）；或在应用配置校验里强制 `EncryptKey` 非空（对齐 Telegram/Slack 的 allow_from 必填模式）。
- **F-11（后续）manager 侧出网无统一出口策略。** 出网点位盘点（本次 grep `http.NewRequest|http.Get`）：demo（2 处，demo only）、recovery/host fixture client（测试夹具）、marketplace（受源闸管辖，但见 S-1）、imbridge provider 出站（飞书/Slack/Telegram API，正常业务）、`web_search`（SearXNG/Tavily/Brave，端点为编译期常量或配置覆写）。没有统一的 egress allowlist/代理层（对比 PiG 的 `PIG_ALLOWED_HOSTS` 与 edge 的 `NetworkHostAllowlist`——**这两个闭集机制都不覆盖 manager 进程自身**）。`web_search` 的 provider 端点虽是常量但出网无边界声明，DNS 重绑定/内网地址无检查（无 SSRF 防护——`web_search` 与 marketplace 下载都接受 URL 且不拒私网地址）。**建议**：给 manager 加统一 egress 配置（env 驱动的 host allowlist，默认放行业务端点），marketplace 下载与 web_search 的 URL 解析后拒私网/环回地址（`net.IP.IsPrivate/IsLoopback`）。归后续：出网点位均需 admin 配置或编译期常量，非默认任意出网。
- **F-12（后续）`.env.example` 未列安全开关。** `OPSKEEPER_MARKETPLACE_DEVMODE / _REQUIRE_SIGNED_SOURCES / _PINNED_PUBKEY / _REGISTRIES`、`OPSKEEPER_PIG_RUNTIME_*` 四元组、`OPSKEEPER_SECRET_KEY` 都不在 `.env.example`（164 行内 grep 无命中）——S-1 的默认翻转如果做了，运维没有文档化的开启路径。**建议**：与 S-1 一并处理。

## 9. 汇总

| # | 条目 | 结论 | 关键缺口 |
|---|---|---|---|
| 1 | secret 管理 | 大部分覆盖 | F-1 chat 面落原文、F-2 弱密钥不拒启 |
| 2 | 日志脱敏 | 原语齐、消费面窄 | F-3 生产消费面、F-4 错误串截断 |
| 3 | 越权 | 覆盖良好 | **S-2 IM 超管身份** |
| 4 | 跨租户 | 推导闸门 + 数据层测试齐 | S-2 的 IM 面、F-5 default 兜底、F-6 飞书无白名单 |
| 5 | prompt injection | 围栏机制完整 | F-7 主链 Instruction 缺、F-8 web_search 不在表 |
| 6 | 审批绕过 | 双签 + 硬闸 + 幂等齐 | F-9 旧内核名单机制 |
| 7 | 审计缺失 | 路由裁决表机制强 | F-10 MCP 注册路由无审计行 |
| 8 | 网络出口 | PiG/edge 闭集完整 | **S-1 marketplace 默认 DevMode**、**S-3 飞书明文无校验**、F-11 manager 无统一出口、F-12 env 文档缺 |

**正式版阻塞（S 级，3 项）**：S-1、S-2、S-3（均给出最小整改路径，合计改动量小：一个默认值翻转 + 一个常量 + 一行 token 比对，各自补一条测试）。

**后续版本（F 级，12 项中 9 项独立 + 3 项与 S 同源）**：按各节建议排期；其中 F-1/F-3（redaction 消费面）与 F-11（manager egress）建议合并成两个独立 change。

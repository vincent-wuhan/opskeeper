# v2026.11.0 安全审查（§6.5，2026-11-24）

八项逐条给出「已覆盖的闸门/测试证据」或「缺口与整改建议」，基线 main@f4b76a36（与发版同源）。分级：**A＝正式版阻塞**、**B＝后续版本整改**。本轮结论：无 A 级缺口，B 级六项。

## 1. secret 管理

**已覆盖**：
- 隧道凭据对集中存储、哈希落库，轮换走 `UpdateSecretHash`（不触碰明文口令）；发布验证用的管理员 token 由 compose 内 JWT secret 现场签发、十分钟有效期、不落盘（部署记录 §部署执行）。
- 节点侧凭据链（决策 93）：`OPSKEEPER_EDGE_AGENT_CONFIG_DIR` + `models.json` 的 `$VAR` 引用，节点磁盘无云厂商密钥（e2e 断言）；`DefaultAgentDir()` 相对路径缺陷已修（凭据不再落进签名插件内容）。
- `audit_open_source` 拒绝 GitHub/OpenAI/Anthropic/AWS 令牌形状与私钥块；诱饵凭据按**精确匹配**豁免且豁免理由成文（不是放宽模式）。
- 密文与密钥推导：`scripts/ledgercheck/tenantkey_test.go` 把租户密钥推导钉成闸门。

**B 级缺口**：
- **B-1（生产配置，非仓内）**：生产 nginx 备份与 teams 侧 vhost 存有硬编码 `Authorization: Bearer <JWT>` 的只读代理 location。该 token `exp=2026-10-06` **已过期**、location 实测不再匹配（404 来自 manager 整站代理），不构成活跃暴露。整改：删除这些 location 与配置中的 JWT 文本，改调用方自带 token 透传。
- **B-2**：manager 的 `OPSKEEPER_JWT_SECRET` 为 compose 静态环境变量，无轮换流程（隧道凭据有 `UpdateSecretHash`，JWT 没有）。整改：纳入与隧道凭据同形的轮换路径。

## 2. 日志脱敏

**已覆盖**：
- `core/domains/server/secret/audit_test.go`：存储密文的**每个字节**不进审计链；`core/manager/server/chatdiagnose/audit_test.go`：用户问题文本不携凭据（两者都有 reflect 递归版断言，决策 322）。
- PiG 任务终态三态脱敏（applied / skipped+notes / 拒绝持久化），`biz/pigruntime/lifecycle_sink_test.go` 与 `contracts_test.go` 钉住；治理网关的参数与结果**只落 SHA-256**（3.3），调用证据表无明文参数。
- decoy 凭据哨兵测试证明节点进程从未见过测试密钥。

无缺口。

## 3. 越权

**已覆盖**：
- 鉴权中间件 casbin + `IsSuperuser` 短路（legacy `role=admin` 回退兼容，`core/base/pkg/auth/jwt.go` 注释即契约）。
- 治理工具网关七闸顺序闸（翻译→tenant→role→incident→resource→read-only 硬闸→限流），`gateway_fail_closed_test.go` 七类矩阵 + 五补充类，每类四重断言（Blocked + deny_reason + **fake Dispatch 零调用** + denied 表行 + denied 审计行）——undeclared/duplicate/drifted/write/destructive/cross-incident/cross-resource 全部 fail closed。
- MCP 端点（`/api/v1/mcp`）经鉴权后组装工具面（决策 108），`make mcp-surface-check` 用仓内客户端打真 handler。

**B 级缺口**：
- **B-3**：MCP 工具面与治理网关七闸的关系未成文——MCP 走的是 decorators 面，与 PiG 网关同源但非同一实例。整改：在两份面的文档里写明「同一策略语义的两个装配点」，避免读者以为 MCP 绕过网关。

## 4. 跨租户

**已覆盖**：租户密钥推导闸门（§1）；任务与调用行的 tenant 前置条件在 store 层单条条件 UPDATE 里（`Claim`/`ReportResult` 拒收他租户行）；网关 tenant 闸在七闸第二位；`incidents/metrics` 类读端点显式带 `tenant_id` 过滤。测试面：`usecase_test.go` 十三条验收（含跨代际/终态拒收）+ 网关矩阵。

无新增缺口。

## 5. prompt injection

**已覆盖**：
- `core/base/pkg/promptguard`：每次渲染现抽 nonce、`Parse` 只认 id 匹配的闭合标签；`make promptguard-check` 是闸门（Makefile L443）。
- 外来文本闭集：`biz/aiops/tools/untrusted_sources.go` 的 `ToolName*` 常量 + `MarkUntrustedOutput` 单点适配，覆盖 host_bash/cloud_bash 与 S-3 新增的知识库检索（KB 内容即外部文本，S-3 附加条款）。
- `pig-tools` 侧另有 `core/pig/pigrpc/promptguard_test.go`。

无缺口。

## 6. 审批绕过

**已覆盖**：
- 审批双签：`biz/approval/dualsign_storage_test.go` + `biz/hitl/dual_sign_test.go`（决策 362）。
- 修复类动作 proposal-only：网关 3.5 契约——`ReadOnly=false` 时落 proposal（pending）后**本轮不执行**，执行仍走既有 human-approved exact-payload 路径；网关不内置执行。
- 写工具一次性审批回执（决策 96/154：`host_restart_service` 本地执行锁死可复现）；per-tool 配额在 tool broker 强制。
- 审批决定入账（`approval` 域路由审计 + `hitl` 审计测试）。

无缺口。

## 7. 审计缺失

**已覆盖**：
- route-audit 表 + `scripts/routeaudit` 闸门：全部写路由入账，本轮新增 `runtime_task_*` 七动词与 `ResourceRuntimeTask`、`ResourcePigTool` 同刀进闭集；三条高频存活路由按 settled 裁决（时间语义由 lease/checkpoint 表持有，理由成文于决策 472）。
- 审计链 HMAC 链头（`chainkey_test.go`）；防篡改账本表结构不暴露给业务（决策 273）。
- 节点平面：自治两阶段落盘审计 + `agent.audit.replay` 全量回放（决策 99-101）；插件安装器每次安装/卸载入链（决策 126）。
- 整改重复写路由的守卫测试自决策 307 起逐包补齐（approval/hitl/loop/iam/imbridge/chatdiagnose/agentteams/dataguard/skill）。

无缺口。

## 8. 网络出口

**已覆盖**：
- PiG 子进程沙箱四禁（2.4）：`ProcessDriver.Launch` 每次复检配置与 profile；子进程仅 `PIG_*` 白名单环境，注入测试逐项断言 PATH/DB_PASSWORD/DATABASE_URL/KUBECONFIG/AWS_*/VAULT_TOKEN/HOME 不泄漏；无宿主写、无 provider 覆盖、无环境扩展发现。
- 节点出网唯一通道 tool broker（限流与 per-tool 配额在此强制）；自治白名单 argv 含元字符即拒（13 条具名拒绝）；PiG 请求经 manager llmgw（节点不能选 provider）。
- 无环境扩展发现（1.4 配置面 fail closed，缺工件即拒启）。

**B 级缺口**：
- **B-4**：llmgw 的上游出口（manager → 模型 Provider）凭据与路由表未在本轮审查范围内逐项核（本轮聚焦控制面入口）。整改：下轮把 llmgw 出口策略（允许哪些 Provider、密钥如何注入）补进审查清单。

## 供应链与发布完整性（附带）

- **B-5（=R-5）**：发版当日 GitHub release 资产域三机位 404，ONNX Runtime 归档级 SHA 未复算；镜像内 `.so` 取自 rc.1 的 SHA 校验构建（同源字节，sha256 已记）。整改：网络恢复后复跑 `make docker-build` 取得可复算证据。
- **B-6（=6.2 缺口）**：无密码学产物签名（cosign/notary 类）。整改：作为独立切片立项，签名对象为镜像 tar 与 GitHub Release 资产。

## 结论

八项中 6 项无缺口、2 项各有 1 条 B 级（secret 管理的 B-1/B-2、网络出口的 B-4），另附供应链 B-5/B-6。**无 A 级（正式版阻塞）项**，v2026.11.0 以当前防线发布成立。B 级全部建议进下一版本切片，其中 B-1（删除生产 nginx 死 location 与过期 JWT 文本）随 BENY-1080 收尾顺手完成。

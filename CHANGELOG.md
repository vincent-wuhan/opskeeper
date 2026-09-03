# Changelog

本文件记录 opskeeper 各版本的主要变更。最新在上。

## zero-manual-ops-loop（零人工运维 闭环 v1）

> Feature branch: `codex/zero-manual-ops-loop` (worktree on `release-7.20`)
> OpenSpec change: `openspec/changes/zero-manual-ops-loop/`
> Release notes: `openspec/changes/zero-manual-ops-loop/RELEASE-NOTES.md`
> Project retrospective: `docs/superpowers/specs/SUMMARY-2026-08-11-zero-manual-ops.md`

### 主题
**"方向一：零人工运维"** — 告警聚合 → 根因定位 → 修复执行 → 恢复验证 → 事故复盘，多 Agent 闭环。

### 新增能力
- **闭环 Orchestrator**（7 阶段）：detected → correlated → investigated → critiqued → approved → recovered → postmortem；MySQL `GET_LOCK` advisory lock；3 表持久化（`loop_event_log` append-only + `loop_state` + `loop_contract`）
- **告警聚合 + 语义降噪**：3 条 static rules（PG / Redis / Host）+ 9 条 DIAGNOSIS_SKILL_MAP + LLM semantic_dedup（circuit breaker + async queue）
- **Recovery Verification**：verify_recovery basetool（4 metric allowlist + 3 档 warning）+ loop.RecoveredPhaseWorker（3 档 retry + severity 升级）
- **Postmortem + Critic + Source commits**：8 章节中文模板 + data-guard-classification redact + gitartifact 落 git + runtime-source-bridge 合并 v1
- **对话入口 / ChatDrawer**：3 HTTP endpoint（diagnose / promote / reports）+ 240px 右抽屉 + `/incidents/:id/diagnose` 独立路由 + ChatDiagnoseService + OrchestratorAdapter + ChatReportPusher
- **Web 时间线 + DPO 组件移植**：6 DPO 组件 + ClosedLoopTimeline + ChatDrawer（i18n + 视觉锁版）
- **Harness runner loop-mode + leaderboard**：`--mode=loop|chat|tool` + 4 指标（rca_accuracy / time_to_remediate / approval_rate / recovery_pass_rate）+ 3 case 端到端（pg / redis / host）
- **DB 迁移**：loop_event_log / loop_contract / diagnostic_conversation / diagnostic_turn / incident_pattern

### ⚠️ Breaking Changes
- 无对外 API breaking（3 条新 API + 6 个新 spec 都是新增）
- `/api/v1/loops/{incident_id}/trigger` + `/timeline` + `/recovery/verify` 是 admin-only 端点
- `feature.chat_diagnose` / `feature.chat_promote` / `feature.kb_first` 三个 feature flag 默认 off

### 验证
- `go build ./...` 0 错
- `go vet ./...` 0 警告
- `go test -race -count=1 -timeout 600s ./...` 全过（~400+ tests）
- Harness 3 case 端到端（pg / redis / host）→ 3/3 QUALIFIED
- 截图 4 张（mockup 静态版兜底）

### Deferred to next change
- `llm-worker-integration`：替换 5 个 LLM-driven worker（detected / correlated / investigated / critiqued / approved / postmortem）+ 真实 metric adapter + 真实 LLM judge
- `chatruntime-kb-implementation`：KB 三件套真实 impl + pgvector + LLM-extracted fingerprint
- 详见 `borrow-map.md` §D

---

## vNext — platform-base-ha（多实例 manager HA 底座）

> Feature branch: `feat/platform-base-ha`
> OpenSpec change: `platform-base-ha`

### ⚠️ Breaking Changes
- **`manager.replicaCount` 默认值 1 → 2**：Helm chart 现在默认部署 2 个 manager 副本。
  单副本部署必须显式设置 `manager.replicaCount=1` 且 `leader.enabled=false`。
- **`/readyz` 响应格式**：从纯文本 `ready` 变为 JSON `{ready, role, checks}`。
  K8s probe 仅检查 HTTP status code（200/503），路径不变，部署 yaml 无需更新。
- **`manager.persistence.enabled` 默认值 true → false**：HA 模式下数据存外部 DB，
  不再依赖 PVC。单副本 embedded 部署需重新启用。

### 新增
- **Redis 分布式锁** (`internal/pkg/redislock`)：Acquire/TryAcquire/Renew/Release，
  Lua 脚本保证原子性；19 个单测
- **Leader 选举** (`internal/pkg/leader`)：per-role 选举，renew 自动续期，
  MarkDraining/ResignAll 优雅让位；11 个单测
- **健康检查拆分** (`internal/pkg/probes`)：/healthz 始终 200、/readyz 并行
  DB+Redis+Workers 检查返回 JSON；8 个单测
- **集群状态 API** (`internal/manager/server/cluster`)：
  `GET /api/v1/cluster/status`（admin only）返回 role/workers/deps；6 个单测
- **优雅退出** (`internal/pkg/shutdown`)：MarkDraining → HTTP drain →
  ResignAll → DB/Redis close；6 个单测
- **4 个 leader-only worker 注册**：scheduler:flow / scheduler:report /
  harness:runner / upgrade:checker（migrate:runner 跳过，走 MySQL GET_LOCK）
- **Helm HA chart**：PDB / HPA / NetworkPolicy / podAntiAffinity /
  topologySpreadConstraints / external DB+Redis 配置
- **3 个新 config 组**：`database.*` / `redis.*` / `leader.*`
- **部署文档**：`docs/deployment/{ha,external-deps,upgrade}.md`

### 配置变更
| 新增 env var | 默认值 | 说明 |
|---|---|---|
| `OPSKEEPER_LEADER_ENABLED` | `true` | 开启 leader 选举 |
| `OPSKEEPER_LEADER_TTL` | `15s` | leader 锁 TTL |
| `OPSKEEPER_LEADER_RENEW_INTERVAL` | `5s` | renew 间隔 |
| `OPSKEEPER_DB_HOST` | 空 | 外部 MySQL host |
| `OPSKEEPER_DB_PORT` | `3306` | MySQL 端口 |
| `OPSKEEPER_DB_SSLMODE` | `disable` | TLS 模式 |
| `OPSKEEPER_REDIS_ADDR` | `127.0.0.1:6379` | Redis 地址 |
| `OPSKEEPER_REDIS_PASSWORD` | 空 | Redis AUTH |

## v1.0.0 (2026-07-13) — 统一 AIOps 平台（路径 A 落地）

大版本：以 opskeeper 为基础，集成 ops-keeper 中间件 Adapter + Harness 评测 +
git-artifact 反向索引 + Helm chart 部署工程。从单产品升级为横跨 6 类资源
（host / network / container / cloud / middleware / git）的统一运维智能化平台。

完整 release notes：[docs/releases/v1.0.0.md](docs/releases/v1.0.0.md)。

### ⚠️ 升级注意（破坏性变更）
- **首版统一平台**：从 v0.9.x 升级需运行 12 张新表的数据库迁移（`opskeeper-manager --migrate up`）
- **AI 助理写权限默认「关闭」**（继承 v0.9.0，fail-safe）
- **Adapter 写操作需审批**：`kill_session` / `flushdb` 等必经 Casbin 工单
- **Harness 双模型 judge 成本 ≈ 2x**：可通过 `--judge.models` 退化到单模型

### 新增（核心能力）
- **中间件 Adapter 接入**（PG/Redis/RabbitMQ/Kafka/K8s/Git 6 类）：65+ 工具方法
- **git-artifact 反向索引**：4 类符号反查（pg_query / redis_cmd / k8s_image / http_route）+ 协议 v0/v1
- **Harness 评测平台**：60+ golden cases + 双模型 judge + leaderboard + 回归基线
- **跨资源域 RCA 合并**：evidence schema + 置信度因子 + SLA histogram
- **可观测性栈集成**：Qdrant + Prometheus + Loki + Tempo + Grafana（Helm 依赖可选）
- **Helm chart 完整化**：可观测性栈依赖 + NOTES + README + 自动 migration
- **Agent 跨域流程**：incident-investigator 加 5 个 tool + 4 段按需跨域流程
- **Owner 透传**：tenant.UserID → SpawnRequest.OwnerUserID → worker ctx

### 文档（6 篇 / 2242 行）
- [docs/integration-guide.md](docs/integration-guide.md) — 集成指南
- [docs/operations-manual.md](docs/operations-manual.md) — 运维手册
- [docs/harness-guide.md](docs/harness-guide.md) — Harness 评测指南
- [docs/api/middleware.md](docs/api/middleware.md) — 中间件 API
- [docs/api/git-artifact.md](docs/api/git-artifact.md) — git-artifact API
- [docs/api/harness.md](docs/api/harness.md) — Harness API

### 内部改进
- `manager → middleware` 跨层转换**仅在 cmd/opskeeper 组合根**，业务包保持单层架构
- 共享状态全部加锁，所有测试 `-race` 全绿（10 包 305+ tests）
- 错误统一 `%w` 包装；不重复记录
- 所有 IO 函数第一个参数为 `context.Context`
- Prometheus label 禁高基数字段全部合规
- 数据库迁移纯增量（expand-contract），不破坏存量数据

### 移除 / 废弃
- 移除：`AutoMigrate` 内嵌工具（v1.1 由独立 `migration-runtime` 取代）
- 废弃：单模型 judge（仍可用 6 个月，v1.2 移除）

### 致谢
感谢 ops-keeper 团队提供 Adapter / Harness / git-artifact 设计参考，及所有早期
采用者的反馈。路径 A 集成于 commit 69aceb8 完成。

---

## v0.9.0 (2026-06-27)

大版本：统一任务抽象、MCP 客户端、AI 助理写权限治理，外加一整轮工作流编排与可观测打磨。

### ⚠️ 升级注意（行为变化）
- **AI 助理写操作开关现在默认「关闭」**（fail-safe）。升级后助理出厂为**只读**——写 / 变更 /
  执行类工具（云端命令、应用配置、安装扩展、发送消息、派发子任务、主机命令）都不暴露给模型。
  管理员在 **设置 → 助理** 显式开启才允许写操作；开启后主机命令（host_bash）会以无限制方式
  在边端执行（绕过命令安全策略），界面有醒目警示。**例外**：`serve_page`（托管只读网页）始终
  可用，不受写开关限制。
- 数据库迁移为**纯增量**（新增 `tasks` 表 + `flows`/`mcp_servers`/`approvals`/`secrets` 等新表
  + `reports.task_id`/`run_id` 两列），不破坏存量数据；已实测 v0.8.x → v0.9.0 迁移零报错。

### 新增
- **统一任务抽象**（HLD-022）：「任务」成为生成的统一入口——定时报告（recurring）+ **一次性
  任务**（oneoff，建即生成）同列；产物按 `task_id` 反向归属任务，任务详情总览其全部产物。
  「立即生成 / 新建定时任务」合并为单个「新建任务 ▾」下拉。生成动作全部归任务侧，产物页只读。
- **MCP 客户端**（HLD-018）：opskeeper 作为 MCP client 接入外部 MCP server（Streamable HTTP）；
  工具自动发现并进入 chat toolbag + 工作流调色板（`mcp__<server>__<tool>`）。**认证**采用
  「命名凭证 + 请求头模板」静态注入（Bearer / API-Key 等），凭证加密存 secrets 库、运行时进程内
  解密、从不经 API 序列化；`trusted` 标志独立于认证，只决定同步直跑 vs 走审批。
- **Agent 写权限总闸**（设置 → 助理）：一键控制 chat 助理全部写权限；开启后 host_bash 旁路
  边端 cmdpolicy（无限制执行），有审计 WARN 日志。
- **产物中心**：「产物」页标签栏区分**页面** / **报告**；卡片缩略图、来源标记、私有默认 +
  显式 TTL 分享链接、新标签页完整打开。
- **工作流编排**：自然语言一句话生成工作流（AI，且自动给节点命名）、HTTP Request 节点、Agent
  节点 persona 下拉、按字段上游变量选择器、节点抽屉分区、工具参数类型自动纠偏（n8n/Dify 风格）。
- **内置技能**：`serve_page`（HTML → 可分享内网网页）、`send_im_message`（发飞书 / 钉钉）。
- **技能页**：以技能为中心重排、合并 MCP、可折叠分组、本地化 Class 徽章与工具名。
- **首页 / 对话**：换行统一 **Shift+Enter**（跨平台）、输入框 `@` 调出设备 / 资源。
- **告警**：`alert_fired` 触发器在事件**重新打开**时也触发（不止新建）。
- **工具**：`get_edge_summary` 省略 `device_ids` 即汇总**全部边端**（最多 16），「巡检所有设备」
  无需先查清单。

### 修复 / 体验
- 工作流运行**失败时把错误顶到顶部横幅**（不再只埋在侧栏节点明细）。
- 运行记录里节点显示**有意义的名字**（name → 工具名 → 类型 兜底），不再是 a/b/c/d；serve_page
  节点直接给出**「打开生成的页面」链接**。
- 边端 traces 上报 TLS 自签证书校验失败（支持 skip-verify）。
- 晚注册的工具重新并入工作流调用器；coordinator 桩去重避免工具重名导致 chat 崩溃；助理遇未知
  工具调用自愈续跑。
- 大量浅色模式 UI 对齐与配色修复；输入框聚焦样式统一。

### 性能 / 内部
- 报告卡缩略图**懒加载**（进视口才拉详情）。
- 清理无用的「页面→任务」关联代码路径。

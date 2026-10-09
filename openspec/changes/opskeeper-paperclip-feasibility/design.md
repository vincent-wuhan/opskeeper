# Context

## 调研基线

- OpsKeeper 仓库：`release/20260922`，工作区在调研前无未提交变更。
- Paperclip GitHub 元数据：TypeScript、MIT License、default branch `master`。
- Paperclip 最新调研 commit：`0f14d261233c545aa6a8a38ec253c498a5130fff`，时间为 2026-09-27 UTC。
- 本地存在 Paperclip 工作副本，但其 `master` 较旧且包含未提交改动；因此规范判断以 GitHub API 拉取的最新 `PLUGIN_SPEC.md`、plugin type、validator、plugin webhook schema 和 approval schema 为准。

## Implementation Tracking Snapshot (2026-09-30)

Multica/OPC tracks implementation in staged issues. The current repository snapshot contains this feasibility specification only; it does not contain the connector runtime.

- **S1 baseline and contract fixture: merged.** GitHub Paperclip PR #1 merged at `d6cb43c1c34b48c4adbaaee4920d259fcd2d7f72`; GitCode MR #1 merged at `9f235a0084913aa0ea9428a3f8927dc60a2f9ca1`.
- **S2 Phase A read-only sync: independently verified, not merged.** The source archive is `paperclip-opskeeper-sync-source.tar.gz` (20,725 bytes, SHA-256 `157d0de4a7659d0399f1c9783bab12c93fbafd82a5510091ff0829cf6318ccb6`). Manifest gate, 10 unit/integration tests, 9 E2E checks, and TypeScript checking pass, but the archive has not been pushed to a Paperclip repository.
- **S3 Phase B approval bridge: local patch/bundle delivered, not applied or merged.** The delivered Paperclip-side patch records 51/51 tests and includes apply/rollback evidence, but it still requires a credentialed operator to apply it to the authoritative Paperclip baseline, re-run validation, and create the review PR.
- **S4 Phase C read-only tools and security review: merged.** GitHub Paperclip PR #2 merged at `cce7be1b26455b28fb1d903a44366009b37e5e11`; GitCode MR #2 merged at `8fb29cdbec67d1185e0b31b9459731cb4007df83`.
- **S5 E2E/install/runbook/go-no-go: not started.** Self-hosted installation, disable/rollback, operator runbook, complete cross-phase E2E, and the final implementation report remain open.

Accordingly, future implementation tasks in `tasks.md` remain unchecked until their artifacts are accepted in the authoritative Paperclip repository or the relevant operational environment. Multica issue status or an attachment alone is not treated as repository-level completion.

## Implementation Tracking Snapshot (2026-10-09)

The current Paperclip plugin baseline is authoritative `master@cce7be1b26455b28fb1d903a44366009b37e5e11`. GitHub Paperclip PR #3 merged into `master` at `5541357c55b1d39f5b5350eac5993ea1bada1765` on 2026-10-09.

- **Current SDK Phase A plugin: merged.** `packages/plugins/plugin-opskeeper-sync` now provides plugin ID `paperclip.opskeeper-sync` version `0.1.0`. It polls every minute through the host HTTP bridge, mirrors incidents and details into company-scoped plugin state, advances a cursor, merges idempotently, marks stale state, isolates one company failure from another, paginates visible companies, enforces HTTPS and host allowlisting, disables redirects, bounds requests and storage, validates deep-link schemes, resolves the token from a secret reference, and exposes only read-only lookup/detail/sync-state tools plus an incident-list dashboard widget.
- **Validation.** Plugin tests pass at 2 files / 6 tests, followed by plugin typecheck and build. Repository typecheck and build pass. Full local `pnpm test:run` reached 13,573 passing tests, 84 skipped, and 13 failures in existing runtime-cache suites because macOS rejects renaming their read-only staging directories; a focused rerun reproduced the same `EACCES`. In GitHub CI, policy, typecheck/release registry, build, runner, all server/chat/workspace shards, serialized suites, e2e, canary dry run, Docker context integrity, and isolated native Runner checks passed. The fork repository's Dependency Review check remains unsupported and failed, matching merged PR #1 and PR #2; it was not a merge blocker.
- **Accepted task updates.** Tasks 3.1, 3.3, 3.4, and 3.5 are complete in the authoritative Paperclip repository. Task 3.1 records the current SDK baseline above.
- **Remaining gaps.** The UI is an incident list with deep links; timeline, RCA summary, and evidence are available through read-only tools but not yet a full detail page. The merged plugin does not yet implement per-principal tool rate limiting or its own audit log, the approval bridge, scoped OpsKeeper service account, live Paperclip + OpsKeeper installation E2E, source-removal tombstones, packaging into a self-hosted deployment, operator runbook, security review, or final go/no-go report. Therefore those tasks remain unchecked.

## Paperclip 能力

Paperclip 插件 manifest V1 支持：

- worker 与 UI entrypoint。
- `apiRoutes`：挂载在 `/api/plugins/:pluginId/api/*`，不能覆盖核心 API。
- `webhooks`：接收外部系统 POST，并持久化 delivery 记录。
- `tools`：向 agent 声明可调用工具。
- plugin database namespace 与迁移。
- UI page/sidebar slot。
- capability 声明与运行时校验。
- jobs、events、managed agents/projects/routines/skills。

Paperclip 核心模型包括：

- company/project/goal/issue 层级。
- agent/user 分配与执行锁。
- approval、issue approval 和审批评论。
- heartbeat run、run event、watchdog。
- activity log、budget、secret、plugin state/database/webhook。

## Paperclip 约束

最新 `PLUGIN_SPEC.md` 明确：

- Plugin Spec 是 post-V1 完整目标架构，不是 V1 实现合同。
- 当前插件运行时仍是早期实现。
- 实际部署偏单租户、自托管、单节点或文件系统持久化。
- 插件 UI 是同源可信 JavaScript，不是前端安全沙箱。
- 动态插件安装尚不适合水平扩展或 ephemeral 云部署。
- runtime npm 安装依赖本地文件系统和 registry 网络。

因此集成必须采用服务端连接器、最小 capability、显式配置和只读默认策略，不能把 Paperclip 插件当成零信任执行环境。

## OpsKeeper 能力

OpsKeeper 已有：

- incident timeline、archive、repair preview summary、recall logs。
- alert incident list/detail/events/investigation。
- approval list/detail/approve/reject。
- loop trigger/timeline/state 和 recovery verify。
- AgentTeams incident event recording。
- MCP JSON-RPC 与工具授权。
- 通知路由与 outbound webhook 通知配置。
- 审计、目标指纹、proposal、命令和 payload hash 校验。

限制方面：

- 现有通知通道偏告警/消息通知，尚未确认所有 incident lifecycle 状态都有稳定、可签名、可重放的 outbound event payload。
- 现有 API 权限偏管理员会话；面向 Paperclip 的 scoped integration token 需要新增或确认。
- AgentTeams 房间状态和 OpsKeeper incident/archive 状态是两套投影，集成时不能把 Paperclip 再变成第三套权威状态。

# Alternatives

## Option A：Paperclip 侧 OpsKeeper Connector Plugin（推荐）

在 Paperclip 中实现可信服务端插件，调用 OpsKeeper API/MCP，把 OpsKeeper 事故映射为 Paperclip issue/工作台对象。

优点：

- 不改 OpsKeeper 核心闭环。
- 最小化权限暴露。
- 能复用 Paperclip issue、审批、agent、UI 和插件治理。
- 可以先只读，再逐步开启桥接。
- 失败时只影响 Paperclip 展示层，不影响恢复链路。

缺点：

- 需要开发和打包 Paperclip 插件。
- 依赖 Paperclip 插件运行时成熟度。
- 需要 OpsKeeper scoped service account。

结论：Phase A/B/C 均采用该路线。

## Option B：把 OpsKeeper 做成 Paperclip adapter

将 OpsKeeper 角色或闭环流程包装成 Paperclip agent adapter，由 Paperclip heartbeat 驱动。

不推荐第一阶段采用：

- Paperclip adapter 语义偏向“执行一个 agent run”，而 OpsKeeper 是多角色、有状态、带安全审批的 incident control plane。
- 会把状态权威从 OpsKeeper 挪到 Paperclip heartbeat。
- 容易绕开 proposal/target/hash 精确授权。
- 失败恢复、角色分离和审计链路需要重建。

可在 Phase C 之后仅作为展示型 adapter 探索，不承载修复动作。

## Option C：用 Paperclip 替换 AgentTeams/Matrix 协作面

把 Rooms 对话、插件投影、Manager/Worker 协作和审批全部迁移到 Paperclip。

不推荐当前实施：

- 工程范围大。
- 需要重建房间事件语义、插件投影和 worker 通信。
- Paperclip 插件系统尚未达到多实例云化分发成熟度。
- 会引入第三套状态和审批模型。

# Recommended Architecture

## Phase A：只读同步与展示

```text
OpsKeeper API
  ↓ polling / optional signed webhook
Paperclip OpsKeeper Connector worker
  ↓ mapping + plugin namespace state
Paperclip issue/comment/UI page
``+

范围：

- Poll `GET /api/v1/alerts/incidents` 或等价 incident list API。
- Pull detail、events、archive、repair preview summary。
- Map incident 到 Paperclip issue。
- Map incident timeline/RCA/evidence 到 issue comments 或 plugin UI sections。
- 保存 mapping、cursor、拉取状态和错误。
- UI 提供 incident list、状态、严重级、责任人、RCA 摘要、证据链接和跳转 OpsKeeper 的 deep link。
- 所有动作按钮默认隐藏或 disabled。

同步策略：

- 初始版本使用 scheduled job polling，避免依赖 OpsKeeper 完整 lifecycle webhook。
- polling interval 默认 30–60 秒，可配置。
- 使用 `updated_at`/cursor 和幂等 upsert。
- API 超时、429、5xx 使用 exponential backoff。
- 不删除 Paperclip issue；OpsKeeper 删除或脱敏时仅标记 `source_removed`。

## Phase B：审批桥接

```text
Paperclip approval UI/action
  ↓ plugin API route, board/admin only
OpsKeeper scoped approval API
  ↓ authoritative approval + audit
OpsKeeper existing workflow
  ↓ status sync
Paperclip issue/comment mirror
```

关键原则：

- OpsKeeper 是唯一审批事实源。
- Paperclip approval 只是触发 OpsKeeper approve/reject 的前端入口。
- 每次桥接必须携带 OpsKeeper approval id、incident id、proposal id、resource、command hash、payload hash、actor 和 idempotency key。
- 双签/租户策略仍由 OpsKeeper 判断。
- 网络失败或超时时只允许显式重试，不允许本地自动补提交。
- Paperclip 保存 bridge request/outcome，但不保存可独立生效的授权决定。

Phase B 前置条件：

- OpsKeeper 提供 read incidents + read approval + submit approval decision 的 scoped service account。
- 明确 service account 不能执行修复、shell、MCP 变更或跨资源操作。
- 建议支持 HMAC request signing 或 mTLS。
- Paperclip 侧 token 使用 `secret_ref`，不下发到 UI。

## Phase C：只读 agent tools

向 Paperclip agent 暴露：

- `opskeeper_list_incidents`
- `opskeeper_get_incident`
- `opskeeper_get_incident_timeline`
- `opskeeper_get_rca_summary`
- `opskeeper_search_archive`

工具只读，并返回来源链接和证据引用。默认不暴露：

- approve/reject。
- repair execute。
- shell/host action。
- MCP mutating call。
- alert resolve/silence。
- recovery verify trigger。

如果未来需要 agent 发起修复建议，也只能生成 Paperclip issue/comment，再由 OpsKeeper 原有 workflow 创建 proposal 并进入人工审批。

# Data Mapping

| OpsKeeper | Paperclip | 说明 |
|---|---|---|
| tenant/company | company | 需要配置显式映射，不做自动多租户推断。 |
| incident | issue | issue 保存镜像和协作，不成为事故权威状态。 |
| incident event | issue comment / plugin timeline | 时间、类型、actor、证据引用保持原样。 |
| RCA report | issue document / plugin UI section | 展示结论和 evidence links。 |
| repair preview A/B | issue description or review checklist | 保留候选差异与拒绝原因。 |
| approval | approval mirror | Paperclip 动作只代理 OpsKeeper 决定。 |
| audit event | activity log / plugin delivery log | 双侧记录 actor、时间、请求 ID。 |
| recovery verification | issue verification note | 只同步结果，不触发执行。 |

建议 plugin namespace 表：

- `incident_map(company_id, opskeeper_incident_id, issue_id, fingerprint, status, last_seen_at)`
- `sync_cursor(company_id, resource, cursor_value, updated_at)`
- `approval_bridge_log(company_id, opskeeper_approval_id, request_id, status, submitted_at, result)`
- `delivery_log(company_id, source_event_id, status, error, duration_ms)`

唯一键必须包含 company 和 OpsKeeper source id，不能用 Paperclip issue id 反推权威状态。

# Plugin Manifest Direction

建议 plugin id：

```text
opskeeper.connector
```

Phase A 最小 capabilities：

- `api.routes.register`
- `issues.read`
- `issues.create`
- `issues.update`
- `issue.comments.read`
- `issue.comments.create`
- `plugin.state.read`
- `plugin.state.write`
- `ui.page.register` 或 `ui.sidebar.register`
- 可选 `database.namespace.*`

Phase B 增加：

- approval 相关只读 capability。
- plugin API route 使用 board/admin auth。
- `activity.log.write`。

不建议申请：

- 核心数据库任意读。
- agent session 任意创建。
- local folder。
- environment execute。
- secret 泛读。

配置项：

- `baseUrl`
- `authSecretRef`
- `tenantMapping`
- `pollIntervalSeconds`
- `readOnly`
- `approvalBridgeEnabled`
- `requestTimeoutMs`
- `retryMax`

`readOnly=true` 时禁用所有 approve/reject/resolve/silence/repair API 调用。

# Security Design

1. **权限最小化**
   - 使用独立 scoped service account。
   - Phase A 只读。
   - Phase B 仅 approval decision 代理。
   - 不复用管理员账号。

2. **权威边界**
   - OpsKeeper 保留 incident、proposal、approval、repair、audit 的权威状态。
   - Paperclip 保存镜像、映射和协作记录。
   - Paperclip agent 不直接获得 OpsKeeper 高权限工具。

3. **传输与身份**
   - 强制 HTTPS。
   - 推荐 HMAC signing 或 mTLS。
   - request id + idempotency key 防重放和重复提交。

4. **插件边界**
   - secret 只在 worker 进程读取。
   - UI 不持有 token。
   - 不 iframe 特权 OpsKeeper 页面。
   - 不把 OpsKeeper API token 写入 plugin state、日志或 issue comment。

5. **审计**
   - 每次同步、审批桥接和 agent tool 调用记录 request id、actor、目标、结果和耗时。
   - 审批结果必须可回链 OpsKeeper approval id 和 audit event。

6. **失败策略**
   - API 不可用时显示 stale 状态和最后同步时间。
   - 不用缓存结果代替审批判断。
   - 超时后不自动重复提交非幂等动作。
   - 插件禁用不影响 OpsKeeper 原有闭环。

# Feasibility Assessment

| 目标 | 可行性 | 结论 |
|---|---:|---|
| 只读事故工作台 | 高 | Phase A 可实施。 |
| RCA/证据展示 | 高 | 依赖 API payload 稳定性。 |
| Paperclip 审批入口 | 中高 | 需 scoped token 和幂等桥接。 |
| 只读 agent tools | 中高 | 需工具 schema 和限流。 |
| OpsKeeper 作为 Paperclip adapter | 中低 | 语义不匹配，暂缓。 |
| 替换 AgentTeams/Matrix | 低 | 工程和安全风险大，不做。 |

预估实施量：

- Phase A：3–5 个工作日。
- Phase B：5–8 个工作日，依赖 OpsKeeper service account。
- Phase C：3–5 个工作日。
- 稳定性、安全测试和打包发布另需 2–3 个工作日。

# Rollout Plan

1. 冻结 Paperclip 插件 API 版本并记录上游 commit。
2. 在独立插件包中实现 read-only connector。
3. 用 fake OpsKeeper API 做契约测试。
4. 本地 Paperclip + OpsKeeper demo 做 E2E。
5. 审查 manifest capabilities 和 secret 处理。
6. 只读试运行一周。
7. 增加 scoped approval account。
8. 在 `approvalBridgeEnabled=false` 下部署审批代码。
9. 双人 review 后逐步开启审批桥接。
10. 最后开放只读 agent tools。

# Open Questions

1. OpsKeeper 是否已有可长期使用的 scoped API token 机制。
2. Incident lifecycle 是否能提供完整 outbound event stream，还是必须长期 polling。
3. Paperclip issue 与 OpsKeeper incident 的 company/tenant 映射规则。
4. Paperclip actor id 如何映射到 OpsKeeper audit actor，尤其是 SSO/用户名不一致时。
5. Paperclip 插件运行时在生产自托管场景的稳定性基线。
6. 双审批、租户策略和 Paperclip approval 状态的展示同步方式。

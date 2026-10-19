## 1. P0 基线冻结与即时修复

- [x] 1.0 清理 OPC 项目中历史阶段日报、历史交付材料、旧发布窗口等已被本计划取代的过时任务；保留安全治理、运行时治理、当前 OpenSpec 与通用工程能力任务。
- [ ] 1.1 执行 `git fetch origin main` 并确认远端 `main` 后，从该 commit 拉出 `codex/plugin-architecture-recovery-plan` 分支，记录基线 commit、环境变量清单和当前部署差异，不修改生产配置。
- [ ] 1.2 修复 `/admin/runtime` 对 `skills: null`、空 dependencies 和空 recovery phases 的兼容，避免能力清单白屏。
- [ ] 1.3 为 Runtime 页面补充 API 空值、空数组和错误响应测试，覆盖标准用户与管理员视角。
- [ ] 1.4 核对 TeamHarness `1.0.75`、五个 SRE 插件包、94 个 manifest 工具与 Makefile/文档描述的一致性。
- [ ] 1.5 梳理 `VERSION`、`RELEASE_VERSION.json`、release scope、backend commit、web hash 和插件版本的漂移清单，形成新版本命名建议。
- [ ] 1.6 运行 `make module-check`、`make pi-plugin-guard`、`go test ./scripts/ledgercheck`，并记录结果与失败处理。
- [ ] 1.7 冻结 P0 基线，在 OPC 中创建总里程碑和阶段闸门。

## 2. P0 Runtime 治理首切片

- [ ] 2.1 按 `runtime-management-gap` 完成统一 runtime identity、deployment location、artifact digest、版本和健康模型。
- [ ] 2.2 实现 runtime 注册、心跳、command acknowledgement、inventory 和健康状态迁移 API。
- [ ] 2.3 为 Manager Chat Worker、Edge Agent、AgentTeams Worker、TeamHarness/plugin manager 和 preview-pg 提供只读适配器。
- [ ] 2.4 建立任务 idempotency、admission、claim、lease、checkpoint、attempt、cancel、result 和 failure classification 模型。
- [ ] 2.5 将一个只读诊断 worker 和一个 repair-preview 执行接入共享任务生命周期。
- [ ] 2.6 补充心跳丢失、进程重启、网络分区、租约过期、重复 claim 和 terminal result 幂等测试。
- [ ] 2.7 增加运行时 inventory 只读视图，展示 runtime、健康、能力、容量、版本、artifact digest 和当前任务。
- [ ] 2.8 通过 `runtime-management-gap` 阶段验收，并在 OPC 更新依赖任务状态。

## 3. P1 PiG Runtime Adapter

- [ ] 3.1 按 `pig-runtime-adapter` 定义 AgentRuntimeAdapter port、runtime descriptor、生命周期事件、健康、取消和 terminal result 契约。
- [ ] 3.2 定义 PiG release identity、兼容范围、能力 manifest、sandbox profile 和 immutable execution snapshot。
- [ ] 3.3 实现 opt-in、固定版本 PiG executable 或 Piglet artifact 加载，禁止 ambient discovery。
- [ ] 3.4 实现进程启动、注册探测、事件流、取消、超时、崩溃处理和终止清理。
- [ ] 3.5 将 PiG 注册和生命周期事件归一到 OpsKeeper runtime health 与任务事件。
- [ ] 3.6 实现 declared、observed、allowlisted 工具对账，未批准工具不得暴露给角色。
- [ ] 3.7 将 PiG 工具 ID 映射为稳定 OpsKeeper 工具 ID，并标注 read/write/destructive 类别。
- [ ] 3.8 在租户、事件、角色、资源、能力、速率和只读策略之前强制准入检查。
- [ ] 3.9 持久化工具开始/结束、调用者、release digest、能力 digest、参数 hash、结果状态、错误和 trace correlation。
- [ ] 3.10 用 fake PiG 覆盖启动、注册、心跳、进度、取消、崩溃、完成和无重放恢复。
- [ ] 3.11 完成 PiG 只读 investigator-style incident E2E、沙箱约束和 secret 不披露测试。
- [ ] 3.12 通过 `pig-runtime-adapter` 阶段验收，并解锁后续插件执行能力。

## 4. P1 插件能力恢复

- [ ] 4.1 建立旧 release 业务板块到六个扩展点的映射表：工作台、观测、事件、修复、审批、知识、协同、企业集成。
  - 初稿：`business-domain-mapping.md`（基线 `release-v2026.09.20-patch.2` 对比 `main@86dff884`），待评审其中“待评审问题”后再勾选。
- [ ] 4.2 盘点当前编译内能力，标记保留在 core、迁移为插件、暂缓迁移和淘汰四类。
- [ ] 4.3 为第一批 `datasource` 候选制定插件包方案，优先 VictoriaMetrics、Elasticsearch 或 OpenSearch。
- [ ] 4.4 为第一批 `adapter` 候选制定插件包方案，优先 Kubernetes、云资源、ITSM、CMDB 或 Git。
- [ ] 4.5 为 `notifier` 候选补齐插件化方案，覆盖飞书、钉钉、Slack、PagerDuty 或 Webhook 的差异化能力。
- [ ] 4.6 为 `provider` 候选补齐模型供应商插件方案，保持凭据环境作用域和费用/速率治理。
- [ ] 4.7 为 `collector` 候选补齐边缘采集插件方案，保持进程模型、日志和资源上限可审查。
- [ ] 4.8 补齐 observability、readonly、middleware 插件中的业务缺口，优先旧 release 已有但新架构未恢复完整的能力。
- [ ] 4.9 强化 repair 插件的计划预览、独立验证、回滚说明和审批证据。
- [ ] 4.10 为每个新增插件包维护 manifest、精确版本、scope、安全级别、兼容范围、测试、签名和回滚文档。
- [ ] 4.11 运行插件构建、测试、准入、签名、灰度和回滚闸门。

## 5. P1/P2 企业流程与协同

- [ ] 5.1 完成 `ops-daily-brief` 剩余前端、后端、双主题双语和 Non-Goals 走查。
- [ ] 5.2 按 `opskeeper-paperclip-feasibility` 创建 OpsKeeper 作用域只读服务账号，仅授予 read-only 与 approval-only scope。
- [ ] 5.3 实现 Paperclip incident list、timeline、RCA summary、evidence links 和 OpsKeeper deep links 的只读 UI。
- [ ] 5.4 实现带 schema validation、timeout、rate limit 和 audit logging 的只读 agent tools。
- [ ] 5.5 在 `approvalBridgeEnabled=false` 默认值下实现 Paperclip 审批桥，不改变默认行为。
- [ ] 5.6 测试审批幂等、超时、重放、无效 target hash、无效 payload hash 和 actor mismatch。
- [ ] 5.7 完成 Paperclip + OpsKeeper 的 incident sync、stale state、approval bridge 和 audit readback E2E。
- [ ] 5.8 审查 Paperclip 插件能力、secret、日志、UI 信任边界和网络出口，并完成自托管安装验证。
- [ ] 5.9 编写 Paperclip 配置、同步恢复、审批错误、禁用回滚和上游升级 runbook。
- [ ] 5.10 完成团队记忆第一层：最近使用 Agent、偏好、事实与习惯的持久化和测试。
- [ ] 5.11 实现记忆可见、可删、来源时间与注入溯源，验证不跨用户泄漏。
- [ ] 5.12 固化共享事件群聊、oncall 排班、告警路由与审批候选人的设计边界，不改变双签数量约束。
- [ ] 5.13 在事件频道中显式展示 Incident Commander、Ops、Comms、Scribe、Agent 和审批权角色。
- [ ] 5.14 补齐证据优先审批卡、normalized plan preview、hash 绑定、双签状态和 break-glass 审计。

## 6. P2 发布准备与全量验证

- [ ] 6.1 更新 `VERSION`、`RELEASE_VERSION.json`、release scope、TeamHarness 版本、插件版本和 artifact digest。
- [ ] 6.2 建立新的 release tag 命名和签名流程，不再复用 9 月 RC 元数据。
- [ ] 6.3 运行完整 Go module test、race、integration、插件测试、Web test/typecheck/build 和 protocol validate。
- [ ] 6.4 运行 fake 模式 E2E、真实推理链路 E2E、公网 staging E2E 和 Paperclip E2E。
- [ ] 6.5 完成安全审查：secret、日志脱敏、越权、跨租户、prompt injection、审批绕过、审计缺失和网络出口。
- [ ] 6.6 完成部署演练、数据迁移演练、插件回滚演练和上一版本回滚演练。
- [ ] 6.7 更新架构图、业务板块图、能力矩阵、运维手册和升级手册。
- [ ] 6.8 形成 release readiness 报告，列出已通过闸门、剩余风险、回滚条件和观察窗口。

## 7. OPC 小队执行交接与线上落地

- [x] 7.0 移交前确认 OPC 项目过时历史阶段任务已收口，且未完成相关历史阶段任务数量为 0。
- [x] 7.1 本计划经用户确认后，在 OPC 创建总执行任务和阶段里程碑，关联本 change 与五个既有活跃 change。
- [x] 7.2 按 Runtime 治理、PiG adapter、插件能力、Paperclip、团队协同、发布验证拆分 OPC 子任务，不合并无关验收项。
- [x] 7.3 为每个 OPC 子任务指定负责人、评审人、优先级、依赖、预计范围、验收命令和回滚方式；负责人为 OPC 小队，评审由小队队长路由到对应阶段执行者与独立评审角色。
- [ ] 7.4 要求执行 PR 使用中文注释和中文提交说明，并关联 OPC 任务编号、OpenSpec change 与任务编号。
- [ ] 7.5 建立执行看板字段：未开始、设计中、实现中、待审、待验证、已合并、staging 验证、生产完成、阻塞、放弃。
- [ ] 7.6 建立每日进度回写机制：完成项、阻塞项、风险、下一步、测试证据和是否影响阶段出口。
- [ ] 7.7 OPC 小队在线上环境按阶段执行部署、冒烟、观察和验收，禁止跳过 P0 依赖。
- [ ] 7.8 每个阶段完成后由计划负责人核对 OpenSpec 勾选状态、OPC 任务状态、代码合并状态和测试证据。
- [ ] 7.9 线上验收发现回归时，先执行回滚或禁用开关，再创建修复任务并更新根因、影响和恢复时间。
- [ ] 7.10 所有活跃 change 达到验收并归档后，关闭 OPC 总任务并输出最终交付报告。

## 8. 计划验收

- [ ] 8.1 用户确认 proposal、design 和 tasks 符合预期。
- [ ] 8.2 OpenSpec 校验通过，Comet 状态进入下一阶段。
- [x] 8.3 OPC 执行包创建完成，任务、负责人、验收和回滚均可在线上追踪。
- [ ] 8.4 总体路线图覆盖运行时治理、PiG、插件能力、Paperclip、团队协同、发布准备和线上交接，无未解释的旧业务板块缺口。

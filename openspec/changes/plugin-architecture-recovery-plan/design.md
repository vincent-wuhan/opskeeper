## Context

当前 `main` 已经完成从旧单体演示架构到 OpsKeeper 2.0 微内核插件架构的转型：

- 旧 release 的 71 个业务路由在当前 `main` 全部保留，并新增 Home、Discover、Node Agents、Plugins 和 Crystallized 等入口。
- 微内核定义了六个封闭扩展点，Manager 与 Edge 均已接入插件宿主。
- 五个 SRE 插件包共声明 94 个唯一工具，覆盖 L1 只读诊断、L2 修复和 L3 受限自治。
- 插件市场、manifest、能力清单、签名、灰度、回滚和审计链路已具备基础。

主要未闭环项集中在执行与治理层：

- `runtime-management-gap` 为 0/41，缺少统一 runtime identity、health、capacity、lease、failover 和任务生命周期。
- `pig-runtime-adapter` 为 0/33，PiG 进程、能力声明、沙箱、取消、崩溃恢复与审计尚未统一接入。
- `opskeeper-paperclip-feasibility` 为 17/27，只读同步、审批桥、E2E、安全审查和自托管安装未完成。
- `team-memory-and-collaboration` 为 3/14，记忆安全、共享事件群聊和排班协同未完成。
- `ops-daily-brief` 为 12/15，剩余验证与走查。
- `VERSION` 与 `RELEASE_VERSION.json` 仍停留在 9 月 RC 快照，与当前 TeamHarness `1.0.75` 和 10 月架构演进漂移。
- `/admin/runtime` 对后端返回的 `skills: null` 缺少兜底，会出现白屏。

## Goals / Non-Goals

**Goals:**

- 形成一份可执行的插件化业务能力恢复路线图。
- 保持当前 `main` 架构为唯一基线，不在旧 release 上继续补能力。
- 用阶段闸门控制运行时治理、PiG、插件、协同、企业集成和发布准备的顺序。
- 明确每个阶段的测试、安全、E2E、发布和回滚验收。
- 计划完成后形成 OPC 小队线上执行包，并要求执行状态、风险和验收结果回写到 OPC。
- 保留并强化旧 release 已具备的业务入口与核心闭环。

**Non-Goals:**

- 不替代或合并五个既有活跃 OpenSpec 的任务清单。
- 不在本计划中直接启用 Paperclip 审批桥或任何 L3 自治能力。
- 不引入运行时加载第三方 Go 代码的机制；实现仍通过 manifest 声明与编译内贡献注册。
- 不把 TeamHarness、Paperclip、Pient 或 PiG 编译成 OpsKeeper 控制面运行时依赖。
- 不重写全部旧业务页面；仅按能力缺口补运行时、插件市场和协同体验。
- 不在计划阶段修改生产配置或轮换生产凭据。

## Decisions

### 1. 采用统筹 change，而不是扩大既有 change 范围

本 change 只负责路线图、依赖、验收与交接。具体需求继续由 `runtime-management-gap`、`pig-runtime-adapter`、Paperclip 和团队协同等活跃 change 承载。

理由：这些 change 已有详细任务与验收口径；把所有实现塞进一个 change 会掩盖真实进度并扩大评审风险。

替代方案被拒绝：把所有剩余任务复制到本 change。该方案会制造双权威清单，导致任务完成状态不一致。

### 2. 先稳定能力清单与运行时底座，再丰富插件能力

执行顺序为：

1. 修复 Runtime 页面空值兼容。
2. 补齐 release 元数据与基线验证。
3. 完成 runtime identity、inventory、health 和任务生命周期。
4. 接入 PiG adapter。
5. 按六个扩展点逐步丰富插件组合。
6. 完成 Paperclip 与团队协同。

理由：插件数量增加后，如果没有统一运行时治理，会放大重复执行、能力漂移、失败恢复和审计缺口。

### 3. 插件能力必须映射到六个封闭扩展点

新增能力默认按以下方式归类：

- 可调用动作：`tool`
- 资源访问：`adapter`
- 查询后端：`datasource`
- 边缘采集：`collector`
- 模型服务：`provider`
- 外发通知：`notifier`

理由：封闭扩展点让准入、清单、回滚和安全策略可推理；新增随意 kind 会绕过宿主治理。

### 4. Runtime 管理是 PiG adapter 的前置阶段

先定义统一 runtime identity、health、capacity、lease 和 artifact digest，再让 PiG 作为一种受治理 runtime driver 接入。

理由：先接驱动再补治理会导致 PiG 成为特殊路径，后续难以与 Edge Agent、Chat Worker、AgentTeams Worker 统一调度和恢复。

### 5. Paperclip 采用只读到受控审批的两段路径

第一阶段只做 incident、timeline、RCA、evidence 与 deep link 的只读同步；第二阶段在 `approvalBridgeEnabled=false` 默认值下实现审批桥，并通过幂等、hash、actor、replay 和审计测试后才允许灰度启用。

理由：企业流程集成价值高，但错误审批或旧状态审批的风险也高。

### 6. 团队协同以角色和记忆安全为边界

先完成最近使用、偏好与事实记忆的第一层，再设计共享事件群聊、值班和审批候选人联动。所有记忆必须可见、可删、可溯源，不跨用户泄漏。

理由：协同不能以绕过审批权和用户边界为代价。

### 7. OPC 小队负责计划落地，但阶段闸门不由小队自行放宽

计划确认后生成 OPC 执行包。每个任务必须携带：

- 来源 change 与任务编号
- 负责角色
- 分支与 PR 要求
- 验收命令
- 安全边界
- 回滚方式
- 线上状态回写要求

小队可以并行执行互不阻塞的任务，但不能跳过 P0 依赖或降低验收标准。

### 8. 发布采用阶段性快照和可回滚策略

在 Runtime 修复和基础验证后建立新的开发基线；在 runtime 管理与 PiG 首切片完成后建立候选发布基线；在 Paperclip 与协同完成后建立企业闭环候选基线。

每个基线必须更新版本元数据、能力矩阵、测试证据和回滚步骤。

## Risks / Trade-offs

- [范围过大导致长期不能发布] → 拆成 P0/P1/P2 阶段闸门，每阶段都产出可验证快照。
- [运行时治理设计被 PiG 特例侵蚀] → PiG 必须实现通用 adapter port，不直接写入业务调度例外。
- [插件清单与实际实现漂移] → 以 manifest 为唯一能力声明，保持签名、版本、兼容矩阵和工具覆盖测试。
- [Paperclip 审批桥被误启用] → 默认关闭，启用需要独立配置、双签、hash 绑定和 E2E 证据。
- [团队记忆泄漏或注入不可溯源] → 先实现可见、可删、来源时间与注入记录，再做共享记忆。
- [OPC 小队任务状态与仓库不同步] → 要求每个 PR 描述关联 OPC 任务，合并和部署后回写状态、证据与剩余风险。
- [公网演示环境与新 main 架构偏差] → 建立独立 staging 部署和环境快照，公网切换前完成只读 E2E。
- [release 元数据漂移] → 将 `VERSION`、`RELEASE_VERSION.json`、TeamHarness 版本、manifest 总数和 artifact digest 纳入发布闸门。

## Migration Plan

1. **P0 稳定基线**
   - 修复 Runtime 页面空值白屏。
   - 运行模块、插件、前端和 ledger 验证。
   - 梳理 release 元数据漂移，建立新的版本命名规则。
   - 冻结当前 `main` 作为计划执行基线。

2. **P0 Runtime 治理首切片**
   - 统一 runtime identity、health、capacity 和 inventory。
   - 接入 Manager、Edge、Chat Worker、AgentTeams Worker、TeamHarness 与 preview runner 的只读适配器。
   - 建立任务 claim、lease、heartbeat、cancel、result 和 stale recovery 模型。

3. **P1 PiG adapter**
   - 定义 runtime adapter port 与 PiG release identity。
   - 用 fake PiG 覆盖启动、注册、心跳、进度、取消、崩溃和完成。
   - 完成工具 ID、读写级别、沙箱、凭据隔离和审计映射。

4. **P1 插件能力恢复**
   - 将新增资源接入、数据源、通知和模型能力按六个扩展点拆分。
   - 优先补观测、只读诊断和 repair/verify 差距。
   - 保持每个包独立版本、manifest、测试和回滚。

5. **P1/P2 企业与协同**
   - 完成 Paperclip 只读面。
   - 实现默认关闭审批桥并完成安全测试。
   - 完成团队记忆第一层和事件协同角色设计。

6. **P2 发布与线上落地**
   - 全量回归、安全审查、部署演练和回滚演练。
   - 更新 release 元数据与能力矩阵。
   - 通过 OPC 小队执行灰度、观察、验收和归档。

回滚策略：

- 插件包按名称和版本回滚。
- Runtime 与任务模型迁移保留旧读路径和迁移前快照。
- PiG 采用 opt-in 固定二进制，禁用后回落到现有 runtime。
- Paperclip 审批桥保持默认关闭，可独立禁用。
- Web 与控制面镜像保留上一版本标签和数据库迁移回滚说明。

## Open Questions

- OPC 小队内每个阶段的 RACI 负责人需要在执行包创建时确认。
- 企业客户的 Paperclip 环境版本、身份系统和审批策略需要在集成测试环境冻结。
- PiG 正式 release artifact 的签名与分发渠道需要在 adapter 实施前确认。
- 是否为 runtime 管理引入独立数据库表族，需以 `runtime-management-gap` 的模型设计为准。

# OpsKeeper 决赛主路演剧本

## 演示原则

- 主线只讲 PostgreSQL 应用连接池耗尽一个事故，避免同时展开 CPU、锁等待等备选案例。
- `home` 展示真实业务体感；`rooms` 展示 Manager 与多角色协作；`teams` 展示任务看板与 Archive 权威证据。
- baseline、Candidate A、Candidate B、人工审批、恢复验证、完整对比表必须逐项读出，不用“已自动修复”代替证据。
- 预演 PASS 只表示具备进入人工审批的资格；真正变更必须绑定 incident、candidate、execution、target fingerprint、scope、参数和有效期。
- Candidate B 在预演阶段被拒绝，不能进入正式 HITL。
- 本环境是单机 Docker 可靠性增强，不是 PolarDB HA；preview-pg 是固定负载重建，不声明复制原实例活动会话。

## 入口与版本

| 用途 | 地址 | 必须状态 |
|---|---|---|
| 业务控制台 | `https://opskeeper.yueming.xin/live-incident` | HTTP 200，三张卡片 baseline 200 |
| Manager / Archive | `https://opskeeper.yueming.xin` | HTTP 200，`/readyz` 200 |
| AgentTeams Dashboard | `https://teams.yueming.xin` | HTTP 200，插件版本一致 |
| TeamHarness 工作台 | `https://teams.yueming.xin/#plugin-route:opskeeper-teamharness/home` | HTTP 200，Archive 可读 |
| Element 房间 | `https://rooms.yueming.xin/#/room/#benyue-lumos-ops:matrix-local.agentteams.io:18080` | HTTP 200，Manager 与角色可见 |
| 监控 | `https://teams.yueming.xin/#plugin-route:monitor-panel/monitor` | Prometheus 查询 `opskeeper_pool_fixture_active_connections / opskeeper_pool_fixture_capacity` |
| Preview 深链 | `https://opskeeper.yueming.xin/preview/` | 只作为 Archive 后的深链，主流程在 home/rooms 看紧凑决策卡 |

开场前执行 `scripts/verify-final-demo.sh --dry-run` 校验配置；正式彩排用唯一 `SCENARIO_IDEMPOTENCY_KEY` 执行 live 模式。脚本会回读：

- `EXPECTED_MANAGER_VERSION`
- `EXPECTED_PLUGIN_VERSION`
- `/readyz`
- 四个公网域名 HTTP 200
- 订单、库存、审计 baseline 200
- 注入后至少一个业务 API 503或超过延迟阈值
- 满载与恢复后的 active/capacity
- Archive `evidence_complete=true`
- Candidate A `PASS` 与 Candidate B `FAIL/REJECTED_BY_PREVIEW`

当前最终演示契约固定为注入后 `4/4` 满载，Candidate A 修复后容量调整为 `8`。公网 readback 必须同时确认 active=4、capacity=4、恢复 capacity=8；任何版本不一致时停止演示。

## 0:00–0:30 开场：业务正常与运维痛点

屏幕：`home/live-incident`

1. 指向订单、库存、审计三张卡片：
   - “这三张卡片不是静态假数据，它们分别走真实 SQL 查询和服务端连接池。”
   - 读出三张卡片均 200，并记录正常延迟。
2. 指向阶段轨道：
   - “传统故障处理要跨监控、聊天、审批、数据库多个控制台；这里由 Manager 统一编排证据、审批和验证。”
3. 版本卡口：
   - “演示前已确认 Manager `$EXPECTED_MANAGER_VERSION`、TeamHarness `$EXPECTED_PLUGIN_VERSION`，避免公网混版。”

操作要求：不要点击注入；先确认三张卡片全部正常，页面壳无错误。

## 0:30–1:00 注入：业务体感与监控确认

屏幕：`home/live-incident`

1. 点击“一键注入”或运行脚本的真实场景 API。
2. 等待 5–10 秒，逐项说明：
   - “页面壳仍可用，说明故障被控制在数据库查询链路。”
   - “订单/库存/审计至少一张进入 503 或超时，业务体感已经出现。”
3. 切到监控，查询：
   ```promql
   max(opskeeper_pool_fixture_active_connections{pool_manifest_id="<manifest_id>"})
   /
   max(opskeeper_pool_fixture_capacity{pool_manifest_id="<manifest_id>"})
   ```
4. 说：“应用连接池满载，业务查询在等待连接；这是受控负载，不是无边界压测。”

预期值：满载比例为 100%，且 active/capacity readback 为 `4/4`。不要把其他满载比例解释成同一契约。

操作警告：注入 key 必须唯一；重复点击同一 key 应返回同一 incident/scenario，而不是新事故。若卡片全部仍 200，停止演示并走回滚/TTL。

## 1:00–4:00 协作、预演与人工审批

### 1:00–1:30 告警与 Manager 主持

屏幕：`rooms/#benyue-lumos-ops`

话术：

> Prometheus 告警按 fingerprint 关联到同一个 incident，没有创建第二个事故。Manager 开始主持调查、修复、验证的角色分工，每一步都会写入权威工作流事件。

检查：

- Manager 发出 `preview_ready` / `awaiting_approval` 等权威 `agentteams.workflow` 消息。
- Element 与 Dashboard 阶段一致。
- 没有普通房间消息能伪造阶段推进。

### 1:30–2:15 诊断与受控预演

屏幕：先看 rooms，再切 home 的紧凑决策卡。

话术：

> 诊断定位到应用侧连接池耗尽。修复不直接上线，先在 preview-pg 固定负载下做受控对比。这个预演不复制生产活动会话，而是重建可解释的连接池等待负载。

读出三类证据：

1. baseline：连接池满载、业务探测受影响。
2. Candidate A：bounded pool resize + recycling，数据一致、写入影响受控，`PASS`。
3. Candidate B：aggressive reset/session clear，业务探针或一致性失败，`REJECTED_BY_PREVIEW`。

明确边界：

> Candidate A 的 PASS 只是获得人工审批资格，不等于已批准。Candidate B 在预演门禁被拒绝，不能进入正式 HITL。

### 2:15–3:00 人工审批

屏幕：home 人工动作提示词与 rooms 审批卡。

复制 home 显示的提示词：

```text
请人工审批 incident <incident_id> 的 Candidate A 修复方案。请逐项核对 candidate_id、execution_id、目标指纹、影响范围、参数与有效期；预演 PASS 只代表具备审批资格，不等于人工批准。
```

操作：

1. 只审批 Candidate A。
2. 逐项核对 candidate、execution、incident、target fingerprint、scope、参数、有效期。
3. 拒绝 Candidate B；说明它已被 preview gate 阻断。

话术：

> 人工批准绑定的是这份精确提案。修复执行会校验 proposal 与目标指纹，重试不能绕过审批重复执行。

### 3:00–4:00 修复与独立验证

屏幕：rooms 中 repairer 与 verifier。

话术：

> repairer 通过已批准的 recovery.execute 执行 bounded resize；verifier 独立检查业务查询、连接池指标和恢复信号。Manager 只推进被证据支撑的状态。

检查：

- `repair_dispatched`、`verifying`、`recovered` 阶段在 Element 和 Dashboard 同步出现。
- recovery.execute 返回一次成功；不要手工调用 fixture recover 或其他旁路修复。
- Manager 若重启，待审批/待验证状态仍应保留。

## 4:00–4:30 恢复、关闭与 Archive 收尾

1. 切回监控，展示 active/capacity 从满载降到阈值以下。
2. 切回 home，不刷新或仅刷新一次：
   - 订单 200
   - 库存 200
   - 审计 200
   - 延迟回到正常区间
3. 说：“业务查询仍走原链路，没有静态 fallback 伪装恢复。”
4. 在现有 incident closure API/UI 中关闭事故，复制 home 提示词：

```text
请对 incident <incident_id> 执行档案关闭：确认业务查询、连接池指标和独立验证结果，然后归档完整证据链与 A/B 预演对比表。
```

5. 切到 teams 的 Archive / preview 深链，展示完整表：
   - baseline
   - Candidate A PASS，人工批准后执行
   - Candidate B REJECTED_BY_PREVIEW，未进入正式变更
   - `evidence_complete=true`
   - 七类事件与 trace_id 可反查

结束语：

> OpsKeeper 的价值不是自动点按钮，而是把调查、修复、验证、审批和恢复证据连成一条可审计闭环。插件把能力接入 AgentTeams，但 Manager 保留权限与事实源，预演和人工审批共同保证影响面可控。

## 操作者防呆与回退

- 注入前确认版本矩阵和 `/readyz`；任何版本不一致都不开始。
- 每次演示使用新的 idempotency key；同 key 重复读必须返回同 ID。
- 三张 baseline 卡片不全 200 时不注入。
- 告警未关联时停止，不能手工创建第二个 incident。
- preview fingerprint 显示 `NOT COMPARABLE` 时停止，不能进入审批。
- Candidate B 不允许通过提示词“强制批准”。
- 修复只允许走已批准 proposal 绑定的 recovery.execute。
- 恢复验证失败时等待 TTL 或使用既有回滚流程，不得伪造成功。
- Archive 缺事件时先补齐真实事件，不修改前端显示。

## Q&A 边界与备选

- 未使用 PolarDB HA：当前是单机 Docker 可靠性增强，生产演进路径是独立控制面数据库、备份恢复、高可用和跨可用区。
- preview-pg：固定负载、独立预演环境，不复制原实例活动会话；锁等待、CPU 飙高可在 Q&A 中切换备选案例。
- 加密：Element 本身支持端到端加密，但决赛主线使用未加密演示房间保证可观察性；不在材料中把房间传输形态夸大为产品级密钥管理。
- 长期架构：home 业务面应演进为独立 `home-app`；当前 pool-fixture 承载轻量业务查询是比赛阶段的受控实现。

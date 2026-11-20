## Why

4.8 方案（`plugin-gap-plan.md` §3.1 / §5.2 / §5.4）判定的五个未恢复工具里有三个归 readonly 包：`query_incidents`、`get_incident_detail`、`query_devices`。它们在管理端有实现（BaseTool，由 `Registry.BuildBaseTools()` 装配，闭包注册表同名录也在），上呼面 S-1 修完即可服务；但 readonly 的 manifest 没有声明它们，于是节点 gate 在每一轮都拒绝调用——能力可达、声明缺失，这正是 G0 缺口表格里"上呼可服务、manifest 未声明"方向的另一半收口。

三个工具的理由已在方案里裁决（§5.2 / §5.4，评审裁决 D-3 采纳 readonly 方案 A）：

- **`query_incidents` / `get_incident_detail`**：事件时间线与既在 readonly 的 `query_alert_rules` 同域（"what is firing / what happened"），readonly 已持有 `alert.read` scope。
- **`query_devices`**：设备清单是拓扑域的入口读——`get_topology` / `find_topology_node` / `get_edge_summary` 都需要 device_id 先行，readonly 已持有 `topology.read`。

## What Changes

- `plugins/pig-ops/opskeeper-sre-readonly/pig-ops.yaml`：
  - `metadata.version` 0.2.0 → **0.3.0**（菜单新增即 schema 增量）。
  - `spec.tools` 增三条：`get_incident_detail`、`query_devices`、`query_incidents`（按名排序插入，class read，逐条注释；**不写 limits**——与方案 §5.2/§5.4 的字面偏差，理由见 design.md：sdk 的 Check 比较两侧 limits，上呼工具两侧都只有零值，写了即红；本仓既有上呼工具同样不写）。
- `core/pig/extensions/opskeeper-sre-readonly/tools.go`：手工清单按三个工具的 `Info()` 原样同步三条 spec（schema 逐字来自 `alerting.QueryIncidentsSchema`、`alerting.GetIncidentDetailBatchSchema`、`QueryEdgesSchema`；该包不走生成链，漂移由 `TestThePackagedToolsetMatchesTheCanonicalSource` + `TestEveryControlPlaneToolTheNodeOffersIsRegisteredHere` 兜住）。
- `scripts/sync-pig-ops.sh` 运行一次，把 canonical 清单拷进包内副本。
- `core/manager/biz/aiops/tools/profile_contract_test.go`：控制面工具 ↔ readonly 声明的对照清单加上三个名字（两个方向的断言各加三条）。
- persona 使用边界：readonly 的 `diagnose-readonly` SKILL 在"检查最近变更"一步写明事件读（`query_incidents` / `get_incident_detail`）与设备清单（`query_devices`）的入口位置——菜单从 20 变 23，persona 必须告诉模型这三个名字是干什么的，否则新菜单只是三个没人会用的名字。
- `core/floor/pluginmanifest/observability_profile_test.go` 的"twelve tools"计数措辞不在本切片（那是 observability 的 S-3）。

### 零 scope 增量（§5.2 / §5.4 已核对）

`alert.read` 与 `topology.read` 都 ⊆ readonly 既有 `required_scopes` ⊆ finance 与 SaaS 两 profile Granted。**不改** `core/domain/plugin.go`、**不改** `core/floor/pluginmanifest/profiles.go`。

### 明确不在本切片内

- 不动 observability 包（`query_knowledge` / `get_host_processes` 与 `knowledge.read` 是 S-3）。
- 不新增闭包注册表条目：三个工具的闭包形态 S-1 之前就存在，本切片只是声明面收口。
- 不改 `host_*` 本地探针面、不改 profile 组合。

## Capabilities

### New Capabilities

- `plugin-gap-slice-s2`：readonly 包 0.3.0 的菜单增量契约——三个新增声明与工具清单、执行器 `Info()` 三方一致，且 persona 说明与菜单同步。

### Modified Capabilities

- 无。本仓库尚无归档的主规格；本切片是既有能力的声明面收口，不改变已归档能力的对外语义。

## Impact

- `plugins/pig-ops/opskeeper-sre-readonly/pig-ops.yaml`：version + 三条 tools。
- `core/pig/extensions/opskeeper-sre-readonly/tools.go`：三条 toolSpec（canonical）。
- `plugins/pig-ops/opskeeper-sre-readonly/extensions/opskeeper-sre-readonly/tools.go`：生成副本（sync 脚本产出）。
- `core/manager/biz/aiops/tools/profile_contract_test.go`：对照清单 +3。
- `plugins/pig-ops/opskeeper-sre-readonly/skills/diagnose-readonly/SKILL.md`：使用边界同步。
- 无 Go 行为改动：三个工具的执行路径（BaseTool 桥 + 闭包注册表）在 S-1 与更早已交付。
- 回滚：包级回退（发布 0.2.0 回退版本或停用 0.3.0 release），节点 reload 后 manifest 回到 20 工具，gate 对新名字自然拒绝（fail-closed 默认行为），无需代码回退。

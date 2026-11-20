# Design: plugin-gap-slice-s2

本文件是 4.8 方案切片 S-2（readonly 0.3.0）的实现输入。方案依据：`plugin-gap-plan.md` §5.0（通用生成链）、§5.2（事件读）、§5.4（设备清单）、§6.3（D-3 裁决：事件读归 readonly）。

## 一、为什么只剩声明面

三个工具的两半都已经存在：

1. **实现**：`core/manager/biz/aiops/tools` 里 `alerting.NewQueryIncidentsTool` / `alerting.NewGetIncidentDetailTool`（BaseTool，`r.alertUC` 门控）与 `NewQueryEdgesTool`（`r.edges || r.devices` 门控）。闭包注册表同名录也在（`query_incidents.go` / `query_edges.go`，节点上呼走 `Registry.Invoke` 的老路）。
2. **可服务**：S-1 的 BaseTool 桥把 BaseTool 形态接上上呼通道，且同名以桥优先——节点发来的参数按 BaseTool 的 schema 构造。

缺的是 readonly 的 `pig-ops.yaml` 没有这三个名字。节点 gate 的 allow-list 就是 manifest，于是模型看得见的三个能力在每一轮被拒。这不是新能力交付，是**把已经能跑的东西写进承诺它的那份文件**。

## 二、三条工具的对照表（manifest ↔ Info() ↔ scope）

| 名字 | Info() 出处 | class | scope 对照 | limits |
| --- | --- | --- | --- | --- |
| `query_incidents` | `alerting.QueryIncidentsSchema`（severity / status / since_minutes / edge_id / rule_key / limit） | read | `alert.read` ⊆ 既有 | 无（见下） |
| `get_incident_detail` | `alerting.GetIncidentDetailBatchSchema`（incident_ids[] 1..16 批量信封） | read | `alert.read` ⊆ 既有 | 无（见下） |
| `query_devices` | `QueryEdgesSchema`（role / status / last_seen_within_minutes / name_contains / ip_contains / limit；返回 device_id 数组） | read | `topology.read` ⊆ 既有 | 无（见下） |

**与方案 §5.2 / §5.4 的偏差（limits 不写）**：方案给三条都写了 `limits: {output_bytes: 262144}`，本切片不写。理由是 `sdk_registry_wiring_test.go` 的 `Check` 比较"作者侧注册的 limits ↔ manifest 声明的 limits"，而控制面上呼工具在 PiG `ToolDefinition` 里没有 limits 字段、在 `skill` 注册表里没有执行器，两侧都只能注册零值——写了 limits 的 manifest 会被 `TestTheShippedPackagesPassTheSdkRegistryCheck` 判红。本仓所有既有控制面上呼工具（`query_alert_rules` / `expand_topology` / `query_promql` 等）都不写 limits，正是同一原因。三条工具的返回量另有真实约束：`query_incidents` 有 `limit`（≤500 行）、`get_incident_detail` 有 `maxItems: 16`、`query_devices` 有 `limit`（≤500 行），schema 即护栏。方案的 output_bytes 意图由既有 host 默认（1 MiB）承接。

注意 `get_incident_detail` 的 BaseTool 形态是 **N+15 批量信封**（`incident_ids[]`），闭包形态是单 id。节点 schema 由 BaseTool 的 `Info()` 生成，所以手工清单必须抄批量信封那份 schema——抄闭包那份会让节点按单 id 构造参数、执行器按批量解析。这是 S-1 design §2.2"同名以桥优先"在 schema 侧的镜像。

`query_devices` 的 wire name 与 Go 类型名（`QueryEdgesTool`）不同：类型名是历史（legacy go id: query_edges），wire name 是模型看到的。manifest 与工具清单都写 wire name。

## 三、手工清单的抄录纪律

readonly 的 `core/pig/extensions/opskeeper-sre-readonly/tools.go` 不走生成链（observability 走 `OPSKEEPER_UPDATE_TOOLSET`，readonly 是手工清单）。抄录三条规则：

1. **schema 逐字**：`Parameters` 字段从 `Info().Parameters` 原样抄，含中文描述与 maxItems 注释——一个 range bound 抄错就是一个模型按错误边界构造参数。
2. **Description 用 `Info().Description`**，不用 `WhenToUse`（工具清单的 Description 是模型决定调不调的依据，与 manager 面同义）。
3. **排序**：清单按名排序，三条分别插到 `get_topology` 之后（`get_incident_detail`）、`host_*` 之前（`query_devices` 在 `host_*` 前？不——排序是字符串序：`get_incident_detail` 在 `get_topology` 前、`query_alert_rules` 后是 `query_devices` / `query_incidents` 在 `host_*` 之后 `query_alert_rules` 之前）。排序由 `TestTheProfilesToolListIsTheAllowListTheHostWillBuild` 钉住，插错位置直接红。

抄录后的兜底：`TestEveryControlPlaneToolTheNodeOffersIsRegisteredHere`（名字在声明里）+ `TestTheGapGateIsRedWhenTheBaseToolBridgeIsNotWired` 的闸门（名字可服务）+ `TestThePackagedToolsetMatchesTheCanonicalSource`（canonical 与包内副本一致）。schema 与执行器的逐字一致没有 readonly 侧的生成链兜底——这是 4.8 §5.0 明知并接受的既有代价，本切片不扩大它也不修它（修它属于另一个 change）。

## 四、persona 同步

菜单 20 → 23。`diagnose-readonly` 的"Check recent change"一步目前只提 deploy / config / scaling，没有告诉模型去哪里看事件。同步内容：

- 事件时间线入口：`query_incidents`（列表）→ `get_incident_detail`（单事件全量时间线，批量信封）。
- 设备清单入口：`query_devices`——"narrow the search"一步的 device_id 来源。

措辞保持该 SKILL 既有风格（程序步骤 + 不做什么 + 汇报顺序），不引入新的承诺面。

## 五、闸门与验收

- `cmd/opskeeper/plugin_gap_gate_test.go` **不改**：闸门读 manifest，加名自动纳入对账（正向：三名字 ∈ 可服务 ∪ 本地；负例的 6 个名字清单不包含这三个——它们闭包面可服务，拿掉桥后由闭包兜底，闸门不报）。这正是不改闸门的理由：S-1 把闸门写成读 manifest 而不是读字面量，本切片就是第一个受益者。
- `profile_contract_test.go` 加三条：该测试是"控制面提供 ↔ 节点声明"的第四向对账，S-2 的三个名字进它的清单后，从 manifest 里删掉任何一个都会在那里红。
- 闸门命令：`make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck -count=1`、`go test ./...`、`make audit-open-source`、`make deadcode-ratchet-check`。

## 六、回滚

包级回退（§8.3）：发布 0.2.0 回退版本或停用 0.3.0 release，节点 reload 后回到 20 工具菜单；gate 对三个新名字 fail-closed 拒绝，无需清理。manager 侧代码零改动，回滚不触碰任何 Go 面。

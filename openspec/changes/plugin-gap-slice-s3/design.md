# Design: plugin-gap-slice-s3

本文件是 4.8 方案切片 S-3（observability 0.3.0）的实现输入。方案依据：`plugin-gap-plan.md` §5.0（通用生成链）、§5.1（query_knowledge 与 knowledge.read）、§5.3（get_host_processes）、§4.1 的措辞要求（S-1 tasks 7.2 的降级分句）。

## 一、两个工具的现状与缺口

| 名字 | 实现面 | 可服务性 | 缺口 |
| --- | --- | --- | --- |
| `query_knowledge` | `tools.NewQueryKnowledgeTool`（BaseTool，`r.knowledge` 门控；闭包面无） | S-1 桥可服务（bag 内即进桥） | manifest 未声明、scope 未定义 |
| `get_host_processes` | `host.NewGetProcessListTool`（BaseTool，无条件注册——只需常接的 caller/edges/devices）；闭包面同名存在 | 桥 + 闭包均可 | manifest 未声明 |

`get_host_processes` 的 wire name（`get_host_processes`）与 Go 类型名（`GetProcessListTool` / `ToolNameGetProcessList`）不同——类型名是历史（get_process_list），wire name 是 `host/identity.go` 里 `ToolNameGetProcessList = "get_host_processes"` 的现值。manifest 与生成清单都写 wire name。

## 二、生成链（与 readonly 的差别）

observability 走生成链，不需要手工抄录：

1. `core/manager/biz/aiops/tools/observability_toolset_test.go` 的 `observabilityTools()` 清单 +2 条（`host.ToolNameGetProcessList` 用 `host.NewGetProcessListTool(nil,nil,nil,nil)`、`ToolNameQueryKnowledge` 用 `NewQueryKnowledgeTool(nil,nil)`——两个构造器对 nil 依赖都是构造期安全的，`Info()` 不碰依赖）。
2. `OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/biz/aiops/tools/ -run Toolset` 重生成 `core/pig/extensions/opskeeper-sre-observability/tools.go`（按名排序自动插入：get_host_processes 在 get_host_load 后、query_knowledge 在 query_change_events 与 query_logql 之间）。
3. `scripts/sync-pig-ops.sh` 同步包内副本（重写 wire import，重写 go.mod/go.sum）。

生成器自带两道护栏：`info.Name != e.wireName`（名字两侧分叉即红）与 `info.Class != "read"`（L1 包混入非读即红）。这两条在本切片先行成立：两个工具的 `Info().Class` 均为 `"read"`。

**与方案 §5.1 / §5.3 的偏差（limits 不写）**：与 S-2 同因——`sdk_registry_wiring_test.go` 的 `Check` 比较"作者侧注册的 limits ↔ manifest 声明的 limits"，而控制面上呼工具在 PiG `ToolDefinition` 里没有 limits 字段、在 `skill` 注册表里没有执行器，两侧都只能注册零值，写了 limits 即红。本仓所有既有控制面上呼工具都不写 limits。两个工具的返回量另有真实约束：`query_knowledge` 有 `max_results`（≤20）与每条预览 800 runes 双重上限；`get_host_processes` 有 `device_ids maxItems: 16` 与 `top_n ≤ 100`。方案的 output_bytes 意图由既有 host 默认（1 MiB）承接。

## 三、`knowledge.read` scope 增量（全计划唯一一处动 profile）

链条是三处同步，缺一处 admission 拒绝（`core/edge/pluginhost/host.go` 有"not granted"的真实节点教训）：

1. `core/domain/plugin.go`：`ScopeKnowledgeRO Scope = "knowledge.read"`，注释写明为什么不入 `observability.read`——KB 与仓库读同源（都来自 knowledge usecase 的 gitartifact/精选文档），但 scope 是凭据边界不是分类学：observability.read 注入的是 Prom/Loki/Tempo 凭据，knowledge.read 注入的是 KB 检索，折进同一个 scope 会让一次声明拿到两类凭据（与 `ScopeCloudRead` 不折进 `host.read` 同构）。
2. `core/floor/pluginmanifest/profiles.go`：finance 与 SaaS 两个 profile 的 `Granted` 都加 `domain.ScopeKnowledgeRO`。只读 scope，不动两个 profile 的 `NotGranted` 清单语义（NotGranted 列的是被拒绝的 scope，knowledge.read 被授予后不在其列）。
3. `plugins/pig-ops/opskeeper-sre-observability/pig-ops.yaml`：`required_scopes += knowledge.read`。

守卫全部既有：`TestEveryComposedPackageIsActuallyAdmittedByItsProfile`（漏第 2 步即红）、`TestTheFinanceProfileIsNotMerelyAStricterSaaS`（只加一边即红）、`TestEveryProfileNamesEveryScopeItRefusesToGrant`（NotGranted 不需要动，因为 knowledge.read 被授予）。`TestTheObservabilityProfileAsksForReadScopesAndNothingWider` 的正向清单 += `knowledge.read`、哲学注释同步（与 alert.read 同为"答案所解释的参照系"类读），负向清单不动。

## 四、7.2 就地降级：桥的第三种 miss

### 4.1 问题

`BuildBaseTools` 对可选依赖是 nil 门控（`r.knowledge == nil` 时 `query_knowledge` 整体不进 bag）。S-1 的桥只有两种 miss 措辞：`withheld`（知道但不服务——保留名单与写类）和 `errBaseToolNotHere`（这里没有→分派链继续）。一个 `r.knowledge == nil` 的部署，节点按 manifest 调 `query_knowledge`，最终听到的是闭包面的 "no such tool"——**工具存在、manifest 声明了、依赖没配，模型却被告知工具不存在**，这是 §4.1"清单与接线各说各话"的措辞版。

### 4.2 方案：给桥注入能力名清单

`newBaseToolUpcall` 增加第二参 `known map[string]bool`（本注册表知道的能力名清单）。注入点在 `main.go`：`agentTools.basetool = newBaseToolUpcall(toolsReg.BuildBaseTools(), toolsReg.KnownToolNames(), ...)`。

`KnownToolNames()`（新增于 `Registry`，manager 侧）派生自**两个面的并集**：闭包注册表的 `r.tools` 键 + BaseTool 无门控候选名的静态清单。后者需要一个静态来源而不是"再 build 一次 bag"——bag 正是随部署配置变化的那个量，再 build 一次得到的还是同一份缺失。清单因此写在 `registry_basetool.go`：`baseToolCandidateNames` 枚举 `BuildBaseTools` 可能产出的每个名字（含被 nil 门控的），由同文件一个已有测试形状守护（新增 `TestBaseToolCandidateNamesCoverEveryUnconditionalBuild`——`BuildBaseTools` 用全依赖构造时产出的每个名字 ∈ 候选清单）。

miss 分句（`basetool_upcall.go` 的 `invoke`）：

- `withheld`：知道但不服务（既有，保留名单/写类）。
- 命中 `known` 但 bag 里没有：**"存在但未配置"**——`tool is known to this control plane but not configured here (a dependency this deployment did not wire is missing)`，denied=true 带 reason，**不**返回 `errBaseToolNotHere`（分派链不再落到闭包面；闭包面对这两个名字本来就是 miss，落下去只会把好措辞换成坏措辞）。
- 其余：`errBaseToolNotHere`（分派链继续）。

对账闸门（S-1）**零改动**：闸门用全依赖 fake 构造 bag（`realregistry_test.go` 的 `stubCodeBrowser` 满足 KnowledgeSearcher），`query_knowledge` / `get_host_processes` 都在服务面内，manifest 加名自动对上。

### 4.3 明确不做

- 不给 `list_repo_sources` 等做同样改造——它们的 miss 路径是 `Info()` 后的执行期错误（"code browser not configured"），工具在 bag 里，模型听到的本来就是配置性句子。
- 不改闭包注册表（S-1 tasks 7.4）。
- 措辞不进 `promptguard`（它是输出围栏不是错误通道）。

## 五、persona 同步

菜单 12 → 14。`opskeeper-observability` SKILL 的"What the tools are for" += 两条：

- `query_knowledge`：知识库检索——"how did we handle X before"的第一跳；命中 playbook 优先于通用推断；NOT for 实时数据（那是本 persona 其余工具）。
- `get_host_processes`：一组 device 的 top N 进程比对（fleet 视角），与 `get_host_load` 成对。

措辞保持该 SKILL 既有风格，不引入新承诺面。既有"K8s 对象与消息队列不在覆盖内"的边界句不动（`TestTheObservabilityProfileShipsAPersonaThatKnowsItsOwnLimits` 守着它）。

## 六、计数与指纹同步

- `observability_profile_test.go:198`："twelve tools" → "fourteen tools"。
- `version_drift_test.go` released：observability `{"0.3.0", <sha256(14 名排序拼接)>}`（指纹在实现完成后由脚本实算替换）。
- `marketplace/registry_roundtrip_test.go` 期望版本 0.2.0 → 0.3.0；package.json 同步。
- ledger（`docs/opskeeper2-architecture.md` §六 及进度表）：observability 12 → 14、总数 97 → 99、upcall 汇总（12 可观测 + 56 中间件 = 68 → 14 + 56 = 70）。计数闸门 `TestEveryToolCountTheCurrentStateSectionQuotesMatchesWhatTheFleetShips` 逐条核对。
- capability 表：`query_knowledge` → 新家族 `knowledge`（CapSource 是代码仓库，KB 是另一套语料与检索路径，混入会让 source 家族的覆盖语义变宽）；`get_host_processes` → `CapHost`（与 get_host_load 同族——都是"对一台具名主机的读"，区别只在从控制面走还是本机走）。

## 七、闸门与验收

- `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck -count=1`
- `go test ./...`（root / core/floor / core/manager 三模块各自 GOWORK=off）
- `make audit-open-source`、`make deadcode-ratchet-check`
- 桥降级新增用例：`known` 命中 + bag 缺失 → 措辞含"not configured"且不含"no such tool"；不在 `known` → 仍 `errBaseToolNotHere`（负例：分派链行为不变）。

## 八、回滚

包级回退（方案 §8.3）：发布 0.2.0 回退版本或停用 0.3.0 release，节点 reload 后回到 12 工具菜单，gate 对两个新名字 fail-closed。`knowledge.read` 在包回退后成为无人消费者，profile 中保留无害（scope 授予是加法）；要清理随下一版 profile 收敛。桥的降级分句是加法（新 miss 分支），manager 二进制回退即消失，不触碰既有两种 miss 的行为。

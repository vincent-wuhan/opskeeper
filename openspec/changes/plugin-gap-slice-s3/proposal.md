## Why

4.8 方案（`plugin-gap-plan.md` §5.1 / §5.3）判定归 observability 包的两个未恢复工具：

- **`query_knowledge`**：知识检索是控制面数据读（Qdrant + knowledge biz 在 manager），与 observability"回答来自控制面"的 charter 一致；该包已承载同源的仓库读三件套（gitartifact 与 KB 同属知识域）。评审裁决 D-1 采纳此归属。节点 investigator 是知识回流的主要消费者（复盘 SKILL 明确先查 KB），不补则节点面每次都回退通用诊断。这是 4.2 已归 4.8 的首选项。
- **`get_host_processes`**：与既在 observability 的 `get_host_load` 成对（"任一主机的负载 / 进程"）；readonly 的 `host_top_processes` 只答本机，语义不覆盖跨主机清单。

S-1 已把 BaseTool 桥接上上呼通道；本切片是 manifest/生成链的声明面收口 + `knowledge.read` 的 scope 增量（唯一一个需要动 profile 的工具）+ §4 措辞的就地降级分句（`plugin-gap-slice-s1` tasks 7.2）。

## What Changes

- `core/domain/plugin.go`：新增 `ScopeKnowledgeRO Scope = "knowledge.read"`（与既有 `ScopeCloudRead` 同形的只读 scope，注释写明为什么 KB 读不入 `observability.read`）。
- `core/floor/pluginmanifest/profiles.go`：finance 与 SaaS 两个 profile 的 `Granted` **都**加 `knowledge.read`——不加则 admission 拒绝（`core/edge/pluginhost/host.go` 记录过"not granted"的真实节点教训）。只读 scope，不触碰两个 profile 的 NotGranted 清单语义。
- `plugins/pig-ops/opskeeper-sre-observability/pig-ops.yaml`：
  - `metadata.version` 0.2.0 → **0.3.0**。
  - `spec.tools` += `get_host_processes`、`query_knowledge`（class read，排序插入，逐条注释；**不写 limits**——与 S-2 同因：sdk Check 的两侧只有零值，写即红；`query_knowledge` 的返回量由 `max_results`（≤20）与预览 800 runes 双重约束，`get_host_processes` 由 `maxItems: 16` 与 `top_n` 约束）。
  - `required_scopes` += `knowledge.read`（与 `observability.read / db.read / alert.read` 并列）。
- `core/manager/biz/aiops/tools/observability_toolset_test.go`：`observabilityTools()` 清单 += 两条（`get_host_processes` 用 `host.NewGetProcessListTool`、`query_knowledge` 用 `NewQueryKnowledgeTool` 的 `Info()`），跑 `OPSKEEPER_UPDATE_TOOLSET=1` 重新生成 `core/pig/extensions/opskeeper-sre-observability/tools.go`，再跑 `scripts/sync-pig-ops.sh`。
- **就地降级（7.2，§4 措辞）**：`cmd/opskeeper/basetool_upcall.go` 的桥增加"已声明但依赖缺失"的第三种 miss——工具在注册表的能力清单上、但本部署的 bag 里没有（如 `r.knowledge == nil` 时 `query_knowledge` 整体不在 bag）。措辞按 §4 分句："存在但未配置"，给出操作者可行动的句子，MUST NOT 说 "no such tool"。实现方式是给桥注入一份"本注册表知道的能力名清单"（由 `BuildBaseTools` 前的注册表闭包 + BaseTool 清单并集派生），miss 时先查这份清单再分句。对账闸门的两条断言与 S-1 保持零改动。
- persona：`opskeeper-observability` SKILL 的工具说明 += `query_knowledge`（复盘先查 KB）与 `get_host_processes`（fleet 进程比对）。
- `core/floor/pluginmanifest/observability_profile_test.go`："twelve tools" 计数措辞 → 十四；scope 断言 += `knowledge.read` 并更新哲学注释（与 `alert.read` 同为"答案所解释的参照系"类读）。
- capability 表：`query_knowledge` → 新家族 `knowledge`；`get_host_processes` → `host`。
- released 指纹表、marketplace roundtrip 期望、ledger 计数（12 → 14，总数 97 → 99）同步。

### 安全标注（§5.1）

KB 内容是外部文本。`promptguard.KindSource` 对 `query_knowledge` 的标注已在 `untrusted_sources.go` 表内（S-1 之前就有，服务 manager 面），桥复用同一 bag 即继承同一标注——**零增量**，本切片只验证不重加（两层围栏会把同一字符串标记两次）。

### 明确不在本切片内

- 不做反向断言（"可服务 ⊆ 声明"）；`rank_edges` / `correlate_incident` 不进任何 manifest（S-1 tasks 7.3）。
- 不动闭包注册表（S-1 tasks 7.4）。
- 不改 readonly 包（S-2 已交付）。
- 不实现其他 S-3 项（方案 §10 只列了 query_knowledge / get_host_processes 两条，无其他）。

## Capabilities

### New Capabilities

- `plugin-gap-slice-s3`：observability 0.3.0 的菜单增量契约——两个新声明、`knowledge.read` scope 与两 profile Granted 的同步、生成链三向一致、以及上呼桥对"存在但未配置"的分句措辞。

### Modified Capabilities

- 无。本仓库尚无归档的主规格；本切片是既有能力的声明面收口与 scope 授予增量。

## Impact

- `core/domain/plugin.go`：+1 scope 常量。
- `core/floor/pluginmanifest/profiles.go`：两 profile Granted 各 +1；`composition_test` / `profile_test` 的 scope 断言同步。
- `plugins/pig-ops/opskeeper-sre-observability/pig-ops.yaml`、`package.json`：version + tools + scope。
- `core/manager/biz/aiops/tools/observability_toolset_test.go`：清单 +2；生成物 `core/pig/extensions/opskeeper-sre-observability/tools.go` 重生成；包内副本 sync。
- `cmd/opskeeper/basetool_upcall.go` + `main.go`：桥的第三种 miss（存在但未配置）与注入点。
- persona SKILL、capability 表、released 指纹、marketplace 期望、ledger 计数同步。
- 回滚：包级回退（0.2.0 回退版本或停用 0.3.0 release），gate 对新名字 fail-closed；`knowledge.read` 在包回退后成为无人消费者的 scope，profile 中保留无害（scope 授予是加法，§8.3 第 4 条）。

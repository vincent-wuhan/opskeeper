# 4.4 adapter 第一批第三方候选插件包方案（Karmada / 阿里云 ECS / CMDB）

> 状态：**方案 v0.1（待评审）**。对应 `tasks.md` 4.4；评审通过前不勾选 4.4。
>
> 基线：`main@9f8bc820`（本任务打开时的 HEAD，即 4.3 已合并的 `datasource-plugin-plan.md` 之后）；与 4.1 `business-domain-mapping.md`、4.2 结论保持口径一致。
>
> 范围：**仅方案产出**。本文档回答"插件包长什么样、怎么注册、怎么测、怎么回滚"，并给出实现切片建议（不开在本任务内合入）。任何对 `core/manager/middleware/toolset/sources.go`、`sources_plugin.go` 或 `plugins/` 包结构的真实改动，必须另开 change 并独立评审。

## 1. 目标与边界

### 1.1 目标

为 adapter 扩展点（`core/floor/kernel/kind.go` 中的 `KindAdapter`）补齐第一批**第三方**插件包方案。

`KindAdapter` 的契约是固定的：贡献体回答 `Diagnose` / `Collect` / `Execute`，并暴露 `<family>.<method>` 形式的名字空间工具（`k8s.*`、`pg.*`、`mq.*`）。第一方已有 8 个 family 通过同一道门注册，因此第三方要证明的不是"能不能写一个包"，而是"一个包写完之后，能不能在这个内核里被看见、被路由、被回滚"。

**本方案最重要的产出不是候选清单，而是这个"能被看见"的判定**：把它拆成两道宿主侧闸门（§3），逐候选说明如何通过（§4–§6），并给出一条不与任何候选耦合的前置宿主切片（§7.1）。

### 1.2 选型结论

| 候选 | family | 一句话理由 | scope 闸门 | 状态 |
|---|---|---|---|---|
| **Karmada 多集群联邦** | `karmada` | 一方 `k8s` 面向**单个**集群的 kubeconfig；跨集群传播与落地判定在当前 8 源里无任何答案，而它是"这次变更实际落到哪几台机器"的唯一来源 | `k8s.read` ⊆ 已授予 | **绿** |
| **阿里云 ECS 只读清单** | `alicloud` | 4.1 §③ 把"云资源同步"明确派给 4.4；内置凭据类型 `alicloud` 已存在（`core/domains/biz/secret/credtype.go:98`）；事故诊断第一跳就是"这台机器是哪台云主机" | `cloud.read` **未授予** | **琥珀** |
| **CMDB 只读配置项** | `cmdb` | 4.1 §③/§⑨ 把 CMDB 指向 4.4；外部 CI 变更进拓扑图，是"事故时知道最近改了什么"的地基 | `topology.read` **未授予** | **琥珀** |

**明确不选**（附理由，避免后续重复讨论）：

- **Git**：一方 `git` family 已在（`core/manager/middleware/toolset/toolset.go:56`），且其全部只读工具已被 `toolset.NotPackaged` 逐条排除（`toolset.go:230-240`，六条理由均为"与 observability 包的 `source` 系列重复"）。第三方再引入一套 `git2.*` 只会为同一答案开出第二条审计路径。
- **ITSM**：与 5.2–5.9 高度重叠（4.1 §⑦/§⑨ 已把外部审批桥与 incident sync 划给 5.x），且 ITSM 的写语义（建单/关单）落在 `alert.*` 上，而 4.1 §7 明确"告警归 4.12"。建议以 **Paperclip** 作为首个 ITSM 类候选在 5.x 落地，4.4 只保证 §7.1 的前置切片让 adapter 打包形状可复用。

### 1.3 与 4.1 §7、4.2 边界

- **4.1 §7**：嵌入模型本期编译内、MCP/技能宿主内管理、告警归 4.12 —— 本任务不引入任何新嵌入模型、不改 MCP 宿主、不动告警归类。
- **4.2 四分类**：adapter 归"迁移为插件"；本文方案严格在"第三方候选"分支内推进，"一方已完成 8 源"分支（`sources.go` / `sources_plugin.go`）已合入，本任务不重写。
- **内核契约**：不在 `core/floor/kernel/kind.go` 中新增任何 Kind 字符串（六元封闭集合保持）；不改 `allKinds`。

### 1.4 非目标

- 不实现真实代码合并；§7 的实现切片**仅作建议**，落地需另开 change。
- 不覆盖 datasource / notifier / provider / collector（各有独立任务）。
- 不动 `core/manager/middleware/toolset/sources.go`（8 源的 `Wire` 字面量）与 `sources_plugin.go` 主流程。
- 不复刻一方的注册方式：`firstPartySources()` + `familyPlugin` + `adapterManifest` 三件套是一方的**编译内**表达，第三方包读自己的 `pig-ops.yaml`，不复用这三个内部符号。
- 不做 ITSM / Paperclip 的具体实现（见 §1.2 不选理由）。

## 2. 现状与扩展点

### 2.1 现状证据

- **一方 8 源**：`core/manager/middleware/toolset/sources.go:97-192` 的 `firstPartySources()` 返回 `pg`、`redis`、`k8s`、`mq`、`kafka`、`rabbitmq`、`git`、`host` 八个 `Source`，每个带一个 `Env`（DSN 环境变量）与一个 `Wire`（构造 + 连接 + `RegisterTools` + 返回 closer）。
- **注册路径**：八个 family 各自作为 `KindAdapter` 贡献体经 `kernel.Host.Activate` 注册，条目名即 family 名（`sources_plugin.go:32-34`：`r.Register(kernel.KindAdapter, string(f.src.Name), f.src)`）。
- **工具面**：`opskeeper-sre-middleware` 声明 56 个工具（`plugins/pig-ops/opskeeper-sre-middleware/pig-ops.yaml`），全部 `class: read`、`safety_level: L1`、`required_scopes: [db.read, k8s.read, mq.read]`。
- **适配器契约**：`core/manager/middleware/adapter/adapter.go` 的 `Adapter` 接口（`Connect` / `Close` / `Health` / `Diagnose` / `Collect` / `Execute`）与 `RiskLevel` 五档（L0 只读 → L4 破坏性）由第三方包直接实现，不重新定义。
- **工具注册**：`core/manager/middleware/registry` 的 `Tool{Name, Description, RiskLevel, Handler, ArgsSchema, ParamsSchema}`；名称必须是 `<family>.<method>`。

### 2.2 adapter 宿主的准入门

`sources_plugin.go:38-49` 的 `adapterPolicy()` 是 adapter 宿主唯一的准入上限：

| 字段 | 值 |
|---|---|
| `Target` | `manager` |
| `MaxSafetyLevel` | `L2` |
| `MaxBlastRadius` | `single-ns` |
| `GrantedScopes` | `db.read` `db.write` `k8s.read` `k8s.exec` `mq.read` `mq.write` `host.read` `host.write` |

注意这份清单里**没有** `topology.read`、`alert.*`、`observability.read`——这三项分别授予了 datasource 宿主（`core/manager/biz/aiops/datasource/datasource.go` 的 `ManagerPolicy`）。同一个 `domain.Scope` 常量在不同宿主上授予情况不同，`Admit` 读的永远是**当前宿主**的那一份。

## 3. 两道宿主侧闸门（本方案的核心发现）

任何第三方 adapter 包在当前代码形态下都要过这两道。**它们都在宿主代码里，4.4 不改；本文档把它们的精确位置与后果写下来，是为了让后续切片的评审有明确的验收对象。**

### 3.1 G1：注册可见性——`Activate` 成功 ≠ 可用

`Sources()`（`sources_plugin.go:90-120`）的读回循环是：

```go
byName := map[Family]Source{}
for _, e := range host.Registry().All(kernel.KindAdapter) {   // 读到了第三方条目
    if s, ok := e.Impl.(Source); ok { byName[s.Name] = s }
}
out := make([]Source, 0, len(byName))
for _, src := range firstPartySources() {                      // 只按一方字面量遍历
    if s, ok := byName[src.Name]; ok { out = append(out, s) }
}
```

`out` **永远只含一方 family**。第三方 family 即使通过 `Activate` 进入注册表（`TestThirdPartyFamilyJoinsTheSameRegistry` 证明的正是这一点），也不会出现在 `Sources()` 里。后果是链式的：

| 消费者 | 位置 | 后果 |
|---|---|---|
| `families()` / `FamilyNames()` | `toolset.go:71-90` | 第三方 family 不在列表中 |
| `ParseFamily(tool)` | `toolset.go:97-111` | `cmdb.ci_get` 解析为 `""` → "不是 middleware 的" |
| `Registry()` / `ReadTools()` / `PackagedReadTools()` | `toolset.go:132/195/247` | 工具不进 manifest 生成面，不进节点包 |
| 启动期连接装配 | `cmd/opskeeper/loop_adapters.go:74` | 第三方 `Wire` 永不被调用 → 永不连接 |
| 工具路由 | `cmd/opskeeper/main.go:6987/7017/7067` | 按 `ParseFamily` 前缀路由 → 第三方工具被路由到错误分支 |

`sources_plugin.go:87-88` 的注释写着"a third-party adapter family a package registers lands in the same list"——**这句注释在当前实现下不成立**，它描述的是意图而不是行为。把它记为 4.4 必须解决的第一个问题，而不是留给下一个任务去发现。

### 3.2 G2：scope 词表——第三方无法引入新 scope

`adapterPolicy()` 是 `toolset` 包的**未导出**函数，`GrantedScopes` 是硬编码的 8 项字面量（`sources_plugin.go:43-48`）。后果：

- 第三方包**不能**在不改宿主代码的前提下申请 `cloud.read`、`cmdb.read` 或任何新 scope。`core/domain/plugin.go:57-59` 的注释允许第三方声明自有 namespaced scope，词表层面没问题；缺的是**授予**。
- `sdk.Admit`（`sdk/manifest.go:392`）对 `RequiredScopes` 的判定是 `m.Spec.RequiredScopes.Satisfies(a.GrantedScopes)`，未授予即拒，且错误信息直接点名缺失项。**这是一个 fail-closed 的真门禁，不是可以绕过的形式检查。**
- 把外部系统的读映射到一个语义不符的既有 scope（例：云 API 读映射到 `host.read`）在技术上能过门禁，但那是 **scope 洗白**：宿主会按 `host.read` 注入本机/SSH 凭据，而包拿去访问云 API。本方案明确否决这条路（§5.3）。

### 3.3 G3：工具面一致性

`opskeeper-sre-middleware/pig-ops.yaml` 是宿主对节点的工具**允许清单**，由 `PackagedReadTools` 生成、受两条测试约束（`TestTheMiddlewareProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel`、`TestToolsetMatchesTheAdapters`）。新 family 的工具必须**要么**进入该清单、**要么**进入 `NotPackagedFamilies` / `NotPackaged` 并附理由——否则这两条测试立刻失败。这是好事：它意味着"多了一批工具"永远是一个 diff，而不是一次静默的能力扩张。

## 4. 候选 1：Karmada 多集群联邦（`opskeeper-adapter-karmada`）

### 4.1 选型理由

- 一方 `k8s` family 绑定**单个**集群的 kubeconfig / in-cluster / endpoint（`adapter/k8s/client.go`），工具集是 `k8s.pod_list` / `k8s.node_list` / `k8s.top_pods` 这类**单集群视角**。
- Karmada 是 CNCF 联邦项目：它把多个成员集群聚成一个控制面，核心对象是 `Cluster`（成员集群）、`Work`（下发的期望态）、`PropagationPolicy`（传播策略）。"某资源此刻实际落在哪些成员集群上"这个问题，在当前 8 源里**没有任何工具能回答**，而在多集群事故里它往往是第一问。
- 它是真正第三方（CNCF 独立项目），与一方 `k8s` 不共享代码、不共享命名空间、不共享 DSN。
- **它只读，且 scope 落在已授予集合内**（`k8s.read`），不需要动 `adapterPolicy()`——这是三个候选里唯一一个从第一天起就是绿灯的。

### 4.2 manifest 草案

详见 `adapter-manifests/karmada.yaml`。要点：

- **包名**：`opskeeper-adapter-karmada`（沿用一方前缀 `adapterPluginPrefix = "opskeeper-adapter-"`，见 `sources_plugin.go:14`，回滚时按包名定位到单个 family）
- **精确版本**：`0.1.0`
- **vendor**：`karmada`
- **family / 贡献名**：`karmada`（**不复用 `k8s`**：同名会撞 `ErrDuplicate`（`core/floor/kernel/registry.go:74`），且工具前缀必须与一方 `k8s.*` 区分，否则无法判断一个 `k8s.*` 工具背后是谁）
- **scope / 安全级别 / 兼容范围**：
  - `targets: [manager]`
  - `safety_level: L1`；`capabilities: [read]`
  - `required_scopes: [k8s.read]` —— **⊆ `adapterPolicy().GrantedScopes`（`sources_plugin.go:45`）**，逐候选验证见 §4.4
  - `approval.required: false`，且**不写** `max_blast_radius`：`sdk.Validate` 明确拒绝"只读级别却携带 blast radius"的 manifest（`sdk/manifest.go:344-348`），写了就是加载失败而不是警告
  - `install.strategy: rolling`、`min_pig_version: 0.4.0`、`min_edge_version: 0.8.0`（与 `opskeeper-sre-middleware` 对齐）
- **能力声明**：仅 5 个 `class: read` 工具（§4.3）

### 4.3 注册路径与工具面

- **注册路径**：`kernel.Host.Activate(ctx, manifest, contrib)` 是唯一入口。第三方包自己实现 `Contribution`（`PluginName()` + `Register(r kernel.Registrar)`），在 `Register` 里执行一次 `r.Register(kernel.KindAdapter, "karmada", source)`，`source` 是与一方同形的 `Source{Name, Env, Wire}`。包**不 import** `toolset` 的任何内部符号；`familyPlugin` / `adapterManifest` / `adapterPolicy` 全部未导出，第三方按公开面重写这十几行是刻意的——宿主换掉合成 manifest 时，第三方包读的是自己的 `pig-ops.yaml`，不会跟着漂移。
- **连接时机**：`Source.Wire(ctx, reg, dsn)`，`dsn != ""` 时连接并返回 closer（启动期路径），`dsn == ""` 时只注册未连接的适配器（清单 / 能力门 / schema 生成路径）。这条契约对第三方与一方**逐字相同**，因为它定义在 `sources.go:44-50`。
- **工具面**（5 个，全部 `read`）：

| 工具 | 级别 | 回答的问题 |
|---|---|---|
| `karmada.cluster_list` | L0 | 有哪些成员集群，各自就绪吗 |
| `karmada.cluster_status` | L0 | 单个成员集群的详细状态与连接时间 |
| `karmada.resource_placement` | L1 | 某资源被传播到哪些成员集群，在哪些上面没落地 |
| `karmada.propagation_status` | L1 | 传播策略及其在成员集群的执行结果 |
| `karmada.work_list` | L0 | 待执行 / 失败的 `Work` 队列 |

- **v1 明确不做**：`karmada.cluster_unbind`、`karmada.propagation_delete`。摘除成员集群、解除传播都是外部系统写操作，见 §4.5。

### 4.4 scope 逐候选验证

```
manifest.required_scopes = [k8s.read]
对照 Policy = toolset.adapterPolicy().GrantedScopes
             = [db.read, db.write, k8s.read, k8s.exec, mq.read, mq.write, host.read, host.write]
k8s.read ∈ GrantedScopes  →  Satisfies() = true  →  Admit 通过
```

同时校验：`safety_level: L1` 的 `MinimumClass()` 为 `read`，manifest 声明的 `capabilities` 最坏类为 `read`，不越过上限；`L1 < 宿主 MaxSafetyLevel L2`；`max_blast_radius` 为空，`RadiusNone.AtMost(RadiusSingleNS)` 成立。

**结论：本候选在 G2 下为绿，不需要任何宿主 policy 变更。**

### 4.5 与 L2 上限的关系

v1 只读，因此不触及 L2。未来若增加"解除传播"这类写操作，**必须**同时满足（缺一即被拦）：

1. `safety_level` 升到 `L2`，`capabilities` 增 `write`——这是**一次显式的 manifest 变更 + 评审**，不得以"工具实际会写、manifest 仍写 L1"的方式通过。`sdk.validateTools`（`sdk/manifest.go:302`）按 `spec.capabilities` 上限校验每个工具的 class，`sdk.Admit` 按 `MaxSafetyLevel` 校验包级别，两道都会拦住。
2. `required_scopes` 增 `k8s.exec`（**已授予**，无需扩 policy）。
3. `approval.required: true` 且 `max_blast_radius: pod`（单成员集群）。宿主上限 `RadiusSingleNS` 比 `pod` 宽，`Admit` 取更窄者成立。
4. 一次 manifest 版本提升（0.1.0 → 0.2.0），`rolling` 策略下由 canary 波次兜底。

**不需要改宿主上限**：`adapterPolicy()` 的 `L2` / `single-ns` 已经容纳 `L2` + `pod`。这正是保留这份上限的价值——它按 blast radius 而不是按动作外观设定。

### 4.6 与 middleware 包及一方 8 源的协同边界

- **与 `opskeeper-sre-middleware`（56 工具）**：只描述接口，不复刻注册方式。新增的 5 个工具要么进入该包的 `spec.tools`，要么进入 `NotPackagedFamilies`（附理由）；本方案**不修改**该文件。
- **与一方 `k8s.*`**：两套工具共存，命名空间不同（`k8s.*` / `karmada.*`），凭据不同（kubeconfig vs 控制面 kubeconfig），**同时激活而非配置期二选一**——联邦视角与单集群视角回答的是不同问题，不存在"谁覆盖谁"。同激活的安全边界由 §7.1 的 S0 负责。
- **路由**：`ParseFamily` 以第一个 `.` 前的前缀匹配，`karmada` 与 `k8s` 无前缀包含关系，不会互相误判。

### 4.7 测试方案

| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | 名字空间前缀解析：`karmada.*` 不得被 `ParseFamily` 判成 `k8s` | 两者互不为前缀 |
| Unit | `resource_placement` 的"未落地"判定：成员集群 `Applied=false` | 返回 `placed=false` + 原因，**不返回空列表** |
| Unit | Karmada API 错误码 → `adapter.ErrTimeout` / `ErrNotConnected` 映射 | 不吞错、不静默成功 |
| Unit | 作用域截断：`Work` 列表强制 `limit` 上限 | 超过上限被裁剪并在结果中声明 |
| Integration | envtest 起 Karmada 控制面 + 2 个 kind member cluster | 5 个工具全部返回真实数据 |
| Integration | 制造一次传播失败（member 不可达） | `work_list` 报出失败项；`cluster_status` 报出 member 离线 |
| Contract | 与一方 `k8s.*` 在同一 fixture 上的边界 | 两套工具答案互补，无重复语义（评审清单） |
| Failure | 控制面 401 / 403 / 5xx / 超时（>30s 对齐 `toolset.ConnectTimeout`） | 错误码正确、无部分成功 |
| Contract | manifest 通过 `sdk.Validate` + `kernel.Host.Admit(adapterPolicy)` | 见 §4.4 的逐项断言 |
| Contract | `TestTheMiddlewareProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel` 与 `TestToolsetMatchesTheAdapters` | S0 落地后全绿 |

### 4.8 回滚方案

- **回滚粒度**：包级（`opskeeper-adapter-karmada`）+ 配置删除。
- **精确因果链**：
  1. 删除 `OPSKEEPER_LOOP_KARMADA_DSN`（配置层先断开来源）；
  2. `Host.Deactivate(ctx, "opskeeper-adapter-karmada")`——`registry.RemovePlugin` 按包名摘除该包写入的全部条目，第三方贡献的 `KindAdapter/karmada` 条目随之消失；
  3. 重启 Manager；
  4. 若已随 S0 合并进 `opskeeper-sre-middleware/pig-ops.yaml`，回滚须**同时**把 `karmada.*` 五条从清单移除或移入 `NotPackagedFamilies`——否则 `TestTheMiddlewareProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel` 会失败（这是刻意的：清单与实装不一致必须失败，而不是静默）。
- **不变量**：`k8s.*`、`pg.*` 等一方 8 源的 56 个工具不受影响；`Sources()` 中一方顺序不变；`kind.go` 六元集合不变。
- **回滚文档三段**：卸载前（确认 DSN 已移除、确认清单 diff 已就位）／卸载后（跑 `make pi-plugin-guard`、`make module-check`、`go test ./scripts/ledgercheck`，在 `runtime view` 确认 `k8s.*` 工具数与清单一致）／不变量（同上）。

## 5. 候选 2：阿里云 ECS 只读清单（`opskeeper-adapter-alicloud`）

### 5.1 选型理由

- 4.1 §③"设备纳管"一栏明确写着 `adapter` 候选是"**云资源** / CMDB 同步"，§⑨"ITSM / CMDB / 云资源 / Paperclip"同样指向 4.4。
- 内置凭据类型已存在：`core/domains/biz/secret/credtype.go:98` 的 `alicloud`（access key / secret / region），以及同文件 `:85` 的 `aws`、`:72` 的 `tencentcloud`。凭据注入链不需要新建。
- 价值具体：事故诊断的第一跳常常是"这个 pod 落在哪台 ECS 上、这台机器最近有没有变更"。一方 8 源里 `host.*` 回答的是**平台被指向的那台机器**，不回答云上全量资产。

### 5.2 manifest 草案

详见 `adapter-manifests/alicloud.yaml`。要点：

- **包名**：`opskeeper-adapter-alicloud`，**精确版本** `0.1.0`，**vendor** `alicloud`
- **family / 贡献名**：`alicloud`
- **scope / 安全级别 / 兼容范围**：
  - `targets: [manager]`
  - `safety_level: L1`；`capabilities: [read]`（v1 全只读，理由见 §5.5）
  - `required_scopes: [cloud.read]` —— **不在 `core/domain/plugin.go:60-73` 的常量集内，也不在 `adapterPolicy().GrantedScopes` 内**，逐候选验证见 §5.4
  - `approval.required: false`，不写 `max_blast_radius`
  - `install.strategy: rolling`、`min_pig_version: 0.4.0`、`min_edge_version: 0.8.0`

### 5.3 为什么是 `cloud.read`，而不是 `host.read`

这是本候选唯一需要防守的设计判断。

- **技术上**：`host.read` 在 `adapterPolicy().GrantedScopes` 内，写它就能过 `Admit`，改动量最小。
- **但它是错的**：`host.read` 的语义是"读取平台被指向的机器"，凭据是本机 / SSH 形态；把它授予一个调用云 OpenAPI 的包，等于让宿主按错误的语义注入凭据，而评审读到的 manifest 会显示一个**与实际用途不符**的 scope。审计账本上这条记录将无法解释。
- 因此本方案坚持新增 `cloud.read`，并把"是否授予"作为**显式决策 D2**（§5.4），而不是让实现者用一个已授予的 scope 蒙混过关。词表层面允许第三方 namespaced scope（`core/domain/plugin.go:57-59`），缺的只是授予。

### 5.4 scope 逐候选验证

```
manifest.required_scopes = [cloud.read]
对照 Policy = toolset.adapterPolicy().GrantedScopes
             = [db.read, db.write, k8s.read, k8s.exec, mq.read, mq.write, host.read, host.write]
cloud.read ∉ GrantedScopes  →  Satisfies() = false
                              →  Admit 拒绝：pig-ops.yaml: spec.required_scopes: not granted: cloud.read
```

**结论：本候选在当前 policy 下为琥珀。** 需要决策 **D2**：`adapterPolicy().GrantedScopes` 是否增加 `cloud.read`（以及 v0.2.0 需要的 `cloud.write`）。

- **D2 = 同意**：前置切片 S0 同时把 scope 常量与 `adapterPolicy()` 扩展为可配置集合（§7.1 步骤 3），本候选转绿。
- **D2 = 拒绝**：本候选**不落地**，云资产清单以宿主内模块形式提供（`core/manager/biz/device` 已有 `EdgeDeviceRepo` 契约，是更自然的落点），不占 adapter 扩展点。**这是一个完整的"不做"选项，且成本可接受**——云清单本身不需要 `Diagnose`/`Collect`/`Execute` 三段契约。

### 5.5 与 L2 上限的关系

v1 全只读（实例 / 磁盘 / 快照清单），因此 `safety_level: L1`。**云资源天然带写操作**（重启实例、挂载盘、释放快照），如果不写清楚，读者会以为"L1 是因为这个包没有写能力"。事实是：**有，但不在 v1**。

v0.2.0 计划新增 `alicloud.reboot_instance`（重启单台 ECS 实例）：

| 维度 | v0.1.0（本文草案） | v0.2.0（后续切片） |
|---|---|---|
| `safety_level` | `L1` | `L2` |
| `capabilities` | `[read]` | `[read, write]` |
| 工具 class | 全部 `read` | 4 个 `read` + 1 个 `write` |
| `required_scopes` | `[cloud.read]` | `[cloud.read, cloud.write]` |
| `approval.required` | `false` | `true` |
| `approval.max_blast_radius` | 不写（只读级别写了会被 `sdk.Validate` 拒绝） | `pod`（单台实例） |

**与 L2 上限的关系**：`adapterPolicy()` 的 `MaxSafetyLevel: L2` 恰好容纳 `L2`；`MaxBlastRadius: single-ns` 比 `pod` 宽，`Admit` 取更窄者成立。**上限不需要改**；需要改的只有 `GrantedScopes`（D2）。一条 ECS 重启的影响面是单台机器，`pod` 是本仓语义里最贴切的半径（`core/domain/safety.go`：`pod` / `single-ns` / `namespace` / `cluster`）。

**不得静默降级**：`sdk.validateTools` 按 `spec.capabilities` 上限校验工具 class，`sdk.Admit` 按 `MaxSafetyLevel` 校验包级别。一个实际会写、却声明 `L1 + [read]` 的包在这两处都会失败——这是本仓已有的保护，方案不依赖人工自觉。

### 5.6 注册路径与工具面

- **注册路径**：同 §4.3（`Activate` 唯一入口；第三方自实现 `Contribution`；不自造注册表）。
- **工具面**（4 个，全部 `read`）：`alicloud.instance_list`、`alicloud.instance_describe`、`alicloud.disk_list`、`alicloud.snapshot_list`。
- **不提供 `alicloud.connect`**：连接在启动期由 `Source.Wire(ctx, reg, dsn)` 完成。一方保留 `k8s.connect` 是因为它支持运行期切换目标集群，本候选没有该需求，复制它只会多一条到凭据的门。
- **凭据**：`core/domains/biz/secret` 的 `alicloud` 类型注入，作用域限于 `cloud.read`。

### 5.7 测试方案

| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | 分页：`limit` 与 `NextToken` 透传，跨页不重不漏 | 结果集与预期分页边界一致 |
| Unit | 空结果与全被过滤：返回"无匹配"而非错误 | 不把空当失败 |
| Unit | 鉴权失败 403 与配额 429 区分 | 错误码语义可区分，不吞错 |
| Unit | 凭据注入：包只收到 `cloud.read` 声明的凭据 | 未声明 scope 的凭据不可达 |
| Integration | 本地 mock OpenAPI（实例 / 磁盘 / 快照查询） | 4 个工具字段映射正确 |
| Integration | 多 region 场景（`region` 未配时只查默认 region） | 行为可预期，不静默扫全 region |
| Failure | 限流 / 超时（对齐 `toolset.ConnectTimeout` 30s） | 退避重试有上限，不无限重试 |
| Contract | `sdk.Validate` + `Admit`（当前 policy 断言**拒绝**并点名 `cloud.read`） | 红灯是正确结果，测试断言的是拒绝信息 |
| Contract | （D2 = 同意后）`Admit` 断言转绿 | 回归保护 |

### 5.8 回滚方案

- **回滚粒度**：包级 + 凭据解绑。
- **精确因果链**：
  1. 从 secret 库解绑该包使用的 `alicloud` 凭据（先断凭据，再停包）；
  2. 删除 `OPSKEEPER_LOOP_ALICLOUD_*` 配置；
  3. `Host.Deactivate(ctx, "opskeeper-adapter-alicloud")`；
  4. 重启 Manager；
  5. 若工具已进入 middleware 包清单，同步处理 `spec.tools` / `NotPackagedFamilies`（同 §4.8 第 4 步）。
- **不变量**：云 API 侧的**任何状态都不被本包修改**（v1 全只读），因此回滚无外部副作用需要清理；一方 8 源不受影响。

## 6. 候选 3：CMDB 只读配置项（`opskeeper-adapter-cmdb`）

### 6.1 选型理由

- 4.1 §③"拓扑"一栏：`CMDB 走 adapter`；§⑨"ITSM / CMDB / 云资源 / Paperclip"同样指向 4.4。
- 现状确认：全仓 `grep -rl -e CMDB -e cmdb core/` 无命中，CMDB **完全未实现**；`core/manager/biz/topology` 只有 `NodeRepo` / `RelationRepo` 两个纯持久化契约，没有外部来源。
- 事故场景的具体收益："最近一次配置变更是什么"是 RCA 的高频第一问，而它只能从外部 CMDB 拿。

### 6.2 manifest 草案

详见 `adapter-manifests/cmdb.yaml`。要点：

- **包名**：`opskeeper-adapter-cmdb`，**精确版本** `0.1.0`，**vendor** `cmdb`
- **family / 贡献名**：`cmdb`
- **scope / 安全级别 / 兼容范围**：
  - `targets: [manager]`
  - `safety_level: L1`；`capabilities: [read]`
  - `required_scopes: [topology.read]` —— **在 `core/domain/plugin.go:70` 的常量集内，但不在 `adapterPolicy().GrantedScopes` 内**，逐候选验证见 §6.4
  - `approval.required: false`，不写 `max_blast_radius`
  - `install.strategy: rolling`、`min_pig_version: 0.4.0`、`min_edge_version: 0.8.0`

### 6.3 为什么是 `topology.read`

这是三个候选里唯一一个**词表已存在、只差授予**的 scope，因此歧义最小：

- `topology.read` 的语义就是"读取资产 / 拓扑信息"，CMDB 的 CI 清单正是拓扑的外部来源；
- 与 §5.3 的 `host.read` 洗白不同，这里没有"把 A 的凭据发给 B"的风险——CMDB 读凭据就是 CMDB 读凭据；
- 需要注意的是**同名不同源**：授予 `topology.read` 意味着宿主注入的拓扑凭据会流向外部 CMDB。若未来 CMDB 与 OpsKeeper 内置拓扑是两套凭据，应拆成两个 scope（如 `topology.read` / `cmdb.read`）而不是共用一个——**本方案按 `topology.read` 起草，并在 §6.4 把它作为决策 D1 的附带问题提交评审。**

### 6.4 scope 逐候选验证

```
manifest.required_scopes = [topology.read]
词表检查：topology.read ∈ core/domain/plugin.go:60-73 的常量集        → 是
对照 Policy = toolset.adapterPolicy().GrantedScopes
             = [db.read, db.write, k8s.read, k8s.exec, mq.read, mq.write, host.read, host.write]
topology.read ∉ GrantedScopes  →  Satisfies() = false
                                 →  Admit 拒绝：pig-ops.yaml: spec.required_scopes: not granted: topology.read
```

**结论：琥珀**，需要决策 **D1**：`adapterPolicy().GrantedScopes` 是否增加 `topology.read`（附带问题：是否拆分为独立的 `cmdb.read`）。

- **D1 = 同意**：本候选转绿。
- **D1 = 拒绝**：CMDB 同步以宿主内模块形式落在 `core/manager/biz/topology` / `core/manager/biz/device`，通过既有 repo 契约写入，不占 adapter 扩展点。代价是它只能用编译内工具暴露，失去 `spec.tools` 允许清单这道人工评审面——**这是拒绝方案的真实代价，写在这里而不是留给实现者去发现。**

### 6.5 与 L2 上限的关系

v1 全只读，`safety_level: L1`。**写回 CMDB（登记一次修复结果）在设计上是明确不做的**：一次自动修复回写 CMDB 会污染 ITSM/CMDB 自身的审计链，且没有可回滚的对称操作。若未来确需写回，必须是 `L2` + `topology.write`（**该 scope 在常量集内不存在**，需同时扩词表与 policy）+ `max_blast_radius: pod` + `approval.required: true`，并单独评审。

宿主上限 `L2` / `single-ns` 同样容纳 `L2` + `pod`；**上限不需要改**，需要改的是词表与 `GrantedScopes`。**当前没有任何候选需要上调 `MaxSafetyLevel` 或 `MaxBlastRadius`——这是一个刻意的结论：扩展点不是靠放宽门禁长出来的。**

### 6.6 注册路径与工具面

- **注册路径**：同 §4.3。
- **工具面**（4 个，全部 `read`）：`cmdb.ci_search`、`cmdb.ci_get`、`cmdb.ci_relations`、`cmdb.ci_change_log`。
- **不提供写回**：见 §6.5。
- **同步落点**：本包**只读不落库**。把 CI 变化写进 OpsKeeper 拓扑是宿主侧的同步任务（消费本包工具的输出），不是本包的动作——把同步做成包内定时任务会让一个"adapter"同时承担读与写两套语义，且写的那部分完全不可回滚。

### 6.7 测试方案

| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | `ci_search` 的分页与字段映射（CMDB 各家 schema 不同，映射表快照比对） | 快照一致 |
| Unit | `ci_change_log` 的时间边界与时区 | 边界 1ms 一致 |
| Unit | `ci_relations` 的关系类型未知值 | 原样返回，不丢弃 |
| Integration | mock CMDB 服务 + 固定 CI fixture | 4 个工具结果集相等 |
| Failure | 401 / 404 / 5xx / 超时 | 错误码正确、不吞错 |
| Failure | CMDB schema 缺字段（某家没有 `ci_change_log`） | 返回"该实例不支持"而不是编造空结果 |
| Contract | `sdk.Validate` + `Admit`（当前 policy 断言**拒绝**并点名 `topology.read`） | 红灯是正确结果 |
| Contract | （D1 = 同意后）`Admit` 断言转绿 | 回归保护 |

### 6.8 回滚方案

- **回滚粒度**：包级 + 配置删除。
- **精确因果链**：删除 CMDB 配置与凭据 → `Host.Deactivate(ctx, "opskeeper-adapter-cmdb")` → 重启 Manager → 若工具已进清单则同步处理 `spec.tools` / `NotPackagedFamilies`。
- **额外注意**：若 S1–S3 中的任何一条已把 CMDB 数据同步进 OpsKeeper 拓扑，回滚**只回滚包，不回滚已落库的数据**——拓扑图里的 CMDB 节点需要单独清理任务。这条必须在实现切片里配一个幂等清理任务，否则"回滚"会留下无法解释的孤儿节点。

## 7. 实现切片建议（仅建议，不在本任务合入）

### 7.1 S0：前置宿主切片（与候选解耦，必须先做）

**S0 解决 §3.1 / §3.2 / §3.3 三道闸门，且不解决任何候选的业务逻辑。** 它是 4.4 从"方案"走向"可落地"的唯一必经之路，且无论 D1 / D2 的结论如何都需要做一部分。

| 步骤 | 内容 | 验收 |
|---|---|---|
| 1 | `Sources()` 的读回循环改为遍历注册表快照并保留一方顺序，新增 `TestAThirdPartyFamilyAppearsInSources`（断言 `Sources()` 含第三方 family，且顺序稳定） | 现有一方 8 源顺序不变，测试通过 |
| 2 | `cmd/opskeeper/loop_adapters.go` 的启动装配按 `Sources()` 走（已是），确认第三方 `Wire` 被调用 | 集成测试：第三方 family 的 `Wire` 被调用一次 |
| 3 | `adapterPolicy()` 提取为**可配置**的 GrantedScopes（默认仍是当前 8 项），使 D1 / D2 的结论可以只改配置不改结构 | 默认值下现有行为逐字不变 |
| 4 | `familyPlugin` / `adapterManifest` 保持不变；新增一个**公开的最小参考实现**示例（或文档片段），说明第三方如何自己写 `Contribution` | 示例可编译、可被 `Admit` |
| 5 | 新 family 的工具进入 `opskeeper-sre-middleware/pig-ops.yaml` 或 `NotPackagedFamilies`，二选一并附理由 | 两条既有测试保持绿 |

**S0 的风险**：步骤 1 改的是 `sources.go` 主流程之外但与之一体的读回逻辑，`TestSourcesIsDerivedFromTheKernel` 会检查 `Sources()` 与 `firstPartySources()` 的**条数与顺序相等**——该测试需要同步调整语义（从"两者必须相等"改为"一方顺序必须是 `Sources()` 的前缀子序列"）。这是 S0 中**唯一有行为风险的一步**，必须在 PR 里单列评审。

### 7.2 建议的包结构（与 4.3 同构）

```
plugins/opskeeper-adapter-karmada/        # alicloud / cmdb 同形
├── pig-ops.yaml                          # 已在 adapter-manifests/ 给出草案
├── go.mod                                # 独立 module，便于版本化与签名
└── internal/
    ├── contributor/contributor.go        # Contribution 实现：PluginName + Register
    ├── karmadaclient/client.go           # 实现 adapter.Adapter（Connect/Health/Diagnose/Collect/Execute）
    ├── register_tools.go                 # RegisterTools(reg, adapter)：声明 L0/L1 工具
    └── translate/                        # 领域映射 + 单测
```

- 独立 Go module，由主仓 `go.work` 引用；不污染主仓 `go.mod` 顶层。
- `RegisterTools` 的签名与一方逐字相同（`*middlewareregistry.Registry` + 具体适配器），这是可以复用的**契约**，不是复用一方的注册方式。
- 集成测试 fixture 放 `core/harness/fixtures/adapter/<family>/`，与既有 harness 同位。

### 7.3 不建议改动

- 不要在 `sources.go` 的 `firstPartySources()` 里加第三方条目——那是把第三方变成编译内依赖，恰好消灭了扩展点的意义。
- 不要把第三方客户端放进 `core/manager/middleware/adapter/<name>`——该目录受 `TestOnlyTheCatalogNamesTheAdapterPackages` 约束（`catalog_test.go:26`），第三方包放进去会让该测试失败。
- 不要给第三方包新增注册入口。`Activate` 是唯一入口，平行注册表会同时消灭 G1 的可诊断性与 `ErrDuplicate` 的 fail-closed 语义。
- 不要改 `core/floor/kernel/kind.go` 的六元集合。

### 7.4 落地门槛

- 用户须单独开 change；本任务不直接合入。
- 落地前需补：真实 Karmada 控制面 / 云 OpenAPI / CMDB 的 staging fixture；CI 接入 `make pi-plugin-guard`、`make module-check`、`go test ./scripts/ledgercheck`；S0 的 PR 单独评审。

## 8. 通用闸门（每个候选都必须通过的清单）

按 `tasks.md` 4.10 与 4.11：

| 闸门 | 内容 |
|---|---|
| **Manifest 完整** | apiVersion、kind、metadata.name/version/vendor/homepage、spec（targets、safety_level、capabilities、tools、required_scopes、audit、approval、install） |
| **精确版本** | `metadata.version` 用 semver 锁；不得用 `latest` / `main` |
| **Scope 最小化** | 仅声明该候选必要的 scope；多写一个就拒（`Satisfies` 只判"包含"，最小化靠评审与 §4.4 / §5.4 / §6.4 的逐项对照） |
| **Scope ⊆ 宿主 Policy** | 逐候选写明对照的 Policy 与结论（绿 / 琥珀），琥珀项必须有显式决策编号 |
| **安全级别** | 涉及写操作的候选必须显式声明 `safety_level` 并说明与 `adapterPolicy()` 的 `L2` / `single-ns` 的关系；**不得静默降级** |
| **命名空间** | family 前缀不得与一方 8 源或其前缀互相包含（`k8s` vs `karmada` 是正例，`k8s2` 是反例） |
| **兼容范围** | `install.min_pig_version` 与 `install.min_edge_version` 必填；与主仓 PiG / Edge 基线（0.4.0 / 0.8.0）对齐 |
| **测试** | unit + integration + contract + failure 四层；integration 必须在 CI 中跑真后端（compose / envtest / mock 服务） |
| **签名** | 经 `core/floor/pluginmanifest/signing.go` 通道，签名文件随 manifest 一同分发 |
| **回滚文档** | 含"卸载前 / 卸载后 / 不变量"三段；每候选的因果链逐步可执行 |
| **闸门命令** | `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` 全过 |

## 9. 与 4.1 / 4.2 一致性自查表

| 自查项 | 4.1 / 4.2 结论 | 本方案是否一致 | 证据 |
|---|---|---|---|
| 嵌入模型编译内 | 嵌入模型本期留在编译内 | ✓ 不引入新嵌入模型 | §1.3 非目标 |
| MCP / 技能宿主内管理 | MCP 与技能宿主内管理 | ✓ 不开新 MCP / 不改技能注册 | §1.3 非目标 |
| 告警归 4.12 | 告警归 4.12 | ✓ 不选 ITSM，理由即"写语义落在 `alert.*`" | §1.2 不选理由 |
| 六扩展点封闭 | `core/floor/kernel/kind.go` 六 Kind 封闭 | ✓ 不新增 Kind / 信号 / family 常量 | §1.3 / §7.3 |
| adapter 归"迁移为插件" | 4.2 四分类：adapter 迁移为插件，第三方候选 0 | ✓ 仅在"第三方候选"分支推进，一方 8 源分支不重写 | §1.3 / §2.1 |
| 一方 8 源不重写 | 经内核注册，证据 `sources.go`、`sources_plugin.go` | ✓ 不改 `firstPartySources()` 与 `familyPlugin` / `adapterManifest` | §7.3 禁止项 |
| 设备纳管 / 云资源 / CMDB 归 4.4 | 4.1 §③、§⑨ 指向 4.4 | ✓ 云资源与 CMDB 入选；Karmada 补的是 4.4 未点名但同族的 K8s 跨集群缺口 | §1.2 / §4.1 / §5.1 / §6.1 |
| ITSM / Paperclip 归 5.2–5.9 | 4.1 §⑦、§⑨ 指向 5.x | ✓ 明确不选，不与 5.x 重叠 | §1.2 不选理由 |
| 协同面只描述接口 | 与 `opskeeper-sre-middleware`（56 工具）协同只描述接口 | ✓ 不改该包、不复刻一方注册方式 | §4.6 / §7.3 |
| 配置期单激活纪律 | 4.3 评审修订：同信号双注册 `ErrDuplicate`，配置期单激活 | ✓ 沿用并**明确例外**：Karmada 与一方 `k8s` 语义互补，同时激活而非二选一，理由写明 | §4.6 |
| Scope 硬约束 | `required_scopes` ⊆ 宿主 `GrantedScopes` | ✓ 逐候选对照 `adapterPolicy()` 并给出绿 / 琥珀结论 | §4.4 / §5.4 / §6.4 |
| 写操作候选的安全级别 | 涉及写操作须显式声明并说明与 L2 上限的关系 | ✓ 三个候选均给出 v1 / v2 对照表；**无一需要上调宿主上限** | §4.5 / §5.5 / §6.5 |

## 10. 备选方案与放弃原因

| 备选 | 内容 | 是否采用 | 原因 |
|---|---|---|---|
| **方案 A：一批做 5 个** | K8s / 云 / ITSM / CMDB / Git 全上 | 否 | ITSM 与 5.x 重叠、Git 与一方重复且已被 `NotPackaged` 排除；原文即"宁缺毋滥"，且 5 个候选里有两个无法过 scope 门禁 |
| **方案 B：只做 1 个最绿的** | 只做 Karmada | 否 | 两个琥珀候选暴露的是**宿主 policy 的真实缺口**，而不是候选本身的缺陷；把它们写下来，D1 / D2 才有评审对象。只交一个绿灯会让"第三方 adapter 需要改宿主"这件事继续隐形 |
| **方案 C：把云 / CMDB 的 scope 塞进既有 scope** | 云读 → `host.read`；CMDB 读 → `topology.read` | 部分 | CMDB 采纳（语义相符，仅差授予）；**云拒绝**：scope 洗白，详见 §5.3 |
| **方案 D：先不动宿主，把第三方包编译进主仓 `firstPartySources()`** | 绕过 G1 / G2 | 否 | 这正是扩展点要消灭的状态：把第三方变成编译内依赖，`Activate` 的门禁形同虚设，且回滚无法按包名进行 |
| **不做（整体）** | 4.4 只写一句"暂不做" | 否 | G1 是一处**注释与实现不符**（§3.1），不写下来它会继续以"第三方已支持"的面貌误导后续任务 |

## 11. 验收清单

- [x] 每个候选有 manifest 草案（`adapter-manifests/{karmada,alicloud,cmdb}.yaml`）
- [x] 每个候选有注册路径（`Activate` 唯一入口，逐候选说明第三方如何实现 `Contribution`）
- [x] 每个候选有测试方案（unit + integration + contract + failure 四层）
- [x] 每个候选有回滚方案（卸载前 / 卸载后 / 不变量三段 + 精确因果链）
- [x] scope ⊆ 宿主 GrantedScopes 逐候选验证，对照的 Policy 写明为 `toolset.adapterPolicy()`（`sources_plugin.go:38-49`），绿 / 琥珀逐候选标注
- [x] 涉及写操作的候选显式声明安全级别并说明与 L2 上限的关系，无静默降级（§4.5 / §5.5 / §6.5）
- [x] 与 4.1 §7 评审结论一致（§9 自查表）
- [x] 与 4.2 四分类一致（§9 自查表）
- [x] 无越界改动（§1.4 / §7.3 明确禁止项；不动 `kind.go`、`sources.go` 主流程、`plugins/` 包结构）
- [x] 受审计跟踪文件中不含内部任务编号（规则见 `docs/OPEN_SOURCE_GATE.md`；内部任务 ID 只记录在 PR 描述与评论）
- [x] PR 描述关联 OpenSpec change `plugin-architecture-recovery-plan` 任务 4.4（提交与评审规范见计划任务 7.4）

## 12. 提交与执行规范

- 提交语言：本任务 PR 使用中文注释 / 中文提交说明（按计划任务 7.4）。
- 关联：OpenSpec change `plugin-architecture-recovery-plan` 任务 4.4；执行任务编号记录在 PR 描述与评审评论中，不写入受审计文件。
- 落盘路径：本文档落盘 `openspec/changes/plugin-architecture-recovery-plan/adapter-plugin-plan.md`；manifest 草案落盘 `openspec/changes/plugin-architecture-recovery-plan/adapter-manifests/`。
- 实现切片建议（§7）**不**在本任务内合入；如要落地，请用户单独开 change，并先裁决 **D1**（`topology.read`）与 **D2**（`cloud.read` / `cloud.write`）。

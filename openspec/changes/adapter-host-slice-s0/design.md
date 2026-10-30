## Context

`adapter` 宿主的当前实现（`core/manager/middleware/toolset/sources_plugin.go`）：

1. `Sources()` 自建一个 `kernel.NewHost(kernel.NewRegistry(), adapterPolicy(), …)`，对 8 个首方 family 逐个 `Activate`。
2. 回读时从 registry 快照建 `byName`，然后 **`for _, src := range firstPartySources()`** 遍历输出。

第 2 步是 G1 的根因：即使第三方 family 被 `Activate` 进同一个 registry 并出现在 `All(KindAdapter)` 快照里，回读循环也只走首方字面量，贡献因此不可见。`adapter-plugin-plan.md` §3.1 记录了这段代码，§3.2 记录了由此派生的第二个缺口 G2（第三方无法引入新 scope 而不改宿主），§3.3 记录了工具清单一致性的 G3。

S0 是 §7.1 五步里的第 1 步，也是 §7.1 明确标注的**唯一有行为风险的步骤**。

## Goals / Non-Goals

**Goals:**

- 修掉"激活成功但读不回来"的 G1，使贡献 family 真正进入闭环接线可见的列表。
- 把 `GrantedScopes` 变成宿主侧可配置值，让 D1/D2 裁决落在代码里而不是文档里。
- 让默认路径（无第三方贡献）的行为逐项不变——8 个 family、顺序、`Env`、`Wire`。
- 让注释与实现重新一致：注释里不保留任何实现不成立的断言。
- 为后续候选包任务留下可验证的形状（参考实现 + 顺序契约 + fail-closed 红灯测试），但不在本切片实现候选包。

**Non-Goals:**

- 不实现 Karmada / 阿里云 / CMDB 任何一个候选包。
- 不导出第三方贡献入口。
- 不改扩展点集合（`core/floor/kernel/kind.go`）。
- 不改 `firstPartySources()`、`NotPackagedFamilies`、`NotPackaged`、`pig-ops.yaml`。
- 不放宽 `sdk/manifest.go` 的校验规则，也不绕过 `adapter/` 目录约束。

## Decisions

### D-S0-1：读回提取为 `sourcesFrom(thirdParty []Source) []Source`

**决策。** 形状不可调，命名可调：提取内部函数 `sourcesFrom(thirdParty []Source) []Source`，`Sources()` 等于 `sourcesFrom(nil)`。

**理由。** 只把回读循环改成遍历 registry 快照是空操作——宿主里本来就只有首方 8 个，遍历快照得到的是同一批条目，第三方仍然进不来。必须让**宿主**知道有第三方条目存在，缺口才能补上。参数化宿主构造这一步同时给测试一个注入点。

**顺带解释的一处偏离。** §7.1 第 1 步的字面写法里包含"导出第三方贡献入口"。本切片**不导出**：没有消费者的导出等于提前冻结 API，§7.3 又禁止并行注册通道。该导出随第一个真实候选包一同落地，届时形状由真实使用者决定。

### D-S0-2：顺序契约

首方 8 个保持 `firstPartySources()` 顺序，构成**严格前缀**；第三方 family 全部排在首方块之后，按 family 名字典序。排序在实现里显式写出，不依赖 registry 内部顺序，也不依赖 Go 包 init 顺序，因此同样的输入两次调用返回同一个列表。

默认路径下第二段为空，`Sources()` 的返回与基线逐项一致（8 项、顺序、`Env`、`Wire`）。

### D-S0-3：第三方走同一条 `Activate`

第三方条目与首方 family 使用**同一个** `host.Activate` + 同一个 `adapterManifest`，因此 `ErrDuplicate` 语义、准入、注册回滚语义完全一致，不存在第二条注册通道，也不存在第二个 registry。名字与既有 family 冲突时贡献被拒绝，而不是覆盖先到的那一个。

### D-S0-4：`AdapterPolicy` 导出 + 默认 10 项授予集合

提取 `AdapterPolicy(granted domain.Scopes) kernel.Policy`（先例：`aiopsdatasource.ManagerPolicy()`），`adapterPolicy()` 保持未导出并等于 `AdapterPolicy(默认授予集合)`。

默认授予集合 = 既有 8 项（原样、原序）+ `topology.read` + `cloud.read`。`MaxSafetyLevel: L2` 与 `MaxBlastRadius: single-ns` **不是参数**，不随授予集合变化。

### D-S0-5：`GrantedScopes` 的语义边界

`GrantedScopes` 是**宿主侧可组装的值，不是包侧可申领的值**。本函数读它、把它交给闸门；任何插件都无法通过"多声明几个 scope"来扩大它，`Satisfies` 对未知 scope 仍然是 fail-closed。扩大授予集合因此永远是一次**宿主代码（组装层）diff**，是评审能看见的 diff，而不是插件的自我声明。这一边界必须写在注释里，否则它会被读成"包可以要什么给什么"。

### D-S0-6：`ScopeCloudRead` 单列，且不带 write

`cloud.read` 与 `host.read` 是两个 scope。`host.read` 由宿主注入本地/SSH 凭据；云厂商开放 API 走各家 AK/SK。把云 API 读取折进 `host.read` 会让一次声明拿到两类凭据，是范围洗白，因此单列。

**不引入 `cloud.write`**：D2 的裁决只覆盖只读，写操作在写操作切片单独评审（`adapter-plugin-plan.md` §5.4）。

### D-S0-7：测试独立可失败

新增测试彼此独立、各自可失败，不是同一条断言的复述：

- 首方 8 个各出现且仅出现一次 / 相对顺序 / `Env` 与 `Wire` 非空——三段分开断言，因为"被静默丢弃"与"顺序错乱"是不同的失败。
- 贡献 family 出现在结果中、位于首方块之后、两次调用顺序一致、两个贡献 family 时为字典序（输入顺序刻意与期望相反）。
- 默认授予集合恰好 10 项且含两个新 scope，前 8 项与基线逐项一致。
- 只读 `cloud.read` / `topology.read` 的 manifest 在默认策略下通过准入。
- **旧 8 项授予集合拒绝 `cloud.read` 并指名缺失 scope**——红灯是正确结果，这条测试是"授予集合可配置"不被误读成"闸门可放松"的护栏。

### D-S0-8：启动接线提取 `wireSources`

从 `wireLoopRemediationAdapters` 提取 `wireSources(ctx, log, reg, srcs []toolset.Source)`，原函数行为等价地转调 `toolset.Sources()`。提取后可以对贡献 family 断言"恰好接线一次"而无需真实后端： family's `Wire` 就是它自己的构造步骤，一个计数的假实现是完整替身。

### D-S0-9：参考实现的位置

参考实现放在 `core/manager/middleware/toolset/example_third_party_adapter_test.go`（package `toolset`），而不是 `sdk/`。

**理由。** 参考实现必须能被"由 `adapterPolicy()` 推导出的 `Admission` 放行"这一条断言住，而 `adapterPolicy()` 在 `core/manager` 模块内未导出；`sdk` 模块看不到它。放进 `sdk` 需要给 `sdk` 新增一条到 `core/floor`（及 `core/edge`）的模块边，违反 `sdk/go.mod` 已声明的不变式"只依赖 `core` 且不携带逻辑"，并把两条模块边推给每一个插件作者的模块图。为一个测试文件付这个代价不成立。

**被否决的备选：** `plugins/opskeeper-adapter-example/` —— `plugins/` 是真实交付目录，会进入 PiG 守卫与节点清单面；`core/harness/fixtures/…` —— 那是集成测试的组装位，不是公开参考。

参考实现满足：`kernel.Contribution`（`PluginName` + `Register(KindAdapter)`）与最小 manifest，`required_scopes: [cloud.read]`（同时钉住 D2），family 名与首方 8 个互不构成前缀关系，注释标明"参考实现，非产品包"，且**不 import 任何首方 adapter 包**。

## 新 family 工具清单接线

新增一个 family 会让 G3 的两条既有测试立刻变成一条 diff，而不是一次静默扩张：

- `TestToolsetMatchesTheAdapters` 用 `PackagedReadTools(Registry())` 与已生成的 toolset 文件比对。贡献 family 一旦进入 `Sources()` 并注册只读工具，其工具就会出现在 packaged 集合里，与已生成文件不一致即失败。
- `TestTheMiddlewareProfilesDeclaredToolsAreExactlyWhatItShipsToTheModel` 双向比对已发布 toolset 与 `pig-ops.yaml` 声明的 `spec.tools`：toolset 提供了但 manifest 未声明的会被**闸门拒绝每一次调用**；manifest 声明了但 toolset 不提供的则"可许可但不可达"。

**本 change 的选择：新 family 的只读工具进入 PiG 中间件 manifest 的 `spec.tools` 允许清单，不进 `NotPackagedFamilies`。**

理由：`NotPackagedFamilies` 保留给"其读取回答的是本节点自身"这一类 family（现存的 `host` 是唯一一例：它以 root 在被管控机器上执行，读取的是控制面被指向的那台主机，而不是这个节点）。候选云读类 family（Karmada / 阿里云 / CMDB）的读取回答的是租户云上资产，正是只读诊断包存在的意义，因此进入允许清单。

**这一选择不落在 S0：** 参考实现不注册任何工具，因此本切片 `pig-ops.yaml` 与 `NotPackagedFamilies` 都**不变**。接线随第一个真实候选包落地，并且必须以 diff 形式出现在评审里——不是把工具加进 toolset 而把 manifest 留在原地。

## Risks / Trade-offs

| 风险 | 等级 | 控制 |
| --- | --- | --- |
| 读回循环改动是本切片唯一的行为风险点 | 中 | D-S0-2 的顺序契约 + D-S0-7 中可独立失败的顺序断言；默认路径与基线逐项一致；PR 中作为独立章节送审 |
| 授予集合扩大被误读成"包侧可申领通道" | 中 | D-S0-5 的注释边界 + 旧 8 项集合的 fail-closed 红灯测试 |
| 参考实现被当作产品包复制 | 低 | 文件内"参考实现，非产品包"注释 + 不放在 `plugins/` + 不 import 任何首方 adapter 包 |
| 实现者顺手加了导出贡献入口 | 低 | D-S0-1 已明令禁止；评审直接拒绝 |

## Rollback

单个 PR，无数据迁移、无部署迁移、无配置变更。回滚 = revert。

回滚后仓库回到 4.4 已记录的"注释与实现不一致"状态（`adapter-plugin-plan.md` §3.1），那不是本次新引入的风险。

## Migration Plan

无。默认路径行为不变：`Sources()` 仍返回首方 8 个 family，顺序、`Env`、`Wire` 逐项一致；授予集合扩大只新增两个只读 scope，不影响既有 family 的准入结果。

## Open Questions

留待后续独立任务，不在 S0 内决策：

- adapter 包在宿主侧的**组装点**。`cmd/opskeeper/main.go` 的 manager 插件宿主目前用 `aiopsdatasource.ManagerPolicy()` 接收 `OPSKEEPER_MANAGER_PLUGINS_DIR` 下的包，也就是按 **datasource** 词表而非 adapter 策略裁决。S0 把 `GrantedScopes` 变成可配置值，正是为了让组装点能换策略而不必改结构；换到哪个组装点由候选包任务单独决策。
- `cloud.write` 的裁决（写操作切片）。
- 导出贡献入口的具体签名（随第一个真实候选包落地）。
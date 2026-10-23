## Why

`adapter` 扩展点是六个封闭扩展点里唯一"贡献出来、但读不回来"的一个。`Sources()` 在 G1 修复之前自建宿主、只激活 8 个首方 family，然后按 `firstPartySources()` 遍历回读——即便一个第三方包把 family 成功 `Activate` 进同一个 registry，回读循环也只会走首方字面量，于是贡献在"激活成功"与"可用于闭环"之间被静默丢弃。

这不是一个还没写到的功能，而是一处**注释与实现不一致**：`Sources()` 的文档写着"第三方 adapter family 会落在同一个列表里"，实现并不会。宿主前置切片 S0 是 `plugin-architecture-recovery-plan` §7.1 第 1 步，也是 4.4 记录的那条注释—实现漂移（`adapter-plugin-plan.md` §3.1）。

S0 同时把 D1/D2 的权限词表裁决落到代码：`topology.read`（D1）与 `cloud.read`（D2）进入 adapter 宿主的默认授予集合。裁决见 `adapter-plugin-plan.md` §6.4（D1）与 §5.4（D2）。裁决之所以在 S0 落地，是因为没有它们，一个候选包即使走通贡献路径，也会在准入门上被拒——G2 会把 G1 的修复卡在门外。

## What Changes

- 把 `Sources()` 的读回逻辑提取为内部函数 `sourcesFrom(thirdParty []Source) []Source`，第三方 family 走**同一条** `host.Activate` + `adapterManifest` 闸门进入同一个 registry，不新增并行注册通道。
- 固定顺序契约：首方 8 个保持 `firstPartySources()` 顺序并构成**严格前缀**；第三方 family 排在首方块之后，按 family 名字典序。默认路径（无第三方贡献）的顺序与基线逐项一致。
- `Sources()` 的文档注释按新行为逐句重写，不再保留"第三方落在同一列表"这句在实现不成立时的措辞。
- 提取导出构造器 `AdapterPolicy(granted domain.Scopes) kernel.Policy`；`adapterPolicy()` 保持未导出，等于 `AdapterPolicy(默认授予集合)`。默认授予集合 = 既有 8 项（原样、原序）+ `topology.read` + `cloud.read`，共 10 项。`MaxSafetyLevel: L2` 与 `MaxBlastRadius: single-ns` 不变。
- `core/domain/plugin.go` 增加 `ScopeCloudRead Scope = "cloud.read"`。**不引入** `cloud.write`——写权限在写操作切片单独评审。
- 补齐独立测试：贡献 family 出现在 `Sources()` 中且位于首方块之后、顺序稳定、字典序；旧 8 项授予集合**拒绝** `cloud.read` 并指名缺失 scope（红灯是正确结果）。
- 提取 `wireSources(ctx, log, reg, srcs)`，使启动接线可对贡献 family 断言"恰好接线一次"，无需真实后端。
- 提供第三方 family 贡献的公开参考实现（Go 测试文件，标注"参考实现，非产品包"）。
- 新增 `openspec/changes/adapter-host-slice-s0/`，本 change 即 §7.1 第 1 步的实施记录。

### 明确不在本切片内

- **不实现任何候选包**（Karmada / 阿里云 / CMDB）。候选包是后续独立任务。
- **不导出第三方贡献入口**（不提供 `toolset.Contribute(...)` 之类的 API）。没有消费者的导出等于提前冻结 API，且 §7.3 禁止并行注册通道；该导出随第一个真实候选包一同落地。
- 不改 `firstPartySources()`（不塞入任何第三方条目）、不改 `core/floor/kernel/kind.go`、不改 `NotPackagedFamilies`/`NotPackaged`、不改 `plugins/pig-ops/opskeeper-sre-middleware/pig-ops.yaml`。

## Capabilities

### New Capabilities

- `adapter-host-slice-s0`：adapter 宿主的贡献可见性契约（`sourcesFrom` 的顺序契约与第三方路径）、准入授予集合的宿主侧可配置契约，以及新 family 的工具清单接线规则。

### Modified Capabilities

- 无。本仓库尚无归档的主规格（archived main specs），本切片只修复既有实现与注释之间的漂移，不改变已归档能力的对外语义。

## Impact

- `core/domain/plugin.go`：新增一个 scope 常量。`core/floor/kernel/kind.go` 不动，扩展点集合不变。
- `core/manager/middleware/toolset/`：`sources_plugin.go` 重构 + 新增测试 + 参考实现；`sources.go` 与 `toolset.go` 不动，因此 `families()`、`ParseFamily()` 的默认行为不变。
- `cmd/opskeeper/loop_adapters.go`：循环提取为 `wireSources`，原 `wireLoopRemediationAdapters` 行为等价地转调。
- 模块图不变：`core/manager`、`core` 与根模块的依赖关系没有新增边。
- 无数据迁移、无部署迁移、无配置项变更。回滚即 revert 单个 PR。
# Tasks: adapter-host-slice-s0

对应 `plugin-architecture-recovery-plan` §7.1「adapter 宿主前置切片 S0」五步，以及 §7.1 中标注的 G1 修复与 D1/D2 落地。

## 1. 修复 G1：贡献可见性（§7.1 第 1 步，唯一有行为风险的步骤）

- [x] 1.1 提取 `sourcesFrom(thirdParty []Source) []Source`，`Sources()` 等于 `sourcesFrom(nil)`
- [x] 1.2 第三方条目走同一个 `host.Activate` + `adapterManifest`，保持 `ErrDuplicate` 语义，不新增并行注册通道
- [x] 1.3 从 registry 快照回读：首方 8 个按 `firstPartySources()` 顺序构成严格前缀，第三方按 family 名字典序排在之后
- [x] 1.4 显式排序，不依赖 registry 内部顺序或包 init 顺序；两次调用同输入返回同列表
- [x] 1.5 按新行为逐句重写 `Sources()` 文档注释，不保留实现不成立的断言
- [x] 1.6 **不导出**第三方贡献入口（随第一个真实候选包落地）

## 2. G1 修复的测试

- [x] 2.1 改写 `TestSourcesIsDerivedFromTheKernel`：各首方 family 出现且仅出现一次 / 相对顺序 / `Env` 与 `Wire` 非空，三段独立断言
- [x] 2.2 新增贡献 family 出现在结果中、位于首方块之后、两次调用顺序一致
- [x] 2.3 新增两个贡献 family 时按 family 名字典序（输入顺序与期望相反）
- [x] 2.4 新增同名贡献不覆盖首方条目
- [x] 2.5 新增启动接线断言：贡献 family 的 `Wire` 恰好被调用一次；未配置 DSN 时不接线

## 3. 策略可配置 + D1/D2 落地

- [x] 3.1 `core/domain/plugin.go` 增加 `ScopeCloudRead Scope = "cloud.read"`，不引入 `cloud.write`
- [x] 3.2 提取导出构造器 `AdapterPolicy(granted domain.Scopes) kernel.Policy`
- [x] 3.3 `adapterPolicy()` 保持未导出，等于 `AdapterPolicy(默认授予集合)`
- [x] 3.4 默认授予集合 = 既有 8 项（原样原序）+ `topology.read` + `cloud.read`
- [x] 3.5 `MaxSafetyLevel: L2`、`MaxBlastRadius: single-ns` 不随授予集合变化
- [x] 3.6 注释写明 `GrantedScopes` 是宿主侧可组装值、不是包侧可申领值

## 4. D1/D2 独立测试

- [x] 4.1 默认授予集合恰好 10 项，含 `topology.read` 与 `cloud.read`，前 8 项与基线逐项一致
- [x] 4.2 只读 `cloud.read` / `topology.read` 的 manifest 在默认策略下通过准入
- [x] 4.3 旧 8 项授予集合**拒绝** `cloud.read` 并指名缺失 scope（红灯是正确结果）
- [x] 4.4 策略上限不随授予集合变化

## 5. 第三方贡献参考实现

- [x] 5.1 实现 `kernel.Contribution`（`PluginName` + `Register(KindAdapter)`）与最小 manifest
- [x] 5.2 `required_scopes: [cloud.read]`（同时钉住 D2），在由 `adapterPolicy()` 推导的 `Admission` 下通过 `Admit`
- [x] 5.3 family 名与首方 8 个互不构成前缀关系
- [x] 5.4 注释标明"参考实现，非产品包"
- [x] 5.5 不 import 任何首方 adapter 包
- [x] 5.6 可按名回滚（`Deactivate`）

## 6. 工具清单接线与文档

- [x] 6.1 在 `design.md` 记录新 family 的工具清单接线选择（见「新 family 工具清单接线」一节）
- [x] 6.2 说明 G3 两条既有测试如何把该选择变成一次 diff
- [x] 6.3 本切片不改动 `pig-ops.yaml` 与 `NotPackagedFamilies`/`NotPackaged`

## 7. 验收

- [x] 7.1 `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck`
- [x] 7.2 `go test ./...` 全量
- [x] 7.3 硬约束逐条自检：不实现候选包、`firstPartySources()` 无第三方条目、`kind.go` 未改、未绕过 `sdk/manifest.go` 校验与 `adapter/` 目录约束

## 8. 明确不在本切片

- [ ] 8.1 候选包（Karmada / 阿里云 / CMDB）——后续独立任务
- [ ] 8.2 导出第三方贡献入口——随第一个真实候选包落地
- [ ] 8.3 adapter 包在宿主侧的组装点（含 manager 插件宿主当前使用 datasource 策略的问题）
- [ ] 8.4 `cloud.write` 裁决——写操作切片
- [ ] 8.5 `pig-ops.yaml` / `NotPackagedFamilies` 接线——随第一个注册只读工具的真实候选包落地
## 1. B1：内核胶水包 `core/manager/biz/notify/`

- [ ] 1.1 新建包（与 `biz/aiops/datasource` 同位），`policy.go` 提供 `Policy()`：从
       `aiopsdatasource.ManagerPolicy()` 派生，**恰好两个 delta**——
       `WithWiderKindCeiling(kernel.KindNotifier, domain.SafetyL3)` 与 `GrantedScopes`
       追加 `domain.ScopeAlertWrite`；MUST NOT 用 `WithKindCeiling`（全局 L2 之上写 L3
       是放宽，写反方向 panic）；MUST NOT 改 `Target` / `MaxSafetyLevel` /
       `MaxBlastRadius` / 既有四个 scope。
- [ ] 1.2 `Plugin*` 六个包名常量（`opskeeper-notifier-{webhook,slack,feishu,dingtalk,wecom,telegram}`），
       包名与渠道类型是两个字符串，与 datasource 的 `PluginProm`/`NameProm` 同一理由。
- [ ] 1.3 `channelPlugin{plugin, typ string, f notify.ChannelFactory}` 实现
       `kernel.Contribution`：`PluginName()` 回包名，`Register` 往
       `kernel.KindNotifier` 注册**渠道类型**作为条目名。
- [ ] 1.4 `manifestFor(plugin, typ)`：L3 / `capabilities: [destructive]` /
       `required_scopes: [alert.read, alert.write]` / `targets: [manager]` /
       `audit: {emits: true, mutates: false}` / `approval: {required: false}` /
       `install: {strategy: pin}` / `version: 1.0.0` / `vendor: opskeeper`。对齐已评审的
       `plugin-architecture-recovery-plan/notifier-manifests/builtin-channels.yaml`，
       不写 `min_pig_version`（与二进制同构建）。
- [ ] 1.5 `Backends{Webhook, Slack, Feishu, DingTalk, WeCom, Telegram notify.ChannelFactory}`
       与 `Builtins()`：六个 factory **逐字复制**自 `registry.go` 的 map 字面量
       （`registry.go` 一字不动，见 `design.md` §3）。
- [ ] 1.6 `Resolve(reg *kernel.Registry) Set` 与 `Set`：`Factory(typ)` / `Holder(typ)` /
       `Types()`；`Set` 结构上满足 `notify.ChannelResolver`，不写适配器。
- [ ] 1.7 `Activate(host, b, log) (Set, error)`：nil 字段跳过；`errors.Is(err,
       kernel.ErrDuplicate)` 记 **Info**（点名让位方与持有方）后继续；**其余错误原样
       返回**（装配层 `os.Exit(1)`）；全部尝试后 `Resolve`，不在此安装视图。
- [ ] 1.8 `Wire(host, b, log) (Set, error)`：`Activate` → `Resolve` →
       `notify.InstallChannelResolver(set)` → 一条 Info `notification channels
       activated`（带 `types` 与每类型的持有包）。安装放最后。

## 2. B2：`core/base/pkg/notify` 的读取视图

- [ ] 2.1 新文件（`registry.go` 不动）：`ChannelResolver` 接口 + `InstallChannelResolver`
       （传 nil = 卸载，回到迁移前路径）。MUST NOT 新增 `core/floor/kernel` /
       `core/floor/pluginmanifest` / `core/domain` 的 import（`make module-check` 验收）。
- [ ] 2.2 `BuildChannel` 读取顺序按 `design.md` §6 的表：未装视图⇒只读旧 map 且**零日志**；
       命中视图⇒用视图；未命中⇒回落旧 map 并打 **Warn，每个类型至多一条**；两边都没有⇒
       错误文本 `unknown channel type %q` 与今天一字不差。Warn 走视图自带 logger
       （`BuildChannel` 的 `log` 参数可能为 nil）。
- [ ] 2.3 空名 sender 的 `built an unnamed sender` 分支文本与判定不变。

## 3. B2：装配层 `cmd/opskeeper/main.go`

- [ ] 3.1 `pluginhost.New(managerPluginsDir, aiopsdatasource.ManagerPolicy(), pluginLog)`
       改为 `notifybiz.Policy()`；`datasource.ManagerPolicy()` 本身一字不改。
- [ ] 3.2 `ph.Load(ctx)` 块**之后**（无条件、目录不存在也要执行）调
       `notifybiz.Wire(ph.Kernel(), notifybiz.Builtins(), pluginLog)`；出错
       `log.Error` + `os.Exit(1)`。顺序是语义，MUST NOT 放到 `ph.Load` 之前。
- [ ] 3.3 不动既有的 `control-plane plugin capabilities` 日志行（它在 `ph.Load` 之前，
       因此不含 notifier 条目，逐字节不变——见 `design.md` §7）。

## 4. 测试

- [ ] 4.1 `TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel`：六类型 ×
       旧 map / 内核两条路径，打到同一捕获端点，断言 `Name()`、HTTP 方法、路径、**请求体
       字节**、签名 header 全等；收尾 `InstallChannelResolver(nil)`。
- [ ] 4.2 `TestAThirdPartyVariantOwningSlackWinsOverTheBuiltinAndBootStillSucceeds`：
       假第三方包先占 `slack`，`Wire` 不报错、让位是 Info、其余五个激活、
       `BuildChannel("slack")` 拿到**第三方** factory。
- [ ] 4.3 `TestNotifierPolicyIsManagerPolicyPlusExactlyTwoDeltas`。
- [ ] 4.4 `TestAnL3PackageIsStillRefusedInOtherKindsUnderTheNotifierPolicy`：L3 包过
       包级闸门、在 `KindTool` 上被拒、整包回滚、`ph.Load` 报 Refused 而非致命。
- [ ] 4.5 `TestWireLogsExactlyTheWhitelistedRecords`：捕获型 handler，断言恰 1 条
       `notification channels activated` + 6 条内核 `plugin activated`，Warn 及以上为 0。
- [ ] 4.6 `TestBuildChannelFallsBackToTheLegacyMapOncePerType` 与
       `TestInstallNilRestoresThePreMigrationPath`。
- [ ] 4.7 错误文本等价（`design.md` I3）：非法 spec 三条路径的错误文本全等。
- [ ] 4.8 既有测试**零修改**全绿：`core/base/pkg/notify` 三个测试文件、
       `core/manager/biz/alert/`、`tests/e2e/notify_*`（G1 / G3 / G4 / G8）、
       `testenv/fakes.go`。

## 5. 闸门与交付

- [ ] 5.1 `make pi-plugin-guard` 通过。
- [ ] 5.2 `make module-check` 通过（含 C3：`notify` 不新增 kernel / pluginmanifest /
       domain import）。
- [ ] 5.3 `go test ./scripts/ledgercheck` 通过；`docs/opskeeper2-architecture.md` 的模块
       读数与 notifier 段落同步更新（新增包与文件计入）。
- [ ] 5.4 全量 `go test ./...`（根 + core/base + core/floor + core/manager + core/edge）
       通过；`make deadcode-ratchet-check` 通过，需要抬基线时同一提交写明理由。
- [ ] 5.5 e2e `-tags e2e` 的 notify 用例通过（G1 / G3 / G4 / G8）。
- [ ] 5.6 手工日志对照：改动前后各启动一次 manager（无 `OPSKEEPER_MANAGER_PLUGINS_DIR`），
       diff 两份启动日志，确认"既有行逐字节不变 + 新增行恰为白名单 7 条 Info"，结果贴进
       PR 描述。
- [ ] 5.7 `openspec validate --all --no-interactive` 通过。
- [ ] 5.8 硬约束复核（`design.md` §10 C1–C8）：`git diff --name-only` 不含
       `kind.go` / `registry.go` / `datasource.go` / `sources_plugin.go` /
       `core/manager/biz/alert/` / `plugins/` / 既有测试文件。
- [ ] 5.9 审计口径：入库文件不含任何内部任务号（`scripts/audit_open_source.py` 的
       `internal task ID` 规则）。
- [ ] 5.10 PR：Conventional Commits 前缀 + 中文说明；描述里写清 §4.1 的两条治理后果
       （`alert.write` 宿主级授予、包级闸门抬到 L3 但 per-kind 仍拒），并关联 OpenSpec
       change `notifier-builtin-kernel-migration` 与 `plugin-architecture-recovery-plan`
       任务 4.5。
- [ ] 5.11 评审确认 `design.md` §11 Q1（落点偏差）后再合入。

## 6. 不在本 change

- [ ] 6.1 B3：删除 `registry.go` 的旧 map 与回落路径、收编 `RegisterChannel`。
- [ ] 6.2 候选插件包（S-8）、渠道类型枚举扩展（S-5）、Router 重名（S-6）、
       `ChannelSpec` 凭证模型（S-7）。
- [ ] 6.3 per-kind scope（`alert.write` 的收窄面）与运行期插件重载（`Set` 活查询）。

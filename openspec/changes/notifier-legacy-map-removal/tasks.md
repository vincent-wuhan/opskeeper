## 1. B3：`core/base/pkg/notify` 删旧 map 与回落

- [x] 1.1 `registry.go`：删除 `channelFactories` map、`channelMu`、`RegisterChannel`、
       `RegisteredChannels`（ADR-1 / ADR-2）；`BuildChannel` 只读视图，`unknown channel
       type %q` 文本一字不改；`ChannelSpec` / `ChannelFactory` / `NewRouterFromSpecs` /
       `specsFromConfig` 不动。
- [x] 1.2 `resolver.go`：`InstallChannelResolver` 改为持指针（nil = 无视图）；
       删除 `displacedByView` / `addedByView` / `restoreRegistryLocked` /
       `fallbackFactory` 与回落 Warn。
- [x] 1.3 `ChannelResolver` 收窄为 `Factory(typ)` 单方法；`Set.Logger()` 删除（ADR-3）；
       `biz/notify.Wire` 的安装调用零改动（结构满足继续成立）。
- [x] 1.4 `notify` 不新增 kernel / pluginmanifest / domain import；`sort` / `sync` 若因此
       变为无引用则一并清除。
- [x] 1.5 D-1b 装配序修正：`cmd/opskeeper/main.go` 把 `notifyRouter := notify.NewFromConfig(...)`
       连同其第一个消费者 `alertSvc` 构造块（含 `SetPreviewDeps`）移到 `notifybiz.Wire`
       成功之后——`NewFromConfig` 急切构渠道，删 map 后若仍先于 Wire，配置文件渠道会被
       "跳过并 Warn"静默清空（B2 靠回落掩盖了这个倒挂，e2e 因不配置文件渠道而不可见）。

## 2. 测试收口（`core/base/pkg/notify`）

- [x] 2.1 退役 `TestBuildChannelFallsBackToTheLegacyMapOncePerType` /
       `TestInstallNilRestoresThePreMigrationPath`；新增
       `TestBuildChannelWithoutAViewRefusesEveryType`（六类型 + 未知类型一律
       `unknown channel type`，零日志）与 `TestInstallNilReturnsTheProcessToNoChannels`。
- [x] 2.2 改写 `TestUnknownChannelTypeErrorTextIsUnchanged` /
       `TestTheUnnamedSenderErrorTextIsUnchangedAcrossTheThreePaths`（三路径收两路径）/
       `TestTheViewAnswersForATypeItHolds`（零 Warn 改零日志）；文本断言保留。
- [x] 2.3 `registry_test.go`：退役 `TestRegisteredChannelsCoversBuiltins` /
       `TestRegisterChannel*` / `TestRegisterThenBuildThirdPartyChannel`；改写
       `TestNewRouterFromSpecsBuildsRegisteredChannels` /
       `TestUnknownChannelTypeIsSkippedNotFatal` /
       `TestNewFromConfigBuildsTheSameChannels` 为显式装视图，断言保留。
- [x] 2.4 新增源级钉子 `TestThePackageHoldsNoSecondRegistry`：`registry.go` / `resolver.go`
       文本中不再出现 `channelFactories` / `RegisterChannel` / `RegisteredChannels`。
- [x] 2.6 新增装配序钉子 `TestNewFromConfigBuildsChannelsThroughTheInstalledView`：
       装视图时配置渠道建成，卸视图后同一配置产出空 router——D-1b 的可执行化。
- [x] 2.5 `notify_test.go` / `webhook_test.go` / `resolver_test.go` 的 `stubResolver` 收窄
       到 `Factory` 单方法；其余零改动。

## 3. 测试收口（`core/manager/biz/notify`）

- [x] 3.1 改写 `TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel`：旧路径一侧
       改为 `Builtins()` factory 直接构造；线上对比口径（Name / 方法 / 路径 / body 字节 /
       签名 material / 时钟归一化）全部保留。
- [x] 3.2 新增 `TestWireInstallsTheViewAndBuiltinsAnswerThroughIt`：`Wire` 后不卸载视图，
       六类型经 `BuildChannel` 可建（钉装配序）。
- [x] 3.3 `TestAThirdPartyVariantOwningSlackWinsOverTheBuiltinAndBootStillSucceeds` /
       `TestWireLogsExactlyTheWhitelistedRecords` /
       `TestNotifierPolicyIsManagerPolicyPlusExactlyTwoDeltas` / `policy_test.go` 零改动。

## 4. 闸门与交付

- [x] 4.1 `make pi-plugin-guard` 通过。
- [x] 4.2 `make module-check` 通过。
- [x] 4.3 `go test ./scripts/ledgercheck` 通过（涉及 `core/base` / `core/manager` 读数的
       话同一提交更新台账）。
- [x] 4.4 五模块全量 `go test ./...`（根 + core/base + core/floor + core/manager + core/edge）。
- [x] 4.5 `make deadcode-ratchet-check` 通过（净删除预期读数向下，基线调整同一提交写明）。
- [x] 4.6 e2e `-tags e2e`：G1 / G3 / G4 / G8 四条零修改通过。
- [x] 4.7 `openspec validate --all --no-interactive` 通过。
- [x] 4.8 `scripts/audit_open_source.py` 通过（无内部任务号）。
- [x] 4.9 硬约束复核：`git diff --name-only` 不含 `kind.go` / `datasource.go` /
       `sources_plugin.go` / `core/manager/biz/alert/` / `plugins/` / `core/floor/kernel/` /
       manifest schema。
- [x] 4.10 交付走 patch 附件通道；PR 描述写明行为变更（未装视图宿主从可建六渠道变为
       `unknown channel type`）与两个删除的导出符号，关联本 change 与
       `plugin-architecture-recovery-plan` §5.1 B3；CHANGELOG 记 BREAKING 一条。

## 5. 不在本 change

- [ ] 5.1 候选插件包（S-8）、渠道类型枚举扩展（S-5）、Router 重名（S-6）、`ChannelSpec`
       凭证模型（S-7）。
- [ ] 5.2 per-kind scope（`alert.write` 收窄面，B2 Q2）与运行期插件重载（`Set` 活查询，
       B2 Q3）。

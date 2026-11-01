# 插件架构恢复方案：全量闸门运行与报告（任务 4.11）

> 口径：本报告只**运行**闸门、把一次性验证**固化**成仓内测试、对灰度与回滚面做**核对**；
> 不修改任何产品代码。属于实现切片（S-0…S-5、G0 等）的失败一律记入 §6 遗留项，不在本任务内修。
>
> 基线：`main`（闸门运行时点为 `21efd90a`）。工具链：go1.26.0；Python 依赖（pytest / fastapi / httpx /
> pyyaml / python-multipart）装在仓库外 venv，经 `make test-plugins PYTHON=<venv>/bin/python` 使用；
> dashboard 测试依赖 `npm install --prefix plugins/opskeeper-teamharness/dashboard` 先行安装。
>
> 变基说明：运行闸门之后，台账 4.10（`bc054167`，仅新增 `plugin-governance-ledger.md` 与两份
> adapter 草案的状态注释，未增删任何 Go 文件）合入 main，本分支随之变基到其上并消解
> `tasks.md` 的相邻行冲突（两侧各自勾选 4.10 与 4.11，全部保留）。变基前后 `core/manager`
> 均为 1038 个 Go 文件 / 262,324 行，第 3 节全部 Go 侧结果不受影响；adapter 草案目录的文件
> 清单（alicloud / cmdb / karmada）在 4.10 合入前后不变，注释改写不影响解码。

## 1. 运行环境里两件必须先写下来的事

1. **Go 模块代理必须用 goproxy.cn。** 本机曾用 aliyun 镜像拉取 `github.com/MichaelKinsy/PiG@v0.4.0`，
   下载体与 `go.sum` 校验和不一致（`verifying … checksum mismatch`）——镜像内容与上游 tag 不符。
   换 `GOPROXY=https://goproxy.cn,direct` 后与 `go.sum` 一致。`GOSUMDB=off` 时 go.sum 仍然强制校验，
   所以此配置没有削弱任何保证。任何人在 aliyun 镜像上跑 `go mod download` 都会得到一个"看似成功
   实则被拒"的构建，这条记录在这里是为了让下一个人少走这一步。
2. **多模块仓库的测试口径。** `go.work` 是不入库的本地文件（`.gitignore`），CI 里没有它。
   仓内权威口径是 `make module-standalone-check` 那条循环：进入各模块目录后 `GOWORK=off go test`。
   本报告所有逐模块命令都按这条口径跑。

## 2. 闸门命令逐条结果

| # | 命令 | 结果 | 输出摘要 |
|---|---|---|---|
| 1 | `make pi-plugin-guard` | ✅ | `pi-plugin-guard: 1 plugin(s) valid (upup)`；根工作区 `no Pi plugins declared; workspace contract ready`；PiG 侧 6 个 node 测试 OK |
| 2 | `make module-check` | ✅ | `modulecheck: all module boundaries hold` |
| 3 | `go test ./scripts/ledgercheck` | ✅（先红后绿） | 初跑两条红：第十五道闸门判定 manager 规模行与树不符（1037/262,081 vs 1038/262,324）。原因不是缺陷：本任务在 `core/manager` 新增 1 个闸门固化测试文件（+243 行），分母因补守卫第十八次变大。按该行自己写下的两条命令重取并回填 `docs/opskeeper2-edge-autonomy-plan.md` 与 `docs/opskeeper2-architecture.md` 后全绿。**这道闸门当场工作了一次，这正是它存在的目的。** |
| 4 | `go test ./core/floor/pluginmanifest/... ./sdk/...` | ✅ | `ok …/core/floor/pluginmanifest 3.117s`、`ok …/sdk 0.076s`（floor 模块与 sdk 模块各自的全部用例） |
| 5 | `go test ./plugins/...` | ⚠️ 口径问题 | `matched no packages`——`plugins/` 下各包是**独立模块**，不在根模块里，这条命令在仓库任何地方都匹配不到包。正确口径见 #6；建议后续把 README/闸门文档里这条写法改成逐模块形式（遗留项 L7） |
| 6 | 逐模块 `GOWORK=off go test ./...`（`plugins/opskeeper-datasource-victoriametrics` + `core/pig/extensions/opskeeper-sre-{observability,readonly,middleware,repair,autonomy}`） | ✅ | 全部 `ok`；VictoriaMetrics 的 `internal/vmclient`、`internal/vmquery` 单测通过（真实 TSDB 集成部分由 CI 的 integration tag 承担，见遗留项 L2） |
| 7 | `make test-plugins` | ✅ | `274 passed, 1 warning in 16.40s`；前置步骤 `bash -n demo_preflight.sh`、npm test、agentteams self-check（含 dist/main.js 42489 字节、zip 冒烟）全过 |
| 8 | `make build-plugins` | ❌ 环境限制 | `plugins/opskeeper-teamharness/scripts/build-package.sh: line 8: ruby: command not found`。本机无 ruby 且无 root 权限装不了，属环境限制不是闸门缺陷；CI 有 ruby。`verify_release` 签名验证链同样需要这个包产物（遗留项 L6） |
| 9 | `make audit-open-source` | ✅ | `open-source gate passed: 3461 text files audited in the tracked tree`（新增文件后复跑仍绿，受审计文件不含内部任务编号） |
| 10 | 本任务新增的三份固化测试 | ✅ | `cmd/opskeeper`（8 个用例：datasource 4 + provider 2 + notifier 2）、`core/manager/middleware/toolset`（3 个用例，其中 1 个含 3 个子用例）、`cmd/opskeeper-edge`（2 个用例，其中 1 个含 6 个子用例）全部通过；三个包的**整包**测试套件全绿 |

## 3. 准入闸门复核与固化（此前的"一次性跑过、未留测试"）

各方案原文都注明草案 manifest 只在成文时人工跑过一次 `sdk.Decode → sdk.Validate → sdk.Admit`。
本任务把每一次复核都固化成仓内可重复的测试，选址原则是**闸门住在它对照的 policy 旁边**：

| 扩展点 | 固化文件 | 对照的真实 policy |
|---|---|---|
| 4.3 datasource（3 份） | `cmd/opskeeper/plan_manifests_gate_test.go` | `aiopsdatasource.ManagerPolicy()`（真构造函数，非复述） |
| 4.4 adapter（3 份） | `core/manager/middleware/toolset/adapter_plan_manifests_test.go` | `adapterPolicy()`（包内未导出，只有住在这里才取得到真本） |
| 4.5 notifier（6 份） | `cmd/opskeeper/plan_manifests_gate_test.go` | 现状 `ManagerPolicy()` + 方案 §4.6 的 NotifierPolicy() 形状 |
| 4.6 provider（3 份） | `cmd/opskeeper/plan_manifests_gate_test.go` | 现状 `ManagerPolicy()` + 方案 §4.6 的 ProviderPolicy() 形状 |
| 4.7 collector（3 份） | `cmd/opskeeper-edge/collector_plan_manifests_test.go` | `pluginhost.EdgePolicyFor(ProfileFinance / ProfileSaaS)`（部署 profile 渲染出的真 policy） |

三份加载器有一小段重复（定位草案目录 + 清单校验）。这是模块边界的直接后果：三个测试分属三个
模块，谁也不能 import 谁。代价是三份各约 40 行的重复，换来的是每道闸门都能引用**未导出的真
policy 构造点**——换成共享库就只能复述 policy，而复述可以和真本各说各话。

### 3.1 复核结论总表

| 族 | 正例 | 负例（全部断言拒绝理由文本，不只断言"被拒"） |
|---|---|---|
| datasource | 3 份草案在 `ManagerPolicy()` 下全过；逐份钉住 L1 / `[read]` / `observability.read` / 兼容范围 0.4.0+0.8.0 | ① 把 `observability.read` 换成宿主不授予的 `log.read` → `not granted: log.read`；② `strategy: canary` → `want "rolling" or "pin"`（词表外值在解码期被拒）；③ 自报 L2 的变体在全局 L1 ceiling 下被拒、在 per-kind 收窄下**不**被拒（见 §3.2-1） |
| adapter | 3 份草案在 `adapterPolicy()` 下全过；钉住 L1 / `[read]` / `[manager]` / `rolling` | 把授予集收回 D1/D2 裁决前的八项一方 scope：cmdb → `not granted: topology.read`，alicloud → `not granted: cloud.read`，karmada 照常通过。即**方案里的两条琥珀逐字复现，撤掉授予就回来**——这就是 D1/D2"只改了授予这一件事"的证据 |
| notifier | 6 份草案在 §4.6 形状（notifier 抬到 L3）下全过；钉住 L3 / `[destructive]` / 恰好 `[alert.read alert.write]` / `pin` | ① 同样授予集、不抬 per-kind（全局 L2）→ `plugin is L3, target policy allows at most L2`（§11.1 的"L2 列"）；② **今天的 `ManagerPolicy()` 拒得更早**：`not granted: alert.write`（见 §3.2-3）；③ §4.6 代码片段里的 `WithKindCeiling(KindNotifier, L3)` 在今天的内核上是构造期 panic——已固化成断言，防止有人原样抄进仓 |
| provider | 3 份草案在现状 `ManagerPolicy()` 下全过（§10.1 的"现状过"列）；钉住 L2 / `[write]` / 空 scope 集 / `pin` | §10.1 四个负对照全部固化：A 低报（L1+[read] 过 L1 全局 ceiling——闸门识别不了低报，这条断言把"我们知道挡不住什么"写进台账）；B scope 膨胀（现状放行、空授予集的 ProviderPolicy 形状拒 `not granted: observability.read, db.read`）；C 升级线（L3+destructive → `plugin is L3, target policy allows at most L2`）；D 双目标（`targets [manager, edge]` 在 manager 宿主包级闸门照样放行——挡它的是 S-2，不是这里） |
| collector | 3 份草案在 finance（L2/pod）与 saas（L3/ns）两档下全过 | §9.1 六个负对照全部固化：`spec.collector` 块 → 解码拒 `field collector not found in type domain.PluginSpec`；`k8s.exec` → finance 拒 / saas 过；L1 挂 `max_blast_radius: pod` → 解码拒 `L1 is read-only and cannot carry a blast radius`；L3 → finance 拒 / **saas 过**；复刻 host.go 注释里那个被真实节点证伪的"L1 单 scope"旧 policy → 全部拒 `not granted:`；manager 宿主 → `targets [edge], host runs "manager"` |

### 3.2 复核中发现的四处方案↔代码漂移（都已写成断言，不是改掉）

1. **per-kind ceiling 在包级闸门不生效，生效点在 Activate。** datasource 方案 §7 与 provider 方案
   §10.1 的表格都把"收窄这个 kind 的 ceiling → 包被拒"当成包级现象。实际上 `Host.Admit` 用的是
   `WidestSafetyCeiling()`（kernel/host.go 的注释写明了为什么：manifest 只报一个等级，不说自己
   会把条目落进哪个 kind，包级闸门只能取最宽的那个，否则会拒掉宿主收得下的包）。所以
   `WithKindCeiling(KindDatasource, L1)` 之后，自报 L2 的草案在包级**仍然放行**——要在包级拒
   它，只能压低全局 ceiling。真正的拒绝点在 `Activate` → `scopedRegistrar.checkCeiling`，且拒绝
   触发整包回滚。固化测试两层都断言了：包级放行（并注明为什么）、激活期拒绝 + 注册表零残留
   （用一个测试贡献体注册 `KindDatasource` 条目走真实 `Activate` 路径）。方案成文时的一次性
   验证没经过宿主，所以看不见这两层。
2. **provider 负对照 A 必须同时改两个字段。** 只把 `safety_level: L2` 改成 `L1`，`[write]` 会在
   解码期先被 `Validate` 拒掉（`declares "write" but safety_level L1 permits at most "read"`），
   到不了想断言的那道闸门。固化测试在 YAML 原文上同时替换两处（`editRaw` 辅助函数）。
3. **notifier 的"L2 列"在现状 policy 下是另一个拒绝。** §11.1 假设授予集已是 §4.6 的形状；对
   今天的 `ManagerPolicy()`（有 alert.read、没有 alert.write），六份草案在 scope 检查就被拒
   （`not granted: alert.write`），L3 还没被比到。固化测试两列都断言：§4.6 授予集 + 全局 L2 →
   安全等级拒绝；现状 ManagerPolicy → scope 拒绝。
4. **datasource 方案 §7 的灰度句是旧词表。** 原文写"灰度由 `install.strategy=canary` 控制；先
   `canary` 再 `rolling`，最后 `replace`"。今天 `InstallPolicy.Valid()` 只接受 `""`/`rolling`/`pin`，
   `canary` 与 `replace` 都装不进来，灰度是 `rolling` 的固定行为（首波 1/10 金丝雀 + 每波 10 台），
   见 §4。已固化成 `TestTheInstallStrategyVocabularyHasNoCanaryValue`（解码期拒绝）。

### 3.3 这些固化测试同时是 4.10 台账"测试覆盖状态"列的证据

每个草案目录的加载器都带**期望清单**：目录里多一份、少一份 `.yaml` 都会让测试红。也就是说
"哪份草案有闸门"不再靠人记——新草案落地那天必须同步这里，被删除（方案转正为真实包）那天
测试会指名要求把闸门搬去 `plugins/<pkg>/pig-ops.yaml`，而不是静默变绿。目录定位按相对后缀
搜索，change 归档进 `openspec/changes/archive/` 后继续工作。

## 4. 灰度核对：`install.strategy` 逐包结论

**词表**（`core/domain/plugin.go` `InstallPolicy.Valid()`）：只接受 `""` / `rolling` / `pin`。
`canary` / `replace` 不存在——它们在解码期就被拒（固化测试见 §3.2-4）。

**消费点**（全部实读代码核对）：

| 消费点 | 读什么 | 生效方式 |
|---|---|---|
| `core/floor/pluginmanifest/rollout.go` `PlanRollout`（:99） | `strategy` 参数 | **唯一真正改变波次形状的地方**。`pin` → 整队单波（"pinned 包做金丝雀是演戏"）；`rolling`（及一切非 pin 值）→ 首波金丝雀 `len*1/10`（最少 1 台）+ 每波 10 台（`WaveSize`/`CanaryFractionTenths`）；金丝雀由包名+版本哈希**确定性**选出，重试不会换一批金丝雀 |
| `core/domains/service/plugin/manager.go`（:81,:137）→ `rollout.go` `Plan`（:179） | `req.Strategy` | 把包的 strategy 透传给 `PlanRollout`，自身不做第二种解释 |
| `core/manager/server/aiops/crystallizedrelease.go`（:185） | `pkg.Manifest.Spec.Install.Strategy` | 发布接口把它原样交给 `StartRelease`，即固化发布走同一条 `PlanRollout` |
| `core/floor/pluginmanifest/catalog.go`（:148） | 同名字段 | **仅展示**（目录视图），不做决策——不算生效点 |
| `core/domains/service/plugin/compatibility.go`（:72-73） | `MinEdgeVersion` / `MinPigVersion` | 兼容下限的真实消费点（与本节并列的另一半闸门） |

**逐包结论**（"生效"= 会改变 `PlanRollout` 的波次形状；"治理声明"= 装进 manifest、被目录/审计
读到、但不改变波次）：

| 包 | strategy | 结论 |
|---|---|---|
| `opskeeper-datasource-{elasticsearch,opensearch,victoriametrics}` | rolling | **生效**：换数据源后端先 1/10 金丝雀再 10 台一波。与方案 §7"先 canary 再 rolling"的**意图**一致，但那条句子用的词表已不存在，实际语义就是 rolling（见 §3.2-4） |
| `opskeeper-adapter-{karmada,cmdb,alicloud}` | rolling | **生效**：只读工具面换版本按波铺开（karmada 草案注释自己论证过"canary wave 是真检查"） |
| `opskeeper-notifier-*` 全部六份草案（参考件 `builtin-channels` 覆盖 webhook/slack/feishu/dingtalk/wecom/telegram 六渠道，候选 `feishu-card`/`slack-blocks`/`dingtalk-markdown`/`webhook-strict`/`pagerduty`） | pin | **生效（单波）+ 治理声明**：通知外发没有"十分之一舰队先用新版"的语义，pin 让每次更换都是运维显式决定；这也是方案 §11.2 评审闸门的一行 |
| `opskeeper-provider-{bedrock,ollama}`、内置七 provider 参考件 | pin | **生效（单波）+ 治理声明**：模型调用直接产生费用，换实现必须是逐台显式动作；费用账本由宿主执行（`InMemoryBudget`），不依赖 strategy |
| `opskeeper-collector-{otelcol-kubeletstats}` | rolling | **生效**：pull 型采集器换版本按波铺 |
| `opskeeper-collector-{filelog}`、内置六采集器参考件 | pin | **生效（单波）+ 治理声明**：日志采集器换版=解析规则换版，历史数据字段形状跟着变，不该按节点波次铺（草案注释原话） |
| 随包 `opskeeper-sre-{observability,readonly,middleware}`（rolling）/ `sre-{repair,autonomy}`（pin）/ `datasource-victoriametrics`（rolling） | — | 与上表同口径；profile 的 `Strategy`（finance=pin / saas=rolling）是**部署档位的默认**，包级 manifest 与之共同决定最终波次 |

**结论**：词表里的两个值都被真实消费，没有"假字段"；方案正文里出现的 `canary` / `replace`
两个词是历史词表，已由固化测试钉住为解码期拒绝。若业务上确实需要"独立金丝雀阶段"或
"先装新再摘旧"，那是给 `InstallPolicy` 扩词表的实现切片，不是闸门问题（遗留项 L5）。

## 5. 回滚演练面核对：三段式文档逐方案审计

核对标准：每个候选包必须有**卸载前 / 卸载后 / 不变量**三段，且因果链精确到"哪一步谁构造、
漏掉哪一步会看到什么"。逐方案结论：

| 方案 | 三段齐备 | 因果链精确性 | 备注 |
|---|---|---|---|
| 4.3 datasource §3.7 | ✅ 显式三段（"回滚文档要点"四条即卸载后段） | ✅ 最强的一份：写明了"卸载 ≠ 一方 Prom 自动接回"，列了漏切配置时的**可见失败形态**（`Set{}.Prom()` 解析为 nil → `query_promql` 按既定降级语义消失） | 还自纠了原文的目标名笔误（`plugin-guard` → `pi-plugin-guard`） |
| 4.4 adapter §4.8 | ✅ 显式点名"回滚文档三段" | ✅ 写明了与 `pig-ops.yaml` 清单的**联动回滚**（不移除 `karmada.*` 会让 `TestTheMiddlewareProfiles…` 失败，且"这是刻意的"） | 不变量含可核对项：一方 8 源 56 工具不受影响、`Sources()` 顺序不变、kind 六元集合不变 |
| 4.5 notifier §6.5 | ✅ 三段以编号因果链呈现（1=卸载前，2–4=卸载与卸载后，5=验证） | ✅ 写明了漏做第 1 步的**定性**："投递失败是可见的，但 critical 告警无人收到——这是操作风险，不是数据风险" | 命名不是"卸载前/卸载后/不变量"字面三段，语义齐备 |
| 4.6 provider §7.5 | ✅ 三段以编号因果链呈现（1=先切默认，2–4=卸载，5=验证+不变量） | ✅ 写明了 `Resolve` 回 `ErrUnknownProvider` 的可见失败、`kind_counts.provider` 减一、账本曲线连续 | 变体命名（"先切默认/卸载/验证"）与其余方案不同但语义等价 |
| 4.7 collector §7/§8 | ✅ 三段以编号因果链呈现 | ✅ 含采集器特有的一步：cgroup 叶子组随子进程退出回收（S-4 清理路径），"无残留组"；并写明"全只读、回滚无外部副作用要清理" | 六个内置采集器今天**不经内核**注册（方案 C2 条目自己指出：无包名可依、无包级回滚路径），这是缺口已由方案 S-4/preinstalled 切片认领 |

**总体结论**：五份方案的三段式回滚面**齐备且互相一致**（同一写法、同一验证命令组合
`make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck`）。两处真实缺口都在
**代码侧不在文档侧**：内置采集器无包级回滚路径（C2，S-4 认领）、签名验证链依赖 ruby 构建的
发布包（L6）。4.10 台账引用本表即可。

## 6. 缺陷与遗留项（按归属切片）

| # | 事项 | 归属 |
|---|---|---|
| L1 | **`make vm-integration-check` 的 skip 分支是坏的**：无 docker 时打印"跳过"后仍继续执行 `go test -tags=integration ./tests/integration/`，随后因 docker 权限失败——"跳过"与"失败"同时成立，等于没有 skip。修法是让 skip 分支真的 `exit 0` | datasource-victoriametrics 实现切片 |
| L2 | VictoriaMetrics 真实 TSDB 集成路径本机无法验证（无 docker），由 CI 的 integration tag 承担；本任务只验证了本地可跑部分（`vmclient`/`vmquery` 单测全绿） | CI / datasource 切片 |
| L3 | datasource 方案 §7 灰度句用了旧词表（`canary`/`replace`），与 `InstallPolicy.Valid()` 不符（§3.2-4）。文档修一句即可，固化测试已钉住现状 | datasource 方案文档（下一版修订） |
| L4 | notifier 方案 §4.6 代码片段 `.WithKindCeiling(kernel.KindNotifier, domain.SafetyL3)` 在今天的内核上是**构造期 panic**（内核已把收窄/抬宽拆成 `WithKindCeiling`/`WithWiderKindCeiling`）；且 `NotifierPolicy()`、`ProviderPolicy()` 两个构造函数**尚不在仓内**——固化测试按方案原文把形状写在了测试里，S-1 落地真构造函数时应替换为对它的调用（两处都留了注释） | notifier/provider 实现切片（S-1） |
| L5 | 若业务确需独立金丝雀阶段或"先装新再摘旧"，需要给 `InstallPolicy` 扩词表并让 `PlanRollout` 认识它——这是一个有波次语义后果的决策，不是笔误 | 单独开 change 评审 |
| L6 | `make build-plugins` 与 `verify_release` 签名验证链依赖 ruby 构建的 teamharness 发布包；本机无 ruby（无 root）无法验证。CI 有 ruby，不构成闸门缺口，只构成"本地跑不全" | 环境限制（记录在案） |
| L7 | `go test ./plugins/...` 在本仓任何位置都 `matched no packages`（plugins 下是独立模块）。若有文档/README 以此为口径，应改为逐模块 `GOWORK=off` 写法 | 文档口径（本报告 §2 已改述） |
| L8 | Go 模块代理：aliyun 镜像的 PiG v0.4.0 与 go.sum 校验和不符，必须用 goproxy.cn（§1-1）。若 CI/开发者文档指定了镜像，需要修 | 基础设施 |
| L9 | `Host.Admit` 取 `WidestSafetyCeiling()` 是**刻意设计**（包级闸门无法知道条目落进哪个 kind），但五份方案里有三份把 per-kind 收窄当成包级现象描述（§3.2-1）。固化测试已把两层语义都断言；方案文档下一版可各补一句"包级放行 + 激活期拒绝" | 方案文档修订（不改代码） |

## 7. 结论

- 闸门命令全部实际运行，结果与输出摘要见 §2；两处环境限制（ruby、docker）与一处口径问题
  （`go test ./plugins/...`）如实记录。
- 准入闸门的一次性验证已固化为三份仓内测试（15 份草案、manager/edge 两侧、真实 policy 构造
  点），并顺带把四处方案↔代码漂移写成断言——漂移不是被"修掉"，是被**钉住**，方案与代码谁再
  动都会有一边变红。
- `install.strategy` 逐包结论见 §4：词表两值均被真实消费；方案里的 `canary`/`replace` 是历史
  词表。
- 回滚三段式文档逐方案齐备（§5），两处真实缺口都在代码侧且已被对应切片认领。
- 4.10 台账的"测试覆盖状态"列可直接引用 §3.3 的清单机制与 §3.1 的总表。

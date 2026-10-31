# 插件包统一治理台账（plugin-governance-ledger）

> 对应任务 4.10：为每个新增插件包维护 manifest、精确版本、scope、安全级别、兼容范围、测试、签名和回滚文档。
> 基线：`main`（PR #111 之后，`21efd90a`）。本文档是**纯方案/台账产出**：不实现任何包、不改 `core/floor/pluginmanifest` 代码、不做 4.12 告警方案。

---

## 1. 目标与边界

本台账是 `plugin-architecture-recovery-plan` 产出的**全部包级增量**的唯一权威登记处。此前每个方案（4.3–4.7）各带一份候选 manifest 与一份"通用闸门"章节（4.3 §7、4.4 §8、4.5 §11、4.6 §10、4.7 §9、4.9 §11），4.5 §9 与 4.9 §1.3 均预留"正式台账归 4.10"。本文档把散落的六份清单收口成一张表，并补上两件此前没有系统做过的事：逐项一致性核对（§4）与签名/分发双路径的可执行步骤（§5）。

**非目标**：不实现任何包；不改 `core/floor/pluginmanifest` 代码；不裁决各方案的开放决策点（D1-a 之类的决策仍归各自切片）；不做 4.12。

---

## 2. 台账规范（权威格式）

### 2.1 落位与文件

- **本文件**即台账：`openspec/changes/plugin-architecture-recovery-plan/plugin-governance-ledger.md`。change 归档时随 change 一起进入 `openspec/changes/archive/`，并在归档前把"待实施"行的状态列刷新为当时的真实状态。
- 台账**不另建机器可读副本**。理由：`core/floor/pluginmanifest/catalog.go` 已经是运行时目录（tenant/system/builtin/registry 四根）的机读索引，方案阶段的包在磁盘上没有工件，为它们再造一份 YAML 索引会产生两个会漂移的事实源——这正是台账要消灭的病。

### 2.2 行格式

每包一行，列定义固定（评审按列核对，缺列即打回）：

| 列 | 含义 | 取值来源 |
|---|---|---|
| 包名 | `metadata.name`，回滚与卸载以它为键 | manifest |
| 版本 | `metadata.version`，semver 锁死（`latest`/`main` 即拒） | manifest |
| 状态 | **已落地**（main 上有代码与 manifest）/ **待实施**（草案）/ **待实施·增量**（对已落地包的版本内增量） | 仓库事实 |
| targets / 级别 / 能力 | `spec.targets`、`spec.safety_level`、`spec.capabilities` | manifest |
| scopes | `spec.required_scopes`（空集写作 ∅，且空集是判定不是省略） | manifest |
| 工具 | `spec.tools` 数量与类别（无工具的包写"无（非工具面）"） | manifest |
| pig/edge 下限 | `install.min_pig_version` / `min_edge_version`；合成 manifest 不写（与二进制同构建，写一个不会漂移的数字是该字段的反面） | manifest |
| strategy | `install.strategy`（rolling / pin） | manifest |
| 准入 | 对照哪份宿主 Policy、当前结论（绿/琥珀/拒）与依据 | 代码实测或方案实测 |
| 测试 | 覆盖层与现状（4.11 的输入清单，见 §6） | 仓库事实 |
| 签名 | 签名状态（见 §5 的三档语义：已签 / 待签 / 不适用） | 仓库事实 |
| 指针 | manifest 路径 + 方案章节 + 回滚章节 | 相对链接 |

### 2.3 状态迁移规则

- 包 PR 合入 main → 行状态从**待实施**改**已落地**，同时刷新测试/签名列；
- 任何对已落地包 manifest 的字段变更（含版本号）必须**同 PR 改本台账行**——"改了包没改账"与"改了账没改包"都是漂移，§4 的核对就是抓这两件事；
- 草案被放弃（如 Git/ITSM 之于 4.4）→ 行整体删除，在所属方案文档的排除记录里留理由，不留在台账里当僵尸行。

---

## 3. 台账

基线参照（增量行的出发点，非本计划新增）：五个一方 SRE 包随包发布于 `plugins/pig-ops/<pkg>/pig-ops.yaml`，当前均为 0.2.0——`opskeeper-sre-{middleware,observability,readonly,repair}` v0.2.0 由 finance/SaaS 两个部署 profile 组合（`core/floor/pluginmanifest/profiles.go` `Composes`），`opskeeper-sre-autonomy` v0.2.0 仅 SaaS 组合（finance 的 L2 上限拒它，这是 profile 的既定语义）。

### 3.1 已落地

| 包名 | 版本 | targets | 级别/能力 | scopes | 工具 | pig/edge | strategy | 准入 | 测试 | 签名 | 指针 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `opskeeper-datasource-victoriametrics` | 0.1.0 | manager | L1 / read | observability.read | 无（数据源面，贡献信号 `prom`） | 0.4.0 / 0.8.0 | rolling | 绿：`datasource.ManagerPolicy()`（L2）下 Satisfies ✓ | unit+contract+integration 三层已交付（PR #107） | **待签**：分发通道未接线（见 §5.5） | manifest `plugins/opskeeper-datasource-victoriametrics/pig-ops.yaml`；方案 4.3 §3；回滚 4.3 §3.7 + `datasource-victoriametrics-slice/design.md` Rollback 节 |

### 3.2 待实施——第三方候选草案

**datasource（4.3，PR #102）**。同组形状：manager / L1 / read / rolling / 0.4.0+0.8.0 / 无工具面（复用一方信号，激活期单激活，`ErrDuplicate` fail-closed）。

| 包名 | 版本 | scopes | 准入 | 测试 | 签名 | 指针 |
|---|---|---|---|---|---|---|
| `opskeeper-datasource-elasticsearch` | 0.1.0 | observability.read（信号 `loki`） | 绿（同 VM：L2 ceiling + 已授予） | 未实施（三层待建：unit+integration[compose 真集群]+contract） | 待签 | `datasource-manifests/elasticsearch.yaml`；4.3 §4；回滚 §4.7 |
| `opskeeper-datasource-opensearch` | 0.1.0 | observability.read（信号 `loki`） | 绿（同上） | 未实施（同上） | 待签 | `datasource-manifests/opensearch.yaml`；4.3 §5；回滚 §5.6 |

**adapter（4.4，PR #103）**。同组形状：manager / L1 / read / rolling / 0.4.0+0.8.0 / approval.required=false / 无 radius（`sdk.Validate` 拒只读级别携带半径）。

| 包名 | 版本 | scopes | 工具 | 准入 | 测试 | 签名 | 指针 |
|---|---|---|---|---|---|---|---|
| `opskeeper-adapter-karmada` | 0.1.0 | k8s.read | 5 × read | 绿：`adapterPolicy()` 八项原授予集内 | 未实施（四层待建：unit+integration[envtest/mock]+contract+failure） | 待签 | `adapter-manifests/karmada.yaml`；4.4 §4；回滚 §4.8 |
| `opskeeper-adapter-alicloud` | 0.1.0 | cloud.read | 4 × read | **绿（S0 后）**：D2 已落，`defaultAdapterScopes()` 含 cloud.read（§4 漂移 D-1，本 PR 已修正 manifest 注释） | 未实施（四层待建，同上） | 待签 | `adapter-manifests/alicloud.yaml`；4.4 §5；回滚 §5.8 |
| `opskeeper-adapter-cmdb` | 0.1.0 | topology.read | 4 × read | **绿（S0 后）**：D1 已落，`defaultAdapterScopes()` 含 topology.read（§4 漂移 D-1） | 未实施（四层待建，同上） | 待签 | `adapter-manifests/cmdb.yaml`；4.4 §6；回滚 §6.8 |

**notifier（4.5，PR #104）**。同组形状：manager / L3 / destructive / [alert.read, alert.write] / pin / 0.4.0+0.8.0 / 无工具面（贡献渠道工厂，注册同一渠道类型即与内置版互斥）。

| 包名 | 版本 | radius | 准入 | 测试 | 签名 | 指针 |
|---|---|---|---|---|---|---|
| `opskeeper-notifier-pagerduty` | 0.1.0 | single-ns | **拒（现状）**：manager 现行 L2 ceiling 拒 L3；内核 per-kind ceiling 已随 S-1 切片落地（`kernel.Policy.KindCeilings`），notifier 宿主接线在 B1–B3 迁移片 | 未实施（三层待建：unit+integration[CI 假端点真 HTTP 契约]+contract） | 待签 | `notifier-manifests/pagerduty.yaml`；4.5 §6；回滚 §6.5 |
| `opskeeper-notifier-feishu-card` | 0.1.0 | —（一次触达一个群） | 同上（L2 拒，实测记录 4.5 §11.1） | 未实施（同上） | 待签 | `notifier-manifests/feishu-card.yaml`；4.5 §7；回滚 §7 尾注 |
| `opskeeper-notifier-slack-blocks` | 0.1.0 | — | 同上 | 未实施（同上） | 待签 | `notifier-manifests/slack-blocks.yaml`；4.5 §8；回滚 §8 尾注 |
| `opskeeper-notifier-dingtalk-markdown` | 0.1.0 | — | 同上 | 未实施（同上） | 待签 | `notifier-manifests/dingtalk-markdown.yaml`；4.5 §9；回滚 §9 尾注（回滚=调回静默失败率，须写进变更单） |
| `opskeeper-notifier-webhook-strict` | 0.1.0 | —（端点即运维选择） | 同上 | 未实施（同上） | 待签 | `notifier-manifests/webhook-strict.yaml`；4.5 §10；回滚 §10 尾注 |

**provider（4.6，PR #108）**。同组形状：manager / L2 / write / ∅（空集是判定）/ pin / 无工具面（贡献 provider 工厂）。

| 包名 | 版本 | scopes | 准入 | 测试 | 签名 | 指针 |
|---|---|---|---|---|---|---|
| `opskeeper-provider-ollama` | 0.1.0 | ∅ | 绿：`ManagerPolicy()`（L2）实测过（4.6 §10.1） | 未实施（三层待建：unit+integration[假端点]+gate） | 待签 | `provider-manifests/ollama.yaml`；4.6 §7；回滚 §7.5 |
| `opskeeper-provider-bedrock` | 0.1.0 | ∅ | 绿（同上） | 未实施（同上，integration 需含 SigV4 签名契约） | 待签 | `provider-manifests/bedrock.yaml`；4.6 §8；回滚 §8.5 |

**collector（4.7，PR #109）**。同组形状：edge / L1 / read / approval.required=false / 无 radius。

| 包名 | 版本 | scopes | strategy | 准入 | 测试 | 签名 | 指针 |
|---|---|---|---|---|---|---|---|
| `opskeeper-collector-otelcol-kubeletstats` | 0.1.0 | k8s.read, observability.read | rolling | 绿：finance（L2/pod）与 saas（L3/ns）两 profile 实测均过（4.7 §9.1） | 未实施（五层待建：unit+integration+contract+failure+resource[内存超限]） | 待签 | `collector-manifests/otelcol-kubeletstats.yaml`；4.7 §7；回滚 §7.2 |
| `opskeeper-collector-filelog` | 0.1.0 | host.read, observability.read | pin | **琥珀**：闸门 C4——manager 的 endpoint 解析只认 `logs`/`traces`，D1-a（三层 endpoint 解析）裁决前装得上、跑不起来 | 未实施（五层待建，同上） | 待签 | `collector-manifests/filelog.yaml`；4.7 §8；回滚 §8.3 |

**alert（4.12，PR 代建）**。新包 / edge / L2 / read+write / pin / alert.read+alert.write / 2 工具（`alert.rule_config`、`alert.rule_apply`，从 repair 硬切 `draft_config_change`/`apply_config_change`）。

| 包名 | 版本 | scopes | 准入 | 测试 | 签名 | 指针 |
|---|---|---|---|---|---|---|
| `opskeeper-sre-alert` | 0.1.0 | alert.read, alert.write | **静态核对，未实测**（起草环境无 Go 工具链；sdk 三段闸门实测固化为 S-1 首条验收，并入 4.11 闸门 harness） | 未实施（S-1~S-4 待建） | 待签 | `alert-manifests/alert-rule-config.yaml`；4.12；command 草案 `alert-manifests/command-alert-rule.md`；回滚见方案 §10 |

### 3.3 待实施——一方预装参考形态（编译内合成 manifest）

这些**不是可分发的插件包**：编译在二进制里，磁盘上没有 pig-ops.yaml；manifest 由装配层的 `manifestFor()` 合成（datasource 先例：`core/manager/biz/aiops/datasource/datasource.go`），与第三方包走同一道 `Admit` 闸门。参考 YAML 放在候选包旁边仅供评审同 diff 对照。签名状态一律**不适用**（无磁盘树可签，出处由二进制发布签名与版本绑定承担，见 §5.3）；下限一律不写（与二进制同构建）。

| 包（各一份 manifest） | 版本 | targets | 级别/能力 | scopes | 参考文件 / 方案 / 回滚 |
|---|---|---|---|---|---|
| `opskeeper-notifier-{webhook,slack,feishu,dingtalk,wecom,telegram}` ×6 | 1.0.0 | manager | L3 / destructive | alert.read, alert.write | `notifier-manifests/builtin-channels.yaml`；4.5 §5；回滚随 B1–B3 各片（§5.1） |
| `opskeeper-provider-{anthropic,custom,deepseek,gemini,kimi,openai,zhipu}` ×7 | 1.0.0 | manager | L2 / write | ∅ | `provider-manifests/builtin-providers.yaml`；4.6 §5；回滚随 §5 迁移片 |
| `opskeeper-collector-hostmetrics` / `-procmetrics` / `-logs`（host.read+observability.read）、`-traces` / `-custommetrics`（observability.read）、`-databasemetrics`（db.read+host.read+observability.read）×6 | 1.0.0 | edge | L1 / read | 见左三组 | `collector-manifests/builtin-collectors.yaml`；4.7 §5；回滚随 §5.3 预装装配片 |

### 3.4 待实施·增量——一方随包版本内增量

| 包 | 版本 | 增量 | 状态 | 测试 | 签名 | 指针 |
|---|---|---|---|---|---|---|
| `opskeeper-sre-readonly` | 0.2.0 → **0.3.0** | tools += `query_devices` / `query_incidents` / `get_incident_detail`（3 × read，20→23）；scopes 不变 | 待实施（切片 S-2；D-3 裁决归 readonly） | 未实施（§8.2 三向一致 + 上呼 integration + E2E） | 不适用（随包发布） | 方案 4.8 §5.2/§5.4；回滚 §8.3（发布 0.2.0 回退版，fail-closed 无需清理） |
| `opskeeper-sre-observability` | 0.2.0 → **0.3.0** | tools += `query_knowledge` / `get_host_processes`（12→14）；scopes += `knowledge.read`（**domain 常量与两 profile 授予均未落**，现状 `core/domain/plugin.go` 无此常量） | 待实施（切片 S-3，依赖 S-1 桥） | 未实施（同上 + profile composition 测试） | 不适用 | 方案 4.8 §5.1/§5.3；回滚 §8.3（scope 成无人消费者无害） |
| `opskeeper-sre-repair` | 0.2.0 → **0.3.0**（沿 4.8 §5.0"菜单新增即 schema 增量"规则推定，切片落地时确认——§4 漂移 D-3） | tools += `preview.read`（read，5→6）；`verify_recovery` schema 绑定；scopes 不变（4.9 §9.3 论证不加 scope） | 待实施（切片 S-2，D3-a 已裁决归 repair） | 未实施（4.9 §11：拒绝路径测试 + drift 测试 + repairpath 断言） | 不适用 | 方案 4.9 §5.2/§9.1；回滚 §10 S-2（manifest 工具清单回退即不再暴露） |

### 3.5 非包级增量索引（不设台账行，防"为什么没有它"）

| 增量 | 为何不是台账行 | 指针 |
|---|---|---|
| adapter 宿主切片 S0（PR #105） | 宿主侧改动：G1 贡献可见性修复 + `AdapterPolicy()` 导出 + D1/D2 授予落地；参考实现是测试文件内的"参考实现，非产品包"，不注册工具、不改 pig-ops.yaml | `adapter-host-slice-s0/`；影响已计入 §3.2 两行的准入列 |
| notifier 内核切片 S-1/S-4（PR #106） | 内核（per-kind ceiling）与共享 sender（errcode 判定钩子）改动，无包工件；它改变的是 §3.2 notifier 五行的准入前提 | `notifier-kernel-first-slice/` |
| 4.8 切片 S-1（G0 对账 + BaseTool 桥） | manager 上呼通道改动，非包增量；它是 §3.4 observability 行的依赖 | 4.8 §5.5 / §10 |
| 4.9 切片 S-1/S-3~S-6 | 预览域证据对象、审批链、回滚对象、判定规则——均宿主/预览域侧，非包增量（包面只有 §3.4 repair 行） | 4.9 §10 |

**覆盖性声明**：本计划产出的包级增量 = §3.1（1 个已落地）+ §3.2（14 个第三方草案）+ §3.3（19 个一方合成参考形态）+ §3.4（3 个版本内增量）= **38 行**（2026-10-31 4.12 落台账后 +1），与 4.3–4.9 七份方案的"产出"清单逐一对应；方案文档中被排除的候选（4.4 的 Git/ITSM、4.7 的 metrics 采集器与 changewatcher、4.8 §7 不补清单）不在台账内，理由留驻原方案。

---

## 4. 一致性核对

### 4.1 核对方法

1. **机械抽取**：对 19 份草案 YAML + 6 份随包 pig-ops.yaml（5 个 SRE 包 + VM 包）逐份解析 `metadata`/`spec` 全字段，与各方案文档的候选表逐字段比对（版本、targets、级别、能力、scopes、工具数、strategy、下限、radius）。
2. **代码对照**：把每份草案的 `required_scopes`/`safety_level` 对照**当前 main 的宿主 Policy 构造点**（`datasource.ManagerPolicy()`、`toolset.adapterPolicy()`/`defaultAdapterScopes()`、`pluginmanifest` 两 profile 的 `Policy()`），不是对照方案写作当时的快照——漂移只能以当前代码为准。
3. **基线对照**：`min_pig_version` 对照 `core/pig/go.mod` 的 PiG 依赖（v0.4.0 ✓）；`min_edge_version` 对照方案声称的 Edge 基线 0.8.0（锚点 `opskeeper-sre-middleware` 的 manifest）。
4. **双副本对照**：VM 包的落地 manifest 与 openspec 草案 manifest 逐字节 diff。

### 4.2 对齐结论

- **全部 37 行的字段值与所属方案文档的候选表逐字段一致**：版本全部 semver 锁（第三方 0.1.0 / 合成 1.0.0 / 随包 0.2.0）；datasource 全 L1+read+单一 scope；notifier 全 L3+destructive+[alert.read,alert.write]+pin；provider 全 L2+write+∅+pin；collector 全 L1+read；adapter 全 L1+read+无 radius。各方案"通用闸门"要求的字段（apiVersion/kind/metadata/spec 全项）在 25 份 YAML 里无一缺失。
- **VM 双副本逐字节一致**（`plugins/opskeeper-datasource-victoriametrics/pig-ops.yaml` ≡ `datasource-manifests/victoriametrics.yaml`）。
- **PiG 下限全仓一致**：17 份声明下限的 manifest 全部 0.4.0，与 `core/pig/go.mod` 的 `github.com/MichaelKinsy/PiG v0.4.0` 相符。
- **合成 manifest 不写下限**的纪律在 notifier/provider/collector 三份参考文件里一致执行，且各自写明了理由。
- **profile 组合关系与五个 SRE 包一致**（finance 4 包、saas 5 包含 autonomy），composition 测试守着。

### 4.3 漂移清单（发现即处置）

| # | 漂移 | 事实 | 处置 |
|---|---|---|---|
| **D-1** | `adapter-manifests/alicloud.yaml`、`cmdb.yaml` 的 STATUS/验证注释**落后于代码** | 两份注释写"今日不通过准入（缺 cloud.read/topology.read 授予），Admit 拒"，这是 PR #103 时点的真话；S0（PR #105，更晚合入）落地 D1/D2 后 `defaultAdapterScopes()` 已含两项，两份草案在当前 `adapterPolicy()` 下**均通过**。方案 4.4 的裁决记录（tasks.md 4.4 行）也已写明"D1/D2 均同意" | **本 PR 已修正**：改写两份注释为"admission-ready（S0 后）"，保留原论证（host.read 是错误捷径、topology.read 双源 caveat），授予集清单同步为当前十项。台账行准入列按修正后状态记录 |
| **D-2** | `opskeeper-sre-readonly` 的 `min_edge_version: 0.7.0` 与全仓基线 0.8.0 不一致 | 它是唯一一份 0.7.0 的随包 manifest（历史原因：该包 2026-09-30 先于 0.8.0 基线创建，后续四个包均写 0.8.0）；4.4/4.7 方案明文"与主仓 Edge 基线 0.8.0 对齐" | **台账标注，不在本 PR 修正**：抬一份已发布包的兼容下限会改变旧节点上的准入结果（0.7.x 节点从可装变拒装），是行为变更不是文案修正，应随 §3.4 该包 0.3.0（S-2）版本 bump 一起裁决对齐 |
| **D-3** | 4.9 的 repair 增量未写明显式版本号 | 4.9 §10 S-2 只写"工具清单 5→6"，未像 4.8 那样写"0.2.0 → 0.3.0" | **台账标注**：§3.4 按 4.8 §5.0 的"菜单新增即 schema 增量"规则推定 0.3.0，切片落地 PR 里确认并把结论回写 4.9 文档 |
| **D-4** | VM 包不在任何 profile 的 `Composes` 内 | `Composes` 语义是"该 fleet 安装的目录包集合"，守护测试只覆盖随包目录（`plugins/pig-ops`）；VM 在 `plugins/opskeeper-datasource-victoriametrics`，装配走 env 开关不是目录装载 | **维持已记录状态**：`datasource-victoriametrics-slice/design.md` "Recorded for later decisions" 第 3 条已把它记为 dcell 名册的开放项，归属插件生态任务；不构成新漂移 |

### 4.4 复核方式

本核对为一次性人工+脚本核对（字段抽取用 YAML 解析、Policy 对照读当前源码），未在仓内留下测试代码。固化建议（归 4.11/切片，不在本任务落）：4.5 §11.1 与 4.7 §9.1 已建议把"目录草案批量过 Decode→Validate→Admit"固化为 `*_manifests_test.go`；该测试落地后，本台账 §4 的人工核对即退化为该测试的红灯输出。

---

## 5. 签名与分发路径核对（不改代码）

`core/floor/pluginmanifest/signing.go` + `trust.go` + `review.go` 构成签名通道。评审管线固定五步且**顺序承重**：`signature → manifest → admission → version → agent_version`，任一步失败不再走到后面（`review.go` 的 `Step` 常量即日志里的失败定位）。下面把两种形态各写成可执行步骤。

### 5.1 通道组件（两形态共用）

- **算法与文件**：仅 ed25519（`SignAlgorithm`）；旁挂签名文件固定名 `pig-ops.sig`（`SignatureFile`），是旁挂而非 manifest 内字段——签名覆盖 manifest，装进 manifest 就是自己签自己。
- **Envelope 承重字段**：`key_id`（运维命名，轮换不改身份）/`algorithm`/`name`/`version`/`tree_digest`（hex sha256，**排除** `pig-ops.sig` 自身后算的整树摘要）/`signature`/`signed_at`（仅供参考，不进签名负载——时钟不是安全属性）。`name+version+tree` 三者都被签，信封不能从一个包挪到另一个包。
- **Verify 顺序**（`trust.go`）：拒未知算法 → 解析 key（未知/已吊销是两种不同答案）→ **按磁盘现状重算树摘要**并与信封比对（签后再改任何文件即失败）→ 验签 → 信封身份与 manifest 复核。Verify 全程不解析 manifest——那是 Review 下一步的事。
- **TrustStore**：空店信任零；同 id 换 key 拒（防把泄露洗成轮换）；吊销留名不留键。配置文件形态 `keys: [{id, key(base64 ed25519 公钥)}]`，空文件拒收。
- **`AllowUnsigned`**：零值即 false——"每个包必须已签名"是默认；`Profile.Policy()` 显式置 false，生产 fleet 无法从 profile 层打开这个口子，它只属于开发节点的本地开关。

### 5.2 第三方包（外部目录分发）：从签名到装载

**发布侧（持有私钥的一方，密钥在运维自己的 vault）**：

1. 构建包树 `root/`（`pig-ops.yaml` + Go 源 + 测试），确认 `metadata.name/version` 就是台账行的包名与精确版本（签名会读它们，写错即废）。
2. `NewSigner(keyID, 私钥)` 构造签名器（`GenerateSigner` 仅用于测试/工具；生产密钥不落仓）。`Signer.Sign(root)`：对**不含** `pig-ops.sig` 的树算摘要、产出 Envelope。
3. `Envelope.WriteTo(root)` 落盘旁挂文件（先签后写；旁挂文件本身在摘要里被排除，包跨机器搬运不需要重签）。
4. 打 tar.gz 分发，**随附归档级 SHA256**（传输摘要与树内签名是两道独立的门：节点先验归档摘要再解包，解包后的树再过签名）。台账该行签名列此刻才可写"已签（key_id=…）"。

**节点侧（edge 插件装载路径，`cmd/opskeeper-edge/plugininstall.go`）**：

5. 下载归档 → **先验传输 SHA256**，不匹配即 refuse（不是 retry——字节错了重试还是错）。
6. 解包进 `.staging-*` 暂存目录（不在包目录里，任何失败即整目录删除；暂存包绝不进激活集）。
7. `pluginmanifest.Review(root, trust, policy)` 五步闸门；`trust` 来自 `TrustStoreFromConfig(配置文件)`，`policy` 由节点 profile 渲染（`AllowUnsigned=false` 强制）。
8. manifest 与请求对账：`metadata.name`/`metadata.version`/`targets` 必须与下发请求一致——**版本贴错标签的包无法回滚到**，这是最便宜的拦截点。
9. 全过后目录改名晋级为正式包目录、激活；`Restore`（按包名+版本回滚重激活）**同样重跑 Review**——树可能被改过，节点 policy 可能收紧过，回滚不是评审豁免权。开机路径对已装包亦复跑 Review（`plugininstall.go` boot 扫描）。

**联邦通道**（根-子集群分发）：根侧经 `federationReleaseSigner()`（env 提供的 base64 ed25519 私钥 + key_id）签名；接收端 `VerifyDir` → `Review` 通过才晋级树（`core/federation/receiver.go`），同样五步。

### 5.3 一方预装（编译内合成 manifest）：为什么不"签名"、凭什么可审计

一方包**没有磁盘树**：编译在二进制里，`manifestFor()` 在启动时合成 manifest（VM 切片的 datasource 先例、notifier/provider/collector 三份参考 YAML 的合成形态），直接 `host.Activate` 过 **`Admit` 同一道准入闸门**（targets/scope/级别/radius 四查，fail-closed）——准入不豁免，豁免的只有五步里的第 1 步（签名）与第 4/5 步（版本下限）：

- **签名不适用是结构性的**：没有 `root/` 可摘要，没有可旁挂的目录。给合成 manifest 造"签名"等于给二进制里的一个函数返回值签名——它的完整性证明就是**二进制本身的发布签名**：发布工件的签名/摘要、`RELEASE_VERSION.json` 的版本绑定、`make module-check`（唯一 PiG 导入点）与 `audit-open-source`。台账签名列因此有第三档"**不适用（随二进制）**"，与"待签"严格区分——把一方包写成"未签名"是在指控一个不存在的缺失。
- **下限不写是语义正确**：包与二进制同一次构建，`min_pig/edge_version` 是一个不会漂移的数，写下来恰好是该字段要防的病的反面（三方参考文件末尾均注明）。第三方包必须写：那里包与二进制是两份工件。
- **可执行步骤**（运维核对一方包"签名等价物"）：核对发布工件签名与 `RELEASE_VERSION.json` → `make module-check && make pi-plugin-guard` → `go test ./scripts/ledgercheck` → 台账行（§3.3/§3.4）字段与合成函数/参考 YAML 对照。

### 5.4 台账签名列的三档语义

| 档 | 含义 | 适用 |
|---|---|---|
| 已签（key_id=…） | 包树已过 `Sign`+`WriteTo`，旁挂文件随包分发 | 尚无（见 5.5） |
| 待签 | 包有/将有工件，发布时尚未执行签名步骤 | §3.1 VM、§3.2 全部草案 |
| 不适用（随二进制） | 编译内合成，无磁盘树；出处由发布签名+版本绑定承担 | §3.3 全部、§3.4 全部 |

### 5.5 现状缺口（记录，不在本任务修）

1. **无私钥签名 CLI**：`Signer` 只有 Go API（联邦接线与测试在用）；发布侧需要一个"对目录跑 Sign+WriteTo 并打印摘要"的工具，建议随 4.11 闸门任务落。
2. **manager 侧外部目录装载无部署接线**：`OPSKEEPER_MANAGER_PLUGINS_DIR` 存在但无部署/发布配置（VM 切片 design "Recorded for later decisions" 第 1 条），manager 端第三方包目前只能像 VM 那样编译内激活——所以 §3.1 的 VM 行是"待签"而非"不适用"：它是真正的第三方包形态，签名义务在，通道未通。
3. **marketplace 卸载通道**超出 VM 切片范围，回滚文档（4.3 §3.7）的四步因果链以它为前提。

---

## 6. 与 4.11 的衔接

4.11（"运行插件构建、测试、准入、签名、灰度和回滚闸门"）的**输入清单就是本台账**：

- **测试列** = 4.11 测试闸门的运行范围：§3.1 一行已有三层实测；§3.2 十四行按扩展点要求的三/四/五层（datasource 三层、adapter/notifier/provider 三层[provider 为 unit+integration+gate]、collector 四层+resource 层）在建包 change 里交付；§3.4 三行按各自方案的 §8/§11 测试表。4.11 逐行核对"层齐没有"，不齐即红。
- **准入列** = 4.11 准入闸门的预期清单：绿行的 manifest 必须在对应 Policy 下复跑仍绿（S0 这类宿主政策变更会翻转既有结论——正是 D-1 的教训：**准入结论必须带"对照哪份 Policy、哪个时点"**，本台账每行都写了）；琥珀/拒行（notifier 五行、filelog）在 4.11 前必须先落各自的解锁切片（B1–B3、D1-a）。
- **签名列** = 4.11 签名闸门的范围：待签行在分发前必须走 §5.2 第 1–4 步；"不适用"行不进签名闸门、进发布工件核对。
- **回滚指针** = 4.11 回滚演练的脚本来源：每行指针指向的回滚章节都已是"卸载前/卸载后/不变量"或等价三段式因果链。
- 闸门命令沿用各方案通用闸门：`make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck`。

## 7. 验收对照

- [x] 台账覆盖本计划全部包级增量，无遗漏项——§3 共 37 行 + §3.5 非包级索引 4 项（说明为何不设行）+ 覆盖性声明
- [x] 每包一行式记录 + 指针——§3.1–§3.4 每行含 manifest 路径、方案章节、回滚章节
- [x] 一致性核对结论——§4.2 对齐清单 5 项 / §4.3 漂移清单 4 项（D-1 已修正，D-2/D-3 标注，D-4 维持已记录状态）
- [x] 签名与分发路径可执行步骤（不改代码）——§5.2 第三方九步 / §5.3 一方三步 + 缺口记录 §5.5
- [x] 与 4.11 衔接——§6，测试列即闸门输入清单

## 8. 提交与执行规范

- 提交语言：本任务使用中文注释 / 中文提交说明（沿 4.3–4.9 先例；仓库根 `CLAUDE.md` 的英文默认在历次方案类提交中按此例外处理）。
- 关联：OpenSpec change `plugin-architecture-recovery-plan` 任务 4.10。**内部任务编号不写入任何受开源审计的跟踪文件**（`scripts/audit_open_source.py` 的正则会拒收），只记录在 PR 描述与评审评论中。
- 落盘路径：本文档落盘 `openspec/changes/plugin-architecture-recovery-plan/plugin-governance-ledger.md`；随本任务修正 `adapter-manifests/{alicloud,cmdb}.yaml` 的过时状态注释（漂移 D-1）。
- 后续各包实现/增量切片**不**在本任务内合入；落地请用户单独开 change，行状态按 §2.3 迁移规则回写本台账。

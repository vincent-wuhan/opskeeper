# P0 基线冻结报告（OpenSpec 任务 1.4 – 1.7）

本文件是 `plugin-architecture-recovery-plan` 第 1 节的交付物，记录 P0 阶段的一致性核对、版本与发布元数据漂移清单、闸门复跑的实测输出，以及基线冻结口径与阶段闸门结构。

所有结论均以 §1.1 的基线 commit 为准，取数命令随文给出，可复算。

> 边界声明：本文件只做架构判断与记录，不改动运行时代码。凡涉及代码或文档正文修改的条目，一律以「处置建议」形式列出并标注是否阻塞，由对应实施任务落地。

## 1. 冻结坐标与口径

### 1.1 基线坐标

| 项 | 值 | 取数命令 |
| --- | --- | --- |
| 基线 commit | `ff1b7e40745b261ac2e4cce11607aa90922dc4af` | `git rev-parse HEAD` |
| 基线标题 | `feat(notifier): migrate the six builtin channels through the kernel（B1+B2） (#118)` | `git log -1 --format=%s` |
| 基线日期 | 2026-11-04 | `git log -1 --format=%ad --date=short` |
| 远端 | `git@github.com:vincent-wuhan/opskeeper.git` | `git remote -v` |
| 工作分支 | `codex/p0-baseline-freeze` | `git branch --show-current` |
| 历史完整性 | 完整历史（非浅克隆），提交计数与祖先关系可回溯 | `git fetch --unshallow origin main` |

### 1.2 取数口径（四条，避免同一个数字出现两种意思）

1. **版本号一律读文件**：`VERSION`、`RELEASE_VERSION.json`、`plugin.yaml`、`package.json`。不从 git tag 反推期望值——远端 7 个历史 tag 全部不满足当前 release workflow 的 tag 语法（§3.1 V-10），用 tag 反推会把历史词表误当成期望。
2. **工具数一律指 manifest `tools[].name` 去重计数**：不把 `install.strategy`、`scopes` 之类字段算进工具数；也不把合成 manifest（`plugins/pig-ops/opskeeper-sre-readonly/pig-opskeeper-ops.yaml`）算进包数——它不是插件 manifest。
3. **闸门结论分两类**：任务指定的「三道必跑闸门」（§4.1）与「附加复跑」（§4.2）。后者不构成发布结论，但用来判断绿灯本身是否可信。
4. **浅克隆上的结论一律作废**：§4.4 记录了一次浅克隆造成的假阳性。

### 1.3 本次冻结的边界

- 冻结对象是「基线 commit + 清单 + 闸门输出」三件套，不是发布物。发布物的定义在 §3.2（命名建议）与 §5.4（阶段闸门）。
- 本次不修改生产配置、不轮换凭据、不打 tag、不推 release 分支、不合并任何实现类 change。

## 2. 任务 1.4：TeamHarness、五个 SRE 插件包与 94 个工具的一致性核对

### 2.1 结论先行

manifest 侧完全自洽：TeamHarness `1.0.75` 在 7 处载体上一致；5 个 SRE 包版本一致为 `0.2.0`；`tools[].name` 去重后正好 94 个，且真二进制闸门逐条对得上。

**不一致全部在描述侧，而且现有闸门照不到**：

- **D-1**：Makefile 与架构文档仍写「5 个包 / 90 个工具」，与实测 94 差 4。
- **D-2**：`opskeeper-sre-readonly` 的 `min_edge_version: 0.7.0` 是全仓唯一的一份 0.7.0，其余四包为 0.8.0。该漂移在 4.10 台账 §4.3 已登记为 D-2，本次核对确认它在当前基线上仍然存在。

换句话说：闸门全绿不等于文档正确。台账与描述侧的一致性需要单独核对，这正是 1.4 存在的理由。

### 2.2 逐项核对表

| 核对项 | 声称 | 实测 | 结论 |
| --- | --- | --- | --- |
| TeamHarness 版本 | `1.0.75` | `plugin.yaml`、`dashboard/package.json`、`dashboard/plugin.json`、`dashboard/public/plugin.json`、`dashboard/package-lock.json`、`adapters/qwenpaw/plugin.json`、`adapters/qwenpaw/plugin.py` 七处一致为 `1.0.75`；`CHANGELOG.md` 顶部条目 `## 1.0.75 — 2026-10-07` | 一致 |
| SRE 包数量 | 5 | `plugins/pig-ops/opskeeper-sre-{middleware,observability,readonly,repair,autonomy}` | 一致 |
| SRE 包版本 | — | 5 份 `pig-ops.yaml` 与 5 份 `package.json` 均 `0.2.0` | 一致 |
| manifest 工具数 | 94 | middleware 56 + observability 12 + readonly 20 + repair 5 + autonomy 1 = 94；去重后仍 94（无重名） | 一致 |
| 真二进制核对 | 94 | `make pig-tool-scoping-check` 通过，`core/pig/pigprofile` 对真 `pig` 二进制实测 5 包 / 94 声明工具逐条对上 | 一致 |
| PiG 依赖 | — | 5 包均 `min_pig_version: 0.4.0`；`core/pig/go.mod` 锁 `github.com/MichaelKinsy/PiG v0.4.0` | 一致 |
| profile 组成 | — | finance 组成 4 包（不含 autonomy，`NotGranted: k8s.exec, db.write, mq.write`）；saas 组成 5 包（含 autonomy） | 一致 |
| `install.strategy` | 两值词表 | `pin` / `rolling`，两值均被 `PlanRollout` 真实消费；autonomy 与 repair 为 `pin`，其余三包为 `rolling` | 一致 |
| 文档与 Makefile 的工具数 | 90 | 实测 94 | **D-1，不一致** |
| edge 兼容下限 | 0.8.0 | readonly 为 `0.7.0`，其余为 `0.8.0` | **D-2，不一致** |

### 2.3 D-1：描述侧仍写「90 个工具」

证据位置：

- `Makefile:1075`：「实测 5 个包 / 90 个声明工具全部被提供」
- `Makefile:1082`：「真二进制验证节点 Agent 被提供了插件工具（CI 决策闸门，实测 5 包 / 90 工具）」
- `docs/opskeeper2-architecture.md` 多处沿用「5 个包 / 90 个工具」；同文件另有一处写的是「20 / 12 / 56 / 5 / 1」，即 94
- 同文件一处还留着更早的旧分解「18 + 12 + 53 + 5」

原因：readonly 由 18 增至 20、middleware 由 53 增至 56，合计 +5（18+12+53+5=88 → 20+12+56+5+1=94）。manifest 随能力增长更新了，文档与 Makefile 注释没有跟着更新。

影响：

- 按文档核对 manifest 会得出「少了 4 个工具」的错误结论，而实际一个不少。
- `make pig-tool-scoping-check` 的说明文字与它自己的实测结果互相矛盾：闸门名下的注释写 90，闸门跑出来是 94。
- 这类漂移的真正代价不是数字错，而是它让「用文档核对仓库」这个习惯失效——第一次核对就会得出错误结论，人对文档的整体信任随之下降。

处置建议（不阻塞 P0 基线）：

1. 把 `Makefile:1075` 与 `Makefile:1082` 的「90」改为「94」，并写明新分解 `20 / 12 / 56 / 5 / 1`；或者更稳妥地改成「以 `make pig-tool-scoping-check` 的实测输出为准」，不在注释里硬编码计数。
2. `docs/opskeeper2-architecture.md` 中所有「90 个工具」的表述按同一口径改写；旧分解「18 + 12 + 53 + 5」一处单独处理。
3. 更长远的做法：在文档里引用工具总数时改为「基线 commit 实测 N」，让数字自带时点，避免下一次能力增长时重复腐烂。

落点建议：随发布准备阶段的文档同步任务（6.7）一起改，或作为独立小 PR 先改——两种都可以，取决于是否要在发布前先让闸门与文档一致（见 §7 第 4 条）。

### 2.4 D-2：readonly 的 `min_edge_version` 仍是 0.7.0

证据：`plugins/pig-ops/opskeeper-sre-readonly/pig-ops.yaml` 写 `min_edge_version: 0.7.0`，其余四包写 `0.8.0`。

含义：该包声明自己可以装到 edge 0.7.0 上。当前 edge 版本线是 0.8.0，0.7.x 已不在受支持范围内；而该包的 20 个工具中有相当一部分依赖 0.8 才有的接口。也就是说，这是一份「准入比实际能力宽」的声明。

处置建议（阻塞 P1 插件准入，不阻塞 P0 基线）：与 4.10 台账 §4.3 的 D-2 处置保持一致，不新开决策——抬一份已发布包的兼容下限会改变旧节点上的准入结果（0.7.x 节点从可装变拒装），属于行为变更而非文案修正，应随台账 §3.4 中该包 `0.2.0 → 0.3.0` 的版本 bump 一起裁决对齐。若确认 readonly 确实兼容 0.7.0，则在 manifest 内补一段说明兼容依据的注释，而不是留着不解释的数字。

### 2.5 一个必须一起说清的数字：94 是快照，不是终值

4.10 台账 §3.4 已登记两项计划增量：`opskeeper-sre-readonly` `0.2.0 → 0.3.0`（工具 20 → 23，新增 `query_devices`、`query_incidents`、`get_incident_detail`），`opskeeper-sre-repair` `0.2.0 → 0.3.0`（工具 5 → 6，新增 `preview.read`）。

因此：

- 94 是**当前基线快照**；按台账计划全部落地后为 98。
- 任何在文档里硬编码「94 个工具」的地方都应写成「基线 `ff1b7e4…` 实测 94（20 / 12 / 56 / 5 / 1），台账目标 98」。否则同一个仓库会同时存在两个「工具总数」，且都没有时点。

## 3. 任务 1.5：版本与发布元数据漂移清单

### 3.1 漂移总表（V-1 – V-14）

| 编号 | 项 | 实测现状 | 期望/来源 | 性质 | 处置建议 | 阻塞级别 |
| --- | --- | --- | --- | --- | --- | --- |
| V-1 | `VERSION` | `v2026.09.14-rc4` | `check_release_version.py` 期望 `v2026.09.14-rc4` | 一致 | 随新版本重写 | 无 |
| V-2 | `RELEASE_VERSION.json` 的 `version` / `release_tag` | `2026.09.14-rc4` / `v2026.09.14-rc4` | 同上 | 一致 | 随新版本重写 | 无 |
| V-3 | `release_date` | `2026-09-16` | `CHANGELOG.md` 顶部条目 `## 2026.09.14-rc4 — 2026-09-14` | 同一次发布出现两个日期 | 以 CHANGELOG 条目日期为准统一（该文件是唯一被断言的日期来源） | 低 |
| V-4 | `release_candidate` | `true` | 仓库已演进到 10–11 月架构（基线含 notifier 内核迁移等合入） | 候选标记长期未关闭 | 新版本按候选流程重新置位，正式版再置 `false` | 低 |
| V-5 | `backend_commit` / `release_branch_head_at_signing` | `305bec8417ef25234f3fa10734ddd8761e25c398` | 基线 `ff1b7e40745b261ac2e4cce11607aa90922dc4af` | 落后 591 个提交（`git rev-list --count 305bec84..ff1b7e4`）；且确认为基线祖先（`git merge-base --is-ancestor` 为真） | 新版本写入新基线 commit | **阻塞发布** |
| V-6 | `web_hash` | `0c973ab5d5151a612c9ca3232c606c87d85247d8` | 树实算 `git rev-parse HEAD:web` = `e12f462fe38bffdab93a9317a2ece3f892fd7509` | 漂移 | 新版本重算 | **阻塞发布** |
| V-7 | `teamharness_source_tree` | `23ba29a0b64ab5a7995413354b7c0b2fe3dcfa12` | 树实算 `HEAD:plugins/opskeeper-teamharness` = `4ce3e682ccc27af7a3cd2ff05dbe7246a449b12b` | 漂移 | 新版本重算 | **阻塞发布** |
| V-8 | `teamharness_version` | `1.0.59` | 仓库实为 `1.0.75`（7 处载体一致，见 §2.2） | 漂移 | 新版本写 `1.0.75` | **阻塞发布** |
| V-9 | `installer_version` | `1.4.3` | `plugins/agentteams-plugin-installer/dashboard/public/plugin.json` = `1.4.3` | 一致 | 无需改动 | 无 |
| V-10 | 远端 tag 词表 | 7 个：`backend-v2026.09.03`、`plugins-v2026.09.03`、`release-v2026.09.03`、`release-v2026.09.19`、`release-v2026.09.20`、`release-v2026.09.20-patch.1`、`release-v2026.09.20-patch.2` | `release.yml` 第 53/146 行的语法 `^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.?[0-9]+)?$`（`scripts/tag_format.py` 从 workflow 读回） | 7 / 7 全部不匹配：都带前缀，且日期式不是三段版本式 | 见 §3.2 选项 A / B | **阻塞发布** |
| V-11 | `release_baseline_ref` | `release/20260922@d2920895363edca87e34ee00fcb33eb1c6090723` | 远端 `release/20260922` 分支仍存在，分支头 `ab9e94f1cfc8dd24eb1615da30dd22a7a0c63801`（2026-11-12） | 引用了一个仍在移动的分支。该分支与 main 在 `8774f7b3576bbde9f41a4dab3941f76ca51f1b9e`（2026-09-21）分叉：main 侧 499 个提交、分支侧 14 个提交，**这 14 个提交不在 main 上** | 新版本一律钉不可变 commit；`release/20260922` 处置见 §7 第 2 条 | **阻塞发布** |
| V-12 | `scope` | 含 `internal`、`db` | 当前树中 `internal/` 与 `db/` 已不存在，其余 10 项（`cmd`、`api`、`web`、两个 `plugins/*`、`deploy`、`docs`、`scripts`、`tests`、`testdata`）均存在 | scope 与仓库结构漂移 | 新版本 scope 按现树重写 | 中 |
| V-13 | `scripts/check_release_version.py` | 硬编码 `expected_version = "2026.09.14-rc4"`、`expected_tag = "v2026.09.14-rc4"`、`expected_baseline = "release/20260922@d2920895…"` | 该脚本是发布期硬闸门 | 任何新版本都要改脚本；漏改则闸门期望与发布物互相矛盾，且错误方向是「闸门红」而非「漏发」 | 与 V-5/V-6/V-7/V-8 同一个 PR 改 | **阻塞发布** |
| V-14 | `source_boundary` 语义 | 「tagged release commit may only add release metadata, docs, or release verification tooling」 | 基线共 3141 个路径，其中 2017 个在该断言的 scope 外 | 该断言在当前树上必然失败（见 §4.3 第 5 条 finding） | 二选一：收窄 scope 让断言成立，或放宽断言。见 §7 第 3 条 | 中 |

补充说明：V-3 与 V-4 不是错误，是「上一次发布留下的状态」在时间上的自然结果。列出来是为了让 §3.3 的落点表能覆盖到它们，而不是为了让清单显得更长。

### 3.2 新版本命名建议

**建议：采用 `v2026.11.0-rc.1` → `v2026.11.0`。**

理由：

1. **两个词表互不相交，必须选一个**。现有发布词表是「日期式 + rc」（`2026.09.14-rc4`），而当前 workflow 语法要求「三段数字式 + 可选 rc」（`v2026.11.0-rc.1`）。历史 tag 7/7 都不匹配现行语法，说明语法是 tag 之后才改的。因此「沿用 9 月命名」必然撞语法，而「只改 workflow」必然要扩语法——两条路都要走，先把选择摆出来。
2. **语义上这次是架构级重建**。发布物与 9 月 RC 不共享同一条能力线：微内核插件架构、六个封闭扩展点、运行时治理基座、notifier 内核迁移都在其上。补丁号（`patch.N`）表达不了这件事。
3. **三段式去掉「日期即版本」的错觉**。11 月可能有多次发布，但版本号只随能力线递增；日期式版本会把「同一天的两次构建」逼成补丁号，而补丁号在语义上被理解为「修 bug」，与实际内容不符。

**选项 A（推荐）**：采用 workflow 现行语法，废弃日期式。

- 新 tag：`v2026.11.0-rc.1`（候选）、`v2026.11.0`（正式）。
- `VERSION` 写 `v2026.11.0-rc.1`；`RELEASE_VERSION.json.version` 写 `2026.11.0-rc.1`（不带 `v` 前缀，与现有一致）。
- 旧 tag 保持不动：不删、不改写、不重签。历史是历史，改写它会让「已发布的产物」失去可复现性。
- 代价：与 9 月 tag 词表不连续，需要在 release note 里写一句「版本号语义变更」。这是一次性的沟通成本。

**选项 B**：改 workflow 语法以同时容纳日期式与 patch。

- 需要同时改 `release.yml` 两处正则、`scripts/tag_format.py` 的读回逻辑，并给 `check_release_version.py` 增加历史 tag 白名单。
- 代价：语法越写越长，而 `scripts/tag_format.py` 的文件注释恰好记录了这个教训——「同一份语法被抄三遍，其中两份说 `rc.4` 而仓库自己的版本不满足，于是构建会以退出码 2 死在项目自己的版本上，而没有人发现」。再扩一份语法是同一条路的下一段。
- 结论：不推荐。除非外部部署系统已经在消费 `release-vYYYY.MM.DD` 这类 tag 词表，且改动成本高于语义损失——这需要 §7 第 1 条拍板时确认。

### 3.3 处置落点表（谁在哪个任务里改）

| 漂移 | 落点 | 说明 |
| --- | --- | --- |
| V-5、V-6、V-7、V-8 | 发布准备任务（6.1） | 同属「重算发布元数据」这一个动作，应在同一个 PR 内完成 |
| V-1、V-2、V-3、V-4 | 发布准备任务（6.1） | 随新版本重写 |
| V-10 | 发布准备任务（6.2） | 新 tag 命名与签名流程；历史 tag 不动 |
| V-11 | 发布准备任务（6.2） | 发布源钉不可变 commit；分支处置见 §7 第 2 条 |
| V-12、V-14 | 发布准备任务（6.1） | scope 与 `source_boundary` 断言对齐 |
| V-13 | 与 V-5 同 PR | 硬编码常量与发布元数据同 PR 修改，避免出现「闸门先红、发布物后改」的中间态 |
| D-1 | 文档同步任务（6.7）或独立小 PR | 精确改动位置见 §2.3；归属见 §7 第 4 条 |
| D-2 | 插件能力恢复切片 S-2（随 readonly `0.2.0 → 0.3.0` bump） | 与 4.10 台账 §4.3 D-2 既有处置一致，不新开决策 |

一条通则：**同一类漂移必须落在同一个 PR**。V-5/V-6/V-7/V-8/V-13 分五个 PR 改，会出现「闸门期望已更新、发布物未更新」和反过来的两种中间态，而这两种中间态都会让 CI 给出难以解释的红或绿。

## 4. 任务 1.6：闸门复跑

执行环境：完整历史检出，基线 `ff1b7e40745b261ac2e4cce11607aa90922dc4af`，工作树无未提交改动。

### 4.1 三道必跑闸门（任务 1.6 指定）

**闸门 1 — `make module-check`**

```
go run ./scripts/modulecheck .
modulecheck: all module boundaries hold
```

结论：通过（退出码 0）。

**闸门 2 — `make pi-plugin-guard`**

```
python3 scripts/pi_plugin_guard_test.py
......
----------------------------------------------------------------------
Ran 6 tests in 0.017s

OK
pi-plugin-guard: 1 plugin(s) valid (upup)
python3 scripts/pi_plugin_guard.py .
pi-plugin-guard: no Pi plugins declared; workspace contract ready
```

结论：通过（退出码 0）。

**闸门 3 — `go test ./scripts/ledgercheck`**

```
ok  	github.com/vincent-wuhan/opskeeper/scripts/ledgercheck	3.203s
```

结论：通过（退出码 0）。

### 4.2 附加复跑（用于判断绿灯本身是否可信）

| 命令 | 结论 | 关键输出 |
| --- | --- | --- |
| `make audit-open-source` | 通过 | `open-source gate passed: 3489 text files audited in the tracked tree` |
| `make pig-tool-scoping-check` | 通过 | `ok github.com/vincent-wuhan/opskeeper/core/pig/pigprofile 32.160s`；对真 `pig` 二进制逐条核对 5 包 / 94 声明工具 |
| `make tag-format-check` | 通过 | `release tag grammar (read from the workflows): ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.?[0-9]+)?$`；`tag format check passed: the grammar accepts VERSION (v2026.09.14-rc4) and all 4 changelog tags, and still rejects 9 things that are not tags` |

注意 `tag-format-check` 的最后一句：现行语法能接受 `VERSION` 与 4 个 changelog tag。这说明**语法与仓库当前状态是自洽的**，V-10 的 7 个远端 tag 是历史遗留，不是现行流程会产生的形状。这两个事实必须一起读：语法不用改（选项 A 的前提成立），但历史 tag 也不能被当作期望样例。

### 4.3 未通过项：`make version-check`（发布期闸门，按设计为红）

```
python3 scripts/check_release_version.py
  outside source boundary: .codegraph/.gitignore
  outside source boundary: .dockerignore
  outside source boundary: .env.example
  ...
  ... and 1997 more
release version check failed: 5 finding(s)
  - plugin.yaml version drifted (expected '1.0.59', found '1.0.75')
  - dashboard plugin version drifted (expected '1.0.59', found '1.0.75')
  - web source tree hash drifted (expected '0c973ab5d5151a612c9ca3232c606c87d85247d8', found 'e12f462fe38bffdab93a9317a2ece3f892fd7509')
  - TeamHarness source tree hash drifted (expected '4ce3e682ccc27af7a3cd2ff05dbe7246a449b12b', found '23ba29a0b64ab5a7995413354b7c0b2fe3dcfa12')
  - release commit contains changes outside its source boundary (expected 'only release metadata, docs, tests and _test.go may differ', found '2017 of 3141 paths, first: .codegraph/.gitignore')
make: *** [Makefile:103：version-check] 错误 1
```

逐条对应：

| finding | 对应漂移 | 说明 |
| --- | --- | --- |
| plugin.yaml / dashboard plugin 版本漂移（期望 1.0.59，实为 1.0.75） | V-8 | 脚本期望值取自 manifest 的 `teamharness_version`，实值取自仓库文件；仓库侧是对的，manifest 侧是旧的 |
| web source tree hash 漂移 | V-6 | 期望值取自 manifest 的 `web_hash`，实值取自当前树 |
| TeamHarness source tree hash 漂移 | V-7 | 同上，对象是 `plugins/opskeeper-teamharness` |
| 发布 commit 含 scope 外改动（2017 / 3141 路径） | V-14、V-12 | `source_boundary` 断言与当前 scope、当前仓库现实不一致 |

处理结论：

- **这不是环境问题，也不是「CI 红」**。它是发布元数据确实停在 9 月这件事的真实投影：常规 CI 链路不跑这道闸门，所以它可以长期红着而无人察觉。本次冻结的意义之一，就是把它从「没人看的红」变成「有处置建议的红」。
- **不在 P0 基线冻结阶段改**。改它等于宣布新版本，而版本号需要先拍板（§7 第 1 条）。处置落点见 §3.3。
- **不得为了让闸门变绿而放宽断言**。第 5 条 finding 的处置（收窄 scope 还是放宽 `source_boundary`）是一个架构决策，不是一次「把红线挪走」的操作。

### 4.4 失败处理：两条经验

**经验 1：浅克隆造成的假阳性，必须先证伪再上报。**

在浅克隆树上跑 `make version-check`，脚本会额外报一条 `release commit is not descended from backend_commit`。补全历史（`git fetch --unshallow origin main`）后，用 `git merge-base --is-ancestor 305bec84… ff1b7e4…` 验证为真，该条 finding 消失。

结论：**任何依赖祖先关系或提交计数的发布闸门，必须在完整历史上跑**。浅克隆上的失败要先证伪再上报，否则会把「克隆方式」当成「仓库坏了」，进而做出错误的回滚决策。这条经验已固化进本文件 §1.2 第 4 条口径。

**经验 2：tag 语法是后加的，不要用历史 tag 反推期望。**

7 个历史 tag 全部不满足现行语法（§3.1 V-10）。若按「以前的 tag 长什么样」来推断「这次 tag 该长什么样」，会得到日期式，从而与 workflow 冲突；反过来，把语法当成「仓库自己的版本不满足」而去改语法，又会重复 `scripts/tag_format.py` 注释里记录过的那个教训。正确顺序是：先从 workflow 读回语法（`scripts/tag_format.py` 就是这么做的），再单独处理历史 tag 的兼容。

### 4.5 与 4.11 闸门报告的关系

4.11 的产出 `gate-run-report.md` 基线为 `21efd90a…`，本次基线为 `ff1b7e4…`，两次之间合入了 notifier 内核迁移（B1+B2）、alert 零调用方清理、BaseTool 上呼桥等变更。

- 三道必跑闸门的结论一致：均为绿。
- 4.11 的遗留项 L1–L9（ruby、docker、`go test ./plugins/...` 无匹配包、goproxy.cn 等）本次未重新评估。它们是环境类遗留，与本 issue 的版本漂移无关，留在发布准备阶段统一处理。
- 4.11 已固化的三份仓内准入测试（`cmd/opskeeper/plan_manifests_gate_test.go`、`core/manager/middleware/toolset/adapter_plan_manifests_test.go`、`cmd/opskeeper-edge/collector_plan_manifests_test.go`）在本次基线上未重跑。**建议在 P1 插件准入切片落地前补跑一次**，因为本次核对发现的 D-2 属于准入口径变化，会影响这些测试的对照结论。

## 5. 任务 1.7：P0 基线冻结

### 5.1 冻结锚点

| 锚点 | 值 | 用途 |
| --- | --- | --- |
| 基线 commit | `ff1b7e40745b261ac2e4cce11607aa90922dc4af`（2026-11-04） | 一切结论的取数点 |
| 基线分支 | `codex/p0-baseline-freeze` | 本次交付分支 |
| 发布 tag | 待 §7 第 1 条拍板后写入 `v2026.11.0-rc.1` | 发布锚点 |
| 回滚锚点 | `release-v2026.09.20-patch.2` → commit `8774f7b3576bbde9f41a4dab3941f76ca51f1b9e`（2026-09-21） | 线上回滚目标；它同时是 main 与 `release/20260922` 的分叉点 |
| 历史 tag 集合 | 7 个（见 V-10） | 回滚阶梯：`release-v2026.09.20-patch.2` → `release-v2026.09.20` → `release-v2026.09.19` → `release-v2026.09.03` |

回滚阶梯的说明：`backend-v2026.09.03` 与 `plugins-v2026.09.03` 是更早的分组件 tag，不适合作为整仓回滚目标；整仓回滚应沿 `release-*` 系列回退。

### 5.2 环境变量清单

来源：仓库 `.env.example`。共 **62 个活跃变量名**（唯一名 62 个；另有 7 个仅在注释中出现的名称，未计入）。清单只列**变量名**，值不在本仓库、不进入本文档、也不进入任何交付物。

| 组 | 数量 | 变量名 |
| --- | --- | --- |
| A 数据库与存储 | 4 | `OPSKEEPER_DB_DIALECT`、`OPSKEEPER_DB_PATH`、`OPSKEEPER_CHROMEM_DIR`、`OPSKEEPER_VECTOR_BACKEND` |
| B 监听地址与端口 | 6 | `OPSKEEPER_HTTP_ADDR`、`OPSKEEPER_HTTP_PORT`、`OPSKEEPER_HTTP_REDIRECT_PORT`、`OPSKEEPER_METRICS_ADDR`、`OPSKEEPER_EDGE_CLOUD_ADDR`、`OPSKEEPER_FRONTIER_ADDR` |
| C 鉴权与凭据（**秘密**，仅列名） | 8 | `OPSKEEPER_ADMIN_PASSWORD`、`OPSKEEPER_JWT_SECRET`、`OPSKEEPER_EDGE_ACCESS_KEY`、`OPSKEEPER_EDGE_SECRET_KEY`、`OPSKEEPER_OPENAI_API_KEY`、`OPSKEEPER_NOTIFY_FEISHU_SECRET`、`OPSKEEPER_NOTIFY_DINGTALK_SECRET`、`OPSKEEPER_NOTIFY_WEBHOOK_SECRET` |
| D 模型与推理 | 2 | `OPSKEEPER_OPENAI_BASE_URL`、`OPSKEEPER_OPENAI_MODEL` |
| E 告警阈值 | 6 | `OPSKEEPER_ALERT_ENABLED`、`OPSKEEPER_ALERT_CPU_PERCENT`、`OPSKEEPER_ALERT_MEM_PERCENT`、`OPSKEEPER_ALERT_DISK_USED_PERCENT`、`OPSKEEPER_ALERT_LOAD1`、`OPSKEEPER_ALERT_COOLDOWN` |
| F 通知渠道 | 17 | `OPSKEEPER_NOTIFY_ENABLED`、`OPSKEEPER_NOTIFY_TIMEOUT`、`OPSKEEPER_NOTIFY_DEFAULT_CHANNELS`、`OPSKEEPER_NOTIFY_LOG_ENABLED`、`OPSKEEPER_NOTIFY_LOG_NAME`、`OPSKEEPER_NOTIFY_FEISHU_ENABLED`、`OPSKEEPER_NOTIFY_FEISHU_NAME`、`OPSKEEPER_NOTIFY_FEISHU_WEBHOOK_URL`、`OPSKEEPER_NOTIFY_DINGTALK_ENABLED`、`OPSKEEPER_NOTIFY_DINGTALK_NAME`、`OPSKEEPER_NOTIFY_DINGTALK_WEBHOOK_URL`、`OPSKEEPER_NOTIFY_SLACK_ENABLED`、`OPSKEEPER_NOTIFY_SLACK_NAME`、`OPSKEEPER_NOTIFY_SLACK_WEBHOOK_URL`、`OPSKEEPER_NOTIFY_WEBHOOK_ENABLED`、`OPSKEEPER_NOTIFY_WEBHOOK_NAME`、`OPSKEEPER_NOTIFY_WEBHOOK_URL` |
| G Edge 节点 | 4 | `OPSKEEPER_EDGE_COLLECTOR_INTERVAL`、`OPSKEEPER_EDGE_COLLECTOR_MODE`、`OPSKEEPER_EDGE_SCRAPE_CONFIG_FILE`、`AGENTTEAMS_CONTROLLER_IMAGE` |
| H 会话、功能开关与技能根 | 7 | `OPSKEEPER_ADMIN_EMAIL`、`OPSKEEPER_JWT_ACCESS_TTL`、`OPSKEEPER_JWT_REFRESH_TTL`、`OPSKEEPER_CHAT_DIAGNOSE_ENABLED`、`OPSKEEPER_CHAT_KB_FIRST_ENABLED`、`OPSKEEPER_CHAT_PROMOTE_ENABLED`、`OPSKEEPER_SKILLS_ROOT` |
| I 可观测与上游 | 4 | `OPSKEEPER_PROM_ENABLED`、`OPSKEEPER_PROM_URL`、`OPSKEEPER_PROM_QUERY_URL`、`OPSKEEPER_PROM_REMOTE_WRITE_URL` |
| J 目录与归档 | 3 | `OPSKEEPER_PAGES_DIR`、`OPSKEEPER_WORKSPACE_ROOT`、`OPSKEEPER_DEMO_ARCHIVE_URL_TEMPLATE` |
| K 部署拓扑 | 1 | `OPSKEEPER_FRONTIER_SERVICE_NAME` |

合计 62，与 `.env.example` 中活跃定义一一对应。

冻结纪律：

- C 组 8 项与 F 组中的 webhook URL、名称类配置项都**不进入本仓库的任何交付物**，只保留变量名。
- 变量名清单随本文件冻结；变量值随部署环境演进，不在冻结范围内。
- 新增或废弃变量必须同步本表，否则「环境变量清单」会退化成一份没人核对的快照。

### 5.3 冻结范围

**冻结**（P0 基线内不再变动）：

- 5 个 SRE 包的 manifest 与工具清单（基线实测 94 个工具，分解 20 / 12 / 56 / 5 / 1）、包版本 `0.2.0`。
- TeamHarness 版本 `1.0.75` 及其 7 处载体。
- `min_pig_version: 0.4.0` 与 `core/pig/go.mod` 的 PiG 依赖锁定。
- profile 组成与 scope 授予集合（finance 4 包 / saas 5 包）。
- `install.strategy` 词表（`pin` / `rolling`）。
- §5.2 的环境变量名清单。
- §4 的闸门结论及其基线 commit。
- `VERSION` 与 `RELEASE_VERSION.json` 的**现值**——冻结为漂移证据，不是期望值。

**不冻结**（后续切片可变更，但变更后必须重新核对）：

- readonly `0.2.0 → 0.3.0`（工具 20 → 23）与 repair `0.2.0 → 0.3.0`（工具 5 → 6）（台账 §3.4）。
- readonly 的 `min_edge_version`（D-2 待裁决）。
- 各扩展点新增插件包（datasource / adapter / notifier / provider / collector / alert）。
- 发布元数据本身（随新版本重写，见 §3.3）。
- 运行时治理与 PiG adapter 的模型（由对应 change 承载）。

### 5.4 阶段闸门结构

四档递进，出口条件不可由执行方自行放宽：

| 档 | 内容 | 出口条件 | 出口证据 |
| --- | --- | --- | --- |
| A 档 | 基线收口（任务 1.4–1.7） | 三道必跑闸门全绿 + 漂移清单逐项有处置建议 + 基线 commit 与环境变量清单冻结 | 本文件 + 闸门输出摘要 |
| B 档 | Runtime 治理首切片 | `runtime-management-gap` 阶段验收通过 | 该 change 的验收记录 + 闸门输出 |
| C 档 | PiG adapter 与插件能力恢复 | `pig-runtime-adapter` 阶段验收通过 + 台账绿行在当前 Policy 下复跑仍绿 | 台账更新 + 准入复跑输出 |
| D 档 | 企业流程与协同（Paperclip 只读面、审批桥默认关闭、团队记忆第一层） | 只读面可用 + 审批桥在 `approvalBridgeEnabled=false` 默认值下通过幂等/超时/重放/hash/actor 测试 | 安全测试输出 + 回滚开关验证 |
| 发布准备 | 全量回归、安全审查、部署与回滚演练 | §3.1 的全部「阻塞发布」项闭合 + `make version-check` 转绿 | release readiness 报告 |

每一档的出口都必须留下四样东西：**闸门命令与实际输出摘要、剩余风险、回滚方式、是否影响下一档**。缺任何一样，该档不算完成。

### 5.5 回滚锚点

- 插件包：按包名 + 版本回滚。manifest 版本号即回滚点，回退即撤掉新增工具声明。
- 运行时与任务模型：迁移期保留旧读路径与迁移前快照。
- PiG：opt-in 固定二进制，禁用后回落到既有 runtime，不留半个注册态。
- Paperclip 审批桥：默认关闭，可独立禁用，不需要回滚整体同步。
- Web 与控制面镜像：保留上一版本 tag 与数据库迁移回滚说明。
- 发布级回滚：沿 `release-*` 阶梯回退，最近的一级是 `release-v2026.09.20-patch.2`（`8774f7b3…`）。

## 6. 验收对照

| 验收项 | 对应章节 | 状态 |
| --- | --- | --- |
| TeamHarness `1.0.75`、五个 SRE 包、94 个 manifest 工具与 Makefile/文档描述的一致性核对 | §2.2（逐项核对表） | 已完成；manifest 侧一致，描述侧 2 处漂移（D-1、D-2）已登记 |
| `VERSION` / `RELEASE_VERSION.json` / release scope / backend commit / web hash / 插件版本的漂移清单 | §3.1（V-1 – V-14） | 已完成，逐项给出处置建议与阻塞级别 |
| 新版本命名建议 | §3.2 | 已完成，建议 `v2026.11.0-rc.1` → `v2026.11.0`，附选项 A/B 与理由 |
| `make module-check` / `make pi-plugin-guard` / `go test ./scripts/ledgercheck` 的真实运行输出 | §4.1 | 已完成，三道均绿，输出原文记录 |
| 失败处理 | §4.3（发布期闸门红，5 条 finding 逐条对应）、§4.4（两条经验） | 已完成 |
| 基线 commit 冻结 | §1.1、§5.1 | 已完成 |
| 环境变量清单 | §5.2（62 个活跃变量名，按 11 组归类，秘密只列名） | 已完成 |
| 阶段闸门与 OPC 里程碑结构 | §5.4 | 已完成，四档出口条件与证据要求已定义 |
| tasks.md 勾选 1.4 – 1.7 | 本 change 的 `tasks.md` | 随本文件同步更新 |

## 7. 待拍板事项（本 issue 不代决）

1. **版本号与 tag 词表**（§3.2）：选项 A（`v2026.11.0-rc.1` → `v2026.11.0`，采用 workflow 现行语法）还是选项 B（扩语法以容纳日期式与 patch）。选 B 前需确认是否有外部部署系统在消费 `release-vYYYY.MM.DD` 词表。
2. **`release/20260922` 分支的处置**（V-11）：该分支与 main 在 `8774f7b3…` 分叉，含 14 个不在 main 上的提交，且截至 2026-11-12 仍在移动。是冻结为只读，还是先把这 14 个提交取回 main 再废弃该分支。
3. **`source_boundary` 断言**（V-14）：维持「发布 commit 只允许加元数据 / 文档 / 测试」的强断言并收窄 scope，还是放宽断言。前者更硬，但会让发布 commit 更难产；后者更好产，但削弱「发布物可复现」这一保证。这一条决定 `make version-check` 能不能转绿。
4. **D-1 描述侧修正的落点**（§2.3）：随发布准备的文档同步任务（6.7）一起改，还是作为独立小 PR 先改掉（先让闸门与文档不再互相打脸）。

## 8. 附：本文件的复算方式

在基线 commit 上执行：

```
git fetch --unshallow origin main          # 或以完整历史检出
git rev-parse HEAD                          # 基线 commit
git log -1 --format=%ad --date=short         # 基线日期
make module-check
make pi-plugin-guard
go test ./scripts/ledgercheck
make audit-open-source
make pig-tool-scoping-check
make tag-format-check
make version-check                          # 发布期闸门，按设计为红
```

漂移项取数：

```
cat VERSION
python3 -c "import json;[print(k,'=',v) for k,v in json.load(open('RELEASE_VERSION.json')).items()]"
git rev-parse HEAD:web
git rev-parse HEAD:plugins/opskeeper-teamharness
git rev-list --count 305bec8417ef25234f3fa10734ddd8761e25c398..HEAD
git merge-base --is-ancestor 305bec8417ef25234f3fa10734ddd8761e25c398 HEAD
git ls-remote --tags origin
python3 -c "import sys;sys.path.insert(0,'scripts');import tag_format;print(tag_format.canonical_pattern())"
```

工具数取数（去重口径见 §1.2 第 2 条）：

```
python3 - <<'PY'
import re, glob, os
total = []
for f in sorted(glob.glob('plugins/pig-ops/opskeeper-sre-*/pig-ops.yaml')):
    tools = re.findall(r'-\s*\{\s*name:\s*([A-Za-z0-9_.:-]+)', open(f).read())
    print(os.path.basename(os.path.dirname(f)), len(tools))
    total += tools
print('entries', len(total), 'unique', len(set(total)))
PY
```

环境变量清单取数：

```
python3 -c "import re;print([l.split('=')[0] for l in open('.env.example') if re.match(r'^[A-Z][A-Z0-9_]*=',l)])"
```

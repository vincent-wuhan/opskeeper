# Harness 评测指南

> **面向**：opskeeper 开发者、平台 SRE、CI 维护者
> **目的**：用 golden case 评测 Agent 决策质量，用 judge 模型打分，用 leaderboard 跟踪回归
> **关联**：
> - ADR：[docs/superpowers/decisions/2026-07-13-harness-judge-models.md](superpowers/decisions/2026-07-13-harness-judge-models.md)
> - 集成指南：[docs/integration-guide.md](integration-guide.md)
> - 运维手册：[docs/operations-manual.md](operations-manual.md)

---

## 一、概念

| 概念 | 说明 |
|---|---|
| **golden case** | 标准化的事故场景（YAML 描述），含注入 / 期望 / rubric |
| **fault-injector** | 在隔离环境注入故障（PG 长事务 / Redis 大 key / K8s pod OOM / 主机磁盘满）|
| **judge** | LLM 评分模型（默认 Claude Sonnet 4 + GPT-4o 双模型取均值）|
| **leaderboard** | 评分历史 + 回归基线 + 排名 |
| **regression baseline** | 历史评分基线，新评分对比基线判断是否下降 |

## 二、CLI 速查（`cmd/opskeeper-eval`）

```bash
opskeeper-eval --help
opskeeper-eval list-cases                           # 列出全部 golden case
opskeeper-eval list-cases --filter pg               # 只看 pg 类
opskeeper-eval run --case pg/long-running-tx        # 跑单个 case
opskeeper-eval run --suite middleware-baseline      # 跑一组
opskeeper-eval run --suite full --env staging       # 全量回归
opskeeper-eval inject --case k8s/pod-oom --env staging  # 仅注入不评分
opskeeper-eval judge --case pg/long-running-tx --response agent-response.json  # 外部 judge
opskeeper-eval leaderboard                          # 查看排行榜
opskeeper-eval leaderboard --threshold 0.6          # recovery_pass_rate 门槛
opskeeper-eval plugin-coverage                      # golden case 的能力期望 vs 插件包能力
opskeeper-eval plugin-coverage --fail-on-gap         # CI：有结构性缺口即非零退出
opskeeper-eval plugin-coverage --filter host/ --json # 只看主机类，机器可读
opskeeper-eval vocabulary                           # golden case 的能力期望 vs 本构建全部能力注册表
opskeeper-eval vocabulary --fail-on-gap              # CI：有结构上无法满足的 case 即非零退出
opskeeper-eval vocabulary --json                    # 机器可读
opskeeper-eval vocabulary --kind-map kinds.json     # 校验 kind 映射文件是否陈旧
opskeeper-eval axes                                 # 每个 case 声明的三个诊断轴
opskeeper-eval axes --fail-on-unmeasured-axis        # CI：有 case 测不出某个轴即非零退出

# 闭环最后一环：生产契约 → judge 可评分的响应
opskeeper-eval project --contract rc.json --kind-map kinds.json --out resp.json
opskeeper-eval project --contract rc.json --kind-map kinds.json --bare --out resp.json
opskeeper-eval judge --case pg/lock-waits --response resp.json   # 直接评分
```

> `judge` 会先做能力检查：本构建无法产出的 case **直接拒绝打分**，并指名缺哪个
> 符号。确实要打分（例如为了看部分得分曲线）时加 `--allow-unservable`，结果
> 里的 `unservable_case` 字段会一直跟着这个分数。

---

## 二.5、能力覆盖闸门（plugin-coverage）

**问题**：golden case 的期望写成 `<family>.<method>`（`pg.lock_waits`、
`host.host_load`），而舰队的能力来自插件包。两套词汇之前没有人做过连接，
于是**一个结构上不可能通过的 case 会永远打 0 分**，而 leaderboard 把它
显示成"Agent 不行"——这是个会让所有人查错方向的假象。

**加入**：`core/floor/pluginmanifest/coverage.go` 建立连接表，
`opskeeper-eval plugin-coverage` 打印结果，`--fail-on-gap` 让 CI 卡住。

**关键性质**：

- **映射是声明的，不是猜的**。`toolCapabilities` 的每一条都是从注册该工具的
  extension 里读出来的，不是从工具名前缀推出来的——按前缀推会把
  `list_database_sources` 猜对、`query_change_events` 猜错。
- **每个缺口都要有解释**，而不是只报一个数字。归到两个命名清单之一：
  - `MiddlewareFamilies`（`pg` / `redis` / `k8s` / `mq` / `kafka` / `rabbitmq`）
    —— 控制面 adapter，**不是**插件包。清单的词汇来自 adapter 实现
    （`core/manager/middleware/adapter/<pkg>/<pkg>.go` 注册 `"<pkg>.method"`），
    不是 case 所在目录名：mq 目录下的 case 写的是 `kafka.*` / `rabbitmq.*`，
    按目录名建表会把它们全报成"包不存在"。
  - `NonPackageFamilies`（`git-artifact`）—— 控制面关联器，根本不是工具族。
- **两个方向的漂移都是红色**：
  - 己方包有工具没有能力族 → `TestEveryShippedToolHasACapabilityFamily`
    （否则该工具对应的 case 会静默掉分）。
  - 表里有条目没有对应工具 → `TestTheCapabilityTableHasNoDeadEntries`
    （改名后遗留的死键在 review 时是误导）。
  - case 里出现既没被打包、也没被任何清单解释的族 →
    `TestEveryCaseFamilyIsEitherPackagedOrNamedAsADeliberateGap`。

**当前真实结果：2/20 全绿。** host 两例由只读包覆盖；其余 18 例的缺口
**全部**落在控制面 adapter 上。这是仓库的真实状态，不是回归——把它打印
出来正是这个闸门存在的意义。

---

## 二.6、结构可满足性闸门（vocabulary）

**问题**：`plugin-coverage` 回答的是"**插件包**能不能服务这个期望"。在那之前
还有一层更靠前的问题——"**这个系统**能不能产出这个期望"。两套词表写在不同
地方，从没有人做过连接，于是**一个任何注册表都没有对应工具的 case 会永远打
0 分**，而 leaderboard 把它显示成"Agent 不行"。

**加入**：`core/harness/vocabulary`（纯计算，不依赖实现）+ `opskeeper-eval
vocabulary`。四个能力来源按"谁拥有这个能力"的顺序被咨询：

| 来源 | 覆盖形态 | 读法 |
|---|---|---|
| `middleware-adapter` | **精确符号** | 真实调用 6 个 adapter 的 `RegisterTools` 进一个空 registry，再 `ListTools("")`。**不扫源码** |
| `plugin:<name>` | **能力族** | `p.Capabilities()`。包声明的是"我覆盖 host 这个族"，不是逐个方法名 |
| `loop-investigator` | **精确符号** | `investigatorreal.RemediationActions`（闭环修复规划器实际会提出的动作） |
| `loop-root-cause-kinds` | 单独报告 | 从 `investigatedOutputSchema` **派生**的 enum，不并入并集 |

**关键性质**：

- **精确匹配优先于族匹配**。`plugin-coverage` 已经论证过按族连接是刻意的
  （逐方法名匹配会在包改名的瞬间产生假阴性）；这里再进一步——全构建范围内
  只要有任何一个注册表按字面名注册了该符号，就报那个注册表，因为它才是能被
  精确指过去的子系统。
- **adapter 工具表是"跑出来的"，不是"扫出来的"**。抓字符串字面量是第二个
  解析器，它对什么算工具名有自己的看法，并且会静默漏掉任何它没有建模的注册
  路径——而那正是能力闸门要防的失效。
- **loop 的动作表有 AST 漂移测试**（`TestTheDeclaredVocabularyMatchesTheCode`），
  双向比对：代码能产出但没声明 → 红；声明了但没代码路径能产出 → 红。
- **根因 enum 是派生的**，直接解析模型实际被约束的那份 JSON schema，所以
  不存在漂移可能；restate 一份就会变成"带测试形状的注释"。

**`judge` 的拒绝**。可满足性不达标时 `opskeeper-eval judge` **直接拒绝打分**，
错误信息指名缺哪个符号。理由和 plugin-coverage 一样：一个 0 分在产物里会被读
成对 agent 的判决，而它其实是对语料库/能力表的判决。`--allow-unservable`
可以强行打分，代价是产物里永久带上 `unservable_case` 字段。

**当前真实结果：9/20 可满足。** 缺口具体且小——14 个符号：

- 根因 3 个：`k8s.top_pods`、`pg.replication_status`、`git-artifact.LinkK8sImage`
- 修复 11 个：`k8s.uncordon`、`k8s.drain`、`k8s.resize_pvc`、`k8s.cleanup_logs`、
  `kafka.restart_broker`、`kafka.scale_consumer`、`kafka.repartition`、
  `rabbitmq.scale_consumer`、`redis.kill_client`、`redis.scan_and_delete`、
  `redis.scan_and_redistribute`

注意 `redis.kill_client`（语料）vs `redis.client_kill`（adapter 注册）：**这是
改名未对齐，不是能力缺失**——两种读法差一个字，而闸门按精确匹配处理，所以它
是缺口。这条差异要由人来裁决，代码不应该替人猜。

**当前真实结果：平台 9/20，闭环 0/20。** 第二个数字才是决定 judge 会怎么做的
那个——见下一节。

> **一个被抓住的错误**：这个闸门的第一版只拿 corpus 去比
> `investigatorreal.RemediationActions`，报出 **0/20**。那个数字是错的——
> 语料的 57 个符号里有 36 个由真实 middleware adapter 注册。拿单个子系统去
> 比整张能力表，得到的是"全都不满足"，而一个"全都不满足"的闸门和一个正常
> 工作的闸门**长得一模一样**：都打印数字、都非零退出。因此
> `TestACaseWhoseSymbolsTheAdaptersRegisterIsReportedServable` 把方向钉死。

---

## 二.7、诊断轴闸门（axes）

**问题**：`plugin-coverage` 与 `vocabulary` 问的都是"这个 case 能不能被服务"。
再往前还有一层：**这个 case 能不能被测量**。judge 从这一批起按
**Localization × Identification × Reason** 打分（arXiv:2606.29193），而一个
case 没声明的轴不会产生数字——"没测到"在产物里和"0 分"长得一模一样。

**三个轴问的三件事**（论文原文：*where the fault occurs* / *what type of fault
it is* / *whether the reasoning trace is grounded in relevant evidence*）：

| 轴 | 问什么 | 读哪一面 | 从 case 的什么派生 |
|---|---|---|---|
| `localization` | 故障在**哪里** | 结论（root cause + remediation） | case id 的资源族（`pg/lock-waits` → `pg`）+ 注入参数里的身份值（`table: orders`、`namespace: test`） |
| `identification` | 故障是**什么类型** | 结论 | case id 的故障段分词（`lock-waits` → `["lock","waits"]`） |
| `reason` | 推理**有没有落到证据上** | **tool call 轨迹**（名字 + 参数 + 结果） | `expect.root_cause_lines` —— 语料要求 agent 必须做出的观测 |

**为什么不用模型判这三个**：它们是"答案里有没有这个资源名/故障名/这些观测"
的核对。模型的判断严格劣于一次 substring 比较，所以 LLM judge 与 heuristic
judge 带的是同一组数字，两者之间的差异只留在判断成分更重的 `rca_accuracy`。

**为什么 `reason` 读轨迹而不是读答案**：这正是三维与只看最终答案的分界线。
一份结论完全正确、tool call 一条没有的回答，在四个过程维度上是满分，和真做过
诊断的回答无法区分；加上这个轴之后它会被 `Flagged` 进人工复评队列：

```
overall=1.00  localization=0.50  identification=1.00  reason=0.00
flagged: the answer scores 1.00 but the reasoning trace is ungrounded (reason=0.00)
```

**判据与阈值**（`core/harness/judge/diagnostic.go`）：`Overall >= 0.7` 且
`reason <= 0.5` 才触发 flag。两个下界各自是一条陈述：结论不对的回答本来就该
证据单薄，全 flag 会淹掉真正要看的队列；而一半要求观测缺失之后，结论已经不再
被"agent 实际看过的东西"支撑。

**为什么不动 `Overall`**：重排权重会静默作废已存下来的每一个分数和由它们画出的
leaderboard 对比，而语料还没有跑出足以支撑新权重的分布。所以这一批改变的是
**判决**（flag），不是总分。

**`--fail-on-unmeasured-axis`**：语料里每个 case 都必须能测出三个轴。今天的
真实结果是 **20/20 可测量**，其中 **5 个的 locus 只有资源族**（注入发生在
"那台主机"/"那个副本"上，case 里没有更窄的名字）。这 5 个在输出里带 `~`：

```
$ opskeeper-eval axes
cases: 20   coarsened locus (family only): 5   unmeasured: 0
~ host/cpu-spike             locus=[host] type=[cpu spike] evidence=3
  pg/lock-waits              locus=[pg orders] type=[lock waits] evidence=2
```

`~` 是**报出来**而不是补一个注入器从没用过的名字：粗的轴看得见，编的名字看不出来。

---

## 三、Golden Case 编写规范

### 3.1 目录结构

```
core/harness/cases/<resource>/<case-name>/case.yaml
```

例：

```
core/harness/cases/host/disk-full/case.yaml
core/harness/cases/pg/long-running-tx/case.yaml
core/harness/cases/redis/big-key/case.yaml
```

### 3.2 YAML 字段规范

```yaml
id: pg/long-running-tx                   # 必填，格式 <resource>/<case-name>
description: 模拟 PG 长事务阻塞          # 必填，一句话
severity: P0                             # 必填，P0/P1/P2/P3
tags: [pg, lock, performance]            # 可选，用于筛选

prerequisites:                           # 可选，前置条件列表
  - pg.test_db_seeded
  - pg.test_user_with_privilege

inject:                                  # 必填，注入步骤
  - type: pg.open_long_tx                # 注入器类型
    duration: 300s                       # 持续时间
    params:                              # 注入参数
      sql: "BEGIN; SELECT pg_sleep(60);"
      sessions: 10

expect:                                  # 必填，期望行为
  time_to_detect: 30                     # 秒，期望 Agent 多快发现
  time_to_remediate: 120                 # 秒，期望 Agent 多快修复
  root_cause_lines:                      # 期望根因工具
    - pg.pg_stat_activity
    - pg.pg_locks
    - pg.top_queries
  remediation_options:                   # 期望修复方案（多选一即可）
    - pg.kill_session
    - pg.cancel_query

rubric:                                  # 必填，评分维度
  rca_accuracy: 0.85                     # 根因准确度阈值
  time_to_remediate: 120                 # 修复时长阈值
  no_collateral_damage: true             # 是否要求零副作用

metadata:                                # 可选，元信息
  owner: "@opskeeper-oncall"
  created_at: "2026-07-13"
  references:
    - "https://wiki.opskeeper.io/runbook/pg-long-tx"
```

### 3.3 完整示例：PG 长事务 case

```yaml
id: pg/long-running-tx
description: 模拟 PG 长事务导致锁等待和性能下降
severity: P0
tags: [pg, lock, performance, long-tx]
prerequisites:
  - pg.test_db_with_orders_table
  - pg.test_user_with_kill_privilege

inject:
  - type: pg.open_long_tx
    duration: 600s
    params:
      sql: "BEGIN; LOCK TABLE orders IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(120); COMMIT;"
      tx_id: tx-12345

expect:
  time_to_detect: 30
  time_to_remediate: 120
  root_cause_lines:
    - pg.pg_stat_activity
    - pg.pg_locks
    - pg.top_queries
  remediation_options:
    - pg.kill_session
    - pg.cancel_query

rubric:
  rca_accuracy: 0.85
  time_to_remediate: 120
  no_collateral_damage: true

metadata:
  owner: "@opskeeper-oncall"
  created_at: "2026-07-13"
  references:
    - "https://wiki.postgresql.org/wiki/Lock_Monitoring"
```

### 3.4 JSON Schema 校验

`core/harness/schema/case.schema.json` 是权威 schema。新增 case 自动校验：

```bash
opskeeper-eval vocabulary --cases-dir core/harness/cases
# → 加载并校验全部 case；任一个不符合 schema 就非零退出
```

校验失败的常见原因：
- 缺 `id` / `description` / `severity` / `inject` / `expect` / `rubric`
- `time_to_detect` > `time_to_remediate`（逻辑错误）
- `inject.type` 未在 fault-injector 注册表中
- `rubric.*` 超出合理范围（`rca_accuracy` 应在 [0, 1]）

---

## 四、故障注入器（fault-injector）

### 4.1 内置注入器

| 前缀 | 注入类型 | 状态 |
|---|---|---|
| `pg.` | `inject_lock_chain` / `begin_txn_hold` / `hold_old_txn` / `run_slow_queries` / `inject_table_bloat` / `run_autovacuum` | ✅ **真实现**（pgx 连真库，决策 297） |
| `pg.` | `inject_replica_lag` | ⚠️ 大声拒绝：单节点造不出复制延迟 |
| `redis.` | `inject_big_key` / `inject_hot_key` / `inject_memory_burst` / `inject_slow_commands` | ✅ **真实现**（go-redis 连真库，决策 298） |
| `host.` | `fill_disk` / `cpu_stress` | ✅ **真实现**（决策 299；**最危险的一个**，见 4.4） |
| `k8s.` | `cordon_node` / `inject_memory_pressure` | ✅ **真实现**（client-go 连真 API server，决策 304） |
| `k8s.` | `set_bad_image` / `fill_pv` | ⚠️ 大声拒绝：信号由真 kubelet 产生 / API 里没有"填满一个卷"这个操作 |
| `rabbitmq.` | `inject_message_burst` | ✅ **真实现**（amqp091-go 连真 broker，决策 301） |
| `kafka.` | `inject_consumer_lag` / `inject_partition_skew` | ✅ **真实现**（kafka-go 连真 broker，决策 300） |
| `kafka.` | `kill_broker` | ⚠️ 大声拒绝：停掉 broker 撤不回来 |

这张表是从代码里读出来的，不是手写的：每个注入器都导出 `SupportedTypes()`，
而 `cmd/opskeeper-eval` 的测试逐条断言「注册表里的每一个类型都能路由回它自己」——
一份从不执行、也没人能查询的清单不叫清单。

**骨架**的意思是：这个注入器不碰任何真实系统，它通过 `CheckAvailable`
自报不可用并说明缺哪个客户端。`inject` 因此以非零退出，逐条打出没执行的原因，
**不会打印任何"注入成功"**。

**真实现**的意思是：它真的连库、真的改数据，而每一种故障都能从数据库外面看见：

| 故障 | 判据（从另一条连接上查） |
|---|---|
| `pg.inject_lock_chain` | `pg_stat_activity` 里 `wait_event_type='Lock'` 的 backend 数 > 0 |
| `pg.inject_table_bloat` | `pg_stat_user_tables.n_dead_tup > 0` |
| `redis.inject_big_key` | `MEMORY USAGE <key>` ≥ 写入量的一半 |
| `redis.inject_hot_key` | `INFO commandstats` 里 `cmdstat_get` 的 calls 增量；`CLIENT LIST` 里有具名连接 |
| `redis.inject_memory_burst` | `INFO memory` 的 `used_memory` 前后差值 > 0 |
| `redis.inject_slow_commands` | 一条**没被碰过**的连接的 PING 耗时 ≥ 暂停时长 |
| `host.fill_disk` | `statfs` 的可用字节前后差值；撤销后至少还回九成 |
| `host.cpu_stress` | `getrusage` 的 CPU 时间增量 ≥ 0.5 CPU 秒；利用率按 worker 归一后不低于 `target_load - 25` 个百分点。采样**到验收线为止、上限 10 秒**（决策 306） |
| `kafka.inject_consumer_lag` | `OffsetFetch` 的 committed 与 `ListOffsets` 的 latest 之差 ≈ `产出量 × (produce_rate−consume_rate)/produce_rate`（±15%） |
| `kafka.inject_partition_skew` | 每个分区 `Last − First` 的记录条数；最忙分区 ≥ 次忙 `skew_factor` 倍，且落在 `target_partition` |
| `k8s.cordon_node` | `node.spec.unschedulable == true`；撤销后为 `false`。**已经 cordon 的节点直接拒绝** |
| `k8s.inject_memory_pressure` | `node.status.conditions` 里出现 `MemoryPressure=True`（这正是 kubelet 做的事）；撤销只摘掉自己加的那一条 |
| `rabbitmq.inject_message_burst` | 独立连接上 `QueueInspect` 的深度**精确等于** `message_count − 1`（那一条被取出来量了字节数）；取出的那条 `len(Body) == message_size_bytes` |

连接来自 `OPSKEEPER_HARNESS_PG_DSN` 与
`OPSKEEPER_HARNESS_REDIS_ADDR`（口令走 `OPSKEEPER_HARNESS_REDIS_PASSWORD`）；
`host` 来自 `OPSKEEPER_HARNESS_HOST_ROOT`；
`kafka` 来自 `OPSKEEPER_HARNESS_KAFKA_BROKERS`（逗号分隔的 `host:port`）；
`rabbitmq` 来自 `OPSKEEPER_HARNESS_RABBITMQ_URL`（完整 AMQP URL，
**口令不许出现在任何一条会被打印出来的错误里**——错误里只回 host）。
`k8s` 来自 `KUBECONFIG`（或注入器显式指定的路径）；**同样没设就一步都不走**。

**没设就一步都不走**——`host` 尤其不猜：在节点 agent 上，
任何形式的默认目录都极可能就是节点的根文件系统。

#### 两条会读"整机"的判据：先证明那个数是静的

`host` 这一族里有两条判据量的不是自己造的东西，而是**整台机器 / 整个卷**：
`cpu_stress` 量的是 CPU 时间占整机核数的比例，`fill_disk` 量的是 `statfs` 的空闲字节。
这两条在一个"旁边还有别人在忙"的机器上会假红，而红的方式很像真的——
**这不是阈值不对，是那个数在那半秒里根本不属于我们。**

| 现象 | 处理 |
|---|---|
| **瞬时**繁忙（另一个测试进程在并行跑、CI 作业刚起步） | `cpu_stress` **等**：采样到验收线满足为止，上限 10 秒。1 秒是下限不是上限 |
| **持续**饱和（这台机器本来就被占着） | **大声拒绝**，包成 `ErrMachineBusy`（`ErrUnavailable` 的下位）。`"CPU 打满"`归因不到我们身上，多等也等不出来 |
| 别的进程在写同一个卷 | `fill_disk` 先连采三次：样本之间动得超过两个块就**跳过**，并把测到的差值写进跳过理由 |

`ErrMachineBusy` 与 `ErrUnavailable` 分开是有用的，因为**"这台机器此刻太忙"
和"注入器坏了"要的后续动作正好相反**：前者换台机器重试，后者去修代码。
两者都报成"注入失败"，会把人引去查一份没问题的代码。

**"没被碰过的连接"这五个字是慢命令那一条的全部要害**：它是四种 Redis 故障里
唯一一种影响所有人的，所以它不能靠注入器自证——说"我已经暂停了"没有任何意义，
要看旁观者的时钟。

### 4.1.1 Kafka 侧为什么有一半类型是拒绝而不是实现

Kafka 的故障内容是**记录**，而记录删不掉。所以这一组的设计不是"怎么造故障"，
而是"哪些故障造了还能收回来"：

| 注入类型 | 可逆性 | 处置 |
|---|---|---|
| `inject_consumer_lag` | **完全可逆**：lag = `latest − committed`，把 committed 提交到头就抹平了，一条记录都不少 | 真实现，且**允许**打在已经存在的 topic 上 |
| `inject_partition_skew` | **不可逆**：分布本身就是故障，而记录无法删除 | 真实现，但只打在**注入器自己建的** topic 上；case 点名的 topic 只是个人类可读的标签，一条记录都不会多 |
| `kill_broker` | **不可逆**，且 Kafka 没有"停用单个 broker"的管理调用，从 broker 内部也没有任何办法把它启回来 | **大声拒绝**，并在拒绝理由里说清是设计如此而不是没实现 |

`kill_broker` 那条 `mq/broker-down` case 因此跑不起来——这是有意的：
注入器宁可交一份"没做"也不交一份"做了但撤不回来"。

**两个实现都有一个共同的坑，已在代码里钉死**：kafka-go 的 `Writer` 默认
`RequiredAcks: RequireNone`（fire-and-forget），`WriteMessages` 返回 nil **不等于**
记录落盘——实测 400 条会稳定少 28 条而错误为 nil。所以两条注入的判据分母都取自
**现读的 offset**，不是"我请求写了几条"，并且少写超过 10% 直接判失败。

### 4.1.2 RabbitMQ 侧：可逆的是一个队列，不是一条消息

RabbitMQ 的故障内容是**消息**，而消息也删不掉——但它与 Kafka 的差别恰好给了
撤销一个出口：`QueueDelete` 一次调用就干净了。**代价是这个出口只对"自己的"
队列安全**：一条已经存在的队列里可能有运维真正的积压，把它删掉不是撤销故障，
是"把故障连同它掩盖的东西一起处理了"。所以 `inject_message_burst` 打在注入器
自己建的队列上，case 点名的 `queue` 只是个人类可读的标签——与 redis 的
`key_prefix`（决策 298）、kafka 的 `topic`（决策 300）同一个形状。

**这个 broker 上也有一个与 kafka 完全同类的坑，而且这次是写之前就知道的**：
`Channel.Publish` 不开 publisher confirm 时，帧写进 socket 就返回 nil，
broker 还在收——"发完了"是一个没人验证过的说法。所以每一条 publish 都等一个
confirm，分批等（每批 200）以免在 channel 上堆一个十万深的缓冲，而确认的**总数**
一个不少。判据的深度取自**另一条连接**的 `QueueInspect`。

还有两个协议层的坑，都写在代码注释里：

- **`QueueDeclarePassive` 的 404 会关掉整条 channel。** 所以"先 passive 探测、
  再在同一条 channel 上声明"在协议层就走不通，第二句会拿到
  `Exception (504) Reason: "channel/connection is not open"`——而那句话看起来
  像 broker 挂了，不像协议规定。探测与声明必须各开一条连接。
- **`QueueDeclare` 对一条已存在的 durable 队列是幂等的、不报错**，所以它答不了
  "这条队列是不是我的"。存在性必须单独问。

判据是**精确等于**而不是"至少"，前提是**先量内容再量数量**：判据要取一条消息
出来量它自己的字节数（深度对得上而全是空 body 是一种真的可能的故障），而取出来
的那条就不在队列里了。反过来做就得在判据里减一——那是一个要靠注释维持的常数。

### 4.2 环境限制

- **staging / dev**：默认允许
- **prod**：`--confirm-prod` **必要但不充分**，还必须带一条双人审批记录
  `--approval <record.json>`（决策 303，见 4.3）。闸门有三条：审批人必须**不是**
  运行命令的人、记录必须**绑定到这一个 case 与这一个环境**、签名必须由**审批人
  自己的密钥**验得出。没有操作者身份（`OPSKEEPER_HARNESS_OPERATOR`）时闸门是
  **关着**的，不是开的
- **注入时间窗**：**有**（决策 302）。`--max-duration` 是单个故障的时间窗上限，
  staging 默认 30m、prod 默认 10m；`--hold` 与 case 自带的 `duration` 任一超过就
  **在碰目标环境之前**拒绝执行。它**拒绝而不截断**

### 4.4 `host` 的三道闸门

`host` 是六个里**最危险**的：`fill_disk` 往真实文件系统写真实块，
做砸了会把 agent 自己所在的那台机器写瘫，而一台瘫掉的机器没有回滚。
所以这个包的设计由这一条决定，多出三道别的四个不需要的闸门：

1. **只往一个被明确指定的目录里写。** `OPSKEEPER_HARNESS_HOST_ROOT`
   指哪个就是哪个，不设就是不可用（见上）。
2. **文件系统根目录被拒绝，哪怕是被显式指定进来的。** 一道
   `root == "/"` 的检查，位置在 `CheckAvailable` 而不是 `Inject`——
   因为 `Inject` 的失败会被当成"环境不支持"，而这一次失败的原因是
   一个配错了的变量。
3. **不越过可用空间地板。** 写之前查一次，每写 1MB 再查一次。
   地板默认 2048MB：一个还在跑的 PostgreSQL / Redis 在磁盘被写满时
   会丢数据或崩溃，而那正是注入之后要诊断的东西。

case 里的 `path` **只能收窄范围，不能扩大**。语料是手写的 YAML，
而一个能被 case 指到任意目录的"磁盘写满"注入器就是一个能把节点写瘫的工具。
落在 root 之外的写法不是被忽略，而是被明确拒绝并说出该配什么。

### 4.3 自动清理

`InjectSpec.Duration` 现在**第一次被读到**（决策 297）：pg 注入器在它到期时
自己撤销——关掉攥着锁的连接、删掉自己建的表、删掉自己播种的行、还原 autovacuum。
撤销步骤按注册顺序记下、**逆序执行**，因为后注入的那一步往往依赖先注入的那一步。

故障同时受两重约束：`Duration` 到期自动撤销，以及 `Cleanup` 显式撤销
（命令行用 `--hold` 走的是后一条）。任一步失败都会**大声报出来**，
而不是把一次失败的撤销当成成功。

注入器**不碰**不是自己建的表：表名在 case 里，而 case 是可以手写的 YAML。
表在就直接用；表不在才建，**并且在撤销时只删自己建的那一张**
（靠表上的 marker 认领，不靠表名——万一有人在注入之后建了同名表，
按表名删就是在删别人的东西）。表名走标识符校验，不是标识符直接拒绝。

---

### 4.1.3 K8s 侧：真 API server，但没有真 kubelet

Kubernetes 这一组能走到"真实现"，靠的不是 minikube，而是 **kwok**：
它起的是真的 etcd、真的 kube-apiserver、真的 controller/scheduler，
但节点是**假的**——没有 kubelet 跑在上面。这条路的价值恰恰在于它把两侧分开了：

| 故障内容由谁产生 | 类型 | 处置 |
|---|---|---|
| **API server 存的就是故障本身** | `cordon_node`（`spec.unschedulable=true`）、`inject_memory_pressure`（`status.conditions` 里加一条） | ✅ 真实现，撤销就是 patch 回原值 |
| **由真 kubelet 跑出来** | `set_bad_image`（可观测信号是 `ImagePullBackOff`）、`fill_pv`（卷被写满是数据面的事） | ⚠️ 大声拒绝 |

`set_bad_image` 的拒绝理由值得写全：改 Deployment 的 image 字段**确实**能让它不再指向旧镜像，
但那验证的是"我刚写的字符串还在"——真正的故障信号 `ImagePullBackOff` 只有真 kubelet 拉镜像失败才会出现。
**照做一半再报成功是撒谎**，所以拒绝。

**cordon 不是断网**。`cordon_node` 遇 `simulate_network_partition: true`
（`k8s/node-notready` case 正是这样标参数）会**大声拒绝**：
cordon 只是让调度器不再往这个节点派新 Pod，节点上已有的 Pod 一个都不动，网络照通。
照做一半再报成功，等于给 case 盖了一个假的"节点失联"。

**两个只对"自己的"东西才安全的类型**，与 redis 的 `key_prefix`、kafka 的 `topic`、
rabbitmq 的 `queue` 是同一个形状的第四次出现：

- `cordon_node`：**已经**是 `unschedulable` 的节点直接拒绝——撤销会 `patch false`，
  等于替运维关掉一个他正在用的维护窗口。
- `inject_memory_pressure`：撤销只摘掉**自己加的那一条** condition，
  别的 condition 一条都不动。

#### 三个撞见的真 bug

1. **Node 没有 `spec` 子资源**。`spec` 是 Pod 才有的。带着 subresource 去 patch 会打到
   `/api/v1/nodes/<name>/spec`，API server 回 404 `the server could not find the requested
   resource`——一句完全看不出是路径写错的话。
2. **status 子资源是争用的**。node controller 一直在写它，读-改-写会周期撞
   `Operation cannot be fulfilled on nodes "...": the object has been modified`
   （`-count=3` 才稳定逮到）。修法：`patchNodeStatus` 走 `retry.RetryOnConflict`。
   这个 Conflict 还制造了**第二个**故障：撤销失败后条件留在节点上，下一次运行读到
   "这个节点已经报着内存压力"并按归属规则拒绝——**一个自造的、假的"这不是我们的节点"**。
3. **旧的骨架测试自己变成了注入器**。`TestInjector_AllSupportedTypesAreRefusedAlike`
   原本调 `New()`，而 `New()` 会读 `KUBECONFIG`；测试进程里设了那个环境变量之后，
   这条**专门用来证明"没有东西被注入"**的测试真的往集群上打了一次 memory pressure，
   留下的条件又造成了后来那个"已经有 MemoryPressure"的怪失败。
   现在全部改成 `New(WithKubeconfig(""))` 把状态钉死——
   **一条想证明"什么都没发生"的测试，必须先保证它确实什么都没做。**

## 五、Judge 模型

### 5.1 默认配置：双模型取均值

| 模型 | 用途 | 备注 |
|---|---|---|
| Claude Sonnet 4 | 主评分 | 中文 / 代码 / 推理强 |
| GPT-4o | 副评分 | 通用推理 / 多语言 |

### 5.2 评分维度

`judge.Score.Dimensions` 里的每个维度都是 0-1。**四个过程维度**：

1. **rca_accuracy** — 根因符号命中比例（`AgentResponse.RootCause` vs `expect.root_cause_lines`）
2. **time_efficiency** — 检测时长 vs `expect.time_to_detect`（不超时满分，3 倍线性衰减到 0）
3. **remediation_quality** — 修复动作命中比例
4. **collateral_safety** — `rubric.no_collateral_damage` 为真且响应带 errors → 0，否则 1

**三个诊断轴**（arXiv:2606.29193，见 §二.7）：

5. **localization** — 结论有没有说出故障所在的资源（case 的族 + 注入参数身份值）
6. **identification** — 结论有没有说出故障的类型（case id 的故障段分词）
7. **reason** — **tool call 轨迹**里有没有语料要求的那些观测

`Overall` 仍是前四个的加权（0.4 / 0.2 / 0.3 / 0.1）；三个轴不参与它，但
`Overall >= 0.7` 且 `reason <= 0.5` 会把这次评分 `Flagged` 进人工复评。
**case 没声明的轴不出现在 `Dimensions` 里**——缺省表示"没测过"，不是 0 分。

### 5.3 一致性校验

两模型评分差异 > 0.2 时标记为 `Flagged`，进入人工 rubric 复评队列。一致率目标：

```
一致率（差异 < 0.1）>= 80%
```

详见 [ADR：judge 模型选型](superpowers/decisions/2026-07-13-harness-judge-models.md)。

### 5.4 缓存 + 增量

- **缓存**：相同 `(case_id, agent_response_hash)` 复用评分结果
- **增量**：PR 修改的 case 跑全量，其余 case 复用上次结果
- **限速**：每分钟最多 60 次 judge 调用

### 5.5 评分降级

若双模型一致率持续 < 70%（持续 4 周），降级到单模型（仅 Claude Sonnet 4）。详见 ADR "回滚条件"。

---

## 六、Leaderboard 与回归基线

### 6.1 Leaderboard 查看

```bash
opskeeper-eval leaderboard
```

输出示例：

```
Harness Leaderboard — 最近 30 天
┌──────────────────────────────┬─────────┬────────┬──────────┐
│ Case                         │ Score   │ Δ vs   │ Status   │
│                              │ (avg)   │ base   │          │
├──────────────────────────────┼─────────┼────────┼──────────┤
│ pg/long-running-tx           │ 0.92    │ +0.02  │ ✅ pass   │
│ redis/big-key                │ 0.88    │ -0.03  │ ⚠ warn  │
│ k8s/pod-oom                  │ 0.85    │ -0.08  │ ❌ fail   │
│ host/disk-full               │ 0.94    │ +0.01  │ ✅ pass   │
└──────────────────────────────┴─────────┴────────┴──────────┘
Overall: 0.90 (baseline 0.91, Δ -0.01)
```

### 6.2 回归基线规则

| 评分下降幅度 | 行为 |
|---|---|
| < 5% | 静默（log only） |
| 5%-15% | **告警**（Slack / 钉钉） |
| > 15% | **CI 阻断**（merge 拒绝） |

### 6.3 基线更新

```bash
# 把当前分数锁成新基线（每月一次，基线文件建议提交进仓库）
opskeeper-eval leaderboard --lock-baseline

# 查看基线表
opskeeper-eval leaderboard --baselines

# CI：对照基线检查回归，有 block 时非零退出
opskeeper-eval leaderboard --check-regression
```

基线落在 `harness/result/baseline.json`（`--baseline-file` 可改），记下锁定时间、
锁定人（`GIT_AUTHOR_NAME` 或 `USER`）、聚合口径，以及每个 case 的分数。
**聚合口径是 `rca_accuracy` / `approval_rate` / `recovery_pass_rate` /
`kb_hit_rate` 四个已测指标的均值**——未测量的指标被跳过而不是当 0，
所以"只跑了 rca 的 case"不会看起来掉了一半。

退出码：出现 **block**（下降 >= 15%）非零；**warn**（5%-15%）默认放过，
加 `--fail-on-warn` 才拦。

**基线尚未锁定时 `--check-regression` 直接非零退出**，不会报"零回归"。
一个这次没跑的 case、一个从未锁过的新 case、一个一个指标都没测出来的 case，
三者都被单列出来（`missing` / `new` / `unmeasured`）——它们都不是"没退步"，
混成一句"无回归"就等于让一次漏跑通过了一次回归检查。
---

## 七、CI 集成

### 7.1 GitHub Actions 示例

```yaml
# .github/workflows/harness.yml
name: Harness Eval
on:
  pull_request:
    paths:
      - 'core/manager/**'
      - 'core/harness/**'
      - 'cmd/opskeeper-eval/**'

jobs:
  eval:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26'
      - name: Build opskeeper-eval
        run: go build -o opskeeper-eval ./cmd/opskeeper-eval
      - name: Run harness suite
        env:
          OPSKEEPER_LLM_ANTHROPIC_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
          OPSKEEPER_LLM_OPENAI_KEY: ${{ secrets.OPENAI_API_KEY }}
        run: |
          ./opskeeper-eval run --suite pr-baseline --env staging --output eval-report.json
      - name: Check regression
        # 基线文件提交进仓库；下降 >= 15% 阻断，5%-15% 只告警。
        # 基线文件不存在时这条命令非零退出，而不是报"零回归"。
        run: |
          ./opskeeper-eval leaderboard --check-regression
      - name: Upload report
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: harness-report
          path: eval-report.json
```

### 7.2 REST API（CI 集成）

**未交付。** 这个接口不存在：本仓库里没有任何路由注册
`/api/v1/harness/runs`（`make apidoc-check` 会报出它）。Harness 目前只有 CLI，
CI 集成走 7.1 的本地进程，不走 HTTP。未来如果要这个接口，
需要在 `core/manager` 里真正增加路由与 handler，而不是只在文档里补一段。

详见 [docs/api/harness.md](api/harness.md)。

---

## 八、生产保护

### 8.1 Prod 环境注入拦截

`inject` 现在读真实的 case 文件、走真实的注入器注册表路由，
并按注入器自己的 `CheckAvailable` 决定能不能注入。

**没配 DSN 时，注入一步都不走，而且是**大声地失败**：**

```bash
$ opskeeper-eval inject --case pg/lock-waits
inject: case=pg/lock-waits env=staging steps=1

inject: 以下 1 步没有执行：
  step 1  pg.inject_lock_chain             action=inject_lock_chain        duration=3m0s
      injector: not available in current env: 没有配置 PostgreSQL 连接（设 OPSKEEPER_HARNESS_PG_DSN，或用 WithDSN / WithPool 传进来）
error: inject: 1 of 1 step(s) not executed
```

配了 DSN，它就**真的**注入——所以故障必须被按住：

```bash
$ export OPSKEEPER_HARNESS_PG_DSN='postgres://opskeeper:opskeeper@127.0.0.1:5432/opskeeper?sslmode=disable'
$ opskeeper-eval inject --case pg/lock-waits --hold 3m
inject: case=pg/lock-waits env=staging steps=1
  step 1  pg.inject_lock_chain             action=inject_lock_chain        duration=3m0s  inject_id=pg-inj-...
inject: 按住 1 步故障 3m0s（Ctrl-C 提前结束并撤销）
```

此时从另一条连接上查得到故障真的在：

```sql
SELECT count(*) FROM pg_stat_activity
 WHERE wait_event_type = 'Lock' AND application_name LIKE 'chain-%';  -- 3
```

不给 `--hold` 时命令会明确提示：故障活在本进程的连接上，进程退出即撤销。
**这不是可以省略的一步**——一次在诊断开始之前就自己好了的故障，
它的诊断结论是关于空气的。

prod 的拦截是真的，且与注入器是否接线无关：

```bash
$ opskeeper-eval inject --case pg/lock-waits --env prod
error: refusing to inject in prod without --confirm-prod
```

### 8.1.1 双人审批（决策 303）

`--confirm-prod` 是一个布尔开关，而**一个布尔开关的签发者与检查者是同一个人**：
任何能敲这行命令的人都能自己确认自己。审批记录由**审批人自己**签：

```bash
# 审批人（bob）在自己那一侧签，密钥只有他有
$ opskeeper-eval approve --case pg/lock-waits --env prod \
      --request-by alice --approve-as bob --out ok.json

# 发起人（alice）拿着这条记录注入
$ OPSKEEPER_HARNESS_OPERATOR=alice opskeeper-eval inject \
      --case pg/lock-waits --env prod --confirm-prod \
      --approval ok.json --approval-keys ./approval-keys
```

密钥来自 `--approval-keys/<identity>.key`（或 `OPSKEEPER_HARNESS_APPROVAL_KEYS`）。
它**必须是审批人自己持有的东西**——如果发起人也持有对方的密钥，
这道闸门又变回布尔开关了。

四条拒绝，每一条都有测试：

| 拒绝 | 为什么 |
|---|---|
| 不知道操作者是谁 | 没法判断审批人是不是第二个人。**缺身份时闸门是关着的** |
| `approved_by` == 操作者 | 全部要害：一个人既发起又批准 |
| 记录不是这个 case / 这个环境 | 没绑定的批条批的是所有东西 |
| 改了签过名的字段 | HMAC 对不上；而且它撞的是**审批人自己**的密钥，挪一个名字就暴露 |

`--approval-max-age`（默认 1h）**压过记录自己写的 `expires_at`**：
一条写着"有效期到明年"的记录与一条写着"一小时"的记录，在没有上限时是同一样东西。
这与 §8.2 的时间窗是同一个形状——默认值可以被显式抬高，
但抬高是一次被记下来的决定。

**时间窗和双人审批不能互相替代**：10 分钟的双人审批仍然可以是 10 分钟的破坏。

### 8.2 时间窗限制

**已实现**（决策 302）。`inject --max-duration` 是**单个故障**能活多久的上限，
默认 staging 30m、prod 10m；它查两样：`--hold`（人随手敲的数）与 case 自带的
`duration`（注入器自己的自撤销时限）。

三条设计决定值得写下来，因为每一条都在替一个具体的坏处：

**闸门在碰目标环境之前。** 它只需要读 case 文件与两个 flag，所以它先跑。
先注入再拒绝等于"报了错但环境已经被改了"——而一个刚被写满的磁盘不会因为
返回码非零就自己恢复。这条有测试：判据是输出里**一个 `inject_id` 都没有**，
连 case 头都没打。

**拒绝而不截断。** 截断看起来更"好用"，但它会让一条 600s 的 case 悄悄变成
一个 300s 的故障，而 harness 会拿那个 300s 的结果去判一份诊断结论——
报告出来的时间与真实发生的时间不一样。这比直接报错坏得多，
而它是那种没人会发现的坏。

**它查的是"故障真正活着的时间"**，也就是进程跑着的时候 `max(duration, hold)`。
这一条在决策 300/301 之后才真正要紧：pg 的锁链活在进程里，进程没了就没了，
但 `host.fill_disk` 写在真磁盘上的块、kafka 与 rabbitmq 里的记录与队列
**活过进程本身**。一个没有上限的注入器，在一台运维机上就是一个
"忘了关的定时炸弹"。

闸门是**默认**而不是**强制**：`--max-duration` 可以被显式抬高（抬高是一次
明确的决定），而语料里每一条 case 都必须落在默认窗口之内——
`TestEveryShippedCaseFitsInsideTheDefaultTimeWindow` 读的是真的 case 目录，
有人加一条 2 小时的 case 它就红。

`--dry-run` 仍然不假装任何事：

```bash
$ opskeeper-eval inject --case pg/lock-waits --dry-run
inject: case=pg/lock-waits env=staging steps=1
  step 1  pg.inject_lock_chain             action=inject_lock_chain        duration=3m0s
```

### 8.3 审计

所有 inject / run / judge 操作必审计：

```bash
logcli query '{app="opskeeper-eval"} |= "inject"' --since=24h
```

---

## 九、Case 库扩展

### 9.1 贡献流程

1. 在 `core/harness/cases/<resource>/<new-case>/case.yaml` 写新 case
2. `opskeeper-eval vocabulary --cases-dir core/harness/cases` 校验 schema（没有 `validate` 子命令；加载与校验在 schema.Loader 里，任一依赖它的子命令都会拒绝一个不合 schema 的 case）
3. 在 staging 跑一次：`opskeeper-eval run --case <new-case> --env staging`
4. PR review + 合并
5. 纳入下月回归基线

### 9.2 案例库覆盖目标（v1.0）

| 资源 | 当前 | 目标 | 缺口 |
|---|---|---|---|
| PG | 6 | 20 | 14（真空闲事务 / 慢查询 / 复制延迟 / vacuum stuck / 索引膨胀 / autovacuum 失效 / etc.）|
| Redis | 4 | 12 | 8 |
| MQ | 4 | 10 | 6 |
| K8s | 4 | 12 | 8 |
| Host | 2 | 6 | 4 |
| **总计** | **20** | **60** | **40** |

每 case 估时 2-3 天（含开发 + review + 跑测）。

---

## 十、相关文档

- ADR：[docs/superpowers/decisions/2026-07-13-harness-judge-models.md](superpowers/decisions/2026-07-13-harness-judge-models.md)
- Spec：[openspec/specs/harness-eval-platform/spec.md](../openspec/specs/harness-eval-platform/spec.md)
- 集成指南：[docs/integration-guide.md](integration-guide.md)
- 运维手册：[docs/operations-manual.md](operations-manual.md)
- API 文档：[docs/api/harness.md](api/harness.md)


---

## 二.7、闭环投影（project）：让 judge 评的是系统真产出的东西

**问题**：judge 评的是 `judge.AgentResponse`，生产写的是 `RootCauseJSON`，两者之间
**什么都没有**。于是历史上每一个分数都来自手写的响应文件，golden 语料从来没有被
真正考核过——它只被一份人手写的东西考核过。

**三套词表在这里相遇，只有两套能对上**：

| 来源 | 形状 | 能否直接映射 |
|---|---|---|
| `remediation_options[].action` | `pg.terminate_long_tx` | ✅ 与 case 同构 |
| `root_cause_object.kind` | `pg_lock`（闭集 enum） | ❌ 另一套 namespace |
| `evidence_chain[].tool` | `query_promql`（裸名、跨族） | ⚠️ 只能作为 tool call 供 LLM judge 推理，不参与精确匹配 |

**根因那一列是刻意留空的。** 投影包 `core/harness/projection` 接受一个
`Resolver`，由调用方通过 `--kind-map` 提供：

```bash
opskeeper-eval project --contract rc.json \
  --kind-map docs/kind-map.example.json \
  --detected-at 2026-10-01T09:59:19Z \
  --investigated-at 2026-10-01T10:00:00Z \
  --recovered-at 2026-10-01T10:01:28Z \
  --bare --out resp.json
```

- **kind 映射是数据，不是代码分支**。"pg_lock 和 pg.lock_waits 是同一个发现"是一次
  关于**语义**的判断，只有懂这个领域的人能做；写成代码分支会让这个判断永远隐形。
  写成 JSON，它可以被 review、diff、签字。
- **没有映射就拒绝出响应**（除非 `--allow-unmapped-root-cause`）。输出一个
  `root_cause_matched` 为空的响应，judge 会打 0 分并读成"这次诊断什么都没找到"——
  那是一次凭空捏造的判决。
- **`--bare` 输出裸响应**，因为 `judge --response` 已经用严格解码读
  `judge.AgentResponse`；再套一层外壳等于让 judge 为同一个类型学第二套 schema。
- **映射文件会被校验**：`vocabulary --kind-map` 会检查每一条映射指向的符号是否真的
  有人产出。映射是数据，工具改名之后它会静默指向一个不存在的名字，然后每一次
  经过它的运行都在 `remediation_quality` 上打 0 而**任何地方都不报错**。仓库里的
  `docs/kind-map.example.json` 由一条测试守着（`TestTheShippedExampleKindMapHasNoStaleEntries`），
  所以它是被维护的产物而不是会腐烂的文档。

**端到端实测**（真实契约，`pg_lock` → 映射到 `pg/lock-waits` 的期望根因）：

```
$ opskeeper-eval project --contract rc.json --kind-map kinds.json --bare --out resp.json
project: kind=pg_lock root_cause=[pg.lock_waits pg.active_sessions]
         remediations=[pg.terminate_long_tx pg.kill_backend] tool_calls=2 → resp.json

$ opskeeper-eval judge --case pg/lock-waits --response resp.json
  rca_accuracy         1     ← kind 映射生效
  remediation_quality  0     ← 闭环提的是 pg.kill_backend，case 期望 pg.kill_session
  time_efficiency      1     ← 41s/60s、88s/120s
  overall              0.70
```

那个 0 是**这一整轮最有价值的输出**：它把"闭环提不出语料期望的修复动作"这件一直
看不见的事，变成了一个可以指着数字讨论的事实。

**防锈**：`projection.Doc` 是 `loop.RootCauseJSON` 的手写镜像（评测面不得依赖控制面
实现，这是 `scripts/modulecheck` 的模块规则）。镜像会烂——契约加字段，两边都还能编译，
投影悄悄少投一层。所以有两条双向测试：真实契约过线格式进镜像（**严格解码**，未知字段
即失败），镜像写出的东西再读回控制面类型。变异验证：给 `loop.RootCauseObject` 加一个
字段 → `TestTheMirrorStillMatchesTheContract` 精确报错。

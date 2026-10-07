# Harness 评测平台：CLI 面（不是 REST）

> **范围**：golden case 执行、fault 注入、judge 评分、三诊断轴、leaderboard 回归、能力词表。
> **真实形态**：`cmd/opskeeper-eval` 一个二进制 + `core/harness` 九个库包。**本平台没有 HTTP 服务。**
> **本文描述的是代码当前实际行为**，含「未交付」一节（文末）。曾经有一份 REST 文档描述 13 个端点，**那 13 个端点从未存在**（见文末）。
> **形状守卫**：`make apidoc-check`（`scripts/apidoc`）保证本文不会声称一个不存在的端点或子命令；本文的每个子命令都在 `cmd/opskeeper-eval/main.go` 的 `case` 里被逐一核对。

---

## 一、为什么是 CLI

评测平台跑的是**注入真实故障、驱动真实 agent、再打分**这件事。它的调用者是 CI 与人，
不是浏览器。它需要的是可复现的命令行、机器可读的 JSON 输出、以及能在没有 HTTP 客户端的
地方跑（节点侧、离线机器）。**曾经按 REST 写的那份文档描述的是一个不存在的服务**——
`docs/api/` 下曾经有十三行 `GET /api/v1/harness/...`，仓库里没有任何一处注册它们。

---

## 二、二进制

```bash
go run ./cmd/opskeeper-eval <subcommand> [flags]
opskeeper-eval --version
opskeeper-eval <subcommand> --help     # 每个子命令的 flag 列表
```

退出码：`0` 成功；`1` 命令返回错误（错误信息在 stderr）；`2` 未知子命令。

---

## 三、子命令

### 3.1 `run` — 执行 case 或 suite

| flag | 作用 |
|---|---|
| `-case` | 单个 case id，例如 `pg/long-running-tx` |
| `-suite` | suite 名 |
| `-env` | 执行环境（默认 staging） |
| `-judge-model` | 评分模型 |
| `-concurrency` | 并发数 |
| `-output` / `-report-dir` | 输出文件 / 报告目录 |

```bash
opskeeper-eval run --case pg/long-running-tx --env staging
opskeeper-eval run --suite middleware-baseline --concurrency 4
```

### 3.2 `inject` — 手动触发故障注入

| flag | 作用 |
|---|---|
| `--case` | case id |
| `--cases-dir` | golden case 目录（默认 `core/harness/cases`） |
| `--target` | 目标（`ns=test deploy=order-svc` 形式，空格分隔的 `key=value`） |
| `--env` | 目标环境，默认 `staging` |
| `--confirm-prod` | 允许对 `prod` 注入。**没有它就注入不了生产**，这是一个必须显式写出来的开关 |
| `--dry-run` | 读真实的 case、走真实的路由，列出这个 case 会注入什么；不注入 |

```bash
# 列出 k8s/pod-oom 会注入什么（这一步现在真的能跑通）
opskeeper-eval inject --case k8s/pod-oom --dry-run

# 真注入（需 KUBECONFIG 指向真 API server；未设则以非零退出并说明缺什么）
opskeeper-eval inject --case k8s/pod-oom --target ns=test deploy=order-svc

# 真注入并按住（pg 已有真实现；需要 OPSKEEPER_HARNESS_PG_DSN）
export OPSKEEPER_HARNESS_PG_DSN='postgres://opskeeper:opskeeper@127.0.0.1:5432/opskeeper?sslmode=disable'
opskeeper-eval inject --case pg/lock-waits --hold 3m
```

**`--hold` 不是可有可无的。** 一条锁链的"存在"就是那几条攥着行锁的连接，
连接属于进程：不给 `--hold`，进程一退出故障就撤销了，
而一次在诊断开始之前就自己好了的故障，诊断结论是关于空气的。

| `--hold` | 注入后把故障按住多久，然后逆序撤销（默认 `0` = 进程退出即撤销） |
| `--max-duration` | **单个故障的时间窗上限**（默认 staging 30m、prod 10m）。`--hold` 与 case 自带的 `duration` 任一超过就**在碰目标环境之前**拒绝执行；**拒绝而不截断**（决策 302） |
| `--approval` | 双人审批记录（JSON）。**prod 必填**；`--confirm-prod` 必要但不充分（决策 303） |
| `--approval-keys` | 审批密钥目录（默认 `OPSKEEPER_HARNESS_APPROVAL_KEYS`），里面有 `<identity>.key` |
| `--approval-max-age` | 一条审批记录最多算多新（默认 1h）。**压过记录自己写的 `expires_at`** |

`opskeeper-eval approve` 签一条记录（用审批人自己的密钥，不碰任何环境）：

| flag | 作用 |
|---|---|
| `--case` / `--env` | 这条审批**绑定到**哪一个 case、哪一个环境 |
| `--request-by` | 发起人（必须与运行时 `OPSKEEPER_HARNESS_OPERATOR` 一致） |
| `--approve-as` | 审批人；密钥从 `<keys-dir>/<identity>.key` 读。**与 `--request-by` 相同直接拒绝** |
| `--valid-for` | 有效期（默认 1h） |
| `--note` | 备注，会被签进记录——改一个字签名就失效 |
| `--out` | 记录写到哪（默认 stdout） |

**PostgreSQL 这一路是**真实现**（决策 297）。** `core/faults/injector/pg`
用 pgx 连真库，注入的每一种故障都能从数据库外面看见：

| 类型 | 故障在数据库里的样子 |
|---|---|
| `pg.inject_lock_chain` | 一排 backend 停在 `pg_stat_activity.wait_event_type='Lock'` |
| `pg.begin_txn_hold` / `pg.hold_old_txn` | backend 停在 `state='idle in transaction'` |
| `pg.run_slow_queries` | N 个 backend 在跑一条给定的长查询 |
| `pg.inject_table_bloat` | 提交后的死元组，`pg_stat_user_tables.n_dead_tup > 0` |
| `pg.run_autovacuum` | `autovacuum_enabled=false` **加上**一个压着 xmin 的长事务 |
| `pg.inject_replica_lag` | **大声拒绝**：单节点造不出复制延迟 |

连接来自 `OPSKEEPER_HARNESS_PG_DSN`。**没设就一步都不走**——不设的时候
`inject` 以非零退出，并把每一步没执行的原因逐条打出来，**不会打印任何"注入成功"**。

**Redis 这一路也是**真实现**（决策 298）。** `core/faults/injector/redis`
用 go-redis 连真库，四种故障的判据都是从旁观连接上读出来的量：

| 类型 | 判据 |
|---|---|
| `redis.inject_big_key` | `MEMORY USAGE` ≥ 写入量的一半 |
| `redis.inject_hot_key` | `INFO commandstats` 里 `cmdstat_get` 的 calls 增量 ≥ 客户端数 |
| `redis.inject_memory_burst` | `INFO memory` 的 `used_memory` 前后差值 > 0 |
| `redis.inject_slow_commands` | 一条没被碰过的连接的 PING 耗时 ≥ 暂停时长 |

连接来自 `OPSKEEPER_HARNESS_REDIS_ADDR`（口令走 `OPSKEEPER_HARNESS_REDIS_PASSWORD`）。
**没设就一步都不走。**

**它写的每一条 key 都带 injectID**（`opskeeper:fault:<injectID>:…`），
所以"只删自己建的 key"不需要撤销时再核对一次——**归属在命名那一刻就回答完了**。

**Host 这一路也是**真实现**（决策 299），而且它是六个里最危险的一个。**
`core/faults/injector/host` 真的写文件系统、真的烧 CPU：

| 类型 | 判据 |
|---|---|
| `host.fill_disk` | `statfs` 的可用字节前后差值；撤销后至少还回九成 |
| `host.cpu_stress` | `getrusage` 的 CPU 时间增量；利用率按 worker 归一后不低于 `target_load - 25` 个百分点 |

它多出三道别的四个不需要的闸门：**只往 `OPSKEEPER_HARNESS_HOST_ROOT`
指定的目录里写**（不设即不可用，不猜默认目录）；**文件系统根目录被拒绝**，
哪怕被显式指定；**不越过可用空间地板**（默认 2048MB，写前查一次、每写 1MB 再查一次）。
case 里的 `path` 只能收窄范围，落在 root 之外会被明确拒绝。

**Kafka 这一路也是真实现**（决策 300），连的是
`OPSKEEPER_HARNESS_KAFKA_BROKERS`（逗号分隔的 `host:port`）：

| 类型 | 判据 |
|---|---|
| `kafka.inject_consumer_lag` | `OffsetFetch` 的 committed 与 `ListOffsets` 的 latest 之差 ≈ `实际产出 × (produce_rate−consume_rate)/produce_rate`（±15%） |
| `kafka.inject_partition_skew` | 每个分区 `Last − First` 的记录条数；最忙分区 ≥ 次忙 `skew_factor` 倍，且落在 `target_partition` |

它与前三个的差别在于**故障的载体是记录，而记录删不掉**，所以撤销方式决定了
什么能造、什么不能造：`consumer_lag` 只提交 offset 不读消息，撤销就是把
committed 提交到头，因此**允许**打在已存在的 topic 上；`partition_skew` 的
分布本身就是故障，撤销只能是删掉整条 topic，因此只打在注入器自己建的 topic 上，
case 点名的 topic 只是个人类可读的标签。`kill_broker` **大声拒绝**——Kafka 没有
停用单个 broker 的管理调用，从 broker 内部也启不回来，造一个撤不回的故障是破坏。

因此 `mq/broker-down` 这条 case 跑不起来。这是有意的：注入器宁可交一份
"没做"，也不交一份"做了但收不回来"。case 本身没有被删——它还在语料里，
`CheckAvailable` / `Inject` 会在执行前就说明原因并以非零退出。

**RabbitMQ 这一路也是真实现**（决策 301），连的是
`OPSKEEPER_HARNESS_RABBITMQ_URL`：

| 类型 | 判据 |
|---|---|
| `rabbitmq.inject_message_burst` | 独立连接上 `QueueInspect` 的深度**精确等于** `message_count − 1`；取出的那条 `len(Body) == message_size_bytes` |

它与 Kafka 那一路是同一个形状的问题（故障内容是消息，消息删不掉），但 RabbitMQ
多给了一个出口：`QueueDelete` 一次就干净了——**代价是这个出口只对"自己的"队列
安全**，所以故障打在注入器自己建的队列上，case 点名的 `queue` 只是标签。

publish 这一步**开 publisher confirm**：不开的时候 `Publish` 只等帧写进 socket，
"发完了"是一个没人验证过的说法。这与 kafka 那边 `RequiredAcks` 默认
`RequireNone` 是同一个坑，这次是在写它的时候就知道的。

k8s 这一路连的是 `KUBECONFIG` 里的**真 API server**（判据见下），没设就一步都不走：

| 注入类型 | 判据（从 API server 另一条连接上读） |
|---|---|
| `k8s.cordon_node` | `node.spec.unschedulable == true`，撤销后为 `false`；已经 cordon 的节点直接拒绝 |
| `k8s.inject_memory_pressure` | `node.status.conditions` 里出现 `MemoryPressure=True`，撤销只摘掉自己加的那一条 |
| `k8s.set_bad_image` | **大声拒绝**：可观测信号 `ImagePullBackOff` 由真 kubelet 产生 |
| `k8s.fill_pv` | **大声拒绝**：API 里没有"填满一个卷"这个操作 |

`cordon_node` 遇 `simulate_network_partition: true` 也会拒绝——cordon 不是断网。
两个"只对自己安全"的理由见 `docs/harness-guide.md` §4.1.3。

一个认不出的类型报 `ErrUnsupportedType` 而不是"不可用"：那是接线问题，
与当前环境无关，报成不可用会把人引去查环境。

### 3.3 `judge` — 对已有响应重跑评分

| flag | 作用 |
|---|---|
| `-case` / `-cases-dir` | 单个 case / 语料目录 |
| `-response` | `judge.AgentResponse` 的 JSON（用 `project` 生成） |
| `-judge` | `heuristic` 或 `llm` |
| `-provider` / `-model` | LLM judge 的 provider 与模型 |
| `-out` | 输出 |
| `-plugins-dir` | 插件包目录（能力可服务性检查用） |
| `-allow-unservable` | 允许对本构建无法服务的 case 打分 |

```bash
opskeeper-eval judge --case pg/long-running-tx --response agent-response.json
opskeeper-eval judge --case pg/long-running-tx --response agent-response.json \
    --judge llm --provider anthropic
```

**`allow-unservable` 是一道诚实的开关**：一个 case 需要的工具本构建没有时，它的分数不是
agent 的成绩。不给这个开关就直接判不合格，等于把平台的失败算成 agent 的失败。

### 3.4 `run-loop` — loop 模式闭环

| flag | 作用 |
|---|---|
| `-case` / `-cases-dir` / `-env` | 同 `run` |
| `-execution-mode` | `dry-run` 或 `real-agentteams` |
| `-incident-id` / `-trace-id` | 真实模式的标识 |
| `-hitl-evidence` / `-mcp-evidence` / `-fixture-before-evidence` / `-fixture-after-evidence` | 六类证据文件 |
| `-judge` / `-judge-model` / `-judge-provider` | 评分路径 |

```bash
opskeeper-eval run-loop --case host/cpu-spike --execution-mode=real-agentteams \
  --incident-id host-cpu-spike-real --trace-id <32 hex> \
  --postmortem-evidence pm.json --judge llm
```

### 3.5 `leaderboard` — 排行榜与回归基线

| flag | 作用 |
|---|---|
| `--dir` | LoopResult JSON 目录（默认 `harness/result/loop`） |
| `--out-dir` | Markdown 报告输出目录 |
| `--threshold` | `recovery_pass_rate` 门槛，低于则 NOT QUALIFIED |
| `--baseline-file` | 回归基线文件（默认 `harness/result/baseline.json`） |
| `--lock-baseline` | 把当前分数写成本次基线 |
| `--baselines` | 打印基线表 |
| `--check-regression` | 对照基线检查回归；有 block 时非零退出 |
| `--fail-on-warn` | `--check-regression` 下 warn 也非零退出 |

### 3.6 `list-cases` — 列出语料

| flag | 作用 |
|---|---|
| `-cases-dir` | 语料目录（默认 `core/harness/cases`） |
| `-filter` | 按 id 片段过滤 |

```bash
opskeeper-eval list-cases --filter pg
```

### 3.7 `plugin-coverage` — case 能力期望 vs 插件包能力

| flag | 作用 |
|---|---|
| `-cases-dir` / `-plugins-dir` / `-filter` | 输入 |
| `-json` | 机器可读输出 |
| `-fail-on-gap` | 有缺口即非零退出 |
| `-fail-on-unrecorded-diagnose-gap` | 未登记的诊断缺口也非零退出 |

### 3.8 `vocabulary` — case 能力期望 vs 本构建真实词表

| flag | 作用 |
|---|---|
| `-cases-dir` / `-plugins-dir` / `-filter` / `-kind-map` | 输入 |
| `-json` / `-fail-on-gap` | 输出与门控 |

```bash
opskeeper-eval vocabulary
```

### 3.9 `project` — 把生产的 RootCauseJSON 投影成评分响应

| flag | 作用 |
|---|---|
| `-contract` | 生产的 RootCauseJSON 契约文件 |
| `-kind-map` | 故障类型映射 |
| `-out` | 输出 `judge.AgentResponse` |
| `-bare` | 只输出响应，不做打分 |
| `-allow-unmapped-root-cause` | 允许契约里出现映射表没有的根因类型 |
| `-detected-at` / `-investigated-at` / `-recovered-at` | 三个时间戳 |

```bash
opskeeper-eval project --contract rc.json --kind-map kinds.json --out resp.json
```

### 3.10 `axes` — 三个诊断轴的声明面

| flag | 作用 |
|---|---|
| `-cases-dir` / `-filter` | 输入 |
| `-json` | 机器可读输出 |
| `-fail-on-unmeasured-axis` | 有 case 三轴中任一无法测量即非零退出 |

```bash
opskeeper-eval axes --fail-on-unmeasured-axis
```

派生规则在 `core/harness/axes`（一份实现，两条评分路径共用），打分在
`core/harness/judge`，存储与对比在 `core/harness/leaderboard`。

---

## 四、CI 里的三道闸门

```bash
make eval-coverage      # plugin-coverage：哪些 case 没有插件能服务
make eval-vocabulary    # vocabulary：哪些 case 连结构上都无法满足
make eval-axes          # axes：哪些 case 没声明三轴
```

三者都是 `eval-gates` 的组成部分，且都在 CI 每次 push 跑到。**它们的输出不是分数，是缺口**：
一个 case 在这里红了，说明平台还不能公平地评它，而不是 agent 答错了。

---

## 五、未交付

以下内容在旧版本文里出现过，**代码从未实现**：

| 项 | 状态 | 缺什么 |
|---|---|---|
| `GET /api/v1/harness/cases`（列 case） | ❌ 从未实现 | 无此端点；用 `list-cases` |
| `POST /api/v1/harness/cases/validate` | ❌ 从未实现 | 无此端点；`schema.NewLoader` 在进程内校验 |
| `GET/POST /api/v1/harness/runs`、`/runs/{run_id}` | ❌ 从未实现 | 无运行记录服务；结果落文件与 leaderboard |
| `POST /api/v1/harness/inject`、`/inject/{inject_id}/stop` | ❌ 从未实现 | 注入是 `inject` 子命令 |
| `GET /api/v1/harness/leaderboard`、`POST .../lock`、`POST .../check-regression` | ❌ 从未实现 | 排行榜是 `leaderboard` 子命令 |
| `GET /api/v1/harness/judge/models`、`POST .../judge`、`GET .../judge/consistency` | ❌ 从未实现 | 评分是 `judge` 子命令 |
| 响应信封 `{code, message, data}` | ❌ 不适用 | CLI 输出是文本或 `--json` 的结构 |

**「双模型 judge 一致性」（judge/consistency）是一个真想法，但不是已交付的端点**：现在能做的
是同一响应分别用 `heuristic` 与 `llm` 打两次并人工比对，命令层面没有内建的一致性判定。

---

## 六、相关

- CLI：`cmd/opskeeper-eval/`
- 库：`core/harness/{schema,injector,judge,axes,leaderboard,projection,vocabulary,runner}`
- 闸门：`make eval-gates`
- 形状守卫：`scripts/apidoc`- 闸门：`make apidoc-check` —— 本文件里每条 `METHOD /path` 必须**被源码里真实的路由注册服务**（决策 269 起要求「注册过」，而不只是「树里出现过这个字符串」）

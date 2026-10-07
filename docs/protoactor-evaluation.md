# 评估：通讯架构层是否改用 protoactor-go

结论先行：**不改**。理由不是依赖重量（我最初的判断是错的，实测已推翻），
而是 protoactor 的核心模型——**位置透明 + gRPC 可达端点**——与 OpsKeeper 的
连接模型和授权模型**方向相反**。它要解决的问题我们没有，而它带来���转换会
动到一块必须冻结的契约。

本文所有外部事实均于 2026-09-30 实测核实，来源见文末。

---

## 一、外部事实核实

| 项 | 实测值 | 来源 |
|---|---|---|
| 模块路径 | `github.com/asynkron/protoactor-go` | `go list -m` |
| 版本 | `v0.0.0-20260118094027-288962e52f3f`（**无 tag，仅 pseudo-version**） | `go list -m -versions` |
| Go 要求 | 1.25.3（我们是 1.26，可接受） | 模块 go.mod |
| License | Apache-2.0 | GitHub API |
| 活跃度 | 5511 star / 52 open issue / 未归档；`dev` 分支最近提交 2026-01-18，仓库最后 push 2026-04-08 | GitHub API |
| 成熟度自述 | README 原文：*"The Go implementation is still in beta… the API might change over time until 1.0."* | README |
| 子系统 | `actor` `cluster` `remote` `router` `persistence` `stream` `scheduler` `testkit` `eventstream` `metrics` `log` `plugin` | 仓库根目录 |
| 设计原则 | *"Pass data, not objects — Protobuf all the way"*；gRPC streams 做网络，Consul 做集群 | README |
| 模块结构 | **单一扁平模块，无任何子目录 go.mod** | 逐目录探测 |
| cluster provider | `consul` `etcd` `k8s` `zk` `automanaged` `test` —— **全部是服务发现，无 broker 中继** | `cluster/clusterproviders/` |

### 依赖成本实测（我最初的判断被推翻）

我一开始认定"依赖炸弹"（根 go.mod 里确实有 `couchbase/gocb`、`k8s.io/client-go`、
`etcd`、`zookeeper`、`echo/v4`），但那是**根模块**的 require 列表，Go 1.17+ 的
模块图裁剪意味着只 import 你实际用的包。实测：

| 引入范围 | 间接依赖数 | 拉入的重物 |
|---|---|---|
| 仅 `actor` | 28 | 无 gRPC、无 k8s/etcd/consul；otel + prometheus client |
| `cluster` + `remote` | 35 | 仅 `grpc` |

而 OpsKeeper **本来就有** `otel v1.43`（比 protoactor 的 v1.38 更新）、
`prometheus/client_golang`、甚至**同版本**的 `emirpasic/gods`。

**所以边际依赖成本接近于零。** 依赖不是拒绝的理由——如果只为了这个理由就下结论，
那是懒惰的分析。

---

## 二、我们现有的通讯架构

约 6954 行（`core/floor/tunnel` + `frontierbound` + `core/edge/biz` + `core/wire`），四层：

```
① console ──HTTP/SSE──► manager                    帧契约已冻结
② manager ⇄ edge        singchia/frontier (geminio)  21 个方法
③ edge   ⇄ agent进程   unix socket + 行分隔 JSON     gate / toolbroker
④ edge   ⇄ pig         stdio JSONL                  pig --mode rpc
```

**第 ② 层是唯一真正"重"的一层**，也是这次评估的对口对象。

### 它的关键性质（protoactor 若要替换，必须逐条对上）

1. **双向出站，零入站要求**。cloud 侧监听端不在本仓库：`singchia/frontier`
   的 broker 终结 geminio，manager 去 dial 它，edge 也去 dial 它。
   **两端都只发起出站连接，谁都不需要被寻址。** 这是 NAT 穿透的正解。
2. **拨号时鉴权**。`Meta{AccessKey, SecretKey}` 在 dial 阶段换成 `EdgeID`，
   之后这条连接上的每个 RPC 都带着**已认证的节点身份**。
3. **一条连接承载 21 个方法 + 反向推送 + 流**（WebSSH 走 `AcceptStream`）。
4. **重连带退避**，handler 重注册。

---

## 三、逐项对标：protoactor 能给什么

| protoactor 能力 | 我们已有 | 换过去的净收益 |
|---|---|---|
| Actor + mailbox + supervision | `pigsupervisor`（崩溃退避 / Degraded / 手工 Restart 不重置预算）、各处 `sync.RWMutex` | **负**——见下方不匹配 ③ |
| `remote`（gRPC 双向流） | frontier 的双向多路复用 RPC | **负**——见不匹配 ① |
| `cluster`（位置透明 / 虚拟 actor） | 显式 edge identity + `bindEdgeTransport` | **负**——见不匹配 ② |
| `pubsub` | SSE | 无 |
| `persistence`（事件溯源） | 无此需求 | 无 |
| `stream` | SSE + tunnel 流 | 无 |
| 性能（README 称 200 万 msg/s） | 我们是**每秒几十条**运维指令 | 完全不相关 |

我们的真实吞吐是：一个节点一次事故里几十条工具调用，加上持续的心跳与遥测推送。
protoactor 的性能卖点在��这个量级上不构成任何理由。

---

## 四、五个不匹配（决定性）

### ① 连接方向相反 —— 决定性

protoactor 的 `remote` / `cluster` 假设**端点网络可达**：节点自己起 gRPC
server（`remote/server.go`），发现靠 consul/etcd/k8s/zookeeper。这些 provider
**没有一个是 broker 中继**，没有任何 NAT 穿透概念。

而 OpsKeeper 管的正是"客户机房里的机器"——NAT 后、无公网入口、
可能还有防火墙。frontier 的 broker 架构**就是为了解决这个问题而存在的**。

换用 protoactor 意味着：要么给每个节点开 ingress（运维上不可接受），
要么重写整个连接模型。**这是把已解决的问题重新变回问题。**

### ② 位置透明与授权模型冲突 —— 决定性

protoactor 的消息按 actor 路径寻址（`$1/$2/mypid`），它的信任前提是
"运行时保证消息送到正确的 actor"。

OpsKeeper 的安全模型恰恰相反，而且是**刻意反向**的：

- 身份来自**拨号时认证的 edge identity**，不来自消息里的自称；
- 闸门从**宿主自己的会话记录**反查调用者角色，扩展传来的 actor 一律不信
  （`gatesocket` 明确"actor 永远由宿主解析"）；
- broker **按工具名**触达，参数由宿主重新编码，不转发 agent 的原始字节。

**位置透明移除的，恰恰是我们花大力气建起来的"按我的记录重新确认你是谁"这一步。**
这两件事在同一个进程里无法同时成立。

### ③ Supervision 语义不合，且现有策略是刻意定制的

`pigsupervisor` 的崩溃策略是安全相关的：崩溃窗口内超预算 → 停止重启、
报 `Degraded`、**保持存活应答 health**；手工 `Restart` **不重置**预算
（防止"点一下就好"的假象）。

换成 protoactor 的通用 supervision，等于用一个更泛化的模型替换一个
更具体、更保守的安全策略。

### ④ "Protobuf all the way" 与冻结的 SSE 契约正面冲突

protoactor 的设计原则是 protobuf 全链路。而 OpsKeeper 的
**SSE 帧契约是冻结的**：前端 52K 行 React 不重写（计划明确要求"前端零改动"），
`core/wire.StreamEvent` 的 11 个事件类型是对外契约。

采纳 protoactor 等于要把 `core/wire` + 21 个 tunnel 方法 + SSE 帧全部重编码为
protobuf 并给前端升版。**这是一个高风险重写，换来的收益是零。**

### ⑤ 成熟度不匹配，且它是"可选依赖"

PiG 是**必须**采用的——它就是 agent 运行时。所以我们对它定了一条铁律：
预稳定上游只能出现在一个模块里（`core/pig`），并配契约测试锁定用到的那部分 API 面。

protoactor 是**可选**的。为一个我们不需要的能力，引入一个 README 自述
beta、无 tag、API 会变的依赖——**同样的风险，零收益**。

---

## 五、结论与建议

### 不改。理由排序

1. **连接模型方向相反**（①）—— protoactor 没有 NAT 穿透，frontier 有。
2. **授权模型冲突**（②）—— 位置透明与"宿主重新确认身份"不可共存。
3. **冻结契约**（④）—— protobuf 全链路会动到不能动的地方。
4. 它是可选依赖，却带着 beta 风险（⑤）。

**注意依赖成本不是理由**（实测已推翻我的初始假设）。如果①②④不存在，
仅仅为了"actor 模型更优雅"而引入，是可以考虑的；但它们存在。

### 什么情况下值得重新评估

给一个明确的触发条件，而不是一句"以后再说"：

- 节点规模达到**万级且大量会话同时处于流式推送**状态——那时"每会话一个 actor"
  带来的隔离与背压才值得它的复杂度。**当前不是**：`nodefleet` 每个活跃会话
  一个条目，`pigwire` 翻译器集合 + 互斥锁足够。
- 出现真正的**跨区域主动-主动**需求，需要 manager 主动寻址节点。
  即便如此，答案也更可能是给 frontier 加能力，而不是换掉它——
  因为 broker 中继那一层无论如何都要存在。

### 如果一定要用，最小可行形态

**只引 `actor`，且只在 manager 进程内**，用两个 actor 替换现有互斥锁映射：
一个 per-edge（frontierbound 的 transport→edgeID 绑定），一个 per-session
（nodefleet 的会话表）。**edge 二进制不碰 protoactor**——它跑在客户机房，
必须小、必须快、必须在控制面失联时正常工作。

即便如此我也**不建议现在做**：收益不明确，而它会给一个已有单一并发模型的
代码库引入第二个模型 + 一个 beta 依赖。

---

## 附：来源

- 模块元数据：`go list -m github.com/asynkron/protoactor-go@latest`、`-versions`
- README：<https://raw.githubusercontent.com/asynkron/protoactor-go/dev/README.md>
- 仓库元数据 / 目录 / 提交：<https://api.github.com/repos/asynkron/protoactor-go>
- 依赖实测：本地 `go mod tidy`（仅 `actor` / `cluster`+`remote` 两种范围）
- OpsKeeper 侧：`core/floor/tunnel/doc.go`（broker 中继与出站拨号）、
  `core/wire/events.go`（冻结的 SSE 事件类型）、`core/edge/pigsupervisor/supervisor.go`

# OpsKeeper 边缘自治与 LLM 网关：与实现对齐的方案（v2）

> **这份文档取代旧版方案里"阶段 0/1 待做"的判断。**
> 旧版方案写于改造之前，它描述的 P0 断点在今天的树里已经不是断点。
> **风险方向因此变了**：不再是"做不做得出来"，而是"方案与台账口径不同步，
> 照旧方案判断会得出完全错误的结论"。这份文档的每一条判定后面都跟着
> 能在本仓复跑的证据；没有证据的条目一律标成"未核实"，不写成已完成。

---

## 一、旧版方案已经过期的四处（照它施工会做错）

| 旧版方案的前提 | 树里的事实 | 证据 |
|---|---|---|
| 「PiG 保持 0.3.0 固定 tag」 | 已升 **v0.4.0**，且新增「插件 SDK 与宿主必须同版」的闸门 | `scripts/modulecheck` 的 `checkPiGSDKPinAgreement` |
| 网关在 `core/manager/llmgateway` | 实际在 **`core/domains/server/llmgw`**（域化之后的位置） | 目录实测 |
| 「阶段 2/3 不在本轮承诺交付时间内」 | **阶段 2/3 已全部落地** | 见第三节 |
| 「无遥测本地 spool，断连即数据丢失」 | **`core/edge/telemetrywal` 已存在**，有丢弃策略与回放 | 包存在且测试通过 |

第五处不是过期而是**已和解**：旧版方案设计的 `llm.token`（30 分钟 TTL 的短时节点令牌）
**没有实现，而且是刻意的**。网关复用节点已有的 `accessKey:secretKey` 隧道凭据
（`core/domains/server/llmgw/llmgw.go` 的 `authenticate` 注释写明了理由：
没有第二个需要单独吊销的东西，也就没有第二个会被忘记吊销的东西）。
代价是长期密钥被呈现在一条 HTTP 路由上，因此该路由必须在 TLS 之后且禁止记录该 header。

**这处差异必须写出来，因为它的安全性质与旧方案不同**：
吊销的粒度从"30 分钟自动失效"变成"轮换节点密钥"，成本归集仍然按节点（`edgebudget_test.go`）。

---

## 二、阶段 0：每机一个 Agent 的交付闭环 —— 已完成

| 条目 | 判定 | 证据 |
|---|---|---|
| LLM 网关（OpenAI 兼容、流式、按节点预算、成本归集、降级） | 完成 | `core/domains/server/llmgw`：`llmgw.go` / `callbounds.go` / `spend.go` / `edgebudget_test.go` / `nodeidentity_test.go` / `degrade_test.go`，测试通过 |
| edge 侧注入端点与凭据 | 完成 | `cmd/opskeeper-edge/agent.go` 的 `agentEnv` 经 `modelCfg.AgentEnvVars()` 注入，并在 **spawn 时刻叠加 `adopter.envOverlay()`**（捕获副本会让重启回到旧配置，循环不收敛） |
| `pig` 二进制进入交付物 | 完成 | `deploy/install/edge/build-edge-bundle.sh` 与 `deploy/Dockerfile.opskeeper-edge`（`OPSKEEPER_EDGE_AGENT_BIN`）、`install-edge.sh` 的 `pig --version` 自检 |
| 密钥不出中心 | 完成 | `tests/e2e/node_agent_delivery_test.go` 用 **decoy key** 断言云厂商密钥到不了节点 |
| 通告地址与真实路由不漂移 | 完成 | `cmd/opskeeper/llmgateway_advertise_test.go` 起真实 router、发真实请求，断言打到模型调用 |

---

## 三、阶段 1：离线与有限自治 —— 已完成

| 条目 | 判定 | 证据 |
|---|---|---|
| 遥测本地 WAL | 完成 | `core/edge/telemetrywal` |
| 自治白名单（trigger / argv / blast_radius / ttl / idempotency_key） | 完成 | `core/edge/autonomy`：`autonomy.go`（含上述五项字段）、`execute.go`、`spool.go`、`pump.go`、`escape_test.go` |
| 幂等与栅栏三用例 | 完成且**超出方案** | `core/edge/policygate/fence_test.go` 共 9 条，含「同一调用提交八次只是一次执行」「授权只在租约内可领取」「同会话第二个写调用必须等待而非排队」 |
| 本地审计留证与回传 | 完成 | `core/edge/auditlog`（`spool.go` / `pump.go`） |

方案只要求三条栅栏用例，树里是九条。**这是实现超出方案，不是方案超纲。**

---

## 四、阶段 2：生态与治理 —— 全部落地

| 条目 | 判定 | 证据 |
|---|---|---|
| 工具注册表 | 完成 | `core/manager/biz/aiops/toolregistry` + `tools/tool_search_tool.go` |
| 工具检索 | 完成，**做法与旧方案文字不同** | `tool_search_tool.go` 是 `select:` 精确名 + 子串匹配（name + description + when_to_use）。**这不是缺口**：该工具的自我定位是"你在系统提示里看到工具名但 schema 未加载"，即**按已知名字取 schema**，不是"从模糊需求里猜工具"；能力清单枚举全部工具名、只折叠 specialty 的 schema，所以名字始终可见。旧方案写的"RRF 融合"是为另一种用途写的 |
| per-tool 资源配额 | 完成 | `sdk` 的 `domain.ToolLimits{OutputBytes, TimeoutSeconds}`，**edge 侧在 `policygate/policy.go` 强制**（未声明则回落到默认值） |
| MCP 兼容层 | 完成 | `core/pig/pigmcp` |
| 渐进式结晶降本 | 完成 | `cmd/opskeeper/loop_crystallize.go` |
| eval 三维化 | 完成 | `cmd/opskeeper-eval/axes.go`：Localization × Identification × Reason |
| 只读边界不得放开 | **保持不变，且有守卫** | `make eval-coverage` 实测输出三行：**诊断轴 17/20**、**修复轴 0/20**、**联合 0/20**。**联合那个 0 是刻意的**：写操作一律经审批通道执行，打包成节点插件等于开出第二扇没有队列的门，所以 upcall 通道对任何包一律拒绝非只读工具。闸门挂在**诊断轴**上（会动：现在是 17/20，三个未覆盖的案例各自的缺口与理由记在 `pluginmanifest.DiagnosisGaps`） |

---

## 五、阶段 3：控制面瘦身与联邦

| 条目 | 判定 | 证据 |
|---|---|---|
| 审计端口抽出、解开反向依赖 | 完成 | 声明边已切 34 / 34 |
| 多集群联邦 | 完成 | `core/floor/federation`（4 MiB 上限、摘要先验后解包、签名 gate）、`core/floor/tunnel/federation.go` 的 `cluster.hello` / `cluster.policy` / `cluster.state` |
| **manager 本体拆分** | **未开始** | 未搬 **984 个 Go 文件 / 252,529 行**（口径就是台账第 15 条闸门跑的那两条命令） |

---

## 六、真实缺口（四条，按"能不能在本仓推进"排序）

1. **manager 本体未拆** —— 但它**不是本仓能测出来的阻塞**：需要先回答部署形态的现实问题，
   而其中两问依赖本仓之外的信息。**这是待人拍板项，不是缺陷。**
2. ~~工具检索是子串匹配，不是语义检索~~ —— **本条经实测后撤销**（决策 436）。
   子串匹配服务的用途是"已知名字取 schema"，不是"从模糊意图里找工具"，
   所以它不是欠账。**它只会在一种情况下变成欠账**：若将来要求模型在
   **不给出工具名**的前提下按自然语言意图选工具，那时才需要语义检索。
3. **真实 provider 未跑通** —— 四阶段交付尺剩下的 2% 全在这里，属外部条件。
4. **发布基线** —— `make version-check` 的红按构造成立（基线之后叠了数百次提交与边界外文件），
   要绿得重定基线或压历史，**这是发布口径决定**。

## 七、两把尺

- **架构尺 A–E：97.75%**（A 100 / B 100 / C 95 / D 95 / E 100）
- **四阶段交付尺：99.5%**

### 阶段完成度：**以台账的四阶段表为准，不要用本文档的判断**

台账 §六 的四阶段表是权威读数：**阶段 0 = 98%、阶段 1 = 100%、阶段 2 = 100%、阶段 3 = 100.0%**
（四阶段等比，加权 ≈ 99.5%）。

**本节此前写着「阶段 0 = 100%」，是错的**，已更正。错因值得留着：

> 本文第二节逐条核对了阶段 0 的**实现项**（网关、凭据注入、`pig` 交付、密钥不出中心），
> 全部在位——**但从未核对阶段 0 的验收门槛**。门槛 0.4 有一条是
> **「一台 edge 通过控制台完成一次真实对话并返回流式输出」**，
> 而 `make e2e-delivery-check` 实测是「真 manager + 真 edge + 真 pig 子进程 +
> 真网关 + 真 SSE 帧，**只替掉模型**」。替掉模型的那一条**没有证据**。

**实现项齐全 ≠ 阶段达成。** 这与上一轮那条错误互为镜像：
一条是把**没做的**判成**做了**（工具检索），一条是把**做全了的**判成**验收过了**。
两者都是**没有去看被问的那个问题**。

**剩余 2% 的准确内容**：一次**真 provider** 的推理。
**只有这一条**——其余门槛已有已执行的证据。

## 八、验收门槛（可复跑）

```
make module-check              # 模块边界
make eval-gates                # 黄金集 20 例
make module-standalone-check   # 每模块独立构建 + 测试
make eval-coverage             # 只读边界不得被凑绿
make ledger-check              # 台账里的数必须是树自己的
```

以上五条在写这份文档时**逐条复跑并记录退出码，五条全为 0**。

### 8.1 方案 §六 的安全专项已经有闸门在守（这是最强的一条证据）

旧方案 §六 列了「栅栏三例 / 节点令牌越权 / 自治逃逸三例 / 覆盖率轴是预期值」
与「回归：确认 plugin-coverage 仍为 0/20」。**这些在今天的树里不是待办，是已接进 CI 的闸门**：

```
make plan-security-check       栅栏三例 / 令牌越权 / 自治逃逸 / 覆盖率轴是预期值
make edge-credential-check     节点进程读不到任何云厂商凭据
make agent-llm-path-check      广告给节点的模型 URL 真的能到达网关注册的路由
make compliance-claims-check   承诺 vs 代码实况：强制 / 惰性 / 仅声明逐条对着树核
make promptguard-check         外来文本进模型前带 nonce 围栏
make mcp-surface-check         MCP 对外协议面
make audit-port-check          审计端口：iam 不再反向依赖 manager
make roadmap-delivery-check    声称已交付的每一项都能指出证据
make crystallize-check         结晶的晋升 / 退役 / 拒绝不可用输入
make broker-pin-check          版本引用只有一个真相源
```

**十条同样逐条复跑，退出码全为 0。** 这一条比本文档里任何文字都更能说明
**旧方案的 §六「测试计划」是在描述一个还没建闸门的时代**。

仍红的两处是 `make version-check`（发布口径）与开源审计的 13 项（待人拍板），
两者都不在上述门槛内。

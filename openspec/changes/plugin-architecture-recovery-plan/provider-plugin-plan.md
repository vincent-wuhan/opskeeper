# 4.6 provider 接内核改造方案 + 模型供应商插件化（Ollama / AWS Bedrock）

> 状态：**方案 v0.1（待评审）**。对应 `tasks.md` 4.6；评审通过前不勾选 4.6。
>
> 基线：`main@7623f485`（PR #105 之后）。方案起草于 `main@483783b4`（PR #104 之后，本任务打开时的 HEAD），交付前已 rebase 对齐：#105 未触碰本 change 目录、`aiopsdatasource.ManagerPolicy()`、内核与 `sdk/manifest.go`，仅向 `core/domain/plugin.go` 增加 `ScopeCloudRead`（`cloud.read`）常量并落地 adapter 宿主切片 S0——与本方案的交叠只有 §11.1 的 scope 常量位次（已按新树更新）。与 4.1 `business-domain-mapping.md`、4.2 四分类保持口径一致。
>
> 范围：**仅方案产出**。本文档回答"模型供应商怎么从 `core/pig/pigmodel` 自持的工厂注册表接到 `kernel.KindProvider`、entry 名该叫什么、既有七个 provider 怎么在默认部署行为不变的前提下迁移、凭据为什么必须留在宿主的环境作用域、费用/速率/并发/超时在宿主侧的哪一个执行点收口、每个第三方候选怎么注册怎么回滚"。§12 的实现切片建议**不**在本任务内合入；任何对 `core/floor/kernel/kind.go`、`core/pig/pigmodel/providers.go` 主流程、`core/domains/biz/setting/llm.go` 或 `plugins/` 包结构的真实改动，必须另开 change 并独立评审。

## 1. 目标与边界

### 1.1 目标

| # | 目标 | 判定 |
|---|---|---|
| G1 | `core/pig/pigmodel` 的自有 `providerFactories` 注册表迁移到 `kernel.KindProvider`，`Activate` 成为注册唯一入口 | §3、§5 |
| G2 | 七个既有 provider id（openai / anthropic / zhipu / gemini / deepseek / kimi / custom）成为**编译内预装插件**，默认部署行为不变 | §5.2 |
| G3 | 治理收口：凭据保持宿主环境作用域；预算 / 速率 / 并发 / 超时在宿主侧**单一执行点**生效，包不能抬高自己的上限 | §6 |
| G4 | 第一批第三方候选：Ollama（自托管、无密钥）+ AWS Bedrock（云厂商、SigV4 签名），每候选有 manifest、注册路径、测试、回滚 | §7–§8、§10 |

### 1.2 与 4.1 / 4.2 的边界

- **4.1 §3**：`provider` 状态为"**未接内核**"，证据 `core/pig/pigmodel/providers.go` 自有 `providerFactories`（七个 provider id）。本方案的全部内容就是消掉这一行的状态，不新增第七个扩展点。
- **4.1 §4 ⑨ 企业集成**："LLM 模型供应商"行标注为"宿主注册表已有，未接内核 / 迁移为插件 / 后续任务 4.6"，本方案不改归属只补路径；同一表"AI 网关 / 节点模型端点"行（`core/manager/higress`、`core/domains/server/llmgw`）归**保留 core**，本方案只描述接口边界（§9），不动它们的一行代码。
- **4.1 §6 缺口表**："`provider` 未接内核，模型供应商只能编译内增加 → 企业集成、协同 → 4.6"。本方案收口该行。
- **4.1 §7 待评审问题 3（任务描述称之为结论 3，硬约束）**："嵌入模型是否归入 `provider`？当前 `KindProvider` 只定义为对话补全。"本方案按"**不归、不设计、不实现**"处理，只在 §11.2 以独立 change 决策点的形式记录（沿用 4.5 §12 的写法）。
- **4.2 四分类**：provider 归"迁移为插件，未接内核"（其结论目前由 4.1 §3/§4 的"归属初判/现状"列承载，尚无独立文档）。本方案只在"迁移"分支内推进，不触碰"保留在 core / 暂缓迁移 / 淘汰"三类。
- **内核契约**：`core/floor/kernel/kind.go` 的 `KindProvider` 注释本身就写着 "replaces the hardcoded switch in pigmodel"。六 Kind 封闭集合不变，**不新增字符串**。

### 1.3 非目标

- **不写实现代码。** §12 只给骨架与禁项。
- 不改 `core/floor/kernel/kind.go`。
- 不改 `core/pig/pigmodel/providers.go` 的主流程（§5.1 说明为什么这迫使迁移分三片、第三片为什么必须独立 change）。
- 不改 `core/domains/biz/setting/llm.go` 的目录解析、`core/domains/model/setting/model.go` 的键常量、`web/src/pages/settings/LLM.tsx` 的 provider 清单——provider id 在这三处的开放是 §2.2 F1 记录的事实，其解法是独立切片（§12 C1），不在本任务。
- 不做嵌入模型（§11.2）。
- 不做 AI 网关（`higress`）与节点模型端点（`llmgw`）的插件化，二者按 4.2 归保留 core。
- 不做 notifier / collector / adapter（4.5 / 4.7 / 4.4）。
- 不引入 per-kind ceiling：本方案是六个扩展点里唯一一个**零内核改动、零策略改动**的（§4.5 论证为什么 provider 不需要它，以及实测依据）。

## 2. 现状证据

### 2.1 模型供应商链路全貌

```text
两条构造路径，一条消费链：

  ① 设置目录   system_settings.llm.*（admin-only 写入；敏感键 *_api_key 默认脱敏，
               审计只落 value_digest + 长度，core/domains/server/setting/http.go:78,122,201）
               core/domains/biz/setting/llm.go allProviderKeys()  ← 七个 id 的硬编码清单
               │   env 兜底：OPSKEEPER_OPENAI_API_KEY 等（core/floor/config/config.go:635）
               ▼
  ② 目录解析   llm.ProviderConfig{ID, Label, APIKey, BaseURL, Models, Model}
               │
        ┌───────┴───────────────────────────────────┐
        ▼                                           ▼
  ③a 一次性补全路径                          ③b 交互式 agent 路径
  llmpig.SettingsSource(catalog)             llmpig.Sync.Publish
  → pigmodel.SettingsSource                  → pigcoding.RegisterProviders(specs)
  → pigmodel.Registry（按 id 缓存 provider）  → PiG ModelRegistry，线型由 apiFor(id) 定
    ├ lookupProviderFactory(id)                └ apiFor：仅 anthropic 特判，其余一律
    │   ← providerFactories map（7 id）             OpenAI Completions（pigsettings.go:257）
    └ Complete → ai.Provider.Stream
        │
        ├ CallObserver → prom.ObserveLLMCall（provider/model/status/秒/in/out token）
        ▼
  ④ 消费者（全部拿 pigmodel.Completer，注入式）
     manager 一次性：query-translate、loop LLMCaller、RCA 摘要、chat2query、评测 judge
     manager agent 内核：agentkernel.Budget.Allow（只查不记，估算恒为 0）
     节点路径：llmgw.Handler（Auth / Budget / Limiter / Bounds）→ 同一个 Registry
```

### 2.2 五个必须改口径的事实

| # | 事实 | 证据 | 对方案的影响 |
|---|---|---|---|
| F1 | **provider id 在四处封闭**，加一个 provider 要改四个文件 | `core/domain/provider.go:26-36`（`KnownProviders`，注释明说 id 同时是存储键前缀与线上值）；`core/domains/model/setting/model.go:133-142`（`LLMProvider*` 常量）；`core/domains/biz/setting/llm.go:94-127`（`allProviderKeys` 的键清单）；`web/src/pages/settings/LLM.tsx:22,92-145`（`LLMProviderID` 联合类型与卡片数组）；再加 `core/pig/pigmodel/providers.go:41-49` 的工厂 map | 内核 entry 名解决的是第五处；其余四处要靠**包同时贡献 descriptor**（§3.4）+ 独立切片 C1（§12）。只把 map 搬进内核而不解决 descriptor，"加一个 provider = 改源码"这句缺口描述只消掉五分之一 |
| F2 | **同一个 provider 在两条路径上说两种线型** | `core/pig/pigmodel/providers.go:110-117` 给 gemini 用原生 Google provider；`core/domains/llmpig/pigsettings.go:257-262` 的 `apiFor` 只特判 anthropic，gemini 落到 OpenAI Completions。`core/pig/pigai/pigai.go:116-120` 只定义了两种线型 | 迁移必须把"线型"归到**同一个注册来源**，否则接了内核也只是把分裂搬进内核。这是 B 片之外的 C2 切片（§12） |
| F3 | **无密钥 provider 被读取层丢弃**：域类型允许"只有 base_url"，目录解析却不允许 | `core/base/pkg/llm/doc.go:52-60` `Configured()` 的注释专门解释为什么允许（"a local vLLM or a corporate gateway … refusing it would push operators into pasting a dummy key"）；而 `core/domains/biz/setting/llm.go:143-147` 在读 base_url **之前**就因 api_key 为空 `continue` | 自托管类（Ollama）在今天的 OpsKeeper 里**配不进来**。这是候选 1 存在的第一理由，也是 C1 切片要修的判定顺序 |
| F4 | **治理执行点分裂且不对称**：四道闸门只有节点路径全有 | 节点路径：`llmgw` 的 `Budget`（集群日上限 + 每节点归因）+ `Limiter`（每节点 RPM，默认 60）+ `CallBounds`（超时 / 输出 token 上限 / 降级）；manager agent 内核：`agentkernel.Budget.Allow` **只查不记**且估算恒为 0（`core/manager/biz/aiops/agentkernel/deps.go:85-96` 的注释自己承认"weaker than the legacy path"）；manager 一次性路径（judge / query-translate / loop / RCA / chat2query）：**预算、速率、并发、超时四项全无**，只有观测 | 费用治理的收口点必须在"每条 manager 路径都经过"的那一缝上，即 `pigmodel.Registry`（§6.2），而不是再加一层只有部分路径经过的网关 |
| F5 | **账本没有 provider 维度** | `prom.ObserveLLMCall` 的指标带 provider/model 标签（`core/floor/prom/manager_metrics.go:414-429`），但 `llm.InMemoryBudget` 与 `llmgw` 的配额都只有"集群 / 节点"两个维度 | 一个第三方 provider 包可以把花费路由到任何价位的模型，而运维没有任何按 provider 收紧或拒绝的手段。§6.4 给出 per-provider 维度的执行点；§11.1 给出按 policy 拒绝的 scope 手段（独立 change） |

### 2.3 已识别缺口（迁移后仍在，由候选包或独立 change 解决）

| 缺口 | 现象 | 证据 | 处置 |
|---|---|---|---|
| G-1 **双记风险** | llmgw 与（本方案新增的）Registry 闸门都会经过同一次调用；若两处都 Record，账本翻倍 | `cmd/opskeeper/main.go:4388-4411`（`globalTokenBudget` 把控制台的账本适配给网关）；`llmgw/spend.go` 的 `Budget.Record` | §6.2 的"**一个账本一个写者**"不变量 + 验收断言 |
| G-2 **manifest 没有费用/速率声明位** | `sdk.Decode` 用 `KnownFields(true)`（`sdk/manifest.go:60-66`），任何新键都是解码错误；`spec.tools[].limits` 只对 tool 生效，provider 不声明 tools | 实测：给草案加 `spec.limits:` 一键即被拒 | §11.3 作为 ABI 扩展的独立 change 决策点给出草案 |
| G-3 **manager 一次性路径完全无闸门** | 评测 judge、查询翻译、闭环 LLMCaller、RCA 摘要、chat2query 都直接拿 `Completer` | §2.2 F4 | §6.2 的执行点选择即为此 |
| G-4 **edge 目标的 provider 包会被 manager 宿主放行** | `Host.Admit` 只检查 `RunsOn(target)`，`targets: [manager, edge]` 的包在 manager 宿主上合法 | 实测（§10.1 对照 D） | §3.5 的宿主侧规则 + 切片 S-2 |

## 3. 注册语义：内核 entry 名 = provider id

这是整个方案的地基。**内核 `KindProvider` 的注册名就是 provider id**（`openai` / `anthropic` / … / `ollama` / `bedrock`），注册的实现体不是裸工厂，而是"**descriptor + 工厂**"一个结构（§3.4）。

### 3.1 三候选取舍

| 候选命名 | 后果 | 判定 |
|---|---|---|
| **entry 名 = provider id** | 与今天 `providerFactories` 的 map 键一一对应；一个包实现一个 id；`ErrDuplicate` 的语义 = "一个 provider id 只有一份实现"；id 同时是设置键前缀（`core/domain/provider.go` 的注释），注册名与存储键同名使"设置行属于谁"不需要第二张映射表 | **采用** |
| entry 名 = 包名（`opskeeper-provider-ollama`） | id 与包名解耦后，解析 `ollama` 要先查一张"包名 → id"的表，等于把 `providerFactories` 原样搬进内核再多一层间接 | 拒绝：迁移变成"多搬一层"，未接内核的状态没变 |
| entry 名 = provider 实例名（`ollama-lab`） | 同一个 Ollama 要两份配置就得装两个包；而 `Activate` 的贡献体是**工厂**，不是已构造的 provider，注册实例等于把配置数据塞进插件注册表——多实例由设置表表达（一行一个 base_url/模型清单），不由注册表表达 | 拒绝：注册表是能力目录，不是配置存储 |

采用第一条的副产品：`pluginhost.Inventory()` 里 provider 条目的 `name` 就是运维在设置页看到的 provider id，`impl` 是贡献体的类型名（`core/floor/kernel/inventory.go:6-12` 故意只渲染类型名而不渲染实现值，以免把密钥或 URL 打进日志）。诊断面不需要另写映射。

### 3.2 同名互斥：`ErrDuplicate`，无覆盖语义

- `core/floor/kernel/registry.go:16` 的 `ErrDuplicate`：同一 `(Kind, Name)` 二次 `Register` 被拒，**不覆盖**；`core/floor/kernel/host.go` 的 `Activate` 对已激活包名同样报 "plugin is already active"。
- `core/pig/pigmodel/providers.go:59-73` 的 `RegisterProviderFactory` 今天已经是"拒绝重复而非替换"，且注释给的理由与内核一致（"a silent replace is how one provider takes over another's id"）。迁移是把这条语义**交给内核执行**，不是新发明。
- 落到本扩展点的具体含义：**一个 provider id 在一个进程内只有一个实现包**。想同时用两个实现，唯一办法是给其中一个换 id（如 `ollama-lab`），于是它在设置页、在 `RegisteredProviders()`、在 inventory 里都是**第三个可见 provider**，而不是一个藏在配置里的分支。宁可让运维看见第三个 id，也不要让"设置里写了 ollama、跑起来的是别人的实现"。

### 3.3 七个内置 id 是保留名（provider 特有的凭据让渡风险）

这条是 provider 与 notifier 最重要的差异，必须单独写。

notifier 的渠道密钥是**每行渠道自己配的**（`endpoint` + `secret`），一个包接管 `slack` 这个类型，拿到的是"以后新建的 slack 渠道会用我的实现"，不自动拿到存量密钥。provider 不是：`openai_api_key` 是设置表里一行**长期有效的组织级密钥**，而 provider id 就是这行键的前缀。如果允许一个第三方包注册 `openai`，等于把运维已经填好的 OpenAI 密钥**整体移交给**一个没人评审过"是否配拿这个密钥"的包。

所以本方案不沿用 4.5 §3.3 的"遇 `ErrDuplicate` 记 Info 后跳过"，而是反过来：

> **七个内置 id（openai / anthropic / zhipu / gemini / deepseek / kimi / custom）是保留名。**装配层在 `Activate` 之前检查贡献体申请的 id：命中保留名即拒绝启动，错误信息点名"保留 id `X` 由一方包 `opskeeper-provider-X` 提供，第三方包请注册新 id"。第三方包要提供"更好的 OpenAI 兼容客户端"，就注册 `openai-x` 并让运维给它单独配一行密钥——密钥跟着包走，而不是包接管密钥。

| 候选 | 做法 | 失败模式 | 判定 |
|---|---|---|---|
| **保留名拒绝** | 装配层（provider 宿主包）在 `Activate` 前比对保留清单 | 运维装了一个想接管 `openai` 的包 → 启动报错、点名包与保留 id → 可见失败 | **采用** |
| 沿用 4.5 的"跳过 + Info" | 第三方先注册，内置方跳过 | 存量密钥被静默移交给未评审的包；运维在设置页看不到任何变化 | 拒绝：对 notifier 是"预期的运维选择"，对 provider 是凭据让渡 |
| 内置先注册，让 `ErrDuplicate` 自然挡住 | 装配顺序倒过来 | 表面上等效，但语义取决于装配顺序而非规则本身——顺序一改（例如第三方目录先 `Load`）语义就翻转 | 拒绝：把安全性质建立在装配顺序上是脆的 |

保留清单落在 provider 宿主包里（一个 `[]string` 常量），**不是内核改动**；内核的 `ErrDuplicate` 仍是最终兜底。

### 3.4 descriptor：一个 id 的"设置形状"随包贡献

只注册工厂不够。provider id 同时是设置键前缀与 UI 卡片（F1），所以包要一次贡献**两样东西**：

```go
// 贡献体（Impl，注册在 KindProvider/<id> 下；一个 entry，随包整体回滚）
type Provider struct {
    ID              string // "ollama"
    Label           string // "Ollama（自托管）"
    DefaultBaseURL  string // "http://127.0.0.1:11434/v1"；空 = 无默认端点
    RequiresBaseURL bool   // custom 为 true；其余为 false
    RequiresAPIKey  bool   // ollama 为 false——F3 的修复点就在这面旗上
    Factory         ProviderFactory
}
```

把它做成**一个 entry**（而不是 `KindProvider/<id>` + 另一张表）的理由：回滚以包名为键，`RemovePlugin` 一次清干净；inventory 一行就能回答"这个 id 是谁提供的、需要什么配置"；`sdk` 的 manifest schema 一字不动（`Impl` 是 `any`）。

设置目录与 UI 消费 descriptor 的那一步（`allProviderKeys()` 改为"内置清单 ⊕ 内核里已激活的 provider 条目"）落在 C1 切片（§12），因为它触碰 `core/domains/biz/setting/llm.go` 与前端，均在本任务非目标清单里。

### 3.5 目标面规则：provider 只允许 manager

`targets: [manager]` 不是部署偏好而是安全边界：节点不持有任何 provider 凭据（`core/domains/server/llmgw/llmgw.go:1-14` 整篇设计就建立在"**要么密钥去节点、要么请求来控制面**，选后者"上）。一个 `targets: [manager, edge]` 的 provider 包等于要求把密钥放到节点上。

实测（§10.1 对照 D）证明现状挡不住它：`Host.Admit` 只做 `RunsOn(manager)`，含 edge 的包同样被放行。所以 provider 宿主包要加一条显式规则：**贡献体的 manifest `spec.targets` 必须恰为 `[manager]`**，否则拒绝并点名。Bedrock 这种要长期 IAM 凭据的包，这条规则就是它不落到节点上的唯一机械保证。

### 3.6 配置期单激活与"无配置即降级"

与 4.3 §3.3 / 4.5 §3.3 同一语义，provider 的表达是：

| 概念 | 键 | 互斥规则 | 谁负责 |
|---|---|---|---|
| 实现单激活 | provider id | `ErrDuplicate` + §3.3 保留名 | 内核 registry + provider 宿主包 |
| 配置多开 | 设置表行（`<id>_base_url` / `<id>_models` / …） | 不互斥；同一 id 只有一份配置（id 即键前缀） | 设置目录 |
| **无配置即降级** | 设置表无该 id 的行 | 该 provider 解析到 `ErrNotConfigured`，**不报错、不阻塞启动**，设置页与 `GET /v1/models` 里不出现在可选项 | `pigmodel.Registry.Resolve` 既有语义 |

第三行是 provider 特有的收益：装了 `opskeeper-provider-ollama` 但还没填 base_url 的部署，行为与没装完全一致。这与 datasource"无后端 → 不注册工具"的降级同族，且**已存在**（`Resolve` 的 `ErrNotConfigured` 分支），迁移零成本继承。

## 4. 宿主 Policy 设计

### 4.1 为什么不能沿用 datasource 的 L1

4.3 方案的三个候选全部是 `safety_level: L1` + `capabilities: [read]`，理由是"只读，没有成功状态可言"。这个推理对 Prometheus / Loki / Tempo 完全成立，对模型供应商**不成立**：

- `core/domain/safety.go` 里 `ClassRead` 的定义是 "**a pure observation. Touches no external system**"。一次补全把整段事故转录发给了第三方，并产生了一条**计费用量记录**——既不"纯"，也不"无外部系统"。
- 一次调用有真实的单价（F5）、真实的失败面（限流 / 配额 / 401 / 超时）和真实的资源占用（连接池、goroutine、并发槽位），"没有成功状态可言"这句话在 provider 上是反的。
- 更实际的证据：本任务自身就把它称为"**费用与外部依赖问题**"。一个被叫作费用问题的能力标成只读，是本文件里最容易被评审一句否掉的谎。

所以 §4.2 用仓库自己的词表逐句对，而不是给一个"看起来合理"的等级。

### 4.2 安全级别论证：L2 + `write`

| 依据（`core/domain/safety.go` 原文） | 结论 |
|---|---|
| L0/L1 = "read-only observation / read-only plus local evidence collection"；`ClassRead` = "a pure observation. **Touches no external system**" | 补全拨号第三方、发送转录、产生计费用量记录 → **L1 不成立** |
| L3 = "mutates **external systems**, scoped approval with a blast radius"；`ClassDestructive` = "mutates state outside OpsKeeper's control: **a service restart, a kubectl apply, a SQL statement**" | 被调用的推理服务没有被改写，例句里没有一句是"调用一次 API"；vendor 侧唯一变化是账单与用量日志 → **L3 是夸大**，且会迫使 manager 抬 per-kind ceiling（§4.5） |
| L2 = "mutates **OpsKeeper state**, single approval"；`ClassWrite` = "mutates OpsKeeper-managed state" | 一次成功的补全确实改写了 OpsKeeper 管理的状态：集群当日 token 账本（`llm.InMemoryBudget.Record`）、节点归因账本（`llmgw.AttributedBudget`）、用量序列（`prom.ObserveLLMCall`）。这正是本任务"费用治理"要管的那个状态 → **采用 L2** |
| `MinimumClass()`：L2 恰是 `ClassWrite` | 声明 `[write]` 正好压在上限，不是够到天花板；`sdk.Validate` 的 `validateGovernance` 通过 |
| `RequiresApproval()`：L2 及以上为 true | 见下面"single approval 到底是谁批"的说明 |
| 未识别等级按 L3 排名 | 低报没有好处、高报也没有便宜可占——如实写 L2 不承担额外风险，实测见 §10.1 |

**"single approval" 是谁批**（这条必须写清，否则 `approval.required: false` 会被读成绕过审批）：`ApprovalPolicy.Required` 门的是 **agent 工具调用**的审批往返（`core/domain/plugin.go:120-123`），provider 包不声明 `spec.tools`，那条路径对它不存在，写 `true` 是声明一个不存在的控制。provider 真正的授权今天就有三层，全部在宿主侧：

1. 设置页的 provider 行是 **admin-only**（`core/domains/server/setting/http.go:78` `requireAdmin`），每次写入进审计且敏感值只留**摘要与长度**（`value_digest`，`http.go:122-131`）——这就是 L2 那个"single approval"的实体；
2. 没有设置行的 provider 解析到 `ErrNotConfigured`，**不会被调用**（§3.6）；
3. 每次调用的上限由宿主的预算 / 速率 / 并发 / 超时闸门执行（§6），包不能抬高。

**词表覆盖不到的残余，按残余记录**：把转录发给第三方是**数据外发**，`ToolClass` 四个值里没有一项描述披露（`read` 说无外部系统、`write` 说 OpsKeeper 状态、`destructive` 说改写他人状态——都不是"把数据送出去"）。本方案不硬套，处置是：① 基础端点由运维在设置页控制（admin-only + 审计）；② 节点零凭据（llmgw 既有设计）；③ 候选包在 manifest 注释里写明出口面（Ollama 仅回环，Bedrock 仅 AWS 区域端点）。若评审认为披露需要一等词汇，那是 `core/domain/safety.go` 的 ABI 扩展，按 §11.3 的同款流程独立决策。

**升级线（写给评审的硬规则）**：若某个 provider 包的调用会改写 vendor 侧状态——上传文件、建 batch 任务、触发 fine-tune、写托管向量库——那它**就是** L3 + `destructive`，且在现状 L2 ceiling 下**装不上**（§10.1 对照 C 实测被拒）。这条线是机械强制的，不靠自觉。

### 4.3 BlastRadius：不声明

`BlastRadius` 是 k8s 语汇（`pod` / `single-ns` / `namespace` / `cluster`，`safety.go:165-172`），语义位是"一次被批准的动作最远能触达多宽"。provider 的"宽"是**花多少钱**，那是 token/金额的量纲，塞进 k8s 半径字段会让两个字段都说不清（§6.4 的预算才是它的真实载体）。所以三份 manifest 都**不写** `max_blast_radius`（`RadiusNone`），这合法（L2 允许携带，只有只读包带半径会被 `validateGovernance` 拒）。

### 4.4 GrantedScopes：空集，且空集是判定

| 位置 | 集合 | 理由 |
|---|---|---|
| 装配层现状 `aiopsdatasource.ManagerPolicy().GrantedScopes`（ceiling） | `observability.read / db.read / alert.read / topology.read` | 不为本方案改一个字（§4.5） |
| 本方案三份 manifest 的 `required_scopes`（ask） | **空集** | `core/domain/plugin.go:60-78` 的常量集里没有任何一项描述"以运维的模型凭据发起一次补全"；`observability.read` 是读指标库、`db.read` 是读数据库，抄任何一个都是 scope 膨胀（§10.1 对照 B 实测：这种膨胀**会被现状 policy 放行**，只能靠评审挡） |

**空 ask 的代价要如实写**：`Scopes.Satisfies` 对空集恒真，所以"不给 scope"这个杠杆对 provider 包**不存在**——运维拒绝一个 provider 包的手段只剩两个：不装它（目录 + 编译期 activator，都是运维控制的），不配置它（§3.6 的降级）。§11.1 给出第三个杠杆（新增 `llm.call` scope）的独立 change 决策点，并说明为什么 provider 与 notifier 不同、**没有**"用不授予表达收紧"的替代路线——因为 ask 为空时无物可扣。

### 4.5 per-kind ceiling：provider 为什么不需要（与 4.5 的差异）

4.5 为放行 L3 的通知包提出 `kernel.Policy.KindCeilings`（已获 2026-10-23 评审裁决接受为切片 S-1）。本方案**不依赖**它：

| 候选 | 判定 |
|---|---|
| provider 一律 L2，沿用现状 `ManagerPolicy()` 的 `MaxSafetyLevel: L2` | **采用**。实测：三份草案在现状 policy 下全部准入（§10.1），**零内核改动、零策略改动** |
| provider 抬到 L3 + `WithKindCeiling(KindProvider, L3)` | 拒绝：① 词表上站不住（§4.2）；② 七个内置 provider 今天就在每个部署里无闸门运行，抬到 L3 意味着**每个现有部署都要先合入内核改动才能继续拥有模型能力**——把一个已经普遍存在的能力变成需要新批准的能力，是方向反了；③ 与 S-1 的评审耦合，无谓地阻塞本任务 |
| **不做**（provider 不接内核） | 拒绝：`provider` 继续是"加模型供应商要改编译内代码"的扩展点，4.1 §6 的缺口行永远关不掉 |

**与 S-1 的兼容性**（写给将来）：S-1 合入后，装配层换成 `notifier.NotifierPolicy()`（= ManagerPolicy + `KindCeilings{notifier: L3}`），per-kind 条目**只能收窄**，`KindProvider` 未列即回落全局 L2——本方案的三份 manifest 在两种装配下命运相同。这是把 provider 定在 L2 的另一个好处：它对策略演进是**中立**的。

### 4.6 `ProviderPolicy()` 草案（仅作 provider 宿主包的显式声明，不进装配层）

```go
// ProviderPolicy is the admission ceiling a host would use if it hosted
// nothing but model providers.
//
// It is written out even though the assembly layer keeps passing
// aiopsdatasource.ManagerPolicy() today, for two reasons. First, a provider
// package asks for no OpsKeeper scope at all — its credential is resolved by
// the host per call, not granted through the scope system — so the honest
// grant set here is empty, and an empty set is a decision worth having in
// type rather than in a review comment. Second, it is the narrow policy the
// §10.1 control B measurement points at: ManagerPolicy grants
// observability.read and db.read, which is wide enough to admit a provider
// package that asks for them (scope inflation). A host built with this
// policy refuses that package mechanically.
//
// MaxSafetyLevel stays L2: see provider-plugin-plan.md §4.2 for the
// line-by-line reading of core/domain/safety.go that puts a model call at
// "mutates OpsKeeper state" (the spend ledger) rather than at either
// "read-only" or "mutates external systems".
func ProviderPolicy() kernel.Policy {
	return kernel.Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL2,        // 与 ManagerPolicy 相同
		MaxBlastRadius: domain.RadiusSingleNS,  // 与 ManagerPolicy 相同
		GrantedScopes:  domain.Scopes{},        // ← 唯一的差别：provider 一个 scope 都不该要
	}
}
```

它的两个合法用途：① provider 宿主包的单测里构造宿主（与 `datasource.NewHost` 同位）；② 若评审在 S-1 之后决定要 per-kind scope 收窄，它就是那一份输入。**它不替换 `cmd/opskeeper/main.go:1711` 的 `pluginhost.New(..., ManagerPolicy(), ...)`**——替换会让 datasource/adapter 包被空授予集拒绝。

## 5. 既有七 provider 的兼容迁移路径

### 5.1 阶段划分

迁移必须落在可独立回滚的切片上，因为 `providers.go` 与设置层都在本任务的非目标清单里。

| 切片 | 内容 | 是否碰 `providers.go` | 是否碰内核 | 回滚 |
|---|---|---|---|---|
| **B1** | 新增 `core/pig/pigmodel/kernel.go`：`Plugin*` 包名常量（七个）、`Provider` 贡献体（§3.4）、`reservedIDs`、`manifestFor()`、`Backends`、`Activate()`、`Resolve()`、`Set`。七个工厂从 map 字面量**原样搬进** `Backends` 装配，函数体逐字不动 | ✗（只读） | ✗ | 删掉 `kernel.go` 与装配调用；map 仍是唯一生效路径，行为逐位不变 |
| **B2** | 装配层在 `ph.Load(ctx)` **之后**调 `providermodel.Activate(ph.Kernel(), backends)`（保留名检查在此执行）；`lookupProviderFactory` 改为"先查内核、未命中回落旧 map"（回落打一条 Warn） | ✓（仅 `lookupProviderFactory` 一个函数体） | ✗ | 关掉装配调用即回纯旧路径 |
| **B3** | `lookupProviderFactory` 只读内核；删除 `providerFactories` map 与 `RegisterProviderFactory`（或改为返回"经内核注册"的错误，指路 `Activate`） | ✓ | ✗（`WithKindCeiling` 不需要，见 §4.5） | git revert；此片之后 `pigmodel` 里不再有第二个注册表 |
| **C1** | 设置目录与 UI 消费 descriptor：`allProviderKeys()` 改为"内置清单 ⊕ 内核已激活条目"，修复 F3 的判定顺序（先读 base_url/默认端点，再判 `RequiresAPIKey`），前端 provider 卡片由目录驱动 | ✗ | ✗ | 独立 change（触碰 `core/domains/biz/setting/llm.go`、`core/domains/model/setting/model.go`、`web/src/pages/settings/LLM.tsx`） |
| **C2** | 线型归一：`apiFor()` 改为读内核条目的 descriptor（或删除该 switch、由工厂自带线型），消掉 F2 的 gemini 分裂 | ✗ | ✗ | 独立 change（`core/domains/llmpig/pigsettings.go`，行为变化需评审） |

B1+B2 合入期间，**默认部署**（`OPSKEEPER_MANAGER_PLUGINS_DIR` 为空）行为由 B2 的回落路径保证不变；B3 是把回落删掉的那一刀。B1/B2 **不需要**任何内核改动——这是与 4.5 最大的执行差异（4.5 的 B1 前置是 S-1）。

### 5.2 默认部署行为不变：七条论证

| # | 不变量 | 论证 |
|---|---|---|
| 1 | **配置面不变** | 设置键、`allProviderKeys()`、`system_settings.llm.*` 的行、env 兜底全部不动（那是 C1 的事）。存量 `openai_api_key` 等行不需要迁移 |
| 2 | **工厂实现不变** | 七个工厂函数体**逐字搬迁**，仍是 `ai.NewAnthropicProvider` / `ai.NewGoogleProvider` / `ai.NewOpenAIProvider(ai.DetectCompat(id, base))` 的组合；`geminiBaseURL` 的版本段补齐逻辑一字不动 |
| 3 | **解析错误语义不变** | `ErrUnknownProvider` / `ErrNotConfigured` / `ErrModelNotOffered` / `ErrNoFallback` 的文本与触发条件不动；B2 之后未知 id 的来源从"map 查不到"变成"内核查不到"，错误串仍是 `pigmodel: unknown provider: <id>` |
| 4 | **凭据生命周期不变** | `getKey` 闭包逐调用重读设置（`registry.go:233-239`）、`StreamOptions.APIKey` 逐调用携带、`Invalidate` 在改 base_url 时由宿主调用、`Close` 释放连接池——四条全部不动 |
| 5 | **缓存策略不变** | `Registry.cache` 按 id 缓存 provider、模型是逐调用字段——B 片不触碰 `Registry` 的任何字段 |
| 6 | **观测与账本不动** | `WithCallObserver(prom.ObserveLLMCall)` 的接缝、指标标签、`llm.InMemoryBudget` 的唯一实例（`main.go:4398` 的"两处共享同一实例"注释）全部保持；§6 的闸门是**新增**在接缝上，不改既有计数 |
| 7 | **既有测试零修改断言通过** | `core/pig/pigmodel/providers_test.go`、`registry_test.go`、`smoke_test.go` 在 B1/B2 后不需改任何断言；新增 `TestTheSevenBuiltinProvidersResolveIdenticallyThroughTheKernel`（同一 `ProviderConfig` 分别走旧 map 与内核，断言构造出的 provider 类型、base_url 规整结果、`RegisteredProviders()` 顺序全等）作为等价性的活锁 |

另附一条**不会**发生的事：`kind.go` 不改、`ManagerPolicy()` 不改、`llmgw`/`higress` 不改、设置层与前端不改。

### 5.3 代码骨架建议（不在本任务合入）

```
core/pig/pigmodel/
├── providers.go        （只读引用；B2 改 lookupProviderFactory，B3 删 map）
├── kernel.go           （B1 新增）
│   ├── PluginOpenAI / PluginAnthropic / PluginZhipu / PluginGemini /
│   │   PluginDeepSeek / PluginKimi / PluginCustom   —— 包名，与 provider id 分开
│   ├── reservedIDs = [...]string{...}               —— §3.3 的保留名清单
│   ├── Provider{ID, Label, DefaultBaseURL, RequiresBaseURL, RequiresAPIKey, Factory}
│   ├── manifestFor(plugin string) pluginmanifest.Plugin   —— L2 / write / 空 scope
│   ├── Backends{OpenAI, Anthropic, Zhipu, Gemini, DeepSeek, Kimi, Custom ProviderFactory}
│   ├── Activate(host *kernel.Host, b Backends) (Set, error)  —— 含保留名与 targets 检查
│   ├── Resolve(reg *kernel.Registry) Set
│   └── Set{ map[domain.ProviderID]Provider } + Set.Factory(id) / Set.Descriptors()
└── registry.go         （不改；provider() 里的 lookup 走 Set）

core/manager/biz/aiops/provider/          （与 datasource 同位；C1 时由设置目录消费）
├── policy.go           —— ProviderPolicy()（§4.6）
└── host.go             —— Backends 装配、Activate 调用点、targets==[manager] 检查

cmd/opskeeper/main.go
├── +1 行（B2）：modelRegistry 构造之前 providermodel.Activate(ph.Kernel(), backends)
└── 注意装配顺序：ph.Load(ctx)（第三方包）→ provider Activate（一方），保留名检查兜底
```

**禁项**（写在这里是为了让评审能直接判）：

- 不在 `providers.go` 里给某个 provider 加 `if` 分支；差异化实现一律是新包、新 id。
- 不把候选包的客户端塞进 `core/pig/pigmodel`（`core/pig/go.mod` 是 OpsKeeper 与 PiG 之间的**唯一**边界，把 vendor SDK 放进去等于让所有模块传递依赖它）。候选包的 HTTP/SigV4 代码放包内，经 `ProviderFactory` 闭包注入。
- 不在 `providers.go` 里新增"扫某个目录自动注册"的旁路；第三方包的唯一入口是 `pluginhost.Load` + 编译期 `RegisterActivator`（OpsKeeper 不在运行时加载第三方 Go，`core/manager/middleware/pluginhost/host.go` 的包注释就是这条不变量）。
- 不为 provider 新增第七个 Kind，不改 `kind.go`。
- 不把任何密钥、base_url 或模型清单写进 manifest（`KnownFields(true)` 会在解码期拒绝，这是特性不是缺陷，见 §6.1）。

## 6. 治理方案：凭据、费用、速率、并发、超时

### 6.1 凭据环境作用域：四条不变量

任务原文要求"凭据保持环境作用域（不入 manifest / 不入库）"。落到本仓的现状与方案，是四条可检验的不变量：

| # | 不变量 | 现状证据 | 迁移后 |
|---|---|---|---|
| I-1 | **宿主逐调用解析，包不持有、不缓存** | `registry.go:233-239` 的 `getKey` 闭包每次调用重读设置；`registry.go:173-179` 用 `StreamOptions.APIKey` 逐调用携带密钥，正是"轮换不停机"的设计（`settings.go` 包注释） | 不变。`ProviderFactory` 的第三参就是 `getKey`，包若缓存密钥即违背契约，评审按此判 |
| I-2 | **manifest 里没有凭据字段，且加不进去** | `sdk.Decode` 的 `KnownFields(true)`（`sdk/manifest.go:60-66`）把任何未知键当解码错误 | 不变。provider 包**无法**声明密钥，这是闭合 schema 给的免费保证 |
| I-3 | **内核注册表里没有凭据，inventory 不会打印它** | 注册的 `Impl` 是贡献体（工厂 + descriptor），`kernel/inventory.go:6-12` 只渲染类型名，理由原文就是"an inventory that rendered implementations would be a place that can print a DSN or an API key" | 不变 |
| I-4 | **节点零凭据** | `llmgw/llmgw.go:1-14`："either the provider key travels to the node, or the request does… the request is the one that travels" | 不变，并由 §3.5 的 targets 规则机械保证 |

**"不入库"的诚实说明**：provider 密钥今天存放在 `system_settings` 表（admin-only 写入、`*_api_key` 默认脱敏、审计只落 `value_digest` 与长度），env 作为兜底种子。本方案**不新增**任何凭据存储：不进 manifest（I-2）、不进内核注册表（I-3）、不进插件包、不落节点（I-4）、不进 PiG 的 `auth.json`（`pigsettings.go` 包注释明确写了 OpsKeeper 不用 PiG 的文件凭据模型）。若评审想把 provider 密钥迁到 `core/domains/biz/secret` 的凭据库（那里已有按类型注入 `InjectEnv` 的机制，见 §8），那是独立的凭据治理 change，不在本任务。

### 6.2 执行点：唯一收口在 `pigmodel.Registry`，一个账本一个写者

这是本方案唯一的结构性新增，因此单独做取舍记录。

**问题陈述**：四道闸门（预算 / 速率 / 并发 / 超时）今天只有节点路径全有（F4）。manager 侧的五条一次性路径（评测 judge、查询翻译、闭环 LLMCaller、RCA 摘要、chat2query）与 agent 内核路径，要么只查不记，要么什么都不查。同时 llmgw 的 `Completer` 就是 `modelRegistry` 本体（`main.go:1143`），所以在网关上再加一层不能覆盖其余路径。

| 候选 | 做法 | 收益 | 代价 / 失败模式 | 判定 |
|---|---|---|---|---|
| **A. Registry 缝**（`pigmodel.WithSpendGate(g)`，与既有 `WithCallObserver` 同位同形） | 在 `Registry.Complete` 内、`Resolve` 之后 `model.Provider.Stream` 之前执行 Check / 并发获取 / 超时包裹，settle 之后 Record | **每条 manager 路径都经过**（`Completer` 是所有消费者的唯一注入口，`chat.go:81-88` 的注释就是为此写的）；resolve 之后才知道 provider id，per-provider 维度在这里免费获得；节点路径顺带受益 | `pigmodel` 增加一个可选依赖（接口在 `core/pig` 内声明，不引 metrics/DB）；与 llmgw 的双重 Check 幂等无害，但 **Record 必须只有一个写者**（G-1） | **采用** |
| B. 包内自限 | 每个包自己实现预算/速率 | 零宿主改动 | `core/domain/plugin.go` 对 `ToolLimits` 的教义原文："**A limit declared and not enforced would be worse than no limit at all**"。一个包不能给自己设上限，被攻陷的包第一个删的就是自己的限流器 | 拒绝 |
| C. 只在 llmgw 加 | 网关已是现成的闸门 | 改动最小 | 只覆盖节点路径；manager 一次性路径照旧裸奔（F4 正是这么来的） | 拒绝 |
| D. main 里给每个消费者包一层 decorator | `otelgenai.NewClient` 旁边再造一个 | 不动 `pigmodel` | 消费者有六处且在增加，漏一处就是旁路；llmgw 拿的是 `modelRegistry` 本体，绕不开要改它 | 拒绝 |
| **不做** | 维持现状 | 零成本 | 4.6 的"费用/速率治理"要求落空；一个第三方 provider 包可以以任意价位、任意并发、无限时长地花运维的钱 | 拒绝 |

**一个账本一个写者（G-1 的解法）**：`*llm.InMemoryBudget` 仍是**唯一**实例（`main.go:4398` 的不变量保持）。A 落地后，集群级 `Record` **移交**给 Registry 的 gate（它在 settle 后拿得到确切的 in/out token）；llmgw 保留它自己的 `Check`（节点在集群还有余额时也可因自身配额被拒）与**节点归因账本 / 降级**（`AttributedBudget` / `Degrader`，那是它独有的维度）。验收断言写死：**同一次调用只被 Record 一次**（用 `InMemoryBudget.Used()` 在一条节点路径请求前后断言增量恰好等于该次 usage）。这条不做，闸门上线第一天就会把账本记成双倍，运维的日上限提前半天烧完——比没有闸门更糟，因为它看起来在工作。

**超时的措辞**：`llmgw/callbounds.go` 的注释明确警告"两个分开写的调用点会让 streaming 答 500、buffered 答 504"。Registry 的 gate 复用**同一个** `CallBounds` 值与**同一个** `timeoutError` 措辞，两处包裹的结果是 `min(同值, 同值)`，错误文本不变。

### 6.3 manifest 声明位：今天不存在，且是被设计掉的

任务原文问"预算上限、并发/速率约束、超时在宿主侧的执行点**与 manifest 声明位**"。诚实的答案是：

- **执行点**：§6.2 的 Registry 缝 + llmgw 既有四道闸门（节点维度）。
- **声明位**：**今天不存在。**`sdk.Decode` 的 `KnownFields(true)` 拒绝一切新键（实测：给草案加 `spec.limits:` 即被拒）；`spec.tools[].limits` 只作用于 tool，provider 不声明 tools。provider 的全部上限今天由**宿主配置**表达：`OPSKEEPER_LLM_DAILY_TOKEN_LIMIT`（集群/日 token）、`OPSKEEPER_LLM_EDGE_DAILY_TOKEN_LIMIT` + `OPSKEEPER_LLM_EDGE_DEGRADE_PERCENT`（节点/日 + 降级）、`OPSKEEPER_LLM_EDGE_RPM`（节点 RPM，默认 60）、`OPSKEEPER_LLM_CALL_TIMEOUT_SECONDS`、`OPSKEEPER_LLM_MAX_OUTPUT_TOKENS`。
- 这是**特性**：`plugin.go` 对 `ToolLimits` 的立场就是"声明必须是被执行的上限的 ask，宿主再 clamp"。一个包能自己写"我每天最多花 X"而宿主不执行，就是那句"worse than no limit at all"。
- 若评审要 per-package 的声明位（例如给第三方 Bedrock 包单独设日上限），§11.3 给出 ABI 扩展草案并标注为独立 change 决策点。**在扩出来之前，评审要的"声明位"就是 manifest 注释 + 宿主 env**，本文按此口径写。

### 6.4 逐项治理表

| 约束 | 今天的执行点 | 覆盖 | 本方案 | 声明位 |
|---|---|---|---|---|
| 集群/日 token 预算 | `llmgw.Budget`（节点）+ `agentkernel.Budget.Allow`（agent 内核，只查不记、估算恒 0） | 节点、agent 内核 | Registry gate 承接 Check/Record，**账本移交**（§6.2） | 宿主 env；无 manifest 位 |
| 节点/日 预算与降级 | `llmgw.NewAttributedBudget` + `Degrader` | 节点 | **不动**（节点归因是 llmgw 独有维度） | 宿主 env |
| 速率 | `llmgw.NewLimiter`（每节点 RPM） | 节点 | gate 增加 **per-provider** 令牌桶（F5：指标有 provider 标签、账本没有 provider 维度，这是补上的那一维） | 宿主 env |
| 并发 | **无** | — | gate 的 per-provider 信号量；饱和即**快失败**并返回带 provider id 的命名错误——把模型调用排进无界队列，是把积压变成故障 | 无（§11.3） |
| 超时 | `llmgw.CallBounds.ProviderTimeout` | 节点 | gate 复用同一 `CallBounds` 与同一错误措辞（§6.2） | 宿主 env |
| 输出 token 上限 | `llmgw.CallBounds.MaxOutputTokens`（clamp 不替换） | 节点 | 同上；manager 路径首次获得 clamp | 宿主 env |
| 凭据 | §6.1 四条不变量 | 全部 | 不动 | manifest 无位且不可加（I-2） |

**关于"自托管不花钱"**：Ollama 这类本地端点没有边际 token 费用，换来的是 CPU/内存与并发槽位。四道闸门对它**同样生效**——一次失控的 agent 循环照样能占住本地推理队列并拖垮同机其他负载。manifest 注释里已写明，不因"免费"豁免。

## 7. 候选 1：Ollama（自托管、无密钥）

### 7.1 选型理由与差异化能力

| 维度 | 经由 `custom`（现状唯一可行路径） | `opskeeper-provider-ollama` |
|---|---|---|
| 能否配置 | **配不进来**：`custom` 也逃不过 F3（api_key 为空即在读取层被跳过），除非运维贴一个假密钥 | descriptor `RequiresAPIKey=false`，C1 修判定顺序后免密钥直配 |
| 默认端点 | 无（`custom` 必须填 base_url，`RequiresBaseURL` 唯一为 true 的 id） | `http://127.0.0.1:11434/v1` 固化在 descriptor |
| 模型清单 | 手写 `<id>_models` 的 JSON 数组 | 可从 `/api/tags` 发现（实现切片里做，manifest 不声明） |
| 出口面 | 取决于运维填的 URL | 仅回环/内网，数据外发面最小的 provider |
| 费用 | 假密钥 + 外部 URL 会让"看起来免费"变成"其实打到了别人家" | 零边际 token 费用，但四道闸门仍生效（§6.4） |

**为什么是包而不是改 `custom`**：`custom` 的语义是"通用 OpenAI 兼容端点"，它**应该**保持无默认端点、要求显式 base_url（`core/domain/provider.go:52-59` 与 `llm.go:153-158` 都为这个语义写了专门的防御：无 base_url 的 custom 若不跳过，SDK 会静默回落 OpenAI 的端点，把运维的密钥发给错误的宿主）。给 `custom` 塞一个"若是 Ollama 就默认 11434"的特例，正是 §5.3 禁项禁止的那类 `if` 分支。

### 7.2 manifest 草案

见 `provider-manifests/ollama.yaml`。要点：`opskeeper-provider-ollama`（0.1.0）/ `vendor: ollama` / `targets: [manager]` / `safety_level: L2` / `capabilities: [write]` / `required_scopes: []` / `approval.required: false`、无 `max_blast_radius` / `install.strategy: pin` + `min_pig_version: 0.4.0`。

### 7.3 注册路径

1. 包落在 `OPSKEEPER_MANAGER_PLUGINS_DIR/opskeeper-provider-ollama/`，带 `pig-ops.yaml`（即 §7.2 那份）。
2. 装配层 `ph.RegisterActivator("opskeeper-provider-ollama", func() kernel.Contribution { return providermodel.Provider{ID: "ollama", ...} })`——`core/manager/middleware/pluginhost/host.go:125` 的既有机制。**没有 activator 的包会被 `Load` 记为 `NoActivator` 并跳过**（`host.go:196`），provider 不声明 tools，所以这条路没有"仅声明也可激活"的兜底，这是有意的：一个没有编译进实现的 provider 包，装了也不该出现。
3. `ph.Load(ctx)` 读目录 → `Admit`（现状 `ManagerPolicy()`：L2 ceiling + 空授予集，实测过）→ 宿主包检查 `targets == [manager]` 与保留名 → `Activate` → `r.Register(KindProvider, "ollama", Provider{...})`。
4. C1 合入后，设置页出现"Ollama（自托管）"卡片，填 base_url（默认已带）与模型清单；`system_settings.llm.*` 新增 `ollama_base_url` / `ollama_models` / `ollama_default_model` 三行（`ollama_api_key` 不出现——descriptor 声明不需要）。
5. 运维把它设为 `default_provider`，或在会话/工具里 pin `provider: ollama`；`Registry.Resolve` 走既有四分支，零改动。

### 7.4 测试方案

| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | `ai.DetectCompat("ollama", "http://127.0.0.1:11434/v1")` 的兼容档与预期一致 | 快照断言（`max_tokens` vs `max_completion_tokens` 等怪癖由 PiG 推导，本包不加分支） |
| Unit | 空 APIKey + 有 BaseURL 的 `ProviderConfig` | `Configured() == true`；`getKey` 返回空串不报错（本地端点不带鉴权头） |
| Integration | `httptest` 假一个 OpenAI 兼容 `/v1/chat/completions`（fixture 放 `core/harness/fixtures/provider/ollama/`） | 捕获的请求与契约逐字段一致，且**没有** `Authorization` 头 |
| Integration | `/api/tags` → 模型清单 | 清单进 descriptor，`GET /v1/models` 可见 |
| E2E（DevBox） | 本机起 Ollama，跑一次只读诊断 turn | 端到端回答 + `prom.LLMTokensTotal{provider="ollama"}` 有计数（闸门对本地端点也记账） |
| Failure | 11434 无监听 / 连接拒绝 / 超长首 token | 错误可分类、不吞错；超时由 gate 的 `CallBounds` 截断 |
| Gate | 装包但不配置 | 启动成功、`ErrNotConfigured`、inventory 有 `provider/ollama` 条目（§3.6 的降级语义） |
| Gate | 同进程再装一个注册 `ollama` 的包 | `ErrDuplicate`，启动失败并点名两个包 |

### 7.5 回滚方案

回滚因果链（精确版）：

1. **先切默认**：把 `default_provider` 从 `ollama` 切回原有 provider（若有会话 pin 了 `provider: ollama`，同样处理）。这一步不能省——不切的话，卸载后每个未 pin 的 turn 都会拿到 `ErrNoFallback` / `ErrNotConfigured`。
2. 从 `OPSKEEPER_MANAGER_PLUGINS_DIR` 移走（或禁用）`opskeeper-provider-ollama`。
3. 重启 Manager：`ph.Load` 不再激活，内核里 `provider/ollama` 条目消失。
4. 设置表里 `ollama_*` 三行仍在，但既无内核条目也无内置清单项（C1 的目录是"内置 ⊕ 内核"），设置页不再显示该卡片，`Resolve("ollama")` 返回 `ErrUnknownProvider`——**可见失败，不是静默换实现**。
5. 验证：`ph.Kernel().Inventory()` 的 `kind_counts` 中 `provider` 数减一；其余七个 provider 的条目与解析行为逐条比对无变化；`make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` 全过；`InMemoryBudget.Used()` 增量与卸载前的用量曲线连续（账本不回滚，只有实现回滚）。

**回滚不变量**：卸载不触碰七个内置 provider、不改 `providers.go`、不动 llmgw。**已产生的用量记录与账本不回滚**——钱花出去的记录是事实，回滚的是实现不是历史。

## 8. 候选 2：AWS Bedrock（云厂商、SigV4 签名）

### 8.1 选型理由与差异化能力

| 维度 | 经由 `custom` | `opskeeper-provider-bedrock` |
|---|---|---|
| 鉴权 | **表达不了**：custom 的前提是 bearer 头；Bedrock 每个请求要 SigV4 签名 | 包内签名器，凭据三元组 access_key_id / secret_access_key / region |
| 线型 | OpenAI Completions；Bedrock 不是这个形状 | `ConverseStream` → PiG stream 的包内翻译层 |
| 模型名 | 任意字符串 | `anthropic.claude-sonnet-4-5`、`amazon.nova-*`、`us.anthropic.…` inference profile，清单由运维配 |
| 必填项 | base_url | **region 必填**（descriptor 增加必填字段的第一个实例，正是 descriptor 存在的证明） |

**为什么不是 vLLM / OpenRouter / 火山方舟 / 百炼**：它们都说 OpenAI 协议、都用 bearer 凭据，`custom` 已经覆盖——为它们各建一个包等于注册七个共享同一工厂、只差默认端点的 id，那是配置不是代码（4.5 §3.1 对"entry 名 = 包名"的否决理由在这里同样适用）。Bedrock 是唯一一类"不写包就永远进不来"的候选，选它正是为了让工厂缝的存在有实证。

**为什么同时选一个自托管**：Ollama 与 Bedrock 分别落在两个极端——零密钥零外发 vs. 长期云凭据 + 真实单价。两个极端都覆盖，中间的 OpenAI 兼容地带交给 `custom`，候选集就闭合了。

### 8.2 manifest 草案

见 `provider-manifests/bedrock.yaml`。与 Ollama 同形：`opskeeper-provider-bedrock`（0.1.0）/ `vendor: aws` / L2 + `[write]` / 空 `required_scopes` / `pin` / `min_pig_version: 0.4.0`。

### 8.3 注册路径与凭据接缝

1–3 同 §7.3，id 换成 `bedrock`，`DefaultBaseURL` 为空（由 region 推导端点）、`RequiresAPIKey` 语义由"必填 region + IAM 凭据"取代。
4. **凭据接缝只描述接口，不改实现**：`core/domains/biz/secret/credtype.go` 已内置 `aws` 凭据类型，其 `InjectEnv` 规则把 `access_key_id/secret_access_key/region` 注入为 `AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_DEFAULT_REGION`。本包在该**同一接缝**上读取（宿主按调用解析、注入环境、不落盘），不新增凭据类型、不新增凭据存储、不把凭据写进 manifest（I-2）。把该接缝从 cloud_bash 执行器接到 provider 构造，是 §12 S-4 的实现内容，本任务不写。
5. 设置卡片由 C1 的目录驱动：`bedrock_region`（必填）、`bedrock_models`、`bedrock_default_model`；`bedrock_api_key` **不出现**，凭据走凭据库按名绑定。

### 8.4 测试方案

| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | SigV4 签名对固定凭据/固定请求的可复算 | 与独立实现的期望签名逐字节一致（签名错是 403，不能靠联调发现） |
| Unit | `ConverseStream` 帧 → PiG 消息流的映射（含 tool_use / stop_reason / usage 双轴） | 字段级快照比对 |
| Unit | usage 双轴（input/output token） | 两轴都进 `ReplyUsage` → 账本两轴都记（F5） |
| Integration | 假 Bedrock 端点（fixture `core/harness/fixtures/provider/bedrock/`）跑流式契约 | 捕获的请求头含正确 SigV4，路径含 region 与 model id |
| Throttling | 429 / `ThrottlingException` | 错误可分类、可被重试路径识别；**不**被当成成功 |
| Gate | 只配 region 不绑凭据 | `ErrNotConfigured`，不拨号（I-1 的反面断言） |
| Gate | `targets: [manager, edge]` 的恶意/误写变体 | 宿主包拒绝并点名（§3.5，实测见 §10.1 对照 D） |
| E2E（可选，DevBox） | 真实 key 打一次 `anthropic.claude-*` | 回答 + 指标/账本两侧都有数 |

### 8.5 回滚方案

1. **先切默认**（同 §7.5 第 1 步；Bedrock 有真实单价，这一步还是止损点）。
2. 移走包目录、重启，`provider/bedrock` 条目消失。
3. 凭据库里的 `aws` 凭据**不删**——它可能同时被 cloud_bash 用；本包只解除引用。
4. 验证同 §7.5 第 5 步，另加：卸载后 24 小时内 `LLMTokensTotal{provider="bedrock"}` 无新增（确认没有旁路残留）。
5. **外部副作用不可撤销**：已发生的 Bedrock 计费不随回滚消失。回滚文档第一句写明"回滚实现不回滚账单"。

## 9. 与 higress / llmgw / opskeeper-sre-* 的协同边界（只描述接口）

| 对象 | 本方案与它的边界 | 接口（只描述，不改） |
|---|---|---|
| `core/domains/server/llmgw`（节点模型端点，4.2 归保留 core） | **不改一行。**它是节点路径的认证 / 预算 / 速率 / 超时 / 降级执行点，本方案不重复它的维度，只按 §6.2 把集群级 Record 移交给 Registry gate | `llmgw.Options{Auth, Completer, Budget, Limiter, Bounds, DefaultModeler}`；`Completer` = `*pigmodel.Registry`。provider 包对它不可见 |
| `core/manager/higress`（AI 网关，4.2 归保留 core） | **不触碰。**它管理的是 consumer/apikey 与自己的审计链、登录限流，与"OpsKeeper 用哪个模型供应商"是两个问题 | 它不消费 `KindProvider`；若将来要把"网关后面的模型"也接到本扩展点，那是 higress 自己的 change |
| `opskeeper-sre-*` 五个工具包 | 工具包不直接调 provider；它们经 agent 循环间接消费。provider 换实现不改变任何工具的 manifest | 无接口耦合；唯一共享的是 `pigmodel.Completer` 注入面 |
| `core/manager/middleware/pluginhost` | 装配层加一个 `RegisterActivator` + 一次 `Activate` 调用（§5.3），`Load` / `Inventory` / 回滚路径全部复用 | `pluginhost.Host.RegisterActivator(name, Activator)`、`ph.Kernel()`、`ph.Load(ctx)` |
| `pig-runtime-adapter`（3.x） | PiG 进程驱动的工具对账、准入、审计与本方案互不重叠；provider 是**控制面**能力，不经 PiG 扩展 | 无 |
| `runtime-management-gap`（2.x） | llmgw 的 runtime 身份/健康不因 provider 插件化改变 | 无 |

## 10. 通用闸门（每个候选都必须通过）

### 10.1 manifest 闸门（已实测）

按 `tasks.md` 4.10 / 4.11，对 `provider-manifests/*.yaml` 逐个跑 `sdk.Decode → sdk.Validate → sdk.Admit`（临时驱动程序，未进仓；复核方式与 4.5 §11.1 相同）：

**正式草案**

| 文件 | name | level | capabilities | required_scopes | radius | strategy | 现状 `ManagerPolicy()`（L2） | L1 ceiling | L0 ceiling |
|---|---|---|---|---|---|---|---|---|---|
| `builtin-providers.yaml` | `opskeeper-provider-openai`（七个 id 各一份） | L2 | `[write]` | `[]` | — | pin | **过** | 拒 | 拒 |
| `ollama.yaml` | `opskeeper-provider-ollama` | L2 | `[write]` | `[]` | — | pin | **过** | 拒 | 拒 |
| `bedrock.yaml` | `opskeeper-provider-bedrock` | L2 | `[write]` | `[]` | — | pin | **过** | 拒 | 拒 |

L1/L0 下的错误文本均为 `pig-ops.yaml: spec.safety_level: plugin is L2, target policy allows at most L1/L0`。

**这一列是 §4.5 的实测依据**：provider 是六个扩展点里唯一一个在**现状 policy 下零改动准入**的——4.5 的六份草案在 L2 ceiling 下全部被拒、必须等 S-1，本方案三份全部通过。也就是说本方案不阻塞、不被阻塞。

**负对照（临时变体，未进仓，只记录结果）**——这四行是本方案"声明是承重的"这一说法的证据：

| 对照 | 变体 | 结果 | 说明 |
|---|---|---|---|
| A | 谎报 `L1 + [read]` | Decode/Validate **过**，L1 ceiling 下也**过** | 闸门**不**能识别低报——等级是评审判断，不是机械检查。这正是 §4.2 必须把逐句论证写进 manifest 注释的原因 |
| B | `required_scopes: [observability.read, db.read]` | 现状 `ManagerPolicy()` 下**过** | scope 膨胀同样不被现状 policy 挡（它的授予集本来就有这两项）。§4.6 的 `ProviderPolicy()`（空授予集）能机械挡住它——这就是那个构造函数存在的第二个理由 |
| C | 升级为 `L3 + [destructive] + single-ns` | 现状 L2 ceiling 下**拒**：`plugin is L3, target policy allows at most L2` | §4.2 的"升级线"是机械强制的：想改写 vendor 侧状态的包**装不进来**，直到运维显式抬 ceiling |
| D | `targets: [manager, edge]` | 现状 manager 宿主**过** | `Host.Admit` 只查 `RunsOn`，挡不住含 edge 的包 → §3.5 的宿主侧规则与切片 S-2 是必要的 |

### 10.2 其余闸门

| 闸门 | 内容 |
|---|---|
| **精确版本** | `metadata.version` 用 semver 锁（`0.1.0` / `1.0.0`），不得 `latest` / `main` |
| **scope 最小化** | 三份草案 `required_scopes` 全部为空集；多写一个就应在评审被打回（对照 B 证明闸门挡不住，只能评审挡） |
| **安全级别** | provider 一律 L2 + `[write]`；写 L1 即为**误述**，评审应直接拒（对照 A 证明低报更便宜，所以要靠评审） |
| **兼容范围** | 第三方包写 `min_pig_version`（对 provider 是承重字段：工厂签名编译在 `github.com/MichaelKinsy/PiG/ai` 上，本仓基线 v0.4.0）；合成的一方 manifest 不写（包与二进制同构建） |
| **目标面** | `targets` 恰为 `[manager]`；含 edge 即拒（§3.5） |
| **测试** | unit + integration + gate 三层；integration 在 CI 用假端点跑真 HTTP/签名契约 |
| **签名** | 经 `core/floor/pluginmanifest/signing.go` 通道，签名文件随包分发；`Profile.Policy()` 已把 `AllowUnsigned` 置 false |
| **回滚文档** | 含"先切默认 / 卸载 / 验证"三段，且必须写出**已发生费用与已产生用量不可回滚** |
| **灰度** | provider 包不做 canary（`install.strategy: pin`）：两批回答同一个问题的模型不同，诊断结论就不可比 |
| **闸门命令** | `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` |

## 11. 独立 change 决策点（本方案不落，沿用 4.5 §12 的写法）

### 11.1 新增 scope：`llm.call`

| 项 | 内容 |
|---|---|
| 触发场景 | 运维要用 policy **拒绝**某个 provider 包（对照 B 证明现状挡不住 scope 膨胀；§4.4 证明空 ask 使"不给 scope"这个杠杆对 provider 不存在——ask 为空时无物可扣，这与 notifier 可用"不授 `alert.write`"表达收紧是关键差异） |
| 建议 | `ScopeLLMCall Scope = "llm.call"`，语义"以运维的模型凭据发起一次补全" |
| 位置 | `core/domain/plugin.go` 常量块末尾（PR #105 已在 `ScopeAlertWrite` 之后新增 `ScopeCloudRead`，本 scope 若立项应排在其后）——#105 也正好佐证了本行的判定：加 scope 是确实会发生、但每次都是一次显式立项的 ABI 变更 |
| 影响面 | ① domain 常量（`plugin.go` 文件头明确写了常量属于插件 **ABI**，改它是破坏性变更，须与 wire 字段同等级别门控）；② 三份 manifest 的 `required_scopes` 改为 `[llm.call]`；③ `ProviderPolicy().GrantedScopes` 加这一项；④ `core/floor/pluginmanifest/profiles.go` 金融档的 `NotGranted` 需显式点名，否则该 scope 在金融档上落空 |
| 判定 | **独立 change**。在它落地前，运维拒绝 provider 包的手段是"不装 / 不配"（§4.4），两个都是真实杠杆，不为一个尚未出现的用例先扩 ABI |

### 11.2 嵌入模型：不归 `KindProvider`，本期不设计

按 4.1 §7 问题 3（任务描述称结论 3，硬约束）：**嵌入模型维持编译内，本方案只做对话补全类 provider**。记录两个事实，不展开设计：

- 阻塞点不在 Kind 封闭集合：`KindProvider` 命名的是一类能力，entry 名可以携带模态（`embedding/<id>` 在同一 Kind 内合法且不撞名）。真正的阻塞是**设置形状**（嵌入模型在 `core/base/pkg/embedding` 有自己的客户端与配置，没有 `<id>_*` 设置行）与**消费者**（RAG 检索走的是向量库客户端，不经 `pigmodel.Completer`）。
- 4.1 §4 ⑧ "嵌入模型"行标注"待定，见 §7 问题 3"，本方案不改变该行的"待定"状态。

### 11.3 manifest 的资源声明位（`spec.limits`）

| 项 | 内容 |
|---|---|
| 触发场景 | §6.3 的实测结论：`KnownFields(true)` 使 provider 的费用/速率/并发/超时**没有** manifest 声明位；若要给第三方包单独设日上限，必须在 schema 里长出来 |
| 建议 | `spec.limits: { daily_tokens, requests_per_minute, max_concurrent, timeout_seconds, max_output_tokens }`——形状对齐既有 `ToolLimits` 的两个已落地字段，语义是 **ask**，宿主 clamp（与 `ToolLimits` 的"声明是 ask、宿主执行"完全同构） |
| 位置 | `core/domain/plugin.go` 的 `PluginSpec` + `sdk/manifest.go` 的校验 + **词表闭合测试**（仿 `pluginmanifest/limits_test.go` 的 `TestTheToolLimitVocabularyIsClosed`，有人加字段时立刻失败并说明三件事） |
| 前置 | §6.2 的 gate 先落地——没有执行点的声明位就是 `plugin.go` 明文反对的那种"看起来像保证的字段" |
| 判定 | **独立 change**。落地前 per-package 上限由宿主 env 全局表达 |

## 12. 实现切片建议（不在本任务合入）

| 切片 | 内容 | 触碰文件 | 前置 | 验收 |
|---|---|---|---|---|
| **S-1** | B1：`pigmodel/kernel.go` + 七方工厂搬迁 + 等价性测试 | `core/pig/pigmodel/kernel.go`（新） | — | `providers_test.go` / `registry_test.go` / `smoke_test.go` 断言零修改通过；`make module-check` 通过 |
| **S-2** | B2：装配层 `Activate`（含 §3.3 保留名与 §3.5 targets 检查）+ `lookupProviderFactory` 内核优先回落 | `cmd/opskeeper/main.go`、`core/pig/pigmodel/providers.go`（仅 lookup 一个函数） | S-1 | 默认部署启动日志与改动前逐行相同；装一个想接管 `openai` 的包 → 启动报错点名保留 id；装一个 `targets: [manager, edge]` 的包 → 报错（§10.1 对照 D） |
| **S-3** | B3：删除遗留 map 与回落；`RegisterProviderFactory` 移除或改为指路错误 | `core/pig/pigmodel/providers.go` | S-2、评审通过 | `grep -n "providerFactories" core/pig/pigmodel/` 只剩内核路径 |
| **S-4** | 治理闸门：`pigmodel.WithSpendGate(g)`（Check / per-provider 速率 / 并发 / `CallBounds` 包裹 / Record），账本移交与 llmgw 的分工 | `core/pig/pigmodel/`（新 spendgate.go）、`cmd/opskeeper/main.go`、`core/domains/server/llmgw/`（Record 分工） | S-2 | **同一次调用只 Record 一次**（断言 `InMemoryBudget.Used()` 增量）；manager 一次性路径首次被预算/超时截断（用例：judge 打到 429 预算上限） |
| **S-5** | C1：设置目录与 UI 消费 descriptor；修 F3（先读端点再判密钥）；`web` 卡片由目录驱动 | `core/domains/biz/setting/llm.go`、`core/domains/model/setting/model.go`、`web/src/pages/settings/LLM.tsx` | S-2 | 无密钥 + 有 base_url 的 provider 出现在目录里（F3 的回归测试）；不填配置的已装包不出现 |
| **S-6** | C2：线型归一，消掉 gemini 在两条路径上的分裂（F2） | `core/domains/llmpig/pigsettings.go` | S-5、评审确认目标线型 | 同一设置下，一次性路径与交互式路径对 gemini 发同一形状的请求（契约测试） |
| **S-7** | 候选包首切片（建议顺序：ollama → bedrock） | `OPSKEEPER_MANAGER_PLUGINS_DIR` 外部目录 + 装配层 activator | S-2、S-5 | 每包独立 PR、独立版本、独立回滚；bedrock 需 S-4 的超时/并发先在 |
| **S-8** | `core/base/pkg/llm/doc.go` 的 provider 注释与 `web` 的 `LLMProviderID` 联合类型清理（F1 的收尾） | 同 S-5 | S-5 | 加 provider 不再需要改 `web/src/pages/settings/LLM.tsx` 的类型联合 |

**顺序理由**：ollama 先做——它同时验证"无密钥路径（F3）""targets 规则""ErrDuplicate"三件事且不需要任何外部凭据，CI 可全量跑；bedrock 需要 S-4 的超时/并发先在，因为它有真实单价与最慢的首 token 延迟。

## 13. 与 4.1 / 4.2 一致性自查表

| 自查项 | 4.1 / 4.2 原文 | 是否一致 | 证据 |
|---|---|---|---|
| provider 归"迁移为插件，未接内核" | 4.1 §3 状态列；4.2 四分类 | ✓ 只推进"迁移"，不触碰保留 / 暂缓 / 淘汰三类 | §1.2 / §3 / §5 |
| 收口 4.1 §6 缺口行 | "`provider` 未接内核，模型供应商只能编译内增加 → 企业集成、协同 → 4.6" | ✓ §3 + §5 给出注册路径，§7–§8 给出第三方候选 | §3.1 / §5.1 |
| AI 网关 / 节点模型端点保留 core | 4.1 §4 ⑨ 对应行；4.2 归"保留 core" | ✓ 只描述接口（§9），不改 `higress` / `llmgw` 一行 | §1.2 / §9 |
| 嵌入模型不进 provider | 4.1 §7 问题 3（任务称结论 3，硬约束） | ✓ 不设计、不实现，仅以独立 change 决策点记录（§11.2），4.1 §4 ⑧ 该行"待定"状态不变 | §1.2 / §11.2 |
| 内核六 Kind 封闭 | `core/floor/kernel/kind.go` | ✓ 复用 `KindProvider`，不新增字符串；`kind.go` 不在触碰清单 | §1.3 / §5.3 禁项 |
| `Activate` 是注册唯一入口 | 4.3 §2.2 关键不变量 | ✓ 七个内置与第三方包一律经 `Host.Activate`，无并行注册表、无目录扫描旁路；B3 后 `pigmodel` 内不再有第二个注册表 | §3.2 / §5.1 / §5.3 禁项 |
| 同名互斥 `ErrDuplicate`，fail-closed | `core/floor/kernel/registry.go:16` | ✓ id 键即互斥键，无覆盖语义；"想共存就换 id"的显式出路已给出 | §3.2 |
| 配置期单激活 | 4.3 §3.3 / 4.5 §3.3 | ✓ 同形；provider 特有的两点（保留名、targets）单列并说明为什么不能照抄 4.5 的"跳过" | §3.3 / §3.5 / §3.6 |
| `required_scopes` ⊆ 既有 domain 常量集 | `core/domain/plugin.go:60-78`；任务硬约束 | ✓ 三份草案全部**空集**；并实测证明膨胀变体会被现状 policy 放行、只能评审挡 | §4.4 / §10.1 对照 B |
| 需要新 scope 时给 domain 常量建议 + 标独立 change | 任务验收项 | ✓ §11.1 给出常量、位置、ABI 影响面与金融档联动，并说明为什么 notifier 的"不授予"替代路线对 provider 不可用 | §11.1 |
| 不套用 datasource 的 L1 / 只读假设 | 任务明确要求 | ✓ §4.1 论证推理为何不成立；§4.2 用 `safety.go` 原文逐句给出 L2 + write | §4.1 / §4.2 |
| 一方实现不重写 | 4.3 §1.2 / §6.3 | ✓ `datasource.go`、`ManagerPolicy()`、`llmgw`、`higress` 一字未改；provider 宿主包另立 | §4.5 / §5.2 / §9 |
| 协同面只描述接口 | 4.3 §8 对应行 | ✓ `pluginhost.RegisterActivator` / `ph.Load` / `llmgw.Options` / `secret.InjectEnv` 均只描述，不改实现 | §8.3 / §9 |
| 凭据保持环境作用域 | `tasks.md` 4.6 原文 | ✓ 四条可检验不变量 + "不入库"的诚实说明（现状在 `system_settings`，本方案不新增任何凭据存储） | §6.1 |
| 受审计文件不含内部任务编号 | `scripts/audit_open_source.py` 的 `internal task ID` 规则 | ✓ 本文与 manifest 草案均不含执行任务编号；编号只出现在 PR 描述与评审评论 | §15 |
| 中文注释与中文提交说明 | 计划任务 7.4 | ✓ 文档与 manifest 注释为中文；提交说明为中文 | §15 |

## 14. 验收清单（对应任务描述）

- [x] 接内核方案完整：注册语义（§3.1 三候选取舍、§3.2 `ErrDuplicate`、§3.3 保留名、§3.4 descriptor、§3.5 目标面、§3.6 单激活）
- [x] 七 provider 兼容迁移：分片 B1/B2/B3（+C1/C2），每片标注触碰面与回滚；B1/B2 不改 `providers.go` 主流程（§5.1）
- [x] 默认部署行为不变有论证：七条不变量 + 既有测试零修改断言通过的判据（§5.2）
- [x] 宿主 Policy 设计：四项齐备，安全级别用 `safety.go` 原文逐句论证、未套用 L1 假设，并给出 per-kind ceiling 的候选比较与"为什么不需要"（§4）
- [x] 凭据环境作用域治理：四条不变量 + 现状说明（§6.1）
- [x] 费用 / 速率 / 并发 / 超时治理：执行点五候选取舍（§6.2）、"一个账本一个写者"（§6.2）、逐项表（§6.4）、manifest 声明位的诚实结论（§6.3）与 ABI 扩展决策点（§11.3）
- [x] 每候选 manifest 草案：`provider-manifests/` 三份（§7.2、§8.2、内置参考清单）
- [x] 每候选注册路径（§7.3、§8.3）、测试方案（§7.4、§8.4）、回滚方案（§7.5、§8.5）
- [x] manifest 闸门按 `sdk.Decode → sdk.Validate → sdk.Admit` 实测，记录 L 级别对照与四个负对照（§10.1）
- [x] 与 4.1 §7（尤其问题 3）、4.2 结论一致，无越界改动（§13；非目标见 §1.3）
- [x] 实现切片仅给建议，未在本任务内合入（§12）
- [x] 受审计跟踪文件中不含内部任务编号
- [x] 中文注释与中文提交说明；PR 描述关联 OpenSpec change `plugin-architecture-recovery-plan` 任务 4.6

## 15. 提交与执行规范

- **落盘路径**：本文档 `openspec/changes/plugin-architecture-recovery-plan/provider-plugin-plan.md`；manifest 草案 `openspec/changes/plugin-architecture-recovery-plan/provider-manifests/`（`builtin-providers.yaml`、`ollama.yaml`、`bedrock.yaml`）。
- **提交语言**：本任务 PR 使用中文注释 / 中文提交说明（按计划任务 7.4）。
- **关联**：OpenSpec change `plugin-architecture-recovery-plan` 任务 4.6；执行任务编号只记录在 PR 描述与评审评论中，**不写入受审计文件**（`scripts/audit_open_source.py` 的 `internal task ID` 规则会拒绝 `BENY-\d+` / `LUM-\d+` 形态）。
- **勾选状态**：本文档为方案 v0.1，评审通过前不勾选 `tasks.md` 4.6（与 4.1 / 4.3 / 4.5 的处理一致）。
- **复核方式**：§10.1 的结果是本任务期间一次性跑过的验证，未在仓内留下测试代码——建议 S-1 落地时把它固化成 `provider-manifests_test.go`（把对照 A–D 一并固化，低报与 scope 膨胀是评审闸门，不是机械闸门，测试要断言的是"我们知道自己挡不住什么"）。

## 16. 风险与开放问题

### 16.1 风险分级

| 级 | 风险 | 触发条件 | 缓解 |
|---|---|---|---|
| **高** | "provider 是 L2" 未获评审认可（评审倾向 L3"外部副作用"论） | 与 4.5 §4.2 的 L3 推理做表面类比 | §4.2 已用词表原文逐句对；若仍被否，方案退化为 §4.5 的 S-1 路线（L3 + per-kind ceiling），迁移分片与候选设计不受影响，只有 §4.5 一节重写——**退化路径已写明** |
| **高** | S-4 的账本移交做错，双记或漏记 | llmgw 与 Registry 都 Record | §6.2 的"一个账本一个写者" + 验收断言（`Used()` 增量恰好等于单次 usage）。这条不做，闸门比没有更糟 |
| **中** | descriptor 只接内核、设置层不接（C1 不落地） | C1 被无限期推迟 | 4.1 §6 的缺口行只关掉一半。缓解：§12 把 C1 列为 S-5，并把 F3（无密钥 provider 配不进来）作为它的独立动机，即使 descriptor 不落地，F3 也该修 |
| **中** | 第三方包塞进本仓 `plugins/` | 实现者图省事 | §5.3 禁项已写；候选包落在 `OPSKEEPER_MANAGER_PLUGINS_DIR`，与 4.5 §6.2 同一治理理由（`vendor: ollama` / `vendor: aws` 的包进本仓等于替第三方实现背书） |
| **中** | Bedrock 翻译层与 PiG 升级互相拖 | PiG 0.x API 移动（`core/pig/go.mod` 自己写了"pre-stable 0.x and its API moves"） | 翻译层只在包内引用 `ai` 的稳定类型；PiG 若提供原生 `APIBedrockConverse`，包在版本号里换实现，不动 `min_pig_version` |
| **低** | 空授予集被误读为"provider 不受治理" | 评审读到 `required_scopes: []` | §4.4 已把代价如实写出（scope 杠杆不存在），并给出 §11.1 的补法 |
| **低** | `web` 的 provider 卡片仍硬编码（F1 残留） | C1 之前的过渡期 | 装了包但设置页无卡片 → 运维经 API/env 配置；S-8 收尾 |

### 16.2 待评审问题

1. **L2 还是 L3**（§4.2 / §4.5）：这是本方案与 4.5 唯一的口径分歧，也是唯一可能要求重写一节的分歧。评审若采纳 L3，则依赖 S-1（`WithKindCeiling`）先合入，本任务的"零内核改动"优势消失。
2. **保留名规则**（§3.3）：不接受第三方包接管 `openai` 等七个 id。是否有"允许显式接管"的运维场景（例如自建的 OpenAI 代理想复用存量密钥）？若有，建议以"显式配置项 + 启动横幅"而非静默跳过表达，仍需独立决策。
3. **空 `required_scopes` 是否可接受**（§4.4 / §11.1）：现状下 provider 包无法被 policy 拒绝，只能"不装 / 不配"。若评审认为这是治理缺口，`llm.call` scope 应尽早立项。
4. **账本移交的边界**（§6.2）：集群级 `Record` 从 llmgw 移交给 Registry gate，llmgw 保留 Check 与节点归因。这个分工是否接受？替代方案是 Registry 只 Check 不 Record、llmgw 继续记——代价是 manager 一次性路径永远没人记账。
5. **Bedrock 的必填字段**（§8.1）：descriptor 需要"必填 region"这类字段，是 schema 的第一次真实扩展（在 Go 结构体层面，不是 manifest 层面）。是否接受把 descriptor 的字段集定为封闭的五个（ID/Label/DefaultBaseURL/RequiresBaseURL/RequiresAPIKey）+ region 类必填项？
6. **`min_edge_version` 对 manager-only 包是否继续写**（与 4.5 §17.2 Q4 同族）：本方案照 pagerduty 先例写了，理由是目录形状一致；若评审认为"写一个永不消费的字段"更糟，删除三份草案的该键即可，不影响其余内容。

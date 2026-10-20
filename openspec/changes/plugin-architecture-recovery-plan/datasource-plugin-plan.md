# 4.3 datasource 第一批插件包方案（VictoriaMetrics / Elasticsearch / OpenSearch）

> 状态：**方案 v0.1（待评审）**。对应 `tasks.md` 4.3；评审通过前不勾选 4.3。
>
> 基线：`main@349f7d43`（本任务打开时的 HEAD）；与 4.1 `business-domain-mapping.md`、4.2 结论保持口径一致。
>
> 范围：**仅方案产出**。本文档回答"插件包长什么样、怎么注册、怎么测、怎么回滚"，并给出首个实现切片建议（不开在本任务内合入）。任何对 `core/manager/biz/aiops/datasource/datasource.go` 或 `plugins/` 包结构的真实改动，必须另开 change 并独立评审。

## 1. 目标与边界

### 1.1 目标
为 datasource 扩展点（`core/floor/kernel/kind.go` 中的 `KindDatasource`）补齐第一批第三方插件包方案，覆盖三类候选后端：

| 候选 | 信号 | 兼容协议 | 角色 |
|---|---|---|---|
| **VictoriaMetrics** | metrics | Prometheus HTTP API（PromQL 兼容 + 增量优化） | `prom` 信号的第三方实现 |
| **Elasticsearch** | logs / 通用检索 | Elasticsearch Query DSL + `_search` API | `loki` 信号的第三方实现（替代或并存 Loki） |
| **OpenSearch** | logs / 通用检索 | OpenSearch DSL（与 ES 7.x 接近） | `loki` 信号的第三方实现（替代或并存 Loki） |

### 1.2 与 4.1 §7、4.2 边界
- **4.1 §7**：嵌入模型本期编译内、MCP/技能宿主内管理、告警归 4.12 —— 本任务不引入任何新嵌入模型、不改 MCP 宿主、不动告警归类。
- **4.2 四分类**：datasource 归"迁移为插件"；本文方案严格在"第三方候选"分支内推进，"一方已完成"分支已由 `core/manager/biz/aiops/datasource/datasource.go` 现有三个 `Prom/Logs/Traces` 结构体承担，本任务不重写。
- **内核契约**：不在 `core/floor/kernel/kind.go` 中新增任何 Kind 字符串（保持六元封闭集合）。

### 1.3 非目标
- 不实现真实代码合并（除非用户单独开 change 同意首切片）；本文档给出**实现切片建议**仅作后续决策参考。
- 不覆盖 provider / notifier / collector / adapter（4.5–4.7、4.4）。
- 不动 `core/manager/biz/aiops/datasource/datasource.go` 的 `Activate/Resolve` 主流程；只在其已有的"预装后端 + 第三方并存"模型基础上扩展。
- 不在本文档中重新定义 `toolcore.{PromQuerier, LogQuerier, TraceQuerier}` 三接口；候选后端必须实现这些接口（或新增等价接口——见 §6 实现切片建议）。

## 2. 现状与扩展点

### 2.1 现状证据
- **一方后端**：`core/manager/biz/aiops/datasource/datasource.go` 中 `Prom{}/Logs{}/Traces{}` 三个结构体，分别以 `pluginmanifest.Plugin{Name: "opskeeper-datasource-prom"/"-loki"/"-tempo"}` 名义 `Activate` 到 `kernel.Host`。信号名为 `prom` / `loki` / `tempo`。
- **第三方贡献**：当前 0。
- **注册入口**：`Activate(ctx, manifest, contributor)` → `host.Registry().Lookup(KindDatasource, signal)` → 由 `Set{}.Prom/Logs/Traces()` 取出，喂给 `opskeeper-sre-observability`（12 个只读工具）的 `query_promql / query_logql / query_traceql` 三个工具。
- **审计闸门**：`ManagerPolicy()` 已设 `MaxSafetyLevel=L2`、`MaxBlastRadius=SingleNS`、授予 scope = `MetricsRO / DBRead / AlertRO / TopologyRO`。

### 2.2 第三方注册路径（必须走的形式）
任何第三方 datasource 插件必须：

1. 实现 `toolcore.PromQuerier` / `LogQuerier` / `TraceQuerier`（按其信号）之一；
2. 实现 `kernel.Contributor` 接口（`PluginName()` + `Register(kernel.Registrar)`）；
3. 通过 `kernel.Host.Activate(ctx, manifest, contributor)` 注入；信号名（`prom` / `loki` / `tempo`）由 manifest 与 `Register` 调用一致决定；
4. **信号级互斥的真实机制**：`core/floor/kernel/registry.go:68` 对同名 `(Kind, signal)` 二次 `Register` 返回 `ErrDuplicate`（fail-closed），不存在覆盖语义。互斥在装配层落实：部署期由配置（如 `datasource.prom.backend` / `datasource.logs.backend`）决定**只构造并只 `Activate` 一份客户端**——一方或第三方，二选一。同一个进程内同时激活两份会启动报错而不是覆盖。

> 关键不变量：`Activate` 是注册唯一入口；不引入并行注册表，不开 `core/manager/biz/aiops/datasource/datasource.go` 之外的"快速通道"；同信号双注册在装配期必须由配置排除。

### 2.3 Manifest 准入校验（`required_scopes` ⊆ `ManagerPolicy.GrantedScopes`）
每个 manifest 加载时按 `kernel.Policy.Check(manifest)` 校验：`manifest.Spec.RequiredScopes` 必须是 `ManagerPolicy().GrantedScopes`（当前为 `observability.read / db.read / alert.read / topology.read`）的**子集**，缺一即拒。未知 scope 同样按 fail-closed 处理（`core/domain/plugin.go:88` `Scopes.Satisfies`）。因此：
- `log.read` 等未在 `domain.Scope` 常量集（`core/domain/plugin.go:60-73`）声明的 scope 不能写进 manifest；强行写即拒。
- 第三方候选凡需新 scope，必须先扩 `ManagerPolicy.GrantedScopes`——但扩 policy 触碰"不动 `datasource.go`"红线，需用户单独决策，本任务不做。
- 本方案三个候选 manifest 的 `required_scopes` 全部严格收敛到 `observability.read`（同时含 `db.read` / `alert.read` 等可选子集由 §3.2 / §4.2 / §5.2 列出）。

### 2.3 能力清单（每个候选必须给出）
| 能力项 | 说明 |
|---|---|
| **协议适配** | 把后端原生 API（VM HTTP API / ES `_search` / OS DSL）映射到 `toolcore` 接口形状 |
| **认证** | bearer token / basic auth / mTLS 任选其一，按 manifest `required_scopes` 注入 |
| **结果规整** | 将后端响应规整为与现有客户端同一类型（`*promquery.InstantResult` / `*logquery.QueryRangeResult` / `*tracequery.SearchResult`），不引入新的返回类型 |
| **错误语义** | 透传 4xx/5xx + 超时，不吞错；超时不超 `QueryPromqlCallTimeout`（30s） |
| **指标自观测** | 上报 query latency / error rate / result point count，由宿主观测面汇总 |

## 3. 候选 1：VictoriaMetrics（metrics）

### 3.1 选型理由
- 与 Prometheus 协议兼容（HTTP API 路径 + PromQL 子集 + 扩展函数）。
- 单二进制、TSDB 紧凑、运维成本低，适合做"低成本 Prometheus 替代"。
- 信号：复用 `prom` —— **不引入新信号名**。

### 3.2 manifest 草案
详见 `datasource-manifests/victoriametrics.yaml`。要点：

- **包名**：`opskeeper-datasource-victoriametrics`
- **精确版本**：`0.1.0`（首发首版；与 manifests 一致）
- **vendor**：`victoriametrics`（上游项目名；非 OpsKeeper 一方，与"第三方候选"语义对齐）
- **scope / 安全级别 / 兼容范围**：
  - `targets: [manager]`（与一方 Prom 同位）
  - `safety_level: L1`（只读）
  - `capabilities: [read]`
  - `required_scopes: [observability.read]`（严格 ⊆ `ManagerPolicy.GrantedScopes`，见 §2.3）
  - `install.min_pig_version: "0.4.0"`（与主仓 PiG 基线对齐，见 `plugins/pig-ops/opskeeper-sre-observability/pig-ops.yaml`）
  - `install.min_edge_version: "0.8.0"`（与主仓 Edge 基线对齐）
  - 信号复用：`prom`（与一方 Prom 同名，沿用 `core/manager/biz/aiops/datasource/datasource.go` 中 `NameProm`）
- **能力声明**：仅声明 1 个贡献 `prom`，由贡献体 `VictoriaMetrics{Q: ...}` 满足 `toolcore.PromQuerier`。
- **凭据**：`vm.basic.username/password`、`vm.bearer.token`、`vm.tls.ca` 三选一，作用域限于 `observability.read`；mTLS 不引入新 scope，沿用既有 CA 注入链。
- **测试覆盖**：`unit`（协议解析）+ `integration`（连真 VM 集群，跑 PromQL range/instant）+ `contract`（与一方 Prom 客户端对比结果点对点）。
- **签名 / 回滚**：见 §7 通用闸门。

### 3.3 注册方式
- 复用 `Activate(ctx, manifest, VictoriaMetrics{Q: vmClient})`，信号名取 `NameProm`（= `"prom"`）。
- 与一方 Prom **配置期单激活二选一**：装配层根据 `datasource.prom.backend: "victoriametrics" | "prometheus"` 决定构造并激活哪一份客户端；同一个进程内同时激活两份会因 `ErrDuplicate` 启动报错（见 §2.2），不存在"后注册覆盖"。
- 启动编排沿用 `datasource.Activate`；本任务不在 `main.go` 加分支。

### 3.4 与 `opskeeper-sre-observability` 的协同
- `query_promql` 工具通过 `Set{}.Prom()` 拿到 `*vmClient`；无需改 `toolcore` 接口。
- `analyze_database_status` / `list_metric_catalog` / `correlate_incident` 等复用同一路径。

### 3.5 与 `core/domains/server/{logs,traces}` 的对接
- VM 不参与 logs/traces；本候选**不**触碰 `core/domains/server/{logs,traces}`。

### 3.6 测试方案
| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | PromQL 解析到 VM HTTP `/api/v1/query_range` 参数映射 | 与 `promquery.Client` 字节级等价 |
| Unit | VM 扩展函数（`quantile_over_time` 子集）超出 PromQL 时降级到错误而非静默截断 | 返回错误信息含 `vm_unsupported_function` |
| Integration | 连真 VM（Docker compose 启动单节点），跑 5 个 PromQL 模板 | 结果与 Prometheus 同表达 ±0（绝对差） |
| Contract | 与 `promquery.Client` 在同一 fixtures 上结果一致 | CI 卡点 |
| Failure | 网络中断 / 401 / 5xx / 超时 | 错误码映射正确、不吞错 |

### 3.7 回滚方案
- **回滚粒度**：包级别（`opskeeper-datasource-victoriametrics`）+ 配置回切。
- **回滚因果链（精确版）**：VM 包卸载 ≠ 一方 Prom 自动接回。完整步骤如下——
  1. 卸载前把 `datasource.prom.backend` 配置切回 `prometheus`（这是 VM 客户端存在的同义条件）；
  2. 从 plugin marketplace 卸载 `opskeeper-datasource-victoriametrics`；
  3. 重启 Manager：一方面装配层不再构造 VM 客户端，另一方面 `datasource.Activate` 走 `Backends.Prom != nil` 分支，构造一方 Prom 客户端并 `Activate` 到 `prom` 信号；
  4. 卸载 + 配置切回 + 重启完成后，`Set{}.Prom()` 才回到一方 Prom 客户端；如果仅卸载不切配置，`Set{}.Prom()` 会因装配层不再构造任何 Prom 客户端而解析为 `nil`，`query_promql` 工具按既定降级语义（无后端 → 不注册工具）消失。
- **回滚文档要点**：
  1. 卸载前确认 `datasource.prom.backend` 已切回 `prometheus`；
  2. 卸载后跑 `make pi-plugin-guard` 与 `make module-check`（修正：原文 `plugin-guard` 笔误，正确目标名以 Makefile 为准）；
  3. 验证 `runtime view` 的 datasource 面板只剩 `prom`（一方）；
  4. 验证 `query_promql` 工具在历史回放用例上仍正常。
- **回滚不变量**：VM 包卸载不会破坏一方 Prom（前提是配置已切回）；不修改 `datasource.go` 主流程。

---

## 4. 候选 2：Elasticsearch（logs / 检索）

### 4.1 选型理由
- 大量既有 ES 集群可复用；不强制引入新存储。
- Query DSL 成熟，组合查询 / 聚合 / ILM 完整。
- 信号：**复用 `loki`**（与一方 Loki 并存）。本任务**不**新增信号名"elasticsearch"。

### 4.2 manifest 草案
详见 `datasource-manifests/elasticsearch.yaml`。要点：

- **包名**：`opskeeper-datasource-elasticsearch`
- **精确版本**：`0.1.0`（与 manifests 一致）
- **vendor**：`elastic`（上游公司名；非 OpsKeeper 一方）
- **scope / 安全级别 / 兼容范围**：
  - `targets: [manager]`
  - `safety_level: L1`
  - `capabilities: [read]`
  - `required_scopes: [observability.read]`（修正：原 `log.read` 不在 `domain.Scope` 常量集中且未授予 `ManagerPolicy`，会触发准入拒绝；按 §2.3 收敛到 `observability.read`，日志读取属观测语义）
  - `install.min_pig_version: "0.4.0"`
  - `install.min_edge_version: "0.8.0"`
  - 信号复用：`loki`（与一方 Loki 同名，沿用 `datasource.go` 中 `NameLoki`）
- **能力声明**：贡献 `loki`；贡献体 `Elasticsearch{Q: esLogQuerier}` 实现 `toolcore.LogQuerier`。
- **协议映射**：将 LogQL（filter 表达式）翻译到 ES Query DSL `bool.filter`；时间范围 → `range`；label → `term`/`keyword`。
- **凭据**：`es.basic.username/password`、`es.api_key`、`es.tls.ca`，作用域限于 `observability.read`。

### 4.3 注册方式
- `Activate(ctx, manifest, Elasticsearch{Q: esClient})`，信号名取 `NameLoki`（= `"loki"`）。
- 与一方 Loki **配置期单激活二选一**：同 §3.3 的语义；配置键 `datasource.logs.backend: "elasticsearch" | "loki"`。同进程双激活会触发 `ErrDuplicate`。
- **LogQL 翻译层**：`core/base/pkg/logquery` 已是 Loki 客户端的入口；ES 插件**不**改 `logquery` 包，而是新增一个 `core/base/pkg/esquery` 子包提供 ES 客户端（仅在 ES 包内部引用，宿主不直接 import）。

### 4.4 与 `opskeeper-sre-observability` 的协同
- `query_logql` 工具拿到的 querier 类型不变；ES 客户端满足 `LogQuerier.QueryRange(opts)` 即可。
- 不在 `query_logql` 工具内加 ES 专属分支；翻译在客户端侧完成。

### 4.5 与 `core/domains/server/{logs,traces}` 的对接
- `core/domains/server/logs/http.go` 是"logs HTTP server"；本任务**不**改其路由或响应；ES 包只通过 `Set{}.Logs()` 注入 querier，被该域经 `toolcore` 间接消费。
- `core/domains/server/traces/` 不涉及。

### 4.6 测试方案
| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | LogQL `filter` → ES `bool.filter` 翻译 | 字段映射表快照比对 |
| Unit | 时间范围 → `range.@timestamp` 转换 | 边界 1ms 一致 |
| Unit | Label → `term`/`keyword` | 复合 label 拆解正确 |
| Integration | 连真 ES 8.x（compose），导入示例日志，跑 10 个 LogQL 模板 | 结果集相等 |
| Failure | 401 / 5xx / index not found / 超时 | 错误映射正确 |
| Cross-signal | 与一方 Loki 同 fixture 同查询 | 行为差异文档化（LogQL 子集功能差异） |

### 4.7 回滚方案
- 卸载 `opskeeper-datasource-elasticsearch`、配置切回 `loki`、重启 Manager（因果链同 §3.7 精确版）。
- 验证 `query_logql` 工具在原 LogQL 用例上仍通过。
- 不动 `logquery` 包；不破坏 Loki 路径。
- 卸载后跑 `make pi-plugin-guard` 与 `make module-check` 校验。

---

## 5. 候选 3：OpenSearch（logs / 检索）

### 5.1 选型理由
- OpenSearch 衍生自 ES 7.x；API 形态接近，但 plugin 体系不同；许多 AWS 客户默认部署 OpenSearch。
- 信号：复用 `loki`，与 ES 共存或二选一。

### 5.2 manifest 草案
详见 `datasource-manifests/opensearch.yaml`。要点：

- **包名**：`opskeeper-datasource-opensearch`
- **精确版本**：`0.1.0`（与 manifests 一致）
- **vendor**：`opensearch-project`（上游项目名；非 OpsKeeper 一方）
- **scope / 安全级别 / 兼容范围**：
  - `targets: [manager]`
  - `safety_level: L1`
  - `capabilities: [read]`
  - `required_scopes: [observability.read]`（同 §4.2 修正理由：日志读取属观测语义；避免 `log.read` 触发准入拒绝）
  - `install.min_pig_version: "0.4.0"`
  - `install.min_edge_version: "0.8.0"`
  - 信号复用：`loki`（与一方 Loki 同名，沿用 `datasource.go` 中 `NameLoki`）
- **能力声明**：贡献 `loki`；贡献体 `OpenSearch{Q: osLogQuerier}` 实现 `toolcore.LogQuerier`。
- **与 ES 的关键差异**：security plugin 默认开启、DSL 7.x 为主；客户端层在 ES 基础上覆写：
  - 认证路径优先 `basic` + TLS（security plugin 习惯）；
  - DSL 走 `/_search` 主路径，避免 PPL（除非用户配置开关）；
  - 显式声明不支持 painless 脚本化（v1 范围）。

### 5.3 注册方式
- 同 §4.3，配置期单激活二选一或三选一；客户端走 OpenSearch 的 `_search` 端点（与 ES 形态相同但 hostname 不同）。
- 配置键 `datasource.logs.backend: "opensearch" | "elasticsearch" | "loki"`。同进程双激活会触发 `ErrDuplicate`。

### 5.4 与 §4 ES 的代码复用边界
- ES 与 OS 包**不共享 Go 代码**；manifest 与测试可参考，但实现各自独立。
- 客户端层放在 `core/base/pkg/osquery`（独立子包，与 `esquery` 同级），避免互相 import。

### 5.5 测试方案
| 层 | 用例 | 通过条件 |
|---|---|---|
| Unit | DSL 7.x 解析 | 关键字段映射快照 |
| Unit | Security plugin 401 / 403 区分 | 错误码语义对齐 |
| Integration | 连真 OpenSearch 2.x | 与 ES 相同 fixtures 行为一致或差异文档化 |
| Cross-compat | ES DSL 子集 vs OpenSearch DSL | 文档化已知差异（如 `_doc` API） |

### 5.6 回滚方案
- 同 §4.7（精确因果链同 §3.7），配置键多一项 `opensearch`；卸载后切回 `loki` 或 `elasticsearch`。

---

## 6. 实现切片建议（仅建议，不在本任务合入）

### 6.1 建议的代码骨架（候选 1 优先落地）
```
plugins/opskeeper-datasource-victoriametrics/
├── pig-ops.yaml                          # 已在 datasource-manifests/ 给出草案
├── go.mod                                # 子模块；不污染主仓 go.mod 顶层
└── internal/
    ├── contributor/contributor.go        # VictoriaMetrics struct + Register
    ├── vmclient/client.go                # HTTP 客户端，实现 PromQuerier
    ├── vmquery/translate.go              # PromQL → VM HTTP API 参数
    └── vmquery/translate_test.go
```

- 子模块方式：把第三方包建为独立 Go module，由主仓 `go.work` 引用；便于独立版本化与签名。
- 集成测试 fixtures：放进 `core/harness/fixtures/datasource/vm/`（与既有 harness 同位）。

### 6.2 与宿主接口的契合点
- `VictoriaMetrics.Q` 类型为 `toolcore.PromQuerier`，即 `*vmclient.Client`。
- `VictoriaMetrics.PluginName()` 返回 `opskeeper-datasource-victoriametrics`（与 manifest.metadata.name 一致）。
- 启动期 `Activate` 入口仍由 `core/manager/biz/aiops/datasource` 提供（不引入新入口）。

### 6.3 不建议改动
- 不要在 `core/manager/biz/aiops/datasource/datasource.go` 中添加 ES/OS 专属分支；统一通过 `toolcore` 接口收敛。
- 不要把 ES/OS 客户端放进 `core/base/pkg/logquery`；新增 `esquery` / `osquery` 子包。
- 不要改 `core/floor/kernel/kind.go` 的六元集合。

### 6.4 落地门槛
- 用户须单独开 change；本任务不直接合入。
- 落地前需补：
  1. 真实 VM / ES / OS 集群的 staging fixture；
  2. CI 接入 `make pi-plugin-guard`、`make module-check`、`go test ./scripts/ledgercheck`；
  3. 在 Runtime 页面增加 datasource 第三方后端可视化（与 1.2/1.3 串联）。

---

## 7. 通用闸门（每个候选都必须通过的清单）

按 `tasks.md` 4.10 与 4.11：

| 闸门 | 内容 |
|---|---|
| **Manifest 完整** | apiVersion、kind、metadata.name/version/vendor/homepage、spec（targets、safety_level、capabilities、required_scopes、audit、approval、install、autonomy） |
| **精确版本** | metadata.version 用 semver 锁；不得用 `latest` / `main` |
| **Scope 最小化** | 仅声明该候选必要的 scope；多写一个就拒 |
| **安全级别** | datasource 一律 L1（只读），不声明 L2；manifest 写 L2 即拒 |
| **兼容范围** | `install.min_pig_version` 与 `install.min_edge_version` 必填；与主仓 PiG/Edge 基线对齐 |
| **测试** | unit + integration + contract 三层；integration 必须在 CI 中跑真集群（compose fixture） |
| **签名** | 经 `core/floor/pluginmanifest/signing.go` 通道，签名文件随 manifest 一同分发 |
| **回滚文档** | 含"卸载前/卸载后/不变量"三段；与 §3.7/§4.7/§5.6 一致 |
| **灰度** | 灰度由 `install.strategy=canary` 控制；先 `canary` 再 `rolling`，最后 `replace` |
| **闸门命令** | `make pi-plugin-guard && make module-check && go test ./scripts/ledgercheck` 全过（修正：原文 `plugin-guard` 笔误，以 Makefile 为准） |

---

## 8. 与 4.1 / 4.2 一致性自查表

| 自查项 | 4.1 §7 原文 | 本方案是否一致 | 证据 |
|---|---|---|---|
| 嵌入模型编译内 | 嵌入模型本期留在编译内 | ✓ 不引入新嵌入模型 | §6.3 禁止项 |
| MCP / 技能宿主内管理 | MCP 与技能宿主内管理 | ✓ 不开新 MCP / 不改技能注册 | §1.3 非目标 |
| 告警归 4.12 | 告警归 4.12 | ✓ 不动告警归类 | §1.3 非目标 |
| 六扩展点封闭 | `core/floor/kernel/kind.go` 六 Kind 封闭 | ✓ 复用 `prom`/`loki`，不新增 | §2.1 / §3.1 / §4.1 / §5.1 |
| datasource 第三方候选 | 4.2 四分类：迁移为插件，第三方候选 | ✓ 仅做"第三方候选"分支 | §1.2 / §2.1 |
| 一方后端不重写 | 一方三后端经内核注册，证据 `datasource.go` | ✓ 不改 `datasource.go` 主流程 | §1.3 / §2.1 / §6.3 |
| 协同面只描述接口 | 与 `opskeeper-sre-observability`（12 工具）协同只描述接口 | ✓ 不改工具实现，仅描述路径 | §3.4 / §4.4 |
| 查询面对接只标注 | 与 `core/domains/server/{logs,traces}` 查询面的对接关系 | ✓ 不改其 HTTP 路由与响应 | §3.5 / §4.5 |

---

## 9. 验收清单（对应 issue 描述）

- [x] 每个候选有 manifest 草案（`datasource-manifests/{victoriametrics,elasticsearch,opensearch}.yaml`）
- [x] 每个候选有注册方式（§3.3 / §4.3 / §5.3）
- [x] 每个候选有测试方案（§3.6 / §4.6 / §5.5）
- [x] 每个候选有回滚方案（§3.7 / §4.7 / §5.6）
- [x] 与 4.1 §7 评审结论一致（§8 自查表）
- [x] 与 4.2 四分类一致（§8 自查表）
- [x] 无越界改动（§1.3 / §6.3 明确禁止项；不动 `datasource.go` 与 `plugins/` 包结构）
- [ ] PR 使用中文注释和中文提交说明（待提交时落实）
- [ ] PR 描述关联 BENY-722 与 OpenSpec change `plugin-architecture-recovery-plan` 任务 4.3（待提交时落实）

## 10. 提交与执行规范

- 提交语言：本任务 PR 使用中文注释 / 中文提交说明（按计划任务 7.4）。
- 关联：本 issue 编号 BENY-722；OpenSpec change `plugin-architecture-recovery-plan` 任务 4.3。
- 落盘路径：本文档落盘 `openspec/changes/plugin-architecture-recovery-plan/datasource-plugin-plan.md`；manifest 草案落盘 `openspec/changes/plugin-architecture-recovery-plan/datasource-manifests/`。
- 实现切片建议（§6）**不**在本任务内合入；如要落地，请用户单独开 change。

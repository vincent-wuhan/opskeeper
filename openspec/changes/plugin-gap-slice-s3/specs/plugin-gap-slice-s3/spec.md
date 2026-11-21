## ADDED Requirements

### Requirement: observability 包声明知识检索与跨主机进程清单工具

`opskeeper-sre-observability` 的 manifest MUST 声明 `query_knowledge` 与 `get_host_processes` 两个工具，class 均为 read；`required_scopes` MUST 在既有三个 scope 之外新增 `knowledge.read`。两条均 MUST NOT 声明 limits（上呼工具在 sdk Check 的两侧只有零值，声明即红）。

#### Scenario: 两个工具在 allow-list 里

- **WHEN** 读取 `opskeeper-sre-observability` 的 manifest `spec.tools`
- **THEN** `get_host_processes`、`query_knowledge` 各出现一次，class 均为 read
- **AND** 列表保持按名排序且无重复

#### Scenario: scope 集恰好扩一个

- **WHEN** 比较本版本与 0.2.0 的 `required_scopes`
- **THEN** 差集恰为 `knowledge.read` 一项

#### Scenario: 版本号反映菜单增量

- **WHEN** 读取 `metadata.version`
- **THEN** 版本为 0.3.0（菜单新增即 schema 增量）

### Requirement: knowledge.read 是被两个 profile 授予的只读 scope

`knowledge.read` MUST 作为 `ScopeKnowledgeRO` 常量存在于 `core/domain/plugin.go`；finance 与 SaaS 两个部署 profile 的 `Granted` MUST 均包含它——缺任一则 `sdk.Admit` 在该 profile 拒绝 observability 0.3.0，包在本 profile 不可装。

#### Scenario: 两 profile 均可装 0.3.0

- **WHEN** 用 finance 或 SaaS profile 的 Granted/Ceiling/MaxRadius 跑 `sdk.Admit(observability 0.3.0 manifest)`
- **THEN** 两个 profile 都放行

#### Scenario: 嵌套关系不破

- **WHEN** 检查 finance Granted ⊆ SaaS Granted
- **THEN** 包含关系成立（knowledge.read 两侧同加）

### Requirement: 生成链三向一致

observability 的 canonical 与包内 tools.go MUST 由生成链产出：清单 +2 后 `TestTheObservabilityToolsetMatchesTheRegistry` 在未重生成时红；重生成 + sync 后两侧与 manifest 的 14 个名字一致。

#### Scenario: 未重生成即红

- **WHEN** `observabilityTools()` 加了两条而 tools.go 未重生成
- **THEN** `TestTheObservabilityToolsetMatchesTheRegistry` 失败

#### Scenario: 包内副本与 canonical 一致

- **WHEN** 运行 packaged 与 canonical 的比对测试
- **THEN** 除既有的 wire import 改写外逐字相同

### Requirement: 桥把"存在但未配置"与"这里没有"分句

上呼桥 MUST 区分三种 miss：知道但不服务（既有 withheld）、**存在但未配置**（命中注册表能力名清单而本部署 bag 未产出——如 knowledge 服务未接线的 query_knowledge）、这里没有（errBaseToolNotHere，分派链继续）。第二种 MUST 返回 denied=true 与含"not configured"的 reason，MUST NOT 返回 errBaseToolNotHere、MUST NOT 让措辞落在 "no such tool"。

#### Scenario: knowledge 未接线的部署

- **WHEN** 一个 `r.knowledge == nil` 的部署收到 `query_knowledge` 调用
- **THEN** 回复说明该工具本控制面知道、但本部署未配置其依赖
- **AND** 措辞不含 "no such tool"

#### Scenario: 不在能力清单上的名字

- **WHEN** 桥收到一个 known 清单与 bag 都没有的名字
- **THEN** 返回 errBaseToolNotHere，分派链继续（行为与 S-1 相同）

#### Scenario: 候选清单随 BuildBaseTools 无缺口

- **WHEN** 用全依赖构造 `BuildBaseTools` 并比对产出的每个名字
- **THEN** 每个名字都在候选清单上（清单缺名即红）

### Requirement: 声明面收口后闸门自动覆盖

两个新名字 MUST 被 S-1 的对账闸门经 manifest 读取自动纳入断言，闸门源码 MUST NOT 为本切片新增名字字面量。

#### Scenario: 闸门读 manifest 而不是读字面量

- **WHEN** 本切片的 diff 里搜索闸门测试文件
- **THEN** 没有为两个新名字新增的字面量清单

### Requirement: persona 说明与菜单同步

observability persona MUST 写明知识检索（复盘/处置先查 KB）与跨主机进程比对的入口，MUST NOT 为新工具承诺读以外的能力；既有"K8s 对象与消息队列不在覆盖内"的边界句 MUST 保持。

#### Scenario: 两个入口被说明

- **WHEN** 读取 `opskeeper-observability` SKILL
- **THEN** `query_knowledge` 与 `get_host_processes` 的用途各有一句，且指向这两个名字

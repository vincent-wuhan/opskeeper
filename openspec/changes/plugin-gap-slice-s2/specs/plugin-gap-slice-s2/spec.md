## ADDED Requirements

### Requirement: readonly 包声明其事件读与设备清单工具

`opskeeper-sre-readonly` 的 manifest MUST 声明 `query_incidents`、`get_incident_detail`、`query_devices` 三个工具，class 均为 read。三者的 scope 需求 MUST 已包含在包既有 `required_scopes`（`alert.read`、`topology.read`）之内，MUST NOT 引入新的 scope。

#### Scenario: 三个工具在 allow-list 里

- **WHEN** 读取 `opskeeper-sre-readonly` 的 manifest `spec.tools`
- **THEN** `query_incidents`、`get_incident_detail`、`query_devices` 各出现一次，class 均为 read
- **AND** 列表保持按名排序且无重复
- **AND** 三条均不声明 limits（上呼工具在 sdk Check 的两侧只有零值，声明即红；返回量由各自 schema 的 limit / maxItems 约束）

#### Scenario: 零 scope 增量

- **WHEN** 比较本版本与 0.2.0 的 `required_scopes`
- **THEN** 两个集合相同

#### Scenario: 版本号反映菜单增量

- **WHEN** 读取 `metadata.version`
- **THEN** 版本为 0.3.0（菜单新增即 schema 增量）

### Requirement: 工具清单与执行器元数据一致

readonly 的工具清单（canonical 与包内副本）MUST 按三个工具 BaseTool `Info()` 的 `Description` 与 `Parameters` 原样收录。`get_incident_detail` MUST 收录批量信封 schema（`incident_ids[]`），MUST NOT 收录闭包形态的单 id schema——节点侧 schema 由 BaseTool 生成，节点发来的参数按 BaseTool 形状构造。

#### Scenario: schema 与 BaseTool 逐字一致

- **WHEN** 比较工具清单条目的 `Parameters` 与管理端 BaseTool `Info().Parameters`
- **THEN** 两者逐字相同

#### Scenario: 批量信封不被抄成单 id

- **WHEN** 读取工具清单里 `get_incident_detail` 的 schema
- **THEN** 参数是 `incident_ids` 数组（1..16）
- **AND** 不是闭包形态的 `incident_id` 单数

#### Scenario: 包内副本与 canonical 一致

- **WHEN** 运行 packaged 与 canonical 的比对测试
- **THEN** 除既有的 wire import 改写外逐字相同

### Requirement: 声明面收口后闸门自动覆盖

三个新名字 MUST 被 S-1 的对账闸门（`plugin_gap_gate_test.go`）经 manifest 读取自动纳入断言，闸门源码 MUST NOT 为本切片新增名字字面量。控制面工具与 readonly 声明的对照测试 MUST 同步收录三个名字。

#### Scenario: 闸门读 manifest 而不是读字面量

- **WHEN** 本切片的 diff 里搜索闸门测试文件
- **THEN** 没有为三个新名字新增的字面量清单

#### Scenario: 从 manifest 删名会红

- **WHEN** 从 readonly manifest 删除三个名字中的任何一个
- **THEN** 对账闸门或控制面对照测试至少一个失败

### Requirement: persona 说明与菜单同步

readonly 的诊断 persona MUST 在使用边界里写明事件时间线与设备清单的入口（`query_incidents` → `get_incident_detail`、`query_devices` 作为 device_id 来源），MUST NOT 为新工具承诺读以外的能力。

#### Scenario: 事件读入口被说明

- **WHEN** 读取 readonly 包的 diagnose persona
- **THEN** 事件列表与详情两个工具的用途被写明，且指向的是这两个名字

#### Scenario: 设备清单入口被说明

- **WHEN** 读取同上 persona
- **THEN** 收窄调查范围的步骤提到 `query_devices` 是 device_id 的来源

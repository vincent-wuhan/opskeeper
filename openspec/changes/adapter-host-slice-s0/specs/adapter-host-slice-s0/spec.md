## ADDED Requirements

### Requirement: 贡献到 adapter 宿主的 family 必须可见于 Sources

`Sources()` MUST 由微内核 registry 派生，而不是返回字面量。贡献到宿主的 family MUST 与首方 family 走同一条 `Activate` 闸门进入同一个 registry，MUST NOT 存在并行的注册通道或第二个 registry。S0 MUST NOT 导出第三方贡献入口。

#### Scenario: 贡献 family 通过同一条闸门进入 registry

- **WHEN** 一个首方字面量从未命名的 family 被 `Activate`
- **THEN** 该 family 以 `KindAdapter` 注册进与首方相同的 registry
- **AND** 其注册项记录贡献方的 plugin 名，以便按名回滚
- **AND** 不存在旁路的注册接口

#### Scenario: 贡献 family 与首方同名

- **WHEN** 贡献的 family 名与已激活的 family 冲突
- **THEN** 激活被拒绝并返回重复错误
- **AND** 先激活的条目不被覆盖，读回结果中该 family 仍只出现一次

### Requirement: Sources 的顺序是契约

`Sources()` 返回的列表 MUST 以首方 family 按 `firstPartySources()` 顺序构成的严格前缀开头，其余条目 MUST 排在首方块之后并按 family 名字典序排列。默认路径（无贡献）MUST 返回首方 8 个 family，顺序、`Env`、`Wire` 与基线逐项一致。顺序 MUST NOT 依赖 registry 内部顺序或包初始化顺序。

#### Scenario: 无贡献时的默认路径

- **WHEN** 读取 `Sources()` 且没有任何第三方贡献
- **THEN** 返回首方 8 个 family，顺序、`Env`、`Wire` 与基线逐项一致
- **AND** 首方块之后没有其他条目

#### Scenario: 存在贡献条目

- **WHEN** 两个贡献 family 按与其期望顺序相反的顺序传入
- **THEN** 两个 family 都出现在首方块之后
- **AND** 顺序按 family 名字典序，而不是传入顺序

#### Scenario: 两次读取同一输入

- **WHEN** 以同一组贡献条目调用两次列表构造
- **THEN** 两次返回的 family 序列一致

### Requirement: 注释与实现一致

`Sources()` 的文档注释 MUST 逐句描述当前实现的行为，MUST NOT 保留在实现不成立时无法验证的断言（例如"第三方 family 落在同一列表"这类在贡献路径未接通前的措辞）。任何关于第三方贡献可见性的陈述 MUST 指向验证该行为的测试。

#### Scenario: 注释陈述可验证

- **WHEN** 文档注释声称贡献 family 出现在返回列表中
- **THEN** 存在独立测试断言该性质，且该测试不是注释复述

### Requirement: 准入授予集合是宿主侧可组装值

adapter 宿主的准入上限 MUST 由 `AdapterPolicy(granted domain.Scopes)` 构造，安全等级上限与爆炸半径上限 MUST NOT 随授予集合变化。`granted` MUST 是宿主侧可组装值而非包侧可申领值：插件 MUST NOT 通过声明更多 scope 扩大它，`Satisfies` 对未知 scope MUST 保持 fail-closed。默认授予集合 MUST 覆盖既有首方 scope（逐项不变）与 `topology.read`、`cloud.read`。

#### Scenario: 只读新 scope 的插件被默认策略放行

- **WHEN** 一个 `required_scopes` 仅含 `cloud.read` 或仅含 `topology.read` 的 manifest 提交给默认 adapter 策略
- **THEN** 准入通过

#### Scenario: 授予集合不足时拒绝并指名缺失 scope

- **WHEN** 一个 `required_scopes` 含 `cloud.read` 的 manifest 提交给只含既有 8 项 scope 的策略
- **THEN** 准入被拒绝
- **AND** 错误信息指名缺失的 scope
- **AND** 该拒绝是正确结果，不得通过放松闸门来"修复"

#### Scenario: 扩大授予集合是宿主代码 diff

- **WHEN** 需要让某 family 通过准入
- **THEN** 变更发生在宿主的授予集合代码上，而不是插件的自我声明
- **AND** `Scopes.Satisfies` 对未知 scope 仍然拒绝

### Requirement: 云读权限单列且只读

MUST 提供 `cloud.read` scope，且 MUST NOT 与 `host.read` 合并——`host.read` 由宿主注入本地/SSH 凭据，云厂商开放 API 走独立凭据。MUST NOT 在本切片引入 `cloud.write`。

#### Scenario: 云读权限有独立 scope 名

- **WHEN** 宿主为云 API 读取注入凭据
- **THEN** 该权限由 `cloud.read` 承载
- **AND** 持有 `host.read` 的插件不因此获得云 API 凭据

#### Scenario: 写权限不在本切片

- **WHEN** 审阅本切片的词表变更
- **THEN** 只新增只读 scope
- **AND** 云上变更所需的写 scope 留在写操作切片单独评审

### Requirement: 新 family 的工具清单接线是显式选择

新增一个注册只读工具的 family 时，MUST 显式选择进入 PiG 中间件 manifest 的 `spec.tools` 允许清单或进入 `NotPackagedFamilies` 并附理由，MUST NOT 让两者都不发生。G3 的两条既有测试 MUST 把该选择变成一次可见的 diff。

#### Scenario: 新 family 的只读工具进入允许清单

- **WHEN** 一个贡献 family 注册的只读工具回答的是被管控资产而非本节点自身
- **THEN** 这些工具名进入 `pig-ops.yaml` 的 `spec.tools` 声明
- **AND** 已生成的 toolset 与 manifest 双向一致

#### Scenario: 排除按 family 为单位并附理由

- **WHEN** 一个 family 的读取回答的是本节点自身而非被管控资产
- **THEN** 该 family 整体进入 `NotPackagedFamilies` 并写明理由
- **AND** 排除项命名的是真实存在的 family/tool，过期排除被删除而非继承

#### Scenario: 未接线时测试失败

- **WHEN** 一个新 family 的只读工具进入 packaged 集合但未在 manifest 中声明
- **THEN** 工具清单一致性测试失败，而不是闸门静默拒绝每一次调用

### Requirement: 启动接线对贡献 family 可断言

闭环修复的启动接线 MUST 以一个可注入 family 列表的函数表达，贡献 family MUST 在其 DSN 配置时被接线恰好一次，未配置 DSN 时 MUST NOT 接线。接线 MUST 属于同一启动循环，不得为贡献 family 增加独立分支。

#### Scenario: 贡献 family 被接线一次

- **WHEN** 启动接线遇到一个 DSN 已配置的贡献 family
- **THEN** 该 family 的接线函数被调用恰好一次，并收到去除首尾空白的 DSN
- **AND** 返回一个可在关闭时运行的收尾函数

#### Scenario: 未配置 DSN 的 family 不接线

- **WHEN** 启动接线遇到一个 DSN 未配置（或仅空白）的贡献 family
- **THEN** 该 family 的接线函数不被调用
- **AND** 不返回其收尾函数
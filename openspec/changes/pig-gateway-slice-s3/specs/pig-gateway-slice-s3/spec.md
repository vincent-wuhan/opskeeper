## ADDED Requirements

### Requirement: PiG 工具调用先过身份翻译

网关 MUST 把 PiG 侧 `(RuntimeToolID, 上报类别)` 翻译为稳定 OpsKeeper 工具身份与 domain 类别后才可继续裁决；翻译 MUST 只消费对账暴露集与发布清单，MUST NOT 维护第二份身份映射；RuntimeToolID MUST NOT 出现在翻译层之外的结构中。

#### Scenario: 未声明的运行时工具

- **WHEN** 调用携带的 RuntimeToolID 不在发布清单内
- **THEN** 翻译拒绝，调用不分发，一条 denied 调用记录落库

#### Scenario: 清单内两个声明撞同一 RuntimeToolID

- **WHEN** 发布清单有两条声明的 RuntimeToolID 相同而 StableToolID 不同
- **THEN** 翻译拒绝（身份级故障），调用不分发并留痕

#### Scenario: 上报类别与暴露面类别分叉

- **WHEN** 调用上报的类别与对账暴露集记录的类别不一致
- **THEN** 翻译拒绝，调用不分发并留痕

### Requirement: 分发前强制全部策略

网关 MUST 在分发之前依次强制 tenant、role、incident scope、resource scope、capability（暴露集内）、read-only 硬闸与限流；每条拒绝 MUST 落一条 denied 调用记录，且 MUST NOT 触发分发。

#### Scenario: 跨事件调用

- **WHEN** 调用携带的 incident 与任务钉死的作用域不一致
- **THEN** 调用被拒绝、不分发并留 denied 记录

#### Scenario: 跨资源调用

- **WHEN** 工具作用域声明了资源白名单且调用携带的资源不在其中
- **THEN** 调用被拒绝、不分发并留 denied 记录

#### Scenario: 只读发布里的变更请求

- **WHEN** 运行时沙箱 ReadOnly=true 且调用类别为 write 或 destructive
- **THEN** 调用直接 Blocked（不进审批通道），不分发并留 denied 记录

#### Scenario: 限流拒绝也被审计

- **WHEN** 同一 (StableToolID, tenant/role/worker) 的调用超过限流预算
- **THEN** 调用被拒绝、不分发并留 denied 记录

### Requirement: 调用持久化携带治理证据

每次 PiG 工具调用 MUST 持久化起止时戳、调用者身份（tenant/role/worker）、release 与 capability digest、参数哈希、结果态、错误与 trace 关联；参数与结果 MUST NOT 以原文落库。

#### Scenario: 成功调用落完整证据

- **WHEN** 一次允许的调用完成分发
- **THEN** 调用行含起止时戳、调用者三元组、两枚 digest、参数哈希、结果哈希与 success 状态

#### Scenario: 拒绝调用也留行

- **WHEN** 一次调用在任何闸被拒
- **THEN** 调用行以 blocked 状态落库，deny_reason 记录拒绝闸

### Requirement: 修复候选只能产 proposal

write/destructive 类调用 MUST NOT 直接执行：ReadOnly 发布下直接 Blocked；非 ReadOnly 发布下网关最多返回「需要 proposal」的裁决，执行 MUST 绑定既有 human-approved exact-payload 审批路径。

#### Scenario: 变更请求在非只读发布下的形态

- **WHEN** ReadOnly=false 且调用类别为 write
- **THEN** 网关返回 ProposalRequired 而非 Allowed，proposal 经既有 review 台账路径记录，不发生分发

### Requirement: 迁移纯增量

调用持久化表 MUST 以纯增量迁移注册（新表族 `runtimemgmt_pig_tool_calls`），MUST NOT 触碰既有表结构；migrator 计数闸门同步。

#### Scenario: 迁移后表存在且幂等

- **WHEN** 迁移列表执行两遍
- **THEN** 表存在且第二遍不报错

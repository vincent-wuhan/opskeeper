## ADDED Requirements

### Requirement: 宿主策略按扩展点分别设定安全级别上限

插件内核宿主的 `Policy` MUST 支持按扩展点（Kind）分别设定安全级别上限（per-kind safety ceiling）。某个 Kind 有条目时，它的生效上限就是该条目本身；某个 Kind 没有条目时，它的生效上限回落全局 `MaxSafetyLevel`。宿主 MUST NOT 把条目与全局取 `min`——那会让条目在"全局更严"时变成空操作，从而让操作员以为生效了的放宽策略其实一条也没放行。

条目携带零值 `SafetyLevel` 时 MUST 按缺省处理（回落全局），因为零值排名是 L3（最宽），当成有效条目等于把系统里最宽的 ceiling 记进了策略。

- **WHEN** 策略为 `MaxSafetyLevel: L2` 且 `KindCeilings[notifier] = L3`
- **THEN** `notifier` 的生效上限是 L3，`tool` / `adapter` / `datasource` / `collector` / `provider` 的生效上限是 L2

#### Scenario: 条目生效，不与全局取 min

- **WHEN** 一份策略的全局 `MaxSafetyLevel` 为 L2，`KindCeilings` 里 `notifier` 为 L3
- **THEN** `EffectiveSafetyCeiling(KindNotifier)` 返回 L3
- **AND** `EffectiveSafetyCeiling(KindTool)` 返回 L2

#### Scenario: 无条目回落全局

- **WHEN** 一份策略没有任何 per-kind 条目
- **THEN** `EffectiveSafetyCeiling(k)` 对任何 Kind 都返回全局 `MaxSafetyLevel`
- **AND** 该策略下的准入判定与 per-kind 能力引入前逐位相同

#### Scenario: 零值条目按缺省处理

- **WHEN** `KindCeilings[notifier]` 的值是零值 `SafetyLevel`
- **THEN** `EffectiveSafetyCeiling(KindNotifier)` 返回全局 `MaxSafetyLevel`，而不是零值

#### Scenario: 最宽上限供包级闸门使用

- **WHEN** 策略的全局 `MaxSafetyLevel` 为 L2，`KindCeilings[notifier] = L3`
- **THEN** `WidestSafetyCeiling()` 返回 L3

### Requirement: 写入方向由入口点区分，写反方向 panic

写入 per-kind ceiling MUST 走两个方向相反的入口点：收窄入口只接受比当前生效上限更严的写入，放宽入口只接受更宽的写入。任一入口收到反方向的写入 MUST panic，MUST NOT 静默忽略——静默忽略会让操作员的改动看起来生效了其实没有，而"系统说一切正常但策略没落"正是这个缺口本身的失败模式。panic 文本 MUST 指向正确的方向。

- **WHEN** 调用者用收窄入口写入一个比当前生效上限更宽的等级
- **THEN** 内核 panic，文本说明该入口只能收紧

#### Scenario: 收窄入口拒绝放宽

- **WHEN** 对全局 L3 的策略调用收窄入口写入 L2
- **THEN** 返回新的一份策略，`notifier` 的生效上限变成 L2
- **AND** 对全局 L2 的策略调用同一入口写入 L3
- **AND** 该调用 panic，且 panic 文本指向"收紧"

#### Scenario: 放宽入口拒绝收紧

- **WHEN** 对全局 L2 的策略调用放宽入口写入 L3
- **THEN** 返回新的一份策略，`notifier` 的生效上限变成 L3
- **AND** 对全局 L3 的策略调用同一入口写入 L2
- **AND** 该调用 panic，且 panic 文本指向"放宽"

#### Scenario: 写入返回新策略而不是改穿接收者

- **WHEN** 一份策略被两个持有者共享，其中一个通过写入入口加了一个条目
- **THEN** 写入方拿到的是新的一份策略
- **AND** 另一个持有者手里的 map 不含新条目

#### Scenario: 未知 Kind 或未知级别被拒

- **WHEN** 写入入口收到一个不在封闭 Kind 集合内的 Kind，或一个不在 L0..L3 内的 `SafetyLevel`
- **THEN** 该写入 panic

### Requirement: 宿主必须能执行自己的策略

`NewHost` MUST 校验每一条 per-kind ceiling：Kind 必须在封闭集合内，`SafetyLevel` 必须是 L0..L3。校验 MUST 落在 `NewHost` 而不是只落在两个写入入口上，因为策略可以绕过入口点直接用结构体字面量构造。一个不认识的安全级别排名是 L3（最宽），所以一个打错的条目会记下系统里最宽的 ceiling，然后安静地放行操作员本来想拦住的包——一个执行不了自己策略的宿主不是保守的宿主，是坏的宿主。

- **WHEN** 一份带 per-kind 条目的策略被交给 `NewHost`，而某条目宿主无法执行
- **THEN** `NewHost` panic，MUST NOT 带着这份策略继续运行

#### Scenario: 宿主拒绝无法执行的 ceiling

- **WHEN** 用一个未知 Kind 的 ceiling 构造宿主
- **THEN** `NewHost` panic

- **WHEN** 用一个超出 L0..L3 的安全级别构造宿主
- **THEN** `NewHost` panic

### Requirement: 包级准入用最宽上限，per-kind 闸门落在激活登记处

内核 MUST 在两处判安全级别上限，且两处的判据不同。`Host.Admit` 是包级闸门，它看到的是一份 manifest，而 manifest 只声明整包一个安全级别、不声明贡献落在哪些 Kind，所以包级闸门 MUST 用 `WidestSafetyCeiling()`。per-kind 的闸门 MUST 落在 `Host.Activate` 的 scoped registrar 上，因为 registrar 同时知道条目落在哪个 Kind 和这个包声明了什么级别。

per-kind 拒绝 MUST 走既有的整包回滚路径（`RemovePlugin`），MUST NOT 新造第二条回滚路径——一个半激活的包不能在表里留下没人清理的条目。

- **WHEN** 一个 L3 包在包级准入过关后要往某个上限更严的 Kind 登记条目
- **THEN** registrar 按该 Kind 的生效上限拒绝这条条目
- **AND** 这个包此前登记的全部条目被撤掉

#### Scenario: 包级准入用最宽上限

- **WHEN** 一份 L3 manifest 在全局 L2、`KindCeilings[notifier] = L3` 的策略下走 `Admit`
- **THEN** 包级准入通过（最宽上限是 L3）

#### Scenario: per-kind 闸门按条目所在 Kind 判

- **WHEN** 同一个 L3 包在上述策略下要往 `notifier` 登记条目
- **THEN** 该条目登记成功（notifier 的生效上限是 L3）

- **WHEN** 同一个 L3 包在上述策略下要往 `tool` 登记条目
- **THEN** 该条目被拒，错误文本指明是 `tool` 的 ceiling 不允许 L3
- **AND** 该包此前写入 `notifier` 的条目被一并撤掉，注册表里不留半激活的条目

#### Scenario: 无 per-kind 条目时激活路径不变

- **WHEN** 一份策略的 `KindCeilings` 为空
- **THEN** `Admit` 与 `Activate` 的判定结果和错误文本与引入 per-kind 能力之前逐位相同

### Requirement: 既有策略构造点零源码改动

`datasource.ManagerPolicy()`、`toolset.adapterPolicy()` 与 edge 的 `Profile.Policy()` 这三个既有策略构造点 MUST 保持零源码改动，且在 per-kind 条目为空时行为逐位不变。per-kind 能力 MUST NOT 反向在这些构造点上派生策略——策略派生留给后续切片，本 change 只把内核能力做出来。

- **WHEN** 引入 per-kind ceiling 能力之后
- **THEN** 三个既有构造点返回的策略与改动前完全一致
- **AND** 由它们构造的宿主在准入与激活上的判定文本逐位不变

#### Scenario: 三个构造点不受影响

- **WHEN** 读取 `ManagerPolicy()` / `adapterPolicy()` / edge 的 `Profile.Policy()`
- **THEN** 返回的策略不含任何 per-kind 条目，且全局上限与改动前相同
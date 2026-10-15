## ADDED Requirements

### Requirement: 用户偏好本地持久化(第一层)
系统 SHALL 将用户偏好持久化到本地。既有持久化能力(主题偏好、accent、结构性视图状态、模型选择)SHALL 保持回归验证;本层唯一新增能力 SHALL 为「常用 Agent 最近使用」:新建持久化 store 记录最近派发的 Agent persona(id 与时间戳),按最近使用自动排序并设条目上限,MUST 排除虚拟 persona `'default'`。⌘K Agent 面板与 Agent 档案墙 SHALL 按最近使用排序消费该偏好。第一层 MUST 不跨设备、MUST 不做跨会话语义记忆。记录 SHALL 只挂会话创建成功路径。

#### Scenario: 偏好跨会话保留
- **WHEN** 用户修改主题/accent/常用 Agent 后重新打开平台
- **THEN** 偏好保留(本地持久化)

#### Scenario: 派发即记录
- **WHEN** 用户经首页快捷卡或档案墙成功创建绑定某 persona 的会话
- **THEN** 该 persona 记入最近使用记录,刷新后保留

#### Scenario: 虚拟 persona 排除
- **WHEN** 会话绑定虚拟 persona `default`
- **THEN** 不记入最近使用记录

#### Scenario: 面板与档案墙按最近使用排序
- **WHEN** 用户打开 ⌘K Agent 面板或档案墙
- **THEN** 最近使用的 Agent 排序靠前

#### Scenario: 第一层边界
- **WHEN** 评估第一层能力范围
- **THEN** 仅本地偏好持久化(含最近使用 Agent),无跨设备同步、无跨会话语义记忆

### Requirement: 用户级记忆与注入(第二层,形态设计,本 change 不实现)
系统 SHOULD(分期)提供用户级记忆存储:结构化条目(偏好+事实+习惯),每条带来源与时间;会话开始时注入 Agent;前端提供记忆查看/编辑/删除入口。记忆 MUST 用户可见可删,MUST NOT 跨用户泄漏。本层需后端立项,本 change 只落形态设计与分期 spec,MUST NOT 在本 change 内实现。

#### Scenario: 新会话利用跨会话偏好(分期验收)
- **WHEN** 用户开启新会话提问
- **THEN** Agent 能利用已存的用户级偏好记忆

#### Scenario: 记忆透明可删(分期验收)
- **WHEN** 用户查看记忆
- **THEN** 可见并可删除任意条目

#### Scenario: 记忆不跨用户泄漏(分期验收)
- **WHEN** 多用户使用系统
- **THEN** 任一用户的记忆不注入其他用户的会话

### Requirement: 会话共享与 oncall 排班(第三层,形态设计,本 change 不实现)
系统 SHOULD(分期)支持会话共享(复用事件群聊形态)与 oncall 排班(值班表/当前值班人/交接)。告警 SHOULD 路由到当前 oncall 值班人并可经审批动线处理。排班 MUST 只影响告警路由与提醒对象及审批「谁可签」的候选人范围,MUST NOT 改变双签数量与约束。本层需后端立项,本 change 只落形态设计与分期 spec,MUST NOT 在本 change 内实现;org/多租户边界(Q6)MUST 在第三层实现前单独立项论证。

#### Scenario: 会话共享复用群聊形态(分期验收)
- **WHEN** 会话被共享给团队成员
- **THEN** 共享会话复用事件群聊形态(成员/消息流/审批卡)

#### Scenario: 值班交接(分期验收)
- **WHEN** 值班交接发生
- **THEN** 新值班人收到待办与进行中事件简报

#### Scenario: 告警路由到值班人(分期验收)
- **WHEN** 告警触发
- **THEN** 路由到当前 oncall 值班人并可经审批动线处理

#### Scenario: 排班不改双签约束
- **WHEN** 排班变更导致审批候选人变化
- **THEN** 双签数量与约束保持不变,仅候选人范围随排班调整

### Requirement: 三层分期顺序
团队记忆与协作 SHALL 按「第一层(本地偏好,本 change 实现)→ 第二层(用户级记忆,后端立项)→ 第三层(共享与排班,后端立项)」分期实现;第二层 MUST 优先于第三层。每层 SHALL 可独立验收。本 change 的实现范围为第一层补齐,二/三层保持形态与分期。

#### Scenario: 分期推进
- **WHEN** 规划实现排期
- **THEN** 第二层排期先于第三层;第一层本 change 内独立验收

#### Scenario: 层间独立验收
- **WHEN** 任一层完成
- **THEN** 该层可独立验收,不被未实现的其他层阻塞
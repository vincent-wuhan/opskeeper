# incident-group-view Specification

## Purpose
TBD - created by archiving change opskeeper-teamily-ui. Update Purpose after archive.
## Requirements
### Requirement: 事件群聊视图
IncidentDetail SHALL 提供「事件即群聊」视图形态:顶部为七阶段闭环进度条(检测/关联/调查/评审/审批/恢复/复盘),下方为群成员头像行(参与的 Agent 角色 + 用户),主体为融合时间线事件的诊断消息流,输入框支持在事件内追问。该视图 MUST 复用现有 incident timeline、诊断聊天与审批数据接口,零后端改动。

#### Scenario: 群聊视图渲染
- **WHEN** 用户打开处于调查阶段的事件并切到群聊视图
- **THEN** 顶部进度条高亮当前阶段,成员行显示已参与角色头像,消息流按时间呈现事件证据、诊断结论与用户消息

#### Scenario: 事件内追问
- **WHEN** 用户在群聊视图输入框发送问题
- **THEN** 消息进入该事件的诊断会话并触发 Agent 回复,回复呈现在消息流中

### Requirement: 审批卡内嵌
当事件存在待审批 proposal 时,群聊视图 SHALL 在消息流内嵌审批卡:展示提议动作、绑定校验状态(manifest/目标/命令/hash)、影响面摘要与 批准/拒绝/查看详情 动作;审批操作后卡片状态 MUST 实时更新。

#### Scenario: 待审批卡呈现与操作
- **WHEN** 事件产生待审批 proposal
- **THEN** 消息流出现审批卡,显示绑定校验与影响面;用户批准后卡片转为已批准态并同步全局审批计数

### Requirement: 详情 Tab 保留时间线
群聊视图 MUST 以 Tab/切换形态与原事件时间线共存:原时间线 SHALL 完整保留为「详情」视图,字段、筛选与导出能力不回退。

#### Scenario: 切换详情视图
- **WHEN** 用户在群聊视图切换到「详情」
- **THEN** 展示原事件时间线页,其既有能力(筛选、字段、操作)与改造前一致


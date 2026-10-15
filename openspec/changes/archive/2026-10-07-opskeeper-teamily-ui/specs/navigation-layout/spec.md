# navigation-layout 规格变更

## ADDED Requirements

### Requirement: 侧栏信息架构重排
侧栏 SHALL 按以下结构重排:「新对话」主 CTA 置顶;其后分组为 对话(最近会话)/ Agent(助理、节点 Agent、工作流)/ Discover(技能、插件、结晶)/ 运维(设备、拓扑、告警、监控、日志、链路)/ 日常(任务、报表、产物、知识库)/ 审批(常驻)/ 管理。既有路由 MUST 全部可达,不得因重排丢失入口。

#### Scenario: 新对话 CTA 置顶
- **WHEN** 用户查看侧栏顶部区域
- **THEN** 「新对话」以主按钮形态位于搜索之后、所有分组之前,点击直接创建新会话并进入对话页

#### Scenario: 既有入口无损
- **WHEN** 遍历重排后侧栏的所有导航项
- **THEN** 每个原有页面仍可通过侧栏或其子项到达,无死链

### Requirement: 审批常驻入口与未读红点
审批 SHALL 作为侧栏常驻项独立于分组展示,存在待审批 proposal 时 MUST 显示未读红点计数;点击进入审批中心。

#### Scenario: 待审批红点
- **WHEN** 系统存在 N(N>0)条待审批 proposal
- **THEN** 侧栏审批项显示红点与计数 N,审批清零后红点消失

### Requirement: Home 工作台
Home 页 SHALL 改版为工作台形态:按时段问候语(含未关闭事件/待审批计数摘要)、大输入框(支持 @提及与模型选择,提交即创建会话)、「你的 Agent」快捷卡行(点击直达 persona 对话)、「进行中」事项卡(运行中事件、待审批项,可跳转)、「试试这些」建议提示词卡。

#### Scenario: 问候与计数
- **WHEN** 用户进入 Home 页
- **THEN** 显示按时段的问候语与当日未关闭事件数、待审批数摘要

#### Scenario: 从 Home 直接开聊
- **WHEN** 用户在 Home 大输入框输入问题并提交
- **THEN** 系统创建新会话并跳转到对话页,首条消息为该问题

#### Scenario: Agent 快捷卡开始对话
- **WHEN** 用户点击「你的 Agent」行中的 persona 卡
- **THEN** 以该 persona 创建会话并进入对话页

### Requirement: 会话列表项升级
侧栏会话列表项 SHALL 呈现为 AgentAvatar + persona 名称 + 会话标题摘要,hover 展示完整标题;当前激活会话高亮。

#### Scenario: 会话项渲染
- **WHEN** 侧栏渲染会话列表
- **THEN** 每项显示对应 persona 的 AgentAvatar、persona 名称与标题摘要,激活项视觉高亮

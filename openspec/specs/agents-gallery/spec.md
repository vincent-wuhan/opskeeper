# agents-gallery Specification

## Purpose
TBD - created by archiving change opskeeper-teamily-ui. Update Purpose after archive.
## Requirements
### Requirement: Agent 档案墙卡片网格
Agents 页 SHALL 以卡片网格呈现全部可用 persona:每卡包含 AgentAvatar、persona 名称、角色职责一句话说明、可用工具集标签(Chip)、最近关联事件;内置 11 个 persona MUST 全部展示。

#### Scenario: 档案墙渲染
- **WHEN** 用户进入 Agents 页
- **THEN** 看到全部 persona 的卡片网格,每卡含头像、名称、职责说明、工具集与最近事件信息

### Requirement: 开始对话动线
每张 persona 卡 SHALL 提供「开始对话」动作,点击后以该 persona 创建新会话并跳转对话页;创建失败时 MUST 给出错误提示且不跳转。

#### Scenario: 从档案墙开始对话
- **WHEN** 用户点击某 persona 卡的「开始对话」
- **THEN** 系统以该 persona 创建会话并进入对话页,可直接发送消息

#### Scenario: 创建失败提示
- **WHEN** 会话创建接口返回错误
- **THEN** 卡片上呈现错误提示,用户停留在 Agents 页

### Requirement: persona avatar 数据与一致性
persona SHALL 支持 `avatar` 字段(emoji 或图片 URL),数据来源为 `agents/*.md` frontmatter 经 Agent 列表 API 透出——完整链路,不设前端编造映射。前端 SHALL 经 persona store 缓存 Agent 列表并向全部展示位提供 avatar 查找;avatar 值为 http(s) URL 时按图片渲染,非 http(s) 值一律按 emoji 文本渲染。侧栏会话列表、聊天头行、档案墙、群聊成员行等展示位 MUST 渲染同一来源的同一头像。avatar 图片加载失败或 avatar 缺失时 MUST 回退角色图标,不出现破图;Agent 列表拉取失败 MUST 静默回退,不产生用户可见错误。

#### Scenario: 各展示位一致渲染
- **WHEN** persona 数据包含 avatar
- **THEN** 侧栏会话列表、聊天头行、档案墙、群聊成员行渲染同一头像

#### Scenario: avatar 图片加载失败回退
- **WHEN** avatar 图片 URL 加载失败
- **THEN** 该处回退渲染角色图标,无破图

#### Scenario: emoji 文本渲染
- **WHEN** avatar 值为非 http(s) 开头的 emoji 文本
- **THEN** 该展示位渲染 emoji 文本头像,不按图片加载

#### Scenario: avatar 缺失回退角色图标
- **WHEN** persona 无 avatar 数据或 Agent 列表拉取失败
- **THEN** 该展示位渲染角色图标,无用户可见错误,系统 MUST NOT 编造 avatar


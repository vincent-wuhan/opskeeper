## ADDED Requirements

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
# agents-gallery 规格变更

## ADDED Requirements

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

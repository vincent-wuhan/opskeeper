# chat-conversation 规格变更

## ADDED Requirements

### Requirement: Agent 消息气泡化
对话流 SHALL 呈现双侧气泡:用户消息右对齐、accent 浅底、rounded-2xl(右下 rounded-md);Agent 消息左对齐、card 底色、rounded-2xl(左下 rounded-md),气泡上方带 AgentAvatar + persona 名头行。流式输出、自动滚动、消息操作 MUST 保持现有行为。

#### Scenario: 双侧气泡形态
- **WHEN** 对话流渲染一轮用户提问与 Agent 回复
- **THEN** 用户气泡右对齐浅紫底,Agent 消息左对齐卡片底并带头像行,两者均为大圆角气泡且角落半径差异化

#### Scenario: 流式行为不回退
- **WHEN** Agent 消息以流式方式逐步输出
- **THEN** 气泡容器随内容增长,滚动跟随与停止按钮行为与改造前一致

### Requirement: 工具调用卡嵌套
Agent 气泡内的工具调用 SHALL 渲染为嵌套圆角卡:单行摘要(工具名+耗时/状态)默认折叠,点击展开查看输入与结果;失败态 MUST 有明确的视觉标识。

#### Scenario: 工具卡折叠与展开
- **WHEN** Agent 消息包含工具调用
- **THEN** 默认仅显示单行摘要卡,点击后展开输入参数与结果详情

### Requirement: DeliverableCard 交付物卡
对话流 SHALL 识别 serve_page / reports / pages 产出的链接,以统一交付物卡渲染:图标 + 标题 + 摘要 + 「打开」动作,在消息流内收口呈现;无法识别的链接 MUST 回退为普通链接渲染,不影响原跳转。

#### Scenario: 交付物链接渲染为卡片
- **WHEN** Agent 消息包含 serve_page 托管页或报表产物链接
- **THEN** 该链接渲染为交付物卡(图标/标题/摘要/打开),点击在新窗口打开产物

#### Scenario: 普通链接回退
- **WHEN** 消息包含非交付物类外部链接
- **THEN** 按普通链接渲染,点击跳转目标与改造前一致

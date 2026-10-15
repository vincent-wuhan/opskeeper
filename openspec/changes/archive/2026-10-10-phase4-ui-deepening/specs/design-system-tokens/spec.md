## ADDED Requirements

### Requirement: 交付物卡视觉一致性

交付物卡（DeliverableCard）类型徽标 SHALL 复用 `Chip` 原语，不得手写重复的 pill 标记；卡内缩略图与展开预览 MUST 使用 `rounded-rk-*` 与 token 边框，不得混用裸 `rounded` 或裸 `zinc` 边框；加载态动画 MUST 受 `motion-safe` 约束。

#### Scenario: 类型徽标使用 Chip 原语
- **WHEN** 渲染交付物卡的类型徽标（如 report / page）
- **THEN** 徽标由 `Chip` 原语渲染，圆角为 `rounded-full`，浅底色调与全站 Chip 一致

#### Scenario: 卡内圆角与边框 token 化
- **WHEN** 检查交付物卡的缩略图与展开预览
- **THEN** 使用 `rounded-rk-*` 与 `border-border` token，深/浅主题下无半径或描边不一致

### Requirement: AgentAvatar 视觉层次

AgentAvatar SHALL 呈现与角色色一致的同色低透明度细环（`ring-1 ring-inset`，约 /20）以消除扁平感，使头像在与 `surface-card` 邻接处保有边界定义；细环 MUST 在深/浅主题下均可见，头像可见色调 MUST 保持与本规范既有 AgentAvatar 定义一致。头像在发言人姓名已作为可见文本呈现的语境中 MUST 保持装饰性（图标 `aria-hidden`），不得重复播报名称。

#### Scenario: 邻接卡片不扁平
- **WHEN** AgentAvatar 与 `surface-card` 容器相邻呈现
- **THEN** 头像带与角色色一致的低透明度细环，视觉层次与卡片协调，深/浅主题均可见

### Requirement: 会话上下文面板视觉一致性与只读性

ContextPanel SHALL 用 `rounded-rk-*` 统一列表项与折叠按钮的圆角，不得使用裸 `rounded`/`rounded-md`；面板 MUST 保持只读——任何分区内 MUST NOT 引入 button 元素。

#### Scenario: 面板圆角统一
- **WHEN** 展开会话上下文面板
- **THEN** 行项与折叠按钮均为 `rounded-rk-*`（rk 阶梯 token），无裸 `rounded` 残留

#### Scenario: 只读不破
- **WHEN** 检查上下文面板的提及与知识分区
- **THEN** 分区内无任何 button 元素（只读展示）

### Requirement: Discover 标签条键盘可达与面板关联

Discover 标签条 SHALL 支持左右方向键在标签间移动并切换，活动标签 MUST 持 roving `tabIndex`（活动=0，其余=-1），焦点对键盘用户 MUST 可见（由全局 `*:focus-visible` token 规则提供）。每个标签 SHALL 通过 `aria-controls` 关联其面板，面板 SHALL 持 `id`、`role="tabpanel"` 与 `aria-labelledby`。标签条 SHALL 对齐控制台页边距，活动标签 MUST 具明确填充对比。既有 `?tab=` 路由行为 MUST 保持不变。

#### Scenario: 方向键切换标签
- **WHEN** 焦点在 Discover 标签条上并按左右方向键
- **THEN** 选中标签随之移动，对应面板切换，`aria-selected` 与 `aria-labelledby` 同步更新

#### Scenario: 标签与面板关联
- **WHEN** 检查 Discover 的活动标签与面板
- **THEN** 活动标签的 `aria-controls` 等于面板 `id`，面板具 `role="tabpanel"` 且 `aria-labelledby` 指向该标签，非活动标签 `tabIndex=-1`

### Requirement: 日报预设入口视觉可辨识

「新建任务」下拉中的「每日值班简报」一键预设项 SHALL 与其相邻的普通新建项在视觉上可辨识（强调色图标 / 轻分隔 / 圆角之一或组合）；其文案、点击行为与提交载荷 MUST 保持不变——不得新增表单字段、端点或 persona。

#### Scenario: 预设入口可辨识
- **WHEN** 打开「新建任务」下拉
- **THEN** 日报预设项与普通「定时任务」项在视觉上可区分，点击后仍预填 daily / 09:00 / 飞书渠道 / 三节模板

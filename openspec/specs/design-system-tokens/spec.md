# design-system-tokens Specification

## Purpose
TBD - created by archiving change opskeeper-teamily-ui. Update Purpose after archive.
## Requirements
### Requirement: 七级圆角阶梯 token
系统 SHALL 在设计 token 层提供七级圆角阶梯 `--radius-xs/sm/md/lg/xl/2xl/3xl`(对应 4/8/12/16/24/32/40px),并在 Tailwind 配置中以 `borderRadius` 语义键接入,供组件统一消费;新增组件 MUST 使用 token 而非散落的任意像素值。

#### Scenario: 组件消费圆角 token
- **WHEN** 开发者在组件中使用 `rounded-lg`、`rounded-2xl` 等 token 化圆角类
- **THEN** 渲染出的圆角值与七级阶梯定义一致(xs=4px 至 3xl=40px),且浅色/深色主题下一致

### Requirement: 柔和阴影 token
系统 SHALL 提供 `--shadow-card` 与 `--shadow-pop` 两个阴影 token(浅色:低透明度柔和分层;深色:高对比投影),并接入 Tailwind `boxShadow` 语义键;Card 类容器 MUST 以阴影替代纯边框分层。

#### Scenario: 卡片呈现柔和分层
- **WHEN** 页面在浅色主题下渲染 Card 容器
- **THEN** 卡片呈现 `--shadow-card` 柔和阴影而非仅 1px 硬边框,深色主题下使用深色阴影变体

### Requirement: 品牌紫色阶扩展
系统 SHALL 在保留品牌紫 #8C6DF0 为主色的前提下扩展 50/100/600/700 色阶(#f3f0fe/#e6e0fd/#6d57d9/#5b49b8),用于浅底 chip、气泡底色与 hover 态;主色、字体、产品名 MUST 保持不变。

#### Scenario: 浅底 chip 使用紫阶
- **WHEN** 渲染角色标签或用户气泡背景
- **THEN** 使用 accent-50/100 浅紫底色,主按钮仍为品牌紫 #8C6DF0 填充

### Requirement: AgentAvatar 组件
系统 SHALL 提供 AgentAvatar 组件:32/40px 圆角方形(xs 阶梯),按 persona 角色映射到角色色浅底 + lucide 图标(覆盖 11 个内置 persona),并预留 `avatar` 字段——存在 avatar URL/emoji 时 MUST 优先渲染该字段,否则回退角色图标。

#### Scenario: persona 渲染角色图标
- **WHEN** 以 persona 标识(如 incident-investigator)渲染 AgentAvatar
- **THEN** 输出该角色对应的 lucide 图标与角色色浅底,同一 persona 在侧栏、聊天、档案墙三处呈现一致

#### Scenario: avatar 字段优先
- **WHEN** persona 数据包含 avatar URL
- **THEN** AgentAvatar 渲染该图片而非角色图标,失败时回退角色图标

### Requirement: ui 原语形态升级
Button/Card/Chip 原语 SHALL 升级为:主按钮 pill 形(rounded-full)+ 品牌紫填充,次级按钮 rounded-lg 描边;Card 为 rounded-2xl + shadow-card + 弱边框;Chip 为 rounded-full 角色色浅底。原语的 props 接口 MUST 保持向后兼容。

#### Scenario: 主按钮 pill 形态
- **WHEN** 使用 Button 原语默认(variant=primary)渲染
- **THEN** 按钮为 rounded-full pill 且品牌紫填充,既有调用点无需改 props 即获得新形态

### Requirement: 双主题与浅色兜底兼容
新增 token 与组件样式 MUST 在暗色(默认)与浅色主题下均正确渲染;浅色主题的 zinc 重映射兜底块 SHALL 与新增浅底色类同步扩展,不得破坏 Traces 表格、代码块、终端等 opt-out 场景。

#### Scenario: 双主题走查
- **WHEN** 切换暗/浅主题浏览任一改造页面
- **THEN** 新 token 驱动的组件(卡片阴影、紫阶 chip、气泡)在两主题下均无"浅底浅字"或对比度失效

#### Scenario: opt-out 场景回归
- **WHEN** 浅色主题下查看 Traces 表格、代码块、xterm 终端
- **THEN** 这些场景保持原有配色,未被新样式污染

### Requirement: 群聊动效语汇
事件群聊 SHALL 提供四类动效:消息进入(fade+上移)、Agent 回复中的 typing 呼吸点(结束后消失)、阶段进度指示器的阶段推进过渡、结晶徽标脉冲。全部动效 MUST 遵循系统「减少动态效果」设置(motion-safe),动效 MUST NOT 改变信息结构——不改排序、不隐藏内容。动效实现 MUST 复用既有动画语汇(`.anim-rise`/`.anim-fade`/`pulse-dot` 等),不引入动画库;普通单聊与控制台高密度场景 MUST NOT 加入新动效。

#### Scenario: 消息进入动效
- **WHEN** 群聊中出现新消息
- **THEN** 该消息以 fade+上移动效进入,内容完整可读

#### Scenario: typing 指示出现与消失
- **WHEN** Agent 生成回复中
- **THEN** 呈现 typing 呼吸点指示,回复完成后指示消失

#### Scenario: 减少动态效果时无动画
- **WHEN** 系统开启「减少动态效果」
- **THEN** 消息进入/typing/阶段推进/结晶脉冲无动画,信息完整可读,无内容被隐藏

#### Scenario: 动效不改变信息结构
- **WHEN** 任一动效播放
- **THEN** 消息排序、内容可见性与无动效状态完全一致

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


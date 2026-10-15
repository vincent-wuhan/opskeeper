# Tasks ·「运维的 Teamily」Web UI 改造

> 对应 specs 的 7 个能力;分三阶段可独立回滚提交,每阶段跑通 test/typecheck/lint + 双主题走查。

## 1. Phase 1 · 设计系统(design-system-tokens)

- [x] 1.1 在 `web/src/styles/index.css` 新增七级圆角阶梯 token `--radius-xs/sm/md/lg/xl/2xl/3xl`(4/8/12/16/24/32/40px),暗/浅双主题均定义
- [x] 1.2 在 `web/src/styles/index.css` 新增 `--shadow-card` / `--shadow-pop` 阴影 token(浅色柔和分层 / 深色高对比)
- [x] 1.3 扩展品牌紫 50/100/600/700 色阶(#f3f0fe/#e6e0fd/#6d57d9/#5b49b8),接入现有 RGB 三元组语义映射
- [x] 1.4 在 `web/tailwind.config.ts` 接入 `borderRadius` 与 `boxShadow` 语义键
- [x] 1.5 同步扩展浅色 zinc 重映射兜底块,保证新增浅底色类在浅色主题下正确
- [x] 1.6 升级 ui/ 原语:Button 主按钮 pill + 次级 rounded-lg 描边;Card rounded-2xl + shadow-card;Chip rounded-full 角色色浅底(props 向后兼容)
- [x] 1.7 新建 `AgentAvatar` 组件:32/40px 圆角方形、11 persona 角色→lucide 图标映射、角色色浅底、预留 avatar 字段(存在时优先渲染)
- [x] 1.8 抽出聊天气泡样式 token(用户右对齐 accent-50 底 / Agent 左对齐 card 底,角落差异化圆角)
- [x] 1.9 为 AgentAvatar 补单元测试(角色图标渲染、avatar 字段优先与回退)
- [x] 1.10 验证:pnpm test / typecheck / lint 全绿;浅色主题抽查 Traces 表格、代码块、xterm 无「浅底浅字」

## 2. Phase 2 · 布局与导航(navigation-layout)

- [x] 2.1 重排 `Sidebar.tsx` IA:新对话主 CTA 置顶,分组为 对话/Agent/Discover/运维/日常/审批/管理,确认全部既有路由可达
- [x] 2.2 审批项提升为侧栏常驻项 + 待审批未读红点计数(数据源为待审批 proposal 数量)
- [x] 2.3 升级会话列表项:AgentAvatar + persona 名 + 标题摘要,hover 完整标题,激活高亮
- [x] 2.4 Home 页改版:按时段问候语 + 未关闭事件/待审批计数摘要 + 大输入框(@提及 + 模型选择)
- [x] 2.5 Home 页「你的 Agent」快捷卡行:persona 卡点击复用 `createSession({ title, agent_id })` 模式直达对话
- [x] 2.6 Home 页「进行中」事项卡(运行中事件 / 待审批项,可跳转)与「试试这些」建议提示词卡
- [x] 2.7 会话列表项渲染测试(头像、persona 名、摘要、激活态)
- [x] 2.8 接通 Home 路由:`App.tsx` 注册 `/home`,侧栏「首页」导航项与折叠栏图标指向它(2.4/2.5/2.6 的成果此前不可达,scope 追加)
- [x] 2.9 补齐 persona 本地化标签:`AgentBadge` 两张标签表补 critic/verifier/reporter(评审员/验证员/报告员)+ 防回归断言覆盖 `PERSONA_VISUALS` 全部键(2.5 走查暴露的既有缺陷,scope 追加)

## 3. Phase 3 · 核心体验页

### 3.1 对话页(chat-conversation)

- [x] 3.1.1 MessageBubble:Agent 消息改为左对齐 card 气泡 + AgentAvatar 头行(保留流式/滚动/既有断言)
- [x] 3.1.2 工具调用卡嵌套圆角化:单行摘要默认折叠,点击展开输入/结果,失败态视觉标识
- [x] 3.1.3 新建 DeliverableCard 组件,识别 serve_page/reports/pages 链接渲染交付物卡,非交付物链接回退普通链接
- [x] 3.1.4 为 DeliverableCard 补测试(识别渲染 + 普通链接回退)

### 3.2 Agent 档案墙(agents-gallery)

- [x] 3.2.1 Agents 页卡片网格:AgentAvatar + persona 名 + 职责说明 + 工具集 Chip + 最近关联事件,覆盖 11 persona
- [x] 3.2.2 每卡「开始对话」动作(创建会话并跳转,失败给出错误提示不跳转)
- [x] 3.2.3 档案墙渲染与开始对话测试

### 3.3 Discover(discover-hub)

- [x] 3.3.1 新建 `pages/Discover.tsx`:Tab 聚合技能/插件市场/自愈结晶既有组件与数据接口,支持 `?tab=` 直达
- [x] 3.3.2 旧路由(技能/插件/结晶)重定向到 `/discover` 并保留 query 参数,路由表集中管理
- [x] 3.3.3 Discover Tab 切换与重定向测试

### 3.4 事件群聊视图(incident-group-view)

- [x] 3.4.1 IncidentDetail 群聊视图:七阶段进度条(复用 dpo/PhaseIndicator)+ 群成员头像行
- [x] 3.4.2 消息流融合事件时间线与诊断聊天(复用 ChatDrawer 数据源),输入框支持事件内追问
- [x] 3.4.3 消息流内嵌审批卡(提议动作、绑定校验、影响面、批准/拒绝/详情),操作后状态与全局审批计数同步
- [x] 3.4.4 原时间线保留为「详情」视图,字段/筛选/导出能力不回退
- [x] 3.4.5 群聊视图与审批卡交互测试

### 3.5 控制台换肤(console-reskin)

- [x] 3.5.1 Dashboard 统计卡/图表容器/列表容器换用新卡片语言(大圆角 + 柔和阴影 + pill 状态),数据逻辑不动
- [x] 3.5.2 Approvals 审批中心大卡化(动作、来源事件、绑定校验、影响面、操作按钮),双签策略标识保留
- [x] 3.5.3 换肤后控制台既有交互测试全绿

## 4. 收尾验证

- [x] 4.1 `pnpm dev` 双主题 + 中英双语逐页走查七个原型对应页面
- [x] 4.2 对照原型图核对:圆角阶梯、阴影层次、AgentAvatar 三处一致性、气泡形态、交付物卡
- [x] 4.3 与 teamily.ai 截图并排比对 Home/Chat/Agents 三页(圆角、密度、气泡、卡片层级)
- [x] 4.4 全量 `pnpm test` / `pnpm typecheck` / `pnpm lint` 通过

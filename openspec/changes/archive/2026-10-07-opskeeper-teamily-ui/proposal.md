# Proposal ·「运维的 Teamily」Web UI 改造

## Why

OpsKeeper 后端已具备七阶段自治闭环、HITL 双签审批、结晶自愈等差异化能力,但前端仍是 13px 高密度表格驱动控制台:Agent 无拟人形象、对话缺气泡形态、审批入口淹没、生态入口分散。参考 teamily.ai("The Human–Agent Platform")的 messenger 形态与柔和设计语言,把 OpsKeeper 升级为「运维的 Teamily」——人与运维 Agent 共处一个工作空间——能显著降低信任门槛,同时保留治理严肃性。行业(Datadog Bits AI、Rootly Copilot)正全面走向"对话优先调查 + 人保留控制权",**尚无产品把可审计审批做成 messenger 形态**,这是本改造要抢占的差异化空位。

## What Changes

- **设计系统升级**:引入七级圆角阶梯 token(4/8/12/16/24/32/40px)、柔和阴影 token(--shadow-card/--shadow-pop)、品牌紫色阶扩展(50/100/600/700);升级 ui/ 原语(Button→pill 主按钮、Card→rounded-2xl+shadow-card);新建 AgentAvatar 组件(persona 角色色浅底 + lucide 图标,预留 avatar 字段)
- **布局与导航重组**:侧栏 IA 重排——「新对话」主 CTA 置顶,分组为 对话/Agent/Discover/运维/日常/审批/管理;审批提升为常驻项 + 未读红点;Home 改版为工作台(时段问候 + 大输入框 + Agent 快捷卡 + 进行中事项 + 建议提示词);会话列表项升级为 AgentAvatar + persona 名 + 摘要
- **核心体验页改版**:
  - ChatThread 气泡化:用户右对齐 accent 浅底气泡(现有),Agent 消息改为左对齐 card 气泡 + AgentAvatar 头行;工具卡嵌套圆角化
  - 新增 DeliverableCard 组件:识别 serve_page/reports/pages 链接,以统一交付物卡收口进对话流
  - Agents 页档案墙:卡片网格展示 11 个 persona,含角色说明、工具集、最近事件、「开始对话」动线
  - Discover 统一壳:Tab 聚合 技能/插件市场/自愈结晶 三个分散入口,旧路由重定向
  - 事件群聊视图:IncidentDetail 升级为「事件即群聊」形态——七阶段进度条 + 群成员头像行 + 消息流融合诊断聊天 + 审批卡内嵌;旧时间线保留为「详情」Tab
  - Dashboard/Approvals 换肤:容器层换用新卡片语言,业务逻辑不动
- 保留:产品名 OpsKeeper、品牌紫 #8C6DF0、Inter/JetBrains Mono 字体、双主题 + 中英双语、既有信息架构骨架(控制台为主导航 + 对话融入)
- 明确不做:不引入 UI 组件库;不动后端 API;不全量替换 ~1500 处硬编码 zinc 类(沿用 light 重映射兜底策略);交付物深化、persona avatar 字段、上下文面板、remix 动线、AI 简报、记忆层等留待后续独立 change

## Capabilities

### New Capabilities

- `design-system-tokens`: 设计 token 与基础组件语言——七级圆角阶梯、阴影体系、品牌紫色阶、ui/ 原语(Button/Card/Chip)形态规范、AgentAvatar 组件、聊天气泡样式 token;暗/浅双主题与既有 accent 切换兼容
- `navigation-layout`: 全局布局与信息架构——侧栏分组重排(新对话 CTA/对话/Agent/Discover/运维/日常/审批红点/管理)、Home 工作台(问候/大输入/Agent 快捷卡/进行中/建议提示词)、会话列表项升级
- `chat-conversation`: 对话体验——ChatThread 双侧气泡形态(用户右/Agent 左 + 头像行)、工具调用卡嵌套圆角、DeliverableCard 交付物卡统一渲染 serve_page/reports/pages 链接
- `agents-gallery`: Agent 档案墙——persona 卡片网格、角色/工具集/最近事件展示、「开始对话」直达会话创建
- `discover-hub`: Discover 统一壳——技能/插件市场/自愈结晶 Tab 聚合、旧路由重定向、统一卡片语言
- `incident-group-view`: 事件群聊视图——七阶段闭环进度条、群成员头像行、消息流融合诊断聊天、审批卡内嵌、旧时间线保留为详情 Tab
- `console-reskin`: 控制台页面换肤——Dashboard/Approvals 等专业页面容器层换用新卡片语言(大圆角 + 浅阴影 + pill 状态),业务数据与交互逻辑不变

### Modified Capabilities

(无——`openspec/specs/` 当前为空,本次改造不修改任何存量 spec 级需求)

## Impact

- **代码**:`web/src/styles/index.css`(token 扩展 + light 重映射块同步)、`web/tailwind.config.ts`(borderRadius/boxShadow 扩展)、`web/src/components/ui/*`(原语升级)、`web/src/components/`(新增 AgentAvatar、DeliverableCard)、`web/src/components/Sidebar.tsx`、`MessageBubble.tsx`、`web/src/pages/`(Home、Agents、新建 Discover、IncidentDetail、Dashboard、Approvals)
- **路由**:新增 Discover 路由;Skills/PluginMarketplace/Crystallized 旧路由重定向
- **API/后端**:零改动——Phase 1-3 全部消费现有接口(persona 列表、session 创建、incident timeline、审批流、crystallize 查询)
- **依赖**:无新增 npm 包(AgentAvatar 用现有 lucide-react,交付物卡复用现有路由/链接组件)
- **风险**:light 主题依赖 ~200 行 zinc 重映射兜底,token/组件改动需抽查 Traces 表格、代码块等 opt-out 场景无"浅底浅字";~1500 处 zinc 硬编码不批量替换,新语言经组件传导逐步覆盖
- **验证面**:`pnpm dev` 双主题双语逐页走查、`pnpm test`/`typecheck`/`lint`(MessageBubble/ChatInput/Agents 现有断言不破,为 AgentAvatar/DeliverableCard/Discover 补测试)、与 teamily.ai 截图并排比对

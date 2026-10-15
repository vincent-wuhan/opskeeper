# Design ·「运维的 Teamily」Web UI 改造(高层框架)

> 深度技术设计在 design 阶段的 Design Doc 细化;本文档记录 open 阶段已确定的架构决策与方案选型。

## Context

前端现状:React 18.3 + TS 5.6 + Vite 5 + Tailwind 3.4(darkMode class)+ zustand,无 UI 组件库,`components/ui/` 6 个自绘原语。设计 token 为 CSS 变量 RGB 三元组 + Tailwind 语义映射(bg/card/border/text/accent/info/warn/ok/danger);暗色默认,浅色靠 `index.css` 中 ~200 行 `html.light .bg-zinc-900{...}` 重映射兜底,全库 ~1500 处硬编码 zinc 类。已有 ChatThread 流式对话、⌘K 面板、@提及、审批流、事件诊断聊天抽屉、双语双主题。

目标形态(参照 teamily.ai,保留品牌紫):messenger 式柔和设计——七级圆角阶梯、浅阴影大卡片、拟人 AgentAvatar、双侧气泡对话、审批/事件等治理要素显性化。

约束:不动后端 API;不引入组件库;浅色主题的 zinc 重映射兜底必须持续生效。

## Goals / Non-Goals

**Goals:**

- 建立 token 化的 teamily 风格设计系统(圆角/阴影/紫阶),经 ui/ 原语与新组件传导到页面
- 信息架构重排:对话入口权重提升、审批显性化、生态入口聚拢
- 五个核心页面达到原型形态:Home 工作台、对话气泡化、Agent 档案墙、Discover 统一壳、事件群聊视图
- 双主题 + 双语全程可用,现有测试/typecheck/lint 不破

**Non-Goals:**

- 不改产品名、不换字体、不换品牌主色、不推翻整体 IA
- 不动后端(Phase 1-3);不新增 npm 依赖
- 不全量替换 zinc 硬编码类(渐进,由组件传导)
- 交付物深化、persona avatar 持久化字段、上下文面板、remix 动线、AI 简报、记忆层 → 后续独立 change

## Decisions

1. **token 策略:扩展现有 CSS 变量体系,不另起炉灶。**
   新增 `--radius-xs/sm/md/lg/xl/2xl/3xl`(4/8/12/16/24/32/40px)、`--shadow-card/--shadow-pop`、品牌紫 50/100/600/700 色阶(`#f3f0fe/#e6e0fd/#6d57d9/#5b49b8`),在 `tailwind.config.ts` 以 `borderRadius`/`boxShadow` 语义键接入。替代方案(全新 token 层/引入 tailwind theme preset)被否:现有 RGB 三元组+语义映射已被全部页面消费,平移成本最低且天然兼容双主题。

2. **组件策略:升级自绘 ui/ 原语,不引入组件库。**
   Button 主按钮→pill、Card→rounded-2xl+shadow-card、Chip→pill 角色色浅底。理由:零新依赖、现有测试可续、~1500 处 zinc 硬编码下组件库迁移不可行。

3. **迁移策略:token 扩展 + 原语升级 + 高影响页面点改,保留 light 重映射兜底块并同步扩展。**
   风格经组件语言传导而非全量类名替换;对 opt-out 场景(Traces 表格、代码块)保持现状,抽查回归。

4. **AgentAvatar:纯前端组件,persona 角色色浅底 + lucide 图标,预留 `avatar` 字段。**
   persona → 角色 lucide 图标映射表内置于组件(调查员/评审员/修复员/验证员/报告员等 11 个);`avatar` URL/emoji 存在时优先渲染。持久化 avatar 字段属后续 change。

5. **对话气泡:增量改造 MessageBubble,不重写。**
   用户侧气泡已存在(`rounded-2xl rounded-br-md`);Agent 消息由全宽改为 card 气泡 + AgentAvatar 头行,工具卡内嵌圆角化。保持现有流式/滚动/测试断言兼容,新增类不删旧断言。

6. **DeliverableCard:前端链接模式识别,无后端配合。**
   统一渲染 serve_page/reports/pages 产出的链接为「图标+标题+摘要+打开」横向卡;识别规则走消息内容链接嗅探,失败时回退普通链接。

7. **事件群聊视图:IncidentDetail 内新增视图形态,复用既有数据源,不做数据改造。**
   七阶段进度条复用 dpo/PhaseIndicator,消息流复用 ChatDrawer 数据源,审批卡复用 Approvals 卡组件;旧时间线保留为「详情」Tab——可逆、渐进,闭环数据(七阶段/群成员/审批)全部已有。

8. **Discover:新页面 Tab 聚合现有组件,旧路由重定向不删除。**
   Skills/PluginMarketplace/Crystallized 组件原样嵌入 Tab;旧路由 302/Redirect 到 `/discover?tab=...`,书签与深链不断。

9. **Agents 档案墙:复用 `Home.tsx` 的 `createSession({ title, agent_id })` 模式实现「开始对话」。**
   卡片数据来自现有 persona 列表接口,无新接口。

## Risks / Trade-offs

- [浅色主题兜底失效:token/组件改动可能让重映射块漏覆盖新类名] → shadow/radius 不走 zinc;新增浅底色用 accent-50 系变量;走查清单强制覆盖 Traces 表格、代码块、xterm 三类 opt-out 场景
- [风格不一致过渡期:~1500 处 zinc 硬编码与新 token 语言并存] → 接受渐进;原语 + 五个核心页先达标,视觉传导随后续 change 推进
- [现有前端测试破坏(MessageBubble/ChatInput/Agents 断言)] → 增量改类名不改 DOM 结构;破坏时以「新形态断言」替换而非删除用例
- [事件群聊视图信息密度过载(消息流+工具卡+审批卡混合)] → 工具调用默认折叠为一行摘要卡,展开看详情;审批卡常驻吸附;详情 Tab 兜底完整时间线
- [Discover 重定向遗漏深链] → 重定向保留 query 参数;旧路由路径集中在一个路由表常量里管理

## Migration Plan

纯前端改造,分三个可独立回滚的阶段提交:Phase 1 设计系统(token/原语/AgentAvatar/气泡样式)→ Phase 2 布局导航(Sidebar/Home/会话列表)→ Phase 3 核心页(ChatThread/Agents/Discover/事件群聊/Dashboard/Approvals)。每阶段跑通 test/typecheck/lint + 双主题走查后合入;回滚 = revert 对应阶段提交。无数据迁移、无接口变更、无发布编排依赖。

## Open Questions

- 事件群聊视图中诊断聊天与事件 timeline 的消息合并排序规则(按时间戳混排 or 分区)→ design 阶段 Design Doc 定
- AgentAvatar 角色→图标映射的具体分配与 11 个 persona 的默认色 → design 阶段定
- Discover 三 Tab 的默认项与结晶 Tab 的空态引导 → design 阶段定

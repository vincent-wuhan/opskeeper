## Context

本 change 是纯打磨（tweak），不改后端、不加依赖、不加功能。所有改动都收敛到**既有 token**：`surface-card`、`rounded-rk-*`（4/8/12/16/24/32/40px 七级阶梯）、`shadow-card`/`shadow-pop`、`Chip` 原语、语义色 `ok/warn/danger/info` 与品牌紫阶 `accent-50/100/600/700`。原则：**能用 token 就不写裸 `zinc-*`；能复用原语就不手写重复标记。**

双主题靠 `html.light` 选择器上的 `zinc` 重映射块兜底。因此新增样式**优先使用 token 类**（`rgb(var(--x))` 驱动，自动双态），避免引入新的裸 `zinc` 类——后者需要同步扩写重映射块，容易漏。本次不改动重映射块的既有语义，只在其确需覆盖时做最小追加。

## 实现说明

### 1. 交付物卡（`components/DeliverableCard.tsx`）
- 类型徽标（现为手写 `rounded bg-zinc-800 px-1.5 py-0.5 text-[10px] text-zinc-300`）改用 `Chip` 的 neutral 语调，消除重复 pill 标记。
- 容器、缩略图、展开预览的 `rounded`/`rounded-rk-*` 混用统一为 `rounded-rk-sm`/`rounded-rk-md`；缩略图与预览的 1px 边框统一到 `border-soft`。
- 加载态 `animate-spin` 外层补 `motion-safe:`。

### 2. AgentAvatar（`components/AgentAvatar.tsx`）
- 保留 `TONE_CLASS` 的 `-500/10` 浅底 + `-300` 图标语汇，但抽取为与 `Chip` 语气同源的映射（同色系）；不改可见色调，只统一来源。
- 与 `surface-card` 邻接时不加环会显扁：新增 `ring-1 ring-inset` 搭配同色低透明度，深浅主题均可见。
- 代表发言人时补 `aria-label={角色名}`；纯装饰语境保持 `aria-hidden`。**注意**：既有测试锁定了 `bg-violet-500/10`、`h-8/h-10`、`rounded-rk-sm/rk-md`，且断言 className 不得含 `rounded-s-`——新增类只能追加，不能替换这些。

### 3. ContextPanel（`components/ContextPanel.tsx`）
- 裸 `rounded`/`rounded-md` → `rounded-rk-sm`：折叠按钮与两类行项统一到 rk 阶梯。
- 知识引用项**不做**跳转改造：它们是纯 `<div>`、没有路由目标，加跳转即新增功能（用户已排除「不要新增功能」）；给非交互行项加 hover 亦会误导为可点。故本表面只做圆角 token 收敛。
- **硬约束**：面板只读——既有测试断言「分区内零 button」，任何新增都不得引入 button。

### 4. Discover（`pages/Discover.tsx`）
- 标签条原本左对齐贴边（root 无 padding）→ 补 `ml-6 mt-4` 对齐控制台页边距（PageHeader 用 `px-6`）。
- 「消除双重 chrome」**不做**：内嵌三页各自渲染自己的 PageHeader，消除它必须改那三张页面，超出本 change「五处表面」范围。
- 键盘与语义：标签加 `id`/`aria-controls`/roving `tabIndex`，面板加 `id`/`aria-labelledby`；左右方向键切换并让焦点跟随。焦点可见性由全局 `*:focus-visible` 规则提供，不额外加样式。
- 路由行为不变（`?tab=` 读写逻辑一字未动）。

### 5. 日报预设入口（`pages/Tasks.tsx`）
- 该按钮原与上下两个普通项类名完全相同。加一条轻分隔（`border-t border-zinc-800/60`）把它与普通新建项区分开。强调色图标已有（`text-sky-400/80`）。**不改文案、不改 onClick、不改 `dailyBriefSeed` 载荷。**

### 6. 简报卡（`components/ReportCards.tsx`、`components/ReportContent.tsx`）—— **本次不做（实现期发现，已从 delta spec 移除）**
- 两者的容器（ReportCards 卡格 `rounded-xl + bg-zinc-900/40`；ReportContent 的四行统计卡）**已被浅色 shim 正确覆盖**（`index.css:142,180` 明确映射 `.bg-zinc-900\/40`）——不存在浅色下「深卡压浅底」缺陷，迁移到 `surface-card`（不透明）只是半透明改不透明，属**无 bug 可修的纯改外观**。
- ReportContent 的四行配色（indigo/rose/violet/cyan）是 HLD-014 刻意设计的报告正文语汇，非应用语义色；ReportCards 无 co-located 测试（渲染卡格需 ResizeObserver 桩），本会话又无法目视验证。收益不可验证、下行风险真实，故不动。
- 用户在聊天里看到的「简报卡」实为 D1 交付物卡（`ReportHostedView` 嵌在 `DeliverableCard` 预览里），已在 §1 覆盖。

### 7. ui 原语（`components/ui/Button.tsx`）—— **本次不做（实现期发现，已从 delta spec 移除）**
- 原计划把 ghost/danger/subtle 的裸 `zinc-*` 收敛到 token。实现期核实后放弃：
  1. **token 不等于等价替换。** 深色主题下 `--border`(40,40,50) 比 `border-zinc-700`(63,63,70) 暗、`--text-muted`(zinc-400) 比 `text-zinc-300` 暗、`hover:bg-border/40` 比 `hover:bg-zinc-800` 弱——转换会**静默改变每个 Button 的外观**（ghost 55 处、danger 18 处、subtle 14 处）。
  2. **subtle 依赖专用兜底规则。** 浅色 shim 为 `bg-zinc-100 text-zinc-900` 写了独立复合选择器（`index.css:157-167`）承载「深色上的亮 pill」这一反向语义，没有反向 surface token，转换需新增 token。
  3. **本会话无法目视验证**（鉴权门），做全局性改色风险不可控；代码库 shim 注释本身也把这次迁移标注为多日重构、暂缓。
- 结论：原语保持现状（裸 zinc 由 shim 保证双主题正确）。该要求已从 delta spec 删除，属**范围收缩**而非新增。

## 测试策略

- **不改既有断言、不删既有用例**；新行为以追加断言覆盖。
- 每个表面改完后运行其 co-located 测试；全量 `cd web && pnpm test` 必须全绿。
- 关键回归风险：AgentAvatar（类名被精确锁定）、ContextPanel（只读零按钮断言）、DeliverableCard（`h-40` 占位尺寸断言）、Discover（路由与 `aria-selected`）。改这四个文件前先读对应测试。

## 验证

- `cd web && pnpm test && pnpm typecheck && pnpm build` 全绿。
- 浏览器（本地已启动：后端 :8090、前端 :5174）双主题 × 中英双语四组走查五处表面。
- Non-Goals 复核：无新功能、无后端改动、无新依赖、无 IA 变化、未删既有测试。

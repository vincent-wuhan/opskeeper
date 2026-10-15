## Why

Phase 4 的五个 UI 表面（交付物卡、AgentAvatar、ContextPanel 会话上下文面板、Discover、日报预设与简报卡）已陆续交付，但视觉与交互密度参差：一部分表面已用上新的 `rounded-rk-*` 圆角阶梯与 `surface-card` 阴影，另一部分仍停留在早期的 `rounded-xl` + 裸 `zinc-*`；同一类元素（徽标、头像、标签条、卡片）在不同表面呈现不一致。这让「运维的 Teamily」控制台看起来像几个不同系统拼在一起。

五处刚交付、上下文还新鲜，正是把它们统一到既定设计规范的最佳时机。本次只做打磨，不新增任何功能。

## What Changes

- **交付物卡**：类型徽标改用 `Chip` 原语（去掉手写重复的 pill 标记）；卡内圆角/边框/缩略图容器统一到 token；加载态动画补 `motion-safe` 约束。
- **AgentAvatar**：补可访问标签（代表发言人时）；角色色与 `Chip` 语义色同源；与 `surface-card` 邻接处补一致细环，消除扁平感。
- **ContextPanel**：裸 `rounded`/`rounded-md` 统一为 `rounded-rk-*`；列表项补 hover，知识引用项可跳转；分区标题层级统一；窄屏可折叠宽度。保持只读（分区内不引入按钮）。
- **Discover**：标签条与内容面板视觉衔接（消除双重 chrome）；补 `focus-visible`、方向键切换与 `aria-controls`/`role=tabpanel` 关联。
- **日报预设入口**：在「新建任务」下拉中与相邻普通项视觉可辨识（强调色图标 / 轻分隔 / 圆角），文案、点击行为与提交载荷保持不变。
- **简报卡**：**本次不做** —— `ReportCards`/`ReportContent` 已被浅色 shim 正确覆盖（`index.css:142,180`），迁移属无 bug 可修的纯改外观，且本会话无法目视验证（见 design.md §6）。聊天内的简报卡实为 D1 交付物卡，已在交付物卡项覆盖。
- （原「ui 原语去裸 zinc」一项在实现期撤销：token 非等价替换会静默改变外观，见 design.md §7。）

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `design-system-tokens`：新增「表面视觉一致性」要求（交付物卡视觉一致性、AgentAvatar 细环、ContextPanel 圆角与只读、Discover 标签可达性、日报预设可辨识）。delta spec 见 `specs/design-system-tokens/spec.md`。

## Impact

- **代码**：`web/src/components/{DeliverableCard,AgentAvatar,ContextPanel}.tsx`、`web/src/pages/{Discover,Tasks}.tsx`。
- **测试**：不得删除任何既有测试用例；`AgentAvatar.test.tsx`/`DeliverableCard.test.tsx`/`ContextPanel.test.tsx`/`Discover.test.tsx` 的既有断言不得被削弱。新增断言一律追加。
- **无后端改动、无新依赖、无信息架构变化、无新增功能。**

## 1. 交付物卡

- [x] 1.1 `web/src/components/DeliverableCard.tsx`：类型徽标改 `Chip` 原语；容器/缩略图/预览圆角统一 `rounded-rk-*`、边框统一 `border-border`；加载动画补 `motion-safe:`
- [x] 1.2 追加断言锁定 Chip 徽标与圆角 token（不删既有，尤其 `h-40` 占位断言）

## 2. AgentAvatar

- [x] 2.1 `web/src/components/AgentAvatar.tsx`：补同色低透明度细环（`ring-1 ring-inset`），消除邻接 card 的扁平感；不放 `aria-label`（发言人姓名已是可见文本，避免重复播报）
- [x] 2.2 追加断言（不得改动既有锁定的 `bg-violet-500/10`、`h-8/h-10`、`rounded-rk-sm/rk-md`，不得让 className 含 `rounded-s-`）

## 3. ContextPanel

- [x] 3.1 `web/src/components/ContextPanel.tsx`：裸 `rounded`/`rounded-md` → `rounded-rk-sm`（折叠按钮 + 两类行项）
- [x] 3.2 追加断言（不得破坏「分区内零 button」只读断言与既有 href/`data-message-id` 断言）

## 4. Discover

- [x] 4.1 `web/src/pages/Discover.tsx`：标签条对齐页边距（`ml-6 mt-4`）；`id`/`aria-controls`/`aria-labelledby`/roving `tabIndex` 关联；左右方向键切换（焦点跟随）
- [x] 4.2 追加断言（不得破坏既有 `?tab=` 路由与 `aria-selected` 断言）

## 5. 日报预设入口

- [x] 5.1 `web/src/pages/Tasks.tsx`：日报预设项加轻分隔（`border-t`）与相邻普通新建项区分，文案与 onClick 与 `dailyBriefSeed` 载荷不变
- [x] 5.2 追加断言锁定预设入口可辨识（不破坏既有预填/载荷断言）

## 6. 全量验证与走查

- [x] 6.1 `cd web && pnpm test` 全绿（exit 0）、`pnpm typecheck`、`pnpm build` 均 exit 0
- [x] 6.2 浏览器双主题 × 中英双语走查五处表面 —— 由 coordinator 真机执行(用户授权登录 admin 账号):10 张截图覆盖 交付物卡(深/浅 × 中/英,含就地展开预览)、AgentAvatar 细环(深/浅,聊天+档案墙)、Discover 标签条(深/浅)、日报预设分隔线(深/浅 × 中/英)、上下文面板空态(深/英);面板内容态由单测覆盖。截图已交付用户,存 `/tmp/opskeeper-ui-shots/final/`
- [x] 6.3 Non-Goals 复核：无新功能、无后端改动、无新依赖、无 IA 变化、未删除既有测试用例

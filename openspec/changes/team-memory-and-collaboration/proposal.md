## Why

OpsKeeper 目前是「单人 + Agent」:偏好不跨会话、会话不共享、没有值班排班,离 teamily 的 Living Memory(跨上下文活记忆)与协作层(Human–Agent Platform 的「团队」一词)还有最后一块形态空白。本 change 落地「团队 + Agent 共治」的形态与分期——跨会话偏好记忆、会话共享(值班交接)、oncall 排班。这是 Phase 4 功能形态深化的 D6 方向。

**现实修正(2026-10-08 设计核查)**:第一层的主题/accent/结构视图态/模型选择持久化**已存在**(mode.ts/theme.ts/ui.ts/modelSelection.ts),唯一真缺口是「常用 Agent」;本 change 的真实实现 = 第一层补齐(最近使用 Agent),二/三层完成形态设计并保持分期(后端立项待排期)。

## What Changes

- **第一层(本 change 真实实现)**:既有持久化(theme/mode/ui/modelSelection)回归验证;唯一新增 = 最近使用 Agent persist store(persona id+时间,自动衰减排序、上限裁剪、排除虚拟 `'default'`),记录钩子挂 `createSession({agent_id})` 成功路径(Home 快捷卡 + 档案墙),⌘K 面板与档案墙按最近使用排序
- **第二层(形态设计,不实现)**:用户级记忆存储(偏好+事实+习惯,带来源时间)、chatruntime 会话开始注入、前端记忆管理入口(可见可删、不跨用户泄漏)
- **第三层(形态设计,不实现)**:会话共享(复用事件群聊形态);oncall 排班(值班表/当前值班人/交接,告警路由+审批候选人范围联动,双签数量约束不变)
- **分期顺序**:第一层(本 change 实现)→ 第二层(后端立项)→ 第三层(后端立项);Q6(org/多租户)第三层实现前单独立项论证

## Capabilities

### New Capabilities

- `team-memory`:团队记忆与协作层形态——三层分期(本地偏好持久化含最近使用 Agent/用户级记忆/会话共享与排班)、记忆透明可删、排班不改双签约束、本 change 实现范围为第一层补齐

### Modified Capabilities

(无)

## Impact

- 第一层(真实实现):`web/src/store/` 新增最近使用 Agent persist store;`web/src/pages/Home.tsx`(快捷卡派发处)与 `web/src/pages/Agents.tsx`(档案墙)挂记录钩子;⌘K 面板/档案墙排序消费——纯前端
- 第二/三层(形态设计):Design Doc 固化立项输入(记忆结构/注入点/管理入口;共享形态/排班/治理边界),后端立项待排期
- 不做多租户重建(org 域已裁,Q6 第三层前单独立项论证);不改双签约束;二/三层本 change 零实现代码
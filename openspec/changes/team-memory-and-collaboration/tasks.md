## 1. 前置核查

- [x] 1.1 核查第一层现状:主题(`store/mode.ts` localStorage)/accent(`store/theme.ts` persist)/结构视图态(`store/ui.ts` persist,partialize 刻意只含 sidebarCollapsed)/模型选择(`store/modelSelection.ts` persist)全部已持久化——第一层唯一真缺口 = 常用 Agent
- [x] 1.2 核查派发落点:`createSession({ agent_id })`(api/chat.ts:80)为唯一 persona 派发路径——Home 快捷卡(`Home.tsx:348`)与档案墙(Agents.tsx);虚拟 persona `'default'` 绑主输入框(`Home.tsx:327`)需排除
- [x] 1.3 定案(2026-10-08 用户确认):常用 Agent = 最近使用自动记录(非显式 pin);本 change 真实实现范围 = 第一层补齐,二/三层形态设计定稿保持分期

## 2. 第一层实现(本 change 真实实现)

- [ ] 2.1 新增最近使用 Agent persist store:persona id + 时间戳,按最近使用排序,条目上限裁剪(8 条),排除虚拟 persona `'default'`;既有持久化(theme/mode/ui/modelSelection)回归验证不重做
- [ ] 2.2 记录钩子:Home persona 快捷卡与档案墙 createSession 成功路径各挂一处记录;失败不记
- [ ] 2.3 排序消费:⌘K Agent 面板与档案墙按最近使用排序;`ui.ts` partialize 不扩展(既有刻意决策)
- [ ] 2.4 测试:新 store 单测(记录/排序/衰减/上限/排除 default);派发后记录断言;面板与档案墙排序消费断言;既有测试只更新不删

## 3. 第二层形态定界(需后端,独立立项,本 change 不实现)

- [ ] 3.1 立项输入固化入 Design Doc:用户级记忆结构(偏好+事实+习惯,带来源时间)、chatruntime 会话开始注入点、前端记忆查看/编辑/删除入口形态
- [ ] 3.2 记忆安全约束固化:可见可删、不跨用户泄漏、注入条目可溯源

## 4. 第三层形态定界(需后端,独立立项,本 change 不实现)

- [ ] 4.1 立项输入固化入 Design Doc:会话共享复用事件群聊形态;oncall 排班(值班表/当前值班人/交接/告警路由/审批候选人联动)
- [ ] 4.2 治理边界固化:排班只影响路由与提醒及候选人范围,不改双签数量约束;与 IM 联动方式;Q6(org/多租户)为第三层实现前单独立项论证项

## 5. 验证

- [ ] 5.1 `cd web && pnpm test` 全绿(exit 0),`pnpm typecheck` exit 0,`pnpm build` exit 0
- [ ] 5.2 走查:⌘K 面板与档案墙最近使用排序生效;偏好跨会话保留;双主题+双语下排序与展示正确
- [ ] 5.3 复核 Non-Goals 全部未破:第一层无跨设备/无跨会话语义记忆;二/三层零实现代码;双签约束未改;无多租户重建
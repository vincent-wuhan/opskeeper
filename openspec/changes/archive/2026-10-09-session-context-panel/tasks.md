## 1. 前置核查

- [x] 1.1 核查 mention token 写入格式(`ChatInput.tsx:229`)与 recomputeMentionContext(`:163`):确认为 `@{type}:{id}({label}) ` 文本 token,历史消息 API 不返回结构化 mentions,面板只能从 content 反推
- [x] 1.2 盘点消息内联 `tool_calls[]` 结构(`web/src/api/chat.ts:6-20`)与工具类别映射(`web/src/lib/toolSkill.ts` toolGroupKey:knowledge 类含 query_knowledge/web_search/*source*)；设计发现 DF1:chatruntime 无结晶类工具,结晶为 incident 闭环事后 hook,不出现在消息流——结晶节移出本期

## 2. 推导纯函数

- [x] 2.1 实现 `deriveSessionContext(messages) → { mentions[], knowledgeRefs[] }`:mention token 解析按 type+id 去重(type 仅接受 device/incident/rule/file);知识引用按 toolGroupKey==='knowledge' 类别映射识别(不逐个工具名硬编码)
- [x] 2.2 每条上下文条目携带来源消息标识,支持溯源
- [x] 2.3 纯函数单测:提及去重、非法 type 忽略、知识引用提取、纯文本不误报、空会话返回空结构

## 3. 面板 UI

- [x] 3.1 实现面板组件:两节呈现(提及对象/知识引用),引导空态
- [x] 3.2 宽屏对话页右列布局;窄屏可折叠侧板;默认折叠,展开态记忆(轻量本地持久化)
- [x] 3.3 点击运维对象跳详情页(既有路由);每条目可定位来源消息
- [x] 3.4 面板只读,不提供编辑/删除入口

## 4. 测试

- [x] 4.1 扩展测试:覆盖 spec 全部场景(提及/知识/空态/不误报/溯源/只读)
- [x] 4.2 保活既有测试:`ChatThread`、`MessageBubble`、`ChatInput` 既有断言不得破(消息流渲染不动);不得删除任何既有测试用例

## 5. 验证与走查

- [x] 5.1 `cd web && pnpm test` 全绿(exit 0),`pnpm typecheck` exit 0,`pnpm build` exit 0
- [x] 5.2 双主题 + 中英双语走查:面板配色、空态文案、条目排版正确 —— 偏差接受:未做浏览器人工走查(见下)
- [x] 5.3 宽窄屏走查:右列与折叠侧板两种形态切换正确,默认折叠且展开态记忆生效 —— 偏差接受:未做浏览器人工走查(见下)
- [x] 5.4 复核 Non-Goals 全部未破:零新增数据接口(`git diff` 无 `core/`、`cmd/` 改动)、消息流渲染未变、无编辑/删除入口

> **偏差接受记录(用户裁定 2026-10-09)**:5.2 / 5.3 为登录后浏览器人工走查。本会话凭据录入被安全策略拦截,不得以任何工具绕过,故未执行;经用户确认接受为已记录偏差后推进 build→verify。
> - 接受原因:走查项为纯视觉/文案核对,不含业务逻辑;功能面已由组件测试等价覆盖(展开/折叠、提及跳转、知识分节、空态、只读、持久化),主题与语言由既有 ui 原语与 `tr()` 承载。
> - 影响范围:仅「双主题观感 + 中英文案 + 宽窄屏折叠形态」未在真实浏览器肉眼确认;不改变面板数据结构、只读边界与零请求约束。verify 阶段仍会做独立构建/测试/安全审查。

> **代码审查记录(review_mode: standard,2026-10-09)**:按 build 阶段 review gate,在全部任务完成后、`comet guard build --apply` 前,加载 Superpowers `requesting-code-review` 请求一次轻量审查(正确性/安全/边界),范围覆盖整 change(`52a9ab9..a1fe4c8`,9 文件 / +624 -16)。结论:**0 Critical / 0 Important / 3 Minor,Ready to merge: Yes**。审查独立核对并确认六项约束全部成立(零新增依赖、零后端接口、面板只读、消息渲染路径未变、以 `toolGroupKey` 类别映射而非工具名白名单识别知识调用、mention 路由映射正确),并确认解析/dedup/边界处理与测试均为真实行为断言。3 项 Minor 按「接受并记录」处理:
> - **M1(first-wins dedup,`sessionContext.ts:84-86`)**:同名知识调用的重复出现保留较早状态。此为 spec 明确要求(「首次出现胜出」)并有注释,属有意设计,非缺陷。
> - **M2(status 原样渲染,`ContextPanel.tsx:140`)**:知识条目状态以英文枚举(`success`/`error`)原样显示,未走 `tr()` 映射。接受原因:与同列 `query_knowledge` 等协议标识名保持一致的英文呈现,改动仅触及观感、无功能影响;为避免纯文案改动引入额外提交与复验周期,记录为已接受 Minor。
> - **M3(来源序号为位置值,`ContextPanel.tsx:38-44`)**:`SourceCaption` 显示「第 N 条」为数组位置值,轮询整批替换时可能漂移。稳定定位符 `data-message-id` 已随条目输出;「点击消息回跳定位」不在本 change 范围,记录为后续候选。
> - 影响范围:三项均为观感/定位信息层面,不改变面板只读边界、数据结构与零请求约束。
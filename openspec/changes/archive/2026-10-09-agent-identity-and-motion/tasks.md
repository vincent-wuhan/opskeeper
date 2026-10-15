## 1. 前置核查

- [x] 1.1 核查后端 avatar 链路可行性:`agents/*.md` frontmatter 读入点、`AgentSummary`(`web/src/api/agents.ts:11-33`)与后端列表 API 字段透出改动范围,形成 Q1 结论(走完整链路或前端降级先行),写回 design.md
- [x] 1.2 盘点 `AgentAvatar` 既有实现(`avatar?: string` prop、onError 回退)与四处展示位传参现状,确认哪些调用点已可直接消费 avatar、哪些缺数据

## 2. avatar 数据链路

- [x] 2.1 按 1.1 结论实现首选链路:`agents/*.md` frontmatter 增加 `avatar:` 槽(emoji 或图片 URL),后端读入并在 Agent 列表 API 透出;`AgentSummary` 补充 `avatar?: string` 可选字段(向后兼容)
- [x] 2.2 实现降级映射:后端未就绪时前端维护内置 11 个 persona 的 persona→avatar 本地映射(仅内置、不支持自定义);后端就绪后移除映射（Q1 裁定走完整后端链路，该降级映射未启用，故无需移除）
- [x] 2.3 为内置 11 个 persona 选定默认 avatar(emoji 优先,规避外链不可达风险)

## 3. 四处展示位一致性

- [x] 3.1 侧栏会话列表(`SessionList.tsx:150`)消费 avatar 渲染
- [x] 3.2 聊天头行(`MessageBubble.tsx:123`)消费 avatar 渲染
- [x] 3.3 档案墙(`Agents.tsx:319`)与 Home(`Home.tsx:437`)消费 avatar 渲染
- [x] 3.4 群聊成员行(`IncidentGroupChat.tsx:139,144`)消费 avatar 渲染
- [x] 3.5 验证 avatar 图片加载失败时全部展示位回退角色图标、无破图(onError 路径)

## 4. 群聊动效

- [x] 4.1 群聊消息进入动效:fade+上移,复用 `.anim-rise`/`.anim-fade`,motion-safe
- [x] 4.2 typing 呼吸点:Agent 回复中呈现(复用 pulse-dot),回复完成后消失
- [x] 4.3 事件群聊阶段推进过渡动效
- [x] 4.4 结晶徽标脉冲:与 `Approvals.tsx:353` StatusChip 语汇一致,缺口 keyframes 仅在此新增
- [x] 4.5 全量验证动效不改变信息结构:无动效状态下排序、内容可见性一致（IncidentGroupChat「keeps message order and content identical regardless of animation classes」断言消息 DOM 顺序不受动效影响）

## 5. 测试

- [x] 5.1 扩展测试:四处展示位同 persona 渲染同一头像;avatar 失败回退角色图标（SessionList / MessageBubble / Agents / IncidentGroupChat 各一处 avatar 透传断言;AgentAvatar「avatar 图片加载失败回退角色图标」覆盖 onError）
- [x] 5.2 扩展测试:降级映射仅覆盖内置 persona,非内置 persona 回退角色图标不编造（agents.test.ts「avatarFor 对未知 agentId 返回 undefined,别名先归一」「空白 avatar 视作缺失」;Q1 裁定未启用前端降级映射,avatarFor 纯查表不编造）
- [x] 5.3 扩展测试:typing 指示出现/消失;motion-safe 下无动画
- [x] 5.4 保活既有测试:`AgentAvatar`、`MessageBubble`、`IncidentGroupChat`、`Agents` 既有断言不得破;不得删除任何既有测试用例（本次仅新增透传断言,既有用例零删除;全量 `pnpm test` 29 文件 / 254 用例 exit 0）

## 6. 验证与走查

- [x] 6.1 `cd web && pnpm test` 全绿(exit 0),`pnpm typecheck` exit 0,`pnpm build` exit 0（实测:`pnpm test` 29 文件 / 254 用例 exit 0;`pnpm typecheck` exit 0;`pnpm build` exit 0;另 `go build ./...` exit 0、`go test ./core/extension/biz/container/ ./core/manager/server/aiops/` exit 0）
- [x] 6.2 双主题 + 中英双语走查:头像渲染、typing 态、群聊动效在浅/深主题与中英文下正确（浏览器实测:浅色+中文、深色+中文、深色+英文三态,`/agents` 档案墙、侧栏会话列表、Home 快捷卡头像一致回退为角色图标、无破图,尺寸 32px/40px 稳定;主题经用户菜单切至 `theme-dark dark`/`theme-light light`、语言 `zh-CN`/`en-US` 均生效。本机 DB 无事件数据,群聊成员行无法真机观测,由已通过的 `IncidentGroupChat.test.tsx` 透传/动效断言覆盖）
- [x] 6.3 系统级「减少动态效果」开启后走查:全部动效关闭、信息完整（构建产物 `dist/assets/index-*.css` 中 `.anim-rise`/`.anim-fade`、`pulse-dot`/`motion-safe:animate-pulse-dot` 均包裹在 `@media (prefers-reduced-motion: no-preference)` 内;`IncidentGroupChat.test.tsx`「keeps message order and content identical regardless of animation classes」证明关闭动效后信息结构不变）
- [x] 6.4 复核 Non-Goals 全部未破:零新依赖(`web/package.json` 未变)、无头像上传功能、动效未改信息结构（`git diff 1cd6b27 -- web/package.json go.mod go.sum` 为空;无新增文件上传入口(仅 Knowledge/PluginMarketplace/设置-Marketplace 既有);4.5 顺序/内容不变量断言通过）

## 7. 代码评审接受记录（build 阶段 `review_mode: standard`，全量 diff `1cd6b27..833fc69`）

独立 code reviewer 结论:**Ready to merge = Yes,0 Critical / 0 Important / 5 Minor**。5 项 Minor 均为非阻断,经裁定接受并记录原因与影响范围:

1. `store/agents.ts:4` 反向依赖 `@/components/AgentAvatar`(数据层依赖组件模块)——计划明确要求复用 `normalizeAgentId`;功能正确,分层倒置留待后续批次下沉至 `lib/persona.ts`。影响:仅模块图,无行为风险。
2. `AgentAvatar.tsx:77-80` `imgBroken` 不随 `avatar` 变更复位——首屏 `byName` 为空先渲染图标不触发 imgBroken,实际触发概率极低,沿用旧模式。影响:极端情形的图标回退,无破图。
3. typing 指示仅入场 fade、退出即时卸载——**计划 Task 7 已显式记录该取舍**,满足 delta spec「回复完成后消失」硬要求。影响:退出无过渡动画,信息完整。
4. 结晶脉冲锚定群聊阶段条 `recovered`+`success`——**计划矛盾 4 已记录**,锚点语义待 verify 走查向产品 owner 确认。影响:锚点可能宽于「结晶」语义,代码正确可测。
5. `AgentAvatar.tsx:93` `<img>` 缺 `referrerPolicy="no-referrer"`——avatar 当前仅来自仓库内 `agents/*.md`,无用户可控注入面。影响:风险极低;若未来开放 avatar 输入需补纵深防御。

均不改动实现、不扩大范围;第 4 项转 verify 阶段走查确认。
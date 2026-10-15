## Context

OpsKeeper 的 Agent 目前是「角色色 + lucide 图标」的抽象形象(`AgentAvatar`,design-system-tokens spec 已预留 `avatar` 字段——存在 avatar URL/emoji 时优先渲染,否则回退角色图标)。内置 11 个 persona 来自 `agents/*.md` frontmatter。Agent 形象出现在四处:侧栏会话列表(`SessionList.tsx:150`)、聊天头行(`MessageBubble.tsx:123`)、档案墙(`Agents.tsx:319`)、群聊成员行(`IncidentGroupChat.tsx:139,144`),另有 `Home.tsx:437`。

动效方面,前端已有可复用语汇:`.anim-rise`/`.anim-fade`(消息进入)、`pulse-dot`(typing/状态呼吸点)、`Approvals.tsx:353` StatusChip 的脉冲徽标。无动画库。

本 change 是 Phase 4 功能形态深化的 D2 方向:对标 teamily 的 First-Class Agents 与 messenger 气泡感,让 Agent 团队有生命力。

## Goals / Non-Goals

**Goals:**
- persona 有脸:`agents/*.md` frontmatter 新增 `avatar:` 槽(emoji 或图片 URL),经 Agent 列表 API(`AgentSummary`)透出到前端
- 四处展示位 + Home 渲染同一头像,来源一致
- avatar 加载失败无破图,回退角色图标
- 群聊动效:消息进入、typing、阶段推进、结晶脉冲,全部 motion-safe

**Non-Goals:**
- 不引入动画库(仅 Tailwind keyframes 与既有 CSS 动画)
- 不做头像上传/裁剪编辑器(Q5 默认不做)
- 动效不改信息结构:不改排序、不隐藏内容
- 不加应用内动效开关,遵循系统「减少动态效果」
- 不改 persona 文案 i18n 回退

## Decisions

- **D1 avatar 数据来源——双轨,后端链路优先**:首选 `agents/*.md` frontmatter 增加 `avatar:` 字段、后端读入并在 `AgentSummary` 透出(完整链路);后端未就绪时由前端维护 persona→avatar 本地映射降级,映射仅覆盖内置 11 个 persona、不支持自定义。降级映射与后端字段同构,后端就绪后删除映射即可,不产生第二套数据模型。(Q1 待设计阶段定档期)
- **D2 头像形态**:emoji 以文本直接渲染进 Avatar 容器;图片 URL 用 `<img>` + `onError` 回退角色图标。不引入头像裁剪/上传。
- **D3 动效实现**:全部基于既有 CSS 动画类与 Tailwind keyframes,`motion-safe:` 前缀或 `prefers-reduced-motion` 媒体查询兜底;复用 `.anim-rise`/`.anim-fade`/`pulse-dot` 语汇,新增 keyframes 仅限缺口(如结晶脉冲)。
- **D4 动效点位(克制)**:①群聊消息进入 fade+上移 ②Agent 回复中 typing 呼吸点(结束后消失)③事件群聊阶段推进过渡 ④结晶徽标脉冲。不在普通单聊、控制台表格等高密度场景加动效。

## Risks / Trade-offs

- [后端改动档期不确定(Q1)] → 降级映射兜底,前端可先行;完整链路后端就绪后切换,映射删除即可
- [外链头像不可达/防盗链] → `onError` 回退角色图标;内置 persona 的 avatar 优先用 emoji 或本应用可访问的 URL
- [动效过多造成干扰] → 点位克制(仅群聊四类)、全部 motion-safe、复用既有语汇不新增风格
- [四处展示位改漏] → 以 `AgentAvatar` 单点消费 avatar,调用方只传数据,避免四处各写回退逻辑

## Migration Plan

纯前端 + persona 配置增量;降级映射为前端常量,回滚即移除。无数据迁移、无破坏性 API 变更(`AgentSummary` 新增可选字段向后兼容)。

## Open Questions

- Q1:后端(frontmatter 读入 + AgentSummary 透出)改动范围与档期?决定走完整链路还是前端降级先行
- Q5:头像上传/裁剪是否单独立项?默认不做
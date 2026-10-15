## Why

Agent 目前只有「角色色 + 图标」的抽象形象,没有脸;事件群聊等群聊场景缺乏消息进入、typing、阶段推进等动效,Agent 团队缺乏生命力。对标 teamily 的 First-Class Agents(Agent 是有形象的一等公民)与 messenger 气泡感,让运维 Agent 团队「有人味」是 Phase 4 功能形态深化(D2)的方向。

## What Changes

- `agents/*.md` frontmatter 新增 `avatar:` 槽(emoji 或图片 URL),后端读入并在 Agent 列表 API(`web/src/api/agents.ts` 的 `AgentSummary`)透出 `avatar` 字段
- 前端 `AgentAvatar` 组件消费 avatar(该组件已有 `avatar?: string` prop,零新前端逻辑);四处展示位一致性:侧栏(`SessionList.tsx:150`)、聊天头行(`MessageBubble.tsx:123`)、档案墙(`Agents.tsx:319`)、群聊成员行(`IncidentGroupChat.tsx:139,144`)及 `Home.tsx:437`
- 降级路径:后端未就绪时由前端维护 persona→avatar 本地映射(仅内置 11 个 persona,不支持自定义)
- avatar 图片加载失败回退角色图标,无破图
- 群聊动效(全部 motion-safe,遵循系统「减少动态效果」):消息进入 fade+上移(复用 `.anim-rise`/`.anim-fade`)、typing 呼吸点(复用 pulse-dot)、阶段推进过渡、结晶徽标脉冲(与 `Approvals.tsx:353` StatusChip 语汇一致)

## Capabilities

### New Capabilities

(无)

### Modified Capabilities

- `agents-gallery`:Agent 形象需求——persona 支持 avatar 字段,四处展示位渲染一致头像,失败回退角色图标
- `design-system-tokens`:群聊动效需求——motion-safe 的消息进入/typing/阶段推进/结晶脉冲动效语汇,不引动画库,不改信息结构

## Impact

- `agents/*.md`(内置 persona frontmatter)、Agent 读入与列表 API(后端,若走完整链路;降级路径则仅前端)
- `web/src/components/AgentAvatar.tsx`、`SessionList.tsx`、`MessageBubble.tsx`、`Agents.tsx`、`IncidentGroupChat.tsx`、`Home.tsx`
- 群聊动效涉及 `IncidentGroupChat.tsx` 与既有 CSS 动画类
- 不引入新依赖(仅 Tailwind keyframes 与既有 CSS 动画)

## Open Questions(设计阶段需澄清)

- Q1:`agents/*.md` frontmatter 新增 avatar 槽、`AgentSummary` 透出 avatar 的后端改动范围与档期?决定 D2 走完整链路还是前端降级映射
- Q5:是否单独立项「头像上传/裁剪」?默认不做
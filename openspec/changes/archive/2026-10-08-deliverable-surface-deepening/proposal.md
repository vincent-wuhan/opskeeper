## Why

Agent 产出的托管页、报表、复盘文档目前只在对话流里以一张「图标 + 标题 + 打开」的卡出现,用户必须点击跳走才能知道「这到底是什么」——而跳转就离开了对话,上下文随即断裂。Phase 1–3 已把整体形态与视觉语言改造到位,Phase 4 的第一件事就是让交付物**可就地预览**:缩略让产物一眼可辨,预览让判断不必离开对话。

## What Changes

- **交付物卡增加缩略窗格**:托管页复用既有 `PageThumb`(拉 HTML + sandbox iframe `srcdoc` + 缩放)渲染真实缩略;报表因是结构化 ContentJSON 而非可渲染页面,走**类型化占位缩略**(图标 + 产物类型 + 生成时间),不伪造截图。
- **点击行为由跳转改为就地预览**:点卡片不再跳走新窗口,而是在对话内展开预览区。托管页复用 sandbox iframe 预览,报表复用既有 `ReportContentView`。
- **抽取共享只读渲染器**:托管页预览与报表预览的渲染逻辑从各自页面抽出为共享只读组件,聊天内预览与独立页面共用,避免出现第二套渲染逻辑。
- **预览三态回退**:`loading / ready / failed`;失败时降级为类型占位缩略并保留「在新窗口打开」出口,不可识别的链接类型保持原卡与普通链接行为。
- **摘要取自产物元数据**(标题/类型/生成时间),缺失即留空,不编造。
- 识别面为**白名单制**:仅明确列举的路由升级为交付物卡,其余一律回退普通 `<a>`,延续既有 spec 的回退原则。

## Capabilities

### New Capabilities

无。本 change 不引入新能力,只深化既有 `chat-conversation` 的交付物卡需求。

### Modified Capabilities

- `chat-conversation`:现有 requirement「DeliverableCard 交付物卡」的行为约定发生变更——当前规定「点击在新窗口打开产物」,本 change 改为「点击在对话内就地展开预览」,并新增缩略窗格与三态回退约定。既有「无法识别的链接 MUST 回退为普通链接」原则不变且继续强化为白名单制。

## Impact

- **前端(主要)**:`web/src/components/DeliverableCard.tsx`(识别面、缩略、预览入口);`web/src/components/MessageBubble.tsx`(`matchDeliverable` 换卡点 `:142-143`)。
- **复用而非重写**:`web/src/pages/Pages.tsx` 的 `PageThumb`(`:44-81`)与页预览弹窗模式(`:359-389`);`web/src/components/ReportContent.tsx` 的 `ReportContentView`(`:203`);既有 `Modal` 原语。
- **后端**:零改动。不新增 API,不引入服务端截图服务。
- **依赖**:零新增 npm 依赖。
- **安全边界**:iframe 强制 `sandbox=""` 且仅限本应用同源托管页,不加载第三方域。
- **性能**:缩略懒挂载,单条消息缩略数量设上限,避免多条长消息同时渲染 iframe 造成滚动卡顿。
- **待确认**:是否存在稳定的 postmortem / 工作流产物前端路由可供识别面扩展(见 design 阶段的未决问题);在确认前白名单只保留 `/pages`、`/reports` 两条,不凭空扩充。
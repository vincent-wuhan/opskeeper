## Why

用户在对话中看不到 Agent 依据了什么——@提及了哪些运维对象、引用了哪些知识库文档。这些上下文已存在于消息流中,却没有可解释的呈现面。「可解释性」是 OpsKeeper「防幻觉」可信主张的产品化落点,也是 Phase 4 功能形态深化(D3)的方向。

## What Changes

- 会话侧新增上下文面板:宽屏为对话页右列,窄屏为可折叠侧板;默认折叠,记忆展开态
- **@提及对象**:解析消息中的结构化 mention token `@{type}:{id}({label})`(`ChatInput.tsx:229` 写入,`:163` recomputeMentionContext),按 device/incident/rule/file 去重列出,点击跳详情
- **知识引用**:从消息内联 `tool_calls[]` 按工具类别映射识别知识检索类工具(如 query_knowledge)的调用,列出该次调用的工具名 / 状态 / 耗时(工具级粒度;文档级明细见 Design Doc §8 Implementation Divergence)
- 核心推导为纯函数 `deriveSessionContext(messages) → { mentions[], knowledgeRefs[] }`,与 UI 解耦、可单测
- (设计阶段 DF1:原设想的「结晶命中」节移出本期——结晶为 incident 闭环事后 hook,不出现在聊天消息流,无法零接口反推;结晶浏览由 Discover/自愈规则页覆盖)

## Capabilities

### New Capabilities

(无)

### Modified Capabilities

- `chat-conversation`:新增会话上下文面板需求——从已渲染消息流反推本会话的提及对象/知识引用并可视化,零新增数据接口

## Impact

- 新增 `web/src/components/`(上下文面板组件)与 `deriveSessionContext` 纯函数模块
- `web/src/pages/` 对话页布局(右列/折叠侧板)
- 只读消费既有消息流数据,不改 `ChatThread`/`MessageBubble`/`ChatInput` 渲染,零后端改动

## Open Questions(设计阶段需澄清)

(无——批次清单中该项 open_questions 为空)
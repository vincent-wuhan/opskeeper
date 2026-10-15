## ADDED Requirements

### Requirement: 会话上下文面板
对话页 SHALL 提供会话上下文面板,分节呈现本会话的 @提及运维对象与知识检索调用两节。面板数据 SHALL 全部从已渲染消息流反推(mention token 解析 + 消息内联 tool_calls 按工具类别映射),系统 MUST NOT 为呈现面板引入新增数据接口。面板 SHALL 为只读,不可在此编辑或删除上下文。宽屏 SHALL 呈现为对话页右列,窄屏 SHALL 呈现为可折叠侧板;默认折叠,展开态 MUST 被记忆。

#### Scenario: 提及对象列出与跳转
- **WHEN** 会话中 @提及某设备
- **THEN** 面板「@提及对象」分节列出该设备(按 type+id 去重),点击跳转设备详情页

#### Scenario: 知识引用列出
- **WHEN** Agent 调用知识检索类工具
- **THEN** 面板「知识引用」分节列出该次知识检索调用(工具名 / 状态 / 耗时)

#### Scenario: 无结构化上下文时引导空态
- **WHEN** 会话无任何可反推的结构化上下文
- **THEN** 面板显示引导空态,MUST NOT 猜测或编造条目

#### Scenario: 面板不误报
- **WHEN** 消息为纯文本且无工具调用,或 mention token 的类型不在 device/incident/rule/file 之内
- **THEN** 面板不产生任何上下文条目

#### Scenario: 上下文可溯源
- **WHEN** 用户查看某条上下文条目
- **THEN** 可定位其来源消息(哪条消息提及或调用产生)

#### Scenario: 只读边界
- **WHEN** 用户在面板中查看上下文
- **THEN** 面板不提供编辑或删除上下文的操作入口

### Requirement: 上下文推导纯函数
会话上下文的推导 SHALL 由纯函数 `deriveSessionContext(messages) → { mentions[], knowledgeRefs[] }` 完成,与 UI 组件解耦。工具识别 MUST 按工具类别映射(知识检索类),MUST NOT 逐个工具名硬编码白名单。

#### Scenario: 推导与 UI 解耦可单测
- **WHEN** 给定消息数组调用推导函数
- **THEN** 返回确定的两类上下文结构,不依赖任何 UI 状态或网络请求

#### Scenario: 新工具按类别纳入
- **WHEN** 新增工具属于既有类别(知识检索类)
- **THEN** 经类别映射自动被面板识别,无需逐个工具名扩展白名单
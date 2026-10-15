# chat-conversation Specification

## Purpose
TBD - created by archiving change opskeeper-teamily-ui. Update Purpose after archive.
## Requirements
### Requirement: Agent 消息气泡化
对话流 SHALL 呈现双侧气泡:用户消息右对齐、accent 浅底、rounded-2xl(右下 rounded-md);Agent 消息左对齐、card 底色、rounded-2xl(左下 rounded-md),气泡上方带 AgentAvatar + persona 名头行。流式输出、自动滚动、消息操作 MUST 保持现有行为。

#### Scenario: 双侧气泡形态
- **WHEN** 对话流渲染一轮用户提问与 Agent 回复
- **THEN** 用户气泡右对齐浅紫底,Agent 消息左对齐卡片底并带头像行,两者均为大圆角气泡且角落半径差异化

#### Scenario: 流式行为不回退
- **WHEN** Agent 消息以流式方式逐步输出
- **THEN** 气泡容器随内容增长,滚动跟随与停止按钮行为与改造前一致

### Requirement: 工具调用卡嵌套
Agent 气泡内的工具调用 SHALL 渲染为嵌套圆角卡:单行摘要(工具名+耗时/状态)默认折叠,点击展开查看输入与结果;失败态 MUST 有明确的视觉标识。

#### Scenario: 工具卡折叠与展开
- **WHEN** Agent 消息包含工具调用
- **THEN** 默认仅显示单行摘要卡,点击后展开输入参数与结果详情

### Requirement: DeliverableCard 交付物卡
对话流 SHALL 识别 serve_page / reports / pages 产出的链接,以统一交付物卡渲染:图标 + 标题 + 摘要 + 缩略窗格 + 「预览」动作,在消息流内收口呈现。识别面 SHALL 为白名单制,仅显式列举的产物路由升级为交付物卡:白名单为 `/pages/<id>`(serve_page 托管页,id 为十六进制串)与 `/reports/<id>`(报表,id 为含连字符的十六进制串)两条;id 匹配 MUST 使用真实产物 id 形态(十六进制/含连字符,长度 16–64),MUST NOT 匹配任意数字或任意路径段。无法识别的链接 MUST 回退为普通链接渲染,不影响原跳转。

交付物卡 SHALL 呈现缩略窗格:托管页产物 MUST 渲染真实缩略;非托管页产物 MUST 渲染类型化占位缩略。缩略内容 MUST 来自产物真实元数据;缺失的元数据 MUST 留空,系统 MUST NOT 伪造截图或编造内容。

点击交付物卡 SHALL 在对话内就地展开预览区,而非跳转离开对话。系统 MUST 在交付物卡上保留「新窗口打开」动作,使用户始终可离开对话访问产物。预览渲染逻辑 MUST 与产物独立页面共用同一实现,同一产物在两处呈现 MUST 一致。

#### Scenario: 交付物链接渲染为卡片
- **WHEN** Agent 消息包含 serve_page 托管页或报表产物链接
- **THEN** 该链接渲染为交付物卡(图标/标题/摘要/缩略窗格/预览),且卡片上提供「新窗口打开」动作

#### Scenario: 普通链接回退
- **WHEN** 消息包含非白名单内的外部链接
- **THEN** 按普通链接渲染,点击跳转目标与改造前一致

#### Scenario: 点击卡片就地预览
- **WHEN** 用户点击交付物卡
- **THEN** 预览在对话流内该卡片处展开,不导航到新窗口,消息流其余内容保持可见

#### Scenario: 托管页缩略为真实渲染
- **WHEN** 交付物卡对应 serve_page 托管页
- **THEN** 缩略窗格以全属性收紧的 sandbox 空值渲染该页 HTML,并按容器宽度等比缩放

#### Scenario: 报表缩略为类型占位
- **WHEN** 交付物卡对应报表产物
- **THEN** 缩略窗格渲染产物类型与生成时间的类型化占位,MUST NOT 渲染伪造的页面截图

#### Scenario: 缩略元数据缺失时不编造
- **WHEN** 产物缺少标题或生成时间等元数据
- **THEN** 缩略占位对应位置留空,MUST NOT 以推测内容填充

### Requirement: 交付物预览三态与降级
交付物预览 SHALL 具备 `loading`、`ready`、`failed` 三种可区分状态。系统在缩略与预览加载期间 MUST 呈现加载态而非空白;加载失败时 MUST 降级为类型化占位缩略,并 MUST 保留「新窗口打开」出口。降级路径 MUST NOT 静默移除产物访问方式。

#### Scenario: 加载中呈现加载态
- **WHEN** 交付物缩略或预览正在加载
- **THEN** 呈现可辨识的加载态,缩略窗格保持占位尺寸,不发生布局抖动

#### Scenario: 托管页加载失败降级
- **WHEN** 托管页 HTML 加载失败
- **THEN** 缩略降级为类型化占位,且「新窗口打开」动作仍可打开该产物

#### Scenario: 预览区域不被移除
- **WHEN** 预览进入 `failed` 状态
- **THEN** 用户仍可从同一张交付物卡在新窗口打开产物,不出现无出口的死路

### Requirement: 交付物渲染边界安全
系统 MUST 仅对本应用同源的托管页产物使用 iframe 渲染缩略与预览。承载托管页 HTML 的 iframe MUST 使用全属性收紧的空值 sandbox,不得启用脚本、表单或弹窗能力。系统 MUST NOT 对第三方外部页面使用 iframe 预览。

#### Scenario: 托管页 iframe 沙箱收紧
- **WHEN** 系统渲染托管页的缩略或预览
- **THEN** 该 iframe 的 sandbox 为空值,不授予脚本、表单与弹窗权限

#### Scenario: 第三方链接不使用 iframe
- **WHEN** 消息包含第三方外部链接
- **THEN** 按普通链接渲染,MUST NOT 放入任何 iframe 中加载

### Requirement: 交付物缩略懒挂载
系统 SHALL 延迟挂载交付物缩略,仅在交付物卡进入可视区域后加载其内容。单条消息内的交付物缩略数量 MUST 设上限,超出的交付物以不渲染缩略的紧凑卡呈现而非全部加载。系统 MUST NOT 因缩略加载导致消息流滚动卡顿。

#### Scenario: 视口外缩略不加载
- **WHEN** 交付物卡尚未进入可视区域
- **THEN** 其缩略内容尚未加载,卡片仍以占位尺寸呈现

#### Scenario: 超出上限的交付物收敛渲染
- **WHEN** 单条消息包含的交付物数量超过缩略上限
- **THEN** 超出部分以不渲染缩略的紧凑卡呈现,交付物本身仍可访问

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


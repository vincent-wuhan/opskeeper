## ADDED Requirements

### Requirement: 群聊动效语汇
事件群聊 SHALL 提供四类动效:消息进入(fade+上移)、Agent 回复中的 typing 呼吸点(结束后消失)、阶段进度指示器的阶段推进过渡、结晶徽标脉冲。全部动效 MUST 遵循系统「减少动态效果」设置(motion-safe),动效 MUST NOT 改变信息结构——不改排序、不隐藏内容。动效实现 MUST 复用既有动画语汇(`.anim-rise`/`.anim-fade`/`pulse-dot` 等),不引入动画库;普通单聊与控制台高密度场景 MUST NOT 加入新动效。

#### Scenario: 消息进入动效
- **WHEN** 群聊中出现新消息
- **THEN** 该消息以 fade+上移动效进入,内容完整可读

#### Scenario: typing 指示出现与消失
- **WHEN** Agent 生成回复中
- **THEN** 呈现 typing 呼吸点指示,回复完成后指示消失

#### Scenario: 减少动态效果时无动画
- **WHEN** 系统开启「减少动态效果」
- **THEN** 消息进入/typing/阶段推进/结晶脉冲无动画,信息完整可读,无内容被隐藏

#### Scenario: 动效不改变信息结构
- **WHEN** 任一动效播放
- **THEN** 消息排序、内容可见性与无动效状态完全一致
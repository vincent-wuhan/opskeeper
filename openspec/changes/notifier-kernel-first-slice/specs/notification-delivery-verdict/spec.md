## ADDED Requirements

### Requirement: 渠道投递结果由渠道自己的回包判定

内置聊天渠道（钉钉 / 飞书 / 企业微信）的投递结果 MUST 由**渠道自己的回包**判定，MUST NOT 只看 HTTP 状态码。这三种渠道在签名错、限流、机器人被收回时都回 **HTTP 200**，只是 body 里的错误码字段非零；只看状态码会把这些故障全记成"投递成功"，运维看到的现象是"消息再也不来了，但系统说一切正常"。

`webhookSender` MUST 提供一个**可空**的响应体判定钩子。可空是硬要求：绝大多数渠道（generic webhook / Slack / Telegram）没有自己的错误码协议，给它们塞一个钩子等于凭空发明一套协议。

- **WHEN** 一个有渠道自己错误码协议的 sender 收到 HTTP 2xx 且 body 里错误码字段非零
- **THEN** `Send` 返回错误，错误文本带上渠道给的错误码与原因
- **AND** 该次投递落成可重试的失败

#### Scenario: 钉钉非零 errcode 判失败

- **WHEN** 钉钉 sender 收到 HTTP 200 且 body 为 `{"errcode":310000,"errmsg":"sign not match"}`
- **THEN** `Send` 返回错误，错误文本含 `310000` 与 `sign not match`

#### Scenario: 飞书两种错误码形状都判失败

- **WHEN** 飞书 sender 收到 HTTP 200 且 body 为 `{"code":<非零>,"msg":"<原因>"}`
- **THEN** `Send` 返回错误，错误文本含错误码与原因
- **WHEN** 同一 sender 收到旧部署形状 `{"StatusCode":<非零>,"StatusMessage":"<原因>"}`
- **THEN** 同样判失败

#### Scenario: 企业微信非零 errcode 判失败

- **WHEN** 企业微信 sender 收到 HTTP 200 且 body 为 `{"errcode":<非零>,"errmsg":"<原因>"}`
- **THEN** `Send` 返回错误，错误文本含错误码与原因

### Requirement: 只有渠道协议内的证据才算拒绝

判定规则 MUST 只承认"渠道协议内的证据"。空 body、只有空白、不是 JSON、以及是 JSON 但不带任何已知错误码字段，MUST 一律判通过——不是渠道协议的东西不是关于渠道的证据。错误码字段值**为零** MUST 判通过，这是渠道确认接收的形状。读取响应体本身失败（例如连接中途断掉）MUST 判失败而不是通过：接收方的裁决恰恰就是没能建立的那件事。

错误码值 MUST 同时接受 JSON number 与**带引号的数字**——渠道不总是回 JSON number。读取的字节数 MUST 有上界（64 KiB），因为一个配错的 endpoint 可能指向一个巨大的响应。

- **WHEN** 一个接了判定钩子的渠道回了一个不携带任何已知错误码字段的响应
- **THEN** 判定通过

- **WHEN** 响应体为空、只有空白，或不是 JSON
- **THEN** 判定通过

- **WHEN** 响应体是 JSON 且错误码字段的值为零
- **THEN** 判定通过

- **WHEN** 响应体是 JSON 且错误码字段是带引号的数字且非零
- **THEN** 判定失败，错误文本按数字值报出

#### Scenario: 空 body 判通过

- **WHEN** 渠道回 HTTP 200 且响应体为空（或只有空白）
- **THEN** 判定通过

#### Scenario: 非 JSON body 判通过

- **WHEN** 渠道回 HTTP 200 且响应体不是 JSON
- **THEN** 判定通过

#### Scenario: JSON 里没有已知错误码字段判通过

- **WHEN** 渠道回 HTTP 200 且响应体是 JSON 但不含 `errcode` / `code` / `StatusCode`
- **THEN** 判定通过

#### Scenario: 零值错误码判通过

- **WHEN** 渠道回 HTTP 200 且响应体里已知错误码字段的值为零
- **THEN** 判定通过

#### Scenario: 带引号的数字错误码

- **WHEN** 渠道回 HTTP 200 且响应体里错误码字段是字符串形式的非零数字
- **THEN** 判定失败，错误文本带出该数字

#### Scenario: 读不出响应体判失败

- **WHEN** 渠道回了 HTTP 2xx 但读取响应体失败
- **THEN** 判定失败，MUST NOT 判通过

### Requirement: 没接钩子的渠道行为不变

没有接判定钩子的 sender（generic webhook / Slack / Telegram）MUST 保持逐位不变：它们没有自己的错误码协议，判定钩子对它们保持未接线。MUST NOT 替它们发明一套错误码协议。

`NewFeishuSender` / `NewDingTalkSender` / `NewWeComSender` 三个构造函数的**公开签名 MUST 不变**，调用方一个都不用改。

- **WHEN** Slack 或 Telegram sender 收到一个 body 里带非零 `errcode` 的 HTTP 200
- **THEN** 该次投递仍判成功，行为与引入钩子之前逐位相同

#### Scenario: 未接线的渠道忽略响应体

- **WHEN** 一个未接判定钩子的 sender 收到 HTTP 200 且 body 里含非零错误码字段
- **THEN** `Send` 不返回错误

#### Scenario: 三个渠道构造函数的公开签名不变

- **WHEN** 既有代码调用 `NewFeishuSender` / `NewDingTalkSender` / `NewWeComSender`
- **THEN** 调用方无需改动

### Requirement: 判定钩子只读响应，不改请求

判定钩子 MUST 只读响应。请求侧的 body、query、签名 MUST 与引入钩子之前逐字相同，否则**已经按现行算法验签的接收方会在升级瞬间全部失败**。既有的签名回归断言 MUST 原样保留，本能力只新增子用例。

- **WHEN** 接上判定钩子之后发送一条飞书或钉钉通知
- **THEN** 请求的 body、query 与签名与改动前逐字相同

#### Scenario: 请求形状不变

- **WHEN** 对接了判定钩子的 sender 发一次请求
- **THEN** 捕获到的请求方法、URL、query、body 与签名头与未接钩子时一致

#### Scenario: 既有签名断言原样通过

- **WHEN** 跑既有的签名回归用例（飞书签名在 body 顶层、钉钉签名在 URL query）
- **THEN** 两者仍然通过，且不互换位置

### Requirement: 渠道拒收走既有的失败与重试链路

渠道判失败 MUST 复用既有的告警派发链路：`Send` 返回错误 ⇒ 投递记录落成 `failed` ⇒ 退避重投 ⇒ 渠道恢复后重投成功、attempt 递增、写成功事件。告警派发主流程（`MaybeNotify` / `FinishDelivery` / `RetryWorker`）MUST NOT 因为这个能力而改动——这条链路今天已经是"发送失败 ⇒ 落 failed ⇒ 被重试"，缺的只是发送器肯不肯报失败。

- **WHEN** 钉钉在一次真实投递里回 HTTP 200 + 非零 errcode，随后渠道恢复正常
- **THEN** 第一次投递落 `failed` 并写失败事件，不写成功事件
- **AND** 重试后第二次投递落 `success`、attempt 递增为 2、写成功事件，事故被标记为已通知

#### Scenario: 非零 errcode 落 failed 并被重试

- **WHEN** 钉钉渠道首次投递回 HTTP 200 且 errcode 非零
- **THEN** 投递记录状态为 `failed`，错误文本含渠道错误码
- **AND** 写下 `notification_failed` 事件，且没有 `notification_sent` 事件

#### Scenario: 渠道恢复后重投成功

- **WHEN** 钉钉渠道在重试时回 HTTP 200 且 errcode 为零
- **THEN** 投递记录状态为 `success`，attempt 递增
- **AND** 写下 `notification_sent` 事件，事故被标记为已通知

### Requirement: 渠道测试接口如实报告渠道的裁决

运维点「测试通道」时，MUST 看到渠道自己的裁决，而不是被 HTTP 状态码掩盖后的假成功。渠道在 200 里拒收时，测试接口 MUST 报 `accepted=false` 并把渠道给的错误码与原因带回来。

- **WHEN** 一个渠道回 HTTP 200 但 body 里错误码非零，运维点测试通道
- **THEN** 接口报该次发送未被接受，消息里含渠道的错误码与原因文本

#### Scenario: 渠道在 200 里拒收

- **WHEN** 用一个回 HTTP 200 + `{"errcode":310000,"errmsg":"sign not match"}` 的假渠道测通道
- **THEN** 测试接口报 `accepted=false`，消息含 `310000` 与 `sign not match`

#### Scenario: 渠道恢复正常后又能测通

- **WHEN** 同一个通道在假渠道恢复正常后再次测试
- **THEN** 测试接口报 `accepted=true`——判定钩子不是把成功也变成失败
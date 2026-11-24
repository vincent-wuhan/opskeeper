## ADDED Requirements

### Requirement: 未配置 EncryptKey 的飞书 app 拒绝处理事件

对 `encrypt_key` 为空的 ImApp，飞书事件端点 MUST 拒绝处理其事件（401），MUST NOT 进入解密或 agent 分发；`verify_token` MUST NOT 被视为事件路径的校验依据。

#### Scenario: 空 key 伪造事件被拒

- **GIVEN** 某 feishu ImApp 行 `encrypt_key` 为空（历史行 / 直改 DB / 旁路写入）
- **WHEN** 任意客户端向飞书事件端点 POST 伪造的 `im.message.receive_v1` 事件（携带该 app_id）
- **THEN** 返回 401
- **AND** bridge.HandleInbound 不被调用（无 agent 运行、无出站回复）
- **AND** 记录 Warn 级日志标明 app_id 与"未配置 encrypt_key"

#### Scenario: 有 key 且签名正确则放行

- **GIVEN** 某 feishu ImApp 配置了 `encrypt_key`
- **WHEN** 携带正确 `X-Lark-Signature`（sha256(timestamp+nonce+key+body)）的事件到达
- **THEN** 验签通过，事件进入既有解密与分发流程

#### Scenario: 有 key 但签名错误被拒

- **GIVEN** 某 feishu ImApp 配置了 `encrypt_key`
- **WHEN** 签名与 (timestamp, nonce, key, body) 不匹配
- **THEN** 返回 401，事件不被处理

#### Scenario: url_verification 握手不受影响

- **GIVEN** 管理员正在飞书开放平台配置回调 URL
- **WHEN** 平台发送 `{type:"url_verification"}` 握手（无签名头）
- **THEN** challenge 正常回显（该路径在 app 解析之前，不触发 key 闸）

### Requirement: 入口校验与运行时拒绝各自独立生效

应用配置校验（webhook 模式必填 encrypt_key）防止新增空 key 行；事件 handler 的硬拒绝防护存量与旁路写入行。两层 MUST 同时存在，任一层被绕过时另一层仍然生效。

#### Scenario: 绕过 API 直写 DB 的空 key 行

- **GIVEN** 运维绕过 Channels API 直接 UPDATE im_apps 将 encrypt_key 置空
- **WHEN** 事件到达该 app
- **THEN** handler 层拒绝（401），不依赖入口校验层

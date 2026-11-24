## ADDED Requirements

### Requirement: 探索环境是一次性的且仅回环可达

通道 MUST 用 `tests/e2e/testenv` 的 fakes 拉起一个一次性 manager 实例，默认运行 MUST NOT 依赖任何外部凭据；实例的监听地址 MUST 仅限回环。通道 MUST 为接入方铸造一枚限权测试令牌，该令牌 MUST NOT 能通过非 testenv 实例的鉴权。

#### Scenario: 零凭据起环境

- **WHEN** 在没有任何 secret 的干净检出上运行通道
- **THEN** manager 实例起来、`healthz` 返回 200、令牌与章程被输出

#### Scenario: 测试令牌打不到别处

- **WHEN** 用通道铸造的令牌向一个非本次 testenv 起的 manager 实例发 `tools/list`
- **THEN** 请求被拒，且该实例的审计链不新增任何行

### Requirement: 判决只来自确定性证据

通道的判决 MUST 由确定性代码给出，证据 MUST 仅取自 manager 侧审计链与调用轨迹。判决 MUST 至少覆盖下列不变量：每次 `tools/call` 在审计链有且仅有一行并带 `X-Opskeeper-Audit-ID`；`tools/list` 未列出的工具调用失败；class 为 write 或 destructive 的调用进入审批队列且不被通道自动放行；调用不越出令牌的租户；应答不含凭据形状的字符串。

#### Scenario: 调了看不见的工具

- **WHEN** 接入方对一枚未出现在其 `tools/list` 里的工具名发 `tools/call`
- **THEN** 应答为错误，判决记录一条不变量违反，manager 侧不产生该工具的副作用

#### Scenario: 写操作停在审批队列

- **WHEN** 接入方调用一个 write 类工具
- **THEN** 该调用以待审批状态落队列，通道返回的判决里它既不算成功也不算失败，且没有任何路径让它被执行

#### Scenario: 审计行缺失即红

- **WHEN** 轨迹里有一次 `tools/call`，而对应会话的审计行查不到
- **THEN** 判决为失败，并指出是哪一次调用

### Requirement: AI 的判断不作门禁

接入方在探索中记录的任何主观判断 MUST 与不变量判决分开存放；这些判断 MUST NOT 改变判决结果。

#### Scenario: 线索不改变结论

- **WHEN** 接入方报告「某处疑似缺陷」，而不变量全部成立
- **THEN** 判决仍为通过，该报告作为线索单独列出

### Requirement: 契约面不因探索而变化

通道 MUST NOT 新增、放宽或改变 `docs/mcp-surface.md` 承诺的任何行为。一次探索前后，同一身份的 `tools/list` 结果 MUST 一致。

#### Scenario: 探索前后工具清单相同

- **WHEN** 在探索开始前与结束后，用同一枚令牌各取一次 `tools/list`
- **THEN** 两个结果逐项相同

### Requirement: 探索受预算约束

每份章程 MUST 声明工具调用数与步数的上限。触及上限时通道 MUST 终止本次探索，并将其记为未完成，而不是通过。

#### Scenario: 跑飞即终止

- **WHEN** 接入方在一份章程下的调用数达到上限
- **THEN** 通道终止探索，令牌失效，报告中标为未完成

### Requirement: 失败轨迹可回放并固化为回归

通道 MUST 能把一条轨迹导出为仓内客户端可重放的用例。回放用例 MUST 在不接入任何 AI 的情况下复现首次探索触达 manager 的调用序列，并并入现有 e2e 套件受同一套闸门约束。

#### Scenario: 无 AI 复现同一条调用序列

- **WHEN** 用仓内客户端重放一条已导出的轨迹
- **THEN** manager 侧审计链新增的行与首次探索逐项对应（方法、工具名、租户）

# Design — notifier 内核首切片（S-1 + S-4）

本文件是实现依据。每一节写清楚"为什么是这个形状"，而不是"代码长什么样"——代码形状在源码里，理由不会。

## 1. 范围与来源

- 方案来源：`openspec/changes/plugin-architecture-recovery-plan/notifier-plugin-plan.md`（任务 4.5 的产物），本 change 落地其中 §13 的 **S-1** 与 **S-4** 两行。
- S-1 = `WithKindCeiling`（方案 §4.5 候选 A）+ 把方案 §11.1 的 manifest 准入对照表固化成测试。
- S-4 = 方案 §13 S-4 行里"顺带给内置 `webhookSender` 加响应体校验钩子（缺口 G-1 的存量修复）"那半句；候选包、渠道类型枚举、装配层 Activate 一律不在本 change。
- 两个切片放同一个 PR：S-1 是内核改动，S-4 是通知投递判定，两者共同服务于"notifier 接内核"这一个目标，且 S-4 的失败路径（投递落 failed → RetryWorker 重试）要在 S-1 的准入语义定下来之后才说得清。

## 2. S-1：per-kind safety ceiling

### 2.1 语义裁定：条目生效，无条目回落全局

**这一节是本 change 最重要的一处裁定。**

方案 §4.5 候选 A 的字面表述是"生效等级 = `min(MaxSafetyLevel, KindCeilings[k])`（只能收紧，不能放宽）"。同一份方案的 §4.6 草案却是：

```go
}.WithKindCeiling(kernel.KindNotifier, domain.SafetyL3) // ← 全局仍是 L2
```

这两句互相矛盾，且**矛盾的方向会直接决定六个 notifier manifest 的命运**：

- 按 `min` 算：`min(L2, L3) = L2`。§4.6 那行写入成了**空操作**，notifier 的生效 ceiling 仍是 L2，方案 §11.1 表里六个 L3 manifest 在 L2 ceiling 下**依然全部被拒**（`plugin is L3, target policy allows at most L2`）。
- 按"只能收紧不能放宽"算：全局 L2 时写 L3 属于**写宽了**，§4.6 那行写入会被收窄入口**直接拒掉**，方案照抄的代码根本编译不过。

所以"按 min 实现"是一个会让方案本身失效的选择。本 change 的裁定是：

> **条目生效，无条目回落全局。** 某个 Kind 有条目时，它的生效 ceiling 就是条目本身；某个 Kind 没有条目时，它的生效 ceiling 是全局 `MaxSafetyLevel`。

配套地，`min` 与"只能收紧"被拆成两个**方向不同的写入入口**，每个入口只接受自己那个方向的写入，写反方向 **panic** 而不是静默忽略。

为什么 panic 而不是忽略：静默忽略会让操作员的改动**看起来生效了，其实没有**——这正是这个缺口本身（消息再也不来了，但系统说一切正常）的同一种失败模式。一个"写了没反应"的策略和一个"写了但被忽略"的策略，对运维是同一件事。

### 2.2 三条不变式（测试的锚点）

| # | 不变式 | 对应测试 |
|---|---|---|
| **I1** | 条目生效：某个 Kind 有条目时，它的生效 ceiling 就是条目本身，**不与全局取 min** | `TestKindCeilingEntryTakesEffectOverTheGlobal` |
| **I2** | 无条目回落全局：某个 Kind 没有条目时，它的生效 ceiling 是全局 `MaxSafetyLevel` | `TestPolicyWithoutKindCeilingsFallsBackToTheGlobalCeiling` / `TestEmptyKindCeilingsMapBehavesLikeNil` |
| **I3** | 写入方向可测：收窄入口只收窄、放宽入口只放宽；写反方向 panic，且 panic 文本要指向正确方向 | `TestWithKindCeilingRefusesWidening` / `TestWithWiderKindCeilingRefusesNarrowing` |

### 2.3 Policy 的形状与两个查询口

```go
type Policy struct {
    Target         domain.DeploymentTarget
    MaxSafetyLevel domain.SafetyLevel
    MaxBlastRadius domain.BlastRadius
    GrantedScopes  domain.Scopes
    // KindCeilings overrides MaxSafetyLevel for one extension point.
    KindCeilings map[Kind]domain.SafetyLevel
}
```

- `EffectiveSafetyCeiling(k)`：条目（且条目非空值）优先，否则全局。条目携带零值时按**缺省**处理——零值 `SafetyLevel` 的排名是 L3（最宽），当成有效条目就是把最宽的 ceiling 记进了策略里。
- `WidestSafetyCeiling()`：全局与所有条目里最宽的一个。它存在的唯一理由是包级闸门：`Admit` 看到的是一份 manifest，而 manifest 只声明整包一个安全级别、**不声明它的贡献落在哪些 Kind**，所以包级闸门只能用最宽值，否则会拒掉宿主随后本来会接受的包。

`KindCeilings` 为空（nil 或空 map）⇒ 两个查询口都等于全局 ⇒ **行为与改动前逐位相同**。这是 S-1 的第一条验收标准，也是本 change 最重要的兼容性保证。

### 2.4 闸门落在哪里

- **包级**：`Host.Admit` 用 `WidestSafetyCeiling()`。过了这一关只说明"这个包有可能被接受"，不说明它一定会被接受——policy 带 per-kind 条目时，包级准入是必要不充分条件。
- **Kind 级**：`Host.Activate` 通过 scoped registrar 登记每个条目时判。registrar 同时知道**条目落在哪个 Kind** 和**这个包声明了什么级别**，而 manifest 只有整包一个级别，所以这里才是 Kind 闸门唯一能判准的地方。
- **拒绝走既有整包回滚**：`Register` 返回错误 ⇒ `Activate` 调 `h.reg.RemovePlugin(p.Name())` 把这个包写的条目全部撤掉 ⇒ 一个半激活的包不会在表里留下没人清理的条目。**不新造第二条回滚路径。**
- **全局回落在这里也生效**：某个 Kind 没有条目时回落全局（I2）。这一条不只是语义一致性——它是防洞的关键：全局 L2 + notifier 条目 L3 的策略下，一个 L3 包在包级准入过关后，它想往 `tool` / `adapter` / `datasource` 落条目时，Kind 级闸门会按全局 L2 把它拒掉。

### 2.5 宿主必须能执行自己的策略

`NewHost` 校验每一个条目：Kind 必须在封闭集合内，`SafetyLevel` 必须是 L0..L3。不认识的安全级别排名是 L3——**一个打错的条目会记下系统里最宽的 ceiling**，然后安静地放行操作员本来想拦住的包。一个执行不了自己策略的宿主不是保守的宿主，是坏的宿主，所以这里选 panic 而不是忽略。

策略可以绕过入口点直接用结构体字面量构造（`Policy{...}`），所以校验放在 `NewHost` 而不是只放在两个写入入口。

## 3. S-4：渠道自己说了算

### 3.1 钩子形状

`webhookSender` 增加一个可空字段 `checkResponse func(body []byte) error`，`newWebhookSender` 多一个参数。**可空是硬要求**：绝大多数渠道（generic webhook / Slack / Telegram）没有自己的错误码协议，给它们塞一个钩子等于凭空发明一套协议。

接线点只有三处：

| 构造函数 | 钩子 | 为什么 |
|---|---|---|
| `NewFeishuSender` | `channelAPIError` | 飞书回 `{"code":…,"msg":…}`，旧部署还回 `{"StatusCode":…,"StatusMessage":…}` |
| `NewDingTalkSender` | `channelAPIError` | 钉钉回 `{"errcode":…,"errmsg":…}` |
| `NewWeComSender` | `channelAPIError` | 企业微信回 `{"errcode":…,"errmsg":…}` |
| `NewGenericWebhookSender` / `NewSlackSender` / `NewTelegramSender` | 无 | 没有渠道自己的错误码协议；保持逐位不变 |

**构造函数的公开签名不变**：这三个构造函数的调用方一个都不用改。

### 3.2 判定规则

1. body 为空 / 只有空白 ⇒ **通过**。有的接收方就回一个空的 200，这不是关于渠道的证据。
2. body 不是 JSON ⇒ **通过**。不是渠道协议的东西不是关于渠道的证据。
3. body 是 JSON 但不带任何已知的错误码字段 ⇒ **通过**。
4. body 带错误码字段且值非零 ⇒ **失败**，错误文本带上渠道给的错误码与原因文本。
5. 错误码可以是 JSON number，也可以是**带引号的数字**——渠道不总是回 JSON number。
6. 读取的字节数有上界（64 KiB）。每个渠道的判定只需要几十字节；上界存在是因为一个配错的 endpoint 可能指向一个巨大的响应。

**失败优先于"读不出"**：读响应体本身失败（例如连接中途断掉）算失败，不算通过——接收方的裁决恰恰就是没能建立的那件事。

### 3.3 请求形状一个字不动

钩子只读响应。请求侧的 body、query、签名必须与改动前逐字相同，否则**已经按现行算法验签的接收方会在升级瞬间全部失败**。签名回归由既有 G4 e2e 断言钉住（Feishu 签名在 body 顶层、DingTalk 签名在 URL query，两者都不许互换），本 change 的 e2e 只**新增**子用例。

### 3.4 失败之后发生什么

`Send` 返回错误 ⇒ `MaybeNotify` 把 `notification_deliveries` 那一行落成 `failed` 并写 `notification_failed` 事件 ⇒ `RetryWorker` 按退避重投 ⇒ 渠道恢复后重投成功，行落 `success`、attempt 递增、写 `notification_sent` 事件、incident 标记已通知。

**告警派发主流程（`MaybeNotify` / `FinishDelivery` / `RetryWorker`）一个字不改**——这条链路今天已经是"发送失败 ⇒ 落 failed ⇒ 被重试"，缺的只是发送器肯不肯报失败。

## 4. 硬约束（C1–C8）

| # | 约束 | 本 change 的落实方式 | 怎么验 |
|---|---|---|---|
| **C1** | `core/floor/kernel/kind.go` 一字不改 | 不新增 Kind、不动封闭集合 | `git diff --name-only` 不含该文件 |
| **C2** | `core/base/pkg/notify/registry.go` 一字不改 | 渠道类型注册表不碰；钩子走 sender 构造，不走注册表 | 同上 |
| **C3** | `core/manager/biz/aiops/datasource/datasource.go`（含 `ManagerPolicy()`）一字不改 | 不在 notifier 侧派生策略；per-kind 能力先落在内核，策略派生留给后续切片 | 同上 + `TestManagerPolicyIsUnaffectedByPerKindCeilings` |
| **C4** | `core/manager/middleware/toolset/sources_plugin.go`（含 `adapterPolicy()`）一字不改 | 同上 | 同上 + `TestAdapterPolicyIsUnaffectedByPerKindCeilings` |
| **C5** | 告警派发主流程（`MaybeNotify` / `FinishDelivery` / `RetryWorker`）不改 | S-4 只改发送器的返回值 | `git diff --name-only` 不含 `core/manager/biz/alert/usecase.go` / `retry.go` |
| **C6** | `plugins/` 不动 | 本 change 不产出任何插件包 | `git diff --name-only` 不含 `plugins/` |
| **C7** | 不新增 Kind；manifest schema 不加字段 | 本 change 不产 manifest | — |
| **C8** | `KindCeilings` 为空 ⇒ 行为逐位不变 | 回落全局 + 既有准入文本一字不变 | `TestEmptyKindCeilingsKeepsAdmissionBitForBit` |

## 5. 测试矩阵

| 验收项 | 测试 | 位置 |
|---|---|---|
| S-1 三个构造点行为逐位不变 | `TestEmptyKindCeilingsKeepsAdmissionBitForBit` / `TestManagerPolicyIsUnaffectedByPerKindCeilings` / `TestAdapterPolicyIsUnaffectedByPerKindCeilings` / `TestEdgePolicyIsUnaffectedByPerKindCeilings` | `core/floor/kernel/policy_test.go`、三个既有测试文件 |
| 收窄入口拒"写宽了" | `TestWithKindCeilingRefusesWidening` | `core/floor/kernel/policy_test.go` |
| 放宽入口拒"写窄了" | `TestWithWiderKindCeilingRefusesNarrowing` | 同上 |
| 未知 Kind / 未知级别被拒 | `TestKindCeilingRejectsUnknownKindAndLevel` / `TestNewHostRefusesAnUnenforceableKindCeiling` | 同上 |
| §11.1 的 L2 拒 / L3 过对照 | `TestSectionElevenOneComparesL2RejectionAgainstL3Acceptance` | 同上 |
| per-kind 闸门：notifier 过、其他 Kind 按全局拒 | `TestPerKindCeilingLetsNotifierThroughAndRefusesOtherKinds` / `TestKindCeilingRefusalRollsBackTheWholePackage` | 同上 |
| S-4 三渠道非零错误码判失败 | `TestDingTalkSenderRejectsANonZeroErrcode` / `TestFeishuSenderRejectsANonZeroCode` / `TestWeComSenderRejectsANonZeroErrcode` | `core/base/pkg/notify/webhook_test.go` |
| 未接钩子的渠道行为不变 | `TestUnwiredChannelsIgnoreTheResponseBody` | 同上 |
| 请求形状不变 | `TestWiredHookDoesNotChangeTheRequestShape` + 既有 G4 e2e | 同上 / `tests/e2e/notify_signed_test.go` |
| 钉钉非零 errcode 落 `notification_deliveries.failed` 并被 RetryWorker 重试后恢复 | `TestDingTalkNonZeroErrcodeFailsTheDeliveryAndIsRetried` | `core/manager/biz/alert/channel_sender_test.go` |
| e2e：渠道在 200 里拒收 | `TestNotify_ChannelErrcodeRejected_G8` | `tests/e2e/notify_errcode_test.go` |

## 6. 偏差与待确认

1. **本 change 的脚手架在本地撰写。** 上游本应有 `agent/agent/5e722b292aa9`（commit `fc1d1309`）上的 `openspec/changes/notifier-kernel-first-slice/` 脚手架。本环境到 github.com:443 不通（`Failed to connect to github.com port 443`），该分支取不到，因此 proposal / design / tasks / 两份 delta spec 在本地按本 change 的实现实际写定。**若上游脚手架后来出现，以它为准，本文件需要逐条对齐。**
2. **与 `notifier-plugin-plan.md` §4.5 字面表述的偏差。** 见 §2.1：裁定为"条目生效、无条目回落全局"，并把写入方向拆成两个入口点。方案 §4.5 写的 `min` 语义与 §4.6 的草案互相矛盾，按 `min` 实现会让六个 L3 manifest 继续被拒。**这一处需要评审确认**——它是内核语义，不是实现细节。
3. **`NotifierPolicy()` 不在本 change。** 方案 §4.6 的草案要在全局 L2 之上给 notifier L3；按 §2.1 的裁定，那是一次**放宽**，必须走放宽入口（`WithWiderKindCeiling`）。策略派生本身留在后续切片（本 change 只把内核能力做出来，不动任何既有构造点）。
4. **`KindCeilings` 的 map 复制语义。** 两个写入入口返回**新的一份** `Policy`，接收者的 map 不会被写穿——一份被多个持有者共享的策略不会因为别人写了一个条目而变形。
5. **端到端那条 e2e 的落点。** 方案 §9 要求"复用 `tests/e2e/testenv` 的 `FakeSlack` httptest server，加一个'返回非零 errcode → 渠道测试接口报错'的子用例"。本 change 照此落：新增 `tests/e2e/notify_errcode_test.go`（catalog G8），`FakeSlack` 增加**增量**的响应覆盖（`SetReply`，默认仍回 200 "ok"），G4 的签名断言一字未改。

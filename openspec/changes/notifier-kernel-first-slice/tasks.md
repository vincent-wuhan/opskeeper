## 1. S-1：内核 per-kind safety ceiling

- [x] 1.1 `core/floor/kernel/host.go` 的 `Policy` 增加 `KindCeilings map[Kind]domain.SafetyLevel`，按 `design.md` §2.1 裁定为「条目生效、无条目回落全局」，MUST NOT 与全局取 `min`。
- [x] 1.2 增加两个查询口：`EffectiveSafetyCeiling(k)`（条目优先，零值条目按缺省处理）与 `WidestSafetyCeiling()`（全局与所有条目里最宽的一个）。
- [x] 1.3 增加两个方向相反的写入入口：`WithKindCeiling`（只收窄）与 `WithWiderKindCeiling`（只放宽），写反方向 panic 且文本指向正确方向；写入返回**新的一份**策略，不写穿接收者的 map。
- [x] 1.4 `NewHost` 校验每一条 ceiling：Kind 必须在封闭集合内、`SafetyLevel` 必须在 L0..L3，无法执行的策略直接 panic。
- [x] 1.5 `Host.Admit` 改用 `WidestSafetyCeiling()` 做包级准入（manifest 只声明整包一个级别，不声明贡献落在哪些 Kind）。
- [x] 1.6 per-kind 闸门落在 `Host.Activate` 的 `scopedRegistrar` 上；拒绝走既有的整包回滚 `h.reg.RemovePlugin(p.Name())`，不新造第二条回滚路径。报错文本区分「per-kind 条目拒绝」与「回落全局拒绝」两种来源。

## 2. S-1 测试

- [x] 2.1 `core/floor/kernel/policy_test.go` 覆盖三条不变式：条目生效（I1）、无条目回落全局（I2）、写入方向可测（I3）。
- [x] 2.2 逐位不变回归：`TestEmptyKindCeilingsKeepsAdmissionBitForBit`，并给 `datasource.ManagerPolicy()` / `toolset.adapterPolicy()` / edge `Profile.Policy()` 三个既有构造点各补一条「不受 per-kind ceiling 影响」的断言。
- [x] 2.3 把方案 §11.1 的准入对照固化成测试：L2 ceiling 下 L3 manifest 被拒、L3 ceiling 下同一个 manifest 通过（`TestSectionElevenOneComparesL2RejectionAgainstL3Acceptance`）。
- [x] 2.4 per-kind 闸门与整包回滚：notifier 条目放行、其他 Kind 按全局拒绝、拒绝后不留半激活条目。

## 3. S-4：内置 webhook sender 的渠道判定钩子

- [x] 3.1 `core/base/pkg/notify/webhook.go` 的 `webhookSender` 增加可空字段 `checkResponse func(body []byte) error`，`newWebhookSender` 增加对应参数。
- [x] 3.2 只在 `NewFeishuSender` / `NewDingTalkSender` / `NewWeComSender` 三处接线 `channelAPIError`；generic webhook / Slack / Telegram 不接线，行为逐位不变。三个渠道构造函数的公开签名不变。
- [x] 3.3 实现判定规则（`design.md` §3.2 六条）：空 body 通过、非 JSON 通过、无已知错误码字段通过、非零失败并带上错误码与原因、错误码接受带引号的数字、读取字节数上界 64 KiB；读响应体失败判失败。

## 4. S-4 测试

- [x] 4.1 `core/base/pkg/notify/webhook_test.go` 覆盖三渠道非零错误码判失败、零值错误码通过、空/不可读 body 通过、带引号的数字、未接钩子渠道忽略响应体、非 2xx 仍然失败。
- [x] 4.2 请求形状回归：`TestWiredHookDoesNotChangeTheRequestShape`，并确认既有 G4 e2e 签名断言（飞书签名在 body 顶层、钉钉签名在 URL query）一字未改。
- [x] 4.3 `core/manager/biz/alert/channel_sender_test.go` 补集成用例：钉钉非零 errcode 落 `notification_deliveries.failed`、写 `notification_failed` 事件、不写 `notification_sent`；`RetryWorker.RunOnce` 重投后落 `success`、attempt 递增为 2、写 `notification_sent` 事件、事故标记已通知。
- [x] 4.4 e2e：新增 `tests/e2e/notify_errcode_test.go`（catalog G8），`testenv.FakeSlack` 增加增量响应开关 `SetReply`（默认仍回 200 "ok"），验证测试通道接口在渠道于 200 里拒收时报 `accepted=false` 并带上错误码与原因，渠道恢复后又能测通。

## 5. 验收与交付

- [x] 5.1 `make pi-plugin-guard` 通过。
- [x] 5.2 `make module-check` 通过。
- [x] 5.3 `go test ./scripts/ledgercheck` 通过（manager 读数 1036 文件 / 261,603 行，与 `docs/opskeeper2-architecture.md` §六、`docs/opskeeper2-edge-autonomy-plan.md` 同步更新）。
- [x] 5.4 全量 `go test ./...` 通过（根模块 + `core/base` + `core/floor` + `core/manager` + `core/edge`）。含 `scripts/deadcode` 棘轮：基线实测 911 / 601 / 310，本 change 的三个新符号（`WithKindCeiling`、`WithWiderKindCeiling`、`FakeSlack.SetReply`）全部落在 test-only 一类，棘轮按树自身读数钉到 914 / 601 / 313，并在同一提交里写明理由。
- [x] 5.5 e2e `-tags e2e` 的 notify 用例通过：`TestNotify_ChannelCRUD_G1`、`TestNotify_ChannelErrcodeRejected_G8`、`TestNotify_Signed_G4`（Feishu_sign_in_body + DingTalk_sign_in_url）、`TestNotify_SlackRichFormat_G3`。
- [x] 5.6 `openspec validate --all --no-interactive` 通过。
- [x] 5.7 硬约束复核：`kind.go` / `notify/registry.go` / `datasource.go`（含 `ManagerPolicy()`）/ `sources_plugin.go`（含 `adapterPolicy()`）/ 告警派发主流程 / `plugins/` / manifest schema 全部零改动。
- [x] 5.8 审计口径：入库文件不含任何内部任务号（`scripts/audit_open_source.py` 的「internal task ID」规则）。
- [ ] 5.9 开 PR（`agent/agent/5e722b292aa9` 分支的脚手架在本地按实现实际写定；详见 `design.md` §6.1）。

## 6. 不在本 change

- [ ] 6.1 `NotifierPolicy()` 策略派生：按 §2.1 的裁定，在全局 L2 之上给 notifier L3 是一次**放宽**，必须走 `WithWiderKindCeiling`。留待后续切片。
- [ ] 6.2 候选插件包、渠道类型枚举扩展、装配层 Activate 一律不在本 change。
- [ ] 6.3 与 `notifier-plugin-plan.md` §4.5 字面 `min` 表述的偏差需要评审确认（`design.md` §6.2）。
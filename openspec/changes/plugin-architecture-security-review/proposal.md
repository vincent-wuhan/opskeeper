## Why

`plugin-architecture-recovery-plan` 任务 6.5 要求完成八项安全审查：secret 管理、日志脱敏、越权、跨租户、prompt injection、审批绕过、审计缺失和网络出口。仓内已有大量分散的防线（audit_open_source 闸门、dataguard redaction、promptguard、审批双签、PiG sandbox 四禁、tool gateway 七闸、tenantkey 推导闸门），但没有一份文档把它们对到 6.5 的八个条目上、逐项回答「已覆盖的闸门/测试证据是什么，缺口在哪」。本 change 补上这份收口审查，作为 v2026.11.0 正式版的发布前读数。

基线：`main@f4b76a36` 干净检出（detached HEAD，`git status` 干净）。全部读数一手：每个证据都指向具体文件、测试名或可复跑命令，不引用二手描述。

## What Changes

- 新增本 change 目录与 `security-review-6-5.md` 报告：八项逐条给出「已覆盖的防线盘点 + 测试/闸门证据 + 复跑命令」，缺口分「正式版阻塞」与「后续版本」两档。
- 报告结论同步进本 proposal 的 Impact 与 tasks（无代码改动）。

不修改任何生产代码、不动任何闸门配置。这是审查交付，不是整改交付。

## Capabilities

### New Capabilities

- `security-review-6-5`：八项安全审查的收口读数与缺口登记。它是文档型能力：要求的不是新行为，而是「每个安全断言都能指向一条可复跑的证据」。

### Modified Capabilities

- 无。

## Impact

- 产物：`openspec/changes/plugin-architecture-security-review/security-review-6-5.md`。
- 审查覆盖的代码面（只读）：`core/base/pkg/{secretbox,promptguard,audit,auth}`、`core/manager/dataguard`、`core/manager/biz/{hitl,approval,pigruntime,marketplace,imbridge}`、`core/manager/biz/aiops/tools/decorators`、`core/manager/server/{mcp,imbridge,marketplace,chatdiagnose}`、`core/domains/server/secret`、`core/edge/{cmdpolicy,autonomy,policygate,restart_service}`、`core/floor/{federation,pluginmanifest,skill/builtin}`、`scripts/{audit_open_source.py,plansecurity.sh,routeaudit,ledgercheck}`、`deploy/install/docker-compose.yml`。
- 发布影响：报告判定 3 项正式版阻塞（S-1 marketplace 生产默认 DevMode、S-2 IM 会话以超管身份运行、S-3 飞书明文事件无 verify_token 校验），其余缺口为后续版本。阻塞项不阻塞 rc.1（已发布），阻塞正式版。

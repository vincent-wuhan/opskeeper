## Why

安全审查 `plugin-architecture-security-review`（PR #145，commit `3d206d5e`，报告 `security-review-6-5.md` §159-160、§72）定级 3 项 S 级（正式版阻塞）缺陷，基线为最新 main 干净检出。三项的共同根因是**信任边界上的默认值放行**：三个入口都以"最宽松"的姿态启动，安全属性依赖管理员事后记得收紧。

**S-1 marketplace 生产默认 DevMode=true**：`cmd/opskeeper/main.go:3148-3149`：

```go
mpDevMode := strings.EqualFold(os.Getenv("OPSKEEPER_MARKETPLACE_DEVMODE"), "true") ||
    os.Getenv("OPSKEEPER_MARKETPLACE_DEVMODE") == ""
```

环境变量未设置（空）时 DevMode=true。`deploy/install/docker-compose.yml` 的 opskeeper service environment 块（本次复核：无任何 MARKETPLACE 条目）与 `deploy/install/install.sh`（全文 grep 无 MARKETPLACE 命中，仅 55 处其他 OPSKEEPER_ 变量）均不设置该变量——生产部署默认走 DevMode：`checkSourceAllowed`（`core/manager/biz/marketplace/usecase.go:614`）直接 `return nil`，任意 git/tarball URL 的源都放行；`requiresSigned` 检查（`usecase.go:316` `if !uc.cfg.DevMode`）同样被短路。install 是 admin-only（这层闸在），但「任意 URL 的 git clone + tarball 解包 + 技能装载」在默认部署下对 admin 是无源约束的——技能 PromptBody 会进系统提示，即 prompt injection 的直通面。

**S-2 IM 会话固定超管身份**：`cmd/opskeeper/main.go:2331` `const imBridgeServiceUserID uint64 = 1`（本次复核行号一致）。传导链（逐环核实于本仓）：

1. `cmd/opskeeper/imbridge_agent_wiring.go:65` `caller()` 只填 `UserID`、Role 留空（注释自认 "admin gating doesn't apply on the IM path"）；
2. `core/manager/service/aiops/service.go:644` role 从 `tenantctx.From(ctx)` 取——webhook 路径的 ctx 是 `core/manager/server/imbridge/http.go:227` 的裸 `context.Background()`，无 tenant → role 为空；
3. `core/manager/biz/aiops/chatruntime/runtime.go:613` `viewerOnly := req.Role == "viewer"`——空 role 即全量工具袋；
4. `core/manager/biz/aiops/tools/configchange/config_tools.go:288` `validateApplyGate` 要求 `Role=="admin" || IsSuperuser`——空 role 落 Forbidden 之外没有任何收紧，而 user_id=1 本身 `IsSuperuser=true`；
5. MCP trusted-server 直连 `CallMCPTool`（`core/manager/biz/aiops/tools/mcp_basetool.go`）无 caller 校验。

后果：任何能向已配置 IM bot 发消息的人（飞书群任何成员，含被拉群的外部协作者；Telegram/Slack 有 sender 白名单缓解）触发一个 superuser 会话，绕过 RBAC——审查原文「任何在 casbin 之上的 superuser 短路」（`core/manager/iam/biz/authz/authz.go:11` 自述）。`cloud_bash`（`tools/cloud_bash_basetool.go`，Class=destructive）仅剩 HITL propose-confirm 一道闸。

**S-3 飞书明文事件零校验**：`core/manager/server/imbridge/http.go:165` `if app.EncryptKey != ""` 才验签，为空则整体跳过；payload `header.token`（verify_token）全仓无消费方（模型有字段、reveal 端点能读，事件路径不用）。入口 `validate()`（`core/manager/biz/imbridge/usecase.go:122`）已要求 webhook 模式必填 encrypt_key，但防不住历史行 / 直改 DB / 旁路写入。伪造一条 `im.message.receive_v1` 即可驱动 S-2 的 superuser agent。

三项在 v2026.11.0 正式版是发布阻塞。

## What Changes

**S-1 — DevMode 默认值反转为 false（fail closed）**

- `mpDevMode` 判定改为仅接受显式 `true`/`1`（大小写不敏感）；未设置 / 其他值一律 false。devmode 只能显式开启。
- `deploy/install/docker-compose.yml` 与 `deploy/docker-compose.yml` 的 opskeeper service environment 增加 `OPSKEEPER_MARKETPLACE_DEVMODE`（默认空 → false）；`.env.example` / `deploy/.env.example` / `deploy/install/.env.example` 补注释条目（一并处理审查 F-12 的 marketplace 安全开关缺档：`_DEVMODE` / `_REQUIRE_SIGNED_SOURCES` / `_PINNED_PUBKEY` / `_REGISTRIES`）。
- 错误提示 `usecase.go:624` 的 "set OPSKEEPER_MARKETPLACE_DEVMODE=true to override" 保留（语义仍对，只是不再是默认）。
- 守卫测试：`cmd/opskeeper/marketplace_devmode_test.go` 锁定"未设 → false、true/1/TRUE → true、false/拼错 → false"；marketplace 包既有 `TestInstall_SourceAllowlist` / `TestInstall_RequireSigned_*` 已覆盖闸门本身。
- UI 文案 `web/src/pages/settings/Marketplace.tsx:1002` 提示同步新默认。

**S-2 — IM 专用受限服务身份（非超管）**

- `core/manager/iam/biz/user` 新增 `EnsureIMBridgeServiceUser`：按固定 email（`imbridge-svc@opskeeper.internal`）幂等查找或创建 `Role=user`、`IsSuperuser=false`、`Status=active` 的服务账号，密码为随机不可登录值。
- `cmd/opskeeper/main.go:2331` 的 `const 1` 改为启动时解析的服务账号 id；失败 → Error 日志 + **禁用 IM bridge**（不注册 handler 公开路由 / protected 路由 / stream supervisor），manager 其余功能继续。
- `cmd/opskeeper/imbridge_agent_wiring.go:65` `caller()` 显式 `Role: RoleUser`，使 chatruntime / configchange apply / 后续按 role 收紧的闸看到真实受限角色而非"空 = 当作最高权限"。
- 该账号不出现在登录面（随机密码不落任何地方），仅作 ownership / audit 锚点。
- 守卫测试：`core/manager/iam/biz/user` 的创建/幂等/错误上抛；`cmd/opskeeper` 的 caller 非 admin。

**S-3 — 飞书 EncryptKey 为空时拒绝处理事件（fail closed）**

- `handleFeishuEvent` 在 `GetAppByAppID` 成功后：`app.EncryptKey == ""` → Warn 日志 + 401，不进入解密/分发。验签逻辑（sha256(timestamp+nonce+key+body)，`core/manager/biz/imbridge/provider/feishu/verify.go`）保持不变。
- url_verification 握手（`http.go:129`，app 解析之前）不受影响——challenge 不触发任何 agent 动作，泄露面为零。
- `verify_token` 本 change 不引入消费方：飞书 v2 事件请求不携带 token（token 只出现在 url_verification 握手里），事件路径无从比对；UI hint 标注"当前不参与事件校验"，避免运营者误以为它是防线。
- 守卫测试：`core/manager/server/imbridge/http_test.go`（该文件已有 7 个审计类测试，fake repo/bridge 模式可直接复用）——空 key 401 且 bridge 未被调用；有 key + 正确签名放行；错签名 401 回归。

S-2 与 S-3 同属 IM 面信任边界，按审查建议合并为本 change 的两个 capability 一并整改。

## Capabilities

### New Capabilities
- `marketplace-install-gate`：marketplace 安装闸的默认安全姿态（源白名单 + 签名强制 + devmode 显式开启语义）
- `im-bridge-service-identity`：IM bridge 的专用受限服务身份及其启动语义
- `im-webhook-event-verification`：IM webhook 入站事件的强校验（fail closed）

### Modified Capabilities
- 无（三处均无既有 spec 覆盖，全部为新增 capability）。

## Impact

**Affected code**
- `cmd/opskeeper/main.go:3148`（DevMode 默认反转）、`cmd/opskeeper/main.go:2331`（服务账号 id 替换硬编码常量）、imbridge 装配点（`:2332-2334`、`:3838`、`:3948`）
- `core/manager/iam/biz/user/usecase.go`（新增 EnsureIMBridgeServiceUser）
- `cmd/opskeeper/imbridge_agent_wiring.go:65`（caller Role 显式化）
- `core/manager/server/imbridge/http.go:165`（空 EncryptKey 拒绝）
- `deploy/install/docker-compose.yml`、`deploy/docker-compose.yml`、`.env.example`、`deploy/.env.example`、`deploy/install/.env.example`
- `web/src/pages/settings/Marketplace.tsx:1002`、`web/src/pages/settings/Channels.tsx`（verify_token hint）

**Affected APIs**：无新增/破坏。`/api/v1/im/feishu/events` 对"未配置 encrypt_key 的 app"从 200 变 401（本就是不可达的安全姿态）。

**Affected dependencies**：无新增。

**兼容性 / 迁移**
- 默认部署升级后 DevMode 自动 false：`opskeeper-official` 未签名包被拒（本就该拒）；确需 dev 放开的集群显式设 `OPSKEEPER_MARKETPLACE_DEVMODE=true`。`local` 源本就在 `AllowedSources` 白名单（`main.go:3189`），scp 本地包流程不受影响。
- 存量 IM thread 映射指向旧 superuser 会话：不迁移（历史可审计），新消息触发新会话落到服务账号名下。
- 存量空 encrypt_key 的 webhook app：升级后事件被拒，管理员在 Channels 页补 key（UI 本就必填，只有旁路写入的行受影响）。

**风险**
- 最大风险是隐性依赖 devmode 的流程被打破。缓解：local 源不受影响；受影响的只有"未签名 + 声称 official"路径（正是要挡的）；环境变量逃生门可先灰度。
- 服务账号创建失败禁用 IM 面：接受（fail closed 优于 superuser 旁路），Error 日志给出恢复指引（查 DB）。

**回滚**：三项各自独立 commit（Conventional Commits），`git revert` 单独回滚；S-1 另有运行时逃生门（env 显式 true）。

**发版**：本 change 合入后评估 v2026.11.1 修补版（生产已发 v2026.11.0）。

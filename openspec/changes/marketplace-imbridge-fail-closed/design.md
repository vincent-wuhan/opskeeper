# Design: marketplace + imbridge fail-closed 整改

## 0. 决策记录（ADR 摘要）

| 决策点 | 候选 | 采用 | 理由 |
|---|---|---|---|
| S-1 默认值 | A. 代码级反转为 false；B. compose/install.sh 显式设 false；C. 两者都做但代码保持空=true | A + compose 补透传 | B 是审查给出的"或"选项，但它依赖"每个部署工件都记得"，正是本次缺陷的成因，且修不了 helm/裸机/`make run` 跑法——闸门语义在代码里就该是对的。代码反转后 compose 的透传条目只是文档价值（让运维知道有这个开关）。 |
| S-2 身份 | A. 专用 user 角色服务账号；B. 保留 admin 但 IM 面配工具黑名单；C. 立即上 per-IM-user 绑定（审查标注的 S3） | A | B 的黑名单随工具新增必然漏（本仓工具面 60+，Class 标注非强制契约）；C 是长期项，不能作为发布阻塞项的修复。专用账号复用现有 role 闸（configchange apply 的 admin 检查、chatruntime viewer 过滤、casbin 成员模型），改动面最小且默认拒。 |
| S-2 失败模式 | A. 创建失败→禁用 IM bridge 继续；B. 失败→退出进程 | A | manager 的核心价值（监控/告警/边缘/审批）不依赖 IM；为一个非关键通道杀整个进程，故障半径不成比例。禁用 + Error 日志即可，语义上等价于"该部署没有 IM 功能"而非"IM 以超管身份运行"。 |
| S-2 身份锚点 | A. 固定 email 幂等键；B. 保留 id=1 常量但换语义 | A | `id=1` 是 bootstrap 顺序的经验事实而非 schema 保证（`BootstrapAdmin` 只在 users 表空时创建，`cmd/opskeeper/main.go:519`）；把巧合当契约正是缺陷本身。email 走 `users.email` 的 uniqueIndex（`core/manager/iam/model/model.go:111`），幂等有 schema 支撑。 |
| S-3 空 EncryptKey | A. 拒绝所有事件；B. 只拒绝写类工具调用；C. 校验 payload header.token | A | B 的"写类"在事件入口不可判定（要等 agent 跑起来才知道会调什么），且读类结果回发到群里本身也是信息泄露面；C 不可行——飞书 v2 事件请求体不携带 token（token 只在 url_verification 握手出现，`http.go:103` 注释自明），事件路径无从比对。审查建议的两个"或"选项里，强制 EncryptKey 非空是可执行的那个。 |
| S-3 与 stream 模式 | stream（WebSocket 长连接）不受影响 | 不改 | 长连接由 manager 主动 dial、AppSecret 鉴权，不存在"任意人 POST"面。Telegram/Slack 走 getUpdates / Socket Mode 同理，且已有 allow_from 强制（ADR-031）。 |
| S-2 Role 贯通方式 | A. adapter 显式传 `Role: RoleUser`；B. 在 adapter 构造 tenantctx 塞进 ctx | A | tenantctx 的合法生产者是 auth 中间件（`core/base/pkg/auth`）；biz 层伪造请求上下文身份会污染审计 slot（audit 装饰器读同一 context）。Role 走 service Caller 显式传参，身份链清晰：handler(无) → adapter(显式) → service → runtime。 |

被放弃方案的理由统一收录在上表，不重复展开。

## 1. S-1 marketplace DevMode 反转

### 现状（main `3d206d5e`）

```
cmd/opskeeper/main.go:3148-3149
  mpDevMode := strings.EqualFold(os.Getenv("OPSKEEPER_MARKETPLACE_DEVMODE"), "true") ||
      os.Getenv("OPSKEEPER_MARKETPLACE_DEVMODE") == ""     // ← 空 = true

core/manager/biz/marketplace/usecase.go:614  checkSourceAllowed: if DevMode → return nil
core/manager/biz/marketplace/usecase.go:316  requiresSigned 闸:  if !DevMode → 才检查
```

`deploy/install/docker-compose.yml` opskeeper service environment（53-130 行区间复核）无 MARKETPLACE 条目；`install.sh` 全文无；三个 `.env.example` 均无（即审查 F-12 登记的 env 文档缺档）。

### 目标语义

| env 值 | DevMode |
|---|---|
| 未设置 | **false** |
| `true` / `TRUE` / `1` | true |
| 其他任何值（`false` / `0` / 拼错） | false |

实现：提取 `cmd/opskeeper` 包内小函数（如 `marketplaceDevModeFromEnv(raw string) bool`），main.go 装配处调用。提取为函数是为了表驱动测试——main.go 的裸语句没法直接测。

不引入"反向开关"（如 `OPSKEEPER_MARKETPLACE_STRICT`）——一个布尔两个名字必然漂移。

### 部署面同步（F-12 一并处理）

- `deploy/install/docker-compose.yml` + `deploy/docker-compose.yml`：environment 增加 `OPSKEEPER_MARKETPLACE_DEVMODE: ${OPSKEEPER_MARKETPLACE_DEVMODE:-}`，默认空串 → false。
- 三个 `.env.example` 补注释条目：`OPSKEEPER_MARKETPLACE_DEVMODE`（留空 = 签名闸生效，显式 true 仅开发）、`OPSKEEPER_MARKETPLACE_REQUIRE_SIGNED_SOURCES`、`OPSKEEPER_MARKETPLACE_PINNED_PUBKEY`、`OPSKEEPER_MARKETPLACE_REGISTRIES`。
- `web/src/pages/settings/Marketplace.tsx:1002` 错误提示改为反映新默认。

### 守卫测试

`cmd/opskeeper/marketplace_devmode_test.go`：
- `TestMarketplaceDevModeDefaultIsFalse`：空串 → false。
- `TestMarketplaceDevModeExplicitTrue`：`true` / `1` / `TRUE` → true。
- `TestMarketplaceDevModeGarbageIsFalse`：`false` / `0` / 拼错 → false。

（marketplace 包闸门行为已有 `core/manager/biz/marketplace/usecase_test.go` 的 `TestInstall_SourceAllowlist`、`TestInstall_RequireSigned_*` 覆盖，DevMode=false 路径即生产默认路径，无需重复。）

## 2. S-2 IM 专用受限服务身份

### 身份模型

```
users 幂等确保一行（非 migration，boot 时执行）：
  email        = "imbridge-svc@opskeeper.internal"   ← uniqueIndex 支撑幂等
  display_name = "IM Bridge (service)"
  role         = "user"        ← 非 admin、非 viewer；走 casbin 成员模型而非 superuser 短路
  is_superuser = false         ← 关键：不触发 authz.go:11 的 superuser-bypass
  status       = "active"
  pass_hash    = 随机 32 字节密码的 argon2id 散列（密码本体不落任何地方）
```

- 密码用随机值而非空串：空 pass_hash 可能被"空密码登录"类回归放过；随机值在 argon2id 下不可碰撞，且 `Login` 路径（`usecase.go:94`）对不存在凭据直接失败。
- email 选 `.internal` 保留域，明确非投递地址、非登录入口。

### 装配时序

```
cmd/opskeeper/main.go 现有顺序：
  514-516  userRepo / userUC 建好（已有）
  1700-1704  imbridgeRepo / imbridgeUC 建好（已有，早于 aiopsSvc）
  2318  aiopsSvc 建好（已有）
  [新增] 在 2331 之前：
    imServiceUser, svcErr := userUC.EnsureIMBridgeServiceUser(rootCtx)
    svcErr != nil → log.Error + imbridgeEnabled = false
    正常       → imBridgeServiceUserID = imServiceUser.ID
  2332-2334  adapter / bridge / handler：imbridgeEnabled=false 时全部跳过（nil handler）
  3838 / 3948  RegisterPublic / RegisterProtected：nil handler 跳过
  2353  stream supervisor：同样跳过
```

禁用即"该部署没有 IM 功能"——`HandleInbound` 不可达、无出站长连接，而不是"IM 以超管身份运行"。

### Role 贯通

`cmd/opskeeper/imbridge_agent_wiring.go:65` 从 `svcaiops.Caller{UserID: id}` 改为 `svcaiops.Caller{UserID: id, Role: svcaiops.RoleUser}`。

传导链闭合验证（逐环在本仓核实）：
- `configchange/config_tools.go:288` `validateApplyGate`：`Role != "admin" && !IsSuperuser` → Forbidden。服务账号两个条件都命中收紧侧。
- `chatruntime/runtime.go:613` `viewerOnly`：user 非 viewer，保留 read 工具 + 非 destructive 工具，但 configchange apply 等显式 admin 闸已在上游挡住。
- casbin：服务账号不是任何 org 成员（不为它建 membership），全部受 casbin 管辖的路由对它默认拒。

**不在 adapter 构造 tenantctx**（见 §0 决策表最后一行）。

### 存量数据

- `im_threads.opskeeper_session_id`（本仓为 opskeeper 会话）指向的旧 chat_sessions 不动：历史可审计。切换后旧 thread 首条消息会因新 caller 无 ownership 触发新会话分配 + 映射行 rotate，一次性扰动，比静默沿用超管会话好。
- 审计：新事件全部落在新服务账号名下，与历史 admin 名下事件天然可区分。

### 守卫测试

`core/manager/iam/biz/user/usecase_imbridge_test.go`：
- 首次创建：role=user、is_superuser=false、email 幂等键正确、pass_hash 非空且非可登录值。
- 二次调用：返回同一 id，不新建行。
- repo 报错：错误上抛（装配层据此禁用）。

`cmd/opskeeper/imbridge_agent_wiring_test.go`（文件已存在，追加）：
- `caller().Role == RoleUser` 且 `IsAdmin() == false`。

## 3. S-3 飞书空 EncryptKey fail-closed

### 变更点（单处）

`core/manager/server/imbridge/http.go` 在 `GetAppByAppID` 成功后（当前 161 行附近）插入：

```go
if app.EncryptKey == "" {
    h.log.Warn("feishu event rejected: app has no encrypt_key (fail closed)",
        slog.String("app_id", appID))
    http.Error(w, "encrypt_key not configured", http.StatusUnauthorized)
    return
}
```

原有 `if app.EncryptKey != ""` 验签块随之恒真，可简化为无条件验签（key 已非空）。解密分支（179 行的空 key 拒绝）成为不可达防御层，保留。

url_verification 握手（129 行）在 app 解析**之前**、不受影响——握手本就不携带签名，飞书控制台配置 URL 时 app 行可能还没填 key，拒绝握手会让管理员配不上。代价是 challenge 可被伪造回显，但 challenge 不触发任何 agent 动作，泄露面为零。

### 分层防御回顾

| 层 | 机制 | 状态 |
|---|---|---|
| 入口 | `validate()` webhook 模式必填 encrypt_key（`usecase.go:122`） | 已有，防新增 |
| 运行时 | handler 空 key → 401 | **本 change 新增**，防存量 / DB 旁路 |
| 传输 | 签名 sha256(ts+nonce+key+body)（`provider/feishu/verify.go`） | 已有，key 非空后恒生效 |

verify_token 维持"存而不校验"，UI hint 标注真实语义，避免虚假安全感。

### 守卫测试

`core/manager/biz/imbridge/provider/feishu/verify_test.go`（包内目前无测试文件）：
- `TestVerifyEventSignature_Match/Mismatch`：锁算法行为。

`core/manager/server/imbridge/http_feishu_gate_test.go`（复用 `http_test.go` 已有的 fake repo / audit 模式）：
- 空 key app + 合法形状事件 → 401，fake bridge 断言未被调用。
- 有 key + 正确签名 → 放行到 bridge。
- 有 key + 错签名 → 401（回归既有行为）。

Handler 构造签名 `NewHandler(bridge, apps, uc, log)` 参数已是接口，fake 直接实现，无需改签名。

## 4. 风险分级与测试策略

| 项 | 等级 | 缓解 |
|---|---|---|
| DevMode 反转破坏隐性 dev 流程 | 中 | local 源白名单不受影响；环境变量逃生门；守卫测试锁语义 |
| 服务账号创建失败禁用 IM | 低（fail closed 是目标行为） | Error 日志 + 明确恢复指引（查 DB 连接） |
| 旧 IM thread 首条消息重建会话 | 低 | 一次性扰动；旧会话历史不丢 |
| 空 key 拒绝影响存量 webhook app | 低 | UI 本就必填；受影响行均为旁路写入 |
| casbin 语义漂移（服务账号无 membership） | 低 | 默认拒正是目标；若后续 IM 面需要受控放行，另立 change 配 membership | 

测试矩阵：每个 S 级项至少 1 个守卫测试（见各节）；全部为纯单测（fake repo / httptest），不依赖 DB 或外网。CI 既有 `-race` 照跑。

## 5. 验证方式

1. `go build ./cmd/opskeeper/ ./core/...` 通过。
2. `go test ./cmd/opskeeper/ -run 'MarketplaceDevMode|ImbridgeAgentWiring' -race`、`go test ./core/manager/iam/biz/user/ ./core/manager/biz/imbridge/... ./core/manager/server/imbridge/ -race` 全绿。
3. 手工冒烟（交付验证者执行）：默认 env 启动 → 启动日志 `dev_mode=false`（`main.go:3693` 处）；未签名 official 包安装被拒；设 `OPSKEEPER_MARKETPLACE_DEVMODE=true` 重启 → 放行。IM 面按 spec 场景逐条 curl。

## 6. 回滚思路

- 单 commit per S 项，`git revert` 独立回滚。
- S-1 有运行时逃生门（env 显式 true），可先于发版灰度。
- S-2 回滚后回到 const 1（superuser）——仅在与 S-3 之外的未知回归冲突时才考虑，且必须在回滚 PR 里重开安全单（等级不降）。

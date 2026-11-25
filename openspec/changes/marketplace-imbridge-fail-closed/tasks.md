# Tasks: marketplace + imbridge fail-closed 整改

## Phase 1: S-1 marketplace DevMode 默认反转

- [x] 1.1 `cmd/opskeeper/main.go:3148`：DevMode 判定改为仅显式 true/1（未设/其他 → false），提取包内可测函数
- [x] 1.2 守卫测试 `cmd/opskeeper/marketplace_devmode_test.go`：空→false、true/1/TRUE→true、false/0/拼错→false
- [x] 1.3 `deploy/install/docker-compose.yml` + `deploy/docker-compose.yml`：opskeeper service environment 增加 `OPSKEEPER_MARKETPLACE_DEVMODE`（默认空）
- [x] 1.4 `.env.example` / `deploy/.env.example` / `deploy/install/.env.example`：补 `_DEVMODE` / `_REQUIRE_SIGNED_SOURCES` / `_PINNED_PUBKEY` / `_REGISTRIES` 注释条目（F-12 一并）
- [x] 1.5 `web/src/pages/settings/Marketplace.tsx:1002` 提示文案同步新默认

## Phase 2: S-2 IM 专用受限服务身份

- [x] 2.1 `core/manager/iam/biz/user/usecase.go`：新增 `EnsureIMBridgeServiceUser`（固定 email 幂等、role=user、is_superuser=false、随机不可登录密码）
- [x] 2.2 守卫测试 `usecase_imbridge_test.go`：创建 / 幂等 / repo 错误上抛
- [x] 2.3 `cmd/opskeeper/main.go:2331`：const 1 改为启动解析的服务账号 id；失败 → Error 日志 + 禁用 IM bridge（nil handler，`:3838` / `:3948` / supervisor 均跳过注册），manager 其余功能继续
- [x] 2.4 `cmd/opskeeper/imbridge_agent_wiring.go:65`：`caller()` 显式 `Role: RoleUser`
- [x] 2.5 守卫测试：追加到 `cmd/opskeeper/imbridge_agent_wiring_test.go`，caller 非 admin、Role 为 user

## Phase 3: S-3 飞书空 EncryptKey fail-closed

- [x] 3.1 `core/manager/server/imbridge/http.go`：app 解析后空 EncryptKey → Warn + 401；验签块简化为无条件（key 已非空）
- [x] 3.2 守卫测试 `core/manager/biz/imbridge/provider/feishu/verify_test.go`：签名 match / mismatch
- [x] 3.3 守卫测试 `core/manager/server/imbridge/http_feishu_gate_test.go`：空 key 401 且 bridge 未调用；有 key 正确签名放行；错签名 401
- [x] 3.4 `web/src/pages/settings/Channels.tsx`：verify_token hint 标注"当前不参与事件校验"

## Phase 4: 交付

- [x] 4.1 `go build ./cmd/opskeeper/ ./core/...` + 相关包 `go test -race` 全绿
- [x] 4.2 三项各自独立 commit（Conventional Commits），patch 附件通道交付
- [ ] 4.3 评估 v2026.11.1 修补版发版（发布运维官，在三项合入后）

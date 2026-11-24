## ADDED Requirements

### Requirement: DevMode 默认关闭（fail closed）

manager 启动时，marketplace 的 DevMode MUST 仅在 `OPSKEEPER_MARKETPLACE_DEVMODE` 显式等于 `true`（大小写不敏感）或 `1` 时开启；环境变量未设置或为任何其他值时 MUST 为 false。

#### Scenario: 默认部署签名闸生效

- **GIVEN** 部署未设置 `OPSKEEPER_MARKETPLACE_DEVMODE`（官方 `deploy/install/docker-compose.yml` 与 `install.sh` 均不设置）
- **WHEN** manager 启动
- **THEN** marketplace DevMode 为 false
- **AND** `checkSourceAllowed` 按 AllowedSources 白名单执行，白名单外 source 安装返回 Forbidden
- **AND** `requiresSigned` 信任闸生效：声称来自 `opskeeper-official` 的包无有效 signature 时安装返回 Forbidden

#### Scenario: 显式开启 devmode

- **GIVEN** 环境变量 `OPSKEEPER_MARKETPLACE_DEVMODE=true`
- **WHEN** manager 启动
- **THEN** DevMode 为 true，白名单与签名闸均被旁路（开发集群语义不变）

#### Scenario: 非法值不放过

- **GIVEN** 环境变量 `OPSKEEPER_MARKETPLACE_DEVMODE=flase`（拼错）或 `0`
- **WHEN** manager 启动
- **THEN** DevMode 为 false（不允许任何 truthy 启发式放大攻击面）

### Requirement: 部署工件不得隐式开启 DevMode 且安全开关有文档

随仓库分发的部署工件（生产 install 包与 dev compose 的 opskeeper service）MUST NOT 为 `OPSKEEPER_MARKETPLACE_DEVMODE` 提供非空默认值；`.env.example` MUST 记录 marketplace 安全开关（`_DEVMODE`、`_REQUIRE_SIGNED_SOURCES`、`_PINNED_PUBKEY`、`_REGISTRIES`）及其"留空 = 签名闸生效"的语义。

#### Scenario: 生产包默认安全

- **GIVEN** 操作员使用 `deploy/install/` 原样安装且不编辑 .env
- **WHEN** 栈启动
- **THEN** 容器内 `OPSKEEPER_MARKETPLACE_DEVMODE` 为空，DevMode 为 false

#### Scenario: dev compose 显式逃生门

- **GIVEN** 开发者在 `deploy/.env` 设置 `OPSKEEPER_MARKETPLACE_DEVMODE=true`
- **WHEN** dev 栈启动
- **THEN** DevMode 为 true，且该行为在 `.env.example` 注释中有据可查

#### Scenario: local 源不受默认收紧影响

- **GIVEN** DevMode 为 false 且管理员安装 `local` 类型（本机路径）
- **WHEN** 执行安装
- **THEN** `local` 在 AllowedSources 白名单内，安装照常进行（收紧只挡白名单外与未签名 official 源）

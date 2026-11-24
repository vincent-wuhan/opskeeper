# v2026.11.0 正式版发版记录（2026-11-24）

A 档最小生产版在 rc.1 之后的收官发布。前置：`release-readiness-v2026.11.0.md`（闸门矩阵与剩余风险）。

## 目标环境

- 阿里云 ECS `i-0jlh5iv8xb89sals283z`（8.130.126.29，乌兰察布，Ubuntu 26.04 x86_64），compose 工程 `/opt/opskeeper`（`docker-compose.json`，服务名 `opskeeper`）。
- 访问通道：**SSH（`~/.ssh/id_ed25519`）**。本轮 Workbench CLI 不可用（云助手对该账号的两个 profile 都报 `InvalidParameter.InstanceId`，生产机不在当前 AK 的实例列表里；`~/.workbench/config.json` 的 aliecs profile 只能看到账号内的另一台上海实例）。SSH 直连可用，且比云助手稳——本次上传 150MB 单文件一次成功，不需要分片。
- 变更前状态：主容器 `opskeeper:v2026.11.0-rc.1`（id `abbe0ee3fcee`），healthz 200。

## 产物与传输

- 镜像在本机（Apple Silicon）构建：`opskeeper:v2026.11.0`，id `sha256:eedfc5be4a08335b2b5fcc3f1ef5040559a40d109e3676c6292832f12bd9a55b`，linux/amd64，149,538,347 B。构建器 `docker buildx build --builder default`（`desktop-linux` 的 container driver 需要拉 `moby/buildkit`，当日 Docker Hub 不通）。
- 导出 `opskeeper-v2026.11.0.tar`，149,563,904 B，sha256 `9fa4f6d7069f7804530ed8ea48d30d443d2753133b2a52e95116e3f7736d0424`；`scp` 直传目标机 `/opt/opskeeper/images/`，`docker load` 后本地 id 与上传前一致。
- GitHub Release `v2026.11.0` 已建，挂产物 `opskeeper-v2026.11.0.tar`（149,563,904 B）。

## 一处必须写下来的构建偏离：ONNX Runtime 归档取不到

**当天 GitHub release 资产域全线不通**：本机、腾讯云机 203.195.204.106、生产机三处对 `github.com/microsoft/onnxruntime/releases/download/v1.20.1/onnxruntime-linux-x64-1.20.1.tgz` 一律 404（9 字节 "Not Found"），**连本仓自己的 rc.1 产物 URL 也是 404**；`registry-1.docker.io` 同样时通时断（`docker/dockerfile:1.7` 重试到第 3 次才拉下来，`debian:bookworm-slim` 重试到第 7 次）。因此 `scripts/fetch_onnxruntime.sh` 那条按钉死 SHA 校验的取件路径当天无法完成。

绕法不是换一个来源的 \`.so\`，而是**取回同一份上游字节**：生产机上还留着 rc.1 镜像，而 rc.1 的构建缓存正是当年通过 SHA 校验的归档。从 `opskeeper:v2026.11.0-rc.1` 里取出 `libonnxruntime.so.1.20.1`（16,559,416 B，sha256 `a5faaf78a37590d3fe640f887620e74f6022d34550172b91ad2131bf0ad77d64`），按上游成员路径 `onnxruntime-linux-x64-1.20.1/lib/libonnxruntime.so.1.20.1` 打成 `builder-cache/onnxruntime-linux-amd64-1.20.1.tgz`，喂给 Dockerfile。

- **偏离的边界要说清**：归档级 SHA（`2c2db5b0…`）当天无法复算，因此这一步**没有**通过 `fetch_onnxruntime.sh` 的归档校验，而是直接构建（脚本未被修改，钉子未被改动）。`deploy/Dockerfile.opskeeper` 自身只校验归档内成员路径存在，该成员来自 rc.1 那次 SHA 校验过的构建，字节同源。
- **未提交**：缓存包在 `.gitignore` 内，不进仓；构建走 `docker buildx build` 直接命令而非 `make docker-build`（后者会先跑取件脚本并因归档 SHA 不匹配而失败）。
- **补齐条件**：网络恢复后重跑 `make docker-build` 应能在脚本的归档 SHA 校验下复现同一份产物；把这条写进 R-5。

## 部署执行

1. `docker load -i /opt/opskeeper/images/opskeeper-v2026.11.0.tar`（目标机确认 `opskeeper:v2026.11.0` = `eedfc5be4a08`）。
2. `cp docker-compose.json docker-compose.json.bak-<ts>`，再把服务镜像从 `opskeeper:v2026.11.0-rc.1` 改为 `opskeeper:v2026.11.0`。
3. `docker compose -f docker-compose.json up -d opskeeper`（容器重建，`opskeeper` → `opskeeper:v2026.11.0`）。
4. 验证：`https://opskeeper.yueming.xin/healthz` → 200 `ok`；`/api/v1/version`（compose 内 JWT secret 现场签 10 分钟管理员 token）→ `{"manager_version":"v2026.11.0"}`；`/api/v1/runtimes` → 4 项，health online。
5. 旧镜像 `opskeeper:v2026.11.0-rc.1` 与 compose 备份**均保留**，回滚路径即"改回一行镜像字段 + `up -d opskeeper`"。

## 本地验证矩阵（干净检出 `f4b76a36`）

| 项 | 结果 |
|---|---|
| 根模块测试 | 全绿（`cigate` 在 `/tmp` 工作树下报路径噪声，同一包在常规路径 144 条全绿） |
| core/manager 全量 | 3906 条 / 163 包全绿 |
| race（根模块） | 全绿 |
| 集成测试（`-tags=integration`） | 全绿 |
| E2E（fake 模式） | 全绿（139.7s；首轮 `TestAPluginToolCrossesTheGateAndRunsOnTheNodeHost` 一次抖动，单跑两连绿后全量复跑绿） |
| 数据迁移演练（真 MySQL 8.4） | `make mysql-migration-check` 绿：整份迁移表在部署所用方言上跑三遍（含新增的 `MigrateTaskTables` 与 `MigrateToolCallTable`，migrator 计数 27） |
| protocol-validate / broker-pin-check | PASS / ok |
| Web（pnpm） | install + typecheck + build + vitest 全绿 |
| 插件 dashboard | teamharness 97/97；installer 1/1 |
| Python 发布测试 | 54 条全绿 |
| 闸门套件（domaincheck / ledgercheck / deadcode / splitprice / audit-open-source） | 全绿 |

## 剩余风险与回滚

- **R-5（新增）**：ONNX Runtime 归档级 SHA 当天无法复算，镜像内的 `.so` 字节取自 rc.1 的 SHA 校验构建（同源，尺寸与 sha256 已记）。网络恢复后需复跑 `make docker-build` 取得可复算的归档级证据。
- **R-6（新增）**：GitHub release 资产域与 Docker Hub 当天多机位不通，发布链路对这两个域的依赖需要独立镜像/缓存策略（本次靠本地重试与生产机存量镜像绕过）。
- **回滚触发**：healthz 非 200、`/api/v1/version` 不等于 `v2026.11.0`、runtimes 数量掉零、迁移报错。
- **回滚动作**：compose 备份 → 镜像字段改回 `opskeeper:v2026.11.0-rc.1` → `docker compose -f docker-compose.json up -d opskeeper` → 复验 healthz（rc.1 时做过一次完整来回演练）。
- **观察窗口**：48 小时，重点看 PiG 生命周期写入、runtime 清单（4 项）、任务表迁移后的首次写入。

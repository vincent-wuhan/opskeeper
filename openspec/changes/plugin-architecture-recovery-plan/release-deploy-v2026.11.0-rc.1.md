# v2026.11.0-rc.1 生产部署记录（2026-11-19）

A 档最小生产版的发布执行记录。前置：`release-readiness-v2026.11.0-rc.1.md`（闸门矩阵与 R-1…R-4）。

## 目标环境

- 阿里云 ECS `i-0jlh5iv8xb89sals283z`（8.130.126.29，乌兰察布，Ubuntu 26.04 x86_64），compose 工程 `/opt/opskeeper`（`docker-compose.json`）。
- 访问通道：阿里云 Workbench CLI（云助手，免 SSH 端口）。注意：单条 exec 长命令会被连接中断（exit 5 NetworkError），长任务须 `setsid nohup` 脱离会话运行，外部只做短轮询。
- 变更前状态：主容器 `opskeeper:ab9e94f-prefile-fix2-1.0.79-amd64`（id `e469517c578c`），healthz 200。

## 产物与传输

- 镜像在本机（Apple Silicon）`docker buildx --platform linux/amd64` 构建，仓库 Dockerfile 零改动；`opskeeper:v2026.11.0-rc.1`（id `abbe0ee3fcee`）。
- 导出 `opskeeper-v2026.11.0-rc.1-amd64.tar.gz`（138,508,619 B），sha256 `cb39fe5be9a25cc309429e42ee80d029fd973b2ff87f673e932a5cbf64deb69c`。
- 传输通道：目标机可达 github.com release 资产域（Workbench OSS 中继与 Docker Hub 均不通）。132MB 单资产上传/下载均超时，改为 17×8MB 分片；目标机侧拼接后 `sha256sum -c` 通过再 `docker load`。临时载体 release 与 tag 已在部署完成后删除。

## 部署时间线（目标机本地时间）

1. 18:02 前 分片下载 + 拼接 + sha256 校验 + `docker load`（断点续传，脚本 `/tmp/deploy-part1.sh`）。
2. 18:02 备份 `docker-compose.json.bak-20261119-180231`；换镜像行 → `up -d opskeeper` → **crash-loop**（见下）。
3. 18:15 处置后在 compose 显式设 `OPSKEEPER_VECTOR_BACKEND=qdrant` → 重建 → healthz 200。
4. 18:17 验证（版本回读 / inventory）。
5. 18:2x 回滚演练。

## 部署中发现并处置的缺陷

1. **vecstore 默认后端在生产容器内不可写（crash-loop）**。新版本引入向量存储注册表，`OPSKEEPER_VECTOR_BACKEND` 未设时默认 `chromem`，持久化目录 `./data/chromem` 相对 cwd `/`，nonroot 无法创建 → `os.Exit(1)` 循环重启。处置：生产 compose 显式 `OPSKEEPER_VECTOR_BACKEND=qdrant`（栈内已有 qdrant 服务且既有知识库向量在 qdrant，非新建空库）。**代码侧两个 follow-up**：`core/floor/config/config.go` 的注释说默认 chromem 而 `cmd/opskeeper/main.go` 的注释说默认 qdrant，两处与实际行为（chromem）不一致；容器默认路径 `./data/chromem` 对 nonroot 镜像永远不可写，应在镜像内提供可写默认或在 Dockerfile/compose 预置 env。
2. **`deploy/Dockerfile.opskeeper` 模块暂存清单缺 `core/faults`**（单行 replace 指令，闸门解析器只认 `replace (...)` 块）→ 镜像构建死于 `go mod download`。已修复并合并（PR #125，`0dccf99a`），闸门测试同步补齐。`deploy/Dockerfile.opskeeper-edge` 存在同型缺口，不在本档范围。

## 验证证据

- `GET /healthz` → 200（body `ok`）。
- `GET /api/v1/version`（管理员 JWT，10 分钟 TTL，用 compose 内 secret 现场签发、不落盘）→ `{"manager_version":"v2026.11.0-rc.1"}`。
- `GET /api/v1/runtimes` → 200，4 项：chat_worker / agentteams_worker / plugin_manager / pool_preview。
- 启动日志 `knowledge: vector store enabled backend=qdrant url=http://qdrant:6333`；数据迁移 25/25 通过（启动日志）；`bootstrap admin skipped; users table non-empty`（未改动既有用户）。

## 回滚演练（上一版本回滚）

切回 `opskeeper:ab9e94f-prefile-fix2-1.0.79-amd64` → healthz 200 → 再切回 `opskeeper:v2026.11.0-rc.1` → healthz 200。终态：rc.1 运行中，旧镜像保留在机（可随时回切），compose 备份 `docker-compose.json.bak-20261119-180231` 在 `/opt/opskeeper/`。

## 回滚触发条件（沿用 readiness 报告）

healthz 非 200 超过 5 分钟、核心 API 5xx、DB 迁移不兼容征兆 → `sed` 换回旧镜像行 + `docker compose up -d opskeeper`（全程 <1 分钟）。

## 观察窗口

部署后 48 小时（至 2026-11-21 18:00）：关注 `docker logs opskeeper`、9100 /metrics、知识库检索与 runtime 页面。注意生产 embedder 自 9 月起因上游 403 处于禁用态（readiness R-3），本次无变化。

## 遗留（正式版前）

- GitHub Release 归档仍未生成：tag 的 release workflow 死于 `make fetch-embedding-model`（GCS 403），需 CN 镜像或改为随仓库分发模型。
- artifact 签名流程（6.2 尾项）。
- 4 个 frontier E2E 用例 UNVERIFIED（R-2）。
- ONNX Runtime amd64 供应镜像 sha256 与官方 pin 不一致（镜像源字节差异，内容已验证为 1.20.1），pin 对账待定。

# OpsKeeper Plugins

OpsKeeper 插件开源首发仓库，先发布 AgentTeams 集成所需的两个插件：

- **AgentTeams Plugin Installer**：AgentTeams Dashboard 插件，提供插件上传、启用、停用、同步和推送 Worker 的控制台能力。
- **OpsKeeper TeamHarness**：QwenPaw/AgentTeams Worker 插件，提供 7 个运维角色 Skill、Manager 协同提示词、stdio MCP 代理和 fail-closed 工具边界。

本仓库只包含插件层，不包含 OpsKeeper 后端、Dashboard 和私有部署材料。插件通过 HTTP API 与既有 OpsKeeper 后端协作，可独立构建、校验和安装。

## 构建要求

- Node.js 20+
- npm 10+
- Python 3.11+
- Ruby 3.2+（标准库 `yaml`）
- zip / tar / shasum

测试依赖：

```bash
python3 -m pip install -r scripts/requirements-test.txt
```

## 一键构建

```bash
make build
```

生成产物：

- `plugins/agentteams-plugin-installer/dist/agentteams-plugin-installer.zip`
- `plugins/opskeeper-teamharness/dist/opskeeper-teamharness-qwenpaw-*.zip`
- `plugins/opskeeper-teamharness/dist/opskeeper-teamharness-*-plugin-manager.tar.gz`
- `plugins/opskeeper-teamharness/dist/SHA256SUMS.txt`（执行 `make validate-release` 后生成）

## 验证

```bash
make verify
```

验证包括：

- Installer manifest、入口 JS 和 zip 结构；
- TeamHarness `plugin.json` / `plugin.yaml` 版本一致性；
- QwenPaw 必需入口、MCP 代理和审计代码；
- zip-slip、生成目录、文件数和解压体积保护；
- 插件 Python 测试与 Installer standalone 自检。

## 安装顺序

### 1. 安装 Plugin Installer

1. 打开 AgentTeams Dashboard。
2. 进入 `Settings → Plugins`。
3. 上传 `agentteams-plugin-installer.zip`。
4. 激活后进入侧边栏 `Plugin 管理`。

### 2. 安装 TeamHarness

在 `Plugin 管理` 中上传 `opskeeper-teamharness-qwenpaw-*.zip`，该包同时满足：

- OpsKeeper Plugin Manager 对 `plugin.yaml` 的解析要求；
- QwenPaw Worker 对顶层目录 `plugin.json` 的安装要求。

安装时可启用 `auto_push`，Plugin Manager 会把原始 zip 推送到 Worker 的 `/api/opskeeper-teamharness/install-plugin` 端点并执行 `qwenpaw plugin install --force`。

也可以在 Worker 侧直接安装：

```bash
bash plugins/opskeeper-teamharness/adapters/qwenpaw/install.sh
```

## 运行时配置

TeamHarness MCP 代理常用配置：

```bash
export OPSKEEPER_BACKEND_URL="https://opskeeper.example.com"
export OPSKEEPER_TENANT_ID="default"
export OPSKEEPER_PERMISSION_MODE="readonly"
```

`OPSKEEPER_PERMISSION_MODE=readonly` 为默认安全边界；如需启用修复类工具，必须在受控环境中显式切换为 `standard` 并配合审批策略。

## 致谢

本仓库是 OpsKeeper Plugins 的独立开源发布，发布范围见 [RELEASE_VERSION.json](RELEASE_VERSION.json)。

感谢 **GoAI AgentTeams** 与 **AgentTeams Dashboard** 在多 Agent 协同、插件扩展和可观测交互设计上的启发，也感谢 **OnGrid** 在早期运维场景与可审计执行思路上给予的帮助。OpsKeeper 是独立演进的产品；上述致谢不代表相关项目对 OpsKeeper 的运营、维护或背书。

## License

本仓库按 Apache License 2.0 分发。商标和非背书声明见 [LICENSE](LICENSE)、[NOTICE.md](NOTICE.md)、[TRADEMARK.md](TRADEMARK.md) 和 [docs/ACKNOWLEDGMENTS.md](docs/ACKNOWLEDGMENTS.md)。

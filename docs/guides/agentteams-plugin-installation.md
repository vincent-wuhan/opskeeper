# AgentTeams × OpsKeeper 插件安装指南

> 适用版本：OpsKeeper TeamHarness `1.0.4`、AgentTeams Dashboard `v1.2.4`。  
> 原则：不修改 AgentTeams Controller / Manager / Dashboard 源码，OpsKeeper 只通过 Dashboard 插件与 plugin-manager 扩展协同能力。

## 1. 包类型与职责

| 包 | 构建产物 | 上传位置 | 用途 |
| --- | --- | --- | --- |
| Dashboard Installer | `plugins/agentteams-plugin-installer/dist/agentteams-plugin-installer.zip` | Dashboard → 设置 → 插件 → 上传插件包 | 给 Dashboard 增加「Plugin 管理」页面，用于安装、启停、Push、Sync OpsKeeper 插件 |
| TeamHarness base 包 | `plugins/opskeeper-teamharness/dist/opskeeper-teamharness-1.0.4-plugin-manager.tar.gz` | Dashboard 侧边栏 → Plugin 管理 → 上传 zip / tar.gz | plugin-manager 的标准输入；Push 时自动转换成 QwenPaw 包并推送到 Worker |
| QwenPaw 诊断包 | `plugins/opskeeper-teamharness/dist/opskeeper-teamharness-qwenpaw-1.0.4.zip` | 不上传到 Dashboard 官方插件入口 | 手工验证 QwenPaw 包结构或排查 Worker 安装问题时使用 |

不要把 TeamHarness 包上传到 Dashboard 的「设置 → 插件」官方上传入口。该入口只接受 Dashboard UI 插件，要求 `manifest.entry.dashboard`；TeamHarness 是 Worker 运行时插件，出现以下报错属于包型用错，而不是包损坏：

```text
插件 opskeeper-teamharness: manifest.entry.dashboard 缺失
```

## 2. 构建 Dashboard Installer

```bash
cd plugins/agentteams-plugin-installer
make clean
make plugin-zip
make self-check
```

成功标准：

- `make self-check` 全部通过；
- zip 根目录包含 `plugin.json` 与 `main.js`；
- `plugin.json` 的 `entry.dashboard` 指向 `main.js`。

## 3. 构建 TeamHarness 包

```bash
cd plugins/opskeeper-teamharness
bash scripts/build-package.sh
```

成功标准：

- `dist/opskeeper-teamharness-1.0.4-plugin-manager.tar.gz` 与 `dist/opskeeper-teamharness-qwenpaw-1.0.4.zip` 均已生成；
- tar 包根目录存在 `plugin.yaml`；
- `adapters/qwenpaw/plugin.json`、`adapters/qwenpaw/plugin.py`、`adapters/qwenpaw/task_trace.py` 完整；
- `mcp/server.py` 完整；
- tar 包内不包含 `node_modules`、`__pycache__`、`.DS_Store`；
- `plugin.yaml` 的 `metadata.name=opskeeper-teamharness` 且 `metadata.version=1.0.4`。

建议发布前记录哈希：

```bash
shasum -a 256 \
  plugins/opskeeper-teamharness/dist/opskeeper-teamharness-1.0.4-plugin-manager.tar.gz \
  plugins/opskeeper-teamharness/dist/opskeeper-teamharness-qwenpaw-1.0.4.zip
```

## 4. Dashboard Installer 安装

1. 打开 Dashboard：`设置 → 插件 → 上传插件包`。
2. 选择 `plugins/agentteams-plugin-installer/dist/agentteams-plugin-installer.zip`。
3. 若上传后提示 `/plugins/agentteams-plugin-installer/plugin.json` 返回 404，先确认服务端文件已解压，再仅重启 Dashboard 容器。AgentTeams Dashboard `v1.2.4` 的生产进程不会即时发现新增的 `public/plugins` 静态文件，重启后即可加载。
4. 刷新页面，侧边栏出现「Plugin 管理」表示安装成功。

只允许重启 Dashboard，不改 Dashboard 镜像或源码。

## 5. plugin-manager 路由

「Plugin 管理」页面调用 `/api/v1/plugins/*`。官方 Dashboard `v1.2.4` 不内置该转发路由，部署时需要在同一入口 Nginx 上配置：

```nginx
location ^~ /api/v1/plugins {
  proxy_pass http://127.0.0.1:18096;
  proxy_set_header Authorization "Bearer <PLUGIN_MANAGER_SA_TOKEN>";
  client_max_body_size 64m;
}

location ^~ /plugins/ {
  proxy_pass http://127.0.0.1:13000;
}
```

安全要求：

- plugin-manager 只绑定本机回环地址，由 Nginx 统一转发；
- Bearer token 只保存在 Nginx 服务器侧配置中，不进入前端插件包或浏览器；
- Nginx 配置文件权限建议为 `0600`；
- token 泄露后立即轮换。

## 6. 安装 TeamHarness

1. 打开 Dashboard 侧边栏「Plugin 管理」。
2. 点击「上传 zip / tar.gz」。
3. 选择 `plugins/opskeeper-teamharness/dist/opskeeper-teamharness-1.0.4-plugin-manager.tar.gz`。
4. 安装成功后，在列表中确认 `opskeeper-teamharness` 版本为 `1.0.4`。
5. 执行 Push，将 base 包转换成 QwenPaw 包并推送到 Worker。
6. 执行 Sync，触发 Worker 侧插件配置热加载。

成功标准：

- 插件行来源显示包含 `manager` 与 `worker`；
- 同步状态为 `in_sync` 或等价状态；
- Worker 插件列表中 `opskeeper-teamharness` 为 loaded / enabled；
- OpsKeeper 相关 MCP 工具可通过 Worker 调用。

## 7. 常见问题

| 现象 | 判断 | 处理 |
| --- | --- | --- |
| Dashboard 上传 Installer 后 `plugin.json` 404 | 解压成功，但 Dashboard 生产进程未加载新增静态文件 | 仅重启 Dashboard，再刷新页面 |
| TeamHarness 报 `manifest.entry.dashboard` 缺失 | 把 Worker 运行时插件传到了 Dashboard 官方插件入口 | 改在「Plugin 管理」上传 `.tar.gz` base 包 |
| Plugin 管理页面返回 HTML 404 | Dashboard 或 Nginx 未转发 `/api/v1/plugins/*` | 检查 Nginx location 与 plugin-manager 健康状态 |
| Push 失败 | Worker 地址、认证或包转换失败 | 查看 plugin-manager 操作日志，先验证 `/healthz` 与 Worker roster |
| Sync 后 Worker 未加载 | Worker 侧热加载失败 | 查看 Worker 插件日志，确认 QwenPaw 包 `plugin.json` 与 `plugin.py` 存在 |

# opskeeper-teamharness QwenPaw Adapter

把 runtime-中立的 opskeeper-teamharness base 包转成 QwenPaw-native 包并安装。

## Build

```bash
ruby adapters/qwenpaw/scripts/build-qwenpaw-plugin.rb plugin.yaml
```

生成 `dist/opskeeper-teamharness-qwenpaw-1.0.5.zip`，结构：

```
opskeeper-teamharness-qwenpaw-1.0.0/
├── plugin.json                 # qwenpaw-native manifest
├── plugin.py                   # qwenpaw entry (sanitizer + audit hook)
├── task_trace.py               # task lifecycle tracking
└── opskeeper-teamharness/
    ├── plugin.yaml             # base manifest (apiVersion: agentteams.agentteam/v1alpha1)
    ├── prompts/                # team/agent/manager prompt overrides
    ├── skills/
    │   ├── agent/opskeeper-{alerter,investigator,critic,reviewer,repairer,verifier}/SKILL.md
    │   └── team/opskeeper-coordination/SKILL.md
    ├── mcp/
    │   ├── server.py           # stdio MCP proxy → opskeeper HTTP /v1/mcp
    │   ├── tools.py            # 23 tools catalog
    │   └── auth.py             # Bearer + signature injection
    └── qwenpaw-skills/         # public skills (qwenpaw runtime 加载)
```

## Install

```bash
bash adapters/qwenpaw/install.sh
```

自动 build → unzip → `qwenpaw plugin install <dir> --force`。

## 只读强制

`plugin.py` 注册 priority 10 的 `on_acting` 中间件，位于脱敏与审计层外层。默认 `OPSKEEPER_PERMISSION_MODE=read_only` 时，未进入显式只读白名单的工具不会调用真实执行器，而是返回 `ToolResponse(state=DENIED)`。只有运行时管理员显式设置 `OPSKEEPER_PERMISSION_MODE=standard` 才能放开变更工具。

## Manager 标记等待

任务派发正文必须包含 `OPSKEEPER TASK <task_id>`；插件在 QwenPaw `PRE_EXECUTE`
阶段等待匹配结果，并跳过所有无结果续跑。标记只有在 `message` 工具成功发送后
才会同时记录到源房间与目标房间；若 LLM 改写派发正文，middleware 会从较长的
`OPSKEEPER-...` task ID 兜底提取。只有匹配的
`OPSKEEPER_RESULT <task_id>`、显式新任务或管理员人工指令会重新唤醒。Worker
结果行必须在同一行携带完整 `@manager:<server>` 前缀；插件也兼容 runtime 渲染成
`manager` 的前缀。
当 Worker 房间中的结果唤醒 Manager 时，插件会通过 Manager 的 Matrix token 直接向
派发时的原始请求房间发送 `@admin:<server> OPSKEEPER_COMPLETE <task_id>` 摘要；
直接发送失败时才注入提示作为 fallback。完成通知不会被视为新任务，也不会重新
进入派发 gate。
派发成功后，QwenPaw agent stop gate 会立即终止当前 ReAct 回合，避免空回合
继续累积 `NO_REPLY` / doom-loop 计数。
同一回合重复派发与同一 pending task 的重复发送会被拒绝。默认等待 600 秒，
可用 `OPSKEEPER_MANAGER_GATE_TTL_SECONDS` 调整（1–3600 秒）。

> 升级既有 Manager workspace 时，force 安装包只替换插件文件；需要重启 Manager
> 才能确保 `PRE_EXECUTE` continuation hook 重新挂载到已存在的 workspace。

## Validate

```bash
ruby adapters/qwenpaw/scripts/validate-qwenpaw-plugin.rb <unzipped-dir>
```

校验 plugin.json / plugin.py / task_trace.py / asset_dir 完整 + 所有 Python 文件编译通过。

## Uninstall

```bash
bash adapters/qwenpaw/uninstall.sh
```

## Env Vars（Worker 容器注入）

| Env | 来源 | 用途 |
|---|---|---|
| `OPSKEEPER_BACKEND_URL` | opskeeper-v2 Helm values 或 Worker CR spec.env | stdio MCP proxy 目标 |
| `OPSKEEPER_GATEWAY_KEY` | agentteams-controller credentials.go | Bearer 认证 |
| `OPSKEEPER_TENANT_ID` | multi-tenant 部署 | tenant 隔离 |
| `OPSKEEPER_TIMEOUT` | 可选 | HTTP timeout，default 30s |
| `OPSKEEPER_LOG_LEVEL` | 可选 | stdio MCP server log level |
| `OPSKEEPER_ACTOR` | 可选 | audit 字段，default `qwenpaw-worker` |
| `OPSKEEPER_MANAGER_GATE_TTL_SECONDS` | 可选 | Manager 标记等待 TTL，default 600 |

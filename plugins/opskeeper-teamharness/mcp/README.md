# opskeeper-teamharness stdio MCP Server

## 启动

```bash
python3 server.py
```

按 JSON-RPC 2.0 over stdio 通信。Worker qwenpaw 通过 `plugin.yaml` mcp.servers 配置自动 spawn。

## 必需环境变量

| Env | 说明 |
|---|---|
| `OPSKEEPER_BACKEND_URL` | opskeeper backend URL（默认 `http://opskeeper:8443`） |
| `OPSKEEPER_GATEWAY_KEY` | Worker qwenpaw 注入的 GatewayKey（Higress consumer 标识） |
| `OPSKEEPER_TENANT_ID` | tenant_id（默认 `default`） |
| `OPSKEEPER_TIMEOUT` | HTTP timeout 秒（默认 `30`） |
| `OPSKEEPER_LOG_LEVEL` | 日志级别（默认 `INFO`，写 stderr） |

## 支持的 JSON-RPC 方法

- `initialize` — MCP 协议握手
- `tools/list` — 返回工具目录
- `tools/call` — 调用 opskeeper 工具

## 安全

- 所有请求 HMAC-SHA256 签名（v1）
- Bearer GatewayKey 不在 log 中明文
- 工具调用审计自动写入 opskeeper `/v1/audit/events`

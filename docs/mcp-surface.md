# MCP 对外协议面

OpsKeeper 把**平台自己的**运维工具以 MCP（Model Context Protocol）暴露出去，
让云厂商已经 MCP 化的 Agent 直接消费，而不需要为 OpsKeeper 写一套私有协议。
这条路的方向是**单向的**：OpsKeeper 对外**是** MCP server；对内它**不是**公网
MCP 的代理——需要外部系统的能力时，走的是平台上登记、审核过的那些 MCP server
（`/api/v1/mcp/servers`，HLD-018），而不是让一个外部客户端借这个端点跳到公网。
理由见改造方案：远程 MCP 是一个中心化信任点（论文 2609.19100）。

## 端点与鉴权

| 项 | 值 |
|---|---|
| 方法/路径 | `POST /api/v1/mcp`（JSON-RPC 2.0，单条请求、单条应答） |
| 内容类型 | 请求 `application/json`；应答固定 `application/json` |
| 鉴权 | `Authorization: Bearer <token>`；token 是 Higress 登记的 consumer API key，或平台签发的服务令牌（三段 JWT） |
| 租户 | `X-Opskeeper-Tenant`（可选，缺省 `default`；与 token 的租户不一致会被拒） |
| 完整性（可选，默认要求） | `X-Opskeeper-Timestamp`（秒）+ `X-Opskeeper-Body-SHA256` + `X-Opskeeper-Signature`＝`HMAC-SHA256(token, ts + "." + bodySHA256)`；`OPSKEEPER_REQUIRE_SIGNATURE=0` 关闭 |
| 版本标记（可选） | `X-Opskeeper-Version: v1`。这是本集群自己的发布标记，**不是 MCP 的一部分**：第三方客户端不发也照常工作（不发＝按 v1 处理），发了一个别的值才会被 400 拒绝 |
| 会话 | `initialize` 应答带 `Mcp-Session-Id`；客户端可以在后续请求里回带，服务端不依赖它 |

## 支持的方法

| 方法 | 应答 | 说明 |
|---|---|---|
| `initialize` | `protocolVersion` / `capabilities.tools` / `serverInfo` / `instructions` | 协议版本**按客户端说的回**：客户端要 `2024-11-05`（`pkg/mcpclient` 声明的就是它）就回 `2024-11-05`，要一个本端点没有的就回本端点最新的 |
| `notifications/initialized` | `202`，无 body | 通知不带 id、不应答；凡以 `notifications/` 开头的一律同样处理，新版本的通知不会被打成「坏请求」 |
| `ping` | `{}` | 规范里的保活工具，两边都可以发 |
| `tools/list` | `{tools: [...], nextCursor?}` | 按调用者身份过滤后分页，页大小 200；`nextCursor` 只在还有下一页时出现，`cursor` 解析失败是 `-32602` 而不是静默回到第一页 |
| `tools/call` | `{content: [...], isError?}` | 每次调用写审计链，应答头带 `X-Opskeeper-Audit-ID`，content 里也内嵌同一个 id |
| 其他 | `-32601` | 本端点只声明 `tools` 能力，所以 `resources/list` / `prompts/list` 得到 method-not-found 是**正确**行为，不是缺实现 |

## 工具可见性

`tools/list` 返回的集合是「平台此刻拥有的工具」按调用者过滤后的结果：

- 来源：`Registry.BuildBaseTools()` + `AppendHostFilesTools` + loop 适配器。也就是
  `/v1/skills` 与流程编排看到的同一份清单，**在 main.go 的接线末尾组装**——因为
  `cloud_bash` / `send_im_message` / `serve_page` 这些工具要在审批流接上之后才
  进入注册表（见 §4.46 与 `core/manager/biz/aiops/tools/registry_late_deps_test.go`）。
- 不含的东西是**有意**的：`mcp_call` 与启动期从外部 MCP server 发现的那批工具不在
  这里。`mcp_call` 是本平台的 MCP **客户端**，把它暴露出去就等于让这个端点变成公网
  MCP 的代理——正是这个端点明确不做的那件事。
- 过滤规则：`tools/list` 与 `tools/call` 走同一套判断——工具类别（read / write /
  destructive）映射到 Casbin 动作，Worker 身份走 `MCPAuthorizer`。**看不到的工具
  也调不动**，因为 `tools/call` 会重新判一次。

## 边界（不因为走 MCP 而放松）

- 写操作与破坏性操作（`cloud_bash`、`restart_service`、`recovery.execute` …）在**工具
  内部**排队等人工批准。经 MCP 进来不绕过那条队列，也没有第二条放行路径。
- `initialize` 的 `instructions` 字段会把这些边界原样告诉客户端，客户端不必先猜。
- 审计链在平台侧写，MCP 调用者只能读它自己那份 receipt，不能写。

## 最小客户端

```bash
curl -sS -X POST "$OPSKEEPER/api/v1/mcp" \
  -H "Authorization: Bearer $MCP_TOKEN" \
  -H "X-Opskeeper-Tenant: $TENANT" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"claude-desktop","version":"1"}}}'

curl -sS -X POST "$OPSKEEPER/api/v1/mcp" \
  -H "Authorization: Bearer $MCP_TOKEN" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

Go 客户端直接用本仓库的 `core/manager/pkg/mcpclient`（`Initialize` → `ListTools` →
`CallTool`）；`TestOurOwnClientCanDriveOurOwnServer` 就是这条路径的端到端证明。

## 已知偏差

| 偏差 | 为什么 |
|---|---|
| 应答只有 `application/json`，不实现 `text/event-stream` | 工具调用是请求/应答式的，没有增量可流；客户端按规范两样都得接受 |
| `Mcp-Session-Id` 签发但不强制回带 | 单进程无会话状态，强制它只会让客户端多一步 |
| 未实现 `resources` / `prompts` / `logging` / `completion` 能力 | 能力集在 `initialize` 里如实声明了只有 `tools`；声明了却不实现才是问题 |
| 工具调用没有取消语义 | `notifications/cancelled` 被接受但不产生效果；要真取消得让工具本身可中断，那是工具侧的事 |

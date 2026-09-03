# scripts/

运维脚本目录。所有脚本遵循 [gospec](https://github.com/singchia/gospec)：
`set -euo pipefail`、锁/信号/PID 友好、参数可观测。

| 脚本 | 用途 |
|------|------|
| `manage-state.sh` | 透传调用 `cmd/agentteams-state`，可在 CI 里用 `AGENTTEAMS_STATE_BIN` 切换到预编译二进制 |
| `sync-builtin-vault.sh` | 重新打包内置知识库到二进制里 |
| `validate-agentteams-protocols.rb` | 用 JSON Schema 校验 `openspec/.../protocols/*.yaml` 与 examples |
| `i18n-lint.mjs` | 扫描 `web/src` 里 `tr()` 的中英完整性 |
| `opskeeper-hitl-monitor.sh` | 长驻守护进程：循环拉 `expire-hitl --loop`，超时 HITL 升级通知（详见下文） |

## opskeeper-hitl-monitor.sh

监控 AgentTeams state.json 中已超过 `--timeout` 的 HITL 请求，
每轮把它们打成 `timeout_escalated` 决策并 POST 到升级 webhook。
逻辑委托给 `go run ./cmd/agentteams-state expire-hitl --loop`，
本脚本负责：

- 持有进程间互斥锁（`mkdir` 锁，macOS/Linux 兼容），同一时间只能跑一个实例
- 写 PID 到 `--pidfile`，崩溃残留 PID 由下次启动的锁 stale 检测清理
- 捕获 `SIGTERM` / `SIGINT`：先 SIGTERM 子进程留 2 秒退避，再 SIGKILL 兜底；最后删 pidfile + 释放锁
- 监听 state.json mtime，每次变化输出一行 `[monitor] iteration=N started=… escalations=…`

### 用法

```bash
scripts/opskeeper-hitl-monitor.sh \
  --interval 1m \
  --timeout 15m \
  --state /var/lib/agentteams/state.json \
  --escalation-url https://hooks.example.com/hitl \
  --pidfile /var/run/opskeeper-hitl-monitor.pid \
  --max-iterations 0          # 0 = 永远运行
```

### 参数

| Flag | 默认 | 含义 |
|------|------|------|
| `--interval DURATION` | `1m` | `expire-hitl --loop` 的轮询间隔（如 `30s` / `5m`） |
| `--timeout DURATION` | `15m` | HITL 超时阈值，传给 `expire-hitl` |
| `--state PATH` | `state.json` | `state.json` 绝对/相对路径（相对时按 repo root 解析） |
| `--escalation-url URL` | （必填） | 升级通知 webhook |
| `--opskeeper-bin PATH` | `go run ./cmd/agentteams-state` | 用预编译二进制替代 `go run`，缩短启动延迟 |
| `--max-iterations N` | `0` | `expire-hitl --loop` 跑 N 轮后退出，`0` = 无限 |
| `--pidfile PATH` | （必填） | 写入自身 PID 的文件，SIGTERM 时由脚本清理 |
| `--lock-dir PATH` | `/tmp/opskeeper-hitl-monitor.lock` | 互斥锁目录（用 `mkdir` 而非 `flock`，macOS 无 `flock`） |
| `-h` / `--help` | — | 打印帮助 |

### 信号

| 信号 | 行为 |
|------|------|
| `SIGTERM` | 立即转发给 `expire-hitl` 子进程并等待 ≤ 2s；超时后 SIGKILL 兜底；最后清理 pidfile + 锁；退出码 `143` |
| `SIGINT` | 同上，退出码 `130` |
| `EXIT` | 兜底：清理 pidfile / 锁（无论子进程死活都执行一次） |

幂等：连续两次 SIGTERM 不会破坏状态（清理逻辑只删自己 PID 对应的 pidfile/锁）。

### 日志格式

子进程（`expire-hitl`）的 stdout/stderr 写到 `$TMPDIR/opskeeper-hitl-monitor.XXXXXX.log`，
`expire-hitl` 退出后打印其最后 20 行以便排查。

监控自身输出（stdout，每行一个 JSON-line 风格的 key=value）：

```
[monitor] starting expire-hitl --loop interval=1m timeout=15m max-iterations=0 state=/abs/state.json
[monitor] iteration=1 started=2026-08-21T10:00:00Z escalations=0
[monitor] iteration=2 started=2026-08-21T10:01:00Z escalations=1
[monitor] expire-hitl exited with code=0; last 20 lines of log: …
```

字段含义：

- `iteration`：从 `state.json` mtime 首次变化开始递增的轮次
- `started`：本次迭代开始时间（UTC ISO-8601）
- `escalations`：当前 `state.json` 中已完成（`completed_at` 已写入）的 escalation 计数

### systemd / supervisor 部署示例

```ini
[Unit]
Description=Opskeeper HITL escalation monitor
After=network-online.target

[Service]
Type=simple
User=opskeeper
ExecStart=/opt/opskeeper/scripts/opskeeper-hitl-monitor.sh \
  --interval 1m --timeout 15m \
  --state /var/lib/agentteams/state.json \
  --escalation-url https://hooks.example.com/hitl \
  --pidfile /run/opskeeper-hitl-monitor.pid
Restart=on-failure
RestartSec=10
KillSignal=TERM
TimeoutStopSec=15
```

### 测试

`tests/check-opskeeper-hitl-monitor.sh` 端到端覆盖：

1. 准备临时 `state.json` + 本地 `python3 -m`-等价的 HTTP server
2. 用 `manage-state.sh` 走完 init → advance(×4) → start-hitl
3. 后台启动 monitor，`--timeout 1ns` 让 HITL 立刻超时
4. 断言：HTTP server 收到 ≥1 条 escalation 通知、pidfile 已生成、锁目录存在
6. 发 SIGTERM 给 monitor，断言：进程退出、pidfile 删除、锁目录清理

无 `python3` 环境会跳过 webhook 验证，但 pidfile + SIGTERM 行为仍会被断言。

```bash
bash tests/check-opskeeper-hitl-monitor.sh
```

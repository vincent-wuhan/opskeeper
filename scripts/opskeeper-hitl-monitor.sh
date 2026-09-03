#!/usr/bin/env bash
# scripts/opskeeper-hitl-monitor.sh — long-running daemon that polls
# agentteams-state's expire-hitl loop for timed-out HITL approvals.
#
# 设计要点（参见 scripts/README.md）:
#   - 默认通过 `go run ./cmd/agentteams-state` 调用 expire-hitl --loop
#   - 任何时刻只允许一个 monitor 实例运行（mkdir 锁）
#   - PID 写到 --pidfile；SIGTERM/SIGINT 时清理子进程和 pidfile
#   - 监听 state.json 的修改时间，把每次循环的开始时间与当时
#     state.json 中已完成的 escalation 计数输出到 stdout
#
# 用法：
#   scripts/opskeeper-hitl-monitor.sh \
#     --interval 1m --timeout 15m \
#     --state state.json \
#     --escalation-url https://hooks.example.com/hitl \
#     --pidfile /var/run/opskeeper-hitl-monitor.pid \
#     --max-iterations 0
set -euo pipefail

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repository_root="$(dirname "$script_directory")"

# ----- 参数默认值 / 解析 -----------------------------------------------------
INTERVAL="1m"
TIMEOUT="15m"
STATE_PATH="state.json"
ESCALATION_URL=""
OPSKEEPER_BIN=""
MAX_ITERATIONS=0
PIDFILE=""
LOCK_DIR="/tmp/opskeeper-hitl-monitor.lock"
WATCH_INTERVAL_SECONDS=1

usage() {
  cat <<'USAGE'
Usage: opskeeper-hitl-monitor.sh [flags]

Flags:
  --interval DURATION         轮询间隔（默认 1m，如 30s / 5m）
  --timeout DURATION          HITL 超时阈值（默认 15m）
  --state PATH                state.json 路径（默认 ./state.json）
  --escalation-url URL        升级通知 webhook（必填）
  --opskeeper-bin PATH        自定义 agentteams-state 二进制；
                              未设置时使用 `go run ./cmd/agentteams-state`
  --max-iterations N          跑 N 轮后退出（0=无限，默认 0）
  --pidfile PATH              写入自身 PID 的文件（必填）
  --lock-dir PATH             互斥锁目录（默认 /tmp/opskeeper-hitl-monitor.lock）
  -h, --help                  显示本帮助
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --interval) INTERVAL="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    --state) STATE_PATH="$2"; shift 2 ;;
    --escalation-url) ESCALATION_URL="$2"; shift 2 ;;
    --opskeeper-bin) OPSKEEPER_BIN="$2"; shift 2 ;;
    --max-iterations) MAX_ITERATIONS="$2"; shift 2 ;;
    --pidfile) PIDFILE="$2"; shift 2 ;;
    --lock-dir) LOCK_DIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "opskeeper-hitl-monitor: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "$ESCALATION_URL" ]] || { echo "opskeeper-hitl-monitor: --escalation-url is required" >&2; exit 2; }
[[ -n "$PIDFILE" ]]        || { echo "opskeeper-hitl-monitor: --pidfile is required" >&2; exit 2; }
[[ "$MAX_ITERATIONS" =~ ^[0-9]+$ ]] || { echo "opskeeper-hitl-monitor: --max-iterations must be a non-negative integer" >&2; exit 2; }

# ----- mkdir 互斥锁（macOS 上没有 flock） -------------------------------------
acquire_lock() {
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    printf '%s\n' "$$" > "$LOCK_DIR/pid"
    return 0
  fi
  # 锁目录已存在——如果里面的进程还在跑就直接放弃
  existing_pid=""
  if [[ -f "$LOCK_DIR/pid" ]]; then
    existing_pid="$(cat "$LOCK_DIR/pid" 2>/dev/null || true)"
  fi
  if [[ -n "$existing_pid" ]] && kill -0 "$existing_pid" 2>/dev/null; then
    echo "opskeeper-hitl-monitor: another instance is running (pid $existing_pid, lock=$LOCK_DIR)" >&2
    return 1
  fi
  # 残留锁（上一轮崩溃留下的），清理后重试
  rm -rf "$LOCK_DIR"
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    printf '%s\n' "$$" > "$LOCK_DIR/pid"
    return 0
  fi
  echo "opskeeper-hitl-monitor: failed to acquire lock $LOCK_DIR" >&2
  return 1
}

release_lock() {
  if [[ -f "$LOCK_DIR/pid" ]]; then
    existing_pid="$(cat "$LOCK_DIR/pid" 2>/dev/null || true)"
    if [[ "$existing_pid" == "$$" ]]; then
      rm -rf "$LOCK_DIR"
    fi
  fi
}

if ! acquire_lock; then
  exit 1
fi

# ----- pidfile + 子进程清理 --------------------------------------------------
mkdir -p "$(dirname "$PIDFILE")"
printf '%s\n' "$$" > "$PIDFILE"

CHILD_PID=""
MONITOR_DONE=""

kill_child() {
  if [[ -z "$CHILD_PID" ]] || ! kill -0 "$CHILD_PID" 2>/dev/null; then
    return 0
  fi
  # 先 SIGTERM 让 expire-hitl 优雅退出，兜底 SIGKILL
  kill -TERM "$CHILD_PID" 2>/dev/null || true
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$CHILD_PID" 2>/dev/null || break
    sleep 0.2
  done
  kill -KILL "$CHILD_PID" 2>/dev/null || true
  wait "$CHILD_PID" 2>/dev/null || true
  CHILD_PID=""
}

cleanup() {
  local exit_code=${1:-0}
  kill_child
  if [[ -f "$PIDFILE" ]]; then
    local existing_pid
    existing_pid="$(cat "$PIDFILE" 2>/dev/null || true)"
    if [[ "$existing_pid" == "$$" ]]; then
      rm -f "$PIDFILE"
    fi
  fi
  release_lock
  if [[ -n "${MONITOR_DONE:-}" ]]; then
    exit "$exit_code"
  fi
  MONITOR_DONE="1"
  exit "$exit_code"
}

# 注意：先注册 EXIT 兜底，再注册 TERM/INT 主动清理
trap 'cleanup $?' EXIT
trap 'cleanup 143' TERM
trap 'cleanup 130' INT

# ----- 计算当前已完成的 escalation 数量（从 state.json 推断） -----------------
# agentteams-state 把每次完成的 escalation 记到 ledger 里。
# 我们用最稳的标记: "completed_at" 字段数 = 完成次数。
count_escalations() {
  local path="$1"
  if [[ ! -f "$path" ]]; then
    echo 0
    return
  fi
  grep -o '"completed_at"' "$path" 2>/dev/null | wc -l | tr -d ' '
}

state_mtime() {
  local path="$1"
  if [[ ! -f "$path" ]]; then
    echo 0
    return
  fi
  # macOS / Linux 双兼容
  stat -c %Y "$path" 2>/dev/null || stat -f %m "$path" 2>/dev/null || echo 0
}

# ----- 选择 expire-hitl 调用的二进制 -----------------------------------------
if [[ -n "$OPSKEEPER_BIN" ]]; then
  EXPIRE_CMD=( "$OPSKEEPER_BIN" expire-hitl )
else
  EXPIRE_CMD=( go run "$repository_root/cmd/agentteams-state" expire-hitl )
fi

# 把 state.json 解析成绝对路径，避免子进程的工作目录变化时找不到
case "$STATE_PATH" in
  /*) STATE_ABS="$STATE_PATH" ;;
  *)  STATE_ABS="$repository_root/$STATE_PATH" ;;
esac

echo "[monitor] starting expire-hitl interval=$INTERVAL timeout=$TIMEOUT max-iterations=$MAX_ITERATIONS state=$STATE_ABS"

iteration=0
while true; do
  if [[ ! -f "$STATE_ABS" ]]; then
    echo "[monitor] state not found; waiting state=$STATE_ABS"
  else
    if ! "${EXPIRE_CMD[@]}" \
      --timeout "$TIMEOUT" \
      --escalation-url "$ESCALATION_URL" \
      --state "$STATE_ABS"; then
      echo "[monitor] expire-hitl failed" >&2
      exit 1
    fi
  fi

  iteration=$((iteration + 1))
  start_time="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  count="$(count_escalations "$STATE_ABS")"
  echo "[monitor] iteration=$iteration started=$start_time escalations=$count"

  if [[ "$MAX_ITERATIONS" -gt 0 && "$iteration" -ge "$MAX_ITERATIONS" ]]; then
    break
  fi
  sleep "$INTERVAL"
done

MONITOR_DONE="1"
exit 0

#!/usr/bin/env bash
# tests/check-opskeeper-hitl-monitor.sh — bash e2e for scripts/opskeeper-hitl-monitor.sh
#
# 用例流程（参见 scripts/README.md）:
#   1. 起一个本地 python3 HTTP server，POST 写入 notify.log
#   2. 用 manage-state.sh 走完 init → advance → start-hitl 准备 state.json
#   3. 后台启动 monitor，--timeout 1ns 让 HITL 立即超时
#   4. 断言：pidfile 已生成、HTTP server 收到 ≥1 条 escalation 通知
#   5. SIGTERM monitor，断言 pidfile 被删除
set -euo pipefail

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repository_root="$(cd "$script_directory/.." && pwd)"
monitor_script="$repository_root/scripts/opskeeper-hitl-monitor.sh"
manage_script="$repository_root/scripts/manage-state.sh"

PASS=0
FAIL=1

assert_eq() {
  local name="$1" actual="$2" expected="$3"
  if [[ "$actual" == "$expected" ]]; then
    echo "  ok  - $name (=$actual)"
  else
    echo "  not ok - $name (actual=$actual expected=$expected)" >&2
    exit "$FAIL"
  fi
}

assert_true() {
  local name="$1" cond="$2" detail="${3:-}"
  if eval "$cond"; then
    echo "  ok  - $name"
  else
    echo "  not ok - $name ${detail:+($detail)}" >&2
    exit "$FAIL"
  fi
}

require_command() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "opskeeper-hitl-monitor tests: required command '$cmd' is not available" >&2
    exit 1
  fi
}

require_command go

# ----- 1. 准备工作目录 + 端口 -------------------------------------------------
TMPDIR="$(mktemp -d -t opskeeper-hitl-monitor.XXXXXX)"
STATE_FILE="$TMPDIR/state.json"
NOTIFY_LOG="$TMPDIR/notifications.log"
SERVER_LOG="$TMPDIR/http.server.log"
MONITOR_LOG="$TMPDIR/monitor.log"
MONITOR_STDERR="$TMPDIR/monitor.stderr"
PIDFILE="$TMPDIR/monitor.pid"
LOCK_DIR="$TMPDIR/monitor.lock"
SERVER_PORT_FILE="$TMPDIR/server.port"

cleanup() {
  local code=$?
  if [[ -n "${MONITOR_PID:-}" ]] && kill -0 "$MONITOR_PID" 2>/dev/null; then
    kill -TERM "$MONITOR_PID" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      kill -0 "$MONITOR_PID" 2>/dev/null || break
      sleep 0.2
    done
    kill -KILL "$MONITOR_PID" 2>/dev/null || true
  fi
  if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
  fi
  if [[ "${KEEP_TMP:-0}" != "1" ]]; then
    rm -rf "$TMPDIR"
  fi
  exit "$code"
}
trap cleanup EXIT INT TERM

# ----- 2. 探测 python3 --------------------------------------------------------
PYTHON_BIN=""
if command -v python3 >/dev/null 2>&1; then
  PYTHON_BIN="$(command -v python3)"
fi

if [[ -z "$PYTHON_BIN" ]]; then
  cat <<'NOTE'
NOTE: python3 not available; skipping server-backed escalation assertions.
  The monitor script itself is still exercised for pidfile/SIGTERM behavior
  using a no-op escalation URL; only the HTTP-side assertions are skipped.
NOTE
  ESCALATION_URL="http://127.0.0.1:1/skip"
  SERVER_PID=""
else
  # 找一个空闲端口
  ESCALATION_PORT="$("$PYTHON_BIN" -c '
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
')"
  ESCALATION_URL="http://127.0.0.1:$ESCALATION_PORT/escalate"
  NOTIFY_URL="http://127.0.0.1:$ESCALATION_PORT/notify"
  printf '%s\n' "$ESCALATION_PORT" > "$SERVER_PORT_FILE"

  cat > "$TMPDIR/http_server.py" <<'PY'
import http.server
import os
import socketserver
import sys

port = int(open(os.environ["PORT_FILE"]).read().strip())
log_path = os.environ["NOTIFY_LOG"]
log = open(log_path, "a", buffering=1)


class Handler(http.server.BaseHTTPRequestHandler):
    def _record(self):
        length = int(self.headers.get("Content-Length", "0") or "0")
        body = self.rfile.read(length).decode("utf-8", errors="replace") if length else ""
        log.write(
            "method={method} path={path} version={version} body={body}\n".format(
                method=self.command,
                path=self.path,
                version=self.headers.get("X-Opskeeper-Version", ""),
                body=body.replace("\n", " "),
            )
        )

    def do_POST(self):
        self._record()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", "9")
        self.end_headers()
        self.wfile.write(b"{\"ok\":1}")

    def do_GET(self):
        self.send_response(204)
        self.end_headers()

    def log_message(self, format, *args):
        return


class ReusableTCPServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True


with ReusableTCPServer(("127.0.0.1", port), Handler) as srv:
    srv.serve_forever()
PY

  PORT_FILE="$SERVER_PORT_FILE" NOTIFY_LOG="$NOTIFY_LOG" \
    "$PYTHON_BIN" "$TMPDIR/http_server.py" >"$SERVER_LOG" 2>&1 &
  SERVER_PID=$!

  # 等 server 上线（最多 5s）
  for _ in $(seq 1 50); do
    if curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$ESCALATION_PORT/" 2>/dev/null | grep -q "204"; then
      break
    fi
    sleep 0.1
  done
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "opskeeper-hitl-monitor tests: failed to start python http server" >&2
    cat "$SERVER_LOG" >&2
    exit 1
  fi
fi

# ----- 3. 准备 state.json ------------------------------------------------------
set +e
"$manage_script" init --state "$STATE_FILE" \
  --task-id "task-monitor-test" \
  --incident-id "incident-monitor-test" \
  --project-id "project-monitor-test" \
  --room-id "!room:matrix.test" >"$TMPDIR/init.log" 2>&1
init_rc=$?
set -e
if [[ $init_rc -ne 0 ]]; then
  echo "opskeeper-hitl-monitor tests: manage-state.sh init failed (rc=$init_rc)" >&2
  cat "$TMPDIR/init.log" >&2
  exit 1
fi

for phase in alert_dedup rca critic_audit review; do
  set +e
  "$manage_script" advance --state "$STATE_FILE" \
    --task-id "task-monitor-test" \
    --phase "$phase" \
    --status completed >"$TMPDIR/advance.$phase.log" 2>&1
  advance_rc=$?
  set -e
  if [[ $advance_rc -ne 0 ]]; then
    echo "opskeeper-hitl-monitor tests: manage-state.sh advance $phase failed (rc=$advance_rc)" >&2
    cat "$TMPDIR/advance.$phase.log" >&2
    exit 1
  fi
done

set +e
"$manage_script" start-hitl --state "$STATE_FILE" \
  --task-id "task-monitor-test" \
  --approval-request-id "A1" \
  --reason "test escalation" \
  --admin "@admin:matrix.test" \
  --notify-url "$NOTIFY_URL" >"$TMPDIR/start-hitl.log" 2>&1
start_hitl_rc=$?
set -e
if [[ $start_hitl_rc -ne 0 ]]; then
  echo "opskeeper-hitl-monitor tests: manage-state.sh start-hitl failed (rc=$start_hitl_rc)" >&2
  cat "$TMPDIR/start-hitl.log" >&2
  exit 1
fi

# ----- 4. 启动 monitor（后台） ------------------------------------------------
"$monitor_script" \
  --interval 50ms \
  --timeout 1ns \
  --state "$STATE_FILE" \
  --escalation-url "$ESCALATION_URL" \
  --pidfile "$PIDFILE" \
  --lock-dir "$LOCK_DIR" \
  --max-iterations 3 \
  >"$MONITOR_LOG" 2>"$MONITOR_STDERR" &
MONITOR_PID=$!

# 等 pidfile 出现（最多 5s）
for _ in $(seq 1 50); do
  if [[ -f "$PIDFILE" ]]; then
    break
  fi
  sleep 0.1
done

assert_true "monitor pidfile created" "[[ -f '$PIDFILE' ]]"
assert_true "monitor process alive" "kill -0 '$MONITOR_PID' 2>/dev/null"

PIDFILE_PID="$(cat "$PIDFILE" 2>/dev/null || echo "")"
assert_eq "pidfile matches monitor pid" "$PIDFILE_PID" "$MONITOR_PID"

# ----- 5. 等 server 收到 escalation 通知（最多 30s） --------------------------
server_got_notification=false
for _ in $(seq 1 100); do
  if [[ -f "$NOTIFY_LOG" ]] && grep -q 'method=POST' "$NOTIFY_LOG" 2>/dev/null; then
    if grep -q 'decision' "$NOTIFY_LOG" 2>/dev/null || grep -q '"approval_request_id"' "$NOTIFY_LOG" 2>/dev/null; then
      server_got_notification=true
      break
    fi
  fi
  if ! kill -0 "$MONITOR_PID" 2>/dev/null; then
    break
  fi
  sleep 0.3
done

if [[ "$server_got_notification" == "true" ]]; then
  echo "  ok  - server received escalation notification"
else
  echo "  not ok - server did NOT receive escalation notification" >&2
  echo "----- monitor stdout -----" >&2
  cat "$MONITOR_LOG" >&2 || true
  echo "----- monitor stderr -----" >&2
  cat "$MONITOR_STDERR" >&2 || true
  echo "----- http server log -----" >&2
  cat "$SERVER_LOG" >&2 || true
  echo "----- notify log -----" >&2
  cat "$NOTIFY_LOG" 2>/dev/null >&2 || true
  exit "$FAIL"
fi

# ----- 6. SIGTERM monitor，验证 graceful shutdown ----------------------------
kill -TERM "$MONITOR_PID" 2>/dev/null || true

# 等 monitor 退出（最多 10s）
for _ in $(seq 1 50); do
  if ! kill -0 "$MONITOR_PID" 2>/dev/null; then
    break
  fi
  sleep 0.2
done

assert_true "monitor exited after SIGTERM" "! kill -0 '$MONITOR_PID' 2>/dev/null"
assert_true "pidfile removed after SIGTERM" "[[ ! -f '$PIDFILE' ]]"
assert_true "lock dir cleaned up" "[[ ! -d '$LOCK_DIR' ]]"

# ----- 7. 二次启动不能拿到同一把锁（可选断言） -------------------------------
if [[ -d "$LOCK_DIR" ]]; then
  echo "  not ok - lock directory still present" >&2
  exit "$FAIL"
fi

echo
echo "----- summary -----"
echo "monitor pidfile (after cleanup): $PIDFILE -> $([ -f "$PIDFILE" ] && echo present || echo absent)"
echo "lock directory  (after cleanup): $LOCK_DIR -> $([ -d "$LOCK_DIR" ] && echo present || echo absent)"
echo "notification log lines: $(wc -l < "$NOTIFY_LOG" 2>/dev/null | tr -d ' ')"
echo "first 3 notifications:"
head -n 3 "$NOTIFY_LOG" 2>/dev/null | sed 's/^/  /'
echo "monitor stdout (last 10 lines):"
tail -n 10 "$MONITOR_LOG" 2>/dev/null | sed 's/^/  /'
echo
echo "PASS: scripts/opskeeper-hitl-monitor.sh + tests/check-opskeeper-hitl-monitor.sh"
exit "$PASS"

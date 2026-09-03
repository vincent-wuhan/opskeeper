#!/usr/bin/env bash
# 完整 Go Manager → real Python mock worker e2e
#
# 启动 Python mock worker (用 fake qwenpaw 模拟 install 成功),
# 然后跑 Go WorkerHTTPClient.InstallPlugin 端到端打过去。
#
# 前提:
#   - python3 + fastapi + uvicorn + python-multipart
#   - go
#   - /tmp/opskeeper-teamharness-docker/opt/agentteams/plugins/opskeeper-teamharness
#     (本仓库 plugins/opskeeper-teamharness 复制过去的目录)
#
# 用法:
#   bash scripts/e2e_real/run.sh
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Step 1: 准备 plugin 路径(若未复制)
PLUGIN_BASE="/tmp/opskeeper-teamharness-docker/opt/agentteams/plugins/opskeeper-teamharness"
if [ ! -f "$PLUGIN_BASE/adapters/qwenpaw/plugin.py" ]; then
    echo "==> Copying plugin to $PLUGIN_BASE ..."
    mkdir -p "$PLUGIN_BASE"
    cp -r "$ROOT/plugins/opskeeper-teamharness/." "$PLUGIN_BASE/"
fi

# Step 2: 准备 entrypoint
ENTRY="/tmp/opskeeper-teamharness-docker/opt/agentteams/bin/worker-entrypoint.py"
mkdir -p "$(dirname "$ENTRY")"
cp "$ROOT/deploy/worker-entrypoint.py" "$ENTRY"

# Step 3: 准备 fake qwenpaw binary (模拟 install 成功)
FAKE_BIN="/tmp/fake-bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/qwenpaw" << 'FAKE'
#!/bin/bash
echo "[fake-qwenpaw] installed package args=$*"
exit 0
FAKE
chmod +x "$FAKE_BIN/qwenpaw"

# Step 4: 启动 Python worker
echo "==> Killing any leftover worker on :8088 ..."
pkill -f worker-entrypoint.py 2>/dev/null || true
sleep 1

echo "==> Starting Python worker (PLUGIN_BASE=$PLUGIN_BASE) ..."
PATH="$FAKE_BIN:$PATH" PLUGIN_BASE="$PLUGIN_BASE" python3 "$ENTRY" >/tmp/e2e_worker.log 2>&1 &
WORKER_PID=$!
sleep 4
if ! curl -sf http://127.0.0.1:8088/health >/dev/null; then
    echo "ERROR: worker not responding on /health"
    tail -20 /tmp/e2e_worker.log
    kill $WORKER_PID 2>/dev/null || true
    exit 1
fi
echo "==> Worker up (PID=$WORKER_PID)"

# Step 5: 跑 Go e2e
echo "==> Running Go WorkerHTTPClient.InstallPlugin ..."
( cd "$ROOT" && go run scripts/e2e_real/manager_install_e2e.go )
GO_STATUS=$?

# Cleanup
kill $WORKER_PID 2>/dev/null || true
wait 2>/dev/null || true

if [ $GO_STATUS -ne 0 ]; then
    echo "==> FAILED — worker log:"
    tail -20 /tmp/e2e_worker.log
    exit $GO_STATUS
fi
echo "==> OK complete e2e"

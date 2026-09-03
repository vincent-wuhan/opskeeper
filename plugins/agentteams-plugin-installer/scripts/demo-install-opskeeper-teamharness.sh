#!/usr/bin/env bash
# End-to-end demo of the Dashboard-upload → worker-qwenpaw-install loop.
#
# 阶段:
#   1. Build agentteams-plugin-installer.zip (Dashboard 端 UI 插件)
#   2. Build opskeeper-teamharness.zip (Worker 端 v1alpha1 plugin)
#   3. 模拟 Higress-authed Dashboard upload (curl POST /v1/plugins/install)
#   4. 模拟 Push 到 worker (curl POST /v1/plugins/{id}/push)
#      (worker 子进程内 qwenpaw plugin install 行为在 production cluster 才执行)
#   5. 验证 GET /v1/plugins 显示已注册 + status=enabled
#
# 不要求 opskeeper 真实运行 — 每步都打印 curl 命令供运维离线 review。
# 真实集群部署时,把所有 OPSKEEPER_URL 替换成实际 Service Host。

set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
PLUGIN_INSTALLER="$ROOT/plugins/agentteams-plugin-installer"
OPSKEEPER_PLUGIN="$ROOT/plugins/opskeeper-teamharness"

OPSKEEPER_URL="${OPSKEEPER_URL:-http://localhost:8080}"
: "${HIGRESS_TOKEN:?HIGRESS_TOKEN is required}"
WORKER_URL="${WORKER_URL:-http://worker-qwenpaw:8088}"  # 实际从 Controller /api/v1/workers 拿

echo "==> 1. Build agentteams-plugin-installer.zip (Dashboard 端 UI 插件)"
"$PLUGIN_INSTALLER/scripts/build-and-zip.sh"

echo
echo "==> 2. Build opskeeper-teamharness.zip (Worker 端 v1alpha1 plugin)"
mkdir -p "$OPSKEEPER_PLUGIN/dist"
(cd "$OPSKEEPER_PLUGIN" && \
  zip -r dist/opskeeper-teamharness.zip plugin.yaml prompts skills mcp adapters scripts loongsuite examples dashboard >/dev/null)
echo "Built opskeeper-teamharness.zip:"
ls -la "$OPSKEEPER_PLUGIN/dist/"

INSTALLER_ZIP="$PLUGIN_INSTALLER/dist/agentteams-plugin-installer.zip"
WORKER_ZIP="$OPSKEEPER_PLUGIN/dist/opskeeper-teamharness.zip"

echo
echo "==> 3. Dashboard upload agentteams-plugin-installer.zip (Dashboard Settings → 插件)"
echo "    真实部署命令:"
echo "    curl -X POST '$OPSKEEPER_URL/api/dashboard/plugins' \\"
echo "         -H 'Cookie: higress_session=$HIGRESS_TOKEN' \\"
echo "         -F 'file=@$INSTALLER_ZIP'"

echo
echo "==> 4. Dashboard upload opskeeper-teamharness.zip 到 opskeeper Manager"
echo "    (经 AgentTeams Plugin 管理 UI → 上传 → 自动调用)"
echo "    真实部署命令:"
echo "    curl -X POST '$OPSKEEPER_URL/v1/plugins/install' \\"
echo "         -H 'Authorization: Bearer $HIGRESS_TOKEN' \\"
echo "         -F 'file=@$WORKER_ZIP'"

echo
echo "==> 5. Dashboard 点 Push → 触发 worker install-plugin 端点"
echo "    (经 AgentTeams Plugin 管理 UI → Push 按钮)"
echo "    真实部署命令:"
echo "    curl -X POST '$OPSKEEPER_URL/v1/plugins/opskeeper-teamharness/push' \\"
echo "         -H 'Authorization: Bearer $HIGRESS_TOKEN'"

echo
echo "==> 6. 验证 GET /v1/plugins/opskeeper-teamharness 显示 status=enabled"
echo "    真实部署命令:"
echo "    curl '$OPSKEEPER_URL/v1/plugins/opskeeper-teamharness' \\"
echo "         -H 'Authorization: Bearer $HIGRESS_TOKEN' | jq .status"

echo
echo "==> 7. 验证 GET /api/opskeeper-teamharness/install-plugin/health 在 worker 端"
echo "    (需知道 worker endpoint,可从 Controller GET /api/v1/workers 拿)"
echo "    curl '${WORKER_URL}/api/opskeeper-teamharness/install-plugin/health'"

echo
echo "==> 8. 完整数据流总结"
cat <<'EOF'
    Dashboard ──► POST /api/dashboard/plugins           (Dashboard UI 插件)
           │
           ▼
    opskeeper-teamharness Dashboard 插件 → POST /v1/plugins/install (multipart zip)
           │
           ▼
    opskeeper Manager
       PluginRegistry.Install (.payload.zip 持久化)
           │
           ▼ POST /v1/plugins/{id}/push
       PluginSyncClient.InstallPlugin
           │
           ▼ GET /api/v1/workers
    AgentTeams Controller
           │
           ▼ multipart POST /api/opskeeper-teamharness/install-plugin
    qwenpaw worker
       register_http_router (FastAPI) → _extract_plugin_zip
           │
           ▼
       qwenpaw plugin install <path> --force (subprocess)
           │
           ▼ plugin.register(api) 重新执行 → Worker runtime active
EOF

echo
echo "==> 9. Live smoke test (如果 opskeeper 监听 $OPSKEEPER_URL)"
if curl -sf "$OPSKEEPER_URL/v1/plugins" >/dev/null 2>&1; then
    echo "GET $OPSKEEPER_URL/v1/plugins:"
    curl -sf "$OPSKEEPER_URL/v1/plugins" -H "Authorization: Bearer $HIGRESS_TOKEN" | python3 -m json.tool | head -20
    echo
    echo "POST install:"
    curl -sf -X POST "$OPSKEEPER_URL/v1/plugins/install" \
        -H "Authorization: Bearer $HIGRESS_TOKEN" \
        -F "file=@$WORKER_ZIP" | python3 -m json.tool | head -20
    echo
    echo "POST push:"
    curl -sf -X POST "$OPSKEEPER_URL/v1/plugins/opskeeper-teamharness/push" \
        -H "Authorization: Bearer $HIGRESS_TOKEN" | python3 -m json.tool | head -20
else
    echo "(opskeeper not running at $OPSKEEPER_URL — 跳到 §10 看静态产物)"
fi

echo
echo "==> 10. 静态验证 (无需 opskeeper 运行)"
echo "Backend handler 文件:"
ls -la "$ROOT/internal/manager/server/agentteams/plugin_http.go"
grep -c "v1/plugins" "$ROOT/internal/manager/server/agentteams/plugin_http.go" | xargs echo "  /v1/plugins/* 路由出现次数:"
echo
echo "Plugin sync interface:"
grep -E "InstallPlugin" "$ROOT/internal/agentteams/plugin_sync.go" | head -5
echo
echo "Worker install endpoint:"
grep -E "install-plugin|/sync|/health" "$OPSKEEPER_PLUGIN/adapters/qwenpaw/plugin.py" | head -10
echo
echo "Dashboard push button:"
grep -nE "push\b|/push|handlePush" "$PLUGIN_INSTALLER/dashboard/src/extensions/api.js" | head -3
grep -nE "handlePush|/push\b|>Push<" "$PLUGIN_INSTALLER/dashboard/src/extensions/route.jsx" | head -3

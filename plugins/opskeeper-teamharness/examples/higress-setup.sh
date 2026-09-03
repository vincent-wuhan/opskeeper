#!/usr/bin/env bash
# higress-setup.sh — 注册 opskeeper 为 Higress MCP proxy server
#
# 调用 AgentTeams 自带的 setup-mcp-proxy.sh（路径需 AgentTeams 安装在标准位置）。
# 这是 out-of-band 桥接脚本：调用但不修改 AgentTeams 仓库。
#
# 必需环境变量：
#   OPSKEEPER_BACKEND_HOST       opskeeper Service 的 external host
#                                 （或 cluster.local DNS 名）
#   OPSKEEPER_BOOTSTRAP_TOKEN    opskeeper bootstrap token（Higress 初始接入用）
#   HIGRESS_COOKIE_FILE          Higress Console session cookie（运维侧导出）
#
# 推荐 AgentTeams 标准路径：
#   /opt/agentteams/agent/skills/mcp-server-management/scripts/setup-mcp-proxy.sh

set -euo pipefail

: "${OPSKEEPER_BACKEND_HOST:?required: opskeeper backend host (e.g. opskeeper.example.com:8443)}"
: "${OPSKEEPER_BOOTSTRAP_TOKEN:?required: bootstrap token for Higress API auth}"
: "${HIGRESS_COOKIE_FILE:?required: Higress session cookie file path}"

# 探测 setup-mcp-proxy.sh 路径
SETUP_MCP_PROXY=""
for candidate in \
  /opt/agentteams/agent/skills/mcp-server-management/scripts/setup-mcp-proxy.sh \
  "${HOME}/agentteams-manager/opt/agentteams/agent/skills/mcp-server-management/scripts/setup-mcp-proxy.sh" \
  ./setup-mcp-proxy.sh; do
  if [ -f "$candidate" ]; then
    SETUP_MCP_PROXY="$candidate"
    break
  fi
done

if [ -z "$SETUP_MCP_PROXY" ]; then
  echo "ERROR: setup-mcp-proxy.sh not found in standard AgentTeams locations." >&2
  echo "Hint: copy from AgentTeams repo or install AgentTeams CLI." >&2
  exit 1
fi

echo ">>> Using setup-mcp-proxy.sh: $SETUP_MCP_PROXY"
echo ">>> Registering opskeeper MCP proxy on Higress"
echo "    backend: https://${OPSKEEPER_BACKEND_HOST}/v1/mcp"

bash "$SETUP_MCP_PROXY" \
  opskeeper \
  "https://${OPSKEEPER_BACKEND_HOST}/v1/mcp" \
  http \
  --header "Authorization: Bearer ${OPSKEEPER_BOOTSTRAP_TOKEN}"

echo ">>> Higress MCP proxy registered. Wait ~10s for auth plugin to activate."
echo ">>> Verify with:"
echo "    mcporter list"
echo "    mcporter list opskeeper --schema"

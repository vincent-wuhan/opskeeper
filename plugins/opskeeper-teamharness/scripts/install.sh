#!/usr/bin/env bash
# install.sh — opskeeper-teamharness lifecycle entrypoint
#
# 检测本地 runtime (qwenpaw / claude-code) 并分发到对应 adapter install.sh。
# 可选：设置 OPSKEEPER_REGISTER_HIGRESS=1 同时调用 examples/higress-setup.sh
# 注册 opskeeper 到 Higress MCP proxy（需 Manager host 权限）。

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export OPSKEEPER_PLUGIN_DIR="${OPSKEEPER_PLUGIN_DIR:-$PLUGIN_DIR}"

ran=0

if command -v qwenpaw >/dev/null 2>&1; then
  bash "${PLUGIN_DIR}/adapters/qwenpaw/install.sh"
  ran=1
fi

if command -v claude-code >/dev/null 2>&1; then
  bash "${PLUGIN_DIR}/adapters/claude-code/install.sh"
  ran=1
fi

if [ "$ran" -eq 0 ]; then
  echo "ERROR: no supported local runtime found; expected qwenpaw or claude-code" >&2
  exit 1
fi

if [ "${OPSKEEPER_REGISTER_HIGRESS:-0}" = "1" ]; then
  bash "${PLUGIN_DIR}/examples/higress-setup.sh"
fi

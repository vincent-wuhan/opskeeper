#!/usr/bin/env bash
# uninstall.sh — opskeeper-teamharness lifecycle cleanup

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export OPSKEEPER_PLUGIN_DIR="${OPSKEEPER_PLUGIN_DIR:-$PLUGIN_DIR}"

if command -v qwenpaw >/dev/null 2>&1; then
  bash "${PLUGIN_DIR}/adapters/qwenpaw/uninstall.sh"
fi

if command -v claude-code >/dev/null 2>&1; then
  bash "${PLUGIN_DIR}/adapters/claude-code/uninstall.sh"
fi

#!/usr/bin/env bash
# claude-code adapter — uninstall 占位
set -euo pipefail

log_file="${OPSKEEPER_INSTALL_LOG:-}"
if [ -n "$log_file" ]; then
  mkdir -p "$(dirname "$log_file")"
  printf '{"event":"uninstall","runtime":"claude-code","pluginDir":"%s"}\n' "${OPSKEEPER_PLUGIN_DIR:-${PWD}}" >> "$log_file"
fi

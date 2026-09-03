#!/usr/bin/env bash
# uninstall.sh — opskeeper-teamharness QwenPaw adapter uninstaller

set -euo pipefail

if command -v qwenpaw >/dev/null 2>&1; then
  printf 'y\n' | qwenpaw plugin uninstall opskeeper-teamharness >/dev/null 2>&1 || true
fi

log_file="${OPSKEEPER_INSTALL_LOG:-}"
if [ -n "$log_file" ]; then
  mkdir -p "$(dirname "$log_file")"
  printf '{"event":"uninstall","runtime":"qwenpaw","pluginDir":"%s"}\n' "${OPSKEEPER_PLUGIN_DIR:-${PWD}}" >> "$log_file"
fi

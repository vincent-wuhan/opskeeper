#!/usr/bin/env bash
# claude-code adapter — 占位实现
# 当前阶段 Claude Code runtime 与 opskeeper-teamharness 集成的具体 hook 由后续阶段定义。
# 此处仅记录 install event，与 teamharness/adapters/claude-code/install.sh 对齐。
set -euo pipefail

log_file="${OPSKEEPER_INSTALL_LOG:-}"
if [ -n "$log_file" ]; then
  mkdir -p "$(dirname "$log_file")"
  printf '{"event":"install","runtime":"claude-code","pluginDir":"%s"}\n' "${OPSKEEPER_PLUGIN_DIR:-${PWD}}" >> "$log_file"
fi

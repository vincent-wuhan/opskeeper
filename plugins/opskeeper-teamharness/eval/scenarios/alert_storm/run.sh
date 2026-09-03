#!/usr/bin/env bash
# alert_storm 剧本一键跑脚本（从 plugin root 调）
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
docker compose -f "$HERE/compose.yml" up --abort-on-container-exit --exit-code-from runner
echo "--- reports ---"
ls -la "$HERE/reports/" 2>/dev/null || true
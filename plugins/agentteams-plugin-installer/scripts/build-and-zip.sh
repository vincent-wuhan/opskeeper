#!/usr/bin/env bash
# Build agentteams-plugin-installer.zip ready to upload to Dashboard.
set -euo pipefail
PLUGIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$PLUGIN_DIR/dist"

echo "==> 1. Install dashboard deps"
(cd "$PLUGIN_DIR/dashboard" && npm install --silent)

echo "==> 2. Build dashboard bundle"
(cd "$PLUGIN_DIR/dashboard" && npm run build)

echo "==> 3. Zip plugin package"
mkdir -p "$DIST"
zip -j "$DIST/agentteams-plugin-installer.zip" \
  "$PLUGIN_DIR/dashboard/public/plugin.json" \
  "$PLUGIN_DIR/dashboard/dist/main.js" \
  "$PLUGIN_DIR/../../LICENSE" \
  "$PLUGIN_DIR/../../NOTICE.md"

echo
echo "==> Done. Upload this zip via Dashboard → Settings → 插件:"
ls -la "$DIST/agentteams-plugin-installer.zip"

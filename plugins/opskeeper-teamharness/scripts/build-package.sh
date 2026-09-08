#!/usr/bin/env bash
set -euo pipefail

PLUGIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT_DIR="$(cd "$PLUGIN_DIR/../.." && pwd)"
cd "$PLUGIN_DIR"

VERSION="$(ruby -ryaml -e 'puts YAML.load_file("plugin.yaml").fetch("metadata").fetch("version")')"
BASE_PACKAGE="dist/opskeeper-teamharness-${VERSION}-plugin-manager.tar.gz"
DASHBOARD_PACKAGE="dist/opskeeper-teamharness-dashboard-${VERSION}.zip"

npm install --silent --prefix dashboard
npm run build --prefix dashboard
cp dashboard/dist/main.js "dashboard/dist/main-${VERSION}.js"
cp dashboard/dist/main.js.map "dashboard/dist/main-${VERSION}.js.map"
mkdir -p dist

OUT_DIR="$PLUGIN_DIR/dist" ruby adapters/qwenpaw/scripts/build-qwenpaw-plugin.rb plugin.yaml
rm -f "$BASE_PACKAGE"
rm -f "$DASHBOARD_PACKAGE"
tar \
  --uname 0 \
  --gname 0 \
  --numeric-owner \
  --exclude '.DS_Store' \
  --exclude '__pycache__' \
  --exclude '*.pyc' \
  --exclude 'dashboard/node_modules' \
  -czf "$BASE_PACKAGE" \
  -C "$PLUGIN_DIR" plugin.yaml prompts skills mcp adapters scripts loongsuite examples README.md CHANGELOG.md dashboard \
  -C "$ROOT_DIR" LICENSE NOTICE.md

(
  cd dashboard
  zip -X -r "../${DASHBOARD_PACKAGE}" plugin.json dist/main.js dist/main.js.map "dist/main-${VERSION}.js" "dist/main-${VERSION}.js.map"
)

printf '\nTeamHarness base package:\n'
ls -lh "$BASE_PACKAGE"
printf '\nSHA256:\n'
shasum -a 256 "$BASE_PACKAGE" "$DASHBOARD_PACKAGE" dist/opskeeper-teamharness-qwenpaw-"${VERSION}".zip

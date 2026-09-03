#!/usr/bin/env bash
set -euo pipefail

PLUGIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT_DIR="$(cd "$PLUGIN_DIR/../.." && pwd)"
cd "$PLUGIN_DIR"

VERSION="$(ruby -ryaml -e 'puts YAML.load_file("plugin.yaml").fetch("metadata").fetch("version")')"
BASE_PACKAGE="dist/opskeeper-teamharness-${VERSION}-plugin-manager.tar.gz"

npm install --silent --prefix dashboard
npm run build --prefix dashboard
mkdir -p dist

OUT_DIR="$PLUGIN_DIR/dist" ruby adapters/qwenpaw/scripts/build-qwenpaw-plugin.rb plugin.yaml
rm -f "$BASE_PACKAGE"
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

printf '\nTeamHarness base package:\n'
ls -lh "$BASE_PACKAGE"
printf '\nSHA256:\n'
shasum -a 256 "$BASE_PACKAGE" dist/opskeeper-teamharness-qwenpaw-"${VERSION}".zip

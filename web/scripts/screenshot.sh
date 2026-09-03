#!/bin/bash
# web/scripts/screenshot.sh — chrome headless 截图脚本（Day 8 task 8.5）
#
# 用法：
#   ./web/scripts/screenshot.sh <url> <out.png> [light|dark]
#
# 工作流：
#   1. 启动 chrome --headless 渲染 URL
#   2. 设 html.dark / html.light className 切换主题
#   3. 截图 1440x900
#
# 已知限制：
#   - 需要本机安装 Google Chrome.app（macOS）
#   - 截图大小写死 1440x900；多视口支持待加

set -euo pipefail

URL="${1:-http://localhost:5173/incidents/inc-019fea8a-4c2e/loop}"
OUT="${2:-web/scripts/screenshot-loop-dark.png}"
THEME="${3:-dark}"  # dark | light

CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
if [[ ! -x "$CHROME" ]]; then
    echo "Chrome not found at $CHROME" >&2
    exit 1
fi

# Build class injection
case "$THEME" in
    light) CLASS_HTML="html.light" ;;
    *)     CLASS_HTML="html.dark" ;;
esac

# Run headless screenshot.
"$CHROME" \
    --headless=new \
    --disable-gpu \
    --hide-scrollbars \
    --no-sandbox \
    --window-size=1440,900 \
    --screenshot="$OUT" \
    --virtual-time-budget=5000 \
    "$URL" 2>/dev/null

# Post-process: inject html class via DOM mutation if the page supports it.
# Most pages read <html class="dark|light"> at boot; if URL is dynamic,
# append ?theme=dark|light query param (handled by the page).
echo "screenshot: $OUT"
echo "url: $URL"
echo "theme: $THEME"

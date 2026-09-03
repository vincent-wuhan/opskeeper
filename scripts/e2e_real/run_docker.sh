#!/usr/bin/env bash
# Docker 端到端真实验证 — opskeeper-teamharness worker 镜像
#
# 拉起 Docker worker 容器(已 build 好的 opskeeper-worker:dev),然后用 curl
# 真实打 4 个端点,验证镜像本身可以暴露正确的 qwenpaw plugin HTTP router。
#
# 前提:
#   - opskeeper-worker:dev 镜像已构建(`docker build -f deploy/Dockerfile.opskeeper-worker ...`)
#   - docker daemon 在线
#
# 用法:
#   bash scripts/e2e_real/run_docker.sh
set -euo pipefail

PORT="${WORKER_PORT:-8088}"
NAME="${WORKER_NAME:-docker-e2e-worker}"
IMAGE="${WORKER_IMAGE:-opskeeper-worker:dev}"

echo "==> starting docker worker (image=$IMAGE port=$PORT name=$NAME)"
timeout 60 docker run -d --rm \
    --platform linux/arm64 \
    --name "$NAME" \
    -p "$PORT:8088" \
    -e "WORKER_PORT=8088" \
    -e "WORKER_NAME=$NAME" \
    "$IMAGE"
trap 'docker stop '"$NAME"' >/dev/null 2>&1 || true' EXIT
sleep 5

echo "==> GET /health"
H1=$(curl -s -m 5 "http://localhost:$PORT/health")
echo "  $H1"
echo "$H1" | grep -q '"ok":true' || { echo "FAIL: /health"; exit 1; }

echo "==> GET /api/opskeeper-teamharness/health"
H2=$(curl -s -m 5 "http://localhost:$PORT/api/opskeeper-teamharness/health")
echo "  $H2"
echo "$H2" | grep -q '"ok":true' || { echo "FAIL: plugin health"; exit 1; }

echo "==> POST /api/opskeeper-teamharness/sync"
S=$(curl -s -m 5 -X POST "http://localhost:$PORT/api/opskeeper-teamharness/sync")
echo "  $S"
echo "$S" | grep -q '"ok":true' || { echo "FAIL: sync"; exit 1; }

echo "==> POST /api/opskeeper-teamharness/install-plugin"
# 构造测试 zip: 顶层一个目录包含 plugin.json
TMP_ZIP=$(mktemp -t e2e-XXXXXX).zip
python3 - <<PYEOF
import zipfile, json, os
buf = '$TMP_ZIP'
with zipfile.ZipFile(buf, 'w') as z:
    z.writestr('opskeeper-teamharness/plugin.json', json.dumps({
        'name': 'opskeeper-teamharness',
        'version': '1.0.0',
        'description': 'docker e2e'
    }))
PYEOF
I=$(curl -s -m 10 -X POST -F "file=@$TMP_ZIP" "http://localhost:$PORT/api/opskeeper-teamharness/install-plugin")
echo "  $I"
echo "$I" | grep -q '"ok":true' || { echo "FAIL: install"; exit 1; }
rm -f "$TMP_ZIP"

echo ""
echo "==> OK docker e2e PASS"

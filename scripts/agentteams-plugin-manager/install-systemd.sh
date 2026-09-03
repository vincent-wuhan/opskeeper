#!/usr/bin/env bash
# install-systemd.sh — install + start the agentteams-plugin-manager service.
# Run as root on the host where plugin-manager should run.
set -euo pipefail

UNIT_SRC="$(cd "$(dirname "$0")" && pwd)/agentteams-plugin-manager.service"
UNIT_DST="/etc/systemd/system/agentteams-plugin-manager.service"
ENV_EXAMPLE="$(cd "$(dirname "$0")" && pwd)/agentteams-plugin-manager.env.example"
ENV_DST="/etc/agentteams-plugin-manager.env"
BIN_DST="/usr/local/bin/agentteams-plugin-manager"

if [[ $EUID -ne 0 ]]; then
  echo "must run as root (sudo $0)" >&2
  exit 1
fi

# 1. binary
if [[ ! -x "$BIN_DST" ]]; then
  echo "binary not found at $BIN_DST — copy it first:" >&2
  echo "  install -m 0755 ./agentteams-plugin-manager $BIN_DST" >&2
  exit 1
fi

# 2. unit
install -m 0644 "$UNIT_SRC" "$UNIT_DST"
echo "installed unit → $UNIT_DST"

# 3. env (only if missing — never overwrite secrets)
if [[ ! -f "$ENV_DST" ]]; then
  install -m 0600 "$ENV_EXAMPLE" "$ENV_DST"
  echo "created env template → $ENV_DST (edit and chmod 0600 if needed)"
else
  echo "env file already exists at $ENV_DST — leaving untouched"
fi

# 4. storage paths
mkdir -p /var/lib/agentteams-plugin-manager
chown -R root:root /var/lib/agentteams-plugin-manager
chmod 0750 /var/lib/agentteams-plugin-manager
mkdir -p /var/log/agentteams-plugin-manager
echo "storage dirs ready"

# 5. reload + enable + start
systemctl daemon-reload
systemctl enable agentteams-plugin-manager
systemctl restart agentteams-plugin-manager
sleep 1
systemctl --no-pager status agentteams-plugin-manager || true

echo ""
echo "verify: curl http://127.0.0.1:18095/healthz"
echo "logs:   journalctl -u agentteams-plugin-manager -f"
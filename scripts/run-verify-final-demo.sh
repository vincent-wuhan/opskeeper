#!/usr/bin/env bash
# Wrapper for running scripts/verify-final-demo.sh inside the public opskeeper
# container. Sources the demo token, manager JWT, and plugin-manager SA token
# from the public host's well-known locations, then executes the verification
# script in a controlled environment.
#
# Required host layout:
#   /root/config/final-demo-e82d97d1-1.0.67/opskeeper.env    contains OPSKEEPER_DEMO_API_TOKEN
#   /root/config/opskeeper-final-demo-e2e.jwt               Manager JWT for the public Manager
#   agentteams-plugin-manager container env                 has PLUGIN_MANAGER_SA_TOKEN
#
# The wrapper fails fast if any of the three required values is missing or empty,
# so that MANAGER_AUTH_TOKEN, DEMO_API_TOKEN, and PLUGIN_HEALTH_TOKEN always
# reach the inner docker exec.

set -euo pipefail

WRAPPER_NAME="run-verify-final-demo"
HOST_CONFIG_DIR="${OPSKEEPER_HOST_CONFIG_DIR:-/root/config}"
HOST_EVIDENCE_DIR="${OPSKEEPER_HOST_EVIDENCE_DIR:-/root/evidence}"
HOST_OPSKEEPER_ENV="${OPSKEEPER_HOST_OPSKEEPER_ENV:-${HOST_CONFIG_DIR}/final-demo-e82d97d1-1.0.67/opskeeper.env}"
HOST_MANAGER_JWT="${HOST_CONFIG_DIR}/opskeeper-final-demo-e2e.jwt"
CONTAINER_NAME="${OPSKEEPER_CONTAINER_NAME:-opskeeper}"
CONTAINER_SCRIPT="${OPSKEEPER_CONTAINER_SCRIPT:-/src/scripts/verify-final-demo.sh}"
HOME_URL="${OPSKEEPER_HOME_URL:-https://opskeeper.yueming.xin/live-incident}"
# Container-internal staging directory for EVIDENCE_OUTPUT. The inner script
# writes the JSON file here (as `nonroot`, this is always writable). After
# `docker exec` returns, the wrapper copies the file to HOST_EVIDENCE_DIR via
# `docker cp`, preserving the host directory's existing permissions.
CONTAINER_EVIDENCE_DIR="${OPSKEEPER_CONTAINER_EVIDENCE_DIR:-/tmp/verify-evidence}"
EXPECTED_MANAGER_VERSION="${EXPECTED_MANAGER_VERSION:-e82d97d1-1.0.67}"
EXPECTED_PLUGIN_VERSION="${EXPECTED_PLUGIN_VERSION:-1.0.67}"
WORKLOAD_FINGERPRINT="${OPSKEEPER_REPAIR_PREVIEW_WORKLOAD_FINGERPRINT:-sha256:db905b8f98c631212336b736f92d80b2a3040a75a44554687cfc782d39c31cc4}"
TARGET_FINGERPRINT="${OPSKEEPER_REPAIR_PREVIEW_TARGET_FINGERPRINT:-0123456789abcdef0123456789abcdef}"
SCENARIO_DURATION_SECONDS="${OPSKEEPER_SCENARIO_DURATION_SECONDS:-180}"

read_demo_token() {
  if [[ ! -r "$HOST_OPSKEEPER_ENV" ]]; then
    printf '%s: opskeeper env file not readable: %s\n' "$WRAPPER_NAME" "$HOST_OPSKEEPER_ENV" >&2
    return 1
  fi
  local value
  value="$(grep '^OPSKEEPER_DEMO_API_TOKEN=' "$HOST_OPSKEEPER_ENV" | head -1 | cut -d= -f2-)"
  if [[ -z "$value" ]]; then
    printf '%s: OPSKEEPER_DEMO_API_TOKEN missing from %s\n' "$WRAPPER_NAME" "$HOST_OPSKEEPER_ENV" >&2
    return 1
  fi
  printf '%s' "$value"
}

read_manager_jwt() {
  if [[ ! -r "$HOST_MANAGER_JWT" ]]; then
    printf '%s: manager JWT file not readable: %s\n' "$WRAPPER_NAME" "$HOST_MANAGER_JWT" >&2
    return 1
  fi
  local value
  value="$(cat "$HOST_MANAGER_JWT")"
  if [[ -z "$value" ]]; then
    printf '%s: manager JWT is empty in %s\n' "$WRAPPER_NAME" "$HOST_MANAGER_JWT" >&2
    return 1
  fi
  printf '%s' "$value"
}

read_plugin_token() {
  local raw
  raw="$(docker inspect "$CONTAINER_NAME-plugin-manager" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null || true)"
  if [[ -z "$raw" ]]; then
    raw="$(docker inspect agentteams-plugin-manager --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null || true)"
  fi
  local value
  value="$(printf '%s\n' "$raw" | grep '^PLUGIN_MANAGER_SA_TOKEN=' | head -1 | cut -d= -f2-)"
  if [[ -z "$value" ]]; then
    printf '%s: PLUGIN_MANAGER_SA_TOKEN missing from plugin-manager container env\n' "$WRAPPER_NAME" >&2
    return 1
  fi
  printf '%s' "$value"
}

DEMO_TOKEN="$(read_demo_token)"
MANAGER_JWT="$(read_manager_jwt)"
PLUGIN_TOKEN="$(read_plugin_token)"

# Final host-side path: the file the user can read after the wrapper finishes.
# The container-internal path (EVIDENCE_OUT) is what we pass to the inner
# script. The host-side path is where `docker cp` lands it.
UTC_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
HOST_EVIDENCE_OUT="${HOST_EVIDENCE_DIR}/verify-final-demo-${UTC_STAMP}.json"
EVIDENCE_OUT="${CONTAINER_EVIDENCE_DIR}/verify-final-demo-${UTC_STAMP}.json"
mkdir -p "$HOST_EVIDENCE_DIR"

SCENARIO_IDEMPOTENCY_KEY="final-demo-e82d97d1-$(date -u +%Y%m%dT%H%M%SZ)-$$"
# validFingerprint (internal/manager/biz/demo/scenario.go) accepts only hex or
# sha256:<64hex>. The previous "pg-pool-waiters-<utc>-<pid>" string failed that
# check and Manager responded with HTTP 400 invalid_request. Derive the alert
# fingerprint from the idempotency key so it is unique per run, stable across
# the idempotent retry, and always passes validFingerprint.
ALERT_FINGERPRINT="sha256:$(printf '%s' "${SCENARIO_IDEMPOTENCY_KEY}" | sha256sum | cut -d' ' -f1)"

printf '%s: launching verify-final-demo.sh inside %s\n' "$WRAPPER_NAME" "$CONTAINER_NAME" >&2
printf '%s: EVIDENCE_OUTPUT(container)=%s\n' "$WRAPPER_NAME" "$EVIDENCE_OUT" >&2
printf '%s: EVIDENCE_OUTPUT(host)=%s\n' "$WRAPPER_NAME" "$HOST_EVIDENCE_OUT" >&2
printf '%s: SCENARIO_IDEMPOTENCY_KEY=%s\n' "$WRAPPER_NAME" "$SCENARIO_IDEMPOTENCY_KEY" >&2
printf '%s: token lengths: manager=%d demo=%d plugin=%d\n' \
  "$WRAPPER_NAME" "${#MANAGER_JWT}" "${#DEMO_TOKEN}" "${#PLUGIN_TOKEN}" >&2

# Ensure the container-internal staging dir exists and is owned by the
# script's runtime uid (the container runs as `nonroot`, but we prepare as
# root for predictability). /tmp itself is always writable.
docker exec -u root "$CONTAINER_NAME" mkdir -p "$CONTAINER_EVIDENCE_DIR" >/dev/null
CONTAINER_UID="$(docker exec "$CONTAINER_NAME" id -u)"
CONTAINER_GID="$(docker exec "$CONTAINER_NAME" id -g)"
docker exec -u root "$CONTAINER_NAME" chown "$CONTAINER_UID:$CONTAINER_GID" "$CONTAINER_EVIDENCE_DIR" >/dev/null

# IMPORTANT: every -e line must end with a single backslash and a newline so the
# shell performs line continuation. No trailing whitespace after the backslash.
# The values are passed verbatim to docker exec; do not use shell expansion that
# could swallow newlines.
docker exec -i \
  -e MANAGER_URL="https://opskeeper.yueming.xin" \
  -e HOME_URL="$HOME_URL" \
  -e TEAMS_URL="https://teams.yueming.xin" \
  -e ROOMS_URL="https://rooms.yueming.xin" \
  -e OPSKEEPER_URL="https://opskeeper.yueming.xin" \
  -e PROMETHEUS_URL="http://opskeeper-demo-prom:9090" \
  -e PLUGIN_HEALTH_URL="http://agentteams-plugin-manager:8095/api/v1/plugins/opskeeper-teamharness/health" \
  -e EXPECTED_MANAGER_VERSION="$EXPECTED_MANAGER_VERSION" \
  -e EXPECTED_PLUGIN_VERSION="$EXPECTED_PLUGIN_VERSION" \
  -e MANAGER_AUTH_TOKEN="$MANAGER_JWT" \
  -e DEMO_API_TOKEN="$DEMO_TOKEN" \
  -e PLUGIN_HEALTH_TOKEN="$PLUGIN_TOKEN" \
  -e HTTP_TIMEOUT_SECONDS=20 \
  -e POLL_INTERVAL_SECONDS=5 \
  -e WORKFLOW_TIMEOUT_SECONDS=900 \
  -e RECOVERY_TIMEOUT_SECONDS=240 \
  -e DEGRADED_LATENCY_MS=1500 \
  -e MIN_STRESSED_UTILIZATION=0.90 \
  -e MAX_RECOVERED_UTILIZATION=0.25 \
  -e SCENARIO_IDEMPOTENCY_KEY="$SCENARIO_IDEMPOTENCY_KEY" \
  -e TARGET_FINGERPRINT="$TARGET_FINGERPRINT" \
  -e ALERT_FINGERPRINT="$ALERT_FINGERPRINT" \
  -e SCENARIO_DURATION_SECONDS="$SCENARIO_DURATION_SECONDS" \
  -e EVIDENCE_OUTPUT="$EVIDENCE_OUT" \
  "$CONTAINER_NAME" bash -lc "$CONTAINER_SCRIPT"
exec_rc=$?

# Pull the evidence JSON back to the host. We tolerate a missing file (the
# inner script may have failed before producing one) but log it loudly. We do
# NOT relax /root/evidence permissions; if the cp target dir is missing or
# unwritable we keep going rather than failing the verify run.
if [[ ! -d "$HOST_EVIDENCE_DIR" ]]; then
  printf '%s: host evidence dir missing: %s (skipping docker cp)\n' \
    "$WRAPPER_NAME" "$HOST_EVIDENCE_DIR" >&2
elif docker exec -u root "$CONTAINER_NAME" test -s "$EVIDENCE_OUT" 2>/dev/null; then
  if docker cp "${CONTAINER_NAME}:${EVIDENCE_OUT}" "$HOST_EVIDENCE_OUT" 2>"${HOST_EVIDENCE_DIR}/.cp-error.log"; then
    printf '%s: evidence copied to %s\n' "$WRAPPER_NAME" "$HOST_EVIDENCE_OUT" >&2
  else
    printf '%s: WARN docker cp failed; evidence remains inside container at %s\n' \
      "$WRAPPER_NAME" "$EVIDENCE_OUT" >&2
    printf '%s: see %s/.cp-error.log\n' "$WRAPPER_NAME" "$HOST_EVIDENCE_DIR" >&2
  fi
else
  printf '%s: no evidence file produced inside container at %s\n' \
    "$WRAPPER_NAME" "$EVIDENCE_OUT" >&2
fi

exit $exec_rc

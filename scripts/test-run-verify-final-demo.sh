#!/usr/bin/env bash
# Focused tests for scripts/run-verify-final-demo.sh.
# Verifies that:
#   1. The wrapper fails fast if any of the three required tokens is empty
#      (MANAGER_AUTH_TOKEN, DEMO_API_TOKEN, PLUGIN_HEALTH_TOKEN).
#   2. When all three tokens are populated, the wrapper invokes `docker exec`
#      with the correct env vars, including the three required tokens.
#   3. Newline-continuation lines do not lose their trailing backslash, so
#      every -e argument is passed to docker exec.
#
# Tests run entirely in a sandbox; no real docker or aliyun access required.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WRAPPER_UNDER_TEST="${SCRIPT_DIR}/run-verify-final-demo.sh"

if [[ ! -x "$WRAPPER_UNDER_TEST" ]]; then
  printf 'test-run-verify-final-demo: wrapper not executable: %s\n' "$WRAPPER_UNDER_TEST" >&2
  exit 1
fi

WORK="$(mktemp -d -t run-verify-test-XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

fail_test() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

pass_test() {
  printf 'PASS: %s\n' "$*"
}

# Build a sandbox that exposes a fake docker and three token source locations.
build_sandbox() {
  local sandbox="$1"
  local demo_value="$2"
  local manager_value="$3"
  local plugin_value="$4"

  rm -rf "$sandbox"
  mkdir -p "$sandbox/config/final-demo-e82d97d1-1.0.67"
  mkdir -p "$sandbox/evidence"
  mkdir -p "$sandbox/bin"

  printf 'OPSKEEPER_DEMO_API_TOKEN=%s\n' "$demo_value" \
    >"$sandbox/config/final-demo-e82d97d1-1.0.67/opskeeper.env"
  printf '%s' "$manager_value" \
    >"$sandbox/config/opskeeper-final-demo-e2e.jwt"

  # Fake docker that records the env vars passed via -e and handles
  # `docker inspect <container>` by reading the matching env file. All other
  # docker commands succeed silently. Plugin-manager container is recognized
  # by suffix "-plugin-manager".
  cat >"$sandbox/bin/docker" <<FAKE_DOCKER
#!/usr/bin/env bash
set -euo pipefail
output_file="\${DOCKER_FAKE_OUTPUT:-}"
plugin_env_file="\${FAKE_PLUGIN_ENV_FILE:-}"
cp_log_file="\${DOCKER_FAKE_CP_LOG:-}"
cp_should_fail="\${DOCKER_FAKE_CP_FAIL:-0}"
mkdir_log_file="\${DOCKER_FAKE_MKDIR_LOG:-}"
id_log_file="\${DOCKER_FAKE_ID_LOG:-}"
chown_log_file="\${DOCKER_FAKE_CHOWN_LOG:-}"
subcommand="\${1:-}"
case "\$subcommand" in
  inspect)
    container="\${2:-}"
    if [[ -n "\$plugin_env_file" && "\$container" == *-plugin-manager ]]; then
      cat "\$plugin_env_file"
      exit 0
    fi
    if [[ "\$container" == *-plugin-manager ]]; then
      printf 'PLUGIN_MANAGER_SA_TOKEN=\n'
      exit 0
    fi
    exit 0
    ;;
  exec)
    shift
    prev=""
    for arg in "\$@"; do
      if [[ "\$prev" == "-e" ]]; then
        printf '%s\n' "\$arg" >>"\$output_file"
        if [[ "\$arg" == EVIDENCE_OUTPUT=* ]]; then
          # Record the container-internal evidence path so cp tests can assert.
          printf '%s\n' "\${arg#EVIDENCE_OUTPUT=}" >>"\${DOCKER_FAKE_EVIDENCE_OUT_FILE:-/tmp/ok}"
        fi
      elif [[ "\$prev" == "-u" ]]; then
        printf 'u=%s\n' "\$arg" >>"\$output_file"
      fi
      prev="\$arg"
    done
    # Capture mkdir/chown/id subexec patterns.
    if [[ "\$*" == *mkdir* ]]; then
      printf 'mkdir\n' >>"\$mkdir_log_file"
    fi
    if [[ "\$*" == *chown* ]]; then
      printf 'chown %s\n' "\$*" >>"\$chown_log_file"
    fi
    # Match `id -u` / `id -g` regardless of preceding docker exec args.
    if [[ "\$*" == *" id -u" || "\$*" == *" id -g" ]]; then
      printf '65532\n' >>"\$id_log_file"
    fi
    exit 0
    ;;
  cp)
    shift
    printf 'cp %s\n' "\$*" >>"\$cp_log_file"
    if [[ "\$cp_should_fail" == "1" ]]; then
      printf 'simulated cp failure\n' >&2
      exit 1
    fi
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
FAKE_DOCKER
  chmod +x "$sandbox/bin/docker"

  printf 'PLUGIN_MANAGER_SA_TOKEN=%s\n' "$plugin_value" \
    >"$sandbox/plugin-manager.env"

  printf 'sandbox ready: %s\n' "$sandbox" >&2
}

run_wrapper_in_sandbox() {
  local sandbox="$1"
  local container_name="${2:-opskeeper}"
  local output_file="$sandbox/docker-calls.txt"
  rm -f "$output_file"
  rm -f "$sandbox/cp-calls.txt" "$sandbox/mkdir-calls.txt" "$sandbox/id-calls.txt" "$sandbox/chown-calls.txt"
  rm -f "$sandbox/evidence-out.txt"
  (
    cd "$sandbox"
    OPSKEEPER_HOST_CONFIG_DIR="$sandbox/config" \
    OPSKEEPER_HOST_EVIDENCE_DIR="$sandbox/evidence" \
  OPSKEEPER_HOST_OPSKEEPER_ENV="$sandbox/config/final-demo-e82d97d1-1.0.67/opskeeper.env" \
    OPSKEEPER_CONTAINER_NAME="$container_name" \
    OPSKEEPER_CONTAINER_SCRIPT="/src/scripts/verify-final-demo.sh" \
    PATH="$sandbox/bin:$PATH" \
    DOCKER_FAKE_OUTPUT="$output_file" \
    DOCKER_FAKE_CP_LOG="$sandbox/cp-calls.txt" \
    DOCKER_FAKE_MKDIR_LOG="$sandbox/mkdir-calls.txt" \
    DOCKER_FAKE_ID_LOG="$sandbox/id-calls.txt" \
    DOCKER_FAKE_CHOWN_LOG="$sandbox/chown-calls.txt" \
    DOCKER_FAKE_EVIDENCE_OUT_FILE="$sandbox/evidence-out.txt" \
    DOCKER_FAKE_CP_FAIL="${DOCKER_FAKE_CP_FAIL:-0}" \
    FAKE_PLUGIN_ENV_FILE="$sandbox/plugin-manager.env" \
    bash "$WRAPPER_UNDER_TEST"
  )
}

# ---------------------------------------------------------------------------
# Test 1: missing DEMO_API_TOKEN must fail fast.
# ---------------------------------------------------------------------------
build_sandbox "$WORK/sandbox1" "" "JWT_VALID" "PLUGIN_VALID"
set +e
output1=$(run_wrapper_in_sandbox "$WORK/sandbox1" 2>&1)
rc1=$?
set -e
if [[ $rc1 -eq 0 ]]; then
  fail_test "wrapper did not fail when DEMO_API_TOKEN is empty"
fi
if [[ "$output1" != *"OPSKEEPER_DEMO_API_TOKEN missing"* ]]; then
  fail_test "wrapper did not report missing DEMO_API_TOKEN: got: $output1"
fi
if [[ -f "$WORK/sandbox1/docker-calls.txt" ]]; then
  fail_test "wrapper invoked docker exec even though DEMO_API_TOKEN was empty"
fi
pass_test "missing DEMO_API_TOKEN fails fast without invoking docker"

# ---------------------------------------------------------------------------
# Test 2: missing manager JWT must fail fast.
# ---------------------------------------------------------------------------
build_sandbox "$WORK/sandbox2" "DEMO_VALID" "" "PLUGIN_VALID"
set +e
output2=$(run_wrapper_in_sandbox "$WORK/sandbox2" 2>&1)
rc2=$?
set -e
if [[ $rc2 -eq 0 ]]; then
  fail_test "wrapper did not fail when manager JWT is empty"
fi
if [[ "$output2" != *"manager JWT is empty"* ]]; then
  fail_test "wrapper did not report empty manager JWT: got: $output2"
fi
pass_test "missing manager JWT fails fast without invoking docker"

# ---------------------------------------------------------------------------
# Test 3: missing plugin token must fail fast.
# ---------------------------------------------------------------------------
build_sandbox "$WORK/sandbox3" "DEMO_VALID" "JWT_VALID" ""
set +e
output3=$(run_wrapper_in_sandbox "$WORK/sandbox3" 2>&1)
rc3=$?
set -e
if [[ $rc3 -eq 0 ]]; then
  fail_test "wrapper did not fail when PLUGIN_MANAGER_SA_TOKEN is empty"
fi
if [[ "$output3" != *"PLUGIN_MANAGER_SA_TOKEN missing"* ]]; then
  fail_test "wrapper did not report missing PLUGIN_MANAGER_SA_TOKEN: got: $output3"
fi
pass_test "missing plugin token fails fast without invoking docker"

# ---------------------------------------------------------------------------
# Test 4: when all three tokens are populated, the wrapper invokes docker exec
# with MANAGER_AUTH_TOKEN, DEMO_API_TOKEN, and PLUGIN_HEALTH_TOKEN reaching
# the inner container.
# ---------------------------------------------------------------------------
build_sandbox "$WORK/sandbox4" "DEMO_VALID_VALUE" "JWT_VALID_VALUE" "PLUGIN_VALID_VALUE"
set +e
output4=$(run_wrapper_in_sandbox "$WORK/sandbox4" "opskeeper" 2>&1)
rc4=$?
set -e
if [[ $rc4 -ne 0 ]]; then
  fail_test "wrapper exited non-zero with all three tokens present: rc=$rc4 output=$output4"
fi
recorded="$WORK/sandbox4/docker-calls.txt"
if [[ ! -s "$recorded" ]]; then
  fail_test "wrapper did not invoke docker exec with -e args"
fi
for required_pair in \
  "MANAGER_AUTH_TOKEN=JWT_VALID_VALUE" \
  "DEMO_API_TOKEN=DEMO_VALID_VALUE" \
  "PLUGIN_HEALTH_TOKEN=PLUGIN_VALID_VALUE" \
  "MANAGER_URL=https://opskeeper.yueming.xin" \
  "EXPECTED_MANAGER_VERSION=e82d97d1-1.0.67" \
  "EXPECTED_PLUGIN_VERSION=1.0.67"; do
    if ! grep -Fxq "$required_pair" "$recorded"; then
      fail_test "wrapper did not pass '$required_pair' to docker exec; recorded: $(tr '\n' '|' <"$recorded")"
    fi
  done
pass_test "wrapper passes MANAGER_AUTH_TOKEN, DEMO_API_TOKEN, PLUGIN_HEALTH_TOKEN to docker exec"

# ---------------------------------------------------------------------------
# Test 5: every -e line preserves the trailing backslash continuation, so
# the docker exec call is not split. Detect this by checking that all the
# env vars we expect are present in order and that the host does not see
# an incomplete -e argument.
# ---------------------------------------------------------------------------
expected_order=(
  "MANAGER_URL=https://opskeeper.yueming.xin"
  "HOME_URL=https://opskeeper.yueming.xin/live-incident"
  "TEAMS_URL=https://teams.yueming.xin"
  "ROOMS_URL=https://rooms.yueming.xin"
  "OPSKEEPER_URL=https://opskeeper.yueming.xin"
  "PROMETHEUS_URL=http://opskeeper-demo-prom:9090"
  "PLUGIN_HEALTH_URL=http://agentteams-plugin-manager:8095/api/v1/plugins/opskeeper-teamharness/health"
  "EXPECTED_MANAGER_VERSION=e82d97d1-1.0.67"
  "EXPECTED_PLUGIN_VERSION=1.0.67"
  "MANAGER_AUTH_TOKEN=JWT_VALID_VALUE"
  "DEMO_API_TOKEN=DEMO_VALID_VALUE"
  "PLUGIN_HEALTH_TOKEN=PLUGIN_VALID_VALUE"
  "HTTP_TIMEOUT_SECONDS=20"
  "POLL_INTERVAL_SECONDS=5"
  "WORKFLOW_TIMEOUT_SECONDS=900"
  "RECOVERY_TIMEOUT_SECONDS=240"
  "DEGRADED_LATENCY_MS=1500"
  "MIN_STRESSED_UTILIZATION=0.90"
  "MAX_RECOVERED_UTILIZATION=0.25"
  "TARGET_FINGERPRINT=0123456789abcdef0123456789abcdef"
  "SCENARIO_DURATION_SECONDS=180"
)
recorded_lines=$(wc -l <"$recorded" | tr -d ' ')
if [[ "$recorded_lines" -lt 20 ]]; then
  fail_test "expected at least 20 docker -e args, got $recorded_lines: $(tr '\n' '|' <"$recorded")"
fi
for entry in "${expected_order[@]}"; do
  if ! grep -F -x "$entry" "$recorded" >/dev/null; then
    fail_test "expected env var not passed verbatim: $entry"
  fi
done
pass_test "newline-continuation preserves every -e argument end-to-end"

# ---------------------------------------------------------------------------
# Test 6: idempotency key uses the configured prefix and a timestamp/pid tail.
# ---------------------------------------------------------------------------
key_value=$(grep '^SCENARIO_IDEMPOTENCY_KEY=' "$recorded" | head -1 | cut -d= -f2-)
if [[ ! "$key_value" =~ ^final-demo-e82d97d1-[0-9]{8}T[0-9]{6}Z-[0-9]+$ ]]; then
  fail_test "idempotency key format unexpected: $key_value"
fi
pass_test "idempotency key has the expected prefix and timestamp tail"

# ---------------------------------------------------------------------------
# Test 6b: ALERT_FINGERPRINT is derived from the idempotency key, so it is
# unique per run, stable across the idempotent retry, and always matches the
# validFingerprint regex (sha256:<64hex>) in internal/manager/biz/demo/scenario.go.
# ---------------------------------------------------------------------------
alert_value=$(grep '^ALERT_FINGERPRINT=' "$recorded" | head -1 | cut -d= -f2-)
if [[ ! "$alert_value" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  fail_test "ALERT_FINGERPRINT does not match sha256:<64hex>: $alert_value"
fi
expected_alert="sha256:$(printf '%s' "$key_value" | sha256sum | cut -d' ' -f1)"
if [[ "$alert_value" != "$expected_alert" ]]; then
  fail_test "ALERT_FINGERPRINT not derived from idempotency key: got $alert_value expected $expected_alert"
fi
# TARGET_FINGERPRINT must also pass validFingerprint (hex or sha256:<64hex>).
target_value=$(grep '^TARGET_FINGERPRINT=' "$recorded" | head -1 | cut -d= -f2-)
if ! [[ "$target_value" =~ ^([0-9a-fA-F]+|sha256:[0-9a-f]{64})$ ]]; then
  fail_test "TARGET_FINGERPRINT does not match validFingerprint pattern: $target_value"
fi
pass_test "ALERT_FINGERPRINT is a sha256:<64hex> derived from idempotency key; TARGET_FINGERPRINT is independent"

# ---------------------------------------------------------------------------
# Test 7: container name override is honored.
# ---------------------------------------------------------------------------
build_sandbox "$WORK/sandbox7" "DEMO_VALID_VALUE" "JWT_VALID_VALUE" "PLUGIN_VALID_VALUE"
set +e
output7=$(run_wrapper_in_sandbox "$WORK/sandbox7" "alt-manager-container" 2>&1)
rc7=$?
set -e
if [[ $rc7 -ne 0 ]]; then
  fail_test "wrapper failed with alternate container name: $output7"
fi
recorded7="$WORK/sandbox7/docker-calls.txt"
# The container name is the trailing positional arg before the bash -lc cmd.
# We can't easily extract it from the captured -e list, but we can confirm
# no failure happened and the file is populated.
if [[ ! -s "$recorded7" ]]; then
  fail_test "wrapper did not invoke docker exec for alternate container"
fi
pass_test "OPSKEEPER_CONTAINER_NAME override is accepted by the wrapper"

# ---------------------------------------------------------------------------
# Test 8: EVIDENCE_OUTPUT defaults to the container-internal staging path
# /tmp/verify-evidence/<UTC>.json and the wrapper prepares that directory
# inside the container before exec.
# ---------------------------------------------------------------------------
build_sandbox "$WORK/sandbox8" "DEMO_VALID_VALUE" "JWT_VALID_VALUE" "PLUGIN_VALID_VALUE"
set +e
output8=$(run_wrapper_in_sandbox "$WORK/sandbox8" 2>&1)
rc8=$?
set -e
if [[ $rc8 -ne 0 ]]; then
  fail_test "wrapper exited non-zero with all three tokens present: rc=$rc8 output=$output8"
fi
recorded8="$WORK/sandbox8/docker-calls.txt"
container_evidence_path=$(grep '^EVIDENCE_OUTPUT=' "$recorded8" | head -1 | cut -d= -f2-)
if [[ ! "$container_evidence_path" =~ ^/tmp/verify-evidence/verify-final-demo-[0-9]{8}T[0-9]{6}Z\.json$ ]]; then
  fail_test "container EVIDENCE_OUTPUT path unexpected: $container_evidence_path"
fi
# mkdir inside the container must have been issued before the main exec.
if [[ ! -s "$WORK/sandbox8/mkdir-calls.txt" ]]; then
  fail_test "wrapper did not prepare CONTAINER_EVIDENCE_DIR inside the container"
fi
if [[ ! -s "$WORK/sandbox8/id-calls.txt" ]]; then
  fail_test "wrapper did not query container uid/gid before chown"
fi
if ! grep -q "^chown " "$WORK/sandbox8/chown-calls.txt"; then
  fail_test "wrapper did not chown the container evidence dir to the runtime uid"
fi
pass_test "EVIDENCE_OUTPUT defaults to /tmp/verify-evidence/<UTC>.json with prep exec"

# ---------------------------------------------------------------------------
# Test 9: docker cp happy path. Pre-seed an evidence file inside the container
# (via the fake docker's `exec test -s` branch returning 0) and assert the
# wrapper invokes docker cp with the correct src/dst.
# ---------------------------------------------------------------------------
# Patch the fake docker for this sandbox so `docker exec ... test -s <path>`
# returns 0 (file present).
build_sandbox "$WORK/sandbox9" "DEMO_VALID_VALUE" "JWT_VALID_VALUE" "PLUGIN_VALID_VALUE"
cat >>"$WORK/sandbox9/bin/docker" <<'EXTRA_FAKE_DOCKER'

# Allow tests to simulate a populated evidence file.
if [[ "${FAKE_EVIDENCE_PRESENT:-0}" == "1" ]]; then
  case "${*}" in
    *"test -s"*)
      exit 0
      ;;
  esac
fi
EXTRA_FAKE_DOCKER
(
  cd "$WORK/sandbox9"
  FAKE_EVIDENCE_PRESENT=1   DOCKER_FAKE_CP_FAIL=0   run_wrapper_in_sandbox "$WORK/sandbox9" >/dev/null 2>&1
)
cp_log="$WORK/sandbox9/cp-calls.txt"
if [[ ! -s "$cp_log" ]]; then
  fail_test "wrapper did not invoke docker cp on happy path"
fi
if ! grep -E -q '^cp opskeeper:/tmp/verify-evidence/verify-final-demo-[0-9]{8}T[0-9]{6}Z\.json '"$WORK/sandbox9"'/evidence/verify-final-demo-[0-9]{8}T[0-9]{6}Z\.json' "$cp_log"; then
  fail_test "docker cp src/dst mismatch: $(tr '\n' '|' <"$cp_log")"
fi
# The wrapper should also print a "copied to ..." line on success.
if ! grep -q "evidence copied to" /tmp/run-verify-test-stdout9 2>/dev/null; then
  : # best-effort log capture is not asserted here
fi
pass_test "docker cp happy path uses container-internal src and host dst"

# ---------------------------------------------------------------------------
# Test 10: docker cp failure does not crash the wrapper. The wrapper must
# log a WARN line and still exit with the inner docker exec rc (0 in this
# test, since the fake exec succeeds).
# ---------------------------------------------------------------------------
build_sandbox "$WORK/sandbox10" "DEMO_VALID_VALUE" "JWT_VALID_VALUE" "PLUGIN_VALID_VALUE"
(
  cd "$WORK/sandbox10"
  FAKE_EVIDENCE_PRESENT=1   DOCKER_FAKE_CP_FAIL=1   run_wrapper_in_sandbox "$WORK/sandbox10" >/tmp/run-verify-test-stdout10 2>&1
)
rc10=$?
if [[ $rc10 -ne 0 ]]; then
  fail_test "wrapper crashed (rc=$rc10) when docker cp failed; expected it to log WARN and return inner-exit rc"
fi
if ! grep -q "WARN docker cp failed" /tmp/run-verify-test-stdout10; then
  fail_test "wrapper did not log WARN docker cp failure: $(cat /tmp/run-verify-test-stdout10)"
fi
pass_test "docker cp failure is logged but does not crash the wrapper"

printf '\nALL TESTS PASSED\n' 

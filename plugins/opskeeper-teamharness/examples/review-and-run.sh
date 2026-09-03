#!/usr/bin/env bash
# review-and-run.sh — 为 6 个 opskeeper Worker CRs 注入 mcpServers[opskeeper]
#
# 这个脚本会被 AgentTeams Manager 侧的运维手工执行。**不修改 AgentTeams 仓库**，
# 只通过 kubectl patch 在外部更新 Worker CR（AgentTeams Controller 已支持 spec.mcpServers）。
#
# 必需环境变量：
#   AGENTTEAMS_NAMESPACE         AgentTeams 部署的 namespace（默认 agentteams）
#   OPSKEEPER_BACKEND_HOST       opskeeper Service external host
#   OPSKEEPER_TRANSPORT          http / sse（默认 http，即 StreamableHTTP）

set -euo pipefail

NAMESPACE="${AGENTTEAMS_NAMESPACE:-agentteams}"
OPSKEEPER_BACKEND_HOST="${OPSKEEPER_BACKEND_HOST:?required: opskeeper backend host}"
TRANSPORT="${OPSKEEPER_TRANSPORT:-http}"

OPSKEEPER_URL="https://${OPSKEEPER_BACKEND_HOST}/v1/mcp"

# 6 个 opskeeper Worker CR names（约定，与 agent-identities.yaml 对齐）
WORKERS=(
  worker-opskeeper-alerter
  worker-opskeeper-investigator
  worker-opskeeper-critic
  worker-opskeeper-reviewer
  worker-opskeeper-repairer
  worker-opskeeper-verifier
)

PATCH_SPEC=$(cat <<EOF
{"spec":{"mcpServers":[{"name":"opskeeper","url":"${OPSKEEPER_URL}","transport":"${TRANSPORT}"}]}}
EOF
)

echo ">>> Will patch ${#WORKERS[@]} Worker CRs in namespace '${NAMESPACE}'"
echo ">>> mcpServers entry: ${PATCH_SPEC}"
echo

for w in "${WORKERS[@]}"; do
  echo "kubectl -n ${NAMESPACE} patch worker ${w} --type=merge -p '${PATCH_SPEC}'"
done

echo
echo ">>> Above commands are PRINTED ONLY. Review carefully, then run them."
echo ">>> Or pipe this script to bash to execute:"
echo "    bash ${0} --execute"
echo

if [ "${1:-}" = "--execute" ]; then
  for w in "${WORKERS[@]}"; do
    kubectl -n "${NAMESPACE}" patch worker "${w}" --type=merge -p "${PATCH_SPEC}"
  done

  echo
  echo ">>> Triggering worker file-sync via Matrix Manager room..."
  kubectl -n "${NAMESPACE}" exec deploy/agentteams-manager -- \
    /opt/agentteams/agent/skills/worker-management/scripts/sync-workers.sh \
    || echo "WARN: file-sync trigger failed; workers may need manual restart"

  echo ">>> Verifying all 6 workers have opskeeper in mcpServers..."
  fail=0
  for w in "${WORKERS[@]}"; do
    has_opskeeper=$(kubectl -n "${NAMESPACE}" get worker "${w}" -o jsonpath='{.spec.mcpServers[*].name}' | grep -o opskeeper || true)
    if [ -z "$has_opskeeper" ]; then
      echo "FAIL: ${w} does not have opskeeper in mcpServers"
      fail=1
    fi
  done
  if [ "$fail" -ne 0 ]; then
    echo "ERROR: not all workers patched. Aborting." >&2
    exit 1
  fi
  echo ">>> All 6 workers verified. opskeeper plugin is now active."
fi

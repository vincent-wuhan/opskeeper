#!/bin/sh
set -eu

MATRIX_URL=http://127.0.0.1:18080
TOKEN_FILE=/root/final-demo-admin-matrix-token

cleanup() {
  rm -f "$TOKEN_FILE"
}
trap 'cleanup; exit 0' EXIT INT TERM

MATRIX_USER=$(docker inspect agentteams-manager --format '{{range .Config.Env}}{{println .}}{{end}}' |
  sed -n 's/^AGENTTEAMS_ADMIN_MATRIX_ID=//p' | head -n 1)
MATRIX_PASSWORD=$(docker inspect agentteams-manager --format '{{range .Config.Env}}{{println .}}{{end}}' |
  sed -n 's/^AGENTTEAMS_ADMIN_PASSWORD=//p' | head -n 1)
ROOM_ID=$(docker inspect opskeeper --format '{{range .Config.Env}}{{println .}}{{end}}' |
  sed -n 's/^OPSKEEPER_DEMO_MATRIX_ROOM=//p' | head -n 1)
MANAGER_TOKEN=$(docker exec agentteams-manager sh -ac 'printf %s "$AGENTTEAMS_MANAGER_MATRIX_TOKEN"')
MANAGER_ID=$(curl -sS -H "Authorization: Bearer $MANAGER_TOKEN" \
  "$MATRIX_URL/_matrix/client/v3/account/whoami" | jq -r '.user_id')

if [ -z "$MATRIX_USER" ] || [ -z "$MATRIX_PASSWORD" ] || [ -z "$ROOM_ID" ]; then
  echo "matrix actor configuration is incomplete" >&2
  exit 1
fi

login_body=$(jq -nc \
  --arg user "$MATRIX_USER" \
  --arg password "$MATRIX_PASSWORD" \
  '{type:"m.login.password",identifier:{type:"m.id.user",user:$user},password:$password}')
login_code=$(curl -sS -o /tmp/final-demo-human-login.json -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' --data "$login_body" \
  "$MATRIX_URL/_matrix/client/v3/login")
if [ "$login_code" != 200 ]; then
  echo "matrix login failed with HTTP $login_code" >&2
  exit 1
fi

umask 077
jq -r '.access_token' /tmp/final-demo-human-login.json > "$TOKEN_FILE"
room_path=$(jq -rn --arg value "$ROOM_ID" '$value|@uri')
manager_url="https://matrix.to/#/$MANAGER_ID"

send_message() {
  message_body=$1
  transaction_id=$2
  message=$(jq -nc \
    --arg body "$message_body" \
    --arg manager "$MANAGER_ID" \
    --arg manager_url "$manager_url" \
    '{msgtype:"m.text",body:$body,"m.mentions":{"user_ids":[$manager]}}')
  curl -sS -o /tmp/final-demo-human-send.json -w '%{http_code}' \
    -X PUT \
    -H "Authorization: Bearer $(cat "$TOKEN_FILE")" \
    -H 'Content-Type: application/json' \
    --data "$message" \
    "$MATRIX_URL/_matrix/client/v3/rooms/$room_path/send/m.room.message/$transaction_id"
}

latest_awaiting_incident() {
  docker exec opskeeper-postgres sh -ac \
    'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At -v ON_ERROR_STOP=1 -c \
     "select incident_id from demo_scenario_runs where status = '\''awaiting_approval'\'' and updated_at > now() - interval '\''15 minutes'\'' order by updated_at desc limit 1"' \
    2>/dev/null || true
}

end_at=$(($(date +%s) + 7200))
while [ "$(date +%s)" -lt "$end_at" ]; do
  incident_id=$(latest_awaiting_incident)
  [ -z "$incident_id" ] && continue

  seen_file="/tmp/final-demo-human-approval-$incident_id.seen"
  done_file="/tmp/final-demo-human-approval-$incident_id.done"
  [ -f "$done_file" ] && continue

  if [ ! -f "$seen_file" ]; then
    date +%s > "$seen_file"
    echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') awaiting explicit human approval incident=$incident_id"
  fi

  first_seen=$(cat "$seen_file" 2>/dev/null || echo 0)
  now=$(date +%s)
  if [ $((now - first_seen)) -ge 3 ] && [ ! -f "$seen_file.ambiguous" ]; then
    http_code=$(send_message \
      '这个方案看起来可以，先处理吧' \
      "final-demo-ambiguous-$incident_id-$now")
    if [ "$http_code" = 200 ]; then
      date +%s > "$seen_file.ambiguous"
      echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') submitted ambiguous approval incident=$incident_id"
    else
      echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') ambiguous approval send deferred incident=$incident_id http=$http_code"
    fi
  fi

  if [ -f "$seen_file.ambiguous" ]; then
    ambiguous_at=$(cat "$seen_file.ambiguous")
    now=$(date +%s)
    if [ $((now - ambiguous_at)) -ge 4 ]; then
      http_code=$(send_message \
        "@manager 已批准 incident_id=$incident_id Candidate A" \
        "final-demo-human-$incident_id-$now")
      if [ "$http_code" = 200 ]; then
        date +%s > "$done_file"
        echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') submitted explicit admin approval incident=$incident_id"
      else
        echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') explicit approval send deferred incident=$incident_id http=$http_code"
      fi
    fi
  fi

  sleep 1
done

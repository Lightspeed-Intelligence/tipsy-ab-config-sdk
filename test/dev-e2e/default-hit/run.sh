#!/usr/bin/env bash
# SDK dev-e2e: default-value hit log (platform task real-enrollment-dashboard,
# design part-c §C6).
#
# Brings up a DEDICATED local platform (own PG container, compiled server from a
# platform checkout), creates through the admin API:
#   - a namespace;
#   - a config key with a published version but NO full release;
#   - a running config_version experiment with two 50/50 groups: "configured"
#     (params {key: version}) and "empty-params" (params {}).
# It then runs the Go driver (this directory, workspace SDK under test) and
# asserts that its stdout contains `reason=default default_cause=no_full_release`.
#
# Usage (from anywhere):
#   TIPSY_REPO=/path/to/tipsy-ab-config bash test/dev-e2e/default-hit/run.sh
#
# Every port/name is overridable; the defaults are deliberately NOT the shared
# dev DB ports (15432 / 5433 / 25432 are refused). The PG container is created
# by this script and removed on exit BY THE ID `docker run` returned, so a
# same-named container this run did not create is never touched (KEEP=1 keeps
# ours and the server for debugging). An existing container with the same name
# is never reused.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
: "${TIPSY_REPO:?set TIPSY_REPO to a platform (tipsy-ab-config) checkout}"
TIPSY_REPO="$(cd "$TIPSY_REPO" && pwd)"

PG_CONTAINER="${PG_CONTAINER:-sdk-default-hit-pg}"
PG_PORT="${PG_PORT:-26409}"
HTTP_ADDR="${HTTP_ADDR:-:28280}"
GRPC_ADDR="${GRPC_ADDR:-:25252}"
METRICS_ADDR="${METRICS_ADDR:-:29291}"
PPROF_ADDR="${PPROF_ADDR:-127.0.0.1:26261}"
SERVICE_SECRET="${SERVICE_SECRET:-devsecret}"
CACHE_WAIT_S="${CACHE_WAIT_S:-12}"
KEEP="${KEEP:-0}"
WORK="${WORK:-$(mktemp -d "${TMPDIR:-/tmp}/sdk-default-hit.XXXXXX")}"
mkdir -p "$WORK/bin"

HTTP_PORT="${HTTP_ADDR##*:}"
GRPC_PORT="${GRPC_ADDR##*:}"
BASE="http://localhost:${HTTP_PORT}"
DSN="postgres://tipsy:tipsy@localhost:${PG_PORT}/tipsy?sslmode=disable"
ADMIN_UID=100001

log() { echo "[default-hit] $*" >&2; }
die() { echo "[default-hit] FAIL: $*" >&2; exit 1; }

case "$PG_PORT" in 15432 | 5433 | 25432) die "refusing shared dev DB port $PG_PORT" ;; esac
for c in docker go curl jq uuidgen lsof; do command -v "$c" >/dev/null 2>&1 || die "$c not found"; done
# Early, friendly check only. It is racy (another run can take the name right
# after it), so ownership never relies on it: see PG_CID below.
if docker ps -a --format '{{.Names}}' | grep -qx "$PG_CONTAINER"; then
  die "container $PG_CONTAINER already exists; remove it or set PG_CONTAINER (never reused)"
fi

# PG_CID is the ID `docker run -d` printed for the container THIS run created.
# Cleanup removes exactly that ID and nothing else. Two failure shapes matter:
#   - create fails (e.g. a concurrent run won the name): nothing is printed,
#     PG_CID stays empty, and the other run's same-named container is left alone;
#   - create succeeds but start fails (e.g. the host port is taken): docker
#     still prints our ID and exits 125, leaving our container in "Created".
#     PG_CID keeps that ID so cleanup removes it.
# Only a full 64-hex ID is ever kept, so cleanup never acts on a name or on
# arbitrary output.
PG_CID=""
SERVER_PID=""
cleanup() {
  if [[ "$KEEP" == "1" ]]; then
    log "KEEP=1: leaving server pid=${SERVER_PID:-none} and container ${PG_CID:-none} ($PG_CONTAINER); work dir $WORK"
    return
  fi
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
    log "stopped server pid $SERVER_PID"
  fi
  if [[ -z "$PG_CID" ]]; then
    log "no container created by this run; nothing removed; logs kept in $WORK"
  elif docker rm -f "$PG_CID" >/dev/null 2>&1; then
    log "removed container $PG_CID (created by this run); logs kept in $WORK"
  else
    log "WARN: could not remove container $PG_CID (created by this run); remove it by hand; logs kept in $WORK"
  fi
}
trap cleanup EXIT

# --- 1. dedicated PG -------------------------------------------------------
log "starting PG $PG_CONTAINER on :$PG_PORT"
RUN_RC=0
PG_CID="$(docker run -d --name "$PG_CONTAINER" -p "${PG_PORT}:5432" \
  -e POSTGRES_USER=tipsy -e POSTGRES_PASSWORD=tipsy -e POSTGRES_DB=tipsy \
  postgres:16-alpine -c jit=off)" || RUN_RC=$?
[[ "$PG_CID" =~ ^[0-9a-f]{64}$ ]] || PG_CID=""
if [[ "$RUN_RC" != "0" ]]; then
  if [[ -n "$PG_CID" ]]; then
    die "docker run created container $PG_CID but failed to start it (rc=$RUN_RC; port $PG_PORT taken?)"
  fi
  die "docker run failed (rc=$RUN_RC) before creating a container (name $PG_CONTAINER taken?)"
fi
[[ -n "$PG_CID" ]] || die "docker run succeeded but printed no container id"
log "created container $PG_CID"
# The postgres image's temporary init server listens on the unix socket only,
# then restarts; probing TCP waits for the final server.
for _ in $(seq 1 60); do
  docker exec "$PG_CID" pg_isready -h 127.0.0.1 -U tipsy -d tipsy >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$PG_CID" pg_isready -h 127.0.0.1 -U tipsy -d tipsy >/dev/null 2>&1 || die "PG not ready"
pg_query_ok=0
for _ in $(seq 1 30); do
  if docker exec "$PG_CID" psql -U tipsy -d tipsy -tAc 'select 1' >/dev/null 2>&1; then
    pg_query_ok=1
    break
  fi
  sleep 1
done
[[ "$pg_query_ok" == "1" ]] || die "PG accepts connections but 'select 1' never succeeded"

# --- 2. platform binaries + migrations -------------------------------------
log "building platform binaries from $TIPSY_REPO"
(cd "$TIPSY_REPO" && GOWORK=off go build -o "$WORK/bin/server" ./cmd/server \
  && GOWORK=off go build -tags devtools -o "$WORK/bin/devtoken" ./cmd/devtoken \
  && GOWORK=off go build -o "$WORK/bin/servicetoken" ./cmd/servicetoken)
log "migrate up"
(cd "$TIPSY_REPO" && DATABASE_URL="$DSN" "$WORK/bin/server" migrate up) >"$WORK/migrate.log" 2>&1 \
  || { tail -20 "$WORK/migrate.log" >&2; die "migrate up failed"; }
docker exec "$PG_CID" psql -U tipsy -d tipsy -v ON_ERROR_STOP=1 -qc \
  "INSERT INTO console_admin(user_id, note) VALUES (${ADMIN_UID}, 'sdk default-hit e2e') ON CONFLICT DO NOTHING;"

# --- 3. server -------------------------------------------------------------
log "starting server (HTTP $HTTP_ADDR, gRPC $GRPC_ADDR); log $WORK/server.log"
(cd "$TIPSY_REPO" && exec env \
  HTTP_ADDR="$HTTP_ADDR" GRPC_ADDR="$GRPC_ADDR" METRICS_ADDR="$METRICS_ADDR" PPROF_ADDR="$PPROF_ADDR" \
  TIPSY_SERVICE_SECRET="$SERVICE_SECRET" \
  TIPSY_CHAT_JWT_PUBLIC_KEY_PEM="$(cat scripts/fixtures/dev-jwt-public.pem)" \
  TIPSY_CHAT_JWT_AUDIENCE=dev TIPSY_CHAT_JWT_ISSUER=tipsy-backend \
  TIPSY_BACKEND_LOGIN_URL="${BASE}/login" TIPSY_BACKEND_LOGOUT_URL="${BASE}/logout" \
  DATABASE_URL="$DSN" \
  "$WORK/bin/server") >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 60); do
  curl -sf "${BASE}/healthz" >/dev/null 2>&1 && break
  kill -0 "$SERVER_PID" 2>/dev/null || { tail -20 "$WORK/server.log" >&2; die "server exited"; }
  sleep 1
done
curl -sf "${BASE}/healthz" >/dev/null || die "server /healthz not ready"
# Positively confirm it is OUR process answering, not something else on the port.
lsof -nP -iTCP:"$HTTP_PORT" -sTCP:LISTEN -a -p "$SERVER_PID" >/dev/null 2>&1 \
  || die "port $HTTP_PORT is not served by our server pid $SERVER_PID"

ADMIN_TOKEN="$(cd "$TIPSY_REPO" && "$WORK/bin/devtoken" --key scripts/fixtures/dev-jwt-private.pem --sub "$ADMIN_UID")"
[[ -n "$ADMIN_TOKEN" ]] || die "devtoken minted an empty token"

api() { # api METHOD PATH [JSON]
  local out code
  out="$(curl -sS -o "$WORK/api.body" -w '%{http_code}' -X "$1" -H "token: ${ADMIN_TOKEN}" \
    -H 'Content-Type: application/json' ${3:+--data "$3"} "${BASE}$2")"
  code="$out"
  [[ "$code" =~ ^2 ]] || die "$1 $2 -> $code $(cat "$WORK/api.body")"
  cat "$WORK/api.body"
}
lc_uuid() { uuidgen | tr '[:upper:]' '[:lower:]'; }

# --- 4. topology through the admin API -------------------------------------
NS="sdk_default_hit_$(date +%s)"
KEY="dh_controlled_key"
CFG_VALUE="configured-value"
LAYER_ID="$(lc_uuid)"
EXP_ID="$(lc_uuid)"
G_CFG="$(lc_uuid)"
G_EMPTY="$(lc_uuid)"
log "topology: ns=$NS key=$KEY experiment=$EXP_ID"

api POST /api/v1/namespaces "$(jq -nc --arg ns "$NS" '{namespace:$ns, description:"sdk default-hit e2e"}')" >/dev/null
api POST /api/v1/layers "$(jq -nc --arg ns "$NS" --arg id "$LAYER_ID" \
  '{namespace:$ns, domain_id:("root:"+$ns), id:$id, name:"default-hit-layer", salt:"", hash_fn:"xxhash", traffic_total:10000, admission:{}}')" >/dev/null
KEY_ID="$(api POST /api/v1/configs/keys "$(jq -nc --arg ns "$NS" --arg k "$KEY" \
  '{namespace:$ns, key:$k, category:"e2e", sub_category:"", description:"controlled, no full release", owner:"e2e"}')" | jq -r '.id | tostring')"
api POST /api/v1/configs/drafts "$(jq -nc --arg ns "$NS" --arg kid "$KEY_ID" --arg v "$CFG_VALUE" \
  '{namespace:$ns, key_id:$kid, value:$v}')" >/dev/null
VERSION_ID="$(api POST /api/v1/configs/publish "$(jq -nc --arg ns "$NS" --arg kid "$KEY_ID" \
  '{namespace:$ns, key_id:$kid}')" | jq -r '.id | tostring')"
# Deliberately NO POST /api/v1/configs/release/full: the key has no full release.

DRAFT="$(api POST /api/v1/experiments "$(jq -nc --arg ns "$NS" --arg layer "$LAYER_ID" --arg id "$EXP_ID" \
  --arg gc "$G_CFG" --arg ge "$G_EMPTY" --arg kid "$KEY_ID" --arg vid "$VERSION_ID" '{
    id:$id, namespace:$ns, layer_id:$layer, type:"config_version", name:"default-hit-exp",
    description:"e2e", owner:"e2e", salt:"", hash_fn:"xxhash", traffic_total:10000, admission:{},
    sticky_enabled:false, env:[],
    groups:[
      {id:$gc, name:"configured", traffic_range_lo:0, traffic_range_hi:4999, admission:{}, whitelist_uids:[],
       key_versions:[{key_id:$kid, version_id:$vid}]},
      {id:$ge, name:"empty-params", traffic_range_lo:5000, traffic_range_hi:9999, admission:{}, whitelist_uids:[]}
    ]}')")"
HASH_TYPE="$(jq -r '.hash_type' <<<"$DRAFT")"
VERSION="$(jq -r '.version | tostring' <<<"$DRAFT")"
if [[ "$HASH_TYPE" == "1" ]]; then
  # New-type experiment: traffic is per-group segments on the layer.
  api PATCH "/api/v1/experiments/${EXP_ID}" "$(jq -c --arg rv "$VERSION" --arg gc "$G_CFG" '{
      row_version:$rv,
      groups:[.groups[] | {id, name, params:(.params // {}),
        segments:[ if .id == $gc then {traffic_range_lo:0, traffic_range_hi:4999}
                   else {traffic_range_lo:5000, traffic_range_hi:9999} end ]}]}' <<<"$DRAFT")" >/dev/null
else
  api PATCH "/api/v1/experiments/${EXP_ID}/layer" "$(jq -nc --arg layer "$LAYER_ID" --arg rv "$VERSION" \
    '{new_layer_id:$layer, traffic_range_lo:0, traffic_range_hi:9999, row_version:$rv}')" >/dev/null
fi
VERSION="$(api GET "/api/v1/experiments/${EXP_ID}" | jq -r '.version | tostring')"
api POST "/api/v1/experiments/${EXP_ID}/start" "$(jq -nc --arg rv "$VERSION" '{row_version:$rv}')" >/dev/null
EXP="$(api GET "/api/v1/experiments/${EXP_ID}")"
[[ "$(jq -r '.status' <<<"$EXP")" == "running" ]] || die "experiment not running: $(jq -c '{status,hash_type}' <<<"$EXP")"
EMPTY_PARAMS="$(jq -c --arg ge "$G_EMPTY" '.groups[] | select(.id == $ge) | (.params // {})' <<<"$EXP")"
[[ "$EMPTY_PARAMS" == "{}" ]] || die "empty-params group has params: $EMPTY_PARAMS"
log "experiment running (hash_type=$HASH_TYPE); configured group $G_CFG, empty-params group $G_EMPTY"

SVC_TOKEN="$(cd "$TIPSY_REPO" && TIPSY_SERVICE_SECRET="$SERVICE_SECRET" \
  "$WORK/bin/servicetoken" --sub sdk-default-hit-e2e --namespaces "$NS" --ttl 1h)"
[[ -n "$SVC_TOKEN" ]] || die "servicetoken minted an empty token"

log "waiting ${CACHE_WAIT_S}s for the platform cache refresh"
sleep "$CACHE_WAIT_S"

# --- 5. driver (workspace SDK) ---------------------------------------------
log "running driver; stdout -> $WORK/driver.stdout"
set +e
(cd "$HERE" && AB_CONFIG_TOKEN="$SVC_TOKEN" go run . \
  -grpc "localhost:${GRPC_PORT}" -ns "$NS" -key "$KEY" -experiment "$EXP_ID" \
  -empty-group "$G_EMPTY" -configured-group "$G_CFG" -configured-value "$CFG_VALUE") \
  | tee "$WORK/driver.stdout"
DRIVER_RC=${PIPESTATUS[0]}
set -e
[[ "$DRIVER_RC" == "0" ]] || die "driver exited $DRIVER_RC (server log: $WORK/server.log)"

# The literal §C6 assertion, on the captured stdout.
grep -F "reason=default default_cause=no_full_release" "$WORK/driver.stdout" >/dev/null \
  || die "stdout lacks 'reason=default default_cause=no_full_release'"
log "PASS: stdout contains 'reason=default default_cause=no_full_release' (work dir $WORK)"

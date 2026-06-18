#!/usr/bin/env bash
# verify-roundrobin.sh — bring up a 3-instance ab-config topology, fan out
# load over `dns:///ab-config-headless.local:50051` via the SDK, then scrape
# each backend's `/metrics` and assert the per-backend gRPC call counts are
# within 10% of each other.
#
# Design references:
#   - design-doc §4.1 (alias topology is the only allowed shape).
#   - design-doc R4: docker DNS != k8s CoreDNS — this script validates the
#     SDK path, not the k8s CoreDNS contract.
#   - design-doc R5: 3-instance fan-out REQUIRES Redis sibling discovery;
#     docker-compose.headless.yml provides it.
#
# Environment / prereqs:
#   - rootless docker available.
#   - tipsy-ab-config-app:latest image present locally (cd ~/tipsy-ab-config && make up
#     once will build it).
#   - main repo's seed.sql + dev fixture PEM available at the relative paths
#     baked in below; override with TIPSY_REPO=/path/to/tipsy-ab-config if
#     they live elsewhere.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
TIPSY_REPO="${TIPSY_REPO:-$HOME/tipsy-ab-config}"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.headless.yml"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-tipsy-headless}"

PEM_PATH="${TIPSY_REPO}/scripts/fixtures/dev-jwt-public.pem"
SEED_SQL="${REPO_ROOT}/test/dev-e2e/sql/seed.sql"
SERVICE_SECRET="${TIPSY_SERVICE_SECRET:-devsecret}"
NAMESPACE="for_dev_agent_test"
LOAD_DURATION="${LOAD_DURATION:-30s}"
LOAD_CONCURRENCY="${LOAD_CONCURRENCY:-50}"

for f in "$PEM_PATH" "$SEED_SQL" "$COMPOSE_FILE"; do
	if [ ! -f "$f" ]; then
		echo "FATAL: required file missing: $f" >&2
		exit 2
	fi
done

if ! command -v docker >/dev/null 2>&1; then
	echo "FATAL: docker not on PATH" >&2
	exit 2
fi
# Detect compose CLI: prefer `docker compose` (v2 plugin); fall back to the
# standalone `docker-compose` binary if the plugin is missing.
if docker compose version >/dev/null 2>&1; then
	COMPOSE=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
	COMPOSE=(docker-compose)
else
	echo "FATAL: neither 'docker compose' nor 'docker-compose' available" >&2
	exit 2
fi

# Teardown trap — installed BEFORE `compose up` so failures anywhere
# downstream (healthcheck timeout, migrate failure, seed conflict, load driver
# crash, Ctrl-C, …) still tear the stack down. Repeated runs would otherwise
# leave PG volumes around → seed.sql with ON_ERROR_STOP=1 collides on rerun,
# and host port mappings (15433 / 16379 / 19091-3) would clash.
# Escape hatch: `export KEEP_STACK=1` to skip compose teardown for post-mortem
# debugging — the temp build dir under $LOAD_BIN_DIR is still removed either
# way so we don't leak /tmp entries.
LOAD_BIN_DIR=""
cleanup() {
	[ -n "$LOAD_BIN_DIR" ] && rm -rf "$LOAD_BIN_DIR"
	if [ "${KEEP_STACK:-0}" = "1" ]; then
		echo "==> KEEP_STACK=1 set, leaving compose stack up for debugging" >&2
		echo "    tear down manually with:" >&2
		echo "      ${COMPOSE[*]} -f $COMPOSE_FILE -p $COMPOSE_PROJECT down -v --remove-orphans" >&2
		return 0
	fi
	echo "==> tearing down compose stack ($COMPOSE_PROJECT)" >&2
	# `|| true` so a no-op down (e.g. early-exit before `up`) doesn't itself
	# make the trap fail and mask the original exit code.
	"${COMPOSE[@]}" -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# 1. Export compose env (PEM + secret) so the compose interpolation succeeds.
TIPSY_CHAT_JWT_PUBLIC_KEY_PEM="$(cat "$PEM_PATH")"
export TIPSY_CHAT_JWT_PUBLIC_KEY_PEM
export TIPSY_SERVICE_SECRET="$SERVICE_SECRET"

echo "================================================================"
echo "Headless round_robin verification"
echo "  compose      : $COMPOSE_FILE"
echo "  project      : $COMPOSE_PROJECT"
echo "  namespace    : $NAMESPACE"
echo "  duration     : $LOAD_DURATION"
echo "  concurrency  : $LOAD_CONCURRENCY"
echo "  alias        : ab-config-headless.local (3-way A-record fan-out)"
echo "================================================================"

# 2. Bring up the topology. Wait for all healthchecks.
"${COMPOSE[@]}" -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" up -d --wait

# 2a. Run migrations from the main repo (the app image bundles the migration
#     files but does NOT auto-apply on startup; production drives goose
#     separately via `tipsy-ab-config migrate up`). Idempotent.
echo "==> applying migrations"
( cd "$TIPSY_REPO" && DATABASE_URL="postgres://tipsy:tipsy@localhost:15433/tipsy?sslmode=disable" \
	go run ./cmd/server migrate up >/dev/null )

# 3. Seed the database (idempotent; safe to re-run).
echo "==> seeding database"
"${COMPOSE[@]}" -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T db \
	psql -U tipsy -d tipsy -v ON_ERROR_STOP=1 < "$SEED_SQL" >/dev/null

# Give the abtest cache one refresh cycle (5s) to pick up seeded rows.
sleep 6

# 4. Sign a local token (uses the same TIPSY_SERVICE_SECRET as the app containers).
echo "==> signing service token"
TOKEN="$(cd "$TIPSY_REPO" && TIPSY_SERVICE_SECRET="$SERVICE_SECRET" go run ./cmd/servicetoken --sub headless-roundrobin --namespaces '*' --ttl 1h 2>/dev/null | tail -1)"
if [ -z "$TOKEN" ]; then
	echo "FATAL: failed to mint service token" >&2
	exit 2
fi

# 5. Drive load over `dns:///ab-config-headless.local:50051` from INSIDE the
#    docker network so docker's embedded DNS returns all 3 backend IPs as A
#    records (matches k8s Headless Service behavior closely enough to exercise
#    the SDK's auto-injected round_robin service config).
echo "==> running gRPC load driver inside the docker network"
LOAD_BIN_DIR="$(mktemp -d)"
# CGO_ENABLED=0 → fully static ELF so it runs in the bare alpine sidecar
# without needing libc.
CGO_ENABLED=0 go build -o "$LOAD_BIN_DIR/roundrobin-load" "${SCRIPT_DIR}/roundrobin-load"

docker run --rm \
	--network "tipsy-headless-net" \
	-v "$LOAD_BIN_DIR/roundrobin-load:/usr/local/bin/roundrobin-load:ro" \
	-e AB_CONFIG_TOKEN="$TOKEN" \
	alpine:3 \
	/usr/local/bin/roundrobin-load \
	-target "dns:///ab-config-headless.local:50051" \
	-ns "$NAMESPACE" \
	-concurrency "$LOAD_CONCURRENCY" \
	-duration "$LOAD_DURATION"

# 6. Scrape per-instance grpc_requests_total counters and compute the spread.
echo "==> scraping per-instance gRPC call counts"
metric_for() {
	local port=$1
	curl -sf "http://localhost:${port}/metrics" \
		| awk '/^tipsy_abconfig_grpc_requests_total\{/ && /AbtestService\/GetExperimentResult/ && /code="OK"/ {sum+=$NF} END {printf "%d\n", sum+0}'
}

C1=$(metric_for 19091)
C2=$(metric_for 19092)
C3=$(metric_for 19093)
TOTAL=$((C1 + C2 + C3))

if [ "$TOTAL" -eq 0 ]; then
	echo "FAIL: no GetExperimentResult/code=OK observations on any backend" >&2
	echo "  (this means the load driver did not reach the backends; check token/seed/DNS)" >&2
	exit 1
fi

MAX=$C1; [ $C2 -gt $MAX ] && MAX=$C2; [ $C3 -gt $MAX ] && MAX=$C3
MIN=$C1; [ $C2 -lt $MIN ] && MIN=$C2; [ $C3 -lt $MIN ] && MIN=$C3
AVG=$(( TOTAL / 3 ))
SPREAD_NUM=$(( MAX - MIN ))
# percentage relative to the average (×10000 then /100 to keep integer math).
SPREAD_PCT_X100=$(( SPREAD_NUM * 10000 / AVG ))

echo ""
echo "================================================================"
echo "per-instance gRPC AbtestService/GetExperimentResult (code=OK)"
echo "  app1 (19091) : $C1"
echo "  app2 (19092) : $C2"
echo "  app3 (19093) : $C3"
echo "  total        : $TOTAL"
echo "  avg          : $AVG"
echo "  max - min    : $SPREAD_NUM"
printf "  spread       : %d.%02d%% of avg (target <10.00%%)\n" $((SPREAD_PCT_X100/100)) $((SPREAD_PCT_X100%100))
echo "================================================================"

if [ $SPREAD_PCT_X100 -lt 1000 ]; then
	echo "PASS"
	exit 0
else
	echo "FAIL: spread $SPREAD_PCT_X100 (×100, percent) exceeds 10% target"
	exit 1
fi

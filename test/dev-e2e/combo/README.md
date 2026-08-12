# Combo and holdout data-plane E2E

This local suite verifies combo/holdout routing through released Go, Python and
Java SDKs. Combo has no SDK-specific wire message: the service returns ordinary
experiment/config results, and the clients prove that those results resolve to
the intended frozen or live values.

The suite requires a platform checkout because it creates topology through the
admin API and runs a real server. Use a dedicated database; never point these
fixtures or mutation checks at a shared development database.

## Automated coverage

| Check | Observation |
|---|---|
| frozen versus live | all three SDKs over HTTP and gRPC return a held group's frozen version and a non-held key's live full release. |
| create-time layer migration | all 60 recorded users remain accounted for: users retained by the migrated layer keep their group and sliced users land in a real holdout-opt group. |
| runtime layer migration | migrate-out and migrate-in preserve per-user groups, expose the expected immediate topology, and switch resolved values with the topology. |
| route pin | a domain route whitelist moves the selected user to the pinned target and the Go SDK returns the target value. |

The checked-in runners do not automate proportional-distribution or zero-width
group sampling. Those are current manual checks described below. Frozen/live
resolution uses all three SDKs because only the client consumes
`has_dynamic_resolution` and decides whether an experiment RPC is needed.

## Non-degeneracy contract

These fixture properties are part of the test, not incidental data:

- A held group's frozen version differs from the current full release.
- The client checks the change in `abtest_fallback`, not an absolute counter,
  and requires a zero delta. Otherwise the correct-looking value could come
  from a fallback instead of the experiment path.
- Migrated experiment groups use asymmetric shares. Symmetric shares allow a
  wrong salt to retain the same group too often.
- Pre-migration and pre-pin anchors are read from the live server. Predicting
  them with the same solver as the implementation would be tautological.
- Every checked-in driver checks its own observation count. The manual sampling
  checks require every meaningful arm to be non-empty.
- Migration accounting covers all 60 users. "Not in the original experiment"
  is insufficient because it cannot distinguish a valid holdout-opt result
  from an unrouted user.

For the manual proportional check, `N=4000` and `p=0.10` give
`sigma=sqrt(p*(1-p)/N)=0.474 percentage points`; use the four-sigma interval
`0.0810..0.1190` for the combined holdout-opt share. This is an operator
criterion, not an assertion in a checked-in runner. At this sample size, a
systematic shift below roughly 2–3 percentage points may remain below the
observation floor.

## Prerequisites

- Platform repository with a current server, migrations, development JWT
  fixture, `cmd/devtoken`, and `cmd/servicetoken`.
- PostgreSQL and Redis available to the platform server.
- Go matching the SDK repository, Python 3.10–3.13, JDK 21 and Maven.
- Free local ports 8081, 50052, 9099 and 6099.
- Two admin identities: a normal admin and a `bootstrap` admin. Bootstrap is
  required because combo node ids are otherwise redacted.
- Separate credentials:
  - admin API: RS256 session from `cmd/devtoken`;
  - data plane: HS256 service token from `cmd/servicetoken`.

Run Go commands in a platform worktree with `GOWORK=off` when its sibling SDK
path is unavailable.

## Local platform

The following is a template; adapt the PostgreSQL container name and platform
repository path. It deliberately uses a dedicated `tipsy_sdk_e2e` database.

```bash
# In the platform repository.
docker exec <pg-container> psql -U tipsy -d postgres -c \
  'CREATE DATABASE tipsy_sdk_e2e OWNER tipsy;'

DATABASE_URL='postgres://tipsy:tipsy@localhost:15432/tipsy_sdk_e2e?sslmode=disable' \
  GOWORK=off go run ./cmd/server migrate up

docker exec <pg-container> psql -U tipsy -d tipsy_sdk_e2e -c \
  "INSERT INTO console_admin(user_id,note) VALUES
     (100001,'e2e'),(100002,'e2e bootstrap') ON CONFLICT DO NOTHING;
   UPDATE console_admin SET roles='[\"bootstrap\"]'::jsonb
     WHERE user_id=100002;"

HTTP_ADDR=:8081 GRPC_ADDR=:50052 \
METRICS_ADDR=:9099 PPROF_ADDR=127.0.0.1:6099 \
TIPSY_SERVICE_SECRET=devsecret \
TIPSY_CHAT_JWT_PUBLIC_KEY_PEM="$(cat scripts/fixtures/dev-jwt-public.pem)" \
TIPSY_CHAT_JWT_AUDIENCE=dev \
TIPSY_CHAT_JWT_ISSUER=tipsy-backend \
TIPSY_BACKEND_LOGIN_URL=http://localhost:8081/login \
TIPSY_BACKEND_LOGOUT_URL=http://localhost:8081/logout \
DATABASE_URL='postgres://tipsy:tipsy@localhost:15432/tipsy_sdk_e2e?sslmode=disable' \
REDIS_ADDR=localhost:6379 \
GOWORK=off go run ./cmd/server
```

Start the server after the source being tested was last modified and wait for
its health endpoint. Stop the entire process group before restarting; killing
only a `go run` wrapper can leave the compiled child serving old code.

In another platform-repository shell:

```bash
export ADMIN_SESSION="$(GOWORK=off go run -tags devtools ./cmd/devtoken --sub 100001)"
export BOOTSTRAP="$(GOWORK=off go run -tags devtools ./cmd/devtoken --sub 100002)"
export AB_CONFIG_TOKEN="$(TIPSY_SERVICE_SECRET=devsecret GOWORK=off \
  go run ./cmd/servicetoken --sub combo-sdk-e2e \
  --namespaces st9_combo --ttl 6h)"
```

The checked-in client expectations and `server/anchor.tsv` belong to one exact
seeded topology. Read [fixture.md](./fixture.md) before rebuilding it: current
platform code gives structural nodes random id suffixes, and those ids are
effective bucketing salts. Creating a fresh topology therefore requires
regenerating candidate users, the anchor and all hard-coded expectations as one
coordinated change; merely substituting newly returned ids does not reproduce
the checked-in suite.

## Run

Run these commands from `test/dev-e2e/combo` unless noted otherwise:

```bash
# Frozen/live: each language runs HTTP and plaintext gRPC.
(cd clients/go && GOWORK=off go run .)
AB_CONFIG_TOKEN="$AB_CONFIG_TOKEN" \
  /path/to/python/bin/python clients/py/run.py
(cd clients/java && mvn -q -DskipTests package && \
  java -jar target/st9-combo-java.jar)

# Route pin.
(cd clients/go && GOWORK=off go run ./pin)

# Create-time migration accounting.
bash server/migration_accounting.sh \
  --anchor server/anchor.tsv \
  --emig <experiment-id> \
  --ho-exp <holdout-opt-experiment-id>

# Runtime migrate-out / migrate-in round trip.
ADMIN_SESSION="$ADMIN_SESSION" AB_CONFIG_TOKEN="$AB_CONFIG_TOKEN" \
bash server/runtime_migration.sh \
  --anchor server/anchor.tsv \
  --combo-id <combo-id> \
  --layer-id <layer-id> \
  --emig <experiment-id> \
  --ho-exp <holdout-opt-experiment-id> \
  --workdir /tmp/tipsy-combo-runtime-migration

# Reverse-solve candidate users for one combo arm.
(cd tools/solve && GOWORK=off go run . \
  <combo-layer-id> <holdout-opt-experiment-id> h1)
```

The Go combo client is pinned to SDK v0.8.0, and the Java combo client to
v0.6.0, by their executable dependency manifests. These are compatibility
baselines, not current-version recommendations. The Python client uses the
active Python environment, so install either the repository package or the
specific release intended for the run before invoking it.

## Manual distribution checks

There is no checked-in runner for these two checks. Run them only after reading
the fixture reproducibility limitation below. They work with a preserved
matching topology or a freshly built topology whose current ids are supplied;
they do not make the hard-coded SDK cases portable to newly randomized ids.

Use the bootstrap-authorized combo list to obtain the current structural ids
and group ids. Export the holdout-opt experiment id and group ids for `c1`:

```bash
curl -fsS -H "Authorization: Bearer $BOOTSTRAP" \
  'http://localhost:8081/api/v1/combos?namespace=st9_combo'

export C1_HO_EXP_ID='<c1 holdout_opt_experiment_id>'
export C1_H_GROUP_ID='<c1 h1 group id>'
export C1_O_GROUP_ID='<c1 o1 group id>'
```

Then sample 4,000 users through the real data-plane endpoint. This command
fails unless all responses are recognized, both holdout-opt groups are
non-empty, and their combined share is within `0.0810..0.1190`:

```bash
python3 - <<'PY'
import json, os, urllib.request

base = os.environ.get("AB_CONFIG_HTTP_BASE", "http://localhost:8081")
token = os.environ["AB_CONFIG_TOKEN"]
experiment = os.environ["C1_HO_EXP_ID"]
known = {os.environ["C1_H_GROUP_ID"]: "h1", os.environ["C1_O_GROUP_ID"]: "o1"}
counts = {"simple": 0, "h1": 0, "o1": 0, "unknown": 0}
for i in range(1, 4001):
    body = json.dumps({
        "namespace": "st9_combo", "user_id": f"p-{i}",
        "experiment_type": "EXPERIMENT_TYPE_CONFIG_VERSION",
        "display_type": "RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP",
    }).encode()
    req = urllib.request.Request(
        base + "/api/v1/abtest/experiment_result", data=body,
        headers={"Authorization": "Bearer " + token,
                 "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as response:
        groups = json.load(response).get("groups") or []
    hits = [known.get(g.get("group_id"), "unknown")
            for g in groups if g.get("experiment_id") == experiment]
    if not hits:
        counts["simple"] += 1
    elif len(hits) == 1:
        counts[hits[0]] += 1
    else:
        counts["unknown"] += 1
share = (counts["h1"] + counts["o1"]) / 4000
print(counts, "holdout_opt_share=", share)
assert counts["unknown"] == 0 and counts["h1"] > 0 and counts["o1"] > 0
assert 0.0810 <= share <= 0.1190
PY
```

For the zero-width check, export the current `c3zero` holdout-opt experiment
and group ids, including the materialized zero-width group, then sample 400
users. This command fails if the zero-width group receives a user, either
non-zero group is unobserved, or a response is unrecognized:

```bash
export C3_HO_EXP_ID='<c3zero holdout_opt_experiment_id>'
export C3_ZERO_GROUP_ID='<c3zero h_zero group id>'
export C3_H_GROUP_ID='<c3zero h_live group id>'
export C3_O_GROUP_ID='<c3zero o_live group id>'

python3 - <<'PY'
import json, os, urllib.request

base = os.environ.get("AB_CONFIG_HTTP_BASE", "http://localhost:8081")
token = os.environ["AB_CONFIG_TOKEN"]
experiment = os.environ["C3_HO_EXP_ID"]
known = {
    os.environ["C3_ZERO_GROUP_ID"]: "zero",
    os.environ["C3_H_GROUP_ID"]: "h_live",
    os.environ["C3_O_GROUP_ID"]: "o_live",
}
counts = {"simple": 0, "zero": 0, "h_live": 0, "o_live": 0, "unknown": 0}
for i in range(1, 401):
    body = json.dumps({
        "namespace": "st9_combo", "user_id": f"z-{i}",
        "experiment_type": "EXPERIMENT_TYPE_CONFIG_VERSION",
        "display_type": "RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP",
    }).encode()
    req = urllib.request.Request(
        base + "/api/v1/abtest/experiment_result", data=body,
        headers={"Authorization": "Bearer " + token,
                 "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as response:
        groups = json.load(response).get("groups") or []
    hits = [known.get(g.get("group_id"), "unknown")
            for g in groups if g.get("experiment_id") == experiment]
    if not hits:
        counts["simple"] += 1
    elif len(hits) == 1:
        counts[hits[0]] += 1
    else:
        counts["unknown"] += 1
print(counts)
assert counts["zero"] == 0 and counts["unknown"] == 0
assert counts["h_live"] > 0 and counts["o_live"] > 0
PY
```

## Cache and mutation rules

- Wait at least 11 seconds after platform writes before data-plane assertions.
- A direct SQL change to configuration data does not advance
  `namespace_snapshot_seq`; without that invalidation, the SDK can retain the
  old snapshot indefinitely even while an HTTP dynamic endpoint sees new data.
  Configuration-plane mutations must advance the sequence exactly as the
  production write path does.
- Experiment-plane mutations are observed through the abtest cache refresh and
  do not use the configuration snapshot sequence.
- Do not name Bash variables `GROUPS`, `UID`, `PATH` or other shell-reserved
  names. `GROUPS` in particular is a Bash builtin and silently corrupts parsed
  group lists.
- Rebuild `clients/java/target/st9-combo-java.jar` whenever Java source or its
  pinned SDK dependency changes.
- A failure must retain the topology and captured work directory long enough to
  identify whether the cause is the platform, stale process, expired token,
  invalid fixture, or SDK behavior.

## Evidence limits

### Pending human verification: reproducible fixture bootstrap

The repository does not currently contain a bootstrap that recreates the exact
structural ids/salts used by the checked-in candidate users and anchor. The
admin API intentionally generates fresh random suffixes, so a blank database
cannot run the checked-in expectations as-is. Until fixture creation and
expectation generation are automated together, this suite needs its matching
preserved topology or a manually regenerated, jointly reviewed fixture. Do not
interpret a failure against an independently rebuilt same-named topology as an
SDK regression.

For the simple arm, the protocol exposes absence of a combo group rather than a
positive "simple" field. The manual commands compensate by requiring recognized
responses, non-empty sibling arms and complete sample accounting, but no more
direct observation exists at this API layer.

The runtime migration script exercises the successful key re-judgment path.
Conflict/rollback behavior belongs to platform integration tests and is not
proved by this SDK-level run.

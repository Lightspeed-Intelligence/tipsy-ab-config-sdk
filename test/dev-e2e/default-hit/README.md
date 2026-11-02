# Default-value hit log E2E

Local end-to-end check for the Go SDK's `get_config hit (default)` line. It comes from platform task real-enrollment-dashboard, design part-c §C6 ("SDK 仓的 dev-e2e").

## Scenario

On a REAL local platform, a user who lands in an experiment's **empty-parameter group** reads a key with these properties:
- the experiment controls the key: another group configures it, so `has_dynamic_resolution=true`;
- the key has **no full release**.

The SDK must return the caller's default and print exactly one line like this:

```text
level=INFO msg="tipsyabconfig: get_config hit (default)" ns=… key=… reason=default default_cause=no_full_release uid=… trace_id=…
```

A control user in the configured group must get the configured value with a `reason=experiment` hit line. That proves the experiment is live and the key really is controlled. Without it, a default line could also be explained by `fast_path`, `abtest_unavailable` or `unknown_key`.

Group membership comes from the live server: the driver calls the public `GetExperimentResult` (`EACH_EXPERIMENT_GROUP`) for deterministic candidate uids. It is never predicted locally. The driver also requires the empty group to arrive with empty `params_versions` on the wire.

## Run

```bash
TIPSY_REPO=/path/to/tipsy-ab-config bash test/dev-e2e/default-hit/run.sh
```

`run.sh`:
1. starts a dedicated `postgres:16-alpine` container;
2. builds `cmd/server`, `cmd/devtoken` and `cmd/servicetoken` from `TIPSY_REPO` with `GOWORK=off`;
3. runs `migrate up` and seeds `console_admin(100001)`;
4. starts the server and checks that the HTTP port is served by its own pid;
5. creates the topology through the admin API (namespace, layer, key + published version **without** a full release, running 50/50 experiment);
6. waits for the cache refresh;
7. runs the driver with the workspace SDK, so the code under test is this tree, not a released version;
8. greps the captured stdout for `reason=default default_cause=no_full_release`.

On exit it stops the server and removes the container it created, on success and on any later failure. `KEEP=1` keeps both for debugging.

Overridable environment variables, with defaults:

| Variable | Default |
|---|---|
| `PG_CONTAINER` | `sdk-default-hit-pg` |
| `PG_PORT` | `26409` |
| `HTTP_ADDR` | `:28280` |
| `GRPC_ADDR` | `:25252` |
| `METRICS_ADDR` | `:29291` |
| `PPROF_ADDR` | `127.0.0.1:26261` |
| `CACHE_WAIT_S` | `12` |
| `WORK` | a fresh `mktemp -d`; keeps server, migrate and driver logs |

Safety rules:
- The shared dev DB ports 15432, 5433 and 25432 are refused.
- An existing container named `PG_CONTAINER` is never reused.
- Cleanup removes the container by the ID `docker run -d` printed, never by name, and only a full 64-hex ID is kept:
  - **Create fails** (two runs race for the same name): the loser's `docker run` prints no ID, so its exit removes nothing and the winner's container survives.
  - **Create succeeds but start fails** (the host port is taken): docker still prints the new container's ID and exits 125, leaving it in `Created`. The script keeps that ID, so its exit removes the container it created.
  - Verified 2026-10-31 by running:
    - a pre-existing same-named container survived both the early name check and a simulated race (a `docker` shim hiding `docker ps`); the original name-based cleanup deleted it under the same race;
    - with the port held by another container, a run under a fresh name failed with "created container … but failed to start it" and removed exactly its own container, leaving the port holder running;
    - an ordinary run, and a run failing after creation (platform build error), both removed the owned container; `KEEP=1` kept it;
    - a run whose `select 1` never succeeded (a shim failing `psql`) failed with that message and removed its container.
- PG readiness probes TCP (`pg_isready -h 127.0.0.1`): the postgres image's temporary init server listens on the unix socket only and then restarts, so a socket probe can pass too early.
- Redis is not needed: the server runs single-node in the default `open` topology-rollout mode.

## Prerequisites

- Docker, Go (the repository `go.work` version), `curl`, `jq`, `uuidgen`, `lsof` (used to confirm the HTTP port is served by the script's own server process).
- A platform checkout providing `scripts/fixtures/dev-jwt-{public,private}.pem`.

## Evidence limits

- **Go only:** the downstream SLS ingestion parses only the Go format (part-c §C4). Python and Java default lines are covered by their unit tests against the shared scenario list `test/sdk-alignment/default_cause_scenarios.txt`.
- **One cause:** this run exercises only `no_full_release`. The other six `default_cause` values are covered by the unit tests.
- **Workspace mode only:** `GOWORK=off` resolves the pinned release in `go.mod`, which predates the default line, and the run fails.
- **Negative control:** with the SDK at the pre-change commit, the same run fails with "want exactly one get_config hit line, got 0". That reproduces part-c §C0.

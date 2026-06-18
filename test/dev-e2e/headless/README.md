# Headless multi-instance round_robin verification

This sub-suite validates the SDK's new behavior (v0.4.0 / v0.5.0):
**when the gRPC dial target uses the `dns:///` scheme, the SDK auto-injects a
`round_robin` load-balancing service config**, and grpc-go's `dns:///` resolver
fans out across every A record returned by DNS — distributing successive RPCs
across all backend instances behind a single hostname.

It is the implementation of acceptance criterion #4 in
[`design-doc.md` §Acceptance Criteria](../../../.zyz-worker/tasks/sdk-headless-roundrobin/design/design-doc.md).

## Topology (alias-locked by design §4.1)

```
                         dns:///ab-config-headless.local:50051
                                       |
              docker embedded DNS A-record list (3 IPs, single name)
                                       |
                  +--------------------+--------------------+
                  |                    |                    |
              app1 :50051         app2 :50051         app3 :50051
                  \                    |                    /
                   +--- shared PG / Redis fan-out (sibling Notify) ---+
```

- **Single hostname → 3 container IPs** via docker compose `networks.aliases`.
  Docker's embedded DNS server returns all 3 IPs as separate A records when
  this alias is resolved — functionally equivalent to a Kubernetes Headless
  Service (`clusterIP: None`) returning all matching pod IPs.
- **Verifier runs inside the same docker network** (alpine sidecar via
  `docker run --network ...`). Running on the host would force the load
  driver to consult `/etc/hosts` (no sudo here) or `nss-myhostname`, neither
  of which give the multi-A-record list that docker DNS provides.
- **Redis is mandatory** (design risk R5): multi-instance ab-config fan-out
  for Subscribe push depends on `internal/api/grpc/configservice/notifier.go`
  using Redis sibling discovery. Without Redis the cluster is silently broken
  for any Subscribe traffic — even though this verification only exercises
  unary RPCs, omitting Redis would still misrepresent the production topology
  this test is supposed to model.

### What is intentionally NOT tested here (design §4.1 prohibitions)

The design explicitly **forbids** the "3 different ports on localhost" shape.
That shape (e.g. `app1:51001`, `app2:51002`, `app3:51003` with a hardcoded
client list) would:

- Bypass `dns:///` resolver code entirely.
- Pass even if the SDK's round_robin injection is broken or missing.
- Validate nothing about the actual SDK behavior change.

If you need to debug a single backend in isolation, edit the docker-compose
file directly; do **not** rewrite the verifier to talk to multiple ports.

## Risk R4 caveat — docker DNS is not k8s CoreDNS

Docker's embedded DNS server is reused here as a stand-in for CoreDNS because
it provides the same "alias name → A-record list" semantics that the SDK's
`dns:///` resolver requires to exercise the round_robin LB code path. It is
**not** a full replacement:

| dimension | docker embedded DNS | k8s CoreDNS |
|---|---|---|
| A-record list for shared alias | yes | yes |
| DNS TTL | 600 s default (compose-level) | 5 s (pod records) |
| SRV record support | partial (service-name only) | full pods + headless |
| record refresh cadence on pod IP change | slow (waits for TTL) | fast (CoreDNS watches the api server) |
| record ordering | round-robin per query | randomized per query |

The SDK's `dns:///` resolver re-resolves every 30 s by default; both docker
and CoreDNS comfortably satisfy that contract. **This harness validates the
SDK-side behavior. The CoreDNS-side contract must be validated separately in
a real cluster.**

## How to run

```sh
# Prereqs:
#   - rootless docker daemon running
#   - tipsy-ab-config-app:latest image present locally
#     (cd ~/tipsy-ab-config && make up   # builds it once)
#   - cmd/servicetoken available in ~/tipsy-ab-config (used to mint the test
#     HS256 token under the same TIPSY_SERVICE_SECRET as the containers).

bash test/dev-e2e/headless/verify-roundrobin.sh
```

Script flow:

1. Generates compose env (PEM + secret) from `~/tipsy-ab-config/scripts/fixtures/dev-jwt-public.pem`.
2. `docker compose up -d --wait` brings up `db` + `redis` + `app1` + `app2` + `app3`,
   waiting for every container's healthcheck.
3. Seeds the database from `test/dev-e2e/sql/seed.sql`.
4. Mints a HS256 service token via the main repo's `cmd/servicetoken`.
5. Builds `roundrobin-load/` as a CGO-free static ELF.
6. Runs the load binary inside an `alpine:3` sidecar on the same docker
   network, dialing `dns:///ab-config-headless.local:50051` (the SDK injects
   `round_robin` automatically because of the `dns:///` prefix).
7. After the load run, scrapes each backend's `/metrics` over the host-mapped
   ports `19091` / `19092` / `19093` and parses
   `tipsy_abconfig_grpc_requests_total{method=".../AbtestService/GetExperimentResult",code="OK"}`.
8. Asserts `(max - min) / avg < 10%` and exits non-zero otherwise.

## Tunable env vars

| var | default | meaning |
|---|---|---|
| `TIPSY_REPO` | `$HOME/tipsy-ab-config` | path to the main platform repo (for PEM + cmd/servicetoken) |
| `TIPSY_SERVICE_SECRET` | `devsecret` | HS256 secret shared between the containers and the test token |
| `LOAD_DURATION` | `30s` | load run length |
| `LOAD_CONCURRENCY` | `50` | concurrent gRPC callers |
| `COMPOSE_PROJECT` | `tipsy-headless` | docker compose project name (also the network prefix) |

## Teardown

The script tears the compose stack down automatically on exit — success,
failure, or `Ctrl-C` all go through the same `trap cleanup EXIT INT TERM`,
which runs:

```sh
docker compose -f test/dev-e2e/headless/docker-compose.headless.yml \
  -p tipsy-headless down -v --remove-orphans
```

You do not need to run anything by hand under normal conditions.

### Keeping the stack up for debugging

If a run fails and you want to inspect container logs / DB state before
teardown, export `KEEP_STACK=1` before invoking the script:

```sh
KEEP_STACK=1 bash test/dev-e2e/headless/verify-roundrobin.sh
```

With `KEEP_STACK=1` the trap skips `compose down` (the temp build dir under
`$TMPDIR` is still cleaned up). When you're done debugging, tear the stack
down manually using the command shown above.

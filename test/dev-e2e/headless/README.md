# Headless Service round-robin verification

This suite verifies the Go SDK's current `dns:///` behavior against three
service instances sharing one DNS name. A `dns:///` target automatically
selects gRPC `round_robin`; the load driver then confirms that all three
backends receive calls with less than 10% spread from the mean.

The load driver is pinned to Go SDK v0.4.0 by
`roundrobin-load/go.mod`. That is an executable compatibility baseline, not the
recommended current SDK version.

## Topology

```text
roundrobin-load
      |
      | dns:///ab-config-headless.local:50051
      v
Docker DNS: three A records
      |------------|------------|
     app1         app2         app3
       \            |            /
        +------ PostgreSQL ------+
        +--------- Redis --------+
```

Redis is required for multi-instance Subscribe notification fan-out. The
verification itself drives `GetExperimentResult`, while keeping the deployment
topology valid for the SDK's complete cache/update model.

Docker embedded DNS is sufficient to exercise the SDK resolver and load
balancer, but it is not a substitute for validating Kubernetes CoreDNS TTL,
record rotation or cluster networking. A real cluster remains the observation
point for those platform properties.

Using separate localhost ports would bypass a multi-address DNS answer and
would not test this behavior.

## Prerequisites

- Docker with Compose v2 or the `docker-compose` binary.
- A locally built `tipsy-ab-config-app:latest` image.
- A checkout of the platform repository containing:
  - `scripts/fixtures/dev-jwt-public.pem`
  - `cmd/server` for local migrations
  - `cmd/servicetoken` for a short-lived HS256 test token
- Go matching this repository's `go.work` version.

The script creates a dedicated Compose database on host port 15433, Redis and
three application containers. Metrics are exposed on ports 19091–19093.

## Run

```bash
TIPSY_REPO=/path/to/tipsy-ab-config \
bash test/dev-e2e/headless/verify-roundrobin.sh
```

The script:

1. starts the Compose stack and waits for health checks;
2. applies the platform migrations and the SDK repository's E2E seed;
3. mints an in-memory one-hour service token;
4. builds the load driver and runs it inside `tipsy-headless-net` against
   `dns:///ab-config-headless.local:50051`;
5. scrapes each backend's successful `GetExperimentResult` counter;
6. fails if no calls were observed, the driver error rate exceeds 1%, or the
   backend spread is 10% or more.

The stack is removed on normal exit and failure.

## Settings

| Variable | Default | Purpose |
|---|---|---|
| `TIPSY_REPO` | `$HOME/tipsy-ab-config` | Platform repository path. |
| `TIPSY_SERVICE_SECRET` | `devsecret` | Secret shared by the local server and generated token. |
| `COMPOSE_PROJECT` | `tipsy-headless` | Compose project name. |
| `LOAD_DURATION` | `30s` | Load duration. |
| `LOAD_CONCURRENCY` | `50` | Concurrent callers. |
| `KEEP_STACK` | `0` | Set to `1` to retain the stack for diagnosis. |

When `KEEP_STACK=1`, remove the retained resources with:

```bash
docker compose \
  -f test/dev-e2e/headless/docker-compose.headless.yml \
  -p tipsy-headless down -v --remove-orphans
```

No token or credential is written to tracked files.

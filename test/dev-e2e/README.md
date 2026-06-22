# dev e2e test harness

End-to-end tests against the **DEV** environment of the tipsy-ab-config platform
(Go AB/config service: gRPC + HTTP, plus Go/Python SDKs). This directory holds
**test-harness artifacts only** — it never modifies product code under
`sdk/`, `api/`.

The original design source is `.zyz-worker/tasks/e2e-dev-tests/design/design-doc.md`
in the upstream platform repo (`Lightspeed-Intelligence/tipsy-ab-config`). The
public SDK repo carries this suite as the downstream-consumer-side evidence that
the released SDK modules (`sdk/go/tipsyabconfig`, `sdk/go/tipsyauth`, the Python
`tipsy-ab-config` package) drive the same platform correctly.

## Layout

- `tools/bucketfind/main.go` — reverse-solves deterministic UIDs for each
  experiment-group target by replicating the platform bucket formula
  `bucket = xxhash64(uid + "-" + salt) % traffic_total` at BOTH the layer level
  and the experiment level, then emits the golden expectations fixture.
- `fixtures/expectations.json` — generated golden expectations (one row per
  `(ns, user_id, key)`); the test drivers assert against these.
- `sql/seed.sql` — idempotent, transaction-wrapped seed for the `demo-test` and
  `for_dev_agent_test` namespaces (config + gray + two experiment types).
- `sql/teardown.sql` — idempotent FK-safe cleanup of this batch's rows.
- `platform/` — ST3 platform-correctness driver: raw HTTP (Go, stdlib only) +
  `grpc_smoke.sh` (raw gRPC via grpcurl).
- `clients/go/` — ST4 Go SDK client-correctness driver (gRPC + HTTP transports).
- `clients/py/` — ST4 Python SDK client-correctness driver (gRPC + HTTP) plus
  `setup_venv.sh` venv bootstrap.
- `clients/java/` — Java SDK client-correctness driver (gRPC + HTTP); a
  standalone Maven project (NOT in the `sdk/java` reactor) depending on the
  locally-installed `io.github.lightspeed-intelligence:tipsy-abconfig` artifact. Build a fat-jar with
  `mvn -q -DskipTests package`; see `clients/java/README.md`.
- `load/` — ST5 medium-load driver (Go, stdlib only); writes `load/last-run.json`.

## Environment variables (all access info; no secrets hard-coded)

Every driver reads access info from env vars (matching `docs/dev-http-token.md`):

| var | default | used by |
|---|---|---|
| `AB_CONFIG_HTTP_BASE` | `https://dev-ab-config.infra.fantacy.live` | all HTTP paths |
| `AB_CONFIG_GRPC_ADDR` | `dev-ab-config-grpc.infra.fantacy.live:443` | gRPC paths (standard TLS via Cloudflare-proxied DNS) |
| `AB_CONFIG_TOKEN` | _(REQUIRED, no default)_ | all paths |
| `AB_CONFIG_GRPC_AUTHORITY` | _(unset)_ | **legacy opt-in**: if set, switches gRPC to the deprecated IP-direct form (`-authority` override + `-insecure`) for origin-path debugging |
| `AB_CONFIG_GRPC_CA_PEM` | _(unset)_ | **legacy opt-in**: Origin CA PEM for the IP-direct Python path |

The dev token (and its expiry) are in `docs/dev-http-token.md`. Export them
before running any driver, e.g.:

```sh
export AB_CONFIG_TOKEN='<dev service token from docs/dev-http-token.md>'
```

## Prerequisites (one-time, per host)

To install the **released** SDKs the way `tipsy-backend` does (private GitHub repo
+ tagged module), the Agent's host needs SSH access to the repo and `git` set
up to use it for HTTPS module fetches:

```sh
# 1. Agent's SSH key must already work for: git clone git@github.com:Lightspeed-Intelligence/tipsy-ab-config-sdk.git
# 2. Redirect github HTTPS → SSH so `go get` and pip git+https flows authenticate too:
git config --global url."ssh://git@github.com/".insteadOf "https://github.com/"
# 3. Tell Go this org is private (skip proxy/sumdb):
export GOPRIVATE='github.com/Lightspeed-Intelligence/*'
# 4. Tools the drivers need:
go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest   # raw-gRPC smoke
# Python 3.12 must be available; the venv bootstrap uses /usr/bin/python3.12.
# (System python 3.14 lacks grpcio wheels; override with PYTHON312=<path> if needed.)
```

## Bucketfind tool

Regenerate the expectations fixture (deterministic; safe to re-run):

```
(cd test/dev-e2e/tools/bucketfind && go run .)
```

It prints a summary table of the reverse-solved UIDs and writes
`test/dev-e2e/fixtures/expectations.json`. The tool is module-local — it imports
`github.com/cespare/xxhash/v2` (already in go.sum) and depends on no product code.

## Run order (against DEV)

The dev abtest cache does an **unconditional full reload every 5 seconds**
(upstream platform `internal/abtest/cache/refresher.go`), so directly-inserted
DB rows are picked up by the running dev server within <=5s — no restart, no
admin write call. The Agent has **no DB access**; the user runs the SQL.

1. **Seed** the dev database (user runs):

   ```
   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f test/dev-e2e/sql/seed.sql
   ```

   `seed.sql` COMMITs the data and then runs two SELF-CHECK SELECTs that must
   return **zero rows** (derived `experiment_config_param` matches
   `experiment_group.params`, and `experiment_status='running'`).

   Skip this step if a prior seed is still in place (`seed.sql` is idempotent;
   re-running is cheap but unnecessary).

2. **Wait >= 5 seconds** for the dev cache to reload.

3. **Run the test drivers** (in this order):

   **ST3 — platform correctness (raw wire):**

   ```sh
   # raw HTTP (config/static + config/dynamic + abtest/experiment_result)
   (cd test/dev-e2e/platform && go run .)

   # raw gRPC (grpcurl; install: go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest)
   bash test/dev-e2e/platform/grpc_smoke.sh
   ```

   **ST4 — client correctness (SDKs, both transports):**

   The SDKs are released as standalone Go modules and a published-tag Python
   package on this public repo (see `docs/usage-and-integration.md §4.1.0` and
   `sdk/python/README.md §Consumer onboarding`). The drivers support two
   install modes — pick one per SDK:

   - **workspace / editable** (default; fast in-repo iteration): Go uses the
     repo `go.work` (which lists every dev-e2e driver module), Python uses
     `pip install -e sdk/python[http]`.
   - **backend / released** (mirrors `tipsy-backend`'s real integration): Go
     does `GOWORK=off go run .` against the `v0.4.0` tag pulled via the public
     module proxy; Python uses
     `pip install ... @ git+ssh://...@python-sdk/v0.5.0#subdirectory=sdk/python`.
     This is the authoritative "consumer onboarding works" evidence.

   ```sh
   # ----- Go SDK (workspace mode; quick) -----
   (cd test/dev-e2e/clients/go && go run .)
   # or a single transport:
   (cd test/dev-e2e/clients/go && go run . -transport http)

   # ----- Go SDK (backend mode; released v0.4.0 via public proxy) -----
   # No GOPRIVATE needed — the SDK repo is public.
   ( cd test/dev-e2e/clients/go && \
     GOWORK=off go run . -fixtures ../../fixtures/expectations.json )

   # ----- Python SDK (editable; quick) -----
   bash test/dev-e2e/clients/py/setup_venv.sh                    # creates .venv/
   test/dev-e2e/clients/py/.venv/bin/python test/dev-e2e/clients/py/run.py

   # ----- Python SDK (backend mode; released v0.5.0 via git+ssh) -----
   SDK_MODE=backend bash test/dev-e2e/clients/py/setup_venv.sh   # creates .venv-backend/
   test/dev-e2e/clients/py/.venv-backend/bin/python test/dev-e2e/clients/py/run.py

   # ----- Java SDK (locally-installed io.github.lightspeed-intelligence:tipsy-abconfig) -----
   (cd sdk/java && mvn -q -DskipTests install)                   # one-time: SDK → ~/.m2
   (cd test/dev-e2e/clients/java && mvn -q -DskipTests package)  # build fat-jar
   AB_CONFIG_TOKEN=... java -jar test/dev-e2e/clients/java/target/tipsy-dev-e2e-java.jar
   # or a single transport:
   AB_CONFIG_TOKEN=... java -jar test/dev-e2e/clients/java/target/tipsy-dev-e2e-java.jar --transport http
   ```

   **ST5 — medium load test:**

   ```sh
   # defaults: 150 concurrency, 150s, experiment_result endpoint, for_dev_agent_test ns
   (cd test/dev-e2e/load && go run .)
   # rate-limited (recommended to stay "medium"):
   (cd test/dev-e2e/load && go run . -target-qps 2000 -duration 120s)
   # writes summary metrics → test/dev-e2e/load/last-run.json (auto)
   ```

   Each Go driver prints `PASS`/`FAIL` per case and a summary, and exits
   non-zero on any failure. The load driver exits non-zero if the error rate
   exceeds the 1% acceptance target.

4. **Teardown** when finished (user runs):

   ```
   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f test/dev-e2e/sql/teardown.sql
   ```

   Then wait >= 5s again; the trailing regression SELECT should report no
   leftover keys/experiments/layers in the two namespaces.

## Transport notes (gRPC on dev)

- **Default path** (since `docs: update dev grpc access guidance`): dev has a
  dedicated Cloudflare-proxied gRPC DNS record
  `dev-ab-config-grpc.infra.fantacy.live:443` with standard TLS. All three
  drivers default to this address — **no `:authority` override, no
  skip-verify, no Origin CA PEM needed**. The Go SDK addr is just
  `grpcs://dev-ab-config-grpc.infra.fantacy.live:443`; grpcurl needs only
  `-H "authorization: Bearer ..."`.
- **Legacy IP-direct path** (deprecated; only for origin-path debugging):
  setting `AB_CONFIG_GRPC_AUTHORITY` (any non-empty value) flips all three
  drivers back to the old form (`-authority` override + `-insecure` /
  `grpcs://IP:443?authority=...&insecure=true`). The Python driver then also
  honors `AB_CONFIG_GRPC_CA_PEM` for grpcio (no native skip-verify). Default
  is empty → standard TLS via domain.
- Each SDK driver tolerates a gRPC connect failure: it WARNs, marks gRPC
  **degraded** in the summary (a visible non-success), and still runs HTTP mode.


## Notes

- Both SQL scripts are idempotent (`ON CONFLICT ... DO UPDATE/DO NOTHING` on
  seed; namespace + fixed-id-band scoped DELETEs on teardown) and safe to re-run.
- Teardown deliberately **keeps** the `namespace_registry` rows and the
  auto-created root domain (`root:<ns>`), leaving the namespaces usable.
- Schema columns are verified against migrations `0001`, `0005`, `0006`;
  `experiment_group` has **no `is_control`** column (dropped by `0006`).
- Bare HTTP/gRPC `user_attrs` must use the typed Value envelope, e.g.
  `{"country":{"s":"US"}}` (not `{"country":"US"}`) — the SDKs encode this
  automatically; only the raw-interface drivers write it by hand. The
  expectations fixture already stores attrs in this envelope form.
- This directory is **not** part of `make test` / `go test ./...` (the tests hit
  a live dev environment); the bucketfind tool is a standalone `package main`.

## Expected results

Reference numbers from the last green run (see `RESULTS.md` for the full
report; if any number is off, that's the failure signal):

| driver | what it asserts | expected |
|---|---|---|
| `platform/` (raw HTTP) | 38 fixture rows × {dynamic, exp_result} + static + sticky | **75/75 PASS** |
| `platform/grpc_smoke.sh` | reflection + 4 RPCs (ConfigService + AbtestService) | **5/5 PASS** |
| `clients/go/` | 38 rows × {http, grpc} = 76 | **76/76 PASS** (both modes) |
| `clients/py/` | 38 rows × {http, grpc} = 76 | **76/76 PASS** (both modes) |
| `clients/java/` | 38 rows × {http, grpc} = 76 | **76/76 PASS** (driver ready; awaits a valid `AB_CONFIG_TOKEN` + seeded DEV to run) |
| `load/` (medium) | 90–150s @ 150 workers, error-rate < 1% | 0 errors, p50 ≈ 220ms, p99 ≈ 340ms |

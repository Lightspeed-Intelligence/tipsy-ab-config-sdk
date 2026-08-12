# DEV end-to-end harness

This directory validates the platform's public HTTP/gRPC data plane and the Go,
Python and Java SDKs against the same seeded expectations. It contains test
drivers and fixtures only; it does not modify SDK or proto implementation.

The normal target is a deployment supplied by the platform owner. The endpoint,
token and database seed state are external inputs; this repository does not
claim that any particular DEV deployment is currently online.

## Suites

| Path | Purpose |
|---|---|
| `sql/` | Idempotent seed and teardown SQL for `demo-test` and `for_dev_agent_test`. |
| `fixtures/expectations.json` | Golden `(namespace, user, key)` expectations shared by all drivers. |
| `tools/bucketfind/` | Regenerates deterministic fixture users with the platform bucket formula. |
| `platform/` | Raw HTTP assertions and a grpcurl smoke test. |
| `clients/go/` | Go SDK over HTTP and gRPC. |
| `clients/py/` | Python SDK over HTTP and gRPC, with editable and released-baseline setup modes. |
| `clients/java/` | Standalone Maven consumer over HTTP and gRPC. |
| `load/` | Medium HTTP load driver; writes `last-run.json`. |
| `headless/` | Local three-backend `dns:///` and `round_robin` verification. |
| `combo/` | Local combo/holdout data-plane fixture and SDK assertions. |

The harness is intentionally outside normal `go test`, Python and Maven unit
test suites because it needs a real service and seeded database.

## Connection contract

All drivers take access data from environment variables:

| Variable | Default | Use |
|---|---|---|
| `AB_CONFIG_HTTP_BASE` | `https://dev-ab-config.infra.fantacy.live` | HTTP base URL. Override with the endpoint supplied for the run. |
| `AB_CONFIG_GRPC_ADDR` | `dev-ab-config-grpc.infra.fantacy.live:443` | gRPC TLS address. Override with the endpoint supplied for the run. |
| `AB_CONFIG_TOKEN` | none, required | HS256 service token authorized for the fixture namespaces. |
| `AB_CONFIG_GRPC_AUTHORITY` | unset | Origin-debugging override for the legacy direct-IP form. |
| `AB_CONFIG_GRPC_CA_PEM` | unset | Private CA PEM used by the Python direct-IP path. |

Obtain current endpoints and a short-lived token from the deployment owner; see
[DEV connection template](../../docs/dev-http-token.md). Never commit a token.

```bash
export AB_CONFIG_HTTP_BASE='<current HTTP base URL>'
export AB_CONFIG_GRPC_ADDR='<current gRPC host:port>'
export AB_CONFIG_TOKEN='<short-lived service token>'
```

The default gRPC form is standard TLS through the configured DNS name. Set the
authority/private-CA variables only when an operator explicitly asks for
origin-path diagnosis. A gRPC initialization failure is reported as degraded
and makes an SDK-driver run non-successful even if HTTP assertions continue.

## Prerequisites

- Go matching the repository `go.work` version.
- Python 3.10–3.13; set `PYTHON312=/path/to/python` if the bootstrap cannot find
  its default Python 3.12 executable.
- JDK 21 and Maven for the Java driver.
- `grpcurl` for the raw gRPC smoke test.
- Database access for the operator running `sql/seed.sql` and
  `sql/teardown.sql`.

The SDK repository is public. Released Go modules can be fetched through the
public Go proxy, Python releases through public Git tags over HTTPS, and Java
releases through Maven Central. No `GOPRIVATE` or GitHub credential is required
for those public release paths. The checked-in Python `SDK_MODE=backend`
bootstrap is a legacy exception: it installs its pinned tag through
`git+ssh://` and therefore requires working GitHub SSH access.

## Run against a deployment

From the SDK repository root:

1. Have the database operator apply the fixture:

   ```bash
   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f test/dev-e2e/sql/seed.sql
   ```

   The script is transaction-wrapped and finishes with self-check queries. Wait
   at least one platform cache-refresh interval before running clients.

2. Run raw protocol checks:

   ```bash
   (cd test/dev-e2e/platform && go run .)
   bash test/dev-e2e/platform/grpc_smoke.sh
   ```

3. Run SDK consumers:

   ```bash
   # Go: repository workspace implementation, both transports.
   (cd test/dev-e2e/clients/go && go run .)

   # Go: isolated released-consumer baseline.
   (cd test/dev-e2e/clients/go && GOWORK=off go run . \
     -fixtures ../../fixtures/expectations.json)

   # Python: repository implementation.
   bash test/dev-e2e/clients/py/setup_venv.sh
   test/dev-e2e/clients/py/.venv/bin/python \
     test/dev-e2e/clients/py/run.py

   # Python: isolated released-consumer baseline.
   SDK_MODE=backend bash test/dev-e2e/clients/py/setup_venv.sh
   test/dev-e2e/clients/py/.venv-backend/bin/python \
     test/dev-e2e/clients/py/run.py

   # Java: current repository implementation installed to the local Maven repo.
   JAVA_SDK_VERSION="$(cd sdk/java && \
     mvn -q help:evaluate -Dexpression=project.version -DforceStdout)"
   (cd sdk/java && mvn -q -DskipTests install)
   (cd test/dev-e2e/clients/java && mvn -q -DskipTests \
     -Dtipsy-abconfig.version="$JAVA_SDK_VERSION" package)
   java -jar test/dev-e2e/clients/java/target/tipsy-dev-e2e-java.jar
   ```

   Add `-transport http|grpc` to the Go driver or `--transport http|grpc` to
   Python and Java to run one transport.

   The isolated Go/Python modes are deliberately pinned by their executable
   dependency files: Go `clients/go/go.mod` uses SDK v0.4.0 and Python
   `clients/py/setup_venv.sh` uses `python-sdk/v0.5.0`. They are compatibility
   baselines, not recommended current versions. The Java standalone harness is
   similarly pinned by `clients/java/pom.xml`; update a pin and its lock/build
   metadata together when changing the baseline.

4. Optionally run medium HTTP load:

   ```bash
   (cd test/dev-e2e/load && go run . \
     -target-qps 2000 -duration 120s)
   ```

   Defaults are 150 workers, 150 seconds, 2000 target QPS and
   `experiment_result`. The driver fails above a 1% error rate and writes a
   JSON result to `test/dev-e2e/load/last-run.json`.

5. Have the database operator remove the fixture:

   ```bash
   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f test/dev-e2e/sql/teardown.sql
   ```

## Expected observation counts

With the checked-in 38-row fixture and both transports enabled:

| Driver | Expected observations |
|---|---:|
| raw HTTP platform driver | 75 |
| grpcurl smoke | 5 |
| each Go/Python/Java SDK driver | 76 |

Treat a different total as an incomplete run, not merely a failed assertion.
This is currently an operator gate: the Go, Python and Java drivers print their
totals but do not compare them with 76. They exit non-zero for assertion
failures or a degraded gRPC transport, so an exit status of zero must still be
paired with the expected-count check above.
Historical result snapshots are intentionally not maintained in the current
documentation; rerun the harness against the target deployment.

## Fixture maintenance

Regenerate deterministic expectations with:

```bash
(cd test/dev-e2e/tools/bucketfind && go run .)
```

Raw HTTP/gRPC attributes use the typed `Value` envelope, for example
`{"country":{"s":"US"}}`. SDK callers pass native values such as
`{"country":"US"}` because each language performs the wire conversion.

The SQL is idempotent and scoped to the fixture namespaces and fixed id band.
Teardown deliberately retains namespace-registry rows and the generated root
domain so the namespaces remain usable.

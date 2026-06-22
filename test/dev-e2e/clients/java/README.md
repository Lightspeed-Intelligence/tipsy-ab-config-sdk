# Java SDK dev-e2e driver (`clients/java/`)

DEV-environment client-correctness driver for the **Tipsy AB-config Java SDK**
(`io.github.lightspeed-intelligence:tipsy-abconfig`). This is the Java counterpart of
[`clients/go/`](../go/) and [`clients/py/`](../py/): one invocation exercises
**both transports** (gRPC and HTTP) against the seeded DEV topology and asserts
every applicable row of [`../../fixtures/expectations.json`](../../fixtures/expectations.json).

Client tags: `java_sdk_grpc` / `java_sdk_http` (already part of `bucketfind`'s
`allClients`, so every fixture row's `applies_to` includes them).

This directory is a **standalone Maven project** (no `<parent>`): it depends on
the *released* SDK artifact `io.github.lightspeed-intelligence:tipsy-abconfig` exactly the way a
downstream consumer would, and is **deliberately NOT part of the `sdk/java`
reactor** (test-harness only; it never touches product code). There is no
`<repositories>` block — once the SDK is published, the dependency resolves
from **Maven Central** like any other public artifact.

## What it asserts

Mirrors the Go driver's `assert.go` one-for-one:

- **config-version rows** (`key != "__custom__"`): builds an `AbtestContext` via
  `newAbtestContext(uid, rawAttrs)` and asserts
  `getConfig(abctx, ns, key, "<DEFAULT>")` equals the fixture `expected_value`
  (a string).
- **custom rows** (`key == "__custom__"`): calls
  `getExperimentResult(CUSTOM_PARAMS, FLAT_KV, traceId="dev-e2e-java-<client>-<ns>-<uid>")`
  and deep-compares `custom_flat_kv` (a `google.protobuf.Struct`) to the fixture
  `expected_value` object, with numeric tolerance (`10` == `10.0`).

`rawAttrs` decodes the fixture's typed-Value envelope (`{"country":{"s":"US"}}`)
back into the raw map the SDK expects (`{"country":"US"}`) — the SDK re-encodes
raw values into `abtestv1.Value` on the wire, so passing the envelope would
double-wrap and break admission matching.

## Build

The harness resolves `io.github.lightspeed-intelligence:tipsy-abconfig` the way a real business
consumer does. Two modes:

**A. Published jar from Maven Central (the real business-consumer simulation).**
Once the SDK is released (`java-sdk/vX.Y.Z` tag → CI `mvn deploy -Prelease`),
the dependency is on Maven Central — no local install, no custom repo. Just set
`<tipsy-abconfig.version>` in `pom.xml` to the released version and build:

```sh
# (optionally first prove nothing is cached locally, so the build MUST hit Central)
rm -rf ~/.m2/repository/io/github/lightspeed-intelligence/tipsy-abconfig
(cd test/dev-e2e/clients/java && mvn -q -DskipTests package)   # pulls io.github.lightspeed-intelligence:tipsy-abconfig from Central
# → target/tipsy-dev-e2e-java.jar
```

**B. Local pre-release build (`~/.m2`).** Before a release is on Central, build
the SDK reactor and install it locally first:

```sh
(cd sdk/java && mvn -q -DskipTests install)                    # → ~/.m2/io/github/lightspeed-intelligence/...
(cd test/dev-e2e/clients/java && mvn -q -DskipTests package)
```

Either way you get `target/tipsy-dev-e2e-java.jar`.

The fat-jar bundles `grpc-netty-shaded`, `protobuf-java-util`, Jackson (fixture
parsing) and `slf4j-simple` (so the SDK's own WARN/ERROR surface on stderr). The
shade plugin merges gRPC's `META-INF/services/*` SPI files so the
NameResolver / LoadBalancer providers are discovered after shading.

## Environment variables

Same contract as the Go/Python drivers (never hard-code secrets):

| var | default | notes |
|---|---|---|
| `AB_CONFIG_HTTP_BASE` | `https://dev-ab-config.infra.fantacy.live` | HTTP transport base URL |
| `AB_CONFIG_GRPC_ADDR` | `dev-ab-config-grpc.infra.fantacy.live:443` | gRPC addr (SDK uses `grpcs://` + this, standard TLS) |
| `AB_CONFIG_TOKEN` | _(REQUIRED, no default)_ | HS256 service token; missing → `FATAL` + exit 2 |
| `AB_CONFIG_GRPC_AUTHORITY` | _(unset)_ | **legacy opt-in**: switches gRPC to the deprecated IP-direct form (`grpcs://addr?authority=...&insecure=true`) |

The DEV token (and its expiry) are in [`docs/dev-http-token.md`](../../../../docs/dev-http-token.md).
The driver runs `create(... startupFailOpen(true) ...)` so a transient startup
pull failure does not abort; a gRPC connect failure (or an empty startup cache)
WARNs, marks gRPC **degraded** in the summary, and continues HTTP mode rather
than crashing.

## Run

Requires a valid `AB_CONFIG_TOKEN` and a DEV database that has been seeded
(`test/dev-e2e/sql/seed.sql`, run by the user) — wait ≥5s after seeding for the
dev cache to reload.

```sh
# fat-jar (recommended)
AB_CONFIG_TOKEN='<dev service token>' \
  java -jar test/dev-e2e/clients/java/target/tipsy-dev-e2e-java.jar

# or via Maven exec (no packaging needed)
AB_CONFIG_TOKEN='<dev service token>' \
  (cd test/dev-e2e/clients/java && mvn -q exec:java)

# single transport
AB_CONFIG_TOKEN=... java -jar .../tipsy-dev-e2e-java.jar --transport http
AB_CONFIG_TOKEN=... java -jar .../tipsy-dev-e2e-java.jar --transport grpc

# explicit fixtures path (otherwise it walks up to find
# test/dev-e2e/fixtures/expectations.json)
AB_CONFIG_TOKEN=... java -jar .../tipsy-dev-e2e-java.jar \
  --fixtures test/dev-e2e/fixtures/expectations.json
```

The driver prints `PASS`/`FAIL` per case and a `SUMMARY` line, and **exits
non-zero** on any failure or if the gRPC transport was degraded. Expected on a
seeded DEV with a valid token: 38 rows × {http, grpc} = **76/76 PASS** (parity
with the Go/Python drivers).

## Notes

- The SDK itself is zero-extra-dependency; this *harness* is not bound by that
  rule, so it uses Jackson for the fixtures parse and `slf4j-simple` as the
  logging backend.
- Tests are skipped (`maven.test.skip=true`); this module has no unit tests, it
  is a runnable driver.
- Like the other client drivers, this is **not** part of `mvn test` /
  `go test ./...` — it hits a live DEV environment.

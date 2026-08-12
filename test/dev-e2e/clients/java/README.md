# Java SDK DEV E2E driver

This standalone Maven project exercises the Java SDK over HTTP and gRPC against
the shared fixture in
[`fixtures/expectations.json`](../../fixtures/expectations.json). It is not part
of the `sdk/java` reactor and consumes `tipsy-abconfig` the same way as an
external application.

The driver checks:

- configuration rows with `newAbtestContext` and `getConfig`;
- custom-parameter rows with `getExperimentResult` and a deep protobuf value
  comparison;
- native Java attributes converted to the typed wire `Value` form;
- a printed observation count for each selected transport.

## SDK dependency

`pom.xml` pins the artifact version used by this harness. The pin is an
executable compatibility baseline, not a current-version recommendation.

For the published baseline, build directly and Maven resolves the artifact from
Central:

```bash
cd test/dev-e2e/clients/java
mvn -q -DskipTests package
```

To exercise current repository source, install the Java reactor and override
the harness property with the matching local version:

```bash
JAVA_SDK_VERSION="$(cd sdk/java && \
  mvn -q help:evaluate -Dexpression=project.version -DforceStdout)"
(cd sdk/java && mvn -q -DskipTests install)
(cd test/dev-e2e/clients/java && mvn -q -DskipTests \
  -Dtipsy-abconfig.version="$JAVA_SDK_VERSION" package)
```

The output is `target/tipsy-dev-e2e-java.jar`. The shade configuration preserves
gRPC service-provider metadata required for name resolvers and load balancers.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `AB_CONFIG_HTTP_BASE` | `https://dev-ab-config.infra.fantacy.live` | HTTP base URL; override with the endpoint supplied for the run. |
| `AB_CONFIG_GRPC_ADDR` | `dev-ab-config-grpc.infra.fantacy.live:443` | gRPC TLS host and port; override with the endpoint supplied for the run. |
| `AB_CONFIG_TOKEN` | none, required | Short-lived HS256 service token authorized for fixture namespaces. |
| `AB_CONFIG_GRPC_AUTHORITY` | unset | Legacy direct-origin diagnostic override. |

Obtain current endpoints and credentials from the deployment owner using the
[DEV connection template](../../../../docs/dev-http-token.md). The driver uses
`startupFailOpen(true)` so HTTP checks can still run after a gRPC startup
failure, but it marks gRPC degraded and exits non-zero.

## Run

After the deployment has the fixture from `test/dev-e2e/sql/seed.sql` and its
cache has refreshed:

```bash
export AB_CONFIG_TOKEN='<short-lived service token>'

# Both transports.
java -jar test/dev-e2e/clients/java/target/tipsy-dev-e2e-java.jar

# One transport.
java -jar test/dev-e2e/clients/java/target/tipsy-dev-e2e-java.jar \
  --transport http
java -jar test/dev-e2e/clients/java/target/tipsy-dev-e2e-java.jar \
  --transport grpc

# Explicit fixture path.
java -jar test/dev-e2e/clients/java/target/tipsy-dev-e2e-java.jar \
  --fixtures test/dev-e2e/fixtures/expectations.json
```

From this directory, `mvn -q exec:java` is an alternative to packaging.

With the checked-in 38-row fixture, both transports should produce 76
observations. The driver exits non-zero for an assertion failure or degraded
gRPC transport, but it does not compare the observed total with 76. Verify the
printed total separately; a different total is an incomplete run even when the
process exits zero. The driver itself has no unit-test sources; Maven's test
phase is disabled because this artifact is a runnable E2E consumer.

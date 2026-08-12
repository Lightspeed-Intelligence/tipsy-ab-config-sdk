# Tipsy AB-config Java SDK

Java 21 client for the Tipsy configuration and A/B experimentation platform.
It maintains an in-process configuration cache, supports gRPC and HTTP, and
asks the server—not the SDK—to perform experiment bucketing. The client never
connects directly to platform storage.

## Modules

`sdk/java` is a Maven reactor:

| Module | Artifact | Purpose |
|---|---|---|
| `tipsy-abconfig-proto` | `io.github.lightspeed-intelligence:tipsy-abconfig-proto` | Protobuf messages and gRPC stubs generated from `api/proto` during the build. |
| `tipsy-auth` | `io.github.lightspeed-intelligence:tipsy-auth` | Standalone HS256 service-token signer. |
| `tipsy-abconfig` | `io.github.lightspeed-intelligence:tipsy-abconfig` | Cache, transports, experiment resolution and optional web helpers. |
| `example` | not published | Runnable JDK HTTP-server example. |

The main package is `io.github.lightspeedintelligence.abconfig`; web helpers
are under `.web`, and signing helpers are under
`io.github.lightspeedintelligence.auth`.

## Install

Releases are published to Maven Central. Select the latest stable
`java-sdk/vX.Y.Z` entry from
[GitHub Releases](https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases)
or [CHANGELOG.md](tipsy-abconfig/CHANGELOG.md), then pin that version:

```xml
<dependency>
  <groupId>io.github.lightspeed-intelligence</groupId>
  <artifactId>tipsy-abconfig</artifactId>
  <version>RELEASED_VERSION</version>
</dependency>
```

The main artifact already depends on `tipsy-auth` and
`tipsy-abconfig-proto`. Declare `tipsy-auth` separately only when using the
signer without the main SDK.

For local development:

```bash
cd sdk/java
mvn -q -DskipTests install
```

Publishing instructions are in [RELEASING.md](./RELEASING.md).

## Quick start

```java
import io.github.lightspeedintelligence.abconfig.AbtestContext;
import io.github.lightspeedintelligence.abconfig.Config;
import io.github.lightspeedintelligence.abconfig.TipsyAbConfigClient;
import java.util.Map;

try (TipsyAbConfigClient client = TipsyAbConfigClient.create(Config.builder()
        .namespaces("my-project")
        .configServiceAddr("grpcs://config.example.com:443")
        .abtestServiceAddr("grpcs://abtest.example.com:443")
        .token(System.getenv("TIPSY_TOKEN"))
        .defaultNamespace("my-project")
        .build())) {

    // Create once per logical request. Construction performs no RPC.
    AbtestContext ctx = client.newAbtestContext(
            "user-123", Map.of("country", "US"), "upstream-request-id");

    String value = client.getConfig(
            ctx, "my-project", "feature.enabled", "false");
    Map<String, String> all = client.getAllConfigs(ctx, "my-project");

    // Local full-release cache read; Optional.empty() means no value.
    String staticValue = client.getConfigStatic(
            "my-project", "feature.enabled").orElse("false");

    System.out.println(value + " / " + staticValue + " / " + all.size());
}
```

The runnable example is
[example/Main.java](example/src/main/java/io/github/lightspeedintelligence/abconfig/example/Main.java).

## Resolution API

| Method | Current behavior |
|---|---|
| `getConfigStatic(ns, key)` | Local full-release cache read; returns `Optional<String>` and performs no experiment RPC. |
| `getConfig(ctx, ns, key, default)` | Experiment or gray hit, then full release, then caller default. A compute failure falls back to full release. |
| `getConfigDefault(ctx, key, default)` | Uses the configured default namespace. |
| `getAllConfigs(ctx, ns)` | Resolves all keys against one cache snapshot and returns a new mutable map. Keys without values are omitted; an empty string is valid. |
| `getAllConfigsDefault(ctx)` | `getAllConfigs` using the default namespace. |
| typed getters | Boolean, long, double, string and JSON accessors over the same static/dynamic semantics. |
| `getExperimentResult(request)` | Exposes the raw proto response for custom parameters and group inspection. |

Create one `AbtestContext` for each logical request and reuse it. The first
dynamic lookup fetches and memoizes at most one experiment result per namespace.
`prefetchConfigVersionFlatKvForNamespace(ns)` starts that fetch early without
blocking and is idempotent.

An empty or `"0"` user id represents no real user and bypasses experiment
lookup. `emptyAbtestContext()` is the explicit form for non-user paths.

Namespace resolution is explicit namespace, then `Config.defaultNamespace`,
then `PROJECT_DEFAULT_NAMESPACE`. A missing default raises
`NamespaceRequiredException`; a namespace not included in `Config.namespaces`
raises `NamespaceNotSubscribedException`.

When the service explicitly reports `has_dynamic_resolution=false`, the SDK
skips an unnecessary experiment wait for the pure-full key. Missing fields
follow the safe dynamic path, so an older server stays functionally correct but
does not provide the optimization. Servers need `api/gen/go` v0.3.0 or newer
to emit this field.

`traceId` is an opaque correlation identifier. An empty value is replaced with
a UUID v4; pass an existing request/trace id when one is available.

The raw response's `config_flat_kv`, `groups[].params_versions`, and
`gray_hits[].key_versions` values are global `config_version` primary-key ids,
not the per-key `version_no`.

## Transports

`Config.transport` selects `Transport.GRPC` or `Transport.HTTP`. A null value
reads `TIPSY_SDK_TRANSPORT`, then defaults to gRPC.

- gRPC performs startup `PullAll`, maintains a streaming `Subscribe`, and keeps
  periodic PullAll as a safety net.
- HTTP posts protojson to `/api/v1/config/pull_all` and
  `/api/v1/abtest/experiment_result`. It does not establish Subscribe; update
  latency is bounded by `pullInterval`.

gRPC target forms:

| Target | Behavior |
|---|---|
| `host:port`, `grpc://host:port` | Plaintext h2c. |
| `grpcs://host:port` | TLS with certificate verification. |
| `dns:///service.namespace.svc.cluster.local:50051` | Native DNS resolver and automatic `round_robin`, intended for a Headless Service. |
| `passthrough:///`, `unix:`, `xds:///` | Native gRPC target passthrough. |
| `http://`, `https://` | Rejected in gRPC mode; select HTTP transport. |

`authority` and the development-only `insecure` query option are available on
`grpcs://`. Do not disable certificate verification in production.

## Configuration

| Setting | Default | Meaning |
|---|---:|---|
| `namespaces` | required | Namespaces held in the local cache. |
| `configServiceAddr` | required | gRPC target or HTTP base URL. |
| `abtestServiceAddr` | empty | Empty disables experiment RPCs and uses full-release values. |
| `token` / `tokenProvider` | required | Static or dynamic Bearer credentials; provider takes precedence. |
| `pullInterval` | 10s | Fallback polling interval and HTTP update interval. |
| `pullTimeout` / `pullRetries` | 5s / 3 | Pull deadline and startup retries. |
| `abtestTimeout` | 1500ms | Per-compute deadline. |
| `startupFailOpen` | false | Continue with an empty cache if startup PullAll fails. |
| `defaultNamespace` | env | Empty reads `PROJECT_DEFAULT_NAMESPACE`. |
| `transport` | env or gRPC | Empty reads `TIPSY_SDK_TRANSPORT`. |
| `maxRecvMessageSize` / `maxSendMessageSize` | 512 MB | gRPC message limits. |
| `channelConfigurator` | null | Customizes the gRPC channel builder. |
| `httpClient` | null | Injected caller-owned JDK `HttpClient`. |
| `onBackgroundError` | null | Callback for startup pull, periodic pull and Subscribe errors. |

Use `client.health()` for a current health snapshot and `client.metrics()` for
SDK counters. Keep service tokens outside code and documentation.

## Web integration

The public API favors explicit `AbtestContext` parameters. This remains correct
when one request fans out across virtual threads.

The optional `.web` package provides:

- `AbtestContextHolder`, a `ThreadLocal` convenience for a strict
  thread-per-request boundary. It does not propagate across executor fan-out.
- `HttpServerSupport`, helpers for JDK `com.sun.net.httpserver`, including trace
  extraction and a wrapper that creates and clears the request context.

Read a holder value once at the boundary and explicitly pass it to fan-out
work. The SDK does not provide Servlet filters, Spring auto-configuration, or a
gRPC server interceptor.

## Service-token signer

`tipsy-auth` issues HS256 JWTs compatible with the service verifier:

```java
import io.github.lightspeedintelligence.auth.IssueOptions;
import io.github.lightspeedintelligence.auth.JwtSigner;
import java.time.Duration;
import java.util.List;

JwtSigner signer = JwtSigner.create(System.getenv("TIPSY_SERVICE_SECRET"));
String token = signer.issue(IssueOptions.builder()
        .subject("my-service")
        .roles(List.of("business_sdk"))
        .namespaces(List.of("my-project"))
        .ttl(Duration.ofHours(2))
        .build());
```

The signer only creates tokens; it does not verify them. The deployment owner
defines the accepted secret, claims, namespace scope and lifetime.

## Intentional Java API mappings

- Java passes `AbtestContext` explicitly; Go can also carry it in
  `context.Context`, while Python can use a `ContextVar`.
- `getConfigStatic` uses `Optional<String>` so a missing value is distinct from
  a valid empty string.
- Java does not expose Go's low-level `waitForAbtest` entry point.
- `create(Config)` has no whole-startup context deadline; individual pulls use
  `pullTimeout` and `pullRetries`.
- Logging uses SLF4J, and channel customization uses
  `UnaryOperator<ManagedChannelBuilder<?>>`.

These are language mappings; gRPC/HTTP, Subscribe behavior, resolution
precedence and the pure-full fast path remain aligned. Token-provider timing is
an implementation difference: Java's `TokenSource` invokes the provider for
each HTTP request and gRPC RPC, matching Go, while the current Python client
only acquires its provider token during initialization and advises client
recreation before expiry; see the pending-verification note in the
[Python README](../python/README.md#configuration).

## Development

```bash
cd sdk/java
mvn -q -DskipTests package
mvn -q test
mvn -q -DskipTests install
```

Java protobuf sources are generated from `api/proto` during the Maven build and
are not checked in. The repository is licensed under [MIT](../../LICENSE).

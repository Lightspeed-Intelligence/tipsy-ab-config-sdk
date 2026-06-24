# Changelog

All notable changes to the Tipsy AB-config Java SDK main module
(`io.github.lightspeed-intelligence:tipsy-abconfig`) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - 2026-06-25

### Changed

- **BREAKING — `AbtestContext` construction no longer eager-prefetches.**
  `newAbtestContext(uid, attrs)` / `(…, traceId)` are now pure-create: they issue
  NO `GetExperimentResult` RPC at construction time. Previously construction
  eagerly pre-fetched the project default namespace's `config_version flat_kv`
  result in the background. **Latency-shape change**: the default namespace is no
  longer warmed at construction, so the FIRST `getConfig` for a namespace now
  pays the `GetExperimentResult` RPC latency inline (every namespace is fetched
  lazily and memoised on first use, at-most-once per ns per request).
- **BREAKING — renamed internal fetch** `getExperimentResultForNamespace` →
  `fetchConfigVersionFlatKvForNamespace` (package-private; the name now reflects
  the hardwired `config_version` + `flat_kv` shape and is no longer confusable
  with the public general-purpose `getExperimentResult`). The public
  `getExperimentResult(ExperimentResultRequest)` API is unchanged.

### Removed

- **BREAKING — removed both `newAbtestContextForNamespace(...)` overloads**
  (`newAbtestContextForNamespace(ns, uid, attrs)` and
  `newAbtestContextForNamespace(ns, uid, attrs, traceId)`). Their only purpose
  was choosing the construction-time eager-prefetch namespace, which no longer
  exists. No compatibility shim. Migrate to `newAbtestContext(uid, attrs[,
  traceId])` plus an explicit
  `abctx.prefetchConfigVersionFlatKvForNamespace(ns)` if warm-up is desired.

### Added

- `AbtestContext.prefetchConfigVersionFlatKvForNamespace(String ns)` — explicit,
  opt-in, non-blocking prefetch (warm-up) of a single namespace's
  `config_version flat_kv` result. Idempotent and at-most-once: a subsequent
  `getConfig` for the same ns reuses the warmed future. An empty / mock context
  or an unsubscribed ns short-circuits and issues no RPC. (Java has no network
  middleware; if warming at a self-built thread-per-request entry point, the
  caller should URL-whitelist-gate the call to avoid mass empty experiment RPCs.)

### Fixed

- Maven Central publishing actually uploads now. The `example` module's
  `central-publishing` `skipPublishing=true` was suppressing the whole aggregate
  bundle upload (the plugin uploads once from the last reactor module = example),
  so a release run went green but published nothing. The example is now excluded
  via the parent's `<excludeArtifacts>tipsy-abconfig-example</excludeArtifacts>`
  (matched by bare artifactId). See `sdk/java/RELEASING.md` Gotchas. No
  source/API change; `0.1.0` artifacts on Central are unaffected.

## [0.1.0] - 2026-06-22

First Java SDK release. Full parity with the Go / Python SDK capability
surface (see `sdk/java/README.md` for the intentional language-mapping
differences).

### Added

- `TipsyAbConfigClient` — the SDK handle. Factory `create(Config)` resolves
  the transport, validates parameters, dials the gRPC channels (or builds the
  HTTP transports), runs the startup `PullAll` sweep, and starts the background
  loops (Subscribe stream in gRPC mode + a periodic fallback `PullAll` loop).
  Implements `AutoCloseable` (try-with-resources / `close()`, idempotent).
- Config resolution API:
  - `getConfigStatic(ns, key) → Optional<String>` — pure full-release cache
    read (no namespace resolution; empty string is a valid value).
  - `getConfig(abctx, ns, key, default)` — dynamic resolution honouring abtest
    hits (whitelist > experiment > full release > default), with at-most-once
    `GetExperimentResult` RPC per namespace per request and silent single-ns
    degradation.
  - `getConfigDefault(abctx, key, default)` — namespace-optional form.
  - `getExperimentResult(ExperimentResultRequest)` — thin pass-through over
    `AbtestService.GetExperimentResult` returning the raw proto response.
- `AbtestContext` per-request user context with `userId()` / `userInfo()` /
  `traceId()` accessors and per-namespace memoisation; factories
  `newAbtestContext(uid, attrs)` / `(…, traceId)` /
  `newAbtestContextForNamespace(…)` / `emptyAbtestContext()` /
  `mockAbtestContext(…)`. `UserInfo` value type with a public `UserInfo.of(uid,
  attrs)` factory.
- `Config` (+ `Config.Builder`) with the full Go knob set: namespaces, config
  / abtest service addresses, pull interval / timeout / retries, abtest
  timeout, startup-fail-open, static `token` / dynamic `tokenProvider`,
  512&nbsp;MB max recv / send message sizes, `onBackgroundError` callback,
  default namespace, `transport`, `channelConfigurator`
  (`UnaryOperator<ManagedChannelBuilder<?>>` dial-options seam), injected
  `httpClient`.
- gRPC and HTTP transports. gRPC: `PullAll` / `Subscribe` (server stream) /
  `GetExperimentResult`, 30s/5s keepalive, per-RPC Bearer JWT credentials,
  512&nbsp;MB inbound (channel) + outbound (per-stub) limits. HTTP: protojson
  over POST (`/api/v1/config/pull_all`, `/api/v1/abtest/experiment_result`),
  polling only (no Subscribe).
- Address scheme parsing (方案 Y): bare `host:port` / `grpc://` → h2c;
  `grpcs://host:port[?authority=&insecure=]` → TLS; `dns:///…` → automatic
  client-side `round_robin`; `passthrough:///` / `unix:` / `xds:///` native
  pass-through; `http(s)://` rejected in gRPC mode.
- Observability: `Health` snapshot + `Metrics` counters; `startupFailOpen`;
  `onBackgroundError` callback with phases `startup_pull` / `periodic_pull` /
  `subscribe`.
- Exception hierarchy: `AbtestContextMissingException`,
  `StartupPullFailedException`, `SdkClosedException`,
  `NamespaceRequiredException`, `NamespaceNotSubscribedException`,
  `ConfigValidationException`, `TransportException`, `TipsyConfigException`.
  Enums: `ExperimentType`, `ResultDisplayType`, `Transport`.
- `Transport` selection: `Config.transport` > `TIPSY_SDK_TRANSPORT` env > gRPC.
- Framework-agnostic web integration in the optional `io.github.lightspeedintelligence.abconfig.web`
  subpackage (pure JDK, zero extra dependencies):
  - `AbtestContextHolder` — `ThreadLocal` holder (`set` / `get` / `clear` /
    `runWith`) with an explicit fan-out warning (not propagated across
    virtual-thread / executor fan-out).
  - `HttpServerSupport` — thin helpers for the JDK built-in
    `com.sun.net.httpserver.HttpServer`: `extractTraceId(HttpExchange)`
    (X-Trace-Id → X-Request-Id → fresh UUID), an `AbtestUserProvider`
    functional interface, and a `wrap(client, provider, next)` context-binding
    handler adapter. No servlet / Spring / gRPC-server dependency.

### Notes

- Does NOT report exposures and does NOT bucket on the client (the server
  owns hashing / bucketing; the SDK only reads results).
- Intentional differences from the Go SDK are documented in
  `sdk/java/README.md` (Optional-ised `getConfigStatic`, explicit
  `AbtestContext` passing instead of `context.Context` carry, no servlet
  filter / Spring auto-config).

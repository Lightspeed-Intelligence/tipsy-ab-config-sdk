# Changelog

All notable changes to the Tipsy AB-config Java SDK main module
(`io.github.lightspeed-intelligence:tipsy-abconfig`) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.9.0] - 2026-07-23

### Changed

- **env is now judged server-side; the SDK no longer sends an env request
  field.** env matching moved to the abConfig server, which reads its own
  `TIPSY_ENV` process environment variable and compares it against each
  experiment's env set. Removed `Config.env` (the builder `env(String)` method
  and the `env()` accessor); the four request construction points
  (`getExperimentResult`, the `config_version` flat_kv fetch behind
  `getConfig`, `pullOnce`, `subscribeOnce`) no longer set the request's `env`
  field.

### Compatibility

- The `env` field remains in the generated request stubs and is left unset by
  this SDK — retained for wire compatibility with the previously released 0.8.0
  and now deprecated on the request path. Newer servers ignore any
  request-supplied env and use `TIPSY_ENV` instead, so both old (0.8.0, still
  sending env) and new SDK builds interoperate with the new server. The proto
  is unchanged (no regen of the request field).

## [0.8.0] - 2026-07-22

### Added

- `Config.env` (builder `env(String)`, default `""`) — a single-value
  environment identifier stamped onto **every** outbound request:
  `getExperimentResult`, the `config_version` flat_kv fetch behind
  `getConfig`, the background `PullAll`, and the `Subscribe` stream. It tells
  the server which environment this process runs in so experiment env
  filtering can apply (an experiment with a non-empty env set is only entered
  when this env is a member; `env=""` enters only experiments that do not
  restrict their env). No environment-variable fallback — the value is used
  verbatim. The build-time protobuf plugin regenerates the request stubs from
  `api/proto` automatically. Mirrors the Go and Python SDKs.

### Compatibility

- 100% backwards compatible. `env` defaults to `""`; the HTTP transport's
  `JsonFormat` omits an empty scalar, so an unset `env` is byte-for-byte
  wire-compatible with older servers. A configured env sent to an older server
  that predates the field is safely ignored.

## [0.7.0] - 2026-07-16

### Changed

- Ignore Subscribe `Heartbeat` events via a `getPayloadCase()` switch
  (forward-compat no-op): the `SNAPSHOT` branch is unchanged, `HEARTBEAT` is a
  liveness no-op, and any other/unset branch is silently skipped. Behaviour is
  equivalent to the previous `hasSnapshot()` guard.
- Reset the Subscribe reconnect backoff to its initial value after a healthy
  (alive for >= 60s) connection drop, instead of always escalating
  exponentially. Short-lived connections still back off exponentially (capped at
  30s). Mirrors the Go SDK.

## [0.6.0] - 2026-07-03

### Added

- Debug-level per-call timing log for `getExperimentResult`. Both call
  sites (the public `getExperimentResult` and the `AbtestContext` lazy
  per-namespace fetch) emit one Debug record per RPC with `ns`, `trace_id`
  and float-millisecond `duration_ms` (the throwable is attached on
  failure). Fields are embedded directly in the slf4j message so they are
  greppable without a structured layout. Debug level only; the existing
  fallback warning is unchanged. Mirrors the Go SDK v0.9.0 change.

## [0.5.0] - 2026-06-30

### Added

- Typed config accessors, mirroring the Go SDK's v0.7.0 surface. Static
  (cache-only) and dynamic (abtest-resolved) variants for each scalar type
  plus JSON:
  - Static: `getConfigStaticBool/Long/Double/String(ns, key, def)` and
    `getConfigStaticJson(ns, key, Class<T>|Type, def)`.
  - Dynamic: `getConfigBool/Long/Double/String(abctx, ns, key, def)` and
    `getConfigJson(abctx, ns, key, Class<T>|Type, def)`.
  - **Bool is lenient and never throws**: the trimmed value equal
    (case-insensitively) to `"true"` or equal to `"1"` ⇒ `true`, everything
    else ⇒ `false`. The console writes canonical `true`/`false`, so the
    write-strict / read-lenient asymmetry is deliberate.
  - **Long is parsed with `Long.parseLong`** (no double round-trip), so values
    beyond 2^53 are lossless.
  - Miss and value-parse failure both fall back to the supplied default; the
    underlying `getConfig` exceptions (client-closed / namespace /
    abtest-context) still propagate — only value-parse failures are swallowed.
    JSON uses the Gson already on the classpath via `protobuf-java-util`; no
    new dependency.
  - The config value stays a canonical string end-to-end; the declared
    `value_type` is a console-side write contract and is not carried to the SDK.

## [0.4.0] - 2026-07-01

### Changed (BREAKING)

- **`GetExperimentResultResponse.gray_hits` is now grouped per gray release.**
  `GrayReleaseHit` changed from the flat `{release_id, key, version_id}` (one
  entry per `(release, key)`) to `{release_id, map<string,int64> key_versions}`
  (one entry per hit `release_id`; `key_versions` maps each `config_key.key`
  name → versionId). This aligns `gray_hits` with
  `ExperimentGroupResult.params_versions`. Read a single key's target via
  `grayHits.get(i).getKeyVersionsMap().get(keyName)` instead of the removed
  `getVersionId()`. No backward compatibility — the old flat fields are gone.
  The int64 values remain versionId (config_version PRIMARY KEY id, globally
  unique), never the semantic version_no.

## [0.3.0] - 2026-06-27

### Added

- **`has_dynamic_resolution` field on `KeyState`.** The full-config snapshot
  (both the `Subscribe` push and the `PullAll` pull paths) now carries a
  presence-aware `optional bool has_dynamic_resolution` per key: whether the key
  has any gray-release / experiment attached (i.e. it needs abtest resolution).
  Deserialised into the local cache with its proto `optional` tri-state preserved
  (`null` = field absent / old server, `TRUE`/`FALSE` = explicitly set).

### Changed

- **`getConfig` fast-path.** When the server explicitly reports a key as pure
  full-rollout (`has_dynamic_resolution == false`), `getConfig` now SKIPS the
  abtest wait (`resultFor`, and its potential `GetExperimentResult` RPC) and
  resolves straight from the full-release value. The fallback / default semantics
  are unchanged — only the wasted RPC is removed. Gated on an EXPLICIT `false`:
  an absent field (old server) or `true` keeps the existing always-wait path, so
  a new SDK pointed at an old server never mis-skips and silently breaks
  gray-release / experiments.

### Compatibility

- **Server-first upgrade ordering (required).** This version expects the server
  to already emit `has_dynamic_resolution` (`api/gen/go` v0.3.0+). Upgrade the
  server FIRST, then the business-side SDK. If the field is absent (old server),
  the SDK safely falls back to always waiting on abtest — functionally correct,
  just without the fast-path benefit. No version-negotiation logic; the absent
  field is the only compatibility signal.

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

# Changelog

All notable changes to the Tipsy AB-config Java SDK main module
(`io.github.lightspeed-intelligence:tipsy-abconfig`) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed (BREAKING)
- 动态配置 / 实验入参 `userId` 统一改名为 `experimentHashId`，让语义更直观：它是实验平台
  用来哈希分桶的稳定标识（可以是 uid，也可以是设备 id 等任意稳定主体键），线上协议字段
  仍为 `user_id`，运行时行为不变。涉及公开 API：
  - `newAbtestContext` / `mockAbtestContext` / `UserInfo.of` /
    `ExperimentResultRequest.Builder.userInfo(String, Map)` 的参数名（Java 按位置传参，
    调用方无需改动）；
  - `AbtestContext.userId()` → `AbtestContext.experimentHashId()`；
  - `UserInfo.uid()` → `UserInfo.experimentHashId()`。
  日志字段名 `uid` 保持不变，避免影响既有日志查询。

## [0.11.0] - 2026-08-27

### Fixed — ctx-cancel 误报（issue #15）

- **调用方上下文取消（gRPC `CANCELLED`）不再按故障处理**。此前上游客户端断连、
  deadline 传播、`close()` 的 executor 中断等产生的
  `StatusRuntimeException(CANCELLED)` 会打 ERROR/WARN、计失败指标并发
  `BackgroundErrorEvent`。现在：
  - Subscribe 流与周期 PullAll 循环命中 ctx-cancel 时**静默退出**——不 inc
    `subscribeDisc`/`pullFailure`、不打 ERROR、不发 `subscribe`/`periodic_pull`
    事件（`SubscribeConnected` 不再被误翻 false）。`close()` 经
    `channel.shutdownNow()` 产生的 UNAVAILABLE 仍由原有 `closed` 兜底覆盖，行为不变。
  - abtest 拉取（`GetExperimentResult`）命中 ctx-cancel 时降为 **INFO** 日志，
    计入**新增指标 `sdk_abtest_canceled_total{namespace}`**
    （`Metrics.abtestCanceledTotal(ns)`），不再计 `abtestFallback`。取值语义不变
    （仍回落 full release / default）。
  - 判定刻意收窄为 `StatusRuntimeException` + `Status.Code.CANCELLED`：
    `DEADLINE_EXCEEDED` 等真错误路径行为不变（回归用例钉住）。
  - 说明：Go 端同批修复（`isContextCanceled`）；**Python 无需改动**——实测
    grpcio 的取消抛 `asyncio.CancelledError`（`BaseException`），现有代码已正确
    处理，属三语言对齐规则的合理例外（论证见 issue #15）。

### Added — secretKey 鉴权（issue #16）

- **`Config.Builder.secretKey(String)`：仅配置 secretKey（即 `TIPSY_SERVICE_SECRET` 本身）
  即可完成鉴权**，无需可信 issuer 签发 JWT。凭据优先级 **SecretKey > TokenProvider > Token**
  （每请求在 `TokenSource.authHeaderValue()` 单取值点判定，gRPC metadata 与 HTTP header
  两路径同源），发送形态为精确字面量 `Authorization: SecretKey <secret>`（服务端按 scheme
  前缀分派，需平台侧 `feat/secretkey-auth` 配套）。Init 校验放宽为 secretKey / token /
  tokenProvider 三者至少其一，缺失时错误文案三端统一：
  `tipsyabconfig: SecretKey, Token or TokenProvider must be set`。
  SDK 不读任何环境变量，secretKey 由业务方显式传入；secret 值不进入任何日志。
  持有 secretKey 即等效全量访问（`internal_service` + `"*"`），信任边界见
  `docs/usage-and-integration.md` §3。

### Added

- **`getConfig` / `getConfigDefault` 命中日志新增结构化归因字段 `reason`**（4 值完备：
  `full_release` / `experiment` / `gray_whitelist` / `abtest_unattributed`）。
  `reason=experiment` 时同时输出 `experiment_id`、`group_id`；`reason=gray_whitelist`
  时同时输出 `release_id`（整数）。条件字段缺席时整键省略（omit），不用空串占位。
  Java 端为保证 SLS 机器可解析，采用**每-reason 固定 kv 文本模板**（字段顺序固定）；
  msg 前缀 `tipsyabconfig: get_config hit (abtest)` / `(full)` 不变，既有检索/告警
  不受影响。`getConfigStatic` 的日志行保持现状（`source=full_static`，无 reason）。

### Changed

- **内部 per-ns abtest fetch 的 wire 形态由 `CONFIG_VERSION + FLAT_KV` 切换为
  `CONFIG_VERSION + EACH_EXPERIMENT_GROUP`**，SDK 本地把 `groups[]` + `gray_hits[]`
  合并为原有的扁平 key→versionId map（逐行复刻平台 flat 合并：灰度无条件优先于实验；
  同类型 key 冲突属异常情况，平台 + SDK 只保障至少返回可选值中的一个），归因信息
  （experimentId / groupId / releaseId）由此保留并进入命中日志。值解析结果与原
  FLAT_KV 等价。公共 API 零改动：`getExperimentResult`、
  `prefetchConfigVersionFlatKvForNamespace` 等签名与语义不变（后者名称中的
  "FlatKv" 指其输出形态，予以保留）。

### Test infrastructure

- 新增 test-scope 依赖 `ch.qos.logback:logback-classic`（parent 统一 pin 1.5.x，
  与 slf4j-api 2.0.x 配套），供测试用 `ListAppender` 捕获并断言命中日志整行形状。
  仅测试期生效，不进入发布 jar 的依赖图。

## [0.10.1] - 2026-08-24

### Changed
- `getConfig` / `getConfigDefault` / `getConfigStatic` 命中日志从 `DEBUG` 提升到 `INFO`
  （字段 `ns`/`key`/`version`，动态路径另带 `uid`/`trace_id`）。其中 `getConfigStatic`
  此前命中时不打日志，本次补齐与另两端一致。目的：在默认 `INFO` 级别下即可从调用方日志
  观测业务实际请求的 config key 与命中版本，无需临时开 DEBUG。
  出于泄漏风险，命中日志**不记录 config value**。

## [0.10.0] - 2026-07-28

### Added

- **`getAllConfigs(abctx, ns)` and `getAllConfigsDefault(abctx)`.** Resolve
  EVERY dynamic config in a namespace for a user in one call, returning a fresh
  mutable `Map<String, String>` of `key → value`. Per-key resolution is
  identical to `getConfig` (abtest whitelist / experiment hit > full release),
  assembled client-side from the local cache snapshot plus the same
  at-most-once memoised `GetExperimentResult` result — no new RPC beyond the one
  `getConfig` would already issue for the namespace (and zero when every key is
  pure full-rollout). A key with neither an abtest hit nor a full-release value
  is OMITTED from the map (there is no per-key default in the get-all form); the
  empty string is a valid value and is preserved. An absent snapshot
  (subscribed but not yet pulled) returns an empty map with zero RPC.
  `getAllConfigsDefault` is `getAllConfigs` with the project default namespace.
  Mirrors the Go and Python SDKs.

### Changed

- **A no-user uid (`""` or `"0"`) now resolves statically — no
  `GetExperimentResult` RPC.** When an `AbtestContext` carries an empty uid
  (`null` normalises to `""`) or the string zero `"0"`, both `getConfig` and
  `getAllConfigs` skip the abtest bucketing / whitelist logic entirely and
  resolve straight from the full-release value (single-key `getConfig` returns
  the supplied default when there is no full release; `getAllConfigs` omits such
  a key). Previously an empty uid still issued a `GetExperimentResult` RPC with
  `user_id=""` — that had no dependable experiment semantics (the server cannot
  meaningfully bucket a no-user request), so this is a behaviour change but not
  a breaking one. The short-circuit lives in the shared lazy-fetch primitive, so
  `prefetchConfigVersionFlatKvForNamespace` also issues no RPC for a no-user
  uid. `emptyAbtestContext()` / `mockAbtestContext(...)` semantics are unchanged
  (a pre-seeded mock result still wins). No fallback metric is bumped (this is a
  deliberate skip, not a degradation).

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

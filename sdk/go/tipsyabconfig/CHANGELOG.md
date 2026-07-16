# Changelog

All notable changes to the Tipsy AB-config Go SDK (`sdk/go/tipsyabconfig`)
are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Tag scheme: `sdk/go/tipsyabconfig/vX.Y.Z` (Go monorepo sub-module rule —
the tag MUST be prefixed with the relative module path). Consumers
install via:

```bash
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig@vX.Y.Z
```

The SDK transitively pulls `api/gen/go` at the version pinned in
`go.mod`. Major proto changes therefore land via an `api/gen/go` tag
bump first, then an SDK tag bump.

## [Unreleased]

## [0.10.0] - 2026-07-16

### Added

- Ignore Subscribe `Heartbeat` events (forward-compat). The server may emit
  liveness-only `Heartbeat` frames on an otherwise-idle Subscribe stream so
  intermediary proxies (e.g. Cloudflare's ~100s edge timeout, which otherwise
  tears the idle stream down with HTTP 524) keep it alive. `handleEvent` now
  switches on the `ConfigUpdateEvent` payload oneof and treats `Heartbeat` as an
  explicit no-op — no cache mutation, no sequence advance, not counted as a
  subscribe event. Requires `api/gen/go` v0.6.0 (adds the `Heartbeat` member to
  the `ConfigUpdateEvent` payload oneof).

### Changed

- Reset the Subscribe reconnect backoff after a healthy connection drop. A
  stream that stayed up at least 60s before dropping now reconnects at the
  initial 1s delay instead of inheriting the escalated exponential backoff; a
  short-lived connection (genuinely unreachable server) still backs off
  exponentially, capped at 30s. Pure helper `resetBackoffIfStable`
  (threshold ≤ 0 never counts as stable). The reset happens before the log/sleep
  so the logged backoff matches the actual wait. Complements the server-side
  heartbeat above.

## [0.9.0] - 2026-07-03

### Added

- Debug-level per-call timing log for `GetExperimentResult`. Both call sites
  (the public `GetExperimentResult` and the `AbtestContext` lazy per-namespace
  fetch) emit one Debug record per RPC with `ns`, `trace_id` and
  float-millisecond `duration_ms` (the error is attached on failure). Info level
  stays silent; the existing fallback warnings are unchanged.

## [0.8.0] - 2026-07-01

### Changed (BREAKING)

- **`GetExperimentResultResponse.gray_hits` is now grouped per gray release.**
  `GrayReleaseHit` changed from the flat `{release_id, key, version_id}` (one
  entry per `(release, key)`) to `{release_id, map<string,int64> key_versions}`
  (one entry per hit `release_id`; `key_versions` maps each `config_key.key`
  name → versionId). This aligns `gray_hits` with
  `ExperimentGroupResult.params_versions`. Read a single key's target via
  `gray_hits[i].GetKeyVersions()[keyName]` instead of the removed
  `gray_hits[i].GetVersionId()`. No backward compatibility — the old flat
  fields are gone. The int64 values remain versionId (config_version PRIMARY
  KEY id, globally unique), never the semantic version_no.
- Requires `api/gen/go` v0.5.0 (grouped `GrayReleaseHit` +
  `ConfigService.GetConfigVersionNos`).

## [0.7.0] - 2026-06-30

### Added

- Typed config accessors. Five per-type getter families resolve the
  canonical string value and parse it at the edge, for both the static
  (`GetConfigStatic*`) and dynamic (`GetConfig*`) paths:
  `GetConfig{Bool,Int64,Float64,String,JSON}` and their `Static` variants.
  - **Bool is lenient and never errors**: `TrimSpace` then
    `EqualFold "true" || == "1"` ⇒ `true`, everything else ⇒ `false`.
  - **Int64 via `strconv.ParseInt`** with no float round-trip, so values
    `> 2^53` are returned losslessly.
  - **JSON** unmarshals into the caller's `out` pointer.
  - Static variants return `(T, ok)`; dynamic variants return `(T, error)`
    and treat a resolved-but-empty value as a miss (returning the default).
  - The value stays a canonical string end-to-end; the config's declared
    type (`value_type`) is a console-side write contract and is **not**
    carried on the wire, in the snapshot, or in the SDK cache. The legacy
    string `GetConfigStatic` / `GetConfig` / `GetConfigDefault` are retained.

### Changed

- Pins `api/gen/go` at `v0.4.0` (drops `ConfigVersionInfo.change_note`).

### Removed

- `ConfigVersionInfo.change_note` is gone from the config proto
  (`reserved 3` / `"change_note"`). Per-version change notes are no longer
  carried; consoles display `versionNo-value` instead.

## [0.6.0] - 2026-06-27

### Added

- The SDK now deserializes the new `optional bool has_dynamic_resolution`
  field on each full-config `KeyState` (from both the `PullAll` and
  `Subscribe` snapshot paths). The value is presence-aware and preserves the
  proto `optional` tri-state: explicitly `true`/`false` when the server set
  it, absent when the field is missing (an older server that predates it).

### Changed

- `GetConfig` fast-path. When the server explicitly reports a key as pure
  full-rollout (`has_dynamic_resolution` is present **and** `false`),
  `GetConfig` now **skips** the abtest wait (`resultFor`, and its potential
  `GetExperimentResult` RPC) and returns the full-release value directly.
  This removes a guaranteed-wasted RPC for pure-full-release keys; the
  fallback / default semantics are unchanged. Gated on an EXPLICIT `false`:
  a key whose field is `true` or absent (old server) keeps the existing
  always-wait abtest path, so a new SDK pointed at an old server never
  mis-skips and silently breaks gray release / experiments.

### Compatibility

- **Server-first upgrade ordering (REQUIRED).** This version expects the
  server to already emit `has_dynamic_resolution`, i.e. a server built
  against **`api/gen/go` v0.3.0 or newer**. **Upgrade the server FIRST, then
  this SDK.** If the field is absent (old server), the SDK safely falls back
  to always waiting on the abtest result — functionally correct (gray release
  / experiments keep working), just without the fast-path benefit. There is
  no version-negotiation logic; the absent field is the only compatibility
  signal, and the fast path is taken only on an explicit `false`, never on an
  absent field.

## [0.5.0] - 2026-06-25

### Removed (BREAKING)

- `Client.NewAbtestContextForNamespace` and
  `Client.NewAbtestContextForNamespaceWithTraceID`. These existed only to pick
  the construction-time eager-prefetch namespace, which no longer happens (see
  Changed). Use the retained `NewAbtestContext` /
  `NewAbtestContextWithTraceID` to construct, then opt into warming a specific
  namespace via `AbtestContext.PrefetchConfigVersionFlatKvForNamespace`. No
  compatibility shim — this is a deliberate breaking change.

### Changed (BREAKING)

- `AbtestContext` construction is now pure-create and issues NO
  `GetExperimentResult` RPC. Previously `NewAbtestContext*` eagerly pre-fetched
  the project default namespace in the background. **Latency-shape change**:
  the default namespace is no longer warmed at construction, so the first
  `GetConfig` for it now pays the `GetExperimentResult` RPC latency inline
  (previously that cost was often hidden by the background prefetch). All
  namespaces are now fetched lazily on first dynamic `GetConfig` and memoised
  (still at most one RPC per namespace per request link).
- `Client.Middleware` and `Client.GinMiddleware` no longer prefetch on every
  request. They gained a variadic `...MiddlewareOption`; pass
  `PrefetchPaths(paths...)` to opt specific **exact** request paths into
  default-namespace prefetch. Default (no option) = no prefetch on any path,
  avoiding a flood of useless empty experiment RPCs for handlers that never
  call `GetConfig`. Existing callers that pass no options still compile and now
  simply attach the context without prefetching.

### Added

- `AbtestContext.PrefetchConfigVersionFlatKvForNamespace(ns)` — explicit,
  opt-in, non-blocking, idempotent (at-most-once) warm of the config_version
  flat_kv experiment result for `ns` into the context. A subsequent
  `GetConfig` / `WaitForAbtest` for the same `ns` reuses the in-flight or
  completed result (no second RPC). Empty / mock contexts and unsubscribed
  namespaces short-circuit without an RPC.

### Internal

- Renamed the misleading internal per-ns fetch helper
  `getExperimentResultForNamespace` →
  `fetchConfigVersionFlatKvForNamespace` (it is hardwired to
  `CONFIG_VERSION` + `RESULT_DISPLAY_TYPE_FLAT_KV`, distinct from the public
  custom_params `GetExperimentResult`, which is unchanged). Extracted the
  per-ns at-most-once ensure-fetch critical section into a shared internal
  `ensureFetch` primitive used by both `resultFor` and the new prefetch API.

## [0.4.0] - 2026-06-19

### Added

- Auto-enable client-side `round_robin` load balancing when the dial
  target uses the `dns:///` gRPC name resolver scheme (typically
  `dns:///<service>.<ns>.svc.cluster.local:<port>` for K8S Headless
  Service deployment). All other address forms (bare `host:port`,
  `grpc://`, `grpcs://`, `passthrough:///`, `unix:`) keep grpc-go's
  default `pick_first` behavior. No SDK Config field changes; opt-in
  via address string only. See `docs/usage-and-integration.md` §4.1.

## [0.3.0] - 2026-06-18

### Removed (BREAKING)

- `Config.ExposureSink`, `Config.ExposureDedupTTL`, type `ExposureSink`,
  type `ExposureSinkFunc`, type `ExposureEvent`, internal `exposureEmitter`
  and `logSink`. The SDK no longer emits exposure events on `GetConfig`.
  Use the upstream experiment-result data report channel instead.
- `GetExperimentResultResponse.exposures` is retained on the proto wire
  for backward compatibility but is never populated by the server.

### Added

- `GetExperimentResultResponse.gray_hits` (`repeated GrayReleaseHit`) —
  populated when `display_type==EACH_EXPERIMENT_GROUP` and
  `experiment_type ∈ {CONFIG_VERSION, ALL}`; otherwise an empty slice.

## [0.2.0] - 2025-11-21

### Added

- Optional `TraceID` field on `ExperimentResultRequest`. When omitted or
  empty the SDK generates a fresh UUID v4 (`uuid.New().String()`, 36-char
  with dashes) before writing the proto, so every request is
  trace-identifiable end-to-end without caller changes.
- `AbtestContext.traceID` field + public `TraceID()` accessor. Set at
  construction and reused by every per-namespace `GetExperimentResult`
  RPC the context issues (both eager prefetch and lazy `WaitForAbtest`).
- New constructors `Client.NewAbtestContextWithTraceID` /
  `Client.NewAbtestContextForNamespaceWithTraceID`. The legacy
  `NewAbtestContext` / `NewAbtestContextForNamespace` keep working — they
  delegate with `""` and auto-generate. `EmptyAbtestContext` and
  `MockAbtestContext` also carry an auto-generated trace_id so every
  context is uniformly attributable.
  (Historical: the `*ForNamespace*` constructors and the eager prefetch they
  fed were REMOVED in the Unreleased section above.)
- `Middleware` and `GinMiddleware` now resolve the per-request trace_id
  from inbound headers: `X-Trace-Id` first, then `X-Request-Id`, with a
  fresh UUID as the final fallback. Whitespace-only header values are
  treated as missing. The chosen id is attached to the AbtestContext so
  all `GetConfig` / `GetExperimentResult` calls inside the request share
  the same trace.
- Background `PullAll` and `Subscribe` RPCs (the 10-second safety-net
  pull and the server-streaming subscribe loop) now generate a fresh
  trace_id per call and emit a `Debug` log line with the id before
  issuing the RPC. This makes "why did this RPC fire?" debuggable from
  both SDK and server logs.
- `ExposureEvent.TraceID` field. The default `logSink` includes the
  field in its JSON line, and consumer-supplied `ExposureSink`
  implementations now receive it on every event — this is the wire-up
  point for upcoming server-side experiment-result reporting.

### Changed

- `Client.go.mod` now requires `api/gen/go v0.2.0` for the proto
  `trace_id` field. External consumers re-running `go mod tidy` will
  see this transitively.
- `go.mod` promotes `github.com/google/uuid v1.6.0` to a direct require
  (it was already indirect via the workspace).

### Compatibility

- 100% backwards compatible. All existing constructor signatures, public
  methods, struct field names, and middleware behaviour are preserved.
  Callers ignoring `trace_id` get a SDK-generated UUID transparently.

## [0.1.0] - 2026-06-16

Initial public release of the Tipsy AB-config Go SDK.

- `Client` / `Init` / `Config` — process-local config cache populated by
  a startup `PullAll`, a long-lived server-streaming `Subscribe`, and a
  10-second fallback `PullAll` safety net.
- `Client.GetConfigStatic` — pure cache read, no abtest, no exposure.
- `Client.GetConfig` / `Client.GetConfigDefault` — abtest-aware lookup
  with the per-request `AbtestContext` (abtest hit > full release
  fallback); emits exposure events asynchronously with a 5-minute
  per-process dedup window.
- `Client.NewAbtestContext` / `NewAbtestContextForNamespace` — eagerly
  pre-fetches the prefetch namespace; lazy per-ns fetch + dedup on first
  access.
  (Historical: eager prefetch and the `NewAbtestContextForNamespace`
  constructor were REMOVED in the Unreleased section above; construction is
  now pure-create.)
- `Client.GetExperimentResult` — low-level proxy for
  `AbtestService.GetExperimentResult`.
- `Middleware` (net/http) + `GinMiddleware` adapter.
- `ExposureEvent`, `ExposureSink`, `ExposureSinkFunc`, default `logSink`
  + async `exposureEmitter` with per-process 5-min dedup.

[Unreleased]: https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/compare/sdk/go/tipsyabconfig/v0.6.0...HEAD
[0.6.0]: https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases/tag/sdk%2Fgo%2Ftipsyabconfig%2Fv0.6.0
[0.5.0]: https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases/tag/sdk%2Fgo%2Ftipsyabconfig%2Fv0.5.0
[0.3.0]: https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases/tag/sdk%2Fgo%2Ftipsyabconfig%2Fv0.3.0
[0.2.0]: https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases/tag/sdk%2Fgo%2Ftipsyabconfig%2Fv0.2.0
[0.1.0]: https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases/tag/sdk%2Fgo%2Ftipsyabconfig%2Fv0.1.0

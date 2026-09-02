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

### Changed (BREAKING)
- 动态配置 / 实验入参 `userID` 统一改名为 `experimentHashID`，让语义更直观：它是实验平台
  用来哈希分桶的稳定标识（可以是 uid，也可以是设备 id 等任意稳定主体键），线上协议字段
  仍为 `user_id`，运行时行为不变。涉及公开 API：
  - `NewAbtestContext` / `NewAbtestContextWithTraceID` / `MockAbtestContext` 的参数名
    （Go 按位置传参，调用方无需改动）；
  - `AbtestContext.UserID()` → `AbtestContext.ExperimentHashID()`；
  - `UserInfo.UID` 字段 → `UserInfo.ExperimentHashID`；
  - `UserProvider` 返回值命名 `uid` → `experimentHashID`（仅命名，签名类型不变）。
  日志字段名 `uid` 保持不变，避免影响既有日志查询。

## [0.14.0] - 2026-08-27

### Added
- 新增 per-ns 计数器 `sdk_abtest_canceled_total`（`Metrics.AbtestCanceledTotal(ns)`）：
  记录因 ctx 取消而降级到全量发布的 GetExperimentResult 次数（与
  `abtest_fallback_total` 互斥，后者保持纯故障信号）。（#15）
- `Config.SecretKey`（issue #16）：仅配置 secretKey（即平台 `TIPSY_SERVICE_SECRET`
  本身）即可完成鉴权，免走可信 issuer 签发 JWT。凭据优先级
  `SecretKey > TokenProvider > Token`（逐请求求值）；传输形态为
  `Authorization: SecretKey <secret>`，gRPC metadata 与 HTTP header 一致。
  Init 校验放宽为三者至少其一（错误文案改为
  `SecretKey, Token or TokenProvider must be set`，三语言对齐）。SDK 不从
  环境变量读取 secretKey。注意：secretKey 校验通过＝平台侧全量访问（任意
  namespace），且需平台先升级支持 `SecretKey` scheme（部署顺序详见
  docs/usage-and-integration.md §3.1）。
- `GetConfig` / `GetConfigDefault` 命中日志（Info）新增结构化字段 `reason`
  （4 值枚举：`full_release` / `experiment` / `gray_whitelist` / `abtest_unattributed`），
  并按 reason 条件输出归因字段：`reason=experiment` 时带 `experiment_id` + `group_id`，
  `reason=gray_whitelist` 时带 `release_id`；缺席字段整键省略（omit 语义，非空串占位）。
  msg 文本（`get_config hit (full)` / `get_config hit (abtest)`）与既有字段
  （`ns`/`key`/`version`/`uid`/`trace_id`）不变。`GetConfigStatic` 日志行不变
  （无 reason，不属入组事件契约）。

### Fixed
- ctx 取消不再被当作故障处理（#15）：grpc-go 把 `context.Canceled` 转成
  **不 wrap 哨兵值**的 status error（`codes.Canceled`），SDK 原有的
  `errors.Is(err, context.Canceled)` 判定对真实 gRPC 取消错误全部失效，导致
  每次正常关停 / 上游断连都产生假 ERROR 日志 + 失败指标 + BackgroundErrorEvent。
  现新增 `isContextCanceled`（哨兵 || `status.Code==codes.Canceled`）统一判定：
  - Subscribe 流 / 周期 PullAll 命中 ⇒ 静默退出：不打 ERROR、不计
    `subscribe_disconnect_total` / `pull_failure_total`、**不发** BackgroundErrorEvent
    （避免把 `Health.SubscribeConnected` 误翻 false）；保留 rootCtx 兜底。
  - abtest per-ns 拉取命中 ⇒ 降为 Info 日志（含 ns/trace_id）+ 计
    `abtest_canceled_total`，不计 `abtest_fallback_total`、不打 WARN；
    返回语义不变（getConfig 仍降级 full/default）。
  - `DeadlineExceeded`（真超时）不在范围内，继续按错误处理。
  - **Python SDK 不改**（issue #15 已实测论证）：grpcio 取消场景抛
    `asyncio.CancelledError`（`BaseException`，不被 `except Exception` 捕获），
    且现有代码已显式拦截——机制不同、行为已正确，属三语言对齐规则的合理例外，
    非漏改。

### Changed
- 内部 per-ns abtest fetch 的请求 display_type 从 `FLAT_KV` 切换为
  `EACH_EXPERIMENT_GROUP`（experiment_type 仍为 `CONFIG_VERSION`），SDK 本地把
  groups + gray_hits 合并为同一 key→versionId 扁平 map（逐行复刻平台 flat 合并
  语义：灰度无条件优先于实验；实验组间后写覆盖；灰度间 first-writer-wins），
  值解析结果与原 FLAT_KV 消费语义等价，同时保留每-key 归因供命中日志使用。
  公共 API 签名与语义零改动（`GetExperimentResult` /
  `PrefetchConfigVersionFlatKvForNamespace` 等均不变；后者 docstring 更新说明
  内部已切 per-group）。

## [0.13.2] - 2026-08-24

### Changed
- `GetConfig` / `GetConfigDefault` / `GetConfigStatic` 命中日志从 `Debug` 提升到 `Info`
  （字段 `ns`/`key`/`version`，动态路径另带 `uid`/`trace_id`）。目的：在默认 `Info`
  级别下即可从调用方日志观测业务实际请求的 config key 与命中版本，无需临时开 Debug。
  出于泄漏风险，命中日志**不记录 config value**。

## [0.13.1] - 2026-07-28

### Fixed

- `go.mod` now correctly requires `api/gen/go` **v0.7.0**. v0.13.0 shipped with
  a stale `v0.5.0` pin, but the SDK source references
  `configv1.ConfigUpdateEvent_Heartbeat`, which only exists since `api/gen/go`
  v0.6.0 — so a plain
  `go get .../sdk/go/tipsyabconfig@v0.13.0` (MVS-resolving the pinned v0.5.0)
  failed to compile with `undefined: configv1.ConfigUpdateEvent_Heartbeat`.
  CI never caught it because the repo's committed `go.work` workspace resolves
  `api/gen/go` to the in-tree copy, masking the pin. No source changes; module
  manifest (`go.mod`/`go.sum`) only.

## [0.13.0] - 2026-07-28

### Added

- `Client.GetAllConfigs(ctx, abctx, ns)` and `Client.GetAllConfigsDefault(ctx,
  abctx)` — resolve EVERY dynamic config key under a namespace for one user in a
  single call and return a freshly allocated, caller-owned `map[string]string`.
  Each key resolves with the identical precedence as `GetConfig` (abtest
  whitelist / experiment hit > full release), sharing the same internal per-key
  resolver, so a key yields the same value under either call. Keys with neither
  an abtest hit nor a full-release value are **omitted** from the map (the
  batched form has no per-key default); an empty-string value is a valid value
  and is kept. The whole namespace is resolved against ONE cache snapshot
  captured up front (snapshot-consistent — never torn across a concurrent cache
  replace). At most one `GetExperimentResult` RPC is issued per (request link,
  ns) and it is **reused** with any `GetConfig` on the same `AbtestContext` +
  ns; when every key is pure full-release (`has_dynamic_resolution` explicitly
  `false`) no RPC is issued at all — a zero-key snapshot counts as vacuously
  all-static (no key could be an abtest hit), matching the Python/Java SDKs.
  A subscribed-but-not-yet-pulled namespace
  returns a non-nil empty map with a nil error and no RPC. `GetAllConfigsDefault`
  is the ns-optional form (resolves the project default namespace; returns
  `ErrNamespaceRequired` when none is configured). Nil-receiver / nil-`abctx`
  errors match `GetConfig` (`ErrClosed` / `ErrAbtestContextMissing`).

### Changed

- **A user id of `""` or `"0"` is now treated as identity-less.** When an
  `AbtestContext` carries such a uid, `GetConfig` / `GetAllConfigs` no longer
  issue a `GetExperimentResult` RPC — every namespace short-circuits to static
  full-release resolution (equivalent to `EmptyAbtestContext`), because neither
  value can be bucketed into an experiment or matched against a whitelist
  server-side. In particular `"0"`, previously sent verbatim as a real user id,
  is now identity-less. The short-circuit lives in the shared lazy-fetch layer,
  so it applies uniformly to `GetConfig`, `GetAllConfigs`,
  `PrefetchConfigVersionFlatKvForNamespace`, and `WaitForAbtest`. This is a
  behavior change but not breaking: an empty/`"0"` uid had no dependable
  experiment semantics before (server-side bucketing of an empty identity is
  meaningless). It is a deliberate short-circuit, not a degraded fallback, so it
  does NOT bump the `abtestFallback` metric; a single DEBUG line
  (`skip abtest: no-user uid`) is logged instead. Pre-seeded `MockAbtestContext`
  results still win (the results-map lookup precedes the short-circuit).

## [0.12.0] - 2026-07-23

### Changed

- **env is now judged server-side; the SDK no longer sends an env request
  field.** `env` matching moved to the abConfig server, which reads its own
  `TIPSY_ENV` process environment variable and compares it against each
  experiment's env set. As a result the SDK no longer stamps env onto any
  outbound request. Removed `Config.Env` and the `Client.Env()` accessor; the
  four request construction points (`GetExperimentResult`, the `config_version`
  flat_kv fetch behind `GetConfig`, `PullAll`, `Subscribe`) no longer set the
  request's `env` field.

### Compatibility

- The `env` field remains in the proto request messages (`api/gen/go` v0.7.0)
  and is left unset by this SDK — it is retained for wire compatibility with the
  previously released v0.11.0 and is now deprecated on the request path. Newer
  servers ignore any request-supplied env and use `TIPSY_ENV` instead, so both
  old (v0.11.0, still sending env) and new SDK builds interoperate with the new
  server. The `go.mod` `api/gen/go` pin is unchanged (no proto regen).

## [0.11.0] - 2026-07-22

### Added

- `Config.Env` (`string`, default `""`) — a single-value environment
  identifier stamped onto **every** outbound request: `GetExperimentResult`,
  the `config_version` flat_kv fetch behind `GetConfig`, the background
  `PullAll`, and the `Subscribe` stream. It tells the server which environment
  this process runs in so experiment env filtering can apply (an experiment
  with a non-empty env set is only entered when this env is a member; `env=""`
  enters only experiments that do not restrict their env). There is **no**
  environment-variable fallback and no `applyDefaults` handling — the value is
  used verbatim.
- `Client.Env()` accessor returning the configured env (`""` when unset),
  for debugging / parity with the Python and Java SDKs.

### Changed

- Requires `api/gen/go` v0.7.0 for the new `env` request field (added to the
  five SDK-facing request messages). The `go.mod` pin is bumped at release,
  alongside the `api/gen/go/v0.7.0` tag; the same bump also catches up the pin,
  which had lagged at `v0.5.0` while the v0.10.0 `Heartbeat` handling already
  required `v0.6.0`.

### Compatibility

- 100% backwards compatible. `Env` defaults to `""`; protojson omits an empty
  scalar in HTTP mode, so an unset `Env` is byte-for-byte wire-compatible with
  older servers. A configured env sent to an older server that predates the
  field is safely ignored (gRPC drops unknown fields; the HTTP gateway decodes
  with `DiscardUnknown`).

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

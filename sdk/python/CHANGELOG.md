# Changelog

All notable changes to the `tipsy-ab-config` Python SDK are documented in
this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.13.0] - 2026-07-23

### Changed

- **env is now judged server-side; the SDK no longer sends an env request
  field.** env matching moved to the abConfig server, which reads its own
  `TIPSY_ENV` process environment variable and compares it against each
  experiment's env set. Removed `Config.env`; the four request construction
  points (`get_experiment_result`, the `config_version` flat_kv fetch behind
  `get_config`, `_pull_once`, `_subscribe_once`) no longer set the request's
  `env` field.

### Compatibility

- The `env` field remains in the vendored proto bindings (`abtest_pb2.py` /
  `config_pb2.py`) and is left unset by this SDK — retained for wire
  compatibility with the previously released 0.12.0 and now deprecated on the
  request path. Newer servers ignore any request-supplied env and use
  `TIPSY_ENV` instead, so both old (0.12.0, still sending env) and new SDK
  builds interoperate with the new server. The proto bindings are unchanged (no
  regen).

## [0.12.0] - 2026-07-22

### Added

- `Config.env` (`str`, default `""`) — a single-value environment identifier
  stamped onto **every** outbound request: `GetExperimentResult`, the
  `config_version` flat_kv fetch behind `get_config`, the background `PullAll`,
  and the `Subscribe` stream. It tells the server which environment this
  process runs in so experiment env filtering can apply (an experiment with a
  non-empty env set is only entered when this env is a member; `env=""` enters
  only experiments that do not restrict their env). No `os.getenv` fallback —
  the value is used verbatim. Regenerates the vendored proto bindings
  (`abtest_pb2.py` / `config_pb2.py`) to include the `env` field on the five
  SDK-facing request messages. Mirrors the Go and Java SDKs.

### Compatibility

- 100% backwards compatible. `env` defaults to `""`; the HTTP transport's
  `MessageToJson` omits an empty scalar, so an unset `env` is byte-for-byte
  wire-compatible with older servers. A configured env sent to an older server
  that predates the field is safely ignored.

## [0.11.0] - 2026-07-16

### Added

- Ignore Subscribe `Heartbeat` events (forward-compat): the server may emit
  liveness-only `Heartbeat` frames on an otherwise-idle Subscribe stream. The
  SDK now handles them as an explicit no-op — no cache mutation, no sequence
  tracking, no metric — and regenerates the vendored proto bindings
  (`config_pb2.py`) to include the `Heartbeat` message. Mirrors the Go SDK.

### Changed

- Reset the Subscribe reconnect backoff after a healthy connection drop: a
  stream that stayed up at least 60s before dropping now reconnects at the
  initial delay instead of continuing the exponential climb. A short-lived
  connection still backs off exponentially. Mirrors the Go SDK.

## [0.10.0] - 2026-07-03

### Added

- Debug-level per-call timing log for `GetExperimentResult`. Both call
  sites (the public `get_experiment_result` and the `AbtestContext` lazy
  per-namespace fetch) emit one Debug record per RPC with `ns`, `trace_id`
  and float-millisecond `duration_ms` (an `err` field on failure). Fields
  are written both into the log message and `extra=` so they are visible
  under the default formatter and available as structured record
  attributes. Info level stays silent; the existing fallback warnings are
  unchanged. Mirrors the Go SDK v0.9.0 change.

## [0.9.0] - 2026-06-30

### Added

- Typed config accessors, mirroring the Go SDK's v0.7.0 surface. Static
  (sync, cache-only) and dynamic (async, abtest-resolved) variants for each
  scalar type plus JSON:
  - Static: `get_config_static_bool/long/double/string/json(namespace, key,
    default=...)`.
  - Dynamic: `async get_config_bool/long/double/string/json(ctx, namespace,
    key, default=...)`.
  - **Bool is lenient and never raises**: the stripped value casefold-equal to
    `"true"` or equal to `"1"` ⇒ `True`, everything else ⇒ `False`. The console
    writes canonical `true`/`false`, so the write-strict / read-lenient
    asymmetry is deliberate.
  - **Long uses `int(str)`**, which is arbitrary-precision in Python, so values
    beyond 2^53 are lossless.
  - Miss and value-parse failure both fall back to the supplied default; the
    underlying `get_config` exceptions (`SDKClosed` / `NamespaceRequired` /
    `NamespaceNotSubscribed` / abtest-context) still propagate — only
    `ValueError` / `json.JSONDecodeError` from parsing are swallowed.
  - The config value stays a canonical string end-to-end; the declared
    `value_type` is a console-side write contract and is not carried to the SDK.

## [0.8.0] - 2026-07-01

### Changed (BREAKING)

- **`GetExperimentResultResponse.gray_hits` is now grouped per gray release.**
  `GrayReleaseHit` changed from the flat `{release_id, key, version_id}` (one
  entry per `(release, key)`) to `{release_id, map<string,int64> key_versions}`
  (one entry per hit `release_id`; `key_versions` maps each `config_key.key`
  name → versionId). This aligns `gray_hits` with
  `ExperimentGroupResult.params_versions`. Read a single key's target via
  `gray_hits[i].key_versions[key_name]` instead of the removed
  `gray_hits[i].version_id`. No backward compatibility — the old flat fields
  are gone. The int64 values remain versionId (config_version PRIMARY KEY id,
  globally unique), never the semantic version_no.

## [0.7.0] - 2026-06-27

### Added

- `KeyState.has_dynamic_resolution` (`Optional[bool]`) — the SDK now
  deserializes the new `optional bool has_dynamic_resolution` field on each
  `KeyState` from both the `PullAll` and `Subscribe` snapshot paths. The value
  preserves wire presence: `True`/`False` when the server explicitly set it,
  `None` when the field was absent (an older server that predates it).
- `ConfigCache.has_dynamic_resolution(namespace, key) -> tuple[bool, bool]` —
  cache accessor returning `(value, present)`; `(False, False)` on any miss
  (no snapshot, no such key, or the field absent on the wire).

### Changed

- `Client.get_config` fast path: when the server has explicitly reported that a
  key carries no gray release / experiment (`has_dynamic_resolution` is present
  **and** `False`), `get_config` now **skips** the `GetExperimentResult` abtest
  wait and returns the full-release value directly. This removes a guaranteed-
  wasted RPC for pure-full-release keys. All fallback/default semantics and
  metrics are unchanged. When the field is absent (old server) or `True`, the
  existing always-wait abtest path is preserved exactly, so gray release /
  experiments never silently break.

### Server compatibility (REQUIRED — upgrade server first)

- This version expects the server to already emit the
  `has_dynamic_resolution` field on `KeyState`, i.e. a server built against
  **`api/gen/go` v0.3.0 or newer**. **Upgrade the server first, then this
  SDK.** If this SDK is pointed at an older server that does not emit the
  field, it safely falls back to the previous behaviour — it ALWAYS waits on
  the abtest result (correct results; gray release / experiments keep working),
  it just does not get the fast-path benefit. The fast path is only taken when
  the field is explicitly `False`, never when it is absent, so a new SDK on an
  old server never wrongly skips abtest.

## [0.6.0] - 2026-06-25

### Changed (BREAKING)

- `Client.new_abtest_context` and `Client.abtest_scope` no longer accept a
  `namespace` parameter. Construction is now a pure create and issues **no**
  `GetExperimentResult` RPC. Every namespace is fetched lazily and memoised
  at-most-once on first dynamic `get_config`. **Latency-shape change:** the
  default namespace is no longer eagerly pre-fetched at construction, so the
  first `get_config` for it now pays the GetExperimentResult RPC latency
  inline (previously that RPC was overlapped with construction).
- `AbtestMiddleware` no longer auto-prefetches on every request. It gained a
  `prefetch_paths: Optional[Sequence[str]] = None` parameter (an exact-match
  URL whitelist). The **default is no prefetch**; only requests whose
  `request.url.path` exactly matches a whitelisted path warm the default
  namespace. This removes the "every request through the middleware fires a
  possibly-wasted experiment RPC" behaviour.

### Added

- `AbtestContext.prefetch_config_version_flat_kv_for_namespace(ns)` — explicit,
  opt-in, single-namespace prefetch API. Non-blocking and idempotent; spawns
  the single per-ns `GetExperimentResult` (config_version + flat_kv) task so a
  later `get_config` for that namespace reuses it (at-most-once). Empty /
  identity-less / mock contexts and unsubscribed namespaces short-circuit
  without issuing any RPC. Requires a running asyncio event loop.

### Internal

- Renamed the internal `Client._get_experiment_result_for_ns` →
  `Client._fetch_config_version_flat_kv_for_ns` (hardwired config_version +
  flat_kv), to disambiguate it from the public general-purpose
  `Client.get_experiment_result` (unchanged). The slot-creation logic moved
  into a single shared `AbtestContext._ensure_fetch` primitive (synchronous,
  lock-free) used by both `wait_for_abtest` and the new prefetch API, removing
  the per-context `asyncio.Lock`.

## [0.5.0] - 2026-06-19

### Fixed

- Restore version-string parity: prior `python-sdk/v0.4.0` shipped with
  `__init__.py` still claiming 0.3.0; this release jumps to 0.5.0 to skip
  the inconsistent state and re-establish a clean baseline.
  (修复版本号一致性：上一版 `python-sdk/v0.4.0` 发布时 `__init__.py`
  仍声明 0.3.0，存在历史不一致；本版直接跳到 0.5.0 重建清晰基线。)

### Added

- Auto-enable client-side `round_robin` load balancing when the channel
  address uses the `dns:///` gRPC name resolver scheme (typically
  `dns:///<service>.<ns>.svc.cluster.local:<port>` for K8S Headless
  Service deployment). All other address forms (bare `host:port`,
  `grpc://`, `grpcs://`, `passthrough:///`, `unix:`) keep grpcio's
  default `pick_first` behavior. No SDK `Config` field changes; opt-in
  via address string only. See `docs/usage-and-integration.md` §4.1.

## [0.4.0] - 2026-06-18

### Removed (BREAKING)

- `Config.exposure_sink`, `Config.exposure_dedup_ttl`, class `ExposureSink`,
  class `ExposureEvent`, internal `ExposureEmitter`. The Python SDK no
  longer emits exposure events on `get_config`. Use the upstream
  experiment-result data report channel instead.
- `GetExperimentResultResponse.exposures` is retained on the proto wire
  for backward compatibility but is never populated by the server.

### Added

- `GetExperimentResultResponse.gray_hits` (`repeated GrayReleaseHit`) —
  populated when `display_type==EACH_EXPERIMENT_GROUP` and
  `experiment_type ∈ {CONFIG_VERSION, ALL}`; otherwise an empty list.

## [0.3.0] - 2026-06-18

### Changed

- **Repository move.** The Python SDK now lives in the public
  `Lightspeed-Intelligence/tipsy-ab-config-sdk` repository. Install via
  `pip install "git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/v0.3.0#subdirectory=sdk/python"`
  — no `GH_PAT` needed.
- **License → MIT.** The Python SDK package is now released under MIT,
  aligned with the parent repository.
- **`pyproject.toml` URLs** (`Homepage`, `Documentation`, `Source`,
  `Issues`, `Changelog`) all point at the new repository.

### Removed

- Dead `_proto` stubs that had never been imported by SDK production code
  (`tipsy/config/v1/internal_pb2*`, `tipsy/abtest/v1/internal_pb2*`,
  `tipsy/audit/v1/audit_pb2*`). The 3 backend-private protos that
  produced them stayed in the private `tipsy-ab-config` repo; only the
  2 SDK-facing protos (`config.proto` + `abtest.proto`) ship here.

### Migration

- Old installs pinned to
  `git+https://${GH_PAT}@github.com/Lightspeed-Intelligence/tipsy-ab-config.git@python-sdk/v0.2.0#subdirectory=sdk/python`
  continue to resolve via the legacy tag preserved on the ab-config repo.
  Starting with v0.3.0, install from the new public repo (no PAT).

## [0.2.0] - 2025-11-21

### Added

- Optional `trace_id` keyword argument on `Client.get_experiment_result`,
  `Client.new_abtest_context`, and `Client.abtest_scope`. When omitted or
  empty the SDK generates a fresh UUID v4 (36-char with dashes) and forwards
  it on the wire, so every request is trace-identifiable end-to-end without
  caller changes.
- `AbtestContext.trace_id` attribute, set at construction and reused by every
  per-namespace `GetExperimentResult` RPC the context issues.
- `AbtestMiddleware` now resolves the per-request `trace_id` from the
  inbound `X-Trace-Id` header, falling back to `X-Request-Id`, and finally
  to a freshly generated UUID. The resolved id is forwarded onto the
  AbtestContext so it propagates to every downstream RPC.
- Background `PullAll` and `Subscribe` RPCs now generate a fresh `trace_id`
  per call and log it at `debug` level before issuing the RPC, mirroring the
  Go SDK pattern.

## [0.1.0] - 2026-06-16

### Added

- 首发 release of the Tipsy AB-config Python SDK.
  - `Client` / `init` / `Config` — process-local config cache populated by a
    startup `PullAll`, long-lived server-streaming `Subscribe`, and a
    10-second fallback `PullAll` safety net.
  - `Client.get_config_static` — pure cache read, no abtest, no exposure.
  - `Client.get_config` — abtest-aware lookup with the per-request
    `AbtestContext` (abtest hit > full release fallback); emits exposure
    events asynchronously with a 5-minute per-process dedup window.
  - `Client.new_abtest_context` — eagerly pre-fetches the prefetch
    namespace; other namespaces lazy-loaded and memoised at-most-once on
    first dynamic `get_config`.
  - `Client.get_experiment_result` — exposes the full wire request
    (`namespace`, `user_info`, `layer_ids`, `type`, `display`).
  - `AbtestMiddleware` — FastAPI / Starlette middleware that binds
    `AbtestContext` into `contextvars` per request.
  - gRPC + HTTP transports (gRPC is the default; HTTP via `tipsy-ab-config[http]`).
  - Python 3.10 / 3.11 / 3.12 / 3.13 CI matrix.
  - Shipped proto bindings under `tipsy_ab_config/_proto/**` regenerated
    with `grpcio-tools==1.66.2` (protobuf 5.x major emission; runtime
    requires `protobuf>=5.29,<7`).

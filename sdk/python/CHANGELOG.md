# Changelog

All notable changes to the `tipsy-ab-config` Python SDK are documented in
this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
  Move to the new public install URL when bumping to v0.3.0+.

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

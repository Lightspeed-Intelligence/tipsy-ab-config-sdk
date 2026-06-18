# Changelog

All notable changes to the Tipsy AB-config Go SDK (`sdk/go/tipsyabconfig`)
are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Tag scheme: `sdk/go/tipsyabconfig/vX.Y.Z` (Go monorepo sub-module rule —
the tag MUST be prefixed with the relative module path). Consumers
install via:

```bash
go get github.com/Lightspeed-Intelligence/tipsy-ab-config/sdk/go/tipsyabconfig@vX.Y.Z
```

The SDK transitively pulls `api/gen/go` at the version pinned in
`go.mod`. Major proto changes therefore land via an `api/gen/go` tag
bump first, then an SDK tag bump.

## [Unreleased]

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
- `Client.GetExperimentResult` — low-level proxy for
  `AbtestService.GetExperimentResult`.
- `Middleware` (net/http) + `GinMiddleware` adapter.
- `ExposureEvent`, `ExposureSink`, `ExposureSinkFunc`, default `logSink`
  + async `exposureEmitter` with per-process 5-min dedup.

[Unreleased]: https://github.com/Lightspeed-Intelligence/tipsy-ab-config/compare/sdk/go/tipsyabconfig/v0.2.0...HEAD
[0.2.0]: https://github.com/Lightspeed-Intelligence/tipsy-ab-config/releases/tag/sdk%2Fgo%2Ftipsyabconfig%2Fv0.2.0
[0.1.0]: https://github.com/Lightspeed-Intelligence/tipsy-ab-config/releases/tag/sdk%2Fgo%2Ftipsyabconfig%2Fv0.1.0

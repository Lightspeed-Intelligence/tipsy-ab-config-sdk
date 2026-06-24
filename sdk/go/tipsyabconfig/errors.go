// Package tipsyabconfig is the Tipsy AB-config business SDK.
//
// It exposes:
//   - a local NamespaceSnapshot cache (dual snapshot_seq: business + experiment)
//     refreshed by PullAll on startup, server-streaming Subscribe at runtime,
//     and a 10 s fallback PullAll safety net.
//   - GetConfigStatic(ns, key, default) for service-level, no-user-context
//     lookups (full-release version only, no abtest call, no exposure).
//   - GetConfig(ctx, abctx, ns, key, default) for user-scoped lookups; this
//     awaits the per-request AbtestContext compute future and falls back to
//     the full-release version when abtest is unavailable.
//   - AbtestContext for per-request user attributes + per-namespace abtest
//     results: construction issues no RPC; every namespace (including the
//     project default) is fetched lazily on first dynamic GetConfig and
//     memoised so the request link issues at most one GetExperimentResult RPC
//     per ns. Opt into warming a namespace early via
//     AbtestContext.PrefetchConfigVersionFlatKvForNamespace.
//   - HTTP middleware adapter for any net/http compatible router; a thin
//     adapter is provided for gin-style frameworks via the AdaptHTTPRequest
//     helper.
//   - Async fire-and-forget exposure emission with a 5-minute per-process
//     dedup window keyed on (uid, key, version) per design §9.2.
//
// All gRPC traffic is JWT-authenticated via a Bearer token (or token-provider
// callback) attached as a grpc.PerRPCCredentials. The SDK never imports any
// of the server-side internal/{config,abtest,namespace,release,snapshot}
// packages and never touches the database directly — it is a pure gRPC
// downstream client.
package tipsyabconfig

import "errors"

// ErrAbtestContextMissing is returned by GetConfig when the caller passes a
// nil AbtestContext. Per abtest-platform-sdk.md §4 callers must explicitly
// pass either a NewAbtestContext result or EmptyAbtestContext().
var ErrAbtestContextMissing = errors.New("tipsyabconfig: abtest context missing")

// ErrStartupPullFailed is returned by Init when the configured startup
// PullAll cannot succeed and StartupFailOpen is false (fail-close path).
var ErrStartupPullFailed = errors.New("tipsyabconfig: startup PullAll failed")

// ErrClosed is returned by SDK calls made after Close.
var ErrClosed = errors.New("tipsyabconfig: client closed")

// ErrNamespaceRequired is returned by the ns-optional dynamic getConfig entry
// points when no explicit namespace is supplied AND the environment variable
// `PROJECT_DEFAULT_NAMESPACE` was empty / unset at Init (so the SDK has no
// defaultNamespace to fall back to). Per design 04 §B.1 (decision A-3) the SDK
// never hard-codes a default namespace.
var ErrNamespaceRequired = errors.New("tipsyabconfig: namespace required (no explicit ns and PROJECT_DEFAULT_NAMESPACE env is empty)")

// ErrNamespaceNotSubscribed is returned when the resolved namespace (explicit
// argument or the project default namespace) is not one of the namespaces the
// client subscribed to at Init. Per design 04 §B.1 the SDK only consumes
// subscribed namespaces; an unsubscribed ns has no local cache to resolve
// against, so the caller must fix the subscription / call site.
var ErrNamespaceNotSubscribed = errors.New("tipsyabconfig: namespace not subscribed")

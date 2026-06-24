package tipsyabconfig

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// UserProvider extracts the user identity for the inbound request. Returning
// a non-nil err means the SDK builds an EmptyAbtestContext for that request
// — the request still succeeds, but every GetConfig within it sees the
// full-release path.
//
// The signature is the neutral one called out in abtest-platform-sdk.md
// §3.2: ctx + *http.Request. Framework-specific adapters (gin / echo) wrap
// this signature with a thin glue layer.
type UserProvider func(ctx context.Context, r *http.Request) (uid string, attrs map[string]any, err error)

// abtestCtxKey is the unexported key used to stash the AbtestContext on the
// request ctx. Use AbtestContextFromContext to extract it from handlers.
type abtestCtxKey struct{}

// WithAbtestContext returns a child ctx carrying abctx. Used by the
// middleware and may be used directly on non-HTTP paths (background workers,
// queue consumers).
func WithAbtestContext(ctx context.Context, abctx *AbtestContext) context.Context {
	return context.WithValue(ctx, abtestCtxKey{}, abctx)
}

// AbtestContextFromContext returns the AbtestContext stashed by
// WithAbtestContext (or the Middleware). Returns nil if absent — callers
// must guard for nil and pass EmptyAbtestContext() as a fallback.
func AbtestContextFromContext(ctx context.Context) *AbtestContext {
	v, _ := ctx.Value(abtestCtxKey{}).(*AbtestContext)
	return v
}

// MiddlewareOption configures the optional behaviour of Middleware /
// GinMiddleware. The zero set of options preserves the default: the middleware
// only builds + attaches the AbtestContext and issues NO prefetch RPC.
type MiddlewareOption func(*middlewareConfig)

// middlewareConfig is the resolved option set for one middleware instance.
type middlewareConfig struct {
	// prefetchPaths is the exact-match request-path whitelist that opts a path
	// into default-namespace prefetch. Empty ⇒ no path ever prefetches.
	prefetchPaths map[string]struct{}
}

// PrefetchPaths opts the listed request paths into default-namespace prefetch:
// for an inbound request whose path EXACTLY matches one of paths (no prefix /
// regex), the middleware warms the client defaultNamespace via
// AbtestContext.PrefetchConfigVersionFlatKvForNamespace after building a real
// (non-empty) user ctx. Paths that are not whitelisted — and every path when
// no PrefetchPaths option is supplied — attach the ctx without any prefetch
// RPC. An empty defaultNamespace makes the prefetch a no-op.
//
// Prefetch is opt-in by design: blanket prefetch on every request would issue
// a large volume of useless empty experiment RPCs for handlers that never call
// GetConfig.
func PrefetchPaths(paths ...string) MiddlewareOption {
	return func(mc *middlewareConfig) {
		if mc.prefetchPaths == nil {
			mc.prefetchPaths = make(map[string]struct{}, len(paths))
		}
		for _, p := range paths {
			mc.prefetchPaths[p] = struct{}{}
		}
	}
}

// resolveMiddlewareConfig folds the variadic options into a middlewareConfig.
func resolveMiddlewareConfig(opts []MiddlewareOption) middlewareConfig {
	var mc middlewareConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&mc)
		}
	}
	return mc
}

// shouldPrefetch reports whether path is whitelisted for default-namespace
// prefetch under this config. Exact match only; an empty whitelist never
// matches.
func (mc middlewareConfig) shouldPrefetch(path string) bool {
	if len(mc.prefetchPaths) == 0 {
		return false
	}
	_, ok := mc.prefetchPaths[path]
	return ok
}

// Middleware returns a net/http compatible middleware that:
//
//  1. Invokes the UserProvider to extract (uid, attrs). On error, builds an
//     EmptyAbtestContext.
//  2. Attaches the AbtestContext to the request ctx. Construction issues NO
//     GetExperimentResult RPC; namespaces are fetched lazily on first dynamic
//     GetConfig.
//  3. When PrefetchPaths opts this request's exact path in (and the ctx is a
//     real user ctx with a non-empty client defaultNamespace), warms the
//     default namespace via PrefetchConfigVersionFlatKvForNamespace. Without
//     PrefetchPaths, no prefetch RPC is ever issued.
//  4. Calls the next handler.
//
// The returned middleware is framework-agnostic; gin, echo, chi etc. all
// have either a native net/http.Handler entry point or a tiny adapter
// available (see sdk/go/example/main.go for the gin pattern). For a gin
// adapter, use AdaptGinUserProvider in your own glue if you need ctx +
// *gin.Context interop.
func (c *Client) Middleware(provider UserProvider, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	mc := resolveMiddlewareConfig(opts)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			traceID := extractTraceFromRequest(r)
			var abctx *AbtestContext
			realUser := false
			if provider == nil {
				abctx = c.EmptyAbtestContext()
			} else {
				uid, attrs, err := provider(ctx, r)
				if err != nil {
					c.logger.Error("tipsyabconfig: user provider failed; using empty abtest ctx", "err", err, "trace_id", traceID)
					abctx = c.EmptyAbtestContext()
				} else {
					abctx = c.NewAbtestContextWithTraceID(ctx, uid, attrs, traceID)
					realUser = true
				}
			}
			// Whitelist-gated prefetch: only for a real user ctx on an exactly
			// matched path; empty defaultNamespace ⇒ no-op inside prefetch.
			if realUser && c.defaultNamespace != "" && mc.shouldPrefetch(r.URL.Path) {
				abctx.PrefetchConfigVersionFlatKvForNamespace(c.defaultNamespace)
			}
			next.ServeHTTP(w, r.WithContext(WithAbtestContext(ctx, abctx)))
		})
	}
}

// extractTraceFromRequest is the inbound-header trace_id extraction policy
// shared by the net/http Middleware and the gin adapter (sdk-trace-id §4,
// user decision 3): prefer X-Trace-Id, then X-Request-Id, otherwise generate
// a fresh uuid.New().String(). Returned value is always non-empty and
// whitespace-trimmed.
func extractTraceFromRequest(r *http.Request) string {
	if r != nil {
		if v := strings.TrimSpace(r.Header.Get("X-Trace-Id")); v != "" {
			return v
		}
		if v := strings.TrimSpace(r.Header.Get("X-Request-Id")); v != "" {
			return v
		}
	}
	return uuid.New().String()
}

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

// Middleware returns a net/http compatible middleware that:
//
//  1. Invokes the UserProvider to extract (uid, attrs). On error, builds an
//     EmptyAbtestContext.
//  2. Eagerly pre-fetches ONLY the project default namespace's config_version
//     result (non-blocking — see NewAbtestContext); other namespaces are
//     fetched lazily on first dynamic GetConfig.
//  3. Attaches the AbtestContext to the request ctx.
//  4. Calls the next handler.
//
// The returned middleware is framework-agnostic; gin, echo, chi etc. all
// have either a native net/http.Handler entry point or a tiny adapter
// available (see sdk/go/example/main.go for the gin pattern). For a gin
// adapter, use AdaptGinUserProvider in your own glue if you need ctx +
// *gin.Context interop.
func (c *Client) Middleware(provider UserProvider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			traceID := extractTraceFromRequest(r)
			var abctx *AbtestContext
			if provider == nil {
				abctx = c.EmptyAbtestContext()
			} else {
				uid, attrs, err := provider(ctx, r)
				if err != nil {
					c.logger.Error("tipsyabconfig: user provider failed; using empty abtest ctx", "err", err, "trace_id", traceID)
					abctx = c.EmptyAbtestContext()
				} else {
					abctx = c.NewAbtestContextWithTraceID(ctx, uid, attrs, traceID)
				}
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

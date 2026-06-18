package tipsyabconfig

import "net/http"

// GinLikeContext is the minimal subset of *gin.Context the SDK adapter
// needs. We keep it as an interface so callers can use the SDK with gin
// without forcing every consumer to depend on the gin module.
//
// To wire gin in your service:
//
//	r := gin.New()
//	r.Use(func(gc *gin.Context) {
//	    sdk.GinMiddleware(gc, userProvider)
//	    gc.Next()
//	})
//
// where userProvider is your UserProvider.
type GinLikeContext interface {
	Request() *http.Request
	SetRequest(*http.Request)
	Set(key string, value any)
}

// GinMiddleware is the adapter form of Middleware for gin-shaped contexts.
// It mutates gc to expose the AbtestContext both via gc.Set("abtest_ctx",
// ...) and via the request ctx (so downstream handlers can pull it through
// AbtestContextFromContext as well).
//
// trace_id is extracted from the inbound *http.Request headers via
// extractTraceFromRequest (X-Trace-Id → X-Request-Id → generated UUID,
// sdk-trace-id §4). gin.Context.GetHeader delegates to the same request
// headers, so reading via gc.Request().Header is exactly equivalent and
// avoids broadening the GinLikeContext interface.
//
// Callers wrap this in a tiny gin.HandlerFunc closure in their own glue
// code; doing so avoids any compile-time dep on the gin module from this
// package.
func (c *Client) GinMiddleware(gc GinLikeContext, provider UserProvider) {
	if gc == nil {
		return
	}
	req := gc.Request()
	if req == nil {
		return
	}
	ctx := req.Context()
	traceID := extractTraceFromRequest(req)
	var abctx *AbtestContext
	if provider == nil {
		abctx = c.EmptyAbtestContext()
	} else {
		uid, attrs, err := provider(ctx, req)
		if err != nil {
			c.logger.Error("tipsyabconfig: user provider failed; using empty abtest ctx", "err", err, "trace_id", traceID)
			abctx = c.EmptyAbtestContext()
		} else {
			abctx = c.NewAbtestContextWithTraceID(ctx, uid, attrs, traceID)
		}
	}
	gc.Set("abtest_ctx", abctx)
	gc.SetRequest(req.WithContext(WithAbtestContext(ctx, abctx)))
}

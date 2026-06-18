package tipsyabconfig

// SubTask B test file (sdk-trace-id design §4 GinMiddleware + user decision 3).
//
// Symmetric to middleware_traceid_test.go but exercises the gin adapter
// surface (GinLikeContext). Reuses the existing fakeGinContext defined in
// middleware_test.go so the test infra stays single-source-of-truth.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// runGinMiddlewareAndReadAbctx invokes Client.GinMiddleware and returns the
// AbtestContext that the adapter stashed on the request ctx. We read from the
// request ctx (not from gc.setKeys) because that mirrors what downstream
// handlers do via AbtestContextFromContext.
func runGinMiddlewareAndReadAbctx(t *testing.T, cli *Client, req *http.Request) *AbtestContext {
	t.Helper()
	gc := &fakeGinContext{req: req, setKeys: map[string]any{}}
	provider := func(_ context.Context, _ *http.Request) (string, map[string]any, error) {
		return "u-gin", nil, nil
	}
	cli.GinMiddleware(gc, provider)
	abctx := AbtestContextFromContext(gc.req.Context())
	if abctx == nil {
		t.Fatal("AbtestContext was not attached to request ctx via gin adapter")
	}
	return abctx
}

func TestGinMiddleware_TraceID_XTraceIdHeaderWins(t *testing.T) {
	cli := newClientForMiddleware(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Trace-Id", "gin-trace-A")
	req.Header.Set("X-Request-Id", "gin-req-IGNORED")

	abctx := runGinMiddlewareAndReadAbctx(t, cli, req)
	if abctx.traceID != "gin-trace-A" {
		t.Fatalf("traceID = %q, want %q", abctx.traceID, "gin-trace-A")
	}
}

func TestGinMiddleware_TraceID_FallsBackToXRequestId(t *testing.T) {
	cli := newClientForMiddleware(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "gin-req-B")

	abctx := runGinMiddlewareAndReadAbctx(t, cli, req)
	if abctx.traceID != "gin-req-B" {
		t.Fatalf("traceID = %q, want %q", abctx.traceID, "gin-req-B")
	}
}

func TestGinMiddleware_TraceID_NeitherHeaderGeneratesUUID(t *testing.T) {
	cli := newClientForMiddleware(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)

	abctx := runGinMiddlewareAndReadAbctx(t, cli, req)
	if abctx.traceID == "" {
		t.Fatal("expected SDK-generated traceID when no incoming headers present (gin)")
	}
	if !uuidV4Pattern.MatchString(abctx.traceID) {
		t.Fatalf("expected UUID-shaped traceID, got %q", abctx.traceID)
	}
}

func TestGinMiddleware_TraceID_WhitespaceHeaderTreatedAsEmpty(t *testing.T) {
	cli := newClientForMiddleware(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Trace-Id", "   ")
	req.Header.Set("X-Request-Id", "gin-req-trim-fallback")

	abctx := runGinMiddlewareAndReadAbctx(t, cli, req)
	if abctx.traceID != "gin-req-trim-fallback" {
		t.Fatalf("traceID = %q, want %q", abctx.traceID, "gin-req-trim-fallback")
	}
}

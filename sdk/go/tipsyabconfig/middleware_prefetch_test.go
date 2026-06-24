package tipsyabconfig

// Middleware URL-whitelist prefetch coverage (D4 / Acceptance Criteria 6).
//
// Contract under test for both Middleware (net/http) and GinMiddleware:
//   - No PrefetchPaths option ⇒ no prefetch RPC for any path (count==0 after
//     the middleware runs, before the handler calls GetConfig).
//   - PrefetchPaths("/x") + request to /x (real user ctx, non-empty default ns)
//     ⇒ exactly 1 prefetch RPC for the default ns.
//   - Unmatched path ⇒ no prefetch.
//   - Provider error / nil provider (empty ctx) ⇒ no prefetch even if path
//     matches.
//   - defaultNamespace == "" ⇒ matched path is a no-op (0 RPC).
//
// Exact-path matching (no prefix / regex) is asserted via the matched vs
// unmatched cases. RPC counting reuses the bufconn harness abServer; the
// prefetch is async so the "expect 1" cases use waitFor and the "expect 0"
// cases sleep-then-assert to give any erroneous goroutine time to surface.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newPrefetchMiddlewareHarness wires a Client whose default ns (ns1) is
// subscribed so a matched-path prefetch has a real ns to warm. The handler does
// NOT call GetConfig, so any abServer RPC observed is purely from middleware
// prefetch.
func newPrefetchMiddlewareHarness(t *testing.T, defaultNs string) (*testHarness, *Client) {
	t.Helper()
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cfg.DefaultNamespace = defaultNs
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return h, cli
}

func okProvider(_ context.Context, r *http.Request) (string, map[string]any, error) {
	return r.Header.Get("X-User-Id"), nil, nil
}

// --- net/http Middleware ---

func TestMiddleware_EmptyWhitelist_NoPrefetch(t *testing.T) {
	h, cli := newPrefetchMiddlewareHarness(t, "ns1")

	before := h.abServer.TotalCalls()
	handler := cli.Middleware(okProvider)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler intentionally does NOT call GetConfig.
	}))
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("X-User-Id", "u1")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	time.Sleep(80 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("no PrefetchPaths ⇒ no prefetch RPC; got %d", got)
	}
}

func TestMiddleware_MatchedPath_PrefetchesDefaultNs(t *testing.T) {
	h, cli := newPrefetchMiddlewareHarness(t, "ns1")

	before := h.abServer.Calls("ns1")
	handler := cli.Middleware(okProvider, PrefetchPaths("/warm"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/warm", nil)
	req.Header.Set("X-User-Id", "u1")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !waitFor(t, 2*time.Second, func() bool { return h.abServer.Calls("ns1")-before == 1 }) {
		t.Fatalf("matched path must trigger exactly 1 prefetch RPC for default ns; got %d", h.abServer.Calls("ns1")-before)
	}
	// And nothing beyond the single default-ns prefetch.
	time.Sleep(50 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 1 {
		t.Fatalf("matched path prefetch must be exactly 1 RPC total; got %d", got)
	}
}

func TestMiddleware_UnmatchedPath_NoPrefetch(t *testing.T) {
	h, cli := newPrefetchMiddlewareHarness(t, "ns1")

	before := h.abServer.TotalCalls()
	handler := cli.Middleware(okProvider, PrefetchPaths("/warm"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	// "/warm/extra" must NOT match "/warm" (exact match, no prefix).
	req := httptest.NewRequest(http.MethodGet, "/warm/extra", nil)
	req.Header.Set("X-User-Id", "u1")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	time.Sleep(80 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("unmatched path must not prefetch (exact match only); got %d", got)
	}
}

func TestMiddleware_ProviderError_NoPrefetchEvenIfMatched(t *testing.T) {
	h, cli := newPrefetchMiddlewareHarness(t, "ns1")

	errProvider := func(_ context.Context, _ *http.Request) (string, map[string]any, error) {
		return "", nil, errors.New("no auth")
	}
	before := h.abServer.TotalCalls()
	handler := cli.Middleware(errProvider, PrefetchPaths("/warm"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/warm", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	time.Sleep(80 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("provider error ⇒ empty ctx ⇒ no prefetch even on matched path; got %d", got)
	}
}

func TestMiddleware_NilProvider_NoPrefetchEvenIfMatched(t *testing.T) {
	h, cli := newPrefetchMiddlewareHarness(t, "ns1")

	before := h.abServer.TotalCalls()
	handler := cli.Middleware(nil, PrefetchPaths("/warm"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/warm", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	time.Sleep(80 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("nil provider ⇒ empty ctx ⇒ no prefetch even on matched path; got %d", got)
	}
}

func TestMiddleware_EmptyDefaultNs_MatchedPathNoOp(t *testing.T) {
	h, cli := newPrefetchMiddlewareHarness(t, "") // empty default ns

	before := h.abServer.TotalCalls()
	handler := cli.Middleware(okProvider, PrefetchPaths("/warm"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/warm", nil)
	req.Header.Set("X-User-Id", "u1")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	time.Sleep(80 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("empty default ns ⇒ matched-path prefetch is a no-op; got %d", got)
	}
}

// --- GinMiddleware ---

func TestGinMiddleware_EmptyWhitelist_NoPrefetch(t *testing.T) {
	h, cli := newPrefetchMiddlewareHarness(t, "ns1")

	before := h.abServer.TotalCalls()
	req := httptest.NewRequest(http.MethodGet, "/warm", nil)
	req.Header.Set("X-User-Id", "u-gin")
	gc := &fakeGinContext{req: req, setKeys: map[string]any{}}
	cli.GinMiddleware(gc, okProvider) // no PrefetchPaths

	time.Sleep(80 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("GinMiddleware with no PrefetchPaths ⇒ no prefetch RPC; got %d", got)
	}
}

func TestGinMiddleware_MatchedPath_PrefetchesDefaultNs(t *testing.T) {
	h, cli := newPrefetchMiddlewareHarness(t, "ns1")

	before := h.abServer.Calls("ns1")
	req := httptest.NewRequest(http.MethodGet, "/warm", nil)
	req.Header.Set("X-User-Id", "u-gin")
	gc := &fakeGinContext{req: req, setKeys: map[string]any{}}
	cli.GinMiddleware(gc, okProvider, PrefetchPaths("/warm"))

	if !waitFor(t, 2*time.Second, func() bool { return h.abServer.Calls("ns1")-before == 1 }) {
		t.Fatalf("GinMiddleware matched path must trigger exactly 1 prefetch RPC; got %d", h.abServer.Calls("ns1")-before)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 1 {
		t.Fatalf("GinMiddleware matched-path prefetch must be exactly 1 RPC total; got %d", got)
	}
}

func TestGinMiddleware_UnmatchedPath_NoPrefetch(t *testing.T) {
	h, cli := newPrefetchMiddlewareHarness(t, "ns1")

	before := h.abServer.TotalCalls()
	req := httptest.NewRequest(http.MethodGet, "/other", nil)
	req.Header.Set("X-User-Id", "u-gin")
	gc := &fakeGinContext{req: req, setKeys: map[string]any{}}
	cli.GinMiddleware(gc, okProvider, PrefetchPaths("/warm"))

	time.Sleep(80 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("GinMiddleware unmatched path must not prefetch; got %d", got)
	}
}

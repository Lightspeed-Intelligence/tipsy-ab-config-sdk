package tipsyabconfig

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddleware_AttachesAbtestContext(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v"}}}))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	provider := func(_ context.Context, r *http.Request) (string, map[string]any, error) {
		return r.Header.Get("X-User-Id"), map[string]any{"country": "US"}, nil
	}

	var seenUID string
	var seenAbCtx bool
	handler := cli.Middleware(provider)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		abctx := AbtestContextFromContext(r.Context())
		if abctx == nil {
			return
		}
		seenAbCtx = true
		seenUID = abctx.UserID()
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Id", "u1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !seenAbCtx {
		t.Fatal("handler did not see AbtestContext on request ctx")
	}
	if seenUID != "u1" {
		t.Fatalf("UserID() returned %q, want u1", seenUID)
	}
}

func TestMiddleware_NilProviderUsesEmpty(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.TotalCalls()
	handler := cli.Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		abctx := AbtestContextFromContext(r.Context())
		if abctx == nil {
			t.Error("expected an AbtestContext (empty) attached")
			return
		}
		// EmptyAbtestContext does not call Compute.
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("EmptyAbtestContext path must not call Compute; got %d", got)
	}
}

func TestMiddleware_ProviderErrorFallsToEmpty(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	provider := func(_ context.Context, r *http.Request) (string, map[string]any, error) {
		return "", nil, errors.New("no auth header")
	}
	handler := cli.Middleware(provider)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		abctx := AbtestContextFromContext(r.Context())
		if abctx == nil {
			t.Error("expected fallback EmptyAbtestContext")
		}
		if abctx != nil && abctx.UserID() != "" {
			t.Errorf("expected empty UserID, got %q", abctx.UserID())
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestAbtestContextFromContext_Nil(t *testing.T) {
	if got := AbtestContextFromContext(context.Background()); got != nil {
		t.Fatalf("expected nil for missing key, got %+v", got)
	}
}

// fakeGinContext implements GinLikeContext.
type fakeGinContext struct {
	req     *http.Request
	setKeys map[string]any
}

func (g *fakeGinContext) Request() *http.Request     { return g.req }
func (g *fakeGinContext) SetRequest(r *http.Request) { g.req = r }
func (g *fakeGinContext) Set(key string, value any)  { g.setKeys[key] = value }

func TestGinMiddleware_ForwardsAbtestContext(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-User-Id", "u-gin")
	gc := &fakeGinContext{req: req, setKeys: map[string]any{}}
	cli.GinMiddleware(gc, func(_ context.Context, r *http.Request) (string, map[string]any, error) {
		return r.Header.Get("X-User-Id"), nil, nil
	})
	abctx, ok := gc.setKeys["abtest_ctx"].(*AbtestContext)
	if !ok || abctx == nil {
		t.Fatalf("expected Set('abtest_ctx', *AbtestContext); got %T", gc.setKeys["abtest_ctx"])
	}
	if abctx.UserID() != "u-gin" {
		t.Fatalf("UserID mismatch: %q", abctx.UserID())
	}
	// The new request must carry abctx on its context too.
	if got := AbtestContextFromContext(gc.req.Context()); got == nil {
		t.Fatal("expected request ctx to also carry AbtestContext")
	}
}

func TestGinMiddleware_NilCtx(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()
	cli.GinMiddleware(nil, nil) // must not panic
}

func TestGinMiddleware_NilRequest(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()
	gc := &fakeGinContext{setKeys: map[string]any{}}
	cli.GinMiddleware(gc, nil) // must not panic when Request() is nil
}

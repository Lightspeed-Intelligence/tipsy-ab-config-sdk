package tipsyabconfig

// SubTask B test file (sdk-trace-id design §4 Middleware + user decision 3).
//
// Covers HTTP middleware trace_id extraction:
//   - X-Trace-Id wins ⇒ traceID = header value
//   - only X-Request-Id ⇒ traceID = header value
//   - neither ⇒ SDK-generated UUID v4
//   - all-whitespace value is treated as empty (trim) ⇒ falls through

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newClientForMiddleware builds a *Client suitable for the HTTP middleware
// tests. It deliberately leaves abtestTr nil and uses a single subscribed ns
// so NewAbtestContext / NewAbtestContextWithTraceID does NOT fire any RPC
// (lazy path is never triggered inside the handler). The trace_id assertion is
// against AbtestContext.traceID, which is set by the constructor regardless
// of RPC behaviour.
func newClientForMiddleware(t *testing.T) *Client {
	t.Helper()
	return &Client{
		cfg: Config{
			AbtestTimeout: 500 * time.Millisecond,
		},
		metrics:              newMetrics(),
		cache:                newConfigCache(newMetrics()),
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		subscribedNamespaces: []string{"ns1"},
		// No defaultNamespace ⇒ no eager prefetch in NewAbtestContextWithTraceID.
		defaultNamespace:    "",
		defaultNsSubscribed: false,
	}
}

// runMiddlewareAndReadAbctx invokes Client.Middleware with a UserProvider that
// returns ("u1", nil, nil) and returns the AbtestContext attached to the
// inner handler's request ctx.
func runMiddlewareAndReadAbctx(t *testing.T, cli *Client, req *http.Request) *AbtestContext {
	t.Helper()
	provider := func(_ context.Context, _ *http.Request) (string, map[string]any, error) {
		return "u1", nil, nil
	}
	var got *AbtestContext
	handler := cli.Middleware(provider)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = AbtestContextFromContext(r.Context())
	}))
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if got == nil {
		t.Fatal("AbtestContext was not attached to request ctx")
	}
	return got
}

func TestMiddleware_TraceID_XTraceIdHeaderWins(t *testing.T) {
	cli := newClientForMiddleware(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Trace-Id", "trace-A")
	// Also set X-Request-Id to verify the precedence rule (X-Trace-Id wins).
	req.Header.Set("X-Request-Id", "req-IGNORED")

	abctx := runMiddlewareAndReadAbctx(t, cli, req)
	if abctx.traceID != "trace-A" {
		t.Fatalf("traceID = %q, want %q (X-Trace-Id wins)", abctx.traceID, "trace-A")
	}
}

func TestMiddleware_TraceID_FallsBackToXRequestId(t *testing.T) {
	cli := newClientForMiddleware(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "req-B")

	abctx := runMiddlewareAndReadAbctx(t, cli, req)
	if abctx.traceID != "req-B" {
		t.Fatalf("traceID = %q, want %q (X-Request-Id fallback)", abctx.traceID, "req-B")
	}
}

func TestMiddleware_TraceID_NeitherHeaderGeneratesUUID(t *testing.T) {
	cli := newClientForMiddleware(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// No X-Trace-Id, no X-Request-Id.

	abctx := runMiddlewareAndReadAbctx(t, cli, req)
	if abctx.traceID == "" {
		t.Fatal("expected SDK-generated traceID when no incoming headers present")
	}
	if !uuidV4Pattern.MatchString(abctx.traceID) {
		t.Fatalf("expected UUID-shaped traceID, got %q", abctx.traceID)
	}
}

func TestMiddleware_TraceID_WhitespaceHeaderTreatedAsEmpty(t *testing.T) {
	cli := newClientForMiddleware(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// All-whitespace X-Trace-Id should be trimmed to "" and skipped, then
	// X-Request-Id fallback wins.
	req.Header.Set("X-Trace-Id", "   ")
	req.Header.Set("X-Request-Id", "req-trim-fallback")

	abctx := runMiddlewareAndReadAbctx(t, cli, req)
	if abctx.traceID != "req-trim-fallback" {
		t.Fatalf("traceID = %q, want %q (X-Trace-Id all-whitespace treated as empty)",
			abctx.traceID, "req-trim-fallback")
	}
}

func TestMiddleware_TraceID_AllWhitespaceBothHeadersGeneratesUUID(t *testing.T) {
	cli := newClientForMiddleware(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Trace-Id", "\t\t")
	req.Header.Set("X-Request-Id", " \n")

	abctx := runMiddlewareAndReadAbctx(t, cli, req)
	if abctx.traceID == "" {
		t.Fatal("expected SDK-generated traceID when both headers are whitespace-only")
	}
	if !uuidV4Pattern.MatchString(abctx.traceID) {
		t.Fatalf("expected UUID-shaped traceID, got %q", abctx.traceID)
	}
}

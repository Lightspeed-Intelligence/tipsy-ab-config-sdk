// Example HTTP service using the tipsyabconfig SDK.
//
// This example uses the stdlib net/http router; the SDK's Middleware is a
// plain func(http.Handler) http.Handler, so it composes naturally. For gin
// users, the SDK exposes a GinMiddleware adapter that takes a duck-typed
// interface and avoids any compile-time gin dependency — see
// sdk/go/tipsyabconfig/gin_adapter.go.
//
// Run with:
//
//	TIPSY_TOKEN=... CONFIG_ADDR=cfg:50051 ABTEST_ADDR=ab:50052 go run .
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfgAddr := envOr("CONFIG_ADDR", "localhost:50051")
	abAddr := envOr("ABTEST_ADDR", "localhost:50051")
	token := os.Getenv("TIPSY_TOKEN")
	if token == "" {
		logger.Error("TIPSY_TOKEN env var is required")
		os.Exit(2)
	}
	namespaces := strings.Split(envOr("NAMESPACES", "tipsy-chat"), ",")

	initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer initCancel()
	sdk, err := tipsyabconfig.Init(initCtx, tipsyabconfig.Config{
		Namespaces:        namespaces,
		ConfigServiceAddr: cfgAddr,
		AbtestServiceAddr: abAddr,
		Token:             token,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("sdk init failed", "err", err)
		os.Exit(1)
	}
	defer sdk.Close()

	mux := http.NewServeMux()

	// /static — no user context; demonstrates the typed static accessor.
	// rerank.threshold is a double, so GetConfigStaticFloat64 returns a real
	// float64 (no more juggling the "0.5" string default by hand).
	mux.HandleFunc("/static", func(w http.ResponseWriter, r *http.Request) {
		val, ok := sdk.GetConfigStaticFloat64("tipsy-chat", "rerank.threshold", 0.5)
		writeJSON(w, map[string]any{"key": "rerank.threshold", "value": val, "from_cache": ok})
	})

	// /user — user-scoped; demonstrates GetConfig + middleware.
	//
	// Constructing the AbtestContext (here, inside Middleware) issues NO
	// GetExperimentResult RPC: the namespace is fetched lazily on the first
	// GetConfig call below. Prefetch is opt-in — pass PrefetchPaths(...) to
	// warm the default namespace ahead of the handler only for the exact
	// request paths you list (e.g. tipsyabconfig.PrefetchPaths("/user")).
	// Without it, no path prefetches, avoiding useless empty experiment RPCs
	// on handlers that never call GetConfig.
	//
	// Middleware automatically extracts trace_id from inbound X-Trace-Id /
	// X-Request-Id headers (or generates one). To override or explicitly
	// stamp a trace_id (e.g. when bridging from another system that already
	// has its own correlation id), construct the AbtestContext yourself via
	// NewAbtestContextWithTraceID instead of relying on Middleware. The
	// /user-explicit handler below shows that pattern.
	userProvider := func(_ context.Context, r *http.Request) (string, map[string]any, error) {
		uid := r.Header.Get("X-User-Id")
		if uid == "" {
			uid = "anonymous"
		}
		return uid, map[string]any{
			"country": r.Header.Get("X-Country"),
		}, nil
	}
	// Opt this handler's path into default-namespace prefetch. Drop the
	// PrefetchPaths option to keep construction pure-create (lazy fetch on
	// first GetConfig).
	withCtx := sdk.Middleware(userProvider, tipsyabconfig.PrefetchPaths("/user"))
	mux.Handle("/user", withCtx(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		abctx := tipsyabconfig.AbtestContextFromContext(r.Context())
		val, err := sdk.GetConfigFloat64(r.Context(), abctx, "tipsy-chat", "rerank.threshold", 0.5)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"uid":      abctx.UserID(),
			"trace_id": abctx.TraceID(),
			"key":      "rerank.threshold",
			"value":    val,
		})
	})))

	// /user-explicit — bypasses Middleware and stamps an explicit trace_id
	// from a custom header so the SDK call, the server log and the upstream
	// caller's existing trace system all join on the same id.
	mux.HandleFunc("/user-explicit", func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		if uid == "" {
			uid = "anonymous"
		}
		// Pull the trace from an upstream-specific header. Empty ⇒ the SDK
		// generates a fresh UUID.
		traceID := r.Header.Get("X-Tipsy-Trace")
		abctx := sdk.NewAbtestContextWithTraceID(r.Context(), uid, map[string]any{
			"country": r.Header.Get("X-Country"),
		}, traceID)
		val, err := sdk.GetConfigFloat64(r.Context(), abctx, "tipsy-chat", "rerank.threshold", 0.5)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"uid":      abctx.UserID(),
			"trace_id": abctx.TraceID(),
			"key":      "rerank.threshold",
			"value":    val,
		})
	})

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		logger.Info("example listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http serve error", "err", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	logger.Info("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		fmt.Fprintln(os.Stderr, "writeJSON error:", err)
	}
}

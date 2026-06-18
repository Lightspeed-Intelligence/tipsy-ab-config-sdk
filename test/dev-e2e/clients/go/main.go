// Command go-sdk-client is the ST4 client-correctness driver for the Go SDK. It
// exercises the SDK over BOTH transports (gRPC and HTTP) against the seeded dev
// topology and asserts every expectations.json row that applies to go_sdk_*.
//
// One invocation tests both transports (internal loop). For each transport it:
//   - Init()s the SDK twice-over-time (one Client per transport),
//   - builds an AbtestContext per (user, attrs) via NewAbtestContext (the SDK
//     encodes raw attr values into the typed Value envelope itself, so we pass
//     RAW values — "US", not {"s":"US"} — decoded back from the fixture),
//   - calls GetConfig / GetConfigStatic and asserts the resolved value.
//
// gRPC transport: dev exposes a Cloudflare-proxied gRPC domain with standard
// TLS (docs/dev-http-token.md §gRPC 接入). The SDK address is just
// `grpcs://dev-ab-config-grpc.infra.fantacy.live:443` — no :authority override,
// no skip-verify. The deprecated IP-direct form
// `grpcs://47.253.175.59:443?authority=...&insecure=true` is only kept as a
// fallback if env vars override the defaults. If gRPC mode fails to connect,
// the driver WARNs, marks gRPC degraded in the summary, and continues HTTP
// mode rather than hard-crashing.
//
// Env vars (never hard-code secrets):
//
//	AB_CONFIG_HTTP_BASE       (default https://dev-ab-config.infra.fantacy.live)
//	AB_CONFIG_GRPC_ADDR       (default dev-ab-config-grpc.infra.fantacy.live:443)
//	AB_CONFIG_TOKEN           (REQUIRED)
//
// Run:  AB_CONFIG_TOKEN=... go run ./test/dev-e2e/clients/go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	tac "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
)

var namespaces = []string{"demo-test", "for_dev_agent_test"}

func main() {
	fixturesFlag := flag.String("fixtures", "", "path to expectations.json")
	transportFlag := flag.String("transport", "both", "transport: grpc|http|both")
	flag.Parse()

	token := os.Getenv("AB_CONFIG_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "FATAL: AB_CONFIG_TOKEN env var is required (see docs/dev-http-token.md)")
		os.Exit(2)
	}
	httpBase := envOr("AB_CONFIG_HTTP_BASE", "https://dev-ab-config.infra.fantacy.live")
	grpcAddr := envOr("AB_CONFIG_GRPC_ADDR", "dev-ab-config-grpc.infra.fantacy.live:443")
	// Legacy override: when AB_CONFIG_GRPC_AUTHORITY is set (e.g. an operator
	// is debugging via the direct origin IP), fall back to the old IP-direct
	// SDK addr form. Empty (default) → use the standard TLS domain above.
	grpcAuthority := os.Getenv("AB_CONFIG_GRPC_AUTHORITY")

	fixturesPath, err := resolveFixturesPath(*fixturesFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL: resolve fixtures path:", err)
		os.Exit(2)
	}
	exps, err := loadExpectations(fixturesPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL: load expectations:", err)
		os.Exit(2)
	}

	fmt.Println("================================================================")
	fmt.Println("ST4 Go SDK client-correctness driver")
	fmt.Println("  http base    :", httpBase)
	fmt.Println("  grpc addr    :", grpcAddr)
	if grpcAuthority != "" {
		fmt.Println("  grpc authority:", grpcAuthority, "(legacy IP-direct fallback)")
	}
	fmt.Println("  fixtures     :", fixturesPath, fmt.Sprintf("(%d rows)", len(exps)))
	fmt.Println("  WARNING      : hitting the SHARED dev environment")
	fmt.Println("================================================================")

	r := &results{}

	if *transportFlag == "both" || *transportFlag == "http" {
		runTransport(r, "go_sdk_http", tac.Config{
			Namespaces:        namespaces,
			ConfigServiceAddr: httpBase,
			AbtestServiceAddr: httpBase,
			Token:             token,
			Transport:         "http",
		}, exps)
	}

	if *transportFlag == "both" || *transportFlag == "grpc" {
		// Default: standard TLS to the gRPC domain — no :authority override,
		// no skip-verify (Cloudflare-proxied DNS, cert matches).
		// Legacy: when AB_CONFIG_GRPC_AUTHORITY is set, fall back to the
		// IP-direct form with authority override + skip-verify.
		var grpcTarget string
		if grpcAuthority == "" {
			grpcTarget = "grpcs://" + grpcAddr
		} else {
			grpcTarget = fmt.Sprintf("grpcs://%s?authority=%s&insecure=true", grpcAddr, grpcAuthority)
		}
		runTransport(r, "go_sdk_grpc", tac.Config{
			Namespaces:        namespaces,
			ConfigServiceAddr: grpcTarget,
			AbtestServiceAddr: grpcTarget,
			Token:             token,
			Transport:         "grpc",
		}, exps)
	}

	fmt.Println("----------------------------------------------------------------")
	fmt.Printf("SUMMARY: %d passed, %d failed (of %d checks)\n", r.passed, r.failed, r.passed+r.failed)
	if r.grpcDegraded {
		fmt.Println("NOTE: gRPC transport was DEGRADED (connect/init failed) — see WARNING above.")
	}
	if r.failed > 0 || r.grpcDegraded {
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runTransport inits the SDK with cfg, runs all applicable expectations for the
// client tag, and closes the SDK. A gRPC init/connect failure is caught, marked
// degraded, and does not crash the run (HTTP mode still proceeds).
func runTransport(r *results, client string, cfg tac.Config, exps []Expectation) {
	fmt.Printf("\n=== transport: %s ===\n", client)

	// Quiet the SDK's own logs (degraded-mode warnings) to keep PASS/FAIL output
	// readable; failures still surface via our assertions.
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// Fail-open so a transient startup pull failure does not abort Init; we want
	// to surface per-case failures, not a single hard crash. But we still detect
	// the empty-cache case via Health below.
	cfg.StartupFailOpen = true

	initCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cli, err := tac.Init(initCtx, cfg)
	if err != nil {
		if client == "go_sdk_grpc" {
			r.grpcDegraded = true
			fmt.Printf("WARNING: gRPC SDK Init failed; marking gRPC degraded and skipping: %v\n", err)
			return
		}
		r.fail("[%s] Init failed: %v", client, err)
		return
	}
	defer cli.Close()

	// Give the first PullAll a moment (Init already did a startup pull, but
	// fail-open may have absorbed a slow one). A quick health check surfaces an
	// empty cache so we don't report misleading per-case FAILs.
	if h := cli.Health(); h.StartupCacheEmpty {
		if client == "go_sdk_grpc" {
			r.grpcDegraded = true
			fmt.Printf("WARNING: gRPC SDK started with EMPTY cache (startup pull failed); marking degraded\n")
			return
		}
		r.fail("[%s] SDK started with empty cache (startup pull failed)", client)
		return
	}

	// Warm up the abtest path once before the asserted loop. The first abtest
	// RPC over a freshly-dialed cross-internet connection (esp. the direct-origin
	// gRPC) can be slow; priming it avoids a one-off slow first call skewing a
	// single assertion. The result is intentionally ignored.
	//
	// We use NewAbtestContextWithTraceID with a synthetic, recognisable id so
	// operators tailing the server log can pick out the warmup row by eye
	// (sdk-trace-id §4). This also exercises the explicit-trace_id path on
	// every dev-e2e run.
	{
		wctx, wcancel := context.WithTimeout(context.Background(), 20*time.Second)
		warmupTrace := fmt.Sprintf("dev-e2e-go-warmup-%s-%d", client, time.Now().UnixNano())
		abctx := cli.NewAbtestContextWithTraceID(wctx, "warmup-probe", nil, warmupTrace)
		if got := abctx.TraceID(); got != warmupTrace {
			r.fail("[%s] warmup AbtestContext.TraceID() = %q, want %q", client, got, warmupTrace)
		}
		_, _ = cli.GetConfig(wctx, abctx, namespaces[0], "welcome_text", "")
		wcancel()
	}

	for _, e := range exps {
		if !e.appliesTo(client) {
			continue
		}
		assertExpectation(cli, client, e, r)
	}
}

// Command platform is the ST3 platform-correctness driver: it exercises the
// dev tipsy-ab-config platform over the RAW HTTP read interfaces (no SDK) and
// asserts every row in fixtures/expectations.json plus a static-vs-dynamic
// matrix.
//
// It deliberately uses ONLY the Go standard library (net/http + encoding/json)
// so it has no dependency on the SDK or the generated protos — it is the
// "ground truth" wire-level driver. The companion grpc_smoke.sh covers the raw
// gRPC path via grpcurl.
//
// Endpoints (POST, JSON; requests are camelCase which the server also accepts,
// responses are snake_case, int64 fields are JSON strings):
//   - POST {base}/api/v1/config/static   {namespace, keys:[...]} → {values:{k:v}}
//   - POST {base}/api/v1/config/dynamic  {namespace, userId, userAttrs, keys}
//     → {values:{k:v}} (experiment/gray overlaid)
//   - POST {base}/api/v1/abtest/experiment_result
//     {namespace, userId, userAttrs,
//     experimentType, displayType, layerIds}
//     → {config_flat_kv, custom_flat_kv, ...}
//
// All access info is read from environment variables (never hard-coded secrets):
//   - AB_CONFIG_HTTP_BASE  (default https://dev-ab-config.infra.fantacy.live)
//   - AB_CONFIG_TOKEN      (REQUIRED; no default)
//
// Run:  AB_CONFIG_TOKEN=... go run ./test/dev-e2e/platform
//
// The drivers FAIL at runtime when the dev DB is not seeded yet — that is
// expected; seed first (sql/seed.sql), wait >=5s, then run.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	fixturesFlag := flag.String("fixtures", "", "path to expectations.json (default: resolved relative to repo root)")
	flag.Parse()

	base := envOr("AB_CONFIG_HTTP_BASE", "https://dev-ab-config.infra.fantacy.live")
	token := os.Getenv("AB_CONFIG_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "FATAL: AB_CONFIG_TOKEN env var is required (see docs/dev-http-token.md)")
		os.Exit(2)
	}

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
	fmt.Println("ST3 platform-correctness driver (raw HTTP)")
	fmt.Println("  base     :", base)
	fmt.Println("  fixtures :", fixturesPath, fmt.Sprintf("(%d rows)", len(exps)))
	fmt.Println("  WARNING  : hitting the SHARED dev environment")
	fmt.Println("================================================================")

	c := newHTTPClient(base, token)
	r := &results{}

	runExpectations(c, exps, r)
	runStaticMatrix(c, r)
	runStickyStability(c, r)

	fmt.Println("----------------------------------------------------------------")
	fmt.Printf("SUMMARY: %d passed, %d failed (of %d checks)\n", r.passed, r.failed, r.passed+r.failed)
	if r.failed > 0 {
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

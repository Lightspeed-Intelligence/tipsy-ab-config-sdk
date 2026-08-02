// ST9 combo data-plane driver for the Go SDK (assertion #2 + #5).
//
// Assertion #2 (frozen vs live): a combo holdout/opt group that HOLDS a key must
// serve the group's FROZEN version, while a key the group does NOT hold must
// serve the live full-release value. The fixture deliberately makes
// frozen != live (group frozen at v1, full release moved to v2) — if they were
// equal the assertion would pass either way and prove nothing.
//
// The run also asserts the abtestFallback DELTA is zero. Without it, a passing
// value could have been delivered by the ab->full fallback arm
// (get_config.go:166) rather than the abtest main path: full-release fallback is
// an intentional, working mechanism, so it masks the very thing #2 tests. We
// take a delta (not an absolute) because unrelated startup/other-key fallbacks
// would make an absolute-zero check red for the wrong reason.
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

const ns = "st9_combo"

type expect struct {
	uid   string
	role  string
	key   string
	value string
	why   string
}

func main() {
	transport := flag.String("transport", "both", "grpc|http|both")
	httpBase := flag.String("http", "http://localhost:8081", "HTTP base")
	grpcAddr := flag.String("grpc", "grpc://localhost:50052", "gRPC target (plaintext)")
	flag.Parse()

	token := os.Getenv("AB_CONFIG_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "FATAL: AB_CONFIG_TOKEN required")
		os.Exit(2)
	}

	// Held keys resolve to the FROZEN version (LIVE_v1) even though the live
	// full release has moved on to LIVE_v2; unheld keys track live.
	cases := []expect{
		{"st9-probe-9", "holdout", "managed_a", "LIVE_v1", "held by h1 -> frozen v1"},
		{"st9-probe-24", "holdout", "managed_a", "LIVE_v1", "held by h1 -> frozen v1"},
		{"st9-probe-4", "opt", "managed_a", "LIVE_v1", "held by o1 -> frozen v1"},
		{"st9-probe-10", "opt", "managed_a", "LIVE_v1", "held by o1 -> frozen v1"},
		// These two take the LIVE value because no combo group holds managed_a
		// FOR THEM — not because they are outside every combo. st9-probe-1 is in
		// c2mig's o_wide group (holding mig_key only), which makes it the stronger
		// case: "in a combo group, but that group does not hold this key => live
		// value". See fixture.md §10 for the full membership table; claiming
		// managed_a into c2mig or c3zero would turn these red.
		// st9-probe-0 is deliberately NOT used here: assertion #5 pins it into the
		// holdout-opt domain, making its expected value ambiguous.
		{"st9-probe-1", "in c2mig/o_wide", "managed_a", "LIVE_v2", "group does not hold this key -> live full v2"},
		{"st9-probe-2", "in E_mig/gA", "managed_a", "LIVE_v2", "group does not hold this key -> live full v2"},
		{"st9-probe-9", "holdout", "unheld_key", "UNHELD_v1", "NOT held -> live full"},
		{"st9-probe-4", "opt", "unheld_key", "UNHELD_v1", "NOT held -> live full"},
		{"st9-probe-1", "in c2mig/o_wide", "unheld_key", "UNHELD_v1", "no group holds it -> live full"},
	}

	pass, fail := 0, 0
	run := func(label string, cfg tac.Config) {
		fmt.Printf("\n=== %s ===\n", label)
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		cfg.Namespaces = []string{ns}
		cfg.Token = token
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cli, err := tac.Init(ctx, cfg)
		if err != nil {
			fmt.Printf("FAIL [%s] Init: %v\n", label, err)
			fail++
			return
		}
		defer cli.Close()
		if h := cli.Health(); h.StartupCacheEmpty {
			fmt.Printf("FAIL [%s] startup cache empty\n", label)
			fail++
			return
		}

		before := cli.Metrics().AbtestFallbackTotal(ns)
		for _, c := range cases {
			rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
			abctx := cli.NewAbtestContext(rctx, c.uid, nil)
			got, err := cli.GetConfig(rctx, abctx, ns, c.key, "<DEFAULT>")
			rcancel()
			switch {
			case err != nil:
				fmt.Printf("FAIL [%s] %s/%s err=%v\n", label, c.uid, c.key, err)
				fail++
			case got != c.value:
				fmt.Printf("FAIL [%s] %-13s %-11s got=%-10q want=%-10q (%s)\n",
					label, c.uid, c.key, got, c.value, c.why)
				fail++
			default:
				fmt.Printf("PASS [%s] %-13s %-11s = %-10q (%s)\n", label, c.uid, c.key, got, c.why)
				pass++
			}
		}
		// Delta, not absolute: unrelated fallbacks must not make this red, but a
		// fallback DURING these calls means the values above came from the
		// full-release arm rather than the abtest main path.
		delta := cli.Metrics().AbtestFallbackTotal(ns) - before
		if delta == 0 {
			fmt.Printf("PASS [%s] abtestFallback delta = 0 (values came from the abtest main path)\n", label)
			pass++
		} else {
			fmt.Printf("FAIL [%s] abtestFallback delta = %d (values may be fallback-delivered)\n", label, delta)
			fail++
		}
	}

	if *transport == "both" || *transport == "http" {
		run("go_http", tac.Config{ConfigServiceAddr: *httpBase, AbtestServiceAddr: *httpBase, Transport: "http"})
	}
	if *transport == "both" || *transport == "grpc" {
		run("go_grpc", tac.Config{ConfigServiceAddr: *grpcAddr, AbtestServiceAddr: *grpcAddr, Transport: "grpc"})
	}

	total := pass + fail
	fmt.Printf("\n----\nSUMMARY: pass=%d fail=%d of %d checks\n", pass, fail, total)
	wantPer := len(cases) + 1
	wantTotal := wantPer
	if *transport == "both" {
		wantTotal = 2 * wantPer
	}
	if total != wantTotal {
		fmt.Printf("OBSERVATION-COUNT MISMATCH: ran %d checks, expected %d\n", total, wantTotal)
		os.Exit(1)
	}
	fmt.Printf("OBSERVATION-COUNT OK (%d expected)\n", wantTotal)
	if fail > 0 {
		os.Exit(1)
	}
}

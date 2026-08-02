// ST9 assertion #5: layer-whitelist pinning routes correctly, verified END-TO-END
// through the Go SDK (not just the server's group output).
//
// Why this is an SDK-level assertion: the pin changes which group the uid lands
// in, which changes the key's resolved version, which the SDK must then find in
// its own snapshot. A server-only check would not exercise that last step.
//
// Two properties make the check non-degenerate:
//
//  1. The pinned uid's PRE-PIN group was recorded from the real server
//     (test/dev-e2e control file), not re-predicted by the bucket solver. Had we
//     re-predicted, the expectation would come from the same salt-fallback rule
//     the code under test uses, and the assertion would be a tautology.
//  2. The pin target is the NARROW slot (holdout-opt domain = 10% of the combo
//     layer) and the uid hashes into the WIDE slot (simple = 90%). So if the pin
//     silently stops working (topology.go:84-93 falls back to hashing when the
//     pinned entity no longer occupies a slot), the uid lands elsewhere with
//     ~90% probability per uid rather than coincidentally staying put.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	tac "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
)

const ns = "st9_combo"

func main() {
	token := os.Getenv("AB_CONFIG_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "FATAL: AB_CONFIG_TOKEN required")
		os.Exit(2)
	}
	base := "http://localhost:8081"
	if v := os.Getenv("AB_CONFIG_HTTP_BASE"); v != "" {
		base = v
	}

	cli, err := tac.Init(context.Background(), tac.Config{
		Namespaces:        []string{ns},
		ConfigServiceAddr: base,
		AbtestServiceAddr: base,
		Token:             token,
		Transport:         "http",
		StartupFailOpen:   true,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		fmt.Println("FAIL Init:", err)
		os.Exit(1)
	}
	defer cli.Close()

	pass, fail := 0, 0
	check := func(uid, key, want, why string) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		got, err := cli.GetConfig(ctx, cli.NewAbtestContext(ctx, uid, nil), ns, key, "<DEFAULT>")
		switch {
		case err != nil:
			fmt.Printf("FAIL %-13s %-11s err=%v\n", uid, key, err)
			fail++
		case got != want:
			fmt.Printf("FAIL %-13s %-11s got=%q want=%q (%s)\n", uid, key, got, want, why)
			fail++
		default:
			fmt.Printf("PASS %-13s %-11s = %-10q (%s)\n", uid, key, got, why)
			pass++
		}
	}

	fmt.Println("=== #5 layer-whitelist pinning, end-to-end via Go SDK ===")
	// PINNED uid: recorded pre-pin group was NONE (simple, live v2). After pinning
	// into the holdout-opt domain it lands in h1, which holds managed_a frozen at
	// v1 => the SDK must now resolve the FROZEN value.
	check("st9-probe-0", "managed_a", "LIVE_v1", "PINNED into holdout-opt -> frozen v1 (pre-pin was LIVE_v2)")
	// The pin only redirects routing; a key no combo group holds still tracks live.
	check("st9-probe-0", "unheld_key", "UNHELD_v1", "PINNED but key unheld -> still live full")
	// NOT pinned, same pre-pin state as the pinned uid: must still be live v2.
	// This is the control arm — it proves the pin is uid-scoped and that
	// "everyone gets frozen" is not the reason the first check passed.
	check("st9-probe-1", "managed_a", "LIVE_v2", "NOT pinned -> still live full v2")
	check("st9-probe-2", "managed_a", "LIVE_v2", "NOT pinned -> still live full v2")

	total := pass + fail
	fmt.Printf("\n----\nSUMMARY: pass=%d fail=%d of %d checks\n", pass, fail, total)
	if total != 4 {
		fmt.Printf("OBSERVATION-COUNT MISMATCH: ran %d, expected 4\n", total)
		os.Exit(1)
	}
	fmt.Println("OBSERVATION-COUNT OK (4 expected)")
	if fail > 0 {
		os.Exit(1)
	}
}

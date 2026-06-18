// Command roundrobin-load is a focused gRPC load driver used by
// `verify-roundrobin.sh` to exercise the Go SDK's `dns:///` resolver +
// round_robin LB across 3 ab-config backend instances. It is intentionally
// independent from `test/dev-e2e/load/` (which is an HTTP-only driver).
//
// Behavior:
//   - dial target: `dns:///ab-config-headless.local:50051` (set via -target)
//   - this forces grpc-go to fan out across all A records returned by docker's
//     embedded DNS and apply the SDK's auto-injected round_robin service
//     config (sdk.go::serviceConfigFor).
//   - calls AbtestService.GetExperimentResult repeatedly with random user IDs
//     in the for_dev_agent_test namespace; each call bumps the abtest_compute
//     metric on the chosen backend.
//   - prints PASS/FAIL summary; exits non-zero on any hard error so the shell
//     script can detect a hard failure separately from the metric-balance
//     assertion.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	tac "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
)

func main() {
	var (
		target      = flag.String("target", "dns:///ab-config-headless.local:50051", "gRPC dial target (must start with dns:/// to test round_robin)")
		ns          = flag.String("ns", "for_dev_agent_test", "namespace to bucket against")
		concurrency = flag.Int("concurrency", 50, "concurrent gRPC callers")
		duration    = flag.Duration("duration", 30*time.Second, "load duration")
	)
	flag.Parse()

	token := os.Getenv("AB_CONFIG_TOKEN")
	if token == "" {
		log.Fatal("AB_CONFIG_TOKEN env var required (HS256 service token signed with TIPSY_SERVICE_SECRET=devsecret)")
	}

	fmt.Printf("================================================================\n")
	fmt.Printf("Headless round_robin load driver\n")
	fmt.Printf("  target      : %s\n", *target)
	fmt.Printf("  namespace   : %s\n", *ns)
	fmt.Printf("  concurrency : %d\n", *concurrency)
	fmt.Printf("  duration    : %s\n", *duration)
	fmt.Printf("================================================================\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sdk, err := tac.Init(ctx, tac.Config{
		Namespaces:        []string{*ns},
		ConfigServiceAddr: *target,
		AbtestServiceAddr: *target,
		Token:             token,
		Transport:         "grpc",
	})
	if err != nil {
		log.Fatalf("SDK Init failed: %v", err)
	}
	defer sdk.Close()

	var (
		okCount  atomic.Int64
		errCount atomic.Int64
	)
	loadCtx, loadCancel := context.WithTimeout(ctx, *duration)
	defer loadCancel()

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 8)
			for {
				select {
				case <-loadCtx.Done():
					return
				default:
				}
				if _, err := rand.Read(buf); err != nil {
					errCount.Add(1)
					continue
				}
				uid := "u-" + hex.EncodeToString(buf)
				callCtx, callCancel := context.WithTimeout(loadCtx, 2*time.Second)
				_, err := sdk.GetExperimentResult(callCtx, tac.ExperimentResultRequest{
					Namespace:   *ns,
					UserInfo:    tac.UserInfo{UID: uid},
					Type:        tac.ExperimentTypeConfigVersion,
					DisplayType: tac.ResultDisplayType(0),
				})
				callCancel()
				if err != nil {
					if loadCtx.Err() != nil {
						return
					}
					errCount.Add(1)
				} else {
					okCount.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	ok := okCount.Load()
	er := errCount.Load()
	total := ok + er
	rate := float64(ok) / duration.Seconds()
	fmt.Printf("\nload finished:\n")
	fmt.Printf("  total calls : %d\n", total)
	fmt.Printf("  ok          : %d\n", ok)
	fmt.Printf("  errors      : %d\n", er)
	fmt.Printf("  ok QPS      : %.1f\n", rate)
	if total == 0 {
		fmt.Println("FAIL: no calls were made")
		os.Exit(1)
	}
	if float64(er)/float64(total) > 0.01 {
		fmt.Printf("FAIL: error rate %.2f%% > 1%%\n", 100*float64(er)/float64(total))
		os.Exit(1)
	}
}

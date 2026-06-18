// Command load is the ST5 medium-load test driver. It drives a chosen HTTP read
// endpoint against the dev tipsy-ab-config platform with random user IDs (so
// traffic spreads across experiment buckets), collects QPS / latency
// percentiles / error rate / status-code distribution, prints a summary, and
// writes a JSON results file.
//
// It uses ONLY the Go standard library: a worker pool plus an optional
// token-channel rate limiter fed by a time.Ticker (no golang.org/x/time/rate
// dependency). Defaults are MEDIUM load and it honors ctrl-C (context cancel)
// for a clean shutdown.
//
// Flags:
//
//	-concurrency     worker count (default 150)
//	-duration        run length (default 150s)
//	-target-qps      cap requests/sec; 0 = unlimited up to concurrency (default 2000, medium load)
//	-endpoint        dynamic|experiment_result (default experiment_result)
//	-ns              namespace (default for_dev_agent_test)
//	-out             results JSON path (default test/dev-e2e/load/last-run.json)
//
// Env vars (never hard-code secrets):
//
//	AB_CONFIG_HTTP_BASE  (default https://dev-ab-config.infra.fantacy.live)
//	AB_CONFIG_TOKEN      (REQUIRED)
//
// Run:  AB_CONFIG_TOKEN=... go run ./test/dev-e2e/load
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type config struct {
	base        string
	token       string
	concurrency int
	duration    time.Duration
	targetQPS   int
	endpoint    string
	ns          string
	out         string
}

func main() {
	cfg := config{}
	flag.IntVar(&cfg.concurrency, "concurrency", 150, "number of concurrent workers")
	flag.DurationVar(&cfg.duration, "duration", 150*time.Second, "test duration")
	flag.IntVar(&cfg.targetQPS, "target-qps", 2000, "target requests/sec cap (0 = unlimited up to concurrency)")
	flag.StringVar(&cfg.endpoint, "endpoint", "experiment_result", "endpoint: dynamic|experiment_result")
	flag.StringVar(&cfg.ns, "ns", "for_dev_agent_test", "namespace to hit")
	flag.StringVar(&cfg.out, "out", defaultOutPath(), "results JSON output path")
	flag.Parse()

	cfg.base = envOr("AB_CONFIG_HTTP_BASE", "https://dev-ab-config.infra.fantacy.live")
	cfg.token = os.Getenv("AB_CONFIG_TOKEN")
	if cfg.token == "" {
		fmt.Fprintln(os.Stderr, "FATAL: AB_CONFIG_TOKEN env var is required (see docs/dev-http-token.md)")
		os.Exit(2)
	}
	if cfg.endpoint != "dynamic" && cfg.endpoint != "experiment_result" {
		fmt.Fprintf(os.Stderr, "FATAL: -endpoint must be dynamic|experiment_result, got %q\n", cfg.endpoint)
		os.Exit(2)
	}

	fmt.Println("################################################################")
	fmt.Println("# ST5 MEDIUM-LOAD driver — hitting the SHARED dev environment! #")
	fmt.Println("################################################################")
	fmt.Printf("  base        : %s\n", cfg.base)
	fmt.Printf("  endpoint    : %s\n", cfg.endpoint)
	fmt.Printf("  namespace   : %s\n", cfg.ns)
	fmt.Printf("  concurrency : %d\n", cfg.concurrency)
	fmt.Printf("  duration    : %s\n", cfg.duration)
	if cfg.targetQPS > 0 {
		fmt.Printf("  target QPS  : %d (rate-limited)\n", cfg.targetQPS)
	} else {
		fmt.Printf("  target QPS  : unlimited (bounded by concurrency)\n")
	}
	fmt.Printf("  out         : %s\n", cfg.out)
	fmt.Println("  (ctrl-C for a clean early shutdown)")
	fmt.Println("----------------------------------------------------------------")

	run(cfg)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(cfg config) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.duration)
	defer cancel()

	// Honor ctrl-C / SIGTERM for a clean early shutdown.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	coll := newCollector()
	driver := newDriver(cfg)

	// Optional rate limiter: a buffered token channel refilled by a ticker. When
	// targetQPS == 0 the limiter is nil and workers run flat-out (bounded by
	// concurrency). A small buffer smooths bursts without exceeding the cap.
	var limiter *rateLimiter
	if cfg.targetQPS > 0 {
		limiter = newRateLimiter(sigCtx, cfg.targetQPS)
		defer limiter.stop()
	}

	start := time.Now()
	var wg sync.WaitGroup
	var inflight atomic.Int64

	// Progress ticker prints a heartbeat every 10s.
	progressDone := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sigCtx.Done():
				close(progressDone)
				return
			case <-t.C:
				elapsed := time.Since(start).Seconds()
				total := coll.total()
				fmt.Printf("  [%4.0fs] requests=%d qps=%.0f errors=%d inflight=%d\n",
					elapsed, total, float64(total)/elapsed, coll.errors(), inflight.Load())
			}
		}
	}()

	for i := 0; i < cfg.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if sigCtx.Err() != nil {
					return
				}
				if limiter != nil {
					if !limiter.acquire() {
						return // limiter ctx done
					}
				}
				inflight.Add(1)
				status, sample, err := driver.do(sigCtx)
				inflight.Add(-1)
				if sigCtx.Err() != nil && err != nil {
					// shutdown-induced cancellation — don't count as an error.
					return
				}
				coll.record(status, sample, err)
			}
		}()
	}

	wg.Wait()
	<-progressDone
	elapsed := time.Since(start)

	summary := coll.summarize(cfg, elapsed)
	printSummary(summary)
	if err := writeResults(cfg.out, summary); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: failed to write results JSON: %v\n", err)
	} else {
		fmt.Printf("\nWrote results to %s\n", cfg.out)
	}

	// Non-zero exit if the error rate exceeds the 1%% acceptance target so CI /
	// the main agent notices a degraded run.
	if summary.ErrorRatePct > 1.0 {
		fmt.Printf("ERROR RATE %.2f%% exceeds 1%% target\n", summary.ErrorRatePct)
		os.Exit(1)
	}
}

func defaultOutPath() string {
	// Default relative to repo root; works with `go run ./test/dev-e2e/load`.
	return "test/dev-e2e/load/last-run.json"
}

// rateLimiter is a simple ticker-fed token bucket (stdlib only). acquire blocks
// until a token is available or the context is done.
type rateLimiter struct {
	tokens chan struct{}
	ticker *time.Ticker
	ctx    context.Context
	done   chan struct{}
}

func newRateLimiter(ctx context.Context, qps int) *rateLimiter {
	// Refill one token per (1s/qps). Buffer up to a small burst (~ qps/10, min 1)
	// so brief scheduling jitter doesn't starve workers while still capping the
	// average rate at qps.
	burst := qps / 10
	if burst < 1 {
		burst = 1
	}
	interval := time.Second / time.Duration(qps)
	if interval <= 0 {
		interval = time.Microsecond
	}
	rl := &rateLimiter{
		tokens: make(chan struct{}, burst),
		ticker: time.NewTicker(interval),
		ctx:    ctx,
		done:   make(chan struct{}),
	}
	go rl.fill()
	return rl
}

func (rl *rateLimiter) fill() {
	defer close(rl.done)
	for {
		select {
		case <-rl.ctx.Done():
			return
		case <-rl.ticker.C:
			select {
			case rl.tokens <- struct{}{}:
			default: // bucket full; drop the token (cap respected)
			}
		}
	}
}

func (rl *rateLimiter) acquire() bool {
	select {
	case <-rl.tokens:
		return true
	case <-rl.ctx.Done():
		return false
	}
}

func (rl *rateLimiter) stop() {
	rl.ticker.Stop()
	<-rl.done
}

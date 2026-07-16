package tipsyabconfig

// Backoff-reset coverage for runSubscribe (design §Proposed Design 3b, ST3,
// §Testing Plan "退避重置" + "接线测试", Acceptance Criteria 4).
//
// Two layers:
//   1. resetBackoffIfStable — a pure, deterministic, timing-free unit test.
//      This is the PRIMARY coverage for ST3's decision logic (reset vs keep vs
//      boundary) and never flakes.
//   2. A white-box wiring test that drives runSubscribe against a bufconn fake
//      whose connections all stay up longer than an injected tiny
//      stableResetThreshold, then die with a real (Unavailable) error. It
//      proves the reset decision is actually WIRED into the reconnect loop
//      (start marker + call + assignment) so a refactor can't silently regress
//      it past the pure-function test. It asserts a loose upper bound on the
//      reconnect cadence to stay non-flaky.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// TestInitialSubscribeBackoffConstant pins SG2: the single source-of-truth
// initial-backoff constant is 1s (used for the loop's initial value, the EOF
// reset, and resetBackoffIfStable's stable return).
func TestInitialSubscribeBackoffConstant(t *testing.T) {
	if initialSubscribeBackoff != time.Second {
		t.Fatalf("initialSubscribeBackoff = %v, want 1s", initialSubscribeBackoff)
	}
}

// TestResetBackoffIfStable is the deterministic, timing-free core coverage for
// ST3's reset decision. uptime >= threshold => reset to the initial backoff
// (regardless of how large the current backoff had grown); uptime < threshold
// => return the current backoff unchanged (the caller then doubles it). SG-a:
// a threshold <= 0 means "never treat as stable" and always keeps the backoff.
func TestResetBackoffIfStable(t *testing.T) {
	const (
		s  = time.Second
		ms = time.Millisecond
	)
	const prodThreshold = 60 * s

	cases := []struct {
		name      string
		backoff   time.Duration
		uptime    time.Duration
		threshold time.Duration
		want      time.Duration
	}{
		// Healthy-then-dropped: reset even from a grown / capped backoff.
		{"stable from 8s backoff resets", 8 * s, 61 * s, prodThreshold, initialSubscribeBackoff},
		{"stable from 30s cap resets", 30 * s, 120 * s, prodThreshold, initialSubscribeBackoff},
		// Boundary: uptime == threshold counts as stable (>= comparison).
		{"uptime == threshold resets (boundary)", 30 * s, prodThreshold, prodThreshold, initialSubscribeBackoff},
		// Just below threshold: keep current backoff (caller doubles it).
		{"uptime just below threshold keeps", 8 * s, prodThreshold - ms, prodThreshold, 8 * s},
		{"tiny uptime keeps backoff", 4 * s, 10 * ms, prodThreshold, 4 * s},
		{"zero uptime keeps backoff", 2 * s, 0, prodThreshold, 2 * s},
		// Tiny-threshold band mirrors the wiring test's white-box injection.
		{"small threshold: stable resets", 8 * s, 30 * ms, 20 * ms, initialSubscribeBackoff},
		{"small threshold: unstable keeps", 8 * s, 10 * ms, 20 * ms, 8 * s},
		{"small threshold: boundary resets", 8 * s, 20 * ms, 20 * ms, initialSubscribeBackoff},
		// SG-a defensive contract: threshold <= 0 means "never treat as stable",
		// so the current backoff is kept regardless of how large uptime is (a
		// zero/negative threshold must not degenerate into "always reset").
		{"zero threshold never resets (keep)", 8 * s, 100 * s, 0, 8 * s},
		{"negative threshold never resets (keep)", 4 * s, 200 * s, -1 * s, 4 * s},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resetBackoffIfStable(tc.backoff, tc.uptime, tc.threshold)
			if got != tc.want {
				t.Fatalf("resetBackoffIfStable(backoff=%v, uptime=%v, threshold=%v) = %v, want %v",
					tc.backoff, tc.uptime, tc.threshold, got, tc.want)
			}
		})
	}
}

// dialHarnessConfig builds a raw ConfigService client wired to the harness's
// bufconn listener, reusing the harness token so the stream auth interceptor
// admits the connection. It is used by the wiring test, which needs to inject
// stableResetThreshold BEFORE runSubscribe starts (so it constructs the Client
// by hand rather than through Init, whose goroutine would otherwise read the
// field concurrently and trip -race).
func dialHarnessConfig(t *testing.T, h *testHarness) (configv1.ConfigServiceClient, *grpc.ClientConn) {
	t.Helper()
	conn, err := grpc.NewClient(
		"passthrough:///bufnet-config",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return h.cfgLis.DialContext(ctx)
		}),
		grpc.WithPerRPCCredentials(tokenSource{static: h.token}),
	)
	if err != nil {
		t.Fatalf("dial harness config conn: %v", err)
	}
	return configv1.NewConfigServiceClient(conn), conn
}

// TestRunSubscribe_HealthyDropResetsBackoff is the SG1 wiring test. It proves
// runSubscribe actually resets the backoff after a connection that stayed
// healthy (uptime >= stableResetThreshold) before dropping with a real error —
// the gap a pure-function test cannot close (start-marker placement, the call,
// and the assignment back into backoff).
//
// Determinism / non-flake design:
//   - stableResetThreshold is injected at 20ms; every faked connection is held
//     ~40ms (> threshold) before erroring, so uptime >= threshold ALWAYS holds
//     (the only timing noise — scheduling — can only *lengthen* uptime, which
//     keeps the "stable" classification). No connection is ever misclassified.
//   - With the reset in place every reconnect waits ~1s (backoff reset each
//     drop), so consecutive attach gaps stay ~1s. Without the reset the wait
//     climbs 1s -> 2s -> 4s, so the 2nd->3rd attach gap alone is already ~2s.
//     We wait for 3 attaches (reached by ~2.1s with the fix, ~3.1s without) and
//     assert the 2nd->3rd gap is < 1.5s: comfortably above the ~1s reset
//     cadence yet well below the ~2s a single un-reset doubling would produce.
func TestRunSubscribe_HealthyDropResetsBackoff(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	// Every connection: send the one driven frame, then hold ~40ms (> the 20ms
	// threshold) and drop with a real, non-EOF, non-Canceled error so the loop
	// takes the real-error (backoff) path each time.
	const holdBeforeError = 40 * time.Millisecond
	h.cfgServer.SetSubscribeErrFn(func() error {
		time.Sleep(holdBeforeError)
		return status.Error(codes.Unavailable, "wiring-test healthy drop")
	})

	configCli, conn := dialHarnessConfig(t, h)

	m := newMetrics()
	rootCtx, cancel := context.WithCancel(context.Background())
	cli := &Client{
		metrics:              m,
		cache:                newConfigCache(m),
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		configCli:            configCli,
		subscribedNamespaces: []string{"ns1"},
		rootCtx:              rootCtx,
		rootCancel:           cancel,
		// White-box injection: tiny threshold so every ~40ms connection counts
		// as "healthy". Set BEFORE the goroutine starts => race-free.
		stableResetThreshold: 20 * time.Millisecond,
	}

	// Driver: the fake only consults the error fn after a pushed frame, so for
	// each new Subscribe attach we push exactly one frame (1:1:1 with attaches).
	// We also record the attach time here to measure reconnect cadence.
	var mu sync.Mutex
	var attachTimes []time.Time
	stopDriver := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopDriver:
				return
			case <-h.cfgServer.subscribeWake:
				mu.Lock()
				attachTimes = append(attachTimes, time.Now())
				mu.Unlock()
				select {
				case h.cfgServer.pushCh <- makeSnapshot("ns1", 1, 1, nil):
				case <-stopDriver:
					return
				}
			}
		}
	}()

	cli.wg.Add(1)
	go cli.runSubscribe()

	t.Cleanup(func() {
		cancel()
		cli.wg.Wait()
		close(stopDriver)
		_ = conn.Close()
	})

	// See non-flake rationale above: fix => 3 attaches by ~2.1s; regression =>
	// ~3.1s (still observed, so the gap assertion — not a timeout — reports it).
	if !waitFor(t, 6*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(attachTimes) >= 3
	}) {
		mu.Lock()
		n := len(attachTimes)
		mu.Unlock()
		t.Fatalf("only %d Subscribe attaches within 6s; reconnect cadence too slow — "+
			"backoff was not reset after healthy drops", n)
	}

	mu.Lock()
	times := append([]time.Time(nil), attachTimes...)
	mu.Unlock()

	// Steady-state gap between the 2nd and 3rd attaches must stay in the "reset"
	// band. Under the fix it is ~1s (uptime ~40ms + 1s reset backoff); if the
	// backoff had doubled instead this gap would be ~2s, past the loose bound.
	gap := times[2].Sub(times[1])
	if gap > 1500*time.Millisecond {
		t.Fatalf("2nd->3rd reconnect gap grew to %v (want ~1s reset cadence, <1.5s); "+
			"backoff was not reset after a healthy drop", gap)
	}

	// Sanity: disconnects were recorded (real-error path was exercised).
	if m.SubscribeDisconnectTotal("ns1") == 0 {
		t.Fatal("subscribe_disconnect_total ns1 should be > 0 after repeated drops")
	}
}

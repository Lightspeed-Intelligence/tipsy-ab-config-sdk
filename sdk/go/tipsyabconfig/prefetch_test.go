package tipsyabconfig

// New coverage for the prefetch-decouple design (G1/G2).
//
// These tests exercise the decoupled construction + explicit prefetch contract:
//   - Construction (NewAbtestContext / NewAbtestContextWithTraceID) issues ZERO
//     GetExperimentResult RPCs (G1).
//   - The explicit PrefetchConfigVersionFlatKvForNamespace API fires exactly one
//     RPC for the requested ns, is non-blocking, idempotent, and at-most-once
//     shared with the lazy GetConfig / WaitForAbtest path (G2 + Acceptance 3/5).
//   - Empty / mock ctx and unsubscribed ns short-circuit with no RPC.
//   - Concurrent first-access (mixed prefetch + GetConfig) on the same ns
//     dedups to exactly one RPC.
//
// All RPC counting reuses the bufconn harness (newHarness) + the capture
// transport helper (newClientWithCaptureAbtest) already established by the
// existing test suite.

import (
	"context"
	"sync"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// TestConstruction_ZeroRPC_RightAfterConstruction is the focused construction
// RPC=0 assertion required by Acceptance Criteria 1: capture count is 0
// immediately after construction, before any GetConfig / prefetch. Uses the
// capture transport so the assertion is synchronous (no goroutine race window).
func TestConstruction_ZeroRPC_RightAfterConstruction(t *testing.T) {
	cli, tr := newClientWithCaptureAbtest(t, "ns1") // default ns = ns1, subscribed

	_ = cli.NewAbtestContext(context.Background(), "u1", nil)
	if got := tr.Calls(); got != 0 {
		t.Fatalf("NewAbtestContext must issue 0 RPC at construction; got %d", got)
	}
	_ = cli.NewAbtestContextWithTraceID(context.Background(), "u2", nil, "tid")
	if got := tr.Calls(); got != 0 {
		t.Fatalf("NewAbtestContextWithTraceID must issue 0 RPC at construction; got %d", got)
	}
}

// TestPrefetch_ThenGetConfigReusesSingleRPC asserts the at-most-once invariant
// across the explicit prefetch warm path and the subsequent lazy GetConfig:
// prefetch fires exactly 1 RPC for the ns; a later GetConfig on the same ns
// reuses the memoised result so the total stays 1.
func TestPrefetch_ThenGetConfigReusesSingleRPC(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full", 2: "ab"}}}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cfg.AbtestTimeout = 2 * time.Second
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")
	if !waitFor(t, 2*time.Second, func() bool { return h.abServer.Calls("ns1")-before == 1 }) {
		t.Fatalf("prefetch should fire exactly 1 RPC; got %d", h.abServer.Calls("ns1")-before)
	}

	// Subsequent GetConfig reuses the prefetched result; the abtest version is
	// returned and NO second RPC is issued.
	v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if v != "ab" {
		t.Fatalf("GetConfig after prefetch = %q, want ab", v)
	}
	if got := h.abServer.Calls("ns1") - before; got != 1 {
		t.Fatalf("prefetch + GetConfig on same ns must total 1 RPC (at-most-once); got %d", got)
	}
}

// TestPrefetch_NonBlocking asserts the prefetch API returns promptly even when
// the underlying RPC is slow (it only triggers, never waits). We then drive the
// fetch to completion via GetConfig and confirm the single-RPC invariant holds.
func TestPrefetch_NonBlocking(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full", 2: "ab"}}}))
	// Server holds each RPC for 300ms; a blocking prefetch would stall here.
	h.abServer.SetDelay(300 * time.Millisecond)
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cfg.AbtestTimeout = 2 * time.Second
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)

	start := time.Now()
	abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("prefetch must be non-blocking; returned after %v (RPC delay is 300ms)", elapsed)
	}

	// Driving completion via GetConfig reuses the in-flight prefetch RPC.
	v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if v != "ab" {
		t.Fatalf("GetConfig = %q, want ab", v)
	}
	if got := h.abServer.Calls("ns1") - before; got != 1 {
		t.Fatalf("non-blocking prefetch + GetConfig must total 1 RPC; got %d", got)
	}
}

// TestPrefetch_Idempotent asserts calling the prefetch API twice for the same
// ns still results in exactly one RPC (the second call reuses the existing
// computeStatus slot).
func TestPrefetch_Idempotent(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	// Small delay so the second prefetch lands while the first RPC is still in
	// flight — the strongest form of the idempotency check.
	h.abServer.SetDelay(120 * time.Millisecond)
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cfg.AbtestTimeout = 2 * time.Second
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")
	abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")

	// Wait for the (single) RPC to complete, then assert exactly one fired.
	if _, err := abctx.WaitForAbtest(context.Background(), "ns1"); err != nil {
		t.Fatalf("WaitForAbtest: %v", err)
	}
	// Give any erroneous second goroutine time to surface.
	time.Sleep(50 * time.Millisecond)
	if got := h.abServer.Calls("ns1") - before; got != 1 {
		t.Fatalf("double prefetch on same ns must total 1 RPC; got %d", got)
	}
}

// TestPrefetch_EmptyCtx_NoRPC asserts that prefetch on an EmptyAbtestContext
// short-circuits without any RPC (identity-less ctx, design Important Details).
func TestPrefetch_EmptyCtx_NoRPC(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.TotalCalls()
	abctx := cli.EmptyAbtestContext()
	abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")
	// Nil receiver must also be a safe no-op.
	var nilCtx *AbtestContext
	nilCtx.PrefetchConfigVersionFlatKvForNamespace("ns1")

	time.Sleep(80 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("empty/nil ctx prefetch must issue NO RPC; got %d", got)
	}
}

// TestPrefetch_UnsubscribedNs_NoRPC asserts that prefetch on a namespace the
// client never subscribed to short-circuits with no RPC.
func TestPrefetch_UnsubscribedNs_NoRPC(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.TotalCalls()
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	abctx.PrefetchConfigVersionFlatKvForNamespace("ns-not-subscribed")

	time.Sleep(80 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("unsubscribed-ns prefetch must issue NO RPC; got %d", got)
	}
}

// TestPrefetch_CarriesContextTraceID asserts the prefetch RPC carries the ctx's
// trace_id on the outbound proto (capture harness records the request). This is
// the white-box twin of the bufconn-driven TestPrefetchAPI_UsesContextTraceID.
func TestPrefetch_CarriesContextTraceID(t *testing.T) {
	cli, tr := newClientWithCaptureAbtest(t, "ns1")

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u1", nil, "warm-trace")
	abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")
	if !waitFor(t, 2*time.Second, func() bool { return tr.Calls() >= 1 }) {
		t.Fatalf("prefetch RPC never observed; calls=%d", tr.Calls())
	}
	req := tr.LastRequest()
	if got := req.GetTraceId(); got != "warm-trace" {
		t.Fatalf("prefetch proto TraceId = %q, want %q", got, "warm-trace")
	}
	if req.GetExperimentType() != abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION {
		t.Fatalf("prefetch type = %v, want CONFIG_VERSION", req.GetExperimentType())
	}
	if req.GetDisplayType() != abtestv1.ResultDisplayType_RESULT_DISPLAY_TYPE_FLAT_KV {
		t.Fatalf("prefetch display = %v, want FLAT_KV", req.GetDisplayType())
	}
}

// TestPrefetchAndGetConfig_ConcurrentMixedAtMostOnce races N goroutines that
// mix explicit prefetch and GetConfig first-access on the SAME ns. The shared
// ensureFetch dedup primitive must collapse them to exactly ONE RPC.
func TestPrefetchAndGetConfig_ConcurrentMixedAtMostOnce(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full", 2: "ab"}}}))
	// Latency so the racers genuinely overlap in flight.
	h.abServer.SetDelay(100 * time.Millisecond)
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cfg.AbtestTimeout = 2 * time.Second
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)

	const goroutines = 24
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			// Even goroutines warm via prefetch; odd ones drive via GetConfig.
			// Both must funnel through the same per-ns computeStatus.
			if i%2 == 0 {
				abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")
				return
			}
			if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil {
				t.Errorf("goroutine %d GetConfig: %v", i, err)
			} else if v != "ab" {
				t.Errorf("goroutine %d GetConfig = %q, want ab", i, v)
			}
		}(i)
	}
	wg.Wait()

	// Ensure any in-flight prefetch-only RPC has been counted before asserting.
	if _, err := abctx.WaitForAbtest(context.Background(), "ns1"); err != nil {
		t.Fatalf("WaitForAbtest drain: %v", err)
	}
	if got := h.abServer.Calls("ns1") - before; got != 1 {
		t.Fatalf("expected exactly 1 RPC across %d racing prefetch+GetConfig goroutines, got %d", goroutines, got)
	}
}

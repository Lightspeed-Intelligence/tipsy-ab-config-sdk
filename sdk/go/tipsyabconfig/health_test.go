package tipsyabconfig

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recordingErrSink collects BackgroundErrorEvents from OnBackgroundError. It is
// safe for concurrent use (the callback runs on SDK background goroutines).
type recordingErrSink struct {
	mu     sync.Mutex
	events []BackgroundErrorEvent
}

func (r *recordingErrSink) cb(ev BackgroundErrorEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingErrSink) byPhase(phase string) []BackgroundErrorEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []BackgroundErrorEvent
	for _, ev := range r.events {
		if ev.Phase == phase {
			out = append(out, ev)
		}
	}
	return out
}

func (r *recordingErrSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// TestHealth_StartupFailOpen_ReturnsUsableClient covers design 05 §R4a / AC:
// dial succeeds, the startup PullAll RPC fails, StartupFailOpen=true ⇒ Init
// returns a non-nil client with err==nil and Health().StartupCacheEmpty==true.
func TestHealth_StartupFailOpen_ReturnsUsableClient(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullError(status.Error(codes.Unavailable, "startup boom"))

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.StartupFailOpen = true
	// Stop the periodic pull from firing inside the test window so the
	// startup_pull assertion below is not polluted by periodic_pull events.
	cfg.PullInterval = time.Hour

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init returned err under StartupFailOpen=true: %v", err)
	}
	if cli == nil {
		t.Fatal("Init returned nil client under fail-open")
	}
	defer cli.Close()

	if !cli.Health().StartupCacheEmpty {
		t.Fatal("Health().StartupCacheEmpty = false; want true after absorbed startup failure")
	}
}

// TestHealth_StartupFailClose_ReturnsErr is the companion negative case (kept
// alongside the existing TestPullAll_AuthFailure / TestPullAll_FailClose):
// StartupFailOpen=false ⇒ Init returns ErrStartupPullFailed and no usable
// client. We assert the Health/StartupCacheEmpty path is NOT reached.
func TestHealth_StartupFailClose_ReturnsErr(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullError(status.Error(codes.Unavailable, "startup boom"))

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.StartupFailOpen = false

	cli, err := Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected ErrStartupPullFailed under fail-close")
	}
	if cli != nil {
		t.Fatalf("expected nil client under fail-close, got %v", cli)
	}
}

// TestHealth_OnBackgroundError_StartupPull asserts the absorbed startup failure
// fires OnBackgroundError exactly once with Phase=="startup_pull" and an empty
// Namespace (the aggregate startup fire).
func TestHealth_OnBackgroundError_StartupPull(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullError(status.Error(codes.Unavailable, "startup boom"))

	sink := &recordingErrSink{}
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.StartupFailOpen = true
	cfg.PullInterval = time.Hour // suppress periodic_pull noise during the test
	cfg.OnBackgroundError = sink.cb

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	startups := sink.byPhase("startup_pull")
	if len(startups) != 1 {
		t.Fatalf("startup_pull fires = %d, want exactly 1 (all events=%d)", len(startups), sink.count())
	}
	ev := startups[0]
	if ev.Namespace != "" {
		t.Errorf("startup_pull Namespace=%q, want \"\" (aggregate)", ev.Namespace)
	}
	if ev.Err == nil {
		t.Error("startup_pull event Err is nil; want the absorbed error")
	}
	if ev.Time.IsZero() {
		t.Error("startup_pull event Time is zero")
	}
}

// TestHealth_OnBackgroundError_PanicSafe asserts a callback that panics does
// NOT crash the SDK: Init still returns a usable client (the startup fire is
// synchronous on the Init goroutine, so an unrecovered panic would propagate
// here), Health() still reflects state, and the panicking callback keeps being
// invoked for the periodic loop without killing the background goroutine.
//
// -race friendly: the callback only touches an atomic counter.
func TestHealth_OnBackgroundError_PanicSafe(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullError(status.Error(codes.Unavailable, "boom"))

	var calls atomic.Int64
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.StartupFailOpen = true
	cfg.PullInterval = 20 * time.Millisecond // let the periodic loop fire too
	cfg.OnBackgroundError = func(ev BackgroundErrorEvent) {
		calls.Add(1)
		panic("consumer callback panics on purpose")
	}

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init returned err despite panicking callback (recover should absorb): %v", err)
	}
	defer cli.Close()

	// Startup fire already happened synchronously; the client must be usable.
	if !cli.Health().StartupCacheEmpty {
		t.Fatal("Health().StartupCacheEmpty=false after absorbed startup failure")
	}

	// The background pull loop must keep firing the (panicking) callback
	// without dying — observe at least 2 more invocations.
	if !waitFor(t, 2*time.Second, func() bool { return calls.Load() >= 3 }) {
		t.Fatalf("expected background loop to survive panics and keep firing; got %d calls", calls.Load())
	}
}

// TestHealth_PeriodicPull_FiresWithNamespace drives a short PullInterval and a
// failing server so the runPullLoop's pullOnce fails, asserting a
// Phase=="periodic_pull" event carrying the namespace and that Health
// .LastPullErr is populated.
func TestHealth_PeriodicPull_FiresWithNamespace(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullError(status.Error(codes.Unavailable, "periodic boom"))

	sink := &recordingErrSink{}
	cfg := h.baseConfigNoAbtest([]string{"nsX"})
	cfg.StartupFailOpen = true               // absorb the startup failure
	cfg.PullInterval = 15 * time.Millisecond // fast periodic loop
	cfg.OnBackgroundError = sink.cb

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if !waitFor(t, 2*time.Second, func() bool { return len(sink.byPhase("periodic_pull")) >= 1 }) {
		t.Fatalf("no periodic_pull event observed; events=%d", sink.count())
	}
	ev := sink.byPhase("periodic_pull")[0]
	if ev.Namespace != "nsX" {
		t.Errorf("periodic_pull Namespace=%q want nsX", ev.Namespace)
	}
	if ev.Err == nil {
		t.Error("periodic_pull Err is nil")
	}
	// Health snapshot must reflect the last pull error.
	if !waitFor(t, time.Second, func() bool { return cli.Health().LastPullErr != nil }) {
		t.Fatal("Health().LastPullErr never populated")
	}
	if cli.Health().LastPullErrTime.IsZero() {
		t.Error("Health().LastPullErrTime is zero despite a recorded pull error")
	}
}

// TestHealth_SubscribeConnected_TrueAfterHealthyStart asserts SubscribeConnected
// starts false (zero value) and flips true once the Subscribe stream opens.
func TestHealth_SubscribeConnected_TrueAfterHealthyStart(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if !waitFor(t, 2*time.Second, func() bool { return cli.Health().SubscribeConnected }) {
		t.Fatalf("SubscribeConnected never became true after a healthy start (subscribeCalls=%d)", h.cfgServer.SubscribeCalls())
	}
}

// TestHealth_SubscribeConnected_FalseAfterStreamError induces a stream error
// (server returns an error after the first frame), then asserts the SDK records
// a Phase=="subscribe" event, flips SubscribeConnected back to false, and
// reconnects (flipping it true again). The harness supports this via
// SetSubscribeErrFn + PushSnapshot, the same mechanism TestSubscribe_*Reconnect
// uses.
func TestHealth_SubscribeConnected_FalseAfterStreamError(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	// Error exactly once (on the first stream) so the SDK reconnects cleanly.
	var fired atomic.Bool
	h.cfgServer.SetSubscribeErrFn(func() error {
		if fired.CompareAndSwap(false, true) {
			return status.Error(codes.Unavailable, "kicked")
		}
		return nil
	})

	sink := &recordingErrSink{}
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.PullInterval = time.Hour // isolate: no periodic_pull noise
	cfg.OnBackgroundError = sink.cb

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	// First stream opens → connected true.
	if !waitFor(t, 2*time.Second, func() bool { return cli.Health().SubscribeConnected }) {
		t.Fatal("SubscribeConnected never became true on first stream")
	}
	// Push a frame to trigger the one-shot error path.
	h.cfgServer.PushSnapshot(makeSnapshot("ns1", 2, 2, nil))

	// A subscribe error must be recorded and the callback fired.
	if !waitFor(t, 3*time.Second, func() bool { return len(sink.byPhase("subscribe")) >= 1 }) {
		t.Fatalf("no subscribe error event observed; events=%d", sink.count())
	}
	if cli.Health().LastSubscribeErr == nil {
		t.Error("Health().LastSubscribeErr is nil after a stream error")
	}
	if cli.Health().LastSubscribeErrTime.IsZero() {
		t.Error("Health().LastSubscribeErrTime is zero after a stream error")
	}
	subEv := sink.byPhase("subscribe")[0]
	if subEv.Err == nil {
		t.Error("subscribe event Err is nil")
	}
	// After backoff the SDK reconnects; the second stream (errFn now returns
	// nil) re-opens and SubscribeConnected goes true again.
	if !waitFor(t, 5*time.Second, func() bool {
		return h.cfgServer.SubscribeCalls() >= 2 && cli.Health().SubscribeConnected
	}) {
		t.Fatalf("expected reconnect + SubscribeConnected=true again; subscribeCalls=%d connected=%v",
			h.cfgServer.SubscribeCalls(), cli.Health().SubscribeConnected)
	}
}

// TestHealth_ConcurrentReads exercises concurrent Health() reads while the
// background pull + subscribe loops run and mutate health state, so
// `go test -race` has something meaningful to catch. The failing server keeps
// the pull loop busy writing LastPullErr while readers poll concurrently.
func TestHealth_ConcurrentReads(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullError(status.Error(codes.Unavailable, "boom"))

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.StartupFailOpen = true
	cfg.PullInterval = 5 * time.Millisecond // hammer the writer side

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					_ = cli.Health() // concurrent read under -race
				}
			}
		}()
	}
	wg.Wait()
}

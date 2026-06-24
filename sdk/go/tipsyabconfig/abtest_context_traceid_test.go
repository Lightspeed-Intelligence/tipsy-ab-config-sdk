package tipsyabconfig

// SubTask B test file (sdk-trace-id design §4 + Testing Plan §SDK Go bullet 5).
//
// Covers AbtestContext.traceID semantics and propagation onto every outbound
// GetExperimentResultRequest. Construction no longer pre-fetches (G1), so the
// trace_id-propagation intent is now asserted via the explicit prefetch API
// (PrefetchConfigVersionFlatKvForNamespace) and the lazy WaitForAbtest path.
//
// Design contract under test:
//   - NewAbtestContextWithTraceID(ctx, uid, attrs, "explicit-id")
//        ⇒ AbtestContext.traceID == "explicit-id"
//   - NewAbtestContextWithTraceID(ctx, uid, attrs, "")
//        ⇒ AbtestContext.traceID == fresh UUID v4 (NOT empty)
//   - Old NewAbtestContext(ctx, uid, attrs) (backward compat)
//        ⇒ AbtestContext.traceID == fresh UUID v4 (NOT empty)
//   - Explicit PrefetchConfigVersionFlatKvForNamespace ⇒ outbound proto
//     TraceId == a.traceID
//   - Lazy WaitForAbtest         ⇒ outbound proto TraceId == a.traceID
//   - Concurrent first-access dedup ⇒ exactly ONE RPC fires AND its TraceId
//     matches the AbtestContext's traceID.
//
// White-box note: this _test.go file lives in `package tipsyabconfig` so it
// can read the unexported `AbtestContext.traceID` field directly (test-only
// exposure). No production-side getter is required.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// newClientWithCaptureAbtestAndPrefetch builds a *Client wired to a capture
// transport AND a configured default+subscribed namespace so the explicit
// prefetch API (PrefetchConfigVersionFlatKvForNamespace) has a real subscribed
// ns to warm. The cache stays empty (no pull). Construction itself never fires
// an RPC; the returned transport only records calls once prefetch / GetConfig
// drives them.
func newClientWithCaptureAbtestAndPrefetch(t *testing.T, ns string) (*Client, *captureAbtestTransport) {
	t.Helper()
	cli, tr := newClientWithCaptureAbtest(t, ns)
	// defaultNamespace + defaultNsSubscribed already set by the base helper.
	return cli, tr
}

func TestNewAbtestContextWithTraceID_ExplicitIDStored(t *testing.T) {
	cli, _ := newClientWithCaptureAbtest(t, "ns1")
	// This test is purely about constructor field assignment; construction
	// never fires an RPC regardless of the default ns.
	cli.defaultNamespace = ""
	cli.defaultNsSubscribed = false

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u1", nil, "explicit-id")
	if abctx == nil {
		t.Fatal("nil AbtestContext")
	}
	if abctx.traceID != "explicit-id" {
		t.Fatalf("traceID = %q, want %q", abctx.traceID, "explicit-id")
	}
}

func TestNewAbtestContextWithTraceID_EmptyGeneratesUUID(t *testing.T) {
	cli, _ := newClientWithCaptureAbtest(t, "ns1")
	cli.defaultNamespace = ""
	cli.defaultNsSubscribed = false

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u1", nil, "")
	if abctx == nil {
		t.Fatal("nil AbtestContext")
	}
	if abctx.traceID == "" {
		t.Fatal("expected SDK-generated traceID when caller passed empty string")
	}
	if !uuidV4Pattern.MatchString(abctx.traceID) {
		t.Fatalf("expected UUID-shaped traceID, got %q", abctx.traceID)
	}
}

func TestNewAbtestContext_BackwardCompat_AutoGeneratesTraceID(t *testing.T) {
	cli, _ := newClientWithCaptureAbtest(t, "ns1")
	cli.defaultNamespace = ""
	cli.defaultNsSubscribed = false

	// Old API path: callers that do not know about trace_id still get a
	// non-empty SDK-generated UUID.
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	if abctx == nil {
		t.Fatal("nil AbtestContext")
	}
	if abctx.traceID == "" {
		t.Fatal("expected SDK-generated traceID on legacy NewAbtestContext path")
	}
	if !uuidV4Pattern.MatchString(abctx.traceID) {
		t.Fatalf("expected UUID-shaped traceID, got %q", abctx.traceID)
	}
}

// TestPrefetchAPI_UsesContextTraceID is the converted form of the old
// TestNewAbtestContext_EagerPrefetchUsesContextTraceID. The trace_id-propagation
// intent (the RPC warmed for a ctx carries that ctx's trace_id) is preserved,
// but the trigger moves from the deleted construction-time eager prefetch to
// the explicit PrefetchConfigVersionFlatKvForNamespace API.
func TestPrefetchAPI_UsesContextTraceID(t *testing.T) {
	cli, tr := newClientWithCaptureAbtestAndPrefetch(t, "ns1")

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u1", nil, "prefetch-id")
	if abctx == nil {
		t.Fatal("nil AbtestContext")
	}
	if abctx.traceID != "prefetch-id" {
		t.Fatalf("traceID = %q, want %q", abctx.traceID, "prefetch-id")
	}
	// Construction is pure-create: no RPC yet.
	if got := tr.Calls(); got != 0 {
		t.Fatalf("construction must issue no RPC; got %d", got)
	}
	// Explicit prefetch fires exactly one RPC for the subscribed ns. Wait for
	// the capture to register the request before asserting the proto field.
	abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")
	if !waitFor(t, 2*time.Second, func() bool { return tr.Calls() >= 1 }) {
		t.Fatalf("explicit prefetch RPC never observed; calls=%d", tr.Calls())
	}
	req := tr.LastRequest()
	if got := req.GetTraceId(); got != "prefetch-id" {
		t.Fatalf("prefetch proto TraceId = %q, want %q", got, "prefetch-id")
	}
	if req.GetNamespace() != "ns1" {
		t.Fatalf("prefetch targeted wrong ns: %q", req.GetNamespace())
	}
}

func TestAbtestContext_LazyWaitForAbtestUsesContextTraceID(t *testing.T) {
	cli, tr := newClientWithCaptureAbtest(t, "ns1")
	// Zero the default ns (no functional effect now that construction never
	// pre-fetches; kept for parity with the field-assignment tests).
	cli.defaultNamespace = ""
	cli.defaultNsSubscribed = false

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u1", nil, "lazy-id")
	if tr.Calls() != 0 {
		t.Fatalf("expected no RPC at construction; got %d", tr.Calls())
	}

	// Lazy entry: WaitForAbtest on a subscribed-but-not-prefetched ns must
	// fire exactly one RPC and carry the AbtestContext's traceID on the
	// outbound proto.
	if _, err := abctx.WaitForAbtest(context.Background(), "ns1"); err != nil {
		t.Fatalf("WaitForAbtest: %v", err)
	}
	if tr.Calls() != 1 {
		t.Fatalf("expected exactly 1 RPC from lazy fetch, got %d", tr.Calls())
	}
	req := tr.LastRequest()
	if got := req.GetTraceId(); got != "lazy-id" {
		t.Fatalf("lazy fetch proto TraceId = %q, want %q", got, "lazy-id")
	}
}

func TestAbtestContext_ConcurrentWaitDedup_OneRPCSharesTraceID(t *testing.T) {
	cli, tr := newClientWithCaptureAbtest(t, "ns1")
	cli.defaultNamespace = ""
	cli.defaultNsSubscribed = false

	// Block the capture so the first RPC stays in flight long enough for
	// every other goroutine to enter resultFor and find an open
	// computeStatus.done. We synthesise the gate by wrapping a slow inner
	// transport.
	gate := make(chan struct{})
	slow := &gatedAbtestTransport{inner: tr, gate: gate}
	cli.abtestTr = slow

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u1", nil, "concurrent-id")

	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = abctx.WaitForAbtest(context.Background(), "ns1")
		}()
	}

	// Give every goroutine time to land inside resultFor and block on the
	// SAME computeStatus.done before we release the gate.
	if !waitFor(t, 2*time.Second, func() bool { return slow.Inflight() >= 1 }) {
		t.Fatal("no in-flight RPC observed")
	}
	// Wait briefly so racing followers join the dedup wait.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := tr.Calls(); got != 1 {
		t.Fatalf("expected exactly 1 RPC (concurrent dedup), got %d", got)
	}
	req := tr.LastRequest()
	if got := req.GetTraceId(); got != "concurrent-id" {
		t.Fatalf("dedup-shared proto TraceId = %q, want %q", got, "concurrent-id")
	}
}

// gatedAbtestTransport delays GetExperimentResult until `gate` is closed,
// while forwarding to the inner captureAbtestTransport. It also tracks how
// many RPCs are currently in flight (post-block, pre-release) so the
// concurrent-dedup test can synchronise without sleeping blindly.
type gatedAbtestTransport struct {
	inner    *captureAbtestTransport
	gate     chan struct{}
	inflight atomic.Int64
}

func (g *gatedAbtestTransport) GetExperimentResult(ctx context.Context, req *abtestv1.GetExperimentResultRequest) (*abtestv1.GetExperimentResultResponse, error) {
	g.inflight.Add(1)
	defer g.inflight.Add(-1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.inner.GetExperimentResult(ctx, req)
}

func (g *gatedAbtestTransport) Inflight() int64 { return g.inflight.Load() }

package tipsyabconfig

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// TestGetConfigDefault_ErrNamespaceRequired verifies that the ns-optional
// dynamic getConfig surface returns ErrNamespaceRequired when no explicit ns
// is supplied AND no project default namespace is configured (decision A-3 /
// design 04 §B.1).
func TestGetConfigDefault_ErrNamespaceRequired(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full"}}}))
	cfg := h.baseConfig([]string{"ns1"}) // no DefaultNamespace, env unset
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	_, err = cli.GetConfigDefault(context.Background(), abctx, "k", "def")
	if !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("expected ErrNamespaceRequired, got %v", err)
	}
	// Explicit-ns GetConfig with empty ns also errors.
	_, err = cli.GetConfig(context.Background(), abctx, "", "k", "def")
	if !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("expected ErrNamespaceRequired on empty ns GetConfig, got %v", err)
	}
}

// TestDefaultNamespace_FromConfig verifies that the configured default
// namespace drives ns-optional getConfig. Construction issues no RPC; the
// default ns is fetched lazily on first GetConfigDefault.
func TestDefaultNamespace_FromConfig(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "ab-v2"}}}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cfg.DefaultNamespace = "ns1"
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if cli.DefaultNamespace() != "ns1" {
		t.Fatalf("DefaultNamespace() = %q, want ns1", cli.DefaultNamespace())
	}

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	// Construction issues no RPC; GetConfigDefault lazily fetches the default ns
	// and resolves to the experiment value.
	val, err := cli.GetConfigDefault(context.Background(), abctx, "k", "def")
	if err != nil {
		t.Fatalf("GetConfigDefault: %v", err)
	}
	if val != "ab-v2" {
		t.Fatalf("expected ab value via default ns, got %q", val)
	}
}

// TestNewAbtestContext_ConstructionIssuesNoRPC is the converted form of the old
// TestNewAbtestContext_EagerPrefetchShape. The previous contract asserted that
// constructing an AbtestContext eagerly pre-fetched the default ns with
// type=config_version + display=flat_kv. Under the decoupled design (G1)
// construction is pure-create: it issues ZERO GetExperimentResult RPCs, for the
// default ns or any other ns. The config_version + flat_kv shape assertion that
// used to live here now lives in TestPrefetchAPI_Shape (the explicit prefetch
// API is the only request-shape carrier left).
func TestNewAbtestContext_ConstructionIssuesNoRPC(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1", "ns2"})
	cfg.DefaultNamespace = "ns1"
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.TotalCalls()
	_ = cli.NewAbtestContext(context.Background(), "u1", nil)
	_ = cli.NewAbtestContextWithTraceID(context.Background(), "u2", nil, "tid")
	// Give any (erroneous) construction-time goroutine a moment to fire so a
	// regression that re-introduces eager prefetch would be caught.
	time.Sleep(100 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("construction must issue NO GetExperimentResult RPC; got %d (default ns1, ns2)", got)
	}
	if got := h.abServer.Calls("ns1"); got != 0 {
		t.Fatalf("default ns1 must NOT be eagerly pre-fetched at construction; got %d", got)
	}
	if got := h.abServer.Calls("ns2"); got != 0 {
		t.Fatalf("ns2 must NOT be requested at construction; got %d", got)
	}
}

// TestPrefetchAPI_Shape replaces the request-shape half of the old
// EagerPrefetchShape test: the explicit prefetch API targets exactly the
// requested ns with type=config_version + display=flat_kv, and only that ns.
func TestPrefetchAPI_Shape(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1", "ns2"})
	cfg.DefaultNamespace = "ns1"
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.TotalCalls()
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")
	if !waitFor(t, 2*time.Second, func() bool { return h.abServer.Calls("ns1") > 0 }) {
		t.Fatal("expected explicit prefetch RPC for ns1")
	}
	// Prefetch warms only the requested ns; ns2 must NOT be requested.
	if got := h.abServer.Calls("ns2"); got != 0 {
		t.Fatalf("prefetch must NOT fan out to ns2; got %d calls", got)
	}
	if got := h.abServer.TotalCalls() - before; got != 1 {
		t.Fatalf("expected exactly 1 prefetch RPC, got %d", got)
	}
	req := h.abServer.LastRequest()
	if req.GetNamespace() != "ns1" {
		t.Fatalf("prefetch ns = %q, want ns1", req.GetNamespace())
	}
	if req.GetExperimentType() != abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION {
		t.Fatalf("prefetch type = %v, want CONFIG_VERSION", req.GetExperimentType())
	}
	if req.GetDisplayType() != abtestv1.ResultDisplayType_RESULT_DISPLAY_TYPE_FLAT_KV {
		t.Fatalf("prefetch display = %v, want FLAT_KV", req.GetDisplayType())
	}
}

// TestNewAbtestContext_NoDefaultConstructionNoRPC is the converted form of the
// old TestNewAbtestContext_NoDefaultNoEagerRPC. With no default ns the old
// constructor skipped its eager pre-request; under the new design construction
// never fires an RPC regardless of whether a default ns is configured, so the
// no-default case is just one instance of the universal "construction RPC=0"
// contract.
func TestNewAbtestContext_NoDefaultConstructionNoRPC(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"}) // no default ns
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.TotalCalls()
	_ = cli.NewAbtestContext(context.Background(), "u1", nil)
	// Give any (erroneous) construction goroutine a moment.
	time.Sleep(50 * time.Millisecond)
	if got := h.abServer.TotalCalls() - before; got != 0 {
		t.Fatalf("construction must fire no RPC (no default ns); got %d", got)
	}
}

// TestResultFor_ConcurrentAtMostOnce is the feedback-point-3 concurrency test:
// multiple goroutines racing on the SAME not-yet-fetched ns must result in
// exactly ONE GetExperimentResult RPC (the rest wait on the shared done
// channel).
func TestResultFor_ConcurrentAtMostOnce(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full", 2: "ab"}}}))
	// Add latency so concurrent first-accessors genuinely race in-flight.
	h.abServer.SetDelay(80 * time.Millisecond)
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
	// Construction fires no RPC; the lazy GetConfig path is exercised below.
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)

	const goroutines = 16
	var wg sync.WaitGroup
	vals := make([]string, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
			if err != nil {
				t.Errorf("goroutine %d GetConfig: %v", i, err)
				return
			}
			vals[i] = v
		}(i)
	}
	wg.Wait()

	if got := h.abServer.Calls("ns1") - before; got != 1 {
		t.Fatalf("expected exactly 1 RPC across %d racing goroutines, got %d", goroutines, got)
	}
	for i, v := range vals {
		if v != "ab" {
			t.Fatalf("goroutine %d got %q, want ab", i, v)
		}
	}
}

// TestGetConfig_FullFallbackPreservedForUnhitKey verifies M6: a key NOT present
// in config_flat_kv resolves to the full-release version (not the default),
// even though the abtest result was fetched for the ns.
func TestGetConfig_FullFallbackPreservedForUnhitKey(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{
		"hit":   {full: 1, versions: map[int64]string{1: "full-hit", 2: "ab-hit"}},
		"unhit": {full: 5, versions: map[int64]string{5: "full-unhit"}},
	}))
	// Experiment only hits "hit"; "unhit" is absent from config_flat_kv.
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"hit": 2},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	// hit -> ab version
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "hit", "def"); err != nil || v != "ab-hit" {
		t.Fatalf("hit: got (%q, %v), want ab-hit", v, err)
	}
	// unhit -> full release (NOT default), reusing the same memoised result.
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "unhit", "def"); err != nil || v != "full-unhit" {
		t.Fatalf("unhit: got (%q, %v), want full-unhit", v, err)
	}
	// missing key -> default.
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "missing", "def"); err != nil || v != "def" {
		t.Fatalf("missing: got (%q, %v), want def", v, err)
	}
}

// TestUserInfo_Accessor verifies the UserInfo() accessor carries uid + attrs.
func TestUserInfo_Accessor(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	attrs := map[string]any{"country": "US", "tier": 3}
	abctx := cli.NewAbtestContext(context.Background(), "u42", attrs)
	ui := abctx.UserInfo()
	if ui.UID != "u42" {
		t.Fatalf("UserInfo().UID = %q, want u42", ui.UID)
	}
	if ui.Attrs["country"] != "US" || ui.Attrs["tier"] != 3 {
		t.Fatalf("UserInfo().Attrs = %+v, want country=US tier=3", ui.Attrs)
	}
	// UserID() must still work for back-compat.
	if abctx.UserID() != "u42" {
		t.Fatalf("UserID() = %q, want u42", abctx.UserID())
	}
	// Nil receiver yields zero value, not a panic.
	var nilCtx *AbtestContext
	if got := nilCtx.UserInfo(); got.UID != "" || got.Attrs != nil {
		t.Fatalf("nil UserInfo() = %+v, want zero", got)
	}
}

// TestGetExperimentResult_Client verifies the thin GetExperimentResult client
// passes all params through and returns the raw response (design 04 §B.6).
func TestGetExperimentResult_Client(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 7},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	resp, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace:   "ns1",
		UserInfo:    UserInfo{UID: "u1", Attrs: map[string]any{"country": "US"}},
		LayerIds:    []string{"L1", "L2"},
		Type:        ExperimentTypeCustomParams,
		DisplayType: ResultDisplayEachExperimentGroup,
	})
	if err != nil {
		t.Fatalf("GetExperimentResult: %v", err)
	}
	if resp.GetConfigFlatKv()["k"] != 7 {
		t.Fatalf("unexpected response: %+v", resp.GetConfigFlatKv())
	}
	req := h.abServer.LastRequest()
	if req.GetNamespace() != "ns1" || req.GetUserId() != "u1" {
		t.Fatalf("ns/uid mismatch: %+v", req)
	}
	if len(req.GetLayerIds()) != 2 {
		t.Fatalf("layer ids not forwarded: %+v", req.GetLayerIds())
	}
	if req.GetExperimentType() != abtestv1.ExperimentType_EXPERIMENT_TYPE_CUSTOM_PARAMS {
		t.Fatalf("type not forwarded: %v", req.GetExperimentType())
	}
	if req.GetDisplayType() != abtestv1.ResultDisplayType_RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP {
		t.Fatalf("display not forwarded: %v", req.GetDisplayType())
	}
	if req.GetUserAttrs()["country"].GetS() != "US" {
		t.Fatalf("user attrs not forwarded: %+v", req.GetUserAttrs())
	}

	// ns-optional + no default ⇒ ErrNamespaceRequired.
	_, err = cli.GetExperimentResult(context.Background(), ExperimentResultRequest{})
	if !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("expected ErrNamespaceRequired, got %v", err)
	}
}

// TestResolveNamespace_NotSubscribed verifies a resolved-but-unsubscribed ns
// is rejected (design 04 §B.1 validation).
func TestResolveNamespace_NotSubscribed(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	_, err = cli.GetConfig(context.Background(), abctx, "ns-not-subscribed", "k", "def")
	if !errors.Is(err, ErrNamespaceNotSubscribed) {
		t.Fatalf("expected ErrNamespaceNotSubscribed, got %v", err)
	}
}

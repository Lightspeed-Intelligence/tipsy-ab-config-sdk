package tipsyabconfig

// Tests for GetAllConfigs / GetAllConfigsDefault and the uid ""/"0" no-user
// short-circuit (design .zyz-worker/tasks/abconfig-sdk-get-all-configs/design.md
// §1/§2, semantics table, Testing Plan items 1-9).
//
// All cases are pure in-process: the fake config server (cfgServer) seeds an
// immutable NamespaceSnapshot via makeSnapshot, and the fake abtest server
// (abServer) both arms the config_flat_kv response AND counts every
// GetExperimentResult RPC (Calls / TotalCalls). RPC-count deltas are the proof
// mechanism for "zero RPC" / "at-most-once" assertions.
//
// These tests are written strictly against the public API contract:
//   func (c *Client) GetAllConfigs(ctx, abctx, ns) (map[string]string, error)
//   func (c *Client) GetAllConfigsDefault(ctx, abctx) (map[string]string, error)
// plus the existing harness helpers (newHarness, makeSnapshot, setHDR,
// baseConfig, typedKey). They must NOT reach into implementation internals.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// initClient runs Init and registers Close cleanup, trimming boilerplate that
// the per-file tests would otherwise repeat.
func initClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// ---------------------------------------------------------------------------
// Testing Plan 1: resolution matrix in a single namespace.
// ---------------------------------------------------------------------------

// TestGetAllConfigs_ResolutionMatrix covers every branch of the per-key
// resolver in one GetAllConfigs call, asserting the semantics table:
//   - abhit    : abtest hit + cache has the ab version      -> ab value
//   - abmiss   : abtest hit but ab version absent in cache   -> WARN + fallback
//     metric tick + full-release value
//   - fullonly : no abtest hit, has full release             -> full value
//   - neither  : no abtest hit, no full release              -> ABSENT from map
//   - emptyval : full release value is ""                    -> present with ""
func TestGetAllConfigs_ResolutionMatrix(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"abhit":    {full: 1, versions: map[int64]string{1: "full-abhit", 2: "ab-abhit"}},
		"abmiss":   {full: 1, versions: map[int64]string{1: "full-abmiss"}},
		"fullonly": {full: 5, versions: map[int64]string{5: "full-only"}},
		"neither":  {full: 0, versions: map[int64]string{}},
		"emptyval": {full: 7, versions: map[int64]string{7: ""}},
	}))
	// abhit resolves to a cached version; abmiss points at version 99 which is
	// NOT in the cache (forces the ab->full fallback + metric).
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"abhit": 2, "abmiss": 99},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs: %v", err)
	}
	want := map[string]string{
		"abhit":    "ab-abhit",
		"abmiss":   "full-abmiss",
		"fullonly": "full-only",
		"emptyval": "", // empty string is a valid value, must be present
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolution matrix map mismatch:\n got=%#v\nwant=%#v", got, want)
	}
	// "neither" (no ab hit, no full release) must be excluded, not "".
	if _, present := got["neither"]; present {
		t.Fatalf("key with neither ab nor full release must be ABSENT from map, got %q", got["neither"])
	}
	// The ab->full fallback for "abmiss" must have ticked the per-ns fallback
	// metric (indirect proof of the WARN branch).
	if cli.Metrics().AbtestFallbackTotal("ns1") == 0 {
		t.Fatal("expected abtest_fallback_total ns1 > 0 from the ab->full fallback (abmiss)")
	}
}

// ---------------------------------------------------------------------------
// Testing Plan 2: GetAllConfigsDefault namespace resolution.
// ---------------------------------------------------------------------------

// TestGetAllConfigsDefault_UsesDefaultNs verifies the ns-optional variant
// resolves the configured project default namespace.
func TestGetAllConfigsDefault_UsesDefaultNs(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1"}},
	}))
	cfg := h.baseConfig([]string{"ns1"})
	cfg.DefaultNamespace = "ns1"
	cli := initClient(t, cfg)

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	got, err := cli.GetAllConfigsDefault(context.Background(), abctx)
	if err != nil {
		t.Fatalf("GetAllConfigsDefault: %v", err)
	}
	if want := map[string]string{"k": "full-v1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("default-ns map mismatch: got=%#v want=%#v", got, want)
	}
}

// TestGetAllConfigsDefault_ErrNamespaceRequired verifies that with no default
// namespace configured (and env unset) the ns-optional variant returns
// ErrNamespaceRequired, matching GetConfigDefault.
func TestGetAllConfigsDefault_ErrNamespaceRequired(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full"}},
	}))
	cli := initClient(t, h.baseConfig([]string{"ns1"})) // no DefaultNamespace

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	_, err := cli.GetAllConfigsDefault(context.Background(), abctx)
	if !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("expected ErrNamespaceRequired, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Testing Plan 3: error paths (mirror getConfig error semantics).
// ---------------------------------------------------------------------------

// TestGetAllConfigs_UnsubscribedNsErr verifies a resolved-but-unsubscribed ns
// yields ErrNamespaceNotSubscribed.
func TestGetAllConfigs_UnsubscribedNsErr(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "v"}},
	}))
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	_, err := cli.GetAllConfigs(context.Background(), abctx, "ns-not-subscribed")
	if !errors.Is(err, ErrNamespaceNotSubscribed) {
		t.Fatalf("expected ErrNamespaceNotSubscribed, got %v", err)
	}
}

// TestGetAllConfigs_NilAbtestContextErr verifies a nil abctx yields
// ErrAbtestContextMissing (same as GetConfig).
func TestGetAllConfigs_NilAbtestContextErr(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "v"}},
	}))
	cli := initClient(t, h.baseConfigNoAbtest([]string{"ns1"}))

	_, err := cli.GetAllConfigs(context.Background(), nil, "ns1")
	if !errors.Is(err, ErrAbtestContextMissing) {
		t.Fatalf("expected ErrAbtestContextMissing, got %v", err)
	}
}

// TestGetAllConfigs_NilClientErr verifies a nil *Client receiver yields
// ErrClosed (matches getConfigResolved's nil-receiver contract).
func TestGetAllConfigs_NilClientErr(t *testing.T) {
	var nilClient *Client
	_, err := nilClient.GetAllConfigs(context.Background(), nil, "ns1")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on nil client, got %v", err)
	}
}

// TestGetAllConfigs_CtxCancelledPropagates verifies that a ctx cancelled while
// the abtest fetch is in flight propagates the ctx error to the caller (same
// as GetConfig; the caller's ctx governs the wait in resultFor).
func TestGetAllConfigs_CtxCancelledPropagates(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full", 2: "ab"}},
	}))
	// Hold the RPC long enough that our cancel lands during the wait.
	h.abServer.SetDelay(500 * time.Millisecond)
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cfg.AbtestTimeout = 2 * time.Second
	cli := initClient(t, cfg)

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := cli.GetAllConfigs(ctx, abctx, "ns1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled propagated, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Testing Plan 4: subscribed but no snapshot yet -> empty non-nil map, no RPC.
// ---------------------------------------------------------------------------

// TestGetAllConfigs_NoSnapshotEmptyMap verifies that a subscribed ns with no
// snapshot yet (StartupFailOpen absorbs the missing pull) returns a non-nil
// empty map, nil error, and issues ZERO GetExperimentResult RPC (design §2
// step 3 / accepted single-key divergence #1).
func TestGetAllConfigs_NoSnapshotEmptyMap(t *testing.T) {
	h := newHarness(t)
	// Deliberately DO NOT SetPullSnapshot for ns1: PullAll returns no snapshot
	// so the cache has none. StartupFailOpen lets Init succeed with an empty
	// cache (the fake returns an empty snapshot list, not an error, so Init
	// succeeds regardless).
	cfg := h.baseConfig([]string{"ns1"})
	cfg.StartupFailOpen = true
	cli := initClient(t, cfg)

	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs on empty cache: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty map, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %#v", got)
	}
	if delta := h.abServer.Calls("ns1") - before; delta != 0 {
		t.Fatalf("no-snapshot GetAllConfigs must issue ZERO RPC, got %d", delta)
	}
}

// ---------------------------------------------------------------------------
// Testing Plan 5: at-most-once RPC across GetConfig + GetAllConfigs.
// ---------------------------------------------------------------------------

// TestGetAllConfigs_AtMostOnceRPCWithGetConfig verifies the memoise invariant:
// the same abctx used for a GetConfig then a GetAllConfigs on the same ns fires
// exactly ONE GetExperimentResult RPC (the second call reuses the memoised
// per-ns result).
func TestGetAllConfigs_AtMostOnceRPCWithGetConfig(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k1": {full: 1, versions: map[int64]string{1: "full1", 2: "ab1"}},
		"k2": {full: 1, versions: map[int64]string{1: "full2", 2: "ab2"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k1": 2, "k2": 2},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)

	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k1", "def"); err != nil || v != "ab1" {
		t.Fatalf("GetConfig k1: got (%q,%v), want ab1", v, err)
	}
	got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs: %v", err)
	}
	if want := map[string]string{"k1": "ab1", "k2": "ab2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("map mismatch: got=%#v want=%#v", got, want)
	}
	if delta := h.abServer.Calls("ns1") - before; delta != 1 {
		t.Fatalf("GetConfig + GetAllConfigs on same ns must total 1 RPC (at-most-once), got %d", delta)
	}
}

// TestGetAllConfigs_AllFalseHDRZeroRPC verifies the full-ns fast-path: when
// every key's has_dynamic_resolution is explicitly false, GetAllConfigs skips
// the abtest RPC entirely and resolves purely from full-release values.
func TestGetAllConfigs_AllFalseHDRZeroRPC(t *testing.T) {
	h := newHarness(t)
	pb := makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"a": {full: 1, versions: map[int64]string{1: "full-a"}},
		"b": {full: 2, versions: map[int64]string{2: "full-b"}},
	})
	setHDR(t, pb, "a", boolPtr(false))
	setHDR(t, pb, "b", boolPtr(false))
	h.cfgServer.SetPullSnapshot(pb)
	// Arm the abtest server so a wrongful call would be counted (and would
	// change the resolved values, surfacing the regression).
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"a": 1, "b": 2},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs: %v", err)
	}
	if want := map[string]string{"a": "full-a", "b": "full-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("map mismatch: got=%#v want=%#v", got, want)
	}
	if delta := h.abServer.Calls("ns1") - before; delta != 0 {
		t.Fatalf("all-false-HDR GetAllConfigs must issue ZERO RPC, got %d", delta)
	}
}

// ---------------------------------------------------------------------------
// Testing Plan 7: returned map mutability / independence.
// ---------------------------------------------------------------------------

// TestGetAllConfigs_ReturnedMapIsMutableAndIndependent verifies the returned
// map is freshly allocated per call: mutating it (delete + overwrite) must not
// affect the values a subsequent GetAllConfigs returns.
func TestGetAllConfigs_ReturnedMapIsMutableAndIndependent(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"a": {full: 1, versions: map[int64]string{1: "full-a"}},
		"b": {full: 2, versions: map[int64]string{2: "full-b"}},
	}))
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	first, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs (first): %v", err)
	}
	// Mutate the returned map aggressively.
	first["a"] = "tampered"
	delete(first, "b")
	first["injected"] = "x"

	second, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs (second): %v", err)
	}
	want := map[string]string{"a": "full-a", "b": "full-b"}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("second call must be unaffected by mutation of the first: got=%#v want=%#v", second, want)
	}
}

// ---------------------------------------------------------------------------
// Testing Plan 8: mixed fast-path (some keys false, one true/absent).
// ---------------------------------------------------------------------------

// TestGetAllConfigs_MixedFastPathFiresOneRPC verifies that when at least one
// key has has_dynamic_resolution true (or absent) while others are explicitly
// false, GetAllConfigs still fires exactly ONE ns RPC and resolves the ab hit.
func TestGetAllConfigs_MixedFastPathFiresOneRPC(t *testing.T) {
	h := newHarness(t)
	pb := makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"falseKey":  {full: 1, versions: map[int64]string{1: "full-false"}},
		"trueKey":   {full: 1, versions: map[int64]string{1: "full-true", 2: "ab-true"}},
		"absentKey": {full: 3, versions: map[int64]string{3: "full-absent", 4: "ab-absent"}},
	})
	setHDR(t, pb, "falseKey", boolPtr(false))
	setHDR(t, pb, "trueKey", boolPtr(true))
	// absentKey: leave HDR nil (old-server frame) — must keep the abtest path.
	h.cfgServer.SetPullSnapshot(pb)
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"trueKey": 2, "absentKey": 4},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs: %v", err)
	}
	want := map[string]string{
		"falseKey":  "full-false",
		"trueKey":   "ab-true",
		"absentKey": "ab-absent",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed fast-path map mismatch: got=%#v want=%#v", got, want)
	}
	if delta := h.abServer.Calls("ns1") - before; delta != 1 {
		t.Fatalf("mixed fast-path must fire exactly 1 RPC, got %d", delta)
	}
}

// ---------------------------------------------------------------------------
// Testing Plan 9: config_flat_kv version=0 entry.
// ---------------------------------------------------------------------------

// TestGetAllConfigs_FlatKvVersionZeroResolvesFull verifies that a
// config_flat_kv entry with version=0 is treated as "no ab hit" (mirroring the
// single-key non-zero version gate): it must NOT tick the fallback metric and
// must resolve to the full-release value.
func TestGetAllConfigs_FlatKvVersionZeroResolvesFull(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1"}},
	}))
	// version 0 for "k" — not a real ab hit.
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 0},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs: %v", err)
	}
	if want := map[string]string{"k": "full-v1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("version=0 must resolve to full: got=%#v want=%#v", got, want)
	}
	if cli.Metrics().AbtestFallbackTotal("ns1") != 0 {
		t.Fatalf("version=0 must NOT tick the fallback metric, got %d", cli.Metrics().AbtestFallbackTotal("ns1"))
	}
}

// boolPtr returns a *bool for the has_dynamic_resolution optional field. It is
// the get-all-configs-test-file local twin of proto.Bool, kept here so the
// file has no extra import just for a one-liner.
func boolPtr(b bool) *bool { return &b }

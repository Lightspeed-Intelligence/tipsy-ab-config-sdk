package tipsyabconfig

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

func TestGetConfigStatic_HitAndMiss(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{
		"hit":   {full: 10, versions: map[int64]string{10: "value10"}},
		"empty": {full: 11, versions: map[int64]string{11: ""}},
	}))
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if v, ok := cli.GetConfigStatic("ns1", "hit", "def"); !ok || v != "value10" {
		t.Fatalf("hit: got (%q,%v)", v, ok)
	}
	// Empty string is a valid value — must NOT fall through to default.
	if v, ok := cli.GetConfigStatic("ns1", "empty", "def"); !ok || v != "" {
		t.Fatalf("empty value lookup: got (%q,%v), want ('', true)", v, ok)
	}
	// Miss — no such key.
	if v, ok := cli.GetConfigStatic("ns1", "nope", "def"); ok || v != "def" {
		t.Fatalf("miss: got (%q,%v), want ('def', false)", v, ok)
	}
	// Miss — no such ns.
	if v, ok := cli.GetConfigStatic("missing", "hit", "def"); ok || v != "def" {
		t.Fatalf("missing ns: got (%q,%v)", v, ok)
	}
}

func TestGetConfig_AbtestHitEmitsExposure(t *testing.T) {
	h := newHarness(t)
	// Cache has both full v=1 and ab v=2.
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "ab-v2"}}}))
	// Compute returns ab version=2 with experiment exposure.
	expID := "101"
	groupID := "202"
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
		Exposures: []*abtestv1.Exposure{
			{Key: "k", Version: 2, Source: "experiment_group", ExperimentId: &expID, GroupId: &groupID},
		},
	})

	sink := newDrainExposureSink()
	cfg := h.baseConfig([]string{"ns1"})
	cfg.ExposureSink = sink
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", map[string]any{"country": "US"})
	val, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if val != "ab-v2" {
		t.Fatalf("expected ab value, got %q", val)
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(sink.Events()) >= 1 }) {
		t.Fatalf("expected exposure, got %d", len(sink.Events()))
	}
	ev := sink.Events()[0]
	if ev.Source != "experiment_group" || ev.ExperimentID != expID || ev.GroupID != groupID || ev.Key != "k" || ev.Version != 2 || ev.UserID != "u1" {
		t.Fatalf("unexpected exposure: %+v", ev)
	}
}

func TestGetConfig_FullFallbackWhenAbtestUnavailable(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full-v1"}}}))
	h.abServer.SetError("ns1", status.Error(codes.Unavailable, "down"))

	sink := newDrainExposureSink()
	cfg := h.baseConfig([]string{"ns1"})
	cfg.ExposureSink = sink
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	val, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if val != "full-v1" {
		t.Fatalf("expected full fallback, got %q", val)
	}
	if cli.Metrics().AbtestFallbackTotal("ns1") == 0 {
		t.Fatal("expected abtest_fallback_total ns1 > 0")
	}
	if len(sink.Events()) != 0 {
		t.Fatalf("expected no exposure on full fallback; got %+v", sink.Events())
	}
}

func TestGetConfig_AbVersionMissingInCacheFallsBack(t *testing.T) {
	h := newHarness(t)
	// Cache only has v=1; abtest says v=99 which is NOT in cache.
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full-only"}}}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 99},
	})

	sink := newDrainExposureSink()
	cfg := h.baseConfig([]string{"ns1"})
	cfg.ExposureSink = sink
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	val, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if val != "full-only" {
		t.Fatalf("expected full fallback, got %q", val)
	}
	if cli.Metrics().AbtestFallbackTotal("ns1") == 0 {
		t.Fatal("expected abtest_fallback_total ns1 > 0 on ab->full fallback")
	}
	// Allow scheduler a moment, then assert no exposure was sent.
	time.Sleep(50 * time.Millisecond)
	if len(sink.Events()) != 0 {
		t.Fatalf("ab->full fallback must not emit exposure; got %+v", sink.Events())
	}
}

func TestGetConfig_NilAbtestContextErr(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v"}}}))
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()
	_, err = cli.GetConfig(context.Background(), nil, "ns1", "k", "")
	if !errors.Is(err, ErrAbtestContextMissing) {
		t.Fatalf("expected ErrAbtestContextMissing, got %v", err)
	}
}

func TestAbtestContext_OneComputePerNsPerRequest(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{
		"k1": {full: 1, versions: map[int64]string{1: "a", 2: "b"}},
		"k2": {full: 1, versions: map[int64]string{1: "c", 2: "d"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k1": 2, "k2": 2},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	// startupPullAll path doesn't call Compute, but harness uses an
	// in-process AbtestService so we still want to baseline.
	before := h.abServer.Calls("ns1")

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	v1, err := cli.GetConfig(context.Background(), abctx, "ns1", "k1", "")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := cli.GetConfig(context.Background(), abctx, "ns1", "k2", "")
	if err != nil {
		t.Fatal(err)
	}
	if v1 != "b" || v2 != "d" {
		t.Fatalf("expected ab values (b,d), got (%q,%q)", v1, v2)
	}
	// Only one Compute per (uid, ns), not one per key.
	if got := h.abServer.Calls("ns1") - before; got != 1 {
		t.Fatalf("expected exactly 1 Compute for ns1 across both GetConfig, got %d", got)
	}
}

func TestEmptyAbtestContext_NoCompute(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full"}}}))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	before := h.abServer.Calls("ns1")
	abctx := cli.EmptyAbtestContext()
	val, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if val != "full" {
		t.Fatalf("expected full fallback, got %q", val)
	}
	if got := h.abServer.Calls("ns1"); got != before {
		t.Fatalf("EmptyAbtestContext must not trigger Compute; calls before=%d after=%d", before, got)
	}
}

func TestMockAbtestContext_Helper(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full", 9: "ab9"}}}))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.MockAbtestContext("u1", map[string]map[string]int64{
		"ns1": {"k": 9},
	})
	val, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if val != "ab9" {
		t.Fatalf("MockAbtestContext should resolve k -> v9, got %q", val)
	}
}

func TestAbtestContext_TimeoutDegradesSilently(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full"}}}))
	// Make Compute hang past the per-call timeout.
	h.abServer.SetDelay(500 * time.Millisecond)
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{ConfigFlatKv: map[string]int64{"k": 99}})

	cfg := h.baseConfig([]string{"ns1"})
	cfg.AbtestTimeout = 20 * time.Millisecond
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	val, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if val != "full" {
		t.Fatalf("expected full fallback after timeout, got %q", val)
	}
	if cli.Metrics().AbtestFallbackTotal("ns1") == 0 {
		t.Fatal("expected fallback metric to be incremented on timeout")
	}
}

func TestAbtestContext_UnknownNamespaceReturnsEmpty(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v"}}}))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	res, err := abctx.WaitForAbtest(context.Background(), "not-subscribed")
	if err != nil {
		t.Fatalf("expected nil err for unknown ns, got %v", err)
	}
	if res == nil || len(res.keyVersions) != 0 {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

func TestAbtestContext_NilReceiver(t *testing.T) {
	var nilCtx *AbtestContext
	_, err := nilCtx.WaitForAbtest(context.Background(), "ns")
	if !errors.Is(err, ErrAbtestContextMissing) {
		t.Fatalf("expected ErrAbtestContextMissing, got %v", err)
	}
}

func TestEncodeUserAttrs_Types(t *testing.T) {
	out := encodeUserAttrs(map[string]any{
		"s":   "hi",
		"i":   42,
		"i64": int64(7),
		"f":   3.5,
		"b":   true,
	}, nil)
	if len(out) != 5 {
		t.Fatalf("expected all 5 attrs encoded, got %d", len(out))
	}
	// Drop unsupported types.
	out2 := encodeUserAttrs(map[string]any{
		"ok":  "x",
		"bad": []int{1, 2, 3},
	}, &warnRecorder{})
	if _, has := out2["bad"]; has {
		t.Fatal("unsupported slice value should not be encoded")
	}
	if _, has := out2["ok"]; !has {
		t.Fatal("expected ok to be encoded")
	}
}

type warnRecorder struct{ msgs []string }

func (w *warnRecorder) Warn(msg string, _ ...any) { w.msgs = append(w.msgs, msg) }

// SDK Init parameter validation.
func TestInit_MissingNamespaces(t *testing.T) {
	h := newHarness(t)
	cfg := h.baseConfig(nil)
	_, err := Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Init to error on empty Namespaces")
	}
}

func TestInit_MissingToken(t *testing.T) {
	h := newHarness(t)
	cfg := h.baseConfig([]string{"ns1"})
	cfg.Token = ""
	cfg.TokenProvider = nil
	_, err := Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Init to error without Token or TokenProvider")
	}
}

func TestInit_TokenProviderTakesPrecedence(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cfg.Token = ""
	calls := 0
	cfg.TokenProvider = func(ctx context.Context) (string, error) {
		calls++
		return h.token, nil
	}
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init with TokenProvider: %v", err)
	}
	defer cli.Close()
	if calls == 0 {
		t.Fatal("expected TokenProvider to be invoked at least once")
	}
}

func TestInit_TokenProviderError(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfig([]string{"ns1"})
	cfg.Token = ""
	boom := errors.New("kaboom")
	cfg.TokenProvider = func(ctx context.Context) (string, error) {
		return "", boom
	}
	_, err := Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Init to fail when TokenProvider errors")
	}
}

func TestClose_Idempotent(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cli.Close(); err != nil {
		t.Fatalf("second Close should be no-op, got %v", err)
	}
}

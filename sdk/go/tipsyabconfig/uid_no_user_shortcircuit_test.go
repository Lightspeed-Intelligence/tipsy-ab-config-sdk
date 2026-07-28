package tipsyabconfig

// Tests for the uid ""/"0" no-user short-circuit (design §1, decision D1):
// when an AbtestContext is constructed via NewAbtestContext with uid "" or "0",
// the lazy fetch layer (ensureFetch) resolves every not-yet-resolved namespace
// to the empty result WITHOUT issuing a GetExperimentResult RPC. This applies
// uniformly to GetConfig, GetAllConfigs, PrefetchConfigVersionFlatKvForNamespace
// and WaitForAbtest. uid "1" is unchanged (RPC fires). A MockAbtestContext with
// uid "" keeps its pre-seeded per-ns results (the short-circuit only affects the
// lazy path).
//
// The proof mechanism throughout is the fake abtest server's per-ns RPC counter
// (h.abServer.Calls / TotalCalls). To keep the assertions honest the snapshots
// deliberately leave has_dynamic_resolution ABSENT (never set it false): with an
// absent HDR field the ONLY thing that can suppress the RPC is the uid
// short-circuit, so a zero count is a genuine proof of it (not the HDR
// fast-path).

import (
	"context"
	"reflect"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// noUserSnapshot is the shared fixture for the uid short-circuit cases: two
// keys, both with a full release AND an ab version present in cache, and HDR
// left absent so the abtest path is NOT skipped by the fast-path. The armed
// abtest response would resolve both keys to their ab version IF the RPC fired
// — so a full-release result is itself proof the RPC was skipped.
func noUserSnapshot() (map[string]typedKey, map[string]int64) {
	keys := map[string]typedKey{
		"k1": {full: 1, versions: map[int64]string{1: "full1", 2: "ab1"}},
		"k2": {full: 3, versions: map[int64]string{3: "full2", 4: "ab2"}},
	}
	flatKv := map[string]int64{"k1": 2, "k2": 4}
	return keys, flatKv
}

// TestNoUserUID_GetConfigZeroRPC verifies that GetConfig with uid "" or "0"
// issues ZERO RPC and returns the pure full-release value (not the armed ab
// value), for both no-user uid spellings; uid "1" is the regression control
// that still fires one RPC and resolves the ab value.
func TestNoUserUID_GetConfigZeroRPC(t *testing.T) {
	keys, flatKv := noUserSnapshot()

	for _, uid := range []string{"", "0"} {
		t.Run("uid="+quoteUID(uid), func(t *testing.T) {
			h := newHarness(t)
			h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, keys))
			h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{ConfigFlatKv: flatKv})
			cli := initClient(t, h.baseConfig([]string{"ns1"}))

			before := h.abServer.Calls("ns1")
			abctx := cli.NewAbtestContext(context.Background(), uid, nil)
			v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k1", "def")
			if err != nil {
				t.Fatalf("GetConfig: %v", err)
			}
			if v != "full1" {
				t.Fatalf("no-user uid must resolve full release, got %q want full1", v)
			}
			if delta := h.abServer.Calls("ns1") - before; delta != 0 {
				t.Fatalf("no-user uid GetConfig must issue ZERO RPC, got %d", delta)
			}
		})
	}

	// Regression control: a real uid still fires exactly one RPC and wins the
	// ab value.
	t.Run("uid=1", func(t *testing.T) {
		h := newHarness(t)
		h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, keys))
		h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{ConfigFlatKv: flatKv})
		cli := initClient(t, h.baseConfig([]string{"ns1"}))

		before := h.abServer.Calls("ns1")
		abctx := cli.NewAbtestContext(context.Background(), "1", nil)
		v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k1", "def")
		if err != nil {
			t.Fatalf("GetConfig: %v", err)
		}
		if v != "ab1" {
			t.Fatalf("real uid must resolve ab value, got %q want ab1", v)
		}
		if delta := h.abServer.Calls("ns1") - before; delta != 1 {
			t.Fatalf("real uid GetConfig must issue exactly 1 RPC, got %d", delta)
		}
	})
}

// TestNoUserUID_GetAllConfigsZeroRPC is the GetAllConfigs twin: uid "" / "0"
// must resolve every key to its full-release value with ZERO RPC; uid "1"
// fires one RPC and resolves the ab versions.
func TestNoUserUID_GetAllConfigsZeroRPC(t *testing.T) {
	keys, flatKv := noUserSnapshot()

	for _, uid := range []string{"", "0"} {
		t.Run("uid="+quoteUID(uid), func(t *testing.T) {
			h := newHarness(t)
			h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, keys))
			h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{ConfigFlatKv: flatKv})
			cli := initClient(t, h.baseConfig([]string{"ns1"}))

			before := h.abServer.Calls("ns1")
			abctx := cli.NewAbtestContext(context.Background(), uid, nil)
			got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
			if err != nil {
				t.Fatalf("GetAllConfigs: %v", err)
			}
			want := map[string]string{"k1": "full1", "k2": "full2"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("no-user uid GetAllConfigs must be pure full-release: got=%#v want=%#v", got, want)
			}
			if delta := h.abServer.Calls("ns1") - before; delta != 0 {
				t.Fatalf("no-user uid GetAllConfigs must issue ZERO RPC, got %d", delta)
			}
		})
	}

	t.Run("uid=1", func(t *testing.T) {
		h := newHarness(t)
		h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, keys))
		h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{ConfigFlatKv: flatKv})
		cli := initClient(t, h.baseConfig([]string{"ns1"}))

		before := h.abServer.Calls("ns1")
		abctx := cli.NewAbtestContext(context.Background(), "1", nil)
		got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
		if err != nil {
			t.Fatalf("GetAllConfigs: %v", err)
		}
		want := map[string]string{"k1": "ab1", "k2": "ab2"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("real uid GetAllConfigs must resolve ab versions: got=%#v want=%#v", got, want)
		}
		if delta := h.abServer.Calls("ns1") - before; delta != 1 {
			t.Fatalf("real uid GetAllConfigs must issue exactly 1 RPC, got %d", delta)
		}
	})
}

// TestNoUserUID_MockSeededResultsStillApply verifies the short-circuit only
// affects the lazy path: a MockAbtestContext with uid "" whose per-ns result is
// pre-seeded still resolves via that seed (the seeded ns is never "not yet
// resolved", so it never reaches the short-circuit branch).
func TestNoUserUID_MockSeededResultsStillApply(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full", 9: "ab9"}},
	}))
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	// uid "" but ns1 pre-seeded to version 9.
	abctx := cli.MockAbtestContext("", map[string]map[string]int64{
		"ns1": {"k": 9},
	})
	v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if v != "ab9" {
		t.Fatalf("mock(uid=\"\") seeded result must still resolve, got %q want ab9", v)
	}
	// GetAllConfigs on the same seeded mock ctx resolves the same version.
	got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs: %v", err)
	}
	if want := map[string]string{"k": "ab9"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mock(uid=\"\") GetAllConfigs seeded result mismatch: got=%#v want=%#v", got, want)
	}
}

// TestNoUserUID_PrefetchAndWaitZeroRPC verifies the short-circuit reaches the
// Prefetch + WaitForAbtest paths (they route through the same ensureFetch): a
// no-user uid must make both no-ops RPC-wise, and WaitForAbtest returns the
// empty result with no error.
func TestNoUserUID_PrefetchAndWaitZeroRPC(t *testing.T) {
	for _, uid := range []string{"", "0"} {
		t.Run("uid="+quoteUID(uid), func(t *testing.T) {
			h := newHarness(t)
			h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
				"k": {full: 1, versions: map[int64]string{1: "full", 2: "ab"}},
			}))
			h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
				ConfigFlatKv: map[string]int64{"k": 2},
			})
			cli := initClient(t, h.baseConfig([]string{"ns1"}))

			before := h.abServer.TotalCalls()
			abctx := cli.NewAbtestContext(context.Background(), uid, nil)
			abctx.PrefetchConfigVersionFlatKvForNamespace("ns1")

			res, err := abctx.WaitForAbtest(context.Background(), "ns1")
			if err != nil {
				t.Fatalf("WaitForAbtest: %v", err)
			}
			if res == nil || len(res.keyVersions) != 0 {
				t.Fatalf("no-user uid WaitForAbtest must return empty result, got %+v", res)
			}
			// Let any erroneous background RPC surface.
			time.Sleep(50 * time.Millisecond)
			if delta := h.abServer.TotalCalls() - before; delta != 0 {
				t.Fatalf("no-user uid prefetch + wait must issue ZERO RPC, got %d", delta)
			}
		})
	}
}

// TestNoUserUID_EmptyAbtestContextUnchanged is the regression guard that the
// existing EmptyAbtestContext semantics are untouched by the uid change:
// GetAllConfigs on an EmptyAbtestContext resolves full-release with no RPC.
func TestNoUserUID_EmptyAbtestContextUnchanged(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full", 2: "ab"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	before := h.abServer.Calls("ns1")
	abctx := cli.EmptyAbtestContext()
	got, err := cli.GetAllConfigs(context.Background(), abctx, "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs: %v", err)
	}
	if want := map[string]string{"k": "full"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("EmptyAbtestContext GetAllConfigs mismatch: got=%#v want=%#v", got, want)
	}
	if delta := h.abServer.Calls("ns1") - before; delta != 0 {
		t.Fatalf("EmptyAbtestContext must issue ZERO RPC, got %d", delta)
	}
}

// quoteUID renders "" as <empty> for readable subtest names.
func quoteUID(uid string) string {
	if uid == "" {
		return "<empty>"
	}
	return uid
}

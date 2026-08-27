package tipsyabconfig

// Unit tests for the SDK-local per-group merge algorithm
// (actual-enrollment-log design §3, Testing Plan 1).
//
// The merge transposes GetExperimentResultResponse.groups[]/gray_hits[] into
// abtestComputeResult.keyVersions (+ per-key attribution). These tests observe
// the merged keyVersions map through WaitForAbtest (same-package access) and
// the resolved values through GetConfig; attribution is asserted separately on
// the EMITTED log lines in enrollment_log_test.go.
//
// TEST CONVENTION (design Testing Plan, r6): any case observing "second and
// later call" behaviour (RPC counts, degrade deltas) constructs a FRESH
// AbtestContext — the ctx memoises per-ns results, so a reused ctx observes
// only the cached merge and the assertion passes trivially (no-op form 7).
//
// WHAT THESE TESTS DO NOT PROVE: they run against fixtures WE construct, so
// they cannot detect a real server whose per-group payload disagrees with its
// flat_kv payload (design R1 / r3-F4 — that risk is carried by the platform
// source review (Goal 6), the platform's own compute test suite (3b-ii), and
// the pre-release live-environment check (AC10)). Do not read green checks
// here as proof of server-side per-group correctness.

import (
	"context"
	"testing"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// mergedResultFor drives one fresh-ctx fetch for ns1 and returns the merged
// compute result (keyVersions is the SDK-local merge output).
func mergedResultFor(t *testing.T, cli *Client) *abtestComputeResult {
	t.Helper()
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	res, err := abctx.WaitForAbtest(context.Background(), "ns1")
	if err != nil {
		t.Fatalf("WaitForAbtest: %v", err)
	}
	if res == nil {
		t.Fatal("WaitForAbtest returned nil result")
	}
	return res
}

// TestMerge_PureExperiment_WritesKeyVersions: groups only (no gray_hits) —
// every params_versions entry lands in keyVersions verbatim.
func TestMerge_PureExperiment_WritesKeyVersions(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"kA": {full: 1, versions: map[int64]string{1: "full-a", 101: "exp-a"}},
		"kB": {full: 1, versions: map[int64]string{1: "full-b", 102: "exp-b"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		Groups: []*abtestv1.ExperimentGroupResult{
			{
				ExperimentId:   "e1",
				GroupId:        "g1",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"kA": 101},
			},
			{
				ExperimentId:   "e2",
				GroupId:        "g2",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"kB": 102},
			},
		},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	res := mergedResultFor(t, cli)
	if got := len(res.keyVersions); got != 2 {
		t.Fatalf("merged keyVersions size = %d, want 2 (%+v)", got, res.keyVersions)
	}
	if res.keyVersions["kA"] != 101 || res.keyVersions["kB"] != 102 {
		t.Fatalf("merged keyVersions = %+v, want kA:101 kB:102", res.keyVersions)
	}

	// Value resolution end-to-end (fresh ctx not required: same merge result).
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "kA", "def"); err != nil || v != "exp-a" {
		t.Fatalf("kA: got (%q,%v), want exp-a", v, err)
	}
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "kB", "def"); err != nil || v != "exp-b" {
		t.Fatalf("kB: got (%q,%v), want exp-b", v, err)
	}
}

// TestMerge_PureGray_WritesKeyVersions: gray_hits only — every key_versions
// entry lands in keyVersions verbatim.
func TestMerge_PureGray_WritesKeyVersions(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"kG": {full: 1, versions: map[int64]string{1: "full-g", 201: "gray-g"}},
		"kH": {full: 1, versions: map[int64]string{1: "full-h", 202: "gray-h"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		GrayHits: []*abtestv1.GrayReleaseHit{
			// Wire contract: gray_hits ascend by release_id (platform-guarded,
			// design §3 order table).
			{ReleaseId: 5, KeyVersions: map[string]int64{"kG": 201}},
			{ReleaseId: 9, KeyVersions: map[string]int64{"kH": 202}},
		},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	res := mergedResultFor(t, cli)
	if got := len(res.keyVersions); got != 2 {
		t.Fatalf("merged keyVersions size = %d, want 2 (%+v)", got, res.keyVersions)
	}
	if res.keyVersions["kG"] != 201 || res.keyVersions["kH"] != 202 {
		t.Fatalf("merged keyVersions = %+v, want kG:201 kH:202", res.keyVersions)
	}
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "kG", "def"); err != nil || v != "gray-g" {
		t.Fatalf("kG: got (%q,%v), want gray-g", v, err)
	}
}

// TestMerge_CrossSource_GrayBeatsExperiment_Strict is the GATE-LEVEL cross-
// source conflict case (design §3 / AC8): the same key claimed by a gray hit
// AND an experiment group must resolve to the GRAY versionId, unconditionally
// (replicates platform engine.go:355-356 "grayOwned → continue").
//
// Fixture discipline (design Testing Plan 3a, quoting platform
// conflictkey_test.go:79-80): the two candidates MUST carry DIFFERENT
// versionIds (2 vs 3) — with equal ids a wrong winner would still produce the
// right answer and this assertion would be a no-op (form 4).
func TestMerge_CrossSource_GrayBeatsExperiment_Strict(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "gray-v2", 3: "exp-v3"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		GrayHits: []*abtestv1.GrayReleaseHit{
			{ReleaseId: 11, KeyVersions: map[string]int64{"k": 2}},
		},
		Groups: []*abtestv1.ExperimentGroupResult{{
			ExperimentId:   "e1",
			GroupId:        "g1",
			ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
			ParamsVersions: map[string]int64{"k": 3},
		}},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	res := mergedResultFor(t, cli)
	if got := res.keyVersions["k"]; got != 2 {
		t.Fatalf("cross-source conflict: keyVersions[k] = %d, want 2 (gray wins unconditionally)", got)
	}
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil || v != "gray-v2" {
		t.Fatalf("cross-source conflict value: got (%q,%v), want gray-v2", v, err)
	}
}

// TestMerge_SameSource_ExperimentConflict_Weak: two experiment groups claim the
// same key with DIFFERENT versionIds. Per the user's relaxation (design §3 /
// AC8) the ONLY guaranteed contract is "至少返回可选值中的一个" — so this
// asserts (a) the key is NOT dropped and (b) the winner is IN the candidate
// set. It deliberately does NOT assert winner identity (platform parity) and
// sets NO stability assertion (user decision, third round). A last↔first
// winner-direction mutation MUST survive this test.
func TestMerge_SameSource_ExperimentConflict_Weak(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "exp-v2", 3: "exp-v3"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		Groups: []*abtestv1.ExperimentGroupResult{
			{
				ExperimentId:   "e1",
				GroupId:        "g1",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"k": 2},
			},
			{
				ExperimentId:   "e2",
				GroupId:        "g2",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"k": 3},
			},
		},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	res := mergedResultFor(t, cli)
	got, present := res.keyVersions["k"]
	if !present {
		t.Fatal("same-source conflict must NOT drop the key (value-loss is outside the relaxation)")
	}
	if got != 2 && got != 3 {
		t.Fatalf("same-source conflict winner = %d, want one of {2, 3}", got)
	}
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if v != "exp-v2" && v != "exp-v3" {
		t.Fatalf("same-source conflict value = %q, want one of exp-v2/exp-v3", v)
	}
}

// TestMerge_MultiGrayConflict_Defensive: two gray_hits carrying the SAME key is
// structurally unreachable on the wire (design §3 / r3-F2: platform computeGray
// folds per-key winners BEFORE assembleGrayHits buckets them, so a key lands in
// exactly one gray hit). This synthetic fixture only guards the defensive code
// path: no panic, key not dropped, winner within the candidate set. It is NOT
// wire-level equivalence evidence and there is no flat-side expectation to
// compute — do not count it toward merge/alignment coverage or gate on it.
func TestMerge_MultiGrayConflict_Defensive(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "gray-v2", 5: "gray-v5"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		GrayHits: []*abtestv1.GrayReleaseHit{
			{ReleaseId: 3, KeyVersions: map[string]int64{"k": 2}},
			{ReleaseId: 9, KeyVersions: map[string]int64{"k": 5}},
		},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	res := mergedResultFor(t, cli)
	got, present := res.keyVersions["k"]
	if !present {
		t.Fatal("multi-gray conflict must NOT drop the key")
	}
	if got != 2 && got != 5 {
		t.Fatalf("multi-gray conflict winner = %d, want one of {2, 5}", got)
	}
}

// TestMerge_EmptyResponse_FullFallback: groups+gray_hits both empty ⇒ empty
// merge result, GetConfig falls through to the full release (no flat fallback,
// design §7).
//
// COVERAGE HONESTY (design Testing Plan 1, no-op form 1): this case has ZERO
// discriminating power for the merge mechanism — an entirely broken merge also
// yields an empty result and a full_release fallback. Do NOT count it toward
// merge coverage in any delivery report.
func TestMerge_EmptyResponse_FullFallback(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	res := mergedResultFor(t, cli)
	if len(res.keyVersions) != 0 {
		t.Fatalf("empty response must merge to empty keyVersions, got %+v", res.keyVersions)
	}
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil || v != "full-v1" {
		t.Fatalf("empty response GetConfig: got (%q,%v), want full-v1", v, err)
	}
}

// TestMerge_EmptyIdGroup_ValueStillWritten_F3 is the value/attribution
// decoupling invariant (design §3 F3): a group with EMPTY experiment_id +
// group_id still gets its params_versions written into keyVersions — skipping
// it would change value resolution, which attribution must never do.
//
// Fixture constraint (design Testing Plan 1): the empty-id group is PAIRED
// with an attributed group, so "reason=abtest_unattributed" (asserted in
// enrollment_log_test.go) cannot be vacuously produced by a merge that writes
// nothing at all — the discriminating assertion HERE is "the value is still
// written correctly".
func TestMerge_EmptyIdGroup_ValueStillWritten_F3(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"kA": {full: 1, versions: map[int64]string{1: "full-a", 2: "anon-a"}},
		"kB": {full: 1, versions: map[int64]string{1: "full-b", 3: "exp-b"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		Groups: []*abtestv1.ExperimentGroupResult{
			{
				// Empty ids: attribution degrades to unattributed; the VALUE
				// must not be lost (F3).
				ExperimentId:   "",
				GroupId:        "",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"kA": 2},
			},
			{
				ExperimentId:   "e1",
				GroupId:        "g1",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"kB": 3},
			},
		},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	res := mergedResultFor(t, cli)
	if res.keyVersions["kA"] != 2 {
		t.Fatalf("empty-id group: keyVersions[kA] = %d, want 2 (value must be written despite missing attribution)", res.keyVersions["kA"])
	}
	if res.keyVersions["kB"] != 3 {
		t.Fatalf("attributed group: keyVersions[kB] = %d, want 3", res.keyVersions["kB"])
	}
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "kA", "def"); err != nil || v != "anon-a" {
		t.Fatalf("empty-id group value: got (%q,%v), want anon-a", v, err)
	}
}

// TestMerge_GrayReleaseIdZero_ValueStillWritten_F3 is the gray-side twin of the
// F3 invariant: a gray hit with release_id=0 still writes its key_versions
// (attribution degrades to unattributed; asserted on logs elsewhere). Paired
// with an attributed gray hit per the F3 fixture constraint.
func TestMerge_GrayReleaseIdZero_ValueStillWritten_F3(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"kC": {full: 1, versions: map[int64]string{1: "full-c", 4: "anon-c"}},
		"kD": {full: 1, versions: map[int64]string{1: "full-d", 5: "gray-d"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		GrayHits: []*abtestv1.GrayReleaseHit{
			{ReleaseId: 0, KeyVersions: map[string]int64{"kC": 4}},
			{ReleaseId: 7, KeyVersions: map[string]int64{"kD": 5}},
		},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	res := mergedResultFor(t, cli)
	if res.keyVersions["kC"] != 4 {
		t.Fatalf("release_id=0 gray hit: keyVersions[kC] = %d, want 4 (value must be written)", res.keyVersions["kC"])
	}
	if res.keyVersions["kD"] != 5 {
		t.Fatalf("attributed gray hit: keyVersions[kD] = %d, want 5", res.keyVersions["kD"])
	}
}

// TestMerge_NonConfigVersionGroupFiltered: the merge only consumes
// experiment_type=CONFIG_VERSION groups (design §3 step 2). This filter is
// fail-closed BY DESIGN (a deliberate exception to F3's fail-open direction,
// design r6 suggestion 1): a CUSTOM_PARAMS group carries no config_version
// semantics, so its params must not leak into keyVersions.
func TestMerge_NonConfigVersionGroupFiltered(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"kCP": {full: 1, versions: map[int64]string{1: "full-cp", 2: "leak"}},
		"kCV": {full: 1, versions: map[int64]string{1: "full-cv", 3: "exp-cv"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		Groups: []*abtestv1.ExperimentGroupResult{
			{
				ExperimentId:   "e-cp",
				GroupId:        "g-cp",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CUSTOM_PARAMS,
				ParamsVersions: map[string]int64{"kCP": 2},
			},
			{
				ExperimentId:   "e-cv",
				GroupId:        "g-cv",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"kCV": 3},
			},
		},
	})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	res := mergedResultFor(t, cli)
	if _, present := res.keyVersions["kCP"]; present {
		t.Fatalf("CUSTOM_PARAMS group must be filtered out of the config_version merge, got keyVersions=%+v", res.keyVersions)
	}
	if res.keyVersions["kCV"] != 3 {
		t.Fatalf("CONFIG_VERSION group must be merged: keyVersions[kCV] = %d, want 3", res.keyVersions["kCV"])
	}
}

// TestInternalFetch_RequestShape_EachExperimentGroup asserts the EMITTED proto
// request of the lazy per-ns fetch (design Testing Plan 1 "内部 fetch 请求形
// 状"): display_type=EACH_EXPERIMENT_GROUP + experiment_type=CONFIG_VERSION.
// The prefetch-path twins live in TestPrefetchAPI_Shape /
// TestPrefetch_CarriesContextTraceID (reversed by this task). The public
// GetExperimentResult passthrough is asserted (and must stay untouched) in
// TestGetExperimentResult_Client.
func TestInternalFetch_RequestShape_EachExperimentGroup(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	// Fresh ctx: first GetConfig on the ns is the one that fires the RPC.
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	if _, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	req := h.abServer.LastRequest()
	if req == nil {
		t.Fatal("no GetExperimentResult request captured for the lazy fetch")
	}
	if req.GetDisplayType() != abtestv1.ResultDisplayType_RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP {
		t.Fatalf("internal fetch display_type = %v, want EACH_EXPERIMENT_GROUP (design §1)", req.GetDisplayType())
	}
	if req.GetExperimentType() != abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION {
		t.Fatalf("internal fetch experiment_type = %v, want CONFIG_VERSION", req.GetExperimentType())
	}
}

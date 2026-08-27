package tipsyabconfig

// Alignment tests (actual-enrollment-log design Testing Plan 3a — user-named
// category, registered independently of the unit suite): for the same logical
// scenario, build BOTH the legacy FLAT_KV-shaped fixture (what an old SDK
// consumed verbatim into keyVersions) and the per-group fixture (what the new
// SDK fetches), and assert
//
//	localMerge(perGroupFixture) == flatFixture's key→versionId map.
//
// The flat-side expectations are HAND-COMPUTED literals following the
// platform's flat assembly rules (platform repo
// internal/abtest/compute/engine.go:329-385, design §3):
//   - gray keys are written first and own their keys unconditionally
//     (engine.go:355-356: experiment entries for gray-owned keys are skipped);
//   - experiment entries otherwise write through.
//
// They are independent anchors, NOT computed by the code under test — never
// "simplify" them into calls to the SDK merge (that would make each assertion
// a tautology).
//
// CAPABILITY BOUNDARY (design r3-F4, must survive into any delivery report):
// both fixtures here are authored by us and the flat expectations follow OUR
// reading of engine.go — by construction these tests CANNOT detect a real
// server whose per-group payload diverges from its flat payload. That risk is
// carried by the platform source review (Goal 6), the platform's own
// internal/abtest/compute suite (Testing Plan 3b-ii) and the pre-release live
// check (AC10). What 3a does guard: the SDK merge staying equivalent to the
// old flat consumption as the SDK evolves.
//
// Scenario set (design 3a):
//   - strict equality (gate-level): pure experiment / pure gray /
//     gray+experiment overlap (gray wins) / empty;
//   - weak (non-gate, user relaxation): same-source experiment conflict —
//     winner ∈ candidate set + key not dropped; NO winner-identity and NO
//     stability assertion;
//   - deliberately ABSENT: multi-gray conflict — structurally unreachable on
//     the wire (design §3 / r3-F2), no flat-side expectation exists; its
//     defensive guard lives in enrollment_merge_test.go.

import (
	"context"
	"reflect"
	"testing"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// alignmentMerge feeds the per-group response to a fresh harness client and
// returns the SDK's locally merged key→versionId map for ns1.
func alignmentMerge(t *testing.T, resp *abtestv1.GetExperimentResultResponse) map[string]int64 {
	t.Helper()
	h := newHarness(t)
	// The merge does not read the config cache; an empty snapshot subscribes
	// the ns so WaitForAbtest reaches the fetch path.
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.abServer.SetResponse("ns1", resp)
	cli := initClient(t, h.baseConfig([]string{"ns1"}))

	// Fresh ctx per scenario (memo convention, design Testing Plan r6 note).
	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	res, err := abctx.WaitForAbtest(context.Background(), "ns1")
	if err != nil {
		t.Fatalf("WaitForAbtest: %v", err)
	}
	if res == nil {
		t.Fatal("nil compute result")
	}
	// Copy so reflect.DeepEqual sees a plain map (and nil-vs-empty is
	// normalised by the caller's literals being non-nil).
	out := make(map[string]int64, len(res.keyVersions))
	for k, v := range res.keyVersions {
		out[k] = v
	}
	return out
}

// TestAlignment_PureExperiment_Strict: experiment-only scenario. Old server
// flat_kv = every group's params_versions folded (no conflicts here) — the
// per-group merge must reproduce it exactly.
func TestAlignment_PureExperiment_Strict(t *testing.T) {
	perGroup := &abtestv1.GetExperimentResultResponse{
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
				ParamsVersions: map[string]int64{"kB": 102, "kC": 103},
			},
		},
	}
	// Flat-side fixture: what the old server's flat assembly (engine.go:340+,
	// experiment write-through, no gray, no conflicts) would have returned and
	// the old SDK consumed verbatim. Hand-computed literal.
	flatFixture := map[string]int64{"kA": 101, "kB": 102, "kC": 103}

	got := alignmentMerge(t, perGroup)
	if !reflect.DeepEqual(got, flatFixture) {
		t.Fatalf("alignment (pure experiment): merged=%v, flat=%v", got, flatFixture)
	}
}

// TestAlignment_PureGray_Strict: gray-only scenario. Old server flat_kv =
// every gray hit's key_versions folded (engine.go:335-337 writes gray keys
// first; no conflicts here).
func TestAlignment_PureGray_Strict(t *testing.T) {
	perGroup := &abtestv1.GetExperimentResultResponse{
		GrayHits: []*abtestv1.GrayReleaseHit{
			// release_id ascending — the platform-guaranteed wire order.
			{ReleaseId: 5, KeyVersions: map[string]int64{"kG": 201}},
			{ReleaseId: 12, KeyVersions: map[string]int64{"kH": 202, "kI": 203}},
		},
	}
	flatFixture := map[string]int64{"kG": 201, "kH": 202, "kI": 203}

	got := alignmentMerge(t, perGroup)
	if !reflect.DeepEqual(got, flatFixture) {
		t.Fatalf("alignment (pure gray): merged=%v, flat=%v", got, flatFixture)
	}
}

// TestAlignment_GrayExperimentOverlap_GrayWins_Strict is the gate-level
// cross-source alignment case: key kX is claimed by a gray hit (301) AND an
// experiment group (302). Platform flat assembly: gray writes kX=301 first;
// the experiment loop SKIPS gray-owned kX (engine.go:355-356) and writes only
// kY=303. Hand-computed flat = {kX:301, kY:303}.
//
// Fixture discipline (platform conflictkey_test.go:79-80, quoted by design):
// the two kX candidates carry DIFFERENT versionIds (301 vs 302) — equal ids
// would let a wrong winner produce the right map (no-op form 4).
func TestAlignment_GrayExperimentOverlap_GrayWins_Strict(t *testing.T) {
	perGroup := &abtestv1.GetExperimentResultResponse{
		GrayHits: []*abtestv1.GrayReleaseHit{
			{ReleaseId: 9, KeyVersions: map[string]int64{"kX": 301}},
		},
		Groups: []*abtestv1.ExperimentGroupResult{
			{
				ExperimentId:   "e1",
				GroupId:        "g1",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"kX": 302, "kY": 303},
			},
		},
	}
	flatFixture := map[string]int64{"kX": 301, "kY": 303}

	got := alignmentMerge(t, perGroup)
	if !reflect.DeepEqual(got, flatFixture) {
		t.Fatalf("alignment (gray beats experiment): merged=%v, flat=%v", got, flatFixture)
	}
}

// TestAlignment_Empty_Strict: empty responses align trivially. COVERAGE
// HONESTY: zero discriminating power for the merge (a broken merge also
// produces an empty map) — kept only for scenario-set completeness, do not
// count it toward alignment coverage.
func TestAlignment_Empty_Strict(t *testing.T) {
	got := alignmentMerge(t, &abtestv1.GetExperimentResultResponse{})
	if !reflect.DeepEqual(got, map[string]int64{}) {
		t.Fatalf("alignment (empty): merged=%v, want empty map", got)
	}
}

// TestAlignment_SameSourceConflict_Weak: two experiment groups claim kC with
// different versionIds. User relaxation (design §3 / AC8): the platform+SDK
// contract for same-source conflicts is only "至少返回可选值中的一个" — assert
// winner ∈ {401, 402} and the key is not dropped. Deliberately NOT asserted:
// which candidate wins (the platform's own flat winner is last-write 402, but
// pinning it here would gate a non-guaranteed property) and any stability
// property (user decision: no such test; ordering constraint lives as a code
// comment in the merge implementation).
func TestAlignment_SameSourceConflict_Weak(t *testing.T) {
	perGroup := &abtestv1.GetExperimentResultResponse{
		Groups: []*abtestv1.ExperimentGroupResult{
			{
				ExperimentId:   "e1",
				GroupId:        "g1",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"kC": 401},
			},
			{
				ExperimentId:   "e2",
				GroupId:        "g2",
				ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
				ParamsVersions: map[string]int64{"kC": 402},
			},
		},
	}

	got := alignmentMerge(t, perGroup)
	winner, present := got["kC"]
	if !present {
		t.Fatal("alignment (same-source conflict): key kC dropped — value loss is outside the relaxation")
	}
	if winner != 401 && winner != 402 {
		t.Fatalf("alignment (same-source conflict): winner = %d, want one of {401, 402}", winner)
	}
	if len(got) != 1 {
		t.Fatalf("alignment (same-source conflict): unexpected extra keys: %v", got)
	}
}

package tipsyabconfig

// Emitted-log assertions for the getConfig hit log (actual-enrollment-log
// design §4, Testing Plan 1 "日志字段断言").
//
// CONTRACT UNDER TEST (design §4, SLS parsing contract): every dynamic-path hit
// log carries reason + uid + trace_id + ns + key + version; experiment_id /
// group_id / release_id are CONDITIONAL fields, present per reason and OMITTED
// (key absent, not empty placeholder) otherwise. reason enum is exactly
// {full_release, experiment, gray_whitelist, abtest_unattributed}. Every
// assertion here targets the EMITTED slog record captured via logRecorder —
// never a constant/enum declaration (no-op form 6).
//
// TEST CONVENTION (design Testing Plan, r6): cases observing "second and later
// call" behaviour (fast-path zero-RPC, degrade metric deltas) construct a
// FRESH AbtestContext per observed call — the ctx memoises per-ns results.
//
// WHAT THESE TESTS DO NOT PROVE: fixtures are locally constructed, so green
// here does not certify real-server per-group payloads (design R1; carried by
// Goal 6 review + platform suite + AC10 live check). The MockAbtestContext
// case proves only the unattributed fallback works — it must NOT be counted
// toward attribution-mechanism coverage (design Testing Plan 1, mock 用例定位).

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// TestLogRecorder_CapturesStaticHit_Smoke is the injected-degradation guard for
// the log observation infrastructure itself: it proves logRecorder actually
// receives the Client's log lines through cfg.Logger, using a log statement
// that predates this task (get_config_static hit). If this smoke test cannot
// see that line, every reason/omit assertion in this file would be observing a
// dead capture point and passing vacuously — treat a failure here as "fix the
// harness", never "relax the assertions".
func TestLogRecorder_CapturesStaticHit_Smoke(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 10, versions: map[int64]string{10: "v10"}},
	}))
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cli := initClient(t, cfg)

	if v, ok := cli.GetConfigStatic("ns1", "k", "def"); !ok || v != "v10" {
		t.Fatalf("GetConfigStatic: got (%q,%v)", v, ok)
	}
	recs := findRecordByMsg(rec, "tipsyabconfig: get_config_static hit")
	if len(recs) != 1 {
		t.Fatalf("logRecorder did not capture the pre-existing static hit line (got %d records: %+v) — observation point is dead", len(recs), rec.all())
	}
	if got := attrString(t, recs[0], "source"); got != "full_static" {
		t.Fatalf("static hit source = %q, want full_static", got)
	}
}

// TestHitLog_ReasonExperiment_Fields: experiment attribution ⇒ msg (abtest),
// reason=experiment, experiment_id+group_id present, release_id OMITTED, and
// the constant fields (ns/key/version/uid/trace_id) all correct.
func TestHitLog_ReasonExperiment_Fields(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "exp-v2"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		Groups: []*abtestv1.ExperimentGroupResult{{
			ExperimentId:   "exp-42",
			GroupId:        "grp-7",
			ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
			ParamsVersions: map[string]int64{"k": 2},
		}},
	})
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-exp", nil, "trace-exp")
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil || v != "exp-v2" {
		t.Fatalf("GetConfig: got (%q,%v), want exp-v2", v, err)
	}

	hit := findConfigHit(t, rec, "k")
	if hit.msg != hitMsgAbtest {
		t.Fatalf("hit msg = %q, want %q (msg text must stay stable, design §4)", hit.msg, hitMsgAbtest)
	}
	if got := attrString(t, hit, "reason"); got != "experiment" {
		t.Fatalf("reason = %q, want experiment", got)
	}
	if got := attrString(t, hit, "experiment_id"); got != "exp-42" {
		t.Fatalf("experiment_id = %q, want exp-42", got)
	}
	if got := attrString(t, hit, "group_id"); got != "grp-7" {
		t.Fatalf("group_id = %q, want grp-7", got)
	}
	if got := attrString(t, hit, "ns"); got != "ns1" {
		t.Fatalf("ns = %q, want ns1", got)
	}
	if got := attrInt64(t, hit, "version"); got != 2 {
		t.Fatalf("version = %d, want 2 (versionId semantics)", got)
	}
	if got := attrString(t, hit, "uid"); got != "u-exp" {
		t.Fatalf("uid = %q, want u-exp", got)
	}
	if got := attrString(t, hit, "trace_id"); got != "trace-exp" {
		t.Fatalf("trace_id = %q, want trace-exp", got)
	}
	requireNoAttrs(t, hit, "release_id")
}

// TestHitLog_ReasonGrayWhitelist_Fields: gray attribution ⇒ reason=
// gray_whitelist, release_id present (int64), experiment_id/group_id OMITTED.
func TestHitLog_ReasonGrayWhitelist_Fields(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "gray-v2"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		GrayHits: []*abtestv1.GrayReleaseHit{
			{ReleaseId: 33, KeyVersions: map[string]int64{"k": 2}},
		},
	})
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-gray", nil, "trace-gray")
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil || v != "gray-v2" {
		t.Fatalf("GetConfig: got (%q,%v), want gray-v2", v, err)
	}

	hit := findConfigHit(t, rec, "k")
	if hit.msg != hitMsgAbtest {
		t.Fatalf("hit msg = %q, want %q", hit.msg, hitMsgAbtest)
	}
	if got := attrString(t, hit, "reason"); got != "gray_whitelist" {
		t.Fatalf("reason = %q, want gray_whitelist", got)
	}
	if got := attrInt64(t, hit, "release_id"); got != 33 {
		t.Fatalf("release_id = %d, want 33 (int64 per design §4)", got)
	}
	if got := attrInt64(t, hit, "version"); got != 2 {
		t.Fatalf("version = %d, want 2", got)
	}
	if got := attrString(t, hit, "uid"); got != "u-gray" {
		t.Fatalf("uid = %q, want u-gray", got)
	}
	if got := attrString(t, hit, "trace_id"); got != "trace-gray" {
		t.Fatalf("trace_id = %q, want trace-gray", got)
	}
	requireNoAttrs(t, hit, "experiment_id", "group_id")
}

// TestHitLog_ReasonFullRelease_PureFull: no abtest hit for the key ⇒ msg
// (full), reason=full_release, all conditional attribution fields OMITTED.
func TestHitLog_ReasonFullRelease_PureFull(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 6, versions: map[int64]string{6: "full-v6"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-full", nil, "trace-full")
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil || v != "full-v6" {
		t.Fatalf("GetConfig: got (%q,%v), want full-v6", v, err)
	}

	hit := findConfigHit(t, rec, "k")
	if hit.msg != hitMsgFull {
		t.Fatalf("hit msg = %q, want %q", hit.msg, hitMsgFull)
	}
	if got := attrString(t, hit, "reason"); got != "full_release" {
		t.Fatalf("reason = %q, want full_release", got)
	}
	if got := attrInt64(t, hit, "version"); got != 6 {
		t.Fatalf("version = %d, want 6", got)
	}
	if got := attrString(t, hit, "uid"); got != "u-full" {
		t.Fatalf("uid = %q, want u-full", got)
	}
	if got := attrString(t, hit, "trace_id"); got != "trace-full" {
		t.Fatalf("trace_id = %q, want trace-full", got)
	}
	requireNoAttrs(t, hit, "experiment_id", "group_id", "release_id")
}

// TestHitLog_ReasonFullRelease_FastPath_NoRPC: has_dynamic_resolution=false ⇒
// zero RPC AND reason=full_release on the emitted hit line. Fresh ctx + only
// this key queried in the link (memo convention) keeps the zero-RPC delta
// honest.
func TestHitLog_ReasonFullRelease_FastPath_NoRPC(t *testing.T) {
	h := newHarness(t)
	pb := makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 4, versions: map[int64]string{4: "full-v4"}},
	})
	setHDR(t, pb, "k", proto.Bool(false))
	h.cfgServer.SetPullSnapshot(pb)
	// Armed so a wrongful RPC would be counted; the assertion is on the delta.
	h.abServer.SetResponse("ns1", perGroupResponse(map[string]int64{"k": 4}))
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContext(context.Background(), "u-fast", nil)
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil || v != "full-v4" {
		t.Fatalf("GetConfig: got (%q,%v), want full-v4", v, err)
	}
	if delta := h.abServer.Calls("ns1") - before; delta != 0 {
		t.Fatalf("fast-path must issue ZERO RPC, got %d", delta)
	}

	hit := findConfigHit(t, rec, "k")
	if got := attrString(t, hit, "reason"); got != "full_release" {
		t.Fatalf("fast-path reason = %q, want full_release", got)
	}
	requireNoAttrs(t, hit, "experiment_id", "group_id", "release_id")
}

// TestHitLog_ReasonFullRelease_AbFullFallback_WarnHasTraceID: the ab hit points
// at a version missing from the local cache ⇒ value comes from the full
// release, so the hit line must say reason=full_release (design §4: the value
// really came from the full release), and the pre-existing WARN must carry the
// ctx trace_id.
func TestHitLog_ReasonFullRelease_AbFullFallback_WarnHasTraceID(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-only"}},
	}))
	// versionId 99 is NOT in the cache ⇒ ab→full fallback.
	h.abServer.SetResponse("ns1", perGroupResponse(map[string]int64{"k": 99}))
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	before := cli.Metrics().AbtestFallbackTotal("ns1")
	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-fb", nil, "trace-fb")
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil || v != "full-only" {
		t.Fatalf("GetConfig: got (%q,%v), want full-only", v, err)
	}
	// Delta, not absolute (no-op form 8).
	if delta := cli.Metrics().AbtestFallbackTotal("ns1") - before; delta != 1 {
		t.Fatalf("ab→full fallback metric delta = %d, want 1", delta)
	}

	hit := findConfigHit(t, rec, "k")
	if hit.msg != hitMsgFull {
		t.Fatalf("hit msg = %q, want %q (value came from full release)", hit.msg, hitMsgFull)
	}
	if got := attrString(t, hit, "reason"); got != "full_release" {
		t.Fatalf("ab→full fallback reason = %q, want full_release", got)
	}
	requireNoAttrs(t, hit, "experiment_id", "group_id", "release_id")

	warns := findRecordByMsg(rec, "tipsyabconfig: ab version missing in local cache; falling back to full")
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 ab→full WARN, got %d (%+v)", len(warns), rec.all())
	}
	if got := attrString(t, warns[0], "trace_id"); got != "trace-fb" {
		t.Fatalf("ab→full WARN trace_id = %q, want trace-fb", got)
	}
}

// TestHitLog_ReasonUnattributed_EmptyIdGroup is the log-axis half of the F3
// decoupling case (value-axis half: TestMerge_EmptyIdGroup_ValueStillWritten_F3
// — the fixture pairs an empty-id group with an attributed group, so this
// reason cannot be produced by a merge that writes nothing): empty
// experiment_id/group_id ⇒ reason=abtest_unattributed with ALL conditional
// fields omitted, while the paired attributed key still logs reason=experiment.
func TestHitLog_ReasonUnattributed_EmptyIdGroup(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"kA": {full: 1, versions: map[int64]string{1: "full-a", 2: "anon-a"}},
		"kB": {full: 1, versions: map[int64]string{1: "full-b", 3: "exp-b"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		Groups: []*abtestv1.ExperimentGroupResult{
			{
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
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	abctx := cli.NewAbtestContext(context.Background(), "u-anon", nil)
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "kA", "def"); err != nil || v != "anon-a" {
		t.Fatalf("kA: got (%q,%v), want anon-a (F3: value survives missing attribution)", v, err)
	}
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "kB", "def"); err != nil || v != "exp-b" {
		t.Fatalf("kB: got (%q,%v), want exp-b", v, err)
	}

	hitA := findConfigHit(t, rec, "kA")
	if got := attrString(t, hitA, "reason"); got != "abtest_unattributed" {
		t.Fatalf("empty-id group reason = %q, want abtest_unattributed", got)
	}
	if got := attrInt64(t, hitA, "version"); got != 2 {
		t.Fatalf("kA version = %d, want 2", got)
	}
	requireNoAttrs(t, hitA, "experiment_id", "group_id", "release_id")

	hitB := findConfigHit(t, rec, "kB")
	if got := attrString(t, hitB, "reason"); got != "experiment" {
		t.Fatalf("paired attributed group reason = %q, want experiment", got)
	}
	if got := attrString(t, hitB, "experiment_id"); got != "e1" {
		t.Fatalf("kB experiment_id = %q, want e1", got)
	}
}

// TestHitLog_ReasonUnattributed_Mock: a MockAbtestContext seeds only
// keyVersions (no attribution), so the hit logs reason=abtest_unattributed.
//
// COVERAGE HONESTY (design Testing Plan 1): this proves the unattributed
// fallback works for mock seeds; it says NOTHING about the merge writing
// attribution (a merge that never writes attribution also passes here). Do NOT
// count it toward attribution coverage (no-op form 3).
func TestHitLog_ReasonUnattributed_Mock(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full", 9: "ab9"}},
	}))
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	abctx := cli.MockAbtestContext("u-mock", map[string]map[string]int64{
		"ns1": {"k": 9},
	})
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil || v != "ab9" {
		t.Fatalf("GetConfig: got (%q,%v), want ab9", v, err)
	}

	hit := findConfigHit(t, rec, "k")
	if hit.msg != hitMsgAbtest {
		t.Fatalf("hit msg = %q, want %q", hit.msg, hitMsgAbtest)
	}
	if got := attrString(t, hit, "reason"); got != "abtest_unattributed" {
		t.Fatalf("mock seed reason = %q, want abtest_unattributed", got)
	}
	if got := attrInt64(t, hit, "version"); got != 9 {
		t.Fatalf("version = %d, want 9 (mock value stays effective)", got)
	}
	if got := attrString(t, hit, "uid"); got != "u-mock" {
		t.Fatalf("uid = %q, want u-mock", got)
	}
	requireNoAttrs(t, hit, "experiment_id", "group_id", "release_id")
}

// TestHitLog_RPCFailureDegrade_DeltaOne (design F8): the per-ns
// GetExperimentResult fails ⇒ GetConfig degrades to the full release,
// reason=full_release, the pre-existing degrade WARN still fires with the ctx
// trace_id, and abtestFallback moves by EXACTLY +1 (delta assertion — an
// absolute ">0" could be green for the wrong cause, no-op form 8). Fresh ctx:
// the failing fetch is memoised per ctx, so the observed call must be the
// first on its ctx.
func TestHitLog_RPCFailureDegrade_DeltaOne(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1"}},
	}))
	h.abServer.SetError("ns1", status.Error(codes.Unavailable, "down"))
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	before := cli.Metrics().AbtestFallbackTotal("ns1")
	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-deg", nil, "trace-deg")
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil || v != "full-v1" {
		t.Fatalf("GetConfig: got (%q,%v), want full-v1", v, err)
	}
	if delta := cli.Metrics().AbtestFallbackTotal("ns1") - before; delta != 1 {
		t.Fatalf("degrade abtestFallback delta = %d, want exactly 1", delta)
	}

	hit := findConfigHit(t, rec, "k")
	if hit.msg != hitMsgFull {
		t.Fatalf("hit msg = %q, want %q", hit.msg, hitMsgFull)
	}
	if got := attrString(t, hit, "reason"); got != "full_release" {
		t.Fatalf("degrade reason = %q, want full_release", got)
	}
	requireNoAttrs(t, hit, "experiment_id", "group_id", "release_id")

	warns := findRecordByMsg(rec, "tipsyabconfig: AbtestService.GetExperimentResult failed; falling back to full release")
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 degrade WARN (existing behaviour unchanged), got %d", len(warns))
	}
	if got := attrString(t, warns[0], "trace_id"); got != "trace-deg" {
		t.Fatalf("degrade WARN trace_id = %q, want trace-deg", got)
	}
}

// TestStaticLog_Unchanged (AC9): get_config_static's log line stays OUTSIDE the
// reason contract — msg and source unchanged, NO reason, NO uid (the static
// path has no user identity and is not an enrollment event; downstream keys on
// "has reason" to select enrollment events, design §4 contract boundary).
func TestStaticLog_Unchanged(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 10, versions: map[int64]string{10: "v10"}},
	}))
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cli := initClient(t, cfg)

	if v, ok := cli.GetConfigStatic("ns1", "k", "def"); !ok || v != "v10" {
		t.Fatalf("GetConfigStatic: got (%q,%v)", v, ok)
	}
	recs := findRecordByMsg(rec, "tipsyabconfig: get_config_static hit")
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 static hit record, got %d", len(recs))
	}
	st := recs[0]
	// Positive anchors first so the omit checks below cannot pass on a dead
	// or mis-shaped record.
	if got := attrString(t, st, "source"); got != "full_static" {
		t.Fatalf("static source = %q, want full_static", got)
	}
	if got := attrInt64(t, st, "version"); got != 10 {
		t.Fatalf("static version = %d, want 10", got)
	}
	requireNoAttrs(t, st, "reason", "uid", "experiment_id", "group_id", "release_id")
}

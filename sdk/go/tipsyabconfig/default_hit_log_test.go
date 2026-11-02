package tipsyabconfig

// Default-value hit log tests (platform task real-enrollment-dashboard, design
// part-c §C1 + §C6).
//
// CONTRACT UNDER TEST: when dynamic GetConfig / GetConfigDefault (and the typed
// accessors built on them) resolve NO value and return the caller's default,
// the SDK emits exactly ONE Info line per call:
//
//	msg  "tipsyabconfig: get_config hit (default)"
//	attrs exactly {ns, key, reason=default, default_cause, uid, trace_id}
//
// never version / experiment_id / group_id / release_id, and never the default
// value itself. default_cause is the FIRST matching row of the C1 table:
// unknown_key > fast_path > no_user > abtest_unavailable > ab_version_missing >
// no_full_release > full_release_value_missing. The return value stays the
// caller's default and Go keeps returning (default, nil). Error exits
// (nil client, nil ctx, namespace errors, ctx canceled while waiting) emit no
// line.
//
// CROSS-LANGUAGE ALIGNMENT: the cause scenarios are NOT defined here. They are
// read from the repo-shared list test/sdk-alignment/default_cause_scenarios.txt,
// which the Python and Java suites also execute row by row with the same
// expected default_cause. Edit that file, not a Go-local table.
//
// OBSERVATION: every assertion targets the EMITTED record captured by
// logRecorder (enrollment_fixtures_test.go). Negative "no line" assertions are
// always paired with a positive control on the same client and recorder, so a
// dead capture cannot make them pass.
//
// WHAT THESE TESTS DO NOT PROVE: fixtures are fabricated locally; a real
// server's empty-parameter-group payload is covered by the dev-e2e scenario
// (test/dev-e2e/default-hit), not here.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// defaultScenarioFile is the repo-shared scenario list, relative to this
// package directory (go test runs with the package dir as cwd).
const defaultScenarioFile = "../../../test/sdk-alignment/default_cause_scenarios.txt"

// c1Causes is the complete default_cause value set of part-c §C1.
var c1Causes = []string{
	"unknown_key",
	"fast_path",
	"no_user",
	"abtest_unavailable",
	"ab_version_missing",
	"no_full_release",
	"full_release_value_missing",
}

// defaultLineAttrs is the exact attr key set of a default line (part-c §C1).
var defaultLineAttrs = []string{"default_cause", "key", "ns", "reason", "trace_id", "uid"}

// defaultScenario is one row of the shared scenario list; see the file header
// there for each column's vocabulary.
type defaultScenario struct {
	id, expect, snapshot, full, hdr, ctx, uid, abtest string
}

// scenarioColumnValues is the closed vocabulary of each column. An unknown
// value fails the load, so a typo in the shared file can never fall through to
// a builder's default branch and silently test something else.
var scenarioColumnValues = map[string][]string{
	"snapshot": {"none", "key_absent", "key"},
	"full":     {"-", "absent", "zero", "uncached"},
	"hdr":      {"-", "absent", "true", "false"},
	"ctx":      {"new", "empty", "mock_seeded", "mock_unseeded"},
	"uid":      {"real", "empty", "zero"},
	"abtest":   {"ok_empty", "ok_empty_group", "ok_other", "ok_v0", "ok_uncached", "rpc_error", "timeout", "not_configured"},
}

func requireOneOf(t *testing.T, id, column, v string) {
	t.Helper()
	for _, ok := range scenarioColumnValues[column] {
		if v == ok {
			return
		}
	}
	t.Fatalf("scenario %s: column %s has unknown value %q (allowed %v)", id, column, v, scenarioColumnValues[column])
}

// loadDefaultScenarios parses the shared list and validates every row's
// vocabulary and cross-column consistency. A missing file fails (never skips).
func loadDefaultScenarios(t *testing.T) []defaultScenario {
	t.Helper()
	data, err := os.ReadFile(defaultScenarioFile)
	if err != nil {
		t.Fatalf("read shared scenario list %s: %v", defaultScenarioFile, err)
	}
	seen := map[string]bool{}
	var out []defaultScenario
	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 8 {
			t.Fatalf("%s:%d: want 8 columns, got %d: %q", defaultScenarioFile, n+1, len(f), line)
		}
		s := defaultScenario{id: f[0], expect: f[1], snapshot: f[2], full: f[3], hdr: f[4], ctx: f[5], uid: f[6], abtest: f[7]}
		if seen[s.id] {
			t.Fatalf("duplicate scenario id %q", s.id)
		}
		seen[s.id] = true
		requireCause(t, s.id, s.expect)
		requireOneOf(t, s.id, "snapshot", s.snapshot)
		requireOneOf(t, s.id, "full", s.full)
		requireOneOf(t, s.id, "hdr", s.hdr)
		requireOneOf(t, s.id, "ctx", s.ctx)
		requireOneOf(t, s.id, "uid", s.uid)
		requireOneOf(t, s.id, "abtest", s.abtest)
		if (s.snapshot == "key") != (s.full != "-") || (s.snapshot == "key") != (s.hdr != "-") {
			t.Fatalf("scenario %s: full/hdr must be set iff snapshot=key", s.id)
		}
		if s.ctx == "empty" && s.uid != "empty" {
			t.Fatalf("scenario %s: ctx=empty implies uid=empty", s.id)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		t.Fatalf("shared scenario list %s has no rows", defaultScenarioFile)
	}
	return out
}

func requireCause(t *testing.T, id, cause string) {
	t.Helper()
	for _, c := range c1Causes {
		if c == cause {
			return
		}
	}
	t.Fatalf("scenario %s: expect %q is not a C1 default_cause", id, cause)
}

func (s defaultScenario) uidValue() string {
	switch s.uid {
	case "empty":
		return ""
	case "zero":
		return "0"
	}
	return "u-sc"
}

// scenarioSnapshot builds the ns1 snapshot for a row (nil for snapshot=none).
// k2 is always present; k's versions are always {0: stray-v0, 5: v5}.
func scenarioSnapshot(s defaultScenario) *configv1.NamespaceSnapshot {
	if s.snapshot == "none" {
		return nil
	}
	snap := &configv1.NamespaceSnapshot{Namespace: "ns1", BusinessSnapshotSeq: 1, ExperimentSnapshotSeq: 1}
	snap.Keys = append(snap.Keys, &configv1.KeyState{
		Key: "k2", FullReleaseVersion: proto.Int64(3), Versions: map[int64]string{3: "full-k2"},
	})
	if s.snapshot == "key" {
		ks := &configv1.KeyState{Key: "k", Versions: map[int64]string{0: "stray-v0", 5: "v5"}}
		switch s.full {
		case "zero":
			ks.FullReleaseVersion = proto.Int64(0)
		case "uncached":
			ks.FullReleaseVersion = proto.Int64(7)
		}
		switch s.hdr {
		case "true":
			ks.HasDynamicResolution = proto.Bool(true)
		case "false":
			ks.HasDynamicResolution = proto.Bool(false)
		}
		snap.Keys = append(snap.Keys, ks)
	}
	return snap
}

// scenarioGroupResponse is one attributed CONFIG_VERSION group exp-sc/grp-sc.
func scenarioGroupResponse(params map[string]int64) *abtestv1.GetExperimentResultResponse {
	return &abtestv1.GetExperimentResultResponse{Groups: []*abtestv1.ExperimentGroupResult{{
		ExperimentId:   "exp-sc",
		GroupId:        "grp-sc",
		ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
		ParamsVersions: params,
	}}}
}

// armScenarioAbtest programs the fake abtest server (and the abtest-related
// Config knobs) for a row.
func armScenarioAbtest(h *testHarness, cfg *Config, s defaultScenario) {
	switch s.abtest {
	case "ok_empty":
		h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})
	case "ok_empty_group":
		h.abServer.SetResponse("ns1", scenarioGroupResponse(map[string]int64{}))
	case "ok_other":
		h.abServer.SetResponse("ns1", scenarioGroupResponse(map[string]int64{"k2": 3}))
	case "ok_v0":
		h.abServer.SetResponse("ns1", scenarioGroupResponse(map[string]int64{"k": 0}))
	case "ok_uncached":
		h.abServer.SetResponse("ns1", scenarioGroupResponse(map[string]int64{"k": 99}))
	case "timeout":
		// Content that would yield ab_version_missing if the timeout did not
		// fire, so a too-generous timeout cannot pass as abtest_unavailable.
		h.abServer.SetResponse("ns1", scenarioGroupResponse(map[string]int64{"k": 99}))
		h.abServer.SetDelay(500 * time.Millisecond)
		cfg.AbtestTimeout = 30 * time.Millisecond
	case "rpc_error":
		h.abServer.SetError("ns1", status.Error(codes.Unavailable, "down"))
	case "not_configured":
		cfg.AbtestServiceAddr = ""
	}
}

func scenarioContext(cli *Client, s defaultScenario) *AbtestContext {
	uid := s.uidValue()
	switch s.ctx {
	case "empty":
		return cli.EmptyAbtestContext()
	case "mock_seeded":
		return cli.MockAbtestContext(uid, map[string]map[string]int64{"ns1": {}})
	case "mock_unseeded":
		return cli.MockAbtestContext(uid, map[string]map[string]int64{})
	}
	return cli.NewAbtestContextWithTraceID(context.Background(), uid, nil, "tr-"+s.id)
}

// requireDefaultLine asserts rec is a well-formed default line: Info level,
// exact message, EXACT attr key set, exact values, the `reason=default
// default_cause=<cause>` adjacency the dev-e2e and SLS text parsing rely on,
// and — when defaultValue is non-empty — that the caller's default appears
// nowhere in the rendered line.
func requireDefaultLine(t *testing.T, rec capturedRecord, ns, key, cause, uid, traceID, defaultValue string) {
	t.Helper()
	if rec.msg != hitMsgDefault {
		t.Fatalf("msg = %q, want %q (record: %s)", rec.msg, hitMsgDefault, rec.text)
	}
	if rec.level != slog.LevelInfo {
		t.Fatalf("level = %v, want INFO", rec.level)
	}
	var keys []string
	for k := range rec.attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(defaultLineAttrs, ",") {
		t.Fatalf("default line attr set = %v, want exactly %v (no version/experiment_id/group_id/release_id/default value) — line: %s", keys, defaultLineAttrs, rec.text)
	}
	for k, want := range map[string]string{"ns": ns, "key": key, "reason": "default", "default_cause": cause, "uid": uid, "trace_id": traceID} {
		if got := attrString(t, rec, k); got != want {
			t.Fatalf("%s = %q, want %q — line: %s", k, got, want, rec.text)
		}
	}
	if !strings.Contains(rec.text, " reason=default default_cause="+cause+" ") {
		t.Fatalf("rendered line lacks the adjacent `reason=default default_cause=%s` pair: %s", cause, rec.text)
	}
	if defaultValue != "" && strings.Contains(rec.text, defaultValue) {
		t.Fatalf("default line leaks the caller's default value %q: %s", defaultValue, rec.text)
	}
}

// findDefaultHit returns the single get_config hit record for key and requires
// it to be the `(default)` variant (so no abtest/full line exists for key).
func findDefaultHit(t *testing.T, r *logRecorder, key string) capturedRecord {
	t.Helper()
	rec := findConfigHit(t, r, key)
	if rec.msg != hitMsgDefault {
		t.Fatalf("the only hit line for key %q is %q, want the default variant (line: %s)", key, rec.msg, rec.text)
	}
	return rec
}

// TestDefaultHitLog_ScenarioListShape guards the shared list itself: it must
// exercise every C1 cause and contain the four §C6 error-prone scenarios, with
// E4 being a pair that differs ONLY in the full-release id (absent vs 0).
func TestDefaultHitLog_ScenarioListShape(t *testing.T) {
	rows := loadDefaultScenarios(t)
	byID := map[string]defaultScenario{}
	causes := map[string]bool{}
	for _, s := range rows {
		byID[s.id] = s
		causes[s.expect] = true
	}
	for _, c := range c1Causes {
		if !causes[c] {
			t.Errorf("shared list has no row expecting %s", c)
		}
	}
	for _, id := range []string{"E1.rpc_ok_empty_param_group", "E2.fast_path_valid_uid", "E3.merged_version_zero", "E4.full_id_absent", "E4.full_id_zero"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("shared list is missing §C6 scenario %s", id)
		}
	}
	a, b := byID["E4.full_id_absent"], byID["E4.full_id_zero"]
	if a.full != "absent" || b.full != "zero" {
		t.Fatalf("E4 pair full = (%q,%q), want (absent,zero)", a.full, b.full)
	}
	a.id, b.id, a.full, b.full = "", "", "", ""
	if a != b {
		t.Fatalf("E4 pair must differ only in full: %+v vs %+v", a, b)
	}
}

// TestDefaultHitLog_SharedScenarios executes EVERY row of the shared list: one
// GetConfig call on a fresh client + ctx, then exactly one default line with
// the row's default_cause, the exact field set, and the caller's default
// returned with a nil error. The loop has no skip path (an unknown column value
// fails the load), so every row runs unless deselected with -run.
func TestDefaultHitLog_SharedScenarios(t *testing.T) {
	for _, s := range loadDefaultScenarios(t) {
		s := s
		t.Run(s.id, func(t *testing.T) {
			h := newHarness(t)
			if snap := scenarioSnapshot(s); snap != nil {
				h.cfgServer.SetPullSnapshot(snap)
			}
			cfg, rec := recordingConfig(h, []string{"ns1"})
			cfg.StartupFailOpen = true
			armScenarioAbtest(h, &cfg, s)
			cli := initClient(t, cfg)
			if s.snapshot == "none" && cli.cache.snapshot("ns1") != nil {
				t.Fatal("fixture: ns1 must have no snapshot for snapshot=none")
			}

			abctx := scenarioContext(cli, s)
			def := "DEF-" + s.id
			v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", def)
			if err != nil || v != def {
				t.Fatalf("GetConfig = (%q,%v), want (%q,nil): the caller's default and a nil error", v, err, def)
			}
			hit := findDefaultHit(t, rec, "k")
			requireDefaultLine(t, hit, "ns1", "k", s.expect, s.uidValue(), abctx.TraceID(), def)
			if n := countConfigHits(rec); n != 1 {
				t.Fatalf("one GetConfig call must emit exactly one hit line, got %d", n)
			}
		})
	}
}

// errExitFixture builds a client whose ns1 holds a no-full-release key k
// (abtest succeeds without k), so a valid call is a no_full_release default.
func errExitFixture(t *testing.T, mutate func(*Config)) (*testHarness, *Client, *logRecorder) {
	t.Helper()
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k":  {versions: map[int64]string{5: "v5"}},
		"k2": {full: 3, versions: map[int64]string{3: "full-k2"}},
	}))
	h.abServer.SetResponse("ns1", scenarioGroupResponse(map[string]int64{"k2": 3}))
	cfg, rec := recordingConfig(h, []string{"ns1"})
	if mutate != nil {
		mutate(&cfg)
	}
	return h, initClient(t, cfg), rec
}

// requirePositiveControl proves the recorder is live on this client: a valid
// call on a fresh ctx emits exactly one no_full_release default line, and it
// is the ONLY hit line captured (so the preceding error exit emitted none).
func requirePositiveControl(t *testing.T, cli *Client, rec *logRecorder) {
	t.Helper()
	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-ctl", nil, "tr-ctl")
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "DEF-ctl"); err != nil || v != "DEF-ctl" {
		t.Fatalf("positive control GetConfig = (%q,%v)", v, err)
	}
	requireDefaultLine(t, findDefaultHit(t, rec, "k"), "ns1", "k", "no_full_release", "u-ctl", "tr-ctl", "DEF-ctl")
	if n := countConfigHits(rec); n != 1 {
		t.Fatalf("expected only the positive-control hit line, got %d hit lines — the error exit emitted one: %+v", n, rec.all())
	}
}

// TestDefaultHitLog_ErrorExitsEmitNoLine covers every Go error exit listed in
// part-c §C1: each returns (default, err) with err unchanged and emits no hit
// line. There is deliberately no "closed non-nil client" case: Go has no such
// exit (see TestDefaultHitLog_AfterCloseStillEmits).
func TestDefaultHitLog_ErrorExitsEmitNoLine(t *testing.T) {
	t.Run("nil_client", func(t *testing.T) {
		// A nil *Client has no logger, so the observable contract is the
		// return pair and "no panic" — emitting here would nil-deref.
		var c *Client
		if v, err := c.GetConfig(context.Background(), nil, "ns1", "k", "DEF"); v != "DEF" || !errors.Is(err, ErrClosed) {
			t.Fatalf("nil client GetConfig = (%q,%v), want (DEF, ErrClosed)", v, err)
		}
		if v, err := c.GetConfigDefault(context.Background(), nil, "k", "DEF"); v != "DEF" || !errors.Is(err, ErrClosed) {
			t.Fatalf("nil client GetConfigDefault = (%q,%v), want (DEF, ErrClosed)", v, err)
		}
	})

	t.Run("nil_abtest_context", func(t *testing.T) {
		_, cli, rec := errExitFixture(t, nil)
		if v, err := cli.GetConfig(context.Background(), nil, "ns1", "k", "DEF"); v != "DEF" || !errors.Is(err, ErrAbtestContextMissing) {
			t.Fatalf("GetConfig(nil ctx) = (%q,%v), want (DEF, ErrAbtestContextMissing)", v, err)
		}
		if n := countConfigHits(rec); n != 0 {
			t.Fatalf("nil-ctx exit emitted %d hit line(s)", n)
		}
		requirePositiveControl(t, cli, rec)
	})

	t.Run("namespace_required", func(t *testing.T) {
		t.Setenv(defaultNamespaceEnvVar, "")
		_, cli, rec := errExitFixture(t, func(c *Config) { c.DefaultNamespace = "" })
		abctx := cli.NewAbtestContext(context.Background(), "u-1", nil)
		if v, err := cli.GetConfigDefault(context.Background(), abctx, "k", "DEF"); v != "DEF" || !errors.Is(err, ErrNamespaceRequired) {
			t.Fatalf("GetConfigDefault = (%q,%v), want (DEF, ErrNamespaceRequired)", v, err)
		}
		if v, err := cli.GetConfig(context.Background(), abctx, "", "k", "DEF"); v != "DEF" || !errors.Is(err, ErrNamespaceRequired) {
			t.Fatalf("GetConfig(ns=\"\") = (%q,%v), want (DEF, ErrNamespaceRequired)", v, err)
		}
		if n := countConfigHits(rec); n != 0 {
			t.Fatalf("namespace-required exit emitted %d hit line(s)", n)
		}
		requirePositiveControl(t, cli, rec)
	})

	t.Run("namespace_not_subscribed", func(t *testing.T) {
		_, cli, rec := errExitFixture(t, nil)
		abctx := cli.NewAbtestContext(context.Background(), "u-1", nil)
		if v, err := cli.GetConfig(context.Background(), abctx, "not-subscribed", "k", "DEF"); v != "DEF" || !errors.Is(err, ErrNamespaceNotSubscribed) {
			t.Fatalf("GetConfig(unsubscribed) = (%q,%v), want (DEF, ErrNamespaceNotSubscribed)", v, err)
		}
		if n := countConfigHits(rec); n != 0 {
			t.Fatalf("namespace-not-subscribed exit emitted %d hit line(s)", n)
		}
		requirePositiveControl(t, cli, rec)
	})

	t.Run("ctx_canceled_while_waiting_for_abtest", func(t *testing.T) {
		h, cli, rec := errExitFixture(t, func(c *Config) { c.AbtestTimeout = 5 * time.Second })
		// The RPC is in flight (server delay) when the caller's wait ctx is
		// already canceled: resultFor returns ctx.Err() (get_config.go
		// "ctx canceled / deadline — surface to caller").
		h.abServer.SetDelay(2 * time.Second)
		abctx := cli.NewAbtestContext(context.Background(), "u-1", nil)
		waitCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if v, err := cli.GetConfig(waitCtx, abctx, "ns1", "k", "DEF"); v != "DEF" || !errors.Is(err, context.Canceled) {
			t.Fatalf("GetConfig(canceled wait ctx) = (%q,%v), want (DEF, context.Canceled)", v, err)
		}
		if n := countConfigHits(rec); n != 0 {
			t.Fatalf("ctx-canceled exit emitted %d hit line(s)", n)
		}
		h.abServer.SetDelay(0)
		requirePositiveControl(t, cli, rec)
	})
}

// TestDefaultHitLog_AfterCloseStillEmits pins part-c §C1: Go has no "closed
// non-nil client" exit — after Close() GetConfig still resolves from the cache
// and, on the default branch, emits the line as usual with (default, nil).
// A fast-path key keeps the call free of any RPC on the closed connection.
func TestDefaultHitLog_AfterCloseStillEmits(t *testing.T) {
	h := newHarness(t)
	pb := makeSnapshot("ns1", 1, 1, map[string]typedKey{"k": {versions: map[int64]string{5: "v5"}}})
	setHDR(t, pb, "k", proto.Bool(false))
	h.cfgServer.SetPullSnapshot(pb)
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-closed", nil, "tr-closed")
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "DEF-closed"); err != nil || v != "DEF-closed" {
		t.Fatalf("GetConfig after Close = (%q,%v), want (DEF-closed, nil)", v, err)
	}
	requireDefaultLine(t, findDefaultHit(t, rec, "k"), "ns1", "k", "fast_path", "u-closed", "tr-closed", "DEF-closed")
}

// TestDefaultHitLog_GoLineByteExact pins the FULL rendered Go default line,
// including attr order. Go's format is the only one the downstream SLS ETL
// parses (part-c §C4), so its order is part of the contract. The order is the
// §C1 field-table order ns, key, reason, default_cause, uid, trace_id (main
// agent ruling 2026-10-31). One cause is enough: order does not depend on the
// cause; the other causes are covered by the set/value checks of the shared
// scenarios.
func TestDefaultHitLog_GoLineByteExact(t *testing.T) {
	_, cli, rec := errExitFixture(t, nil)
	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-exact", nil, "tr-exact")
	if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "DEF-exact"); err != nil || v != "DEF-exact" {
		t.Fatalf("GetConfig = (%q,%v), want (DEF-exact, nil)", v, err)
	}
	requireText(t, findDefaultHit(t, rec, "k"),
		`level=INFO msg="tipsyabconfig: get_config hit (default)" ns=ns1 key=k reason=default default_cause=no_full_release uid=u-exact trace_id=tr-exact`)
}

// TestDefaultHitLog_CauseHelperNilAbtestResult pins the nil arm of the cause
// helper: a nil abtest result on a non-fast-path key is abtest_unavailable,
// never "evaluated" (part-c §C1 judgement point 1: Go's nil must not be read
// as a successful evaluation). GetConfig cannot reach this arm today —
// resultFor never returns nil with a nil error, and nil on the fast path is
// classified earlier as fast_path — so it is asserted on the pure helper
// directly. A helper that dereferences nil panics; the recover turns that into
// an ordinary failure so the rest of the package still runs.
func TestDefaultHitLog_CauseHelperNilAbtestResult(t *testing.T) {
	snap := &NamespaceSnapshot{Namespace: "ns1", Keys: map[string]KeyState{
		"k": {Versions: map[int64]string{5: "v5"}},
	}}
	var got string
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("defaultCauseFor panicked on a nil abtest result: %v", r)
			}
		}()
		got = defaultCauseFor(snap, "k", nil, "u")
	}()
	if got != "abtest_unavailable" {
		t.Fatalf("defaultCauseFor(nil abresult) = %q, want abtest_unavailable", got)
	}
}

// TestDefaultHitLog_GetConfigDefaultEmits: the ns-optional entry point emits
// the same line, with ns = the RESOLVED namespace.
func TestDefaultHitLog_GetConfigDefaultEmits(t *testing.T) {
	_, cli, rec := errExitFixture(t, func(c *Config) { c.DefaultNamespace = "ns1" })
	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-dns", nil, "tr-dns")
	if v, err := cli.GetConfigDefault(context.Background(), abctx, "k", "DEF-dns"); err != nil || v != "DEF-dns" {
		t.Fatalf("GetConfigDefault = (%q,%v), want (DEF-dns, nil)", v, err)
	}
	requireDefaultLine(t, findDefaultHit(t, rec, "k"), "ns1", "k", "no_full_release", "u-dns", "tr-dns", "DEF-dns")
}

// TestDefaultHitLog_TypedAccessorsEmit: every dynamic typed accessor built on
// GetConfig emits exactly one default line on a no-value key and returns its
// typed default. A key whose value RESOLVES but fails to parse returns the
// typed default too, but logs its ordinary hit line and NO default line — a
// value was resolved (part-c §C1 "没有解析出值").
func TestDefaultHitLog_TypedAccessorsEmit(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k":   {versions: map[int64]string{5: "v5"}},
		"bad": {full: 4, versions: map[int64]string{4: "not-a-number"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	calls := []struct {
		name string
		call func(abctx *AbtestContext) error
	}{
		{"Bool", func(a *AbtestContext) error {
			v, err := cli.GetConfigBool(context.Background(), a, "ns1", "k", true)
			if err == nil && !v {
				return errors.New("want typed default true")
			}
			return err
		}},
		{"Int64", func(a *AbtestContext) error {
			v, err := cli.GetConfigInt64(context.Background(), a, "ns1", "k", 42)
			if err == nil && v != 42 {
				return errors.New("want typed default 42")
			}
			return err
		}},
		{"Float64", func(a *AbtestContext) error {
			v, err := cli.GetConfigFloat64(context.Background(), a, "ns1", "k", 1.5)
			if err == nil && v != 1.5 {
				return errors.New("want typed default 1.5")
			}
			return err
		}},
		{"String", func(a *AbtestContext) error {
			v, err := cli.GetConfigString(context.Background(), a, "ns1", "k", "DEF-str")
			if err == nil && v != "DEF-str" {
				return errors.New("want typed default DEF-str")
			}
			return err
		}},
		{"JSON", func(a *AbtestContext) error {
			out := map[string]any{"untouched": true}
			err := cli.GetConfigJSON(context.Background(), a, "ns1", "k", &out)
			if err == nil && out["untouched"] != true {
				return errors.New("want out untouched")
			}
			return err
		}},
	}
	for i, c := range calls {
		trace := "tr-typed-" + c.name
		abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-typed", nil, trace)
		if err := c.call(abctx); err != nil {
			t.Fatalf("GetConfig%s: %v", c.name, err)
		}
		var defaults []capturedRecord
		for _, r := range rec.all() {
			if r.msg == hitMsgDefault {
				defaults = append(defaults, r)
			}
		}
		if len(defaults) != i+1 {
			t.Fatalf("after GetConfig%s: %d default lines, want %d (one per call)", c.name, len(defaults), i+1)
		}
		requireDefaultLine(t, defaults[i], "ns1", "k", "no_full_release", "u-typed", trace, "")
	}

	before := countConfigHits(rec)
	abctx := cli.NewAbtestContext(context.Background(), "u-typed", nil)
	if v, err := cli.GetConfigInt64(context.Background(), abctx, "ns1", "bad", 7); err == nil || v != 7 {
		t.Fatalf("GetConfigInt64(bad) = (%d,%v), want (7, parse error)", v, err)
	}
	if n := countConfigHits(rec) - before; n != 1 {
		t.Fatalf("parse-failure call emitted %d hit lines, want exactly 1 (its full hit)", n)
	}
	if hit := findConfigHit(t, rec, "bad"); hit.msg != hitMsgFull {
		t.Fatalf("parse-failure line = %q, want the full hit line, not a default line", hit.msg)
	}
}

// TestDefaultHitLog_EveryCallEmits: no sampling, no dedup (part-c §C1) — three
// calls on the SAME ctx emit three lines, while the abtest RPC stays
// memoised at one.
func TestDefaultHitLog_EveryCallEmits(t *testing.T) {
	h, cli, rec := errExitFixture(t, nil)
	before := h.abServer.Calls("ns1")
	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-rep", nil, "tr-rep")
	for i := 0; i < 3; i++ {
		if v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "DEF-rep"); err != nil || v != "DEF-rep" {
			t.Fatalf("call %d: (%q,%v)", i, v, err)
		}
	}
	var n int
	for _, r := range rec.all() {
		if r.msg == hitMsgDefault {
			requireDefaultLine(t, r, "ns1", "k", "no_full_release", "u-rep", "tr-rep", "DEF-rep")
			n++
		}
	}
	if n != 3 {
		t.Fatalf("3 calls emitted %d default lines, want 3", n)
	}
	if d := h.abServer.Calls("ns1") - before; d != 1 {
		t.Fatalf("abtest RPC count = %d, want 1 (memoised per ctx)", d)
	}
}

// TestDefaultHitLog_StaticAndGetAllDoNotEmit: GetConfigStatic and
// GetAllConfigs are out of scope (part-c §C1) and must not grow a default
// line. The trailing GetConfig is the positive control.
func TestDefaultHitLog_StaticAndGetAllDoNotEmit(t *testing.T) {
	_, cli, rec := errExitFixture(t, nil)
	if v, ok := cli.GetConfigStatic("ns1", "k", "DEF-s"); ok || v != "DEF-s" {
		t.Fatalf("GetConfigStatic miss = (%q,%v)", v, ok)
	}
	all, err := cli.GetAllConfigs(context.Background(), cli.NewAbtestContext(context.Background(), "u-all", nil), "ns1")
	if err != nil {
		t.Fatalf("GetAllConfigs: %v", err)
	}
	if _, present := all["k"]; present {
		t.Fatalf("GetAllConfigs must omit the no-value key, got %v", all)
	}
	if n := countConfigHits(rec); n != 0 {
		t.Fatalf("static / get-all emitted %d get_config hit line(s)", n)
	}
	requirePositiveControl(t, cli, rec)
}

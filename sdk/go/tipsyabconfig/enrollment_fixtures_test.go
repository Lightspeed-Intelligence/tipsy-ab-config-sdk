package tipsyabconfig

// Shared test fixtures + observation helpers for the actual-enrollment-log
// design (getConfig 实际入组日志, design.md §3/§4 + Testing Plan).
//
// TEST CONVENTION (design Testing Plan, r6 — applies package-wide):
// any case that needs to observe "second and later call" behaviour (fast-path
// "no RPC", RPC-failure degrade deltas, request-shape capture, ...) MUST
// construct a FRESH AbtestContext for the observed call. All three SDKs memo
// the (request, namespace) pair (Go: abtest_context.go ensureFetch — "AT MOST
// ONE GetExperimentResult RPC per ns per request link"), so on a reused ctx the
// second call onwards observes only the memoised value and the assertion passes
// trivially (no-op form 7).
//
// FIXTURE MIGRATION NOTE (design Important Details F2): the internal per-ns
// fetch now requests display_type=EACH_EXPERIMENT_GROUP, and the SDK merges
// groups[]/gray_hits[] locally; a response carrying only config_flat_kv is no
// longer consumed by the internal path. perGroupResponse transposes the legacy
// flat fixtures one key at a time — expected key→value resolution stays
// byte-identical. The pre-existing flat fixtures contain no conflicting keys
// (design r4 sweep), so a single group carrying all keys is a faithful 1:1
// transposition; do NOT use this helper to construct conflict expectations.

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// perGroupResponse transposes a flat key→versionId map into the per-group
// (EACH_EXPERIMENT_GROUP) response shape: one CONFIG_VERSION experiment group
// carrying every key, with non-empty experiment/group ids so the merged
// attribution is a normal "experiment" hit. An empty map transposes to the
// empty response (no groups, no gray_hits) — same as the old empty flat map.
func perGroupResponse(kv map[string]int64) *abtestv1.GetExperimentResultResponse {
	if len(kv) == 0 {
		return &abtestv1.GetExperimentResultResponse{}
	}
	cp := make(map[string]int64, len(kv))
	for k, v := range kv {
		cp[k] = v
	}
	return &abtestv1.GetExperimentResultResponse{
		Groups: []*abtestv1.ExperimentGroupResult{{
			ExperimentId:   "exp-fixture",
			GroupId:        "grp-fixture",
			ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
			ParamsVersions: cp,
		}},
	}
}

// capturedRecord is one slog record captured by logRecorder: message, level and
// a flat attr map (Value.Any() per key).
type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// logRecorder is a minimal slog.Handler that records every log line the Client
// emits, at every level (Debug included). Wire it via cfg.Logger =
// slog.New(recorder).
//
// WithAttrs/WithGroup return the receiver unchanged (pre-bound attrs would be
// dropped) — acceptable here because the SDK logs via direct
// logger.Info/Warn/Debug calls and never pre-binds attrs.
type logRecorder struct {
	mu   sync.Mutex
	recs []capturedRecord
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := make(map[string]any, rec.NumAttrs())
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, capturedRecord{level: rec.Level, msg: rec.Message, attrs: attrs})
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

// all returns a copy of the captured records.
func (r *logRecorder) all() []capturedRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedRecord, len(r.recs))
	copy(out, r.recs)
	return out
}

// recordingConfig is baseConfig with a fresh logRecorder attached, so tests can
// assert the EMITTED log lines (design Testing Plan: reason assertions must be
// made on emitted records, never on enum/constant declarations — no-op form 6).
func recordingConfig(h *testHarness, namespaces []string) (Config, *logRecorder) {
	rec := &logRecorder{}
	cfg := h.baseConfig(namespaces)
	cfg.Logger = slog.New(rec)
	return cfg, rec
}

const (
	hitMsgAbtest = "tipsyabconfig: get_config hit (abtest)"
	hitMsgFull   = "tipsyabconfig: get_config hit (full)"
)

// findConfigHit returns THE single get_config hit record (full or abtest
// variant) whose "key" attr equals key. Zero or multiple matches fatal: zero
// means the observation point is dead (a silently-mis-scoped recorder must not
// let the omit-assertions below pass trivially), multiple means the fixture is
// ambiguous.
func findConfigHit(t *testing.T, r *logRecorder, key string) capturedRecord {
	t.Helper()
	var out []capturedRecord
	for _, rec := range r.all() {
		if (rec.msg == hitMsgAbtest || rec.msg == hitMsgFull) && rec.attrs["key"] == key {
			out = append(out, rec)
		}
	}
	if len(out) != 1 {
		t.Fatalf("expected exactly 1 get_config hit record for key %q, got %d (records=%+v)", key, len(out), r.all())
	}
	return out[0]
}

// findRecordByMsg returns all captured records with the given message.
func findRecordByMsg(r *logRecorder, msg string) []capturedRecord {
	var out []capturedRecord
	for _, rec := range r.all() {
		if rec.msg == msg {
			out = append(out, rec)
		}
	}
	return out
}

// attrString fatals unless rec carries a string attr key with a non-nil value,
// and returns it.
func attrString(t *testing.T, rec capturedRecord, key string) string {
	t.Helper()
	v, ok := rec.attrs[key]
	if !ok {
		t.Fatalf("record %q missing attr %q (attrs=%v)", rec.msg, key, rec.attrs)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("record %q attr %q is %T (%v), want string", rec.msg, key, v, v)
	}
	return s
}

// attrInt64 fatals unless rec carries an int64-typed attr key, and returns it.
// slog normalises int/int64 attr values to int64 via Value.Any().
func attrInt64(t *testing.T, rec capturedRecord, key string) int64 {
	t.Helper()
	v, ok := rec.attrs[key]
	if !ok {
		t.Fatalf("record %q missing attr %q (attrs=%v)", rec.msg, key, rec.attrs)
	}
	i, ok := v.(int64)
	if !ok {
		t.Fatalf("record %q attr %q is %T (%v), want int64", rec.msg, key, v, v)
	}
	return i
}

// requireNoAttrs asserts the record OMITS every listed key entirely (design §4
// SLS contract: absent conditional fields are omitted, never empty-string
// placeholders). Callers must pair this with positive attr assertions on the
// same record so a dead capture cannot green these negative checks.
func requireNoAttrs(t *testing.T, rec capturedRecord, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, present := rec.attrs[k]; present {
			t.Fatalf("record %q must OMIT attr %q, but it is present with value %v (omit semantics, design §4)", rec.msg, k, v)
		}
	}
}

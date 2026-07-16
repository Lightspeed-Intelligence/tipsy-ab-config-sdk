package tipsyabconfig

// Heartbeat handling on the SDK consumer side (design §Proposed Design 3,
// §Testing Plan "单元(SDK)" first bullet, Acceptance Criteria 3).
//
// A server-emitted Heartbeat is a liveness-only frame: it MUST NOT touch the
// cache, MUST NOT advance knownSeqs, and MUST NOT be counted as a config
// change event (SubscribeEventReceivedTotal). Old SDKs land it in the default
// branch (forward-compat); this SDK adds an explicit no-op branch. These are
// pure white-box unit tests on handleEvent — we build a minimal Client with no
// background loops so the "counts unchanged" assertions are deterministic and
// race-free (no concurrent pull/subscribe goroutine can perturb the counters).

import (
	"io"
	"log/slog"
	"testing"

	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// newHandleEventClient builds the smallest Client that handleEvent needs
// (metrics + cache + logger), with no background goroutines running. This keeps
// the metric/cache assertions deterministic — nothing else mutates state.
func newHandleEventClient() *Client {
	m := newMetrics()
	return &Client{
		metrics: m,
		cache:   newConfigCache(m),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestHandleEvent_Heartbeat_NoCacheChange asserts a Heartbeat event is a pure
// no-op: it does not panic, does not change cached payload, does not advance
// knownSeqs, and is not counted as a subscribe (config-change) event.
func TestHandleEvent_Heartbeat_NoCacheChange(t *testing.T) {
	cli := newHandleEventClient()

	// Seed a known cache + seq state.
	cli.cache.applyProto(makeSnapshot("ns1", 5, 7, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 3, versions: map[int64]string{3: "v3"}}}))

	// Baseline snapshot of everything a heartbeat must leave untouched.
	baseSeqs := cli.cache.knownSeqs([]string{"ns1"})
	baseFull, baseFullOK := cli.cache.fullReleaseVersion("ns1", "k")
	baseEvents := cli.metrics.SubscribeEventReceivedTotal("ns1")

	// A heartbeat with a real timestamp: must be ignored entirely.
	cli.handleEvent(&configv1.ConfigUpdateEvent{
		Payload: &configv1.ConfigUpdateEvent_Heartbeat{
			Heartbeat: &configv1.Heartbeat{UnixNanos: 123},
		},
	})

	// A heartbeat whose inner message is nil must also not panic (the branch
	// must not dereference the payload — it carries no config).
	cli.handleEvent(&configv1.ConfigUpdateEvent{
		Payload: &configv1.ConfigUpdateEvent_Heartbeat{Heartbeat: nil},
	})

	// knownSeqs unchanged.
	gotSeqs := cli.cache.knownSeqs([]string{"ns1"})
	if gotSeqs["ns1"].BusinessSnapshotSeq != baseSeqs["ns1"].BusinessSnapshotSeq ||
		gotSeqs["ns1"].ExperimentSnapshotSeq != baseSeqs["ns1"].ExperimentSnapshotSeq {
		t.Fatalf("heartbeat advanced knownSeqs: before=%+v after=%+v",
			baseSeqs["ns1"], gotSeqs["ns1"])
	}
	// Cached payload unchanged.
	gotFull, gotFullOK := cli.cache.fullReleaseVersion("ns1", "k")
	if gotFull != baseFull || gotFullOK != baseFullOK {
		t.Fatalf("heartbeat changed cached value: before=(%d,%v) after=(%d,%v)",
			baseFull, baseFullOK, gotFull, gotFullOK)
	}
	// Not counted as a config change event.
	if got := cli.metrics.SubscribeEventReceivedTotal("ns1"); got != baseEvents {
		t.Fatalf("heartbeat counted as subscribe event: before=%d after=%d", baseEvents, got)
	}
}

// TestHandleEvent_HeartbeatThenSnapshot_SnapshotStillApplies is a regression
// guard: heartbeats interleaved before/after a real Snapshot must not suppress
// or undo the Snapshot's applyProto, and only the Snapshot is counted.
func TestHandleEvent_HeartbeatThenSnapshot_SnapshotStillApplies(t *testing.T) {
	cli := newHandleEventClient()

	// Initial cache state (biz/exp = 1, full = 1).
	cli.cache.applyProto(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v1"}}}))

	// Heartbeat before the snapshot: no-op.
	cli.handleEvent(&configv1.ConfigUpdateEvent{
		Payload: &configv1.ConfigUpdateEvent_Heartbeat{Heartbeat: &configv1.Heartbeat{UnixNanos: 1}},
	})

	// A real snapshot with advanced seqs must still apply normally.
	cli.handleEvent(&configv1.ConfigUpdateEvent{
		Payload: &configv1.ConfigUpdateEvent_Snapshot{
			Snapshot: makeSnapshot("ns1", 5, 5, map[string]struct {
				full     int64
				versions map[int64]string
			}{"k": {full: 2, versions: map[int64]string{2: "v2"}}}),
		},
	})

	// Heartbeat after the snapshot: still a no-op, must not roll the apply back.
	cli.handleEvent(&configv1.ConfigUpdateEvent{
		Payload: &configv1.ConfigUpdateEvent_Heartbeat{Heartbeat: &configv1.Heartbeat{UnixNanos: 2}},
	})

	// Snapshot took effect: full=2, seqs=(5,5).
	if v, ok := cli.cache.fullReleaseVersion("ns1", "k"); !ok || v != 2 {
		t.Fatalf("snapshot not applied around heartbeats: got (%d,%v), want (2,true)", v, ok)
	}
	seqs := cli.cache.knownSeqs([]string{"ns1"})
	if seqs["ns1"].BusinessSnapshotSeq != 5 || seqs["ns1"].ExperimentSnapshotSeq != 5 {
		t.Fatalf("snapshot seq not applied: %+v", seqs["ns1"])
	}
	// Exactly one config event counted (the snapshot); the two heartbeats not.
	if got := cli.metrics.SubscribeEventReceivedTotal("ns1"); got != 1 {
		t.Fatalf("subscribe event count = %d, want 1 (heartbeats must not count)", got)
	}
}

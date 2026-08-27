package tipsyabconfig

// Tests for issue #15 (design-phase2 §#15, Go): context cancellation is an
// EXPECTED termination, not a fault. The grpc-go status error for a cancelled
// ctx does NOT wrap the context.Canceled sentinel (grpc rpc_util.go
// errContextCanceled is a bare status error whose Is() only compares against
// other *status.Error), so every `errors.Is(err, context.Canceled)` in the SDK
// was blind to real gRPC cancellations. The fix routes both shapes through
// isContextCanceled and, on a hit:
//
//   - subscribe / periodic pull: silent return — no ERROR log, no
//     subscribeDisc/pullFailure inc, no BackgroundErrorEvent (user decision
//     2026-08-27: ctx-cancel fires NO event, so SubscribeConnected is never
//     falsely flipped).
//   - abtest per-ns fetch: Info log (ns + trace_id) + abtestCanceled inc
//     (new counter, per user decision: observability moves OUT of
//     abtestFallback instead of being erased), abtestFallback NOT inc'd,
//     return semantics unchanged (degrade to full release).
//
// Real (non-cancel) errors must keep the pre-#15 behaviour bit-for-bit — the
// regression halves of these tests pin that.
//
// TEST CONVENTION (package-wide, see enrollment_fixtures_test.go): every
// observed abtest call constructs a FRESH AbtestContext — the per-ns result is
// memoised per ctx, so a reused ctx observes only the memo (no-op form 7).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// abtestCanceledInfoMsg is the Info line the abtest per-ns fetch emits on a
// ctx-cancel hit (design-phase2 §#15: Info with ns/trace_id, replacing the
// degrade WARN for this shape only).
const abtestCanceledInfoMsg = "tipsyabconfig: AbtestService.GetExperimentResult canceled; falling back to full release"

// abtestDegradeWarnMsg is the pre-existing real-error degrade WARN (must stay
// untouched for non-cancel errors).
const abtestDegradeWarnMsg = "tipsyabconfig: AbtestService.GetExperimentResult failed; falling back to full release"

// subscribeErrorMsg / pullErrorMsg are the pre-existing ERROR lines that must
// NOT fire on a ctx-cancel.
const subscribeErrorMsg = "tipsyabconfig: Subscribe stream error; reconnecting"
const pullErrorMsg = "tipsyabconfig: periodic PullAll failed"

// grpcCanceledErr is the wire shape grpc-go produces for a cancelled ctx: a
// status error carrying codes.Canceled that does NOT wrap context.Canceled.
func grpcCanceledErr() error {
	return status.Error(codes.Canceled, "context canceled")
}

// TestIsContextCanceled_Table is the helper truth table from design-phase2
// §#15 测试: bare sentinel / wrapped sentinel / grpc status Canceled / other
// codes / nil. DeadlineExceeded (both shapes) is explicitly OUT of scope
// (issue 待确认取舍 3: a real timeout stays an error).
func TestIsContextCanceled_Table(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare sentinel", context.Canceled, true},
		{"wrapped sentinel", fmt.Errorf("rpc: %w", context.Canceled), true},
		{"grpc status Canceled (does not wrap sentinel)", grpcCanceledErr(), true},
		{"grpc status Unavailable", status.Error(codes.Unavailable, "down"), false},
		{"grpc status DeadlineExceeded", status.Error(codes.DeadlineExceeded, "late"), false},
		{"grpc status Unknown", status.Error(codes.Unknown, "boom"), false},
		{"context.DeadlineExceeded sentinel", context.DeadlineExceeded, false},
		{"plain error", errors.New("boom"), false},
		{"io.EOF", io.EOF, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Guard the fixture's core premise so the table can never rot into
			// asserting the wrong thing: the grpc status shape really is
			// invisible to errors.Is against the sentinel.
			if tc.name == "grpc status Canceled (does not wrap sentinel)" &&
				errors.Is(tc.err, context.Canceled) {
				t.Fatal("fixture premise broken: grpc status error now wraps context.Canceled — re-check the issue root cause")
			}
			if got := isContextCanceled(tc.err); got != tc.want {
				t.Fatalf("isContextCanceled(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestSubscribe_CtxCancelStatus_Silent: the Subscribe stream dying with the
// grpc Canceled status shape is a silent exit — no subscribeDisc inc, no ERROR
// log, no "subscribe" BackgroundErrorEvent, and NO reconnect attempt (the
// pre-fix behaviour was all four, with the loop only exiting a round later via
// rootCtx.Err()). SubscribeCalls > 1 inside the observation window would mean
// the hit was treated as "clean EOF, reconnect immediately" — also a bug.
func TestSubscribe_CtxCancelStatus_Silent(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.cfgServer.SetSubscribeErrFn(grpcCanceledErr)

	sink := &recordingErrSink{}
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cfg.PullInterval = time.Hour // keep periodic_pull noise out of the sink
	cfg.OnBackgroundError = sink.cb
	cli := initClient(t, cfg)

	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("subscribe never attached")
	}
	// Push one frame: the fake server sends it, then errFn returns the
	// canceled-status error and the stream dies. Waiting for the cache to
	// reflect the frame pins "the error has reached the client" (the error
	// follows the frame on the same stream).
	h.cfgServer.PushSnapshot(makeSnapshot("ns1", 5, 5, nil))
	if !waitFor(t, 2*time.Second, func() bool {
		s := cli.cache.snapshot("ns1")
		return s != nil && s.BusinessSnapshotSeq == 5
	}) {
		t.Fatal("pushed frame never reached the client")
	}

	// Absence window: on the pre-fix (fault) path every one of these fires
	// within milliseconds of the Recv error; on a wrong "clean EOF" reading
	// the reconnect lands immediately. waitFor returning true = violation.
	violated := func() bool {
		return cli.Metrics().SubscribeDisconnectTotal("ns1") != 0 ||
			len(sink.byPhase("subscribe")) != 0 ||
			len(findRecordByMsg(rec, subscribeErrorMsg)) != 0 ||
			h.cfgServer.SubscribeCalls() > 1
	}
	if waitFor(t, 700*time.Millisecond, violated) {
		t.Fatalf("ctx-cancel on subscribe was treated as a fault: disc=%d, events=%d, errorLogs=%d, subscribeCalls=%d",
			cli.Metrics().SubscribeDisconnectTotal("ns1"),
			len(sink.byPhase("subscribe")),
			len(findRecordByMsg(rec, subscribeErrorMsg)),
			h.cfgServer.SubscribeCalls())
	}
}

// TestSubscribe_RealError_StillReports pins the regression half: a non-cancel
// stream error keeps the full pre-#15 fault treatment — subscribeDisc inc,
// ERROR log, and a Phase=="subscribe" BackgroundErrorEvent. (Reconnect itself
// is covered by TestSubscribe_ErrorReconnects.)
func TestSubscribe_RealError_StillReports(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.cfgServer.SetSubscribeErrFn(func() error {
		return status.Error(codes.Unavailable, "kicked")
	})

	sink := &recordingErrSink{}
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cfg.PullInterval = time.Hour
	cfg.OnBackgroundError = sink.cb
	cli := initClient(t, cfg)

	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("subscribe never attached")
	}
	h.cfgServer.PushSnapshot(makeSnapshot("ns1", 5, 5, nil))

	if !waitFor(t, 3*time.Second, func() bool {
		return cli.Metrics().SubscribeDisconnectTotal("ns1") > 0
	}) {
		t.Fatal("real subscribe error did not bump subscribeDisc")
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(sink.byPhase("subscribe")) >= 1 }) {
		t.Fatal("real subscribe error did not fire a subscribe BackgroundErrorEvent")
	}
	if len(findRecordByMsg(rec, subscribeErrorMsg)) == 0 {
		t.Fatal("real subscribe error did not log the reconnect ERROR line")
	}
}

// TestPullLoop_CtxCancelStatus_Silent: a periodic PullAll failing with the
// grpc Canceled status shape exits the loop silently — no pullFailure inc, no
// ERROR log, no "periodic_pull" event, and no further pull ticks (the loop
// returns; pre-fix it kept ticking and reporting until rootCtx caught up).
func TestPullLoop_CtxCancelStatus_Silent(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	sink := &recordingErrSink{}
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cfg.OnBackgroundError = sink.cb // PullInterval stays 50ms from baseConfig
	cli := initClient(t, cfg)

	// Let the startup pull settle, then flip the server to canceled-status
	// errors: the next periodic tick hits it.
	base := h.cfgServer.PullCalls()
	h.cfgServer.SetPullError(grpcCanceledErr())
	if !waitFor(t, 3*time.Second, func() bool { return h.cfgServer.PullCalls() > base }) {
		t.Fatal("no periodic pull tick reached the erroring server")
	}

	violated := func() bool {
		return cli.Metrics().PullFailureTotal("ns1") != 0 ||
			len(sink.byPhase("periodic_pull")) != 0 ||
			len(findRecordByMsg(rec, pullErrorMsg)) != 0
	}
	if waitFor(t, 500*time.Millisecond, violated) {
		t.Fatalf("ctx-cancel on periodic pull was treated as a fault: pullFailure=%d, events=%d, errorLogs=%d",
			cli.Metrics().PullFailureTotal("ns1"),
			len(sink.byPhase("periodic_pull")),
			len(findRecordByMsg(rec, pullErrorMsg)))
	}
	// Loop-exit half: after the first canceled tick the loop must RETURN, not
	// continue. With the 50ms interval a still-alive loop racks up ~10 more
	// server calls across 500ms; allow 1 for a tick in flight at observation.
	c1 := h.cfgServer.PullCalls()
	time.Sleep(500 * time.Millisecond)
	if c2 := h.cfgServer.PullCalls(); c2 > c1+1 {
		t.Fatalf("pull loop kept running after ctx-cancel: calls %d -> %d", c1, c2)
	}
}

// TestPullLoop_RealError_StillReports pins the regression half for the pull
// loop: a non-cancel periodic failure keeps pullFailure inc + ERROR log + a
// Phase=="periodic_pull" event carrying the namespace, and the loop keeps
// ticking (no silent exit).
func TestPullLoop_RealError_StillReports(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	sink := &recordingErrSink{}
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cfg.OnBackgroundError = sink.cb
	cli := initClient(t, cfg)

	base := h.cfgServer.PullCalls()
	h.cfgServer.SetPullError(status.Error(codes.Unavailable, "down"))

	if !waitFor(t, 3*time.Second, func() bool {
		return cli.Metrics().PullFailureTotal("ns1") > 0
	}) {
		t.Fatal("real periodic pull error did not bump pullFailure")
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(sink.byPhase("periodic_pull")) >= 1 }) {
		t.Fatal("real periodic pull error did not fire a periodic_pull event")
	}
	if ev := sink.byPhase("periodic_pull")[0]; ev.Namespace != "ns1" {
		t.Fatalf("periodic_pull event Namespace = %q, want ns1", ev.Namespace)
	}
	if len(findRecordByMsg(rec, pullErrorMsg)) == 0 {
		t.Fatal("real periodic pull error did not log the ERROR line")
	}
	// The loop must keep ticking on real errors (only ctx-cancel exits).
	c1 := h.cfgServer.PullCalls()
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.PullCalls() > c1 }) {
		t.Fatalf("pull loop stopped ticking after a real error (calls stuck at %d, base %d)", c1, base)
	}
}

// TestAbtest_CtxCancelStatus_InfoAndCanceledCounter: the per-ns
// GetExperimentResult failing with the grpc Canceled status shape emits the
// Info line (ns + trace_id), bumps abtestCanceled by exactly 1, does NOT touch
// abtestFallback, does NOT emit the degrade WARN — and the caller-visible
// return semantics stay identical to any other per-ns failure: GetConfig
// degrades to the full release value.
func TestAbtest_CtxCancelStatus_InfoAndCanceledCounter(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1"}},
	}))
	h.abServer.SetError("ns1", grpcCanceledErr())
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	beforeCanceled := cli.Metrics().AbtestCanceledTotal("ns1")
	beforeFallback := cli.Metrics().AbtestFallbackTotal("ns1")

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-cxl", nil, "trace-cxl")
	v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil || v != "full-v1" {
		t.Fatalf("GetConfig: got (%q,%v), want (full-v1,nil) — degrade semantics must not change", v, err)
	}

	if delta := cli.Metrics().AbtestCanceledTotal("ns1") - beforeCanceled; delta != 1 {
		t.Fatalf("abtestCanceled delta = %d, want exactly 1", delta)
	}
	if delta := cli.Metrics().AbtestFallbackTotal("ns1") - beforeFallback; delta != 0 {
		t.Fatalf("abtestFallback delta = %d, want 0 (ctx-cancel moved OUT of fallback)", delta)
	}

	infos := findRecordByMsg(rec, abtestCanceledInfoMsg)
	if len(infos) != 1 {
		t.Fatalf("expected exactly 1 canceled Info record, got %d (records=%+v)", len(infos), rec.all())
	}
	if infos[0].level != slog.LevelInfo {
		t.Fatalf("canceled record level = %v, want Info", infos[0].level)
	}
	if got := attrString(t, infos[0], "ns"); got != "ns1" {
		t.Fatalf("canceled Info ns = %q, want ns1", got)
	}
	if got := attrString(t, infos[0], "trace_id"); got != "trace-cxl" {
		t.Fatalf("canceled Info trace_id = %q, want trace-cxl", got)
	}
	if warns := findRecordByMsg(rec, abtestDegradeWarnMsg); len(warns) != 0 {
		t.Fatalf("ctx-cancel must not emit the degrade WARN, got %d", len(warns))
	}
}

// TestAbtest_ParentCtxCanceled_Sentinel covers the OTHER shape of the same
// hit: the caller's request ctx (captured at AbtestContext construction) is
// already cancelled, so the RPC fails client-side before reaching the server
// (bare context.Canceled sentinel or a locally-synthesised Canceled status,
// depending on grpc version — isContextCanceled must catch either). Same
// contract: canceled +1, fallback +0, no WARN, degrade to full.
func TestAbtest_ParentCtxCanceled_Sentinel(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1"}},
	}))
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	beforeCanceled := cli.Metrics().AbtestCanceledTotal("ns1")
	beforeFallback := cli.Metrics().AbtestFallbackTotal("ns1")
	beforeCalls := h.abServer.Calls("ns1")

	pctx, cancel := context.WithCancel(context.Background())
	cancel() // request already aborted before the fetch starts
	abctx := cli.NewAbtestContextWithTraceID(pctx, "u-cxl2", nil, "trace-cxl2")
	// The WAIT ctx is alive — only the fetch's parent ctx is dead, so the
	// degrade result comes back synchronously.
	v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil || v != "full-v1" {
		t.Fatalf("GetConfig: got (%q,%v), want (full-v1,nil)", v, err)
	}

	if delta := cli.Metrics().AbtestCanceledTotal("ns1") - beforeCanceled; delta != 1 {
		t.Fatalf("abtestCanceled delta = %d, want exactly 1", delta)
	}
	if delta := cli.Metrics().AbtestFallbackTotal("ns1") - beforeFallback; delta != 0 {
		t.Fatalf("abtestFallback delta = %d, want 0", delta)
	}
	if warns := findRecordByMsg(rec, abtestDegradeWarnMsg); len(warns) != 0 {
		t.Fatalf("ctx-cancel must not emit the degrade WARN, got %d", len(warns))
	}
	if got := h.abServer.Calls("ns1"); got != beforeCalls {
		t.Logf("note: RPC reached the server %d time(s) despite dead ctx (grpc raced the cancel) — still fine, the error shape is what matters", got-beforeCalls)
	}
}

// TestAbtest_RealError_FallbackNotCanceled pins the regression + exclusivity
// half: a non-cancel per-ns failure keeps the pre-#15 treatment (WARN +
// abtestFallback +1) and must NOT leak into the new abtestCanceled counter.
// Fresh AbtestContext per the package memo convention.
func TestAbtest_RealError_FallbackNotCanceled(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"k": {full: 1, versions: map[int64]string{1: "full-v1"}},
	}))
	h.abServer.SetError("ns1", status.Error(codes.Unavailable, "down"))
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cli := initClient(t, cfg)

	beforeCanceled := cli.Metrics().AbtestCanceledTotal("ns1")
	beforeFallback := cli.Metrics().AbtestFallbackTotal("ns1")

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u-real", nil, "trace-real")
	v, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil || v != "full-v1" {
		t.Fatalf("GetConfig: got (%q,%v), want (full-v1,nil)", v, err)
	}

	if delta := cli.Metrics().AbtestFallbackTotal("ns1") - beforeFallback; delta != 1 {
		t.Fatalf("real-error abtestFallback delta = %d, want exactly 1 (pre-#15 behaviour)", delta)
	}
	if delta := cli.Metrics().AbtestCanceledTotal("ns1") - beforeCanceled; delta != 0 {
		t.Fatalf("real-error abtestCanceled delta = %d, want 0 (counter is cancel-exclusive)", delta)
	}
	warns := findRecordByMsg(rec, abtestDegradeWarnMsg)
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 degrade WARN on real error, got %d", len(warns))
	}
	if got := attrString(t, warns[0], "trace_id"); got != "trace-real" {
		t.Fatalf("degrade WARN trace_id = %q, want trace-real", got)
	}
	if infos := findRecordByMsg(rec, abtestCanceledInfoMsg); len(infos) != 0 {
		t.Fatalf("real error must not emit the canceled Info line, got %d", len(infos))
	}
}

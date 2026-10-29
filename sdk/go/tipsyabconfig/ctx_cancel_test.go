package tipsyabconfig

// Tests for issue #15 (design-phase2 §#15, Go): context cancellation is an
// EXPECTED termination, not a fault. The grpc-go status error for a cancelled
// ctx does NOT wrap the context.Canceled sentinel (grpc rpc_util.go
// errContextCanceled is a bare status error whose Is() only compares against
// other *status.Error), so every `errors.Is(err, context.Canceled)` in the SDK
// was blind to real gRPC cancellations. The fix routes both shapes through
// isContextCanceled and, on a hit:
//
//   - subscribe / periodic pull: only the SDK's OWN shutdown (Close → rootCtx
//     canceled) is a silent exit — no ERROR log, no subscribeDisc/pullFailure
//     inc, no BackgroundErrorEvent (user decision 2026-08-27). The loops decide
//     on rootCtx.Err(), not on the error code: a codes.Canceled sent by the
//     server or a proxy while rootCtx is alive is a FAULT — reported, and the
//     loop keeps going. (Until 2026-10 the loops exited on any Canceled code,
//     so one remote cancel stopped pulling/subscribing for the process
//     lifetime with no signal; the *_RemoteCanceled_* tests pin the fix.)
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

// TestSubscribe_LocalClose_Silent: Close() while the stream is attached ends
// runSubscribe silently — the in-flight Recv fails with the Canceled status,
// rootCtx is done, and nothing is reported (no subscribeDisc, no ERROR log, no
// "subscribe" event).
func TestSubscribe_LocalClose_Silent(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	sink := &recordingErrSink{}
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cfg.PullInterval = time.Hour // keep periodic_pull noise out of the sink
	cfg.OnBackgroundError = sink.cb
	cli := initClient(t, cfg)

	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("subscribe never attached")
	}
	_ = cli.Close() // returns after runSubscribe has exited (wg.Wait)

	if d := cli.Metrics().SubscribeDisconnectTotal("ns1"); d != 0 {
		t.Fatalf("Close counted as a subscribe disconnect: disc=%d", d)
	}
	if n := len(sink.byPhase("subscribe")); n != 0 {
		t.Fatalf("Close fired %d subscribe BackgroundErrorEvent(s)", n)
	}
	if n := len(findRecordByMsg(rec, subscribeErrorMsg)); n != 0 {
		t.Fatalf("Close logged the subscribe ERROR line %d time(s)", n)
	}
}

// TestSubscribe_RemoteCanceled_ReportsAndReconnects: the stream dying with a
// codes.Canceled sent by the SERVER (rootCtx alive) is a fault — subscribeDisc
// inc, ERROR log, "subscribe" event — and the loop reconnects. Before the fix
// it was taken for shutdown: runSubscribe returned and never reconnected.
func TestSubscribe_RemoteCanceled_ReportsAndReconnects(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.cfgServer.SetSubscribeErrFn(grpcCanceledErr)

	sink := &recordingErrSink{}
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cfg.PullInterval = time.Hour
	cfg.OnBackgroundError = sink.cb
	cli := initClient(t, cfg)

	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("subscribe never attached")
	}
	// The fake server sends this frame, then errFn ends the stream with the
	// Canceled status.
	h.cfgServer.PushSnapshot(makeSnapshot("ns1", 5, 5, nil))

	// Reconnect after the initial 1s backoff.
	if !waitFor(t, 4*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 2 }) {
		t.Fatalf("subscribe did not reconnect after a remote Canceled (calls=%d)", h.cfgServer.SubscribeCalls())
	}
	if d := cli.Metrics().SubscribeDisconnectTotal("ns1"); d == 0 {
		t.Fatal("remote Canceled did not bump subscribeDisc")
	}
	if n := len(sink.byPhase("subscribe")); n == 0 {
		t.Fatal("remote Canceled did not fire a subscribe BackgroundErrorEvent")
	}
	if n := len(findRecordByMsg(rec, subscribeErrorMsg)); n == 0 {
		t.Fatal("remote Canceled did not log the subscribe ERROR line")
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

// TestPullLoop_LocalClose_Silent: Close() while a periodic PullAll is in
// flight ends the loop silently — the call fails with the Canceled status,
// rootCtx is done, and nothing is reported (no pullFailure, no ERROR log, no
// "periodic_pull" event).
func TestPullLoop_LocalClose_Silent(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	sink := &recordingErrSink{}
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cfg.OnBackgroundError = sink.cb // PullInterval stays 50ms from baseConfig
	cli := initClient(t, cfg)

	// Park the next periodic pull on the server so Close lands mid-call.
	base := h.cfgServer.PullCalls()
	h.cfgServer.SetPullGate(make(chan struct{}))
	if !waitFor(t, 3*time.Second, func() bool { return h.cfgServer.PullCalls() > base }) {
		t.Fatal("no periodic pull tick reached the server")
	}
	_ = cli.Close() // returns after runPullLoop has exited (wg.Wait)

	if f := cli.Metrics().PullFailureTotal("ns1"); f != 0 {
		t.Fatalf("Close counted as a pull failure: pullFailure=%d", f)
	}
	if n := len(sink.byPhase("periodic_pull")); n != 0 {
		t.Fatalf("Close fired %d periodic_pull BackgroundErrorEvent(s)", n)
	}
	if n := len(findRecordByMsg(rec, pullErrorMsg)); n != 0 {
		t.Fatalf("Close logged the periodic pull ERROR line %d time(s)", n)
	}
}

// TestPullLoop_RemoteCanceled_ReportsAndKeepsTicking: a periodic PullAll
// failing with a codes.Canceled sent by the SERVER (rootCtx alive) is a fault
// — pullFailure inc, ERROR log, "periodic_pull" event — and the loop keeps
// ticking. Before the fix the loop returned on it and never pulled again.
func TestPullLoop_RemoteCanceled_ReportsAndKeepsTicking(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	sink := &recordingErrSink{}
	cfg, rec := recordingConfig(h, []string{"ns1"})
	cfg.AbtestServiceAddr = ""
	cfg.OnBackgroundError = sink.cb // PullInterval stays 50ms from baseConfig
	cli := initClient(t, cfg)

	h.cfgServer.SetPullError(grpcCanceledErr())
	if !waitFor(t, 3*time.Second, func() bool {
		return cli.Metrics().PullFailureTotal("ns1") > 0
	}) {
		t.Fatal("remote Canceled on periodic pull did not bump pullFailure")
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(sink.byPhase("periodic_pull")) >= 1 }) {
		t.Fatal("remote Canceled on periodic pull did not fire a periodic_pull event")
	}
	if len(findRecordByMsg(rec, pullErrorMsg)) == 0 {
		t.Fatal("remote Canceled on periodic pull did not log the ERROR line")
	}
	// The loop must keep ticking: with a 50ms interval a live loop racks up
	// several more calls quickly.
	c1 := h.cfgServer.PullCalls()
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.PullCalls() > c1+2 }) {
		t.Fatalf("pull loop stopped after a remote Canceled (calls stuck at %d)", c1)
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

package tipsyabconfig

// ST2 test file (grpc-latency-todos design 改动 3 + Testing Plan §ST2).
//
// Covers the per-call Debug timing log emitted after every
// AbtestService.GetExperimentResult RPC at BOTH call sites:
//   - experiment_result.go public Client.GetExperimentResult
//   - abtest_context.go fetchConfigVersionFlatKvForNamespace (lazy per-ns fetch)
//
// Contract under test (design 改动 3 / Acceptance Criterion 3):
//   - msg is exactly "tipsyabconfig: GetExperimentResult rpc" at Debug level;
//   - attrs carry ns, trace_id (non-empty), duration_ms as FLOAT ms > 0
//     (μs precision — an int would truncate the ~1ms real latency to 0);
//   - err attr appended ONLY on failure (success records have no err key);
//   - an Info-level logger captures NOTHING new (Debug short-circuits).
//
// Per Testing Plan we run through the bufconn harness (real gRPC round trip
// guarantees duration_ms > 0); harness baseConfig hardcodes a discard Logger,
// so every test overrides cfg.Logger with the capture handler below.

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const rpcTimingLogMsg = "tipsyabconfig: GetExperimentResult rpc"

// capturedLogRecord is one slog record snapshotted by captureLogHandler.
type capturedLogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]slog.Value
}

// captureLogHandler is a minimal slog.Handler that stores every record at or
// above minLevel under a mutex. Background SDK goroutines (pull/subscribe
// loops) share the client logger and may write concurrently with the test
// goroutine reading, hence the lock.
//
// WithAttrs / WithGroup return the receiver unchanged: the SDK production
// code never uses logger.With / groups, so no attr merging is needed.
type captureLogHandler struct {
	minLevel slog.Level
	mu       sync.Mutex
	records  []capturedLogRecord
}

func newCaptureLogHandler(minLevel slog.Level) *captureLogHandler {
	return &captureLogHandler{minLevel: minLevel}
}

func (h *captureLogHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.minLevel
}

func (h *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]slog.Value, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, capturedLogRecord{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h *captureLogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureLogHandler) WithGroup(_ string) slog.Handler      { return h }

// recordsWithMsg snapshots every captured record whose message equals msg.
func (h *captureLogHandler) recordsWithMsg(msg string) []capturedLogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]capturedLogRecord, 0, 1)
	for _, r := range h.records {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

// assertTimingAttrs asserts the shared attr contract of one timing record:
// ns matches, trace_id is a non-empty string, duration_ms is a FLOAT > 0.
func assertTimingAttrs(t *testing.T, rec capturedLogRecord, wantNs string) {
	t.Helper()
	if rec.level != slog.LevelDebug {
		t.Fatalf("timing record level = %v, want %v", rec.level, slog.LevelDebug)
	}
	nsVal, ok := rec.attrs["ns"]
	if !ok {
		t.Fatal("timing record missing ns attr")
	}
	if got := nsVal.String(); got != wantNs {
		t.Fatalf("ns attr = %q, want %q", got, wantNs)
	}
	traceVal, ok := rec.attrs["trace_id"]
	if !ok {
		t.Fatal("timing record missing trace_id attr")
	}
	if traceVal.String() == "" {
		t.Fatal("timing record trace_id attr is empty")
	}
	durVal, ok := rec.attrs["duration_ms"]
	if !ok {
		t.Fatal("timing record missing duration_ms attr")
	}
	if durVal.Kind() != slog.KindFloat64 {
		t.Fatalf("duration_ms attr kind = %v, want %v (float ms with μs precision)", durVal.Kind(), slog.KindFloat64)
	}
	if durVal.Float64() <= 0 {
		t.Fatalf("duration_ms = %v, want > 0 (real bufconn round trip)", durVal.Float64())
	}
}

func TestGetExperimentResult_RPCTimingDebugLog_Success(t *testing.T) {
	h := newHarness(t)
	cfg := h.baseConfig([]string{"ns1"})
	capture := newCaptureLogHandler(slog.LevelDebug)
	cfg.Logger = slog.New(capture)
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if _, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	}); err != nil {
		t.Fatalf("GetExperimentResult: %v", err)
	}

	recs := capture.recordsWithMsg(rpcTimingLogMsg)
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 timing record, got %d: %+v", len(recs), recs)
	}
	rec := recs[0]
	assertTimingAttrs(t, rec, "ns1")
	if _, hasErr := rec.attrs["err"]; hasErr {
		t.Fatalf("success record must NOT carry an err attr; got %v", rec.attrs["err"])
	}
	// Correlation contract: the logged trace_id is the SAME id stamped on the
	// outbound proto, so SDK and server log lines align per call.
	req := h.abServer.LastRequest()
	if req == nil {
		t.Fatal("no GetExperimentResult request captured by fake server")
	}
	if got, want := rec.attrs["trace_id"].String(), req.GetTraceId(); got != want {
		t.Fatalf("logged trace_id = %q, want outbound proto TraceId %q", got, want)
	}
}

func TestGetExperimentResult_RPCTimingDebugLog_ErrAttrOnFailure(t *testing.T) {
	h := newHarness(t)
	h.abServer.SetError("ns1", status.Error(codes.Internal, "boom"))
	cfg := h.baseConfig([]string{"ns1"})
	capture := newCaptureLogHandler(slog.LevelDebug)
	cfg.Logger = slog.New(capture)
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if _, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	}); err == nil {
		t.Fatal("expected GetExperimentResult to fail (fake server returns Internal)")
	}

	recs := capture.recordsWithMsg(rpcTimingLogMsg)
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 timing record, got %d: %+v", len(recs), recs)
	}
	rec := recs[0]
	assertTimingAttrs(t, rec, "ns1")
	errVal, hasErr := rec.attrs["err"]
	if !hasErr {
		t.Fatal("failure record must carry an err attr")
	}
	if errVal.Any() == nil {
		t.Fatal("err attr value is nil, want the RPC error")
	}
}

func TestAbtestContextLazyFetch_RPCTimingDebugLog(t *testing.T) {
	h := newHarness(t)
	cfg := h.baseConfig([]string{"ns1"})
	capture := newCaptureLogHandler(slog.LevelDebug)
	cfg.Logger = slog.New(capture)
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	// Lazy per-ns fetch: WaitForAbtest triggers exactly one
	// fetchConfigVersionFlatKvForNamespace RPC and blocks until its goroutine
	// finishes — the Debug record is emitted before st.done closes, so no
	// extra synchronisation is needed here.
	if _, err := abctx.WaitForAbtest(context.Background(), "ns1"); err != nil {
		t.Fatalf("WaitForAbtest: %v", err)
	}

	recs := capture.recordsWithMsg(rpcTimingLogMsg)
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 timing record from the lazy fetch, got %d: %+v", len(recs), recs)
	}
	rec := recs[0]
	assertTimingAttrs(t, rec, "ns1")
	if _, hasErr := rec.attrs["err"]; hasErr {
		t.Fatalf("success record must NOT carry an err attr; got %v", rec.attrs["err"])
	}
	if got, want := rec.attrs["trace_id"].String(), abctx.TraceID(); got != want {
		t.Fatalf("logged trace_id = %q, want AbtestContext trace id %q", got, want)
	}
}

func TestRPCTimingLog_SilentAtInfoLevel(t *testing.T) {
	// Acceptance Criterion 3: an Info-level logger sees ZERO new output —
	// the timing log is Debug-only, and slog short-circuits via Enabled.
	h := newHarness(t)
	cfg := h.baseConfig([]string{"ns1"})
	capture := newCaptureLogHandler(slog.LevelInfo)
	cfg.Logger = slog.New(capture)
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if _, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	}); err != nil {
		t.Fatalf("GetExperimentResult: %v", err)
	}

	if recs := capture.recordsWithMsg(rpcTimingLogMsg); len(recs) != 0 {
		t.Fatalf("Info-level logger must capture no timing records, got %d: %+v", len(recs), recs)
	}
}

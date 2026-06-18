package tipsyabconfig

// SubTask B test file (sdk-trace-id design §4 ExposureEvent.TraceID +
// default logSink output).
//
// Covers:
//   - ExposureSink receives an ExposureEvent whose TraceID equals the
//     AbtestContext's traceID that triggered the abtest hit. We exercise this
//     through the real GetConfig path (bufconn harness + abtest server) so the
//     emitter receives the trace from the same AbtestContext the production
//     code path uses.
//   - The DEFAULT logSink (used when Config.ExposureSink is nil) MUST emit a
//     `trace_id` field in its INFO log line so operator-grade logs are
//     self-sufficient.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

func TestExposure_TraceID_PropagatesFromAbtestContext(t *testing.T) {
	h := newHarness(t)
	// Cache has full v=1 and ab v=2; abtest returns ab=2 with an experiment
	// exposure for ("k", 2).
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "ab-v2"}}}))
	expID := "101"
	groupID := "202"
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
		Exposures: []*abtestv1.Exposure{
			{Key: "k", Version: 2, Source: "experiment_group", ExperimentId: &expID, GroupId: &groupID},
		},
	})

	sink := newDrainExposureSink()
	cfg := h.baseConfig([]string{"ns1"})
	cfg.ExposureSink = sink

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u1", map[string]any{"country": "US"}, "T-X")
	if abctx.traceID != "T-X" {
		t.Fatalf("traceID = %q, want %q", abctx.traceID, "T-X")
	}
	val, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if val != "ab-v2" {
		t.Fatalf("expected ab value, got %q", val)
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(sink.Events()) >= 1 }) {
		t.Fatalf("expected exposure event; got %d", len(sink.Events()))
	}
	ev := sink.Events()[0]
	if ev.TraceID != "T-X" {
		t.Fatalf("ExposureEvent.TraceID = %q, want %q", ev.TraceID, "T-X")
	}
}

func TestExposure_DefaultLogSink_EmitsTraceIDField(t *testing.T) {
	// Drive the default logSink (Config.ExposureSink left nil) and verify the
	// emitted INFO log line carries the `trace_id` field.
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "ab-v2"}}}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
		Exposures: []*abtestv1.Exposure{
			{Key: "k", Version: 2, Source: "experiment_group"},
		},
	})

	logBuf := newSyncBuffer()
	cfg := h.baseConfig([]string{"ns1"})
	cfg.Logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// cfg.ExposureSink left nil ⇒ Init wires the default logSink.

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContextWithTraceID(context.Background(), "u1", nil, "T-LOG")
	if _, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil {
		t.Fatalf("GetConfig: %v", err)
	}

	// The default logSink emits one INFO log per exposure. Poll for the line
	// rather than blanket-sleep so the test stays fast.
	found := waitFor(t, 2*time.Second, func() bool {
		return findLogLineWithField(t, logBuf, "exposure", "trace_id", "T-LOG")
	})
	if !found {
		// Fallback diagnostic: dump the buffer for the failure msg.
		t.Fatalf("expected INFO log line on exposure path with trace_id=%q;\nlogs=\n%s",
			"T-LOG", logBuf.String())
	}
}

// TestExposure_Emit_StampsTraceID is a focused low-level unit test on
// exposureEmitter.emit: bypasses the bufconn rig and asserts the emit
// signature stamps `traceID` onto every produced ExposureEvent. This guards
// the contract independently from the GetConfig integration test above.
func TestExposure_Emit_StampsTraceID(t *testing.T) {
	ch := make(chan ExposureEvent, 2)
	e, _ := startEmitter(t, &channelSink{ch: ch}, time.Minute, nil)
	e.emit("u1", "ns1", "k", 10, []*abtestv1.Exposure{
		{Key: "k", Version: 10, Source: "experiment_group"},
	}, "stamped-trace-id")
	select {
	case ev := <-ch:
		if ev.TraceID != "stamped-trace-id" {
			t.Fatalf("ExposureEvent.TraceID = %q, want %q", ev.TraceID, "stamped-trace-id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emit never delivered to sink")
	}
}


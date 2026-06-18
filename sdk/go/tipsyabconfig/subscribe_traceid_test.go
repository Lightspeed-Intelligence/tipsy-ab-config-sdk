package tipsyabconfig

// SubTask B test file (sdk-trace-id design §4 Subscribe + §3 C SDK Debug log).
//
// Covers Client.subscribeOnce trace_id behaviour:
//   - Every Subscribe attempt generates a FRESH UUID v4 that flows onto the
//     outbound SubscribeRequest.TraceId proto field (the bufconn fake server
//     records it).
//   - A Debug log line MUST be emitted on the subscribe path with the same
//     `trace_id` value.
//
// Subscribe streaming is not abstracted behind configTransport; the SDK calls
// c.configCli.Subscribe directly. The standard newHarness() bufconn rig is
// therefore the smallest reliable surface — we just override Logger so we can
// scan JSON log output.

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestSubscribe_TraceID_OnOutboundRequest(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	logBuf := newSyncBuffer()
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.Logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	// Wait for the first Subscribe call to land at the fake server.
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatalf("Subscribe never attached; calls=%d", h.cfgServer.SubscribeCalls())
	}
	req := h.cfgServer.LastSubscribeReq()
	if req == nil {
		t.Fatal("no SubscribeRequest captured")
	}
	if req.GetTraceId() == "" {
		t.Fatal("expected non-empty TraceId on SubscribeRequest")
	}
	if !uuidV4Pattern.MatchString(req.GetTraceId()) {
		t.Fatalf("expected UUID-shaped TraceId, got %q", req.GetTraceId())
	}
}

func TestSubscribe_TraceID_DebugLogCarriesField(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	logBuf := newSyncBuffer()
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.Logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatalf("Subscribe never attached; calls=%d", h.cfgServer.SubscribeCalls())
	}
	req := h.cfgServer.LastSubscribeReq()
	if req == nil {
		t.Fatal("no SubscribeRequest captured")
	}
	wantTraceID := req.GetTraceId()
	if wantTraceID == "" {
		t.Fatal("captured SubscribeRequest had empty TraceId")
	}

	// Give the SDK a beat to flush its log line. We do a short polling wait
	// rather than a fixed sleep to keep the test fast on healthy runs.
	found := waitFor(t, 2*time.Second, func() bool {
		return findLogLineWithField(t, logBuf, "Subscribe", "trace_id", wantTraceID)
	})
	if !found {
		t.Fatalf("expected DEBUG log line containing %q with trace_id=%q;\nlogs=\n%s",
			"Subscribe", wantTraceID, logBuf.String())
	}
}

package tipsyabconfig

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSubscribe_PushAppliesToCache(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v1"}}}))

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	// Wait for subscribe to attach.
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("Subscribe never attached")
	}

	// Push a snapshot with advanced seqs.
	h.cfgServer.PushSnapshot(makeSnapshot("ns1", 5, 5, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 2, versions: map[int64]string{2: "v2"}}}))

	if !waitFor(t, 2*time.Second, func() bool {
		v, ok := cli.cache.fullReleaseVersion("ns1", "k")
		return ok && v == 2
	}) {
		t.Fatal("cache not updated by pushed subscribe event")
	}
	if cli.Metrics().SubscribeEventReceivedTotal("ns1") == 0 {
		t.Fatal("subscribe_event_received_total not incremented")
	}
}

func TestSubscribe_ErrorReconnects(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	// First subscribe call: server returns error after the first push.
	var fired atomic.Bool
	h.cfgServer.SetSubscribeErrFn(func() error {
		if fired.CompareAndSwap(false, true) {
			return status.Error(codes.Unavailable, "kicked")
		}
		return nil
	})

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	// Wait for first subscribe.
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("first subscribe never attached")
	}
	// Push a frame to trigger the err path.
	h.cfgServer.PushSnapshot(makeSnapshot("ns1", 2, 2, nil))

	// Wait for reconnect (a second Subscribe call). With backoff 1s the
	// reconnect should happen within ~3-5s.
	if !waitFor(t, 5*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 2 }) {
		t.Fatalf("expected reconnect; only %d subscribe calls", h.cfgServer.SubscribeCalls())
	}
	if cli.Metrics().SubscribeDisconnectTotal("ns1") == 0 {
		t.Fatal("subscribe_disconnect_total ns1 should be > 0")
	}
}

func TestSubscribe_CancelClean(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("subscribe never attached")
	}
	// Close should cancel rootCtx, exit Subscribe loop, drain exposure
	// queue, and return without deadlock.
	done := make(chan error, 1)
	go func() { done <- cli.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within 3s")
	}
}

// Test that re-connect uses freshly-computed known_seqs.
func TestSubscribe_ReconnectUsesLatestKnownSeqs(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	once := atomic.Bool{}
	h.cfgServer.SetSubscribeErrFn(func() error {
		if once.CompareAndSwap(false, true) {
			return status.Error(codes.Unavailable, "boom")
		}
		return nil
	})

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("subscribe never attached")
	}
	// Push an advancing snapshot; cache moves to seq=10.
	h.cfgServer.PushSnapshot(makeSnapshot("ns1", 10, 10, nil))

	// Wait until the cache reflects seq=10.
	if !waitFor(t, 2*time.Second, func() bool {
		s := cli.cache.snapshot("ns1")
		return s != nil && s.BusinessSnapshotSeq == 10
	}) {
		t.Fatal("cache never advanced after push")
	}
	// Wait for reconnect.
	if !waitFor(t, 5*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 2 }) {
		t.Fatal("reconnect did not occur")
	}
	// Now the second SubscribeRequest must carry known_seqs ns1=(10,10).
	req := h.cfgServer.LastSubscribeReq()
	if req == nil {
		t.Fatal("no second subscribe req")
	}
	seqs, ok := req.KnownSeqs["ns1"]
	if !ok || seqs.BusinessSnapshotSeq != 10 || seqs.ExperimentSnapshotSeq != 10 {
		t.Fatalf("reconnect known_seqs ns1 = %+v, want biz=10 exp=10", seqs)
	}
}

// errEventSilentlyIgnoredForUnknownPayload exercises handleEvent on a nil
// payload — this is a unit test on the package private path.
func TestHandleEvent_NilSafe(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()
	// Direct call into the SDK from the same package.
	cli.handleEvent(nil)
}

var _ = errors.New

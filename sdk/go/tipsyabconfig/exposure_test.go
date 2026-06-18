package tipsyabconfig

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// channelSink captures every event into a channel.
type channelSink struct {
	ch chan ExposureEvent
}

func (s *channelSink) Sink(ev ExposureEvent) { s.ch <- ev }

func startEmitter(t *testing.T, sink ExposureSink, ttl time.Duration, clock func() time.Time) (*exposureEmitter, context.CancelFunc) {
	t.Helper()
	e := newExposureEmitter(sink, ttl, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if clock != nil {
		e.now = clock
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go e.run(ctx, &wg)
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return e, cancel
}

func TestExposure_BasicEmit(t *testing.T) {
	ch := make(chan ExposureEvent, 4)
	e, _ := startEmitter(t, &channelSink{ch: ch}, time.Minute, nil)
	expID := "1"
	groupID := "2"
	e.emit("u1", "ns1", "k", 10, []*abtestv1.Exposure{
		{Key: "k", Version: 10, Source: "experiment_group", ExperimentId: &expID, GroupId: &groupID},
	}, "")
	select {
	case ev := <-ch:
		if ev.UserID != "u1" || ev.Namespace != "ns1" || ev.Key != "k" || ev.Version != 10 || ev.Source != "experiment_group" {
			t.Fatalf("event content wrong: %+v", ev)
		}
		if ev.ExperimentID != "1" || ev.GroupID != "2" {
			t.Fatalf("ids wrong: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not received")
	}
}

func TestExposure_DedupWithin5Min(t *testing.T) {
	ch := make(chan ExposureEvent, 8)
	var now atomic.Int64
	now.Store(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC).UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }
	e, _ := startEmitter(t, &channelSink{ch: ch}, 5*time.Minute, clock)

	for i := 0; i < 3; i++ {
		e.emit("u1", "ns1", "k", 10, nil, "")
	}
	// Wait briefly so the worker drains.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected first emit to flow through")
	}
	select {
	case ev := <-ch:
		t.Fatalf("dedup violated: got second emit %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	// Advance past TTL -> should pass.
	now.Add(int64(6 * time.Minute))
	e.emit("u1", "ns1", "k", 10, nil, "")
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected emit after ttl window")
	}
}

func TestExposure_DistinguishesKeyVersionUid(t *testing.T) {
	ch := make(chan ExposureEvent, 16)
	e, _ := startEmitter(t, &channelSink{ch: ch}, time.Minute, nil)
	e.emit("u1", "ns1", "k", 10, nil, "")
	e.emit("u1", "ns1", "k", 11, nil, "") // different version
	e.emit("u1", "ns1", "other", 10, nil, "")
	e.emit("u2", "ns1", "k", 10, nil, "") // different uid

	deadline := time.Now().Add(2 * time.Second)
	count := 0
	for time.Now().Before(deadline) && count < 4 {
		select {
		case <-ch:
			count++
		case <-time.After(50 * time.Millisecond):
		}
	}
	if count != 4 {
		t.Fatalf("expected 4 distinct emits, got %d", count)
	}
}

func TestExposure_QueueOverflowDrops(t *testing.T) {
	// Use a small blocking sink so the queue fills up.
	blockCh := make(chan struct{})
	blockingSink := ExposureSinkFunc(func(ev ExposureEvent) {
		<-blockCh
	})
	e := newExposureEmitter(blockingSink, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Shrink queue capacity so we don't have to push 4096 events.
	e.queue = make(chan ExposureEvent, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go e.run(ctx, &wg)

	// Push more events than capacity. Each emit is to a unique
	// (uid, key, version) so dedup is bypassed.
	const N = 64
	for i := 0; i < N; i++ {
		e.emit("u", "ns", "k", int64(i), nil, "")
	}
	// At this point the queue is full and many events have been dropped.
	// The test passes if we never deadlocked or panicked above.

	close(blockCh)
	// Allow worker to drain a bit.
	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait()
}

func TestExposure_SinkPanicRecovered(t *testing.T) {
	count := atomic.Int64{}
	first := true
	sink := ExposureSinkFunc(func(ev ExposureEvent) {
		count.Add(1)
		if first {
			first = false
			panic("boom")
		}
	})
	e, _ := startEmitter(t, sink, time.Minute, nil)
	e.emit("u1", "ns", "k", 1, nil, "")
	e.emit("u1", "ns", "k", 2, nil, "") // different version - bypass dedup
	if !waitFor(t, 2*time.Second, func() bool { return count.Load() >= 2 }) {
		t.Fatalf("expected sink to recover from panic; got %d invocations", count.Load())
	}
}

func TestExposure_DefaultSourceWhenNoMatch(t *testing.T) {
	ch := make(chan ExposureEvent, 4)
	e, _ := startEmitter(t, &channelSink{ch: ch}, time.Minute, nil)
	// abExposures doesn't contain (k, 10) - default attribution kicks in.
	e.emit("u1", "ns1", "k", 10, []*abtestv1.Exposure{
		{Key: "other", Version: 99, Source: "whitelist"},
	}, "")
	select {
	case ev := <-ch:
		if ev.Source != "experiment_group" {
			t.Fatalf("expected default source experiment_group, got %q", ev.Source)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not received")
	}
}

func TestExposure_WhitelistSource(t *testing.T) {
	ch := make(chan ExposureEvent, 4)
	e, _ := startEmitter(t, &channelSink{ch: ch}, time.Minute, nil)
	relID := int64(77)
	e.emit("u1", "ns1", "k", 10, []*abtestv1.Exposure{
		{Key: "k", Version: 10, Source: "whitelist", ReleaseId: &relID},
	}, "")
	select {
	case ev := <-ch:
		if ev.Source != "whitelist" || ev.ReleaseID != 77 {
			t.Fatalf("wrong attribution: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not received")
	}
}

func TestPickExposureForKey(t *testing.T) {
	a := &abtestv1.Exposure{Key: "a", Version: 1, Source: "experiment_group"}
	b := &abtestv1.Exposure{Key: "b", Version: 2, Source: "whitelist"}
	got := pickExposureForKey("b", 2, []*abtestv1.Exposure{a, nil, b})
	if got == nil || got.Source != "whitelist" {
		t.Fatalf("expected b match, got %+v", got)
	}
	if got := pickExposureForKey("x", 99, []*abtestv1.Exposure{a, b}); got != nil {
		t.Fatalf("expected no match, got %+v", got)
	}
}

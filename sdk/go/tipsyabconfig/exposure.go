package tipsyabconfig

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// ExposureEvent is the SDK-emitted exposure record. One event corresponds to
// one (uid, ns, key, version) hit through an abtest path. The shape is a
// SDK-stable struct so production callers can route to Kafka / HTTP / log
// without depending on the proto types.
type ExposureEvent struct {
	UserID           string
	Namespace        string
	Key              string
	Version          int64
	Source           string // "experiment_group" | "whitelist"
	ExperimentID     string // "" when source = whitelist (v2: TEXT ids)
	GroupID          string // "" when source = whitelist
	ExperimentStatus string // for source = experiment_group
	ReleaseID        int64  // 0 when source = experiment_group
	// TraceID is the per-request trace id of the AbtestContext that produced
	// this exposure (sdk-trace-id §4). It is the reporting hook for the
	// upcoming "experiment-result data report" channel: same id as the
	// server-side compute log so log ↔ report ↔ upstream caller all join on
	// one column.
	TraceID   string
	EmittedAt time.Time
}

// ExposureSink receives exposure events. Implementations should be
// non-blocking; the SDK calls Sink on a dedicated background goroutine.
type ExposureSink interface {
	Sink(ev ExposureEvent)
}

// ExposureSinkFunc adapts a plain function into an ExposureSink.
type ExposureSinkFunc func(ExposureEvent)

// Sink implements ExposureSink.
func (f ExposureSinkFunc) Sink(ev ExposureEvent) { f(ev) }

// logSink is the default sink — emits one INFO log per exposure.
type logSink struct{ logger *slog.Logger }

// Sink implements ExposureSink.
func (s *logSink) Sink(ev ExposureEvent) {
	s.logger.Info("tipsyabconfig: exposure",
		"uid", ev.UserID,
		"ns", ev.Namespace,
		"key", ev.Key,
		"version", ev.Version,
		"source", ev.Source,
		"experiment_id", ev.ExperimentID,
		"group_id", ev.GroupID,
		"release_id", ev.ReleaseID,
		"trace_id", ev.TraceID,
	)
}

// exposureEmitter is the SDK's async exposure pipeline. emit() is
// non-blocking and offers a per-process LRU-style dedup window keyed on
// (uid, key, version) per design §9.2.
//
// Implementation: an unbuffered map[string]time.Time guarded by RWMutex
// holds last-seen timestamps. On emit we drop the event if a recent entry
// exists; otherwise we push onto a buffered channel and the background
// goroutine drains to the sink. We GC the dedup map opportunistically when
// it grows past dedupMaxEntries.
type exposureEmitter struct {
	sink ExposureSink
	ttl  time.Duration

	logger *slog.Logger

	queue chan ExposureEvent

	dmu             sync.Mutex
	dedup           map[string]time.Time
	dedupMaxEntries int
	now             func() time.Time
}

const defaultExposureQueueSize = 4096

func newExposureEmitter(sink ExposureSink, ttl time.Duration, logger *slog.Logger) *exposureEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	if sink == nil {
		sink = &logSink{logger: logger}
	}
	return &exposureEmitter{
		sink:            sink,
		ttl:             ttl,
		logger:          logger,
		queue:           make(chan ExposureEvent, defaultExposureQueueSize),
		dedup:           make(map[string]time.Time, 1024),
		dedupMaxEntries: 16384,
		now:             time.Now,
	}
}

// emit is the hot-path entry called from GetConfig. It builds zero-or-more
// ExposureEvent rows (one per hit-eligible exposure on the matching key),
// deduplicates each, and posts to the background queue.
//
// abExposures is the Exposure list returned by AbtestService.Compute. We
// only emit a row when the exposure's (key, version) match the resolved
// (key, version) — the response can carry hits for many keys but the SDK
// only attributes the one we actually returned.
//
// traceID is the AbtestContext's per-request trace id (sdk-trace-id §4); it
// is stamped onto every emitted event so the log sink and the upcoming
// upstream "experiment-result data report" channel can join on the same id
// as the server-side compute log.
func (e *exposureEmitter) emit(uid, ns, key string, version int64, abExposures []*abtestv1.Exposure, traceID string) {
	if e == nil {
		return
	}
	matched := pickExposureForKey(key, version, abExposures)
	if matched == nil {
		// No source attribution in the Compute response; per design we
		// still record the hit at "experiment_group" with zero ids so
		// downstream dashboards know an SDK-side resolve happened.
		matched = &abtestv1.Exposure{Key: key, Version: version, Source: "experiment_group"}
	}
	ev := ExposureEvent{
		UserID:    uid,
		Namespace: ns,
		Key:       key,
		Version:   version,
		Source:    matched.GetSource(),
		TraceID:   traceID,
		EmittedAt: e.now(),
	}
	if matched.ExperimentId != nil {
		ev.ExperimentID = *matched.ExperimentId
	}
	if matched.GroupId != nil {
		ev.GroupID = *matched.GroupId
	}
	if matched.ExperimentStatus != nil {
		ev.ExperimentStatus = *matched.ExperimentStatus
	}
	if matched.ReleaseId != nil {
		ev.ReleaseID = *matched.ReleaseId
	}
	if !e.shouldEmit(uid, key, version) {
		return
	}
	select {
	case e.queue <- ev:
	default:
		// Queue full — drop with a warn. We do not block GetConfig.
		e.logger.Warn("tipsyabconfig: exposure queue full; dropping event",
			"ns", ns, "key", key, "version", version)
	}
}

func pickExposureForKey(key string, version int64, exposures []*abtestv1.Exposure) *abtestv1.Exposure {
	for _, ex := range exposures {
		if ex == nil {
			continue
		}
		if ex.Key == key && ex.Version == version {
			return ex
		}
	}
	return nil
}

// shouldEmit returns true if this (uid, key, version) tuple has not been
// seen within the ttl window. Returning false means we drop the event as a
// duplicate. We always update the last-seen timestamp when returning true.
func (e *exposureEmitter) shouldEmit(uid, key string, version int64) bool {
	k := dedupKey(uid, key, version)
	now := e.now()
	e.dmu.Lock()
	defer e.dmu.Unlock()
	if last, ok := e.dedup[k]; ok && now.Sub(last) < e.ttl {
		return false
	}
	if len(e.dedup) >= e.dedupMaxEntries {
		// Opportunistic GC: drop entries past their TTL.
		for ek, ev := range e.dedup {
			if now.Sub(ev) >= e.ttl {
				delete(e.dedup, ek)
			}
		}
	}
	e.dedup[k] = now
	return true
}

func dedupKey(uid, key string, version int64) string {
	return fmt.Sprintf("%s\x1f%s\x1f%d", uid, key, version)
}

// run drains the exposure queue. Exits when ctx is cancelled and the queue
// is empty.
func (e *exposureEmitter) run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Drain any pending events synchronously, then exit.
			for {
				select {
				case ev := <-e.queue:
					e.sink.Sink(ev)
				default:
					return
				}
			}
		case ev := <-e.queue:
			func() {
				defer func() {
					if r := recover(); r != nil {
						e.logger.Error("tipsyabconfig: exposure sink panicked", "panic", r)
					}
				}()
				e.sink.Sink(ev)
			}()
		}
	}
}

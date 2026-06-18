package tipsyabconfig

import "sync/atomic"

// Metrics exposes the per-process SDK counters listed in
// config-platform-sdk.md §10.6. Counters are simple atomic uint64s; the
// host application can poll them and forward to its own metrics pipeline.
//
// All counters are monotonic; per-namespace counters are stored in a
// sync-protected map. Lookups for unknown namespaces return zero.
type Metrics struct {
	cacheEmpty       atomic.Uint64
	pullFailure      *nsCounter
	subscribeDisc    *nsCounter
	subscribeEvent   *nsCounter
	localCacheBytes  *nsGauge
	abtestFallback   *nsCounter
	businessSeqMoved *nsCounter
	experimentSeqMov *nsCounter
}

func newMetrics() *Metrics {
	return &Metrics{
		pullFailure:      newNSCounter(),
		subscribeDisc:    newNSCounter(),
		subscribeEvent:   newNSCounter(),
		localCacheBytes:  newNSGauge(),
		abtestFallback:   newNSCounter(),
		businessSeqMoved: newNSCounter(),
		experimentSeqMov: newNSCounter(),
	}
}

// CacheEmptyTotal corresponds to sdk_cache_empty_total.
func (m *Metrics) CacheEmptyTotal() uint64 { return m.cacheEmpty.Load() }

// PullFailureTotal corresponds to sdk_pull_failure_total{namespace}.
func (m *Metrics) PullFailureTotal(ns string) uint64 { return m.pullFailure.get(ns) }

// SubscribeDisconnectTotal corresponds to sdk_subscribe_disconnect_total{namespace}.
func (m *Metrics) SubscribeDisconnectTotal(ns string) uint64 { return m.subscribeDisc.get(ns) }

// SubscribeEventReceivedTotal corresponds to sdk_subscribe_event_received_total{namespace}.
func (m *Metrics) SubscribeEventReceivedTotal(ns string) uint64 { return m.subscribeEvent.get(ns) }

// LocalCacheBytes corresponds to sdk_local_cache_bytes{namespace}.
func (m *Metrics) LocalCacheBytes(ns string) uint64 { return m.localCacheBytes.get(ns) }

// AbtestFallbackTotal corresponds to sdk_abtest_fallback_total{namespace}.
// The "key" dimension from design §10.6 is collapsed to per-ns; the SDK
// host can re-fan out at log level if it needs key-level detail.
func (m *Metrics) AbtestFallbackTotal(ns string) uint64 { return m.abtestFallback.get(ns) }

// BusinessSeqChangeTotal corresponds to sdk_business_seq_change_total{namespace}.
func (m *Metrics) BusinessSeqChangeTotal(ns string) uint64 { return m.businessSeqMoved.get(ns) }

// ExperimentSeqChangeTotal corresponds to sdk_experiment_seq_change_total{namespace}.
func (m *Metrics) ExperimentSeqChangeTotal(ns string) uint64 { return m.experimentSeqMov.get(ns) }
